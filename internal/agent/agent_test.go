package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeVendor struct{ token string }

func (f fakeVendor) Vend(context.Context, string) (string, error) { return f.token, nil }

// memSink is an in-memory Sink with the agent plane's write semantics: a write-back
// is merged into the stored item (worklist.MergeRunItems) and a reply is appended
// through worklist.AppendReply, so tests see what a real run's writes produce.
type memSink struct {
	items   []worklist.WorkItem
	ingests int // write-back calls, so batching is observable
	replies int // Reply calls
}

func (m *memSink) List(context.Context) ([]worklist.WorkItem, error) {
	out := make([]worklist.WorkItem, len(m.items))
	for i, it := range m.items {
		it.Thread = append([]worklist.Message(nil), it.Thread...)
		out[i] = it
	}
	return out, nil
}

func (m *memSink) Ingest(_ context.Context, items []worklist.WorkItem) error {
	m.ingests++
	m.put(worklist.MergeRunItems(m.items, items))
	return nil
}

func (m *memSink) Reply(_ context.Context, itemID string, inReplyTo time.Time, reply worklist.Message) error {
	m.replies++
	for i := range m.items {
		if m.items[i].ID == itemID {
			_, err := worklist.AppendReply(&m.items[i], inReplyTo, reply)
			return err
		}
	}
	return nil
}

func (m *memSink) put(items []worklist.WorkItem) {
	for _, it := range items {
		found := false
		for i := range m.items {
			if m.items[i].ID == it.ID {
				m.items[i] = it
				found = true
				break
			}
		}
		if !found {
			m.items = append(m.items, it)
		}
	}
}

// runIngest vends a token, fetches from GitHub, and writes to the sink.
func TestRunIngestFetchesAndWrites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("q"), "review-requested:@me") {
			_, _ = w.Write([]byte(`{"items":[{"number":7,"title":"T","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	sink := &memSink{}
	err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.items) != 1 || sink.items[0].GitHub.Number != 7 {
		t.Fatalf("expected 1 fetched item (#7), got %+v", sink.items)
	}
}

// A re-ingest (Refresh / backfill) reconciles in place: it must keep each item's
// conversation thread and user/agent overrides while taking the fresh GitHub fields,
// never clobbering the thread the way a raw fetch-and-replace would.
func TestRunIngestPreservesThreads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("q"), "review-requested:@me") {
			_, _ = w.Write([]byte(`{"items":[{"number":7,"title":"Fresh title","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	hidden := time.Now().Add(-time.Hour).UTC()
	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#7", Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 7, Title: "Stale title"},
		Thread: []worklist.Message{
			{Role: worklist.RoleUser, Content: "Can you review this PR?", Kind: worklist.KindReviewRequest},
			{Role: worklist.RoleAgent, Content: "Looks good."},
		},
		Meta: worklist.Metadata{HiddenAt: hidden},
	}}}

	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}

	var got *worklist.WorkItem
	for i := range sink.items {
		if sink.items[i].ID == "github:o/r#7" {
			got = &sink.items[i]
		}
	}
	if got == nil {
		t.Fatal("item #7 missing after re-ingest")
	}
	if len(got.Thread) != 2 {
		t.Fatalf("re-ingest wiped the thread: got %d turns, want 2", len(got.Thread))
	}
	if got.Meta.HiddenAt.IsZero() {
		t.Error("re-ingest dropped the user's hidden override")
	}
	if got.GitHub.Title != "Fresh title" {
		t.Errorf("re-ingest should refresh GitHub fields, got title %q", got.GitHub.Title)
	}
}

// runRank asks the ranker to propose axes and writes the proposal back.
func TestRunRankProposesAxes(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:      "github:o/r#1",
		Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}},
	}}}
	err := Run(context.Background(), Params{JobType: JobRank}, fakeVendor{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.items[0].Signals.Proposed == nil {
		t.Fatal("expected the stub ranker to set a proposal")
	}
}

func TestRunUnknownJobType(t *testing.T) {
	if err := Run(context.Background(), Params{JobType: "nope"}, fakeVendor{}, &memSink{}); err == nil {
		t.Fatal("expected an error for an unknown job type")
	}
}

// ingestServer answers the two radar queries with the given open PR numbers.
// incomplete sets GitHub's incomplete_results flag on the involves: query. Timeline
// calls (enrich) are counted per item number.
func ingestServer(t *testing.T, open []int, incomplete bool, timelines map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case r.URL.Path == "/search/issues":
			if strings.Contains(r.URL.Query().Get("q"), "review-requested:@me") {
				_, _ = w.Write([]byte(`{"items":[]}`))
				return
			}
			var items []string
			for _, n := range open {
				items = append(items, fmt.Sprintf(`{"number":%d,"title":"T","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}`, n))
			}
			fmt.Fprintf(w, `{"incomplete_results":%v,"items":[%s]}`, incomplete, strings.Join(items, ","))
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			timelines[r.URL.Path]++
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
}

func prItem(n int, thread ...worklist.Message) worklist.WorkItem {
	return worklist.WorkItem{
		ID: fmt.Sprintf("github:o/r#%d", n), Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: n, State: "open"},
		Thread: thread,
	}
}

func itemByID(t *testing.T, items []worklist.WorkItem, id string) worklist.WorkItem {
	t.Helper()
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("item %s missing", id)
	return worklist.WorkItem{}
}

// After a COMPLETE fetch, a stored item that is no longer in GitHub's open set (merged,
// closed) is retired: stamped completed, its thread kept, and enrich stops spending a
// timeline call on it. Items still open stay live.
func TestRunIngestRetiresItemsThatLeftTheOpenSet(t *testing.T) {
	timelines := map[string]int{}
	srv := ingestServer(t, []int{7}, false, timelines)
	defer srv.Close()
	thread := worklist.Message{Role: worklist.RoleUser, Content: "review?"}
	sink := &memSink{items: []worklist.WorkItem{prItem(7), prItem(8, thread)}}

	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, sink.items, "github:o/r#7"); !got.Meta.CompletedAt.IsZero() {
		t.Error("an item still open on GitHub was retired")
	}
	merged := itemByID(t, sink.items, "github:o/r#8")
	if merged.Meta.CompletedAt.IsZero() {
		t.Fatal("an item missing from a complete fetch was not retired")
	}
	if len(merged.Thread) != 1 {
		t.Errorf("retiring must keep the thread; got %d turns", len(merged.Thread))
	}
	for path, n := range timelines {
		if strings.Contains(path, "/issues/8/") {
			t.Errorf("enrich fetched the timeline of a retired item (%s, %d calls)", path, n)
		}
	}
	if timelines["/repos/o/r/issues/7/timeline"] == 0 {
		t.Errorf("enrich should still cover the open item; timeline calls: %v", timelines)
	}

	// A second ingest leaves the retirement time alone instead of re-stamping it.
	first := merged.Meta.CompletedAt
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, sink.items, "github:o/r#8").Meta.CompletedAt; !got.Equal(first) {
		t.Errorf("re-ingest re-stamped CompletedAt: %v -> %v", first, got)
	}
}

// A PARTIAL fetch proves nothing about what it did not return, so nothing is retired.
func TestRunIngestPartialFetchRetiresNothing(t *testing.T) {
	srv := ingestServer(t, []int{7}, true, map[string]int{})
	defer srv.Close()
	sink := &memSink{items: []worklist.WorkItem{prItem(7), prItem(8)}}
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, sink.items, "github:o/r#8"); !got.Meta.CompletedAt.IsZero() {
		t.Error("an item missing from a PARTIAL fetch was retired")
	}
}

// A complete fetch with no open work at all retires everything stored.
func TestRunIngestEmptyCompleteFetchRetiresAll(t *testing.T) {
	srv := ingestServer(t, nil, false, map[string]int{})
	defer srv.Close()
	sink := &memSink{items: []worklist.WorkItem{prItem(7), prItem(8)}}
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	for _, it := range sink.items {
		if it.Meta.CompletedAt.IsZero() {
			t.Errorf("%s was not retired after a complete, empty fetch", it.ID)
		}
	}
}

// A retired item that shows up open again (reopened, or the user is involved again)
// resurfaces: the fresh fetch clears CompletedAt while the thread is kept.
func TestRunIngestReopenedItemResurfaces(t *testing.T) {
	srv := ingestServer(t, []int{8}, false, map[string]int{})
	defer srv.Close()
	retired := prItem(8, worklist.Message{Role: worklist.RoleUser, Content: "review?"})
	retired.Meta.CompletedAt = time.Now().Add(-24 * time.Hour).UTC()
	sink := &memSink{items: []worklist.WorkItem{retired}}
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	got := itemByID(t, sink.items, "github:o/r#8")
	if !got.Meta.CompletedAt.IsZero() {
		t.Error("a reopened item stayed retired")
	}
	if len(got.Thread) != 1 {
		t.Errorf("resurfacing must keep the thread; got %d turns", len(got.Thread))
	}
}
