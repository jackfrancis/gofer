package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeVendor struct{ token string }

func (f fakeVendor) Vend(context.Context, string) (string, error) { return f.token, nil }

// memSink is an in-memory Sink with the agent plane's write semantics: a write-back
// is merged into the stored item (worklist.MergeRunItems) and a reply is appended
// through worklist.AppendReply, and a research adjustment is set alone (as
// worklist.ApplyResearch does), so tests see what a real run's writes produce.
// Reads go through worklist.Find and worklist.Select, as on the agent plane.
type memSink struct {
	items      []worklist.WorkItem
	ingests    int                  // write-back calls, so batching is observable
	replies    int                  // Reply calls
	researches int                  // Research calls
	selects    []worklist.Selection // Select calls, so paging is observable

	// beforeIngest, if set, runs once just before the first write-back is merged.
	beforeIngest func()
}

func (m *memSink) Item(_ context.Context, id, turn string) (worklist.WorkItem, bool, error) {
	it, ok := worklist.Find(m.items, id)
	if !ok {
		return worklist.WorkItem{}, false, nil
	}
	return worklist.ThreadWindow(it, turn, worklist.MaxItemBytes), true, nil
}

func (m *memSink) Select(_ context.Context, sel worklist.Selection) ([]worklist.WorkItem, string, error) {
	m.selects = append(m.selects, sel)
	page, next := worklist.Select(m.items, sel, time.Now().UTC())
	return page, next, nil
}

func (m *memSink) Ingest(_ context.Context, items []worklist.WorkItem) error {
	m.ingests++
	if f := m.beforeIngest; f != nil {
		m.beforeIngest = nil
		f() // what lands in the store between the run's read and its first write
	}
	m.put(worklist.MergeRunItems(m.items, items))
	return nil
}

func (m *memSink) Reply(_ context.Context, itemID, replyTo string, reply worklist.Message) error {
	m.replies++
	for i := range m.items {
		if m.items[i].ID == itemID {
			_, err := worklist.AppendReply(&m.items[i], replyTo, reply)
			return err
		}
	}
	return nil
}

func (m *memSink) Research(_ context.Context, itemID string, adj worklist.ResearchAdjustment) error {
	m.researches++
	for i := range m.items {
		if m.items[i].ID == itemID {
			_, err := worklist.SetResearch(&m.items[i], adj)
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
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
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
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
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
		ThreadReadThrough: "pos:1",
		Meta:              worklist.Metadata{HiddenAt: hidden},
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
	if got.ThreadReadThrough != "pos:1" || got.HasUnreadReply() {
		t.Errorf("re-ingest dropped the read cursor, got %q", got.ThreadReadThrough)
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
	var mu sync.Mutex // enrich fetches timelines concurrently
	return httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
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
			mu.Lock()
			timelines[r.URL.Path]++
			mu.Unlock()
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

// The public-only guard in ingest (ADR 0007): a hit from a private repository is never
// written, and a repository whose visibility check has no answer makes the fetch
// incomplete, so a stored item that is missing from it is not retired.
func TestRunIngestPublicOnly(t *testing.T) {
	var mu sync.Mutex
	flaky := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := func(repo string, n int) string {
			return fmt.Sprintf(`{"number":%d,"title":"T","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/%s","pull_request":{"url":"x"}}`, n, repo)
		}
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case "/search/issues":
			if strings.Contains(r.URL.Query().Get("q"), "is:pr is:open involves:@me") {
				fmt.Fprintf(w, `{"items":[%s,%s]}`, hit("o/r", 7), hit("o/secret", 1))
				return
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		case "/repos/o/r":
			mu.Lock()
			f := flaky
			mu.Unlock()
			if f {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"id":1,"full_name":"o/r","private":false}`))
		case "/repos/o/secret":
			_, _ = w.Write([]byte(`{"id":2,"full_name":"o/secret","private":true}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/repos/o/secret/") {
				t.Errorf("request to a private repository: %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	params := Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()}

	sink := &memSink{}
	if err := Run(context.Background(), params, fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	for _, it := range sink.items {
		if it.GitHub.Repo == "o/secret" {
			t.Fatalf("a private repository's hit was written: %s", it.ID)
		}
	}
	itemByID(t, sink.items, "github:o/r#7")

	// o/r's check now has no answer: its hit is skipped and the stored #8 (absent from
	// the search) must not be retired, since the fetch is no longer complete.
	mu.Lock()
	flaky = true
	mu.Unlock()
	sink = &memSink{items: []worklist.WorkItem{prItem(7), prItem(8)}}
	if err := Run(context.Background(), params, fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	for _, it := range sink.items {
		if !it.Meta.CompletedAt.IsZero() {
			t.Errorf("%s was retired after a fetch with an unconfirmed repository", it.ID)
		}
	}
}

// failRanker fails every proposal, as a model outage or a malformed answer would.
type failRanker struct{}

func (failRanker) Propose(context.Context, worklist.WorkItem) (worklist.AxisProposal, error) {
	return worklist.AxisProposal{}, fmt.Errorf("model unavailable")
}

// freshPR7GitHub is a GitHub whose only open work is o/r#7, titled "Fresh title".
func freshPR7GitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)
	return srv
}

// A Refresh re-fetches an item that already carries a model proposal and a research
// adjustment. The fetch derives neither, and the chained rank fails here, so both
// stored values must survive while the GitHub fields refresh.
func TestRunIngestKeepsModelSignals(t *testing.T) {
	srv := freshPR7GitHub(t)

	prop := worklist.AxisProposal{Relevance: 0.9, Impact: 0.8, Engagement: 0.7, Urgency: 0.6, Confidence: 1, Rationale: "model proposal"}
	adj := worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 0.5, Rationale: "from the discussion"}
	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#7", Source: "github", Type: worklist.TypePullRequest,
		GitHub:  worklist.GitHubRef{Repo: "o/r", Number: 7, Title: "Stale title"},
		Signals: worklist.Signals{Proposed: &prop, Research: &adj},
	}}}

	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client(), Ranker: failRanker{}},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	got := sink.items[0]
	if got.GitHub.Title != "Fresh title" {
		t.Fatalf("refresh should take the fetched title, got %q", got.GitHub.Title)
	}
	if got.Signals.Proposed == nil || *got.Signals.Proposed != prop {
		t.Errorf("refresh dropped the model proposal: got %+v", got.Signals.Proposed)
	}
	if got.Signals.Research == nil || !reflect.DeepEqual(*got.Signals.Research, adj) {
		t.Errorf("refresh dropped the research adjustment: got %+v", got.Signals.Research)
	}
}

// A scoring pass that fails for an item leaves its previous proposal in place, and
// one that succeeds replaces it.
func TestRunRankFailureKeepsPreviousProposal(t *testing.T) {
	prev := worklist.AxisProposal{Relevance: 0.9, Confidence: 1, Rationale: "previous"}
	seed := func() *memSink {
		return &memSink{items: []worklist.WorkItem{{
			ID:      "github:o/r#1",
			Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}, Proposed: &prev},
		}}}
	}

	sink := seed()
	if err := Run(context.Background(), Params{JobType: JobRank, Ranker: failRanker{}}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	if p := sink.items[0].Signals.Proposed; p == nil || *p != prev {
		t.Fatalf("a failed scoring pass must keep the previous proposal, got %+v", p)
	}

	sink = seed()
	if err := Run(context.Background(), Params{JobType: JobRank}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	if p := sink.items[0].Signals.Proposed; p == nil || p.Rationale == "previous" {
		t.Fatalf("a successful scoring pass must replace the proposal, got %+v", p)
	}
}

// A discussion saves a new research adjustment after the Refresh read the items but
// before it wrote them back. The Refresh must not restore the adjustment it read.
func TestRunIngestKeepsResearchWrittenMeanwhile(t *testing.T) {
	srv := freshPR7GitHub(t)
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	older := worklist.ResearchAdjustment{Relevance: 0.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "older", AppliedAt: t0}
	newer := worklist.ResearchAdjustment{Relevance: 1.8, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "newer", AppliedAt: t0.Add(time.Hour)}
	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#7", Source: "github", Type: worklist.TypePullRequest,
		GitHub:  worklist.GitHubRef{Repo: "o/r", Number: 7, Title: "Stale title"},
		Signals: worklist.Signals{Research: &older},
	}}}
	sink.beforeIngest = func() {
		if err := sink.Research(context.Background(), "github:o/r#7", newer); err != nil {
			t.Error(err)
		}
	}
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client(), Ranker: failRanker{}},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	got := sink.items[0]
	if got.GitHub.Title != "Fresh title" {
		t.Fatalf("refresh should take the fetched title, got %q", got.GitHub.Title)
	}
	if r := got.Signals.Research; r == nil || r.Rationale != "newer" {
		t.Fatalf("the refresh restored the adjustment it read over a newer one: got %+v", r)
	}
}

// sideEffectRanker records which items reach the model and runs during() inside the
// first call, to stand in for what lands in the web tier meanwhile.
type sideEffectRanker struct {
	mu     sync.Mutex
	got    []string
	during func()
}

func (r *sideEffectRanker) Propose(_ context.Context, it worklist.WorkItem) (worklist.AxisProposal, error) {
	r.mu.Lock()
	r.got = append(r.got, it.ID)
	f := r.during
	r.during = nil
	r.mu.Unlock()
	if f != nil {
		f()
	}
	return worklist.AxisProposal{Relevance: 0.5, Impact: 0.5, Engagement: 0.5, Urgency: 0.5, Confidence: 1, Rationale: "new"}, nil
}

// With room for one item, the rank pass picks by the deterministic baseline: a newly
// requested review with an old, low proposal goes to the model ahead of an item the
// user merely commented on. A stored proposal must not decide its own refresh.
func TestRunRankSelectsOnBaseline(t *testing.T) {
	now := time.Now().UTC()
	low := worklist.AxisProposal{Relevance: 0.01, Impact: 0.01, Engagement: 0.01, Urgency: 0.01, Confidence: 1, Rationale: "old"}
	requested := worklist.WorkItem{ID: "requested", Signals: worklist.Signals{
		Reasons: []worklist.Reason{worklist.ReasonReviewRequested}, AwaitingMeSince: now.Add(-time.Hour), Proposed: &low,
	}}
	commented := worklist.WorkItem{ID: "commented", Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonCommented}}}
	// The premise: the stored proposal ranks the review below the comment, and the
	// baseline ranks it above.
	if worklist.Score(requested, now).Rank >= worklist.Score(commented, now).Rank {
		t.Fatal("test premise: the old proposal should rank the review lower")
	}
	if worklist.BaselineScore(requested, now).Rank <= worklist.BaselineScore(commented, now).Rank {
		t.Fatal("test premise: the baseline should rank the review higher")
	}

	sink := &memSink{items: []worklist.WorkItem{commented, requested}}
	rr := &sideEffectRanker{}
	if err := Run(context.Background(), Params{JobType: JobRank, Ranker: rr, RankLimit: 1}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	if len(rr.got) != 1 || rr.got[0] != "requested" {
		t.Fatalf("want the requested review re-ranked, got %v", rr.got)
	}
}

// A research adjustment that lands while a rank pass is asking the model survives
// the pass's write-back: rank sends only the proposal it derived.
func TestRunRankKeepsResearchWrittenMeanwhile(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	older := worklist.ResearchAdjustment{Relevance: 0.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "older", AppliedAt: t0}
	newer := worklist.ResearchAdjustment{Relevance: 1.8, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "newer", AppliedAt: t0.Add(time.Hour)}
	sink := &memSink{items: []worklist.WorkItem{{ID: "github:o/r#1", Signals: worklist.Signals{Research: &older}}}}
	rr := &sideEffectRanker{during: func() { sink.items[0].Signals.Research = &newer }}
	if err := Run(context.Background(), Params{JobType: JobRank, Ranker: rr}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	got := sink.items[0].Signals
	if got.Proposed == nil || got.Proposed.Rationale != "new" {
		t.Fatalf("the new proposal must land, got %+v", got.Proposed)
	}
	if got.Research == nil || got.Research.Rationale != "newer" {
		t.Fatalf("rank restored the adjustment it read over a newer one: got %+v", got.Research)
	}
}

// A Refresh of a still-pending review keeps who requested it: GitHub's search does
// not say. Here enrich cannot read the timeline and rank fails, so nothing replaces
// the stored values. The bot policy had set aside a proposal made when the requester
// counted as human; it must stay set aside rather than apply again because the
// refresh blanked the requester.
func TestRunIngestKeepsPendingRequester(t *testing.T) {
	store := worklist.NewMemoryStore()
	botRequestSeed(store, time.Time{})
	got := runRefresh(t, freshPR7GitHub(t), store)
	if got.GitHub.Title != "Fresh title" {
		t.Fatalf("refresh should take the fetched title, got %q", got.GitHub.Title)
	}
	if got.Signals.ReviewRequestedBy != "k8s-ci-robot" || !got.Signals.ReviewRequestedByBot {
		t.Fatalf("refresh blanked the pending request's requester: %q bot=%v", got.Signals.ReviewRequestedBy, got.Signals.ReviewRequestedByBot)
	}
	if p := got.Signals.CurrentProposal(); p != nil {
		t.Fatalf("a proposal the bot policy had set aside applies again: %+v", p)
	}
}

// The pending request is carried while it may still be pending: when the fetch
// still shows it, or when the review search was partial and could have missed it.
// A complete search that no longer shows it confirms the request is gone.
func TestCarryPendingRequest(t *testing.T) {
	stored := worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}, ReviewRequestedBy: "alice"}

	pending := worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}}
	carryPendingRequest(&pending, stored, true)
	if pending.ReviewRequestedBy != "alice" {
		t.Fatalf("still requested: want the requester kept, got %q", pending.ReviewRequestedBy)
	}

	removed := worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonAuthor}}
	carryPendingRequest(&removed, stored, true)
	if removed.ReviewRequestedBy != "" || slices.Contains(removed.Reasons, worklist.ReasonReviewRequested) {
		t.Fatalf("a complete search confirms the request is gone; got %+v", removed)
	}

	missed := worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonAuthor}}
	if !carryPendingRequest(&missed, stored, false) {
		t.Fatal("a request kept after a partial search must be reported as inferred")
	}
	if missed.ReviewRequestedBy != "alice" || !slices.Contains(missed.Reasons, worklist.ReasonReviewRequested) || !slices.Contains(missed.Reasons, worklist.ReasonAuthor) {
		t.Fatalf("a partial search proves nothing: want the request kept, got %+v", missed)
	}

	// The store learned the requester from a timeline before it saw the request; a
	// search that now shows the request keeps it, and nothing was inferred.
	timelineOnly := worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonAuthor}, ReviewRequestedBy: "bob"}
	shown := worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}}
	if carryPendingRequest(&shown, timelineOnly, true) {
		t.Fatal("a request the search showed is not inferred")
	}
	if shown.ReviewRequestedBy != "bob" {
		t.Fatalf("want the timeline's requester kept, got %q", shown.ReviewRequestedBy)
	}
	// Without a stored request, a partial search infers none.
	none := worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonAuthor}}
	if carryPendingRequest(&none, timelineOnly, false) || slices.Contains(none.Reasons, worklist.ReasonReviewRequested) {
		t.Fatalf("no request was stored; none may be inferred, got %+v", none)
	}
}

// botRequestSeed is a stored PR whose review a bot requested, carrying a proposal
// made when that requester still counted as human: the bot policy sets it aside.
func botRequestSeed(store *worklist.MemoryStore, completedAt time.Time) *worklist.AxisProposal {
	human := &worklist.AxisProposal{Relevance: 0.9, Impact: 0.9, Engagement: 0.9, Urgency: 0.9, Confidence: 1, Rationale: "scored as a human ask"}
	store.Seed("u1", worklist.WorkItem{
		ID: "github:o/r#7", OwnerID: "u1", Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 7, Title: "Stale title"},
		Signals: worklist.Signals{
			Reasons:           []worklist.Reason{worklist.ReasonReviewRequested},
			ReviewRequestedBy: "k8s-ci-robot", ReviewRequestedByBot: true,
			Proposed: human,
		},
		Meta: worklist.Metadata{CompletedAt: completedAt},
	})
	return human
}

// runRefresh runs an ingest against srv with enrich unable to read timelines (the
// stubs answer them with an object) and ranking failing, so nothing replaces what
// the refresh itself stores, and returns the stored item.
func runRefresh(t *testing.T, srv *httptest.Server, store *worklist.MemoryStore) worklist.WorkItem {
	t.Helper()
	sink := NewStoreSink(store, "u1").WithPolicy(worklist.NewRunPolicy([]string{"k8s-ci-robot"}))
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client(), Ranker: failRanker{}},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	items, _ := store.List(context.Background(), "u1")
	return items[0]
}

// The review search reports incomplete results and misses the PR, which arrives
// through involves: alone. That is no evidence the request was removed: the request
// and its bot requester are kept, and the set-aside proposal stays set aside.
func TestRunIngestPartialReviewSearchKeepsRequest(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.Contains(q, "review-requested:@me"):
			_, _ = w.Write([]byte(`{"incomplete_results":true,"items":[]}`))
		case strings.Contains(q, "is:pr") && strings.Contains(q, "involves:@me"):
			_, _ = w.Write([]byte(`{"items":[{"number":7,"title":"Fresh title","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}]}`))
		default:
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	store := worklist.NewMemoryStore()
	botRequestSeed(store, time.Time{})

	got := runRefresh(t, srv, store)
	if got.GitHub.Title != "Fresh title" {
		t.Fatalf("refresh should take the fetched title, got %q", got.GitHub.Title)
	}
	if !slices.Contains(got.Signals.Reasons, worklist.ReasonReviewRequested) {
		t.Fatalf("a partial review search dropped the pending request: reasons %v", got.Signals.Reasons)
	}
	if got.Signals.ReviewRequestedBy != "k8s-ci-robot" || !got.Signals.ReviewRequestedByBot {
		t.Fatalf("a partial review search blanked the requester: %q bot=%v", got.Signals.ReviewRequestedBy, got.Signals.ReviewRequestedByBot)
	}
	if p := got.Signals.CurrentProposal(); p != nil {
		t.Fatalf("a proposal the bot policy had set aside applies again: %+v", p)
	}
}

// A complete fetch retired the PR; it later resurfaces with a new review request.
// The retired request's bot requester must not be carried onto the new one.
func TestRunIngestResurfacedItemDropsOldRequester(t *testing.T) {
	store := worklist.NewMemoryStore()
	botRequestSeed(store, time.Now().Add(-24*time.Hour).UTC())

	got := runRefresh(t, freshPR7GitHub(t), store)
	if !got.Meta.CompletedAt.IsZero() {
		t.Fatal("the resurfaced item should be open again")
	}
	if got.Signals.ReviewRequestedBy != "" || got.Signals.ReviewRequestedByBot {
		t.Fatalf("the resurfaced request inherited the retired one's requester: %q bot=%v", got.Signals.ReviewRequestedBy, got.Signals.ReviewRequestedByBot)
	}
}

// reviewSnapshot runs a full refresh in which the involvement search finds o/r#7.
// reviewFound says whether the review-requested search shows it too (otherwise that
// search reports incomplete results). timeline is the PR's timeline body; empty
// makes the timeline unavailable (404), and longTimeline serves a timeline longer
// than enrich reads. Ranking runs with the stub ranker.
func reviewSnapshot(t *testing.T, sink Sink, reviewFound bool, timeline string, longTimeline bool) worklist.WorkItem {
	t.Helper()
	const hit = `{"number":7,"title":"PR","state":"open","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}`
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"me"}`)
		case strings.Contains(q, "review-requested:@me"):
			if reviewFound {
				fmt.Fprintf(w, `{"total_count":1,"items":[%s]}`, hit)
			} else {
				fmt.Fprint(w, `{"incomplete_results":true,"items":[]}`)
			}
		case strings.Contains(q, "is:pr") && strings.Contains(q, "involves:@me"):
			fmt.Fprintf(w, `{"total_count":1,"items":[%s]}`, hit)
		case r.URL.Path == "/search/issues":
			fmt.Fprint(w, `{"items":[]}`)
		case strings.HasSuffix(r.URL.Path, "/timeline") && longTimeline:
			w.Header().Set("Link", fmt.Sprintf(`<%s?page=next>; rel="next"`, r.URL.Path))
			fmt.Fprint(w, `[{"event":"commented","created_at":"2026-01-03T09:00:00Z","actor":{"login":"bob"}}]`)
		case strings.HasSuffix(r.URL.Path, "/timeline") && timeline != "":
			fmt.Fprint(w, timeline)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := Run(ctx, Params{JobType: JobIngest, GitHubBaseURL: srv.URL, Client: srv.Client()}, fakeVendor{token: "test"}, sink); err != nil {
		t.Fatal(err)
	}
	items, err := readAll(ctx, Params{}, sink, worklist.Selection{})
	if err != nil || len(items) != 1 {
		t.Fatalf("list returned %d items, error %v", len(items), err)
	}
	return items[0]
}

// Refreshes against a review search and a timeline that disagree or fall short. Only
// positive evidence changes what is known about the user's review request: a
// complete search showing it, or a whole timeline showing who asked or that it ended.
func TestRefreshReviewRequestEvidence(t *testing.T) {
	const requested = `{"event":"review_requested","created_at":"2026-01-01T09:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"review-bot"}}`
	const withdrawn = `{"event":"review_request_removed","created_at":"2026-01-02T09:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"review-bot"}}`
	const reviewed = `{"event":"reviewed","submitted_at":"2026-01-02T09:00:00Z","state":"commented","user":{"login":"me"}}`
	newSink := func() Sink {
		return NewStoreSink(worklist.NewMemoryStore(), "owner").WithPolicy(worklist.NewRunPolicy([]string{"review-bot"}))
	}
	pending := func(it worklist.WorkItem) bool {
		return slices.Contains(it.Signals.Reasons, worklist.ReasonReviewRequested)
	}

	// A partial search kept the request; the whole timeline shows it withdrawn.
	t.Run("withdrawal ends a carried request", func(t *testing.T) {
		sink := newSink()
		reviewSnapshot(t, sink, true, "["+requested+"]", false)
		item := reviewSnapshot(t, sink, false, "["+requested+","+withdrawn+"]", false)
		if pending(item) || item.Signals.ReviewRequestedBy != "" {
			t.Fatalf("withdrawn request still counted: reasons %v, requester %q", item.Signals.Reasons, item.Signals.ReviewRequestedBy)
		}
	})

	// A partial search kept the request; the timeline is longer than enrich reads,
	// so it proves nothing: the request and its bot requester stay.
	t.Run("partial timeline keeps a carried request", func(t *testing.T) {
		sink := newSink()
		reviewSnapshot(t, sink, true, "["+requested+"]", false)
		item := reviewSnapshot(t, sink, false, "", true)
		if !pending(item) || item.Signals.ReviewRequestedBy != "review-bot" || !item.Signals.ReviewRequestedByBot {
			t.Fatalf("an inconclusive timeline changed the request: reasons %v, requester %q bot=%v", item.Signals.Reasons, item.Signals.ReviewRequestedBy, item.Signals.ReviewRequestedByBot)
		}
	})

	// The search itself shows the request, though the user's own review ended their
	// personal one (a team request names the team, not them): the search wins.
	t.Run("a search-confirmed request survives the timeline", func(t *testing.T) {
		sink := newSink()
		item := reviewSnapshot(t, sink, true, "["+requested+","+reviewed+"]", false)
		if !pending(item) {
			t.Fatalf("a request the search showed was dropped: reasons %v", item.Signals.Reasons)
		}
	})

	// The user reviewed personally, then their team was asked. A complete search
	// records the team request; the next, partial one misses it. The timeline,
	// unchanged, shows the personal request ended but a team request after it, which
	// may include the user: the kept request stays.
	t.Run("a team request after the user's review stays", func(t *testing.T) {
		sink := newSink()
		const team = `{"event":"review_requested","created_at":"2026-01-03T09:00:00Z","requested_team":{"slug":"my-team"},"actor":{"login":"bob"}}`
		timeline := "[" + requested + "," + reviewed + "," + team + "]"
		if first := reviewSnapshot(t, sink, true, timeline, false); !pending(first) {
			t.Fatalf("setup: the confirmed team request was not recorded: reasons %v", first.Signals.Reasons)
		}
		if item := reviewSnapshot(t, sink, false, timeline, false); !pending(item) {
			t.Fatalf("a partial search lost the team request: reasons %v", item.Signals.Reasons)
		}
	})

	// The search confirmed a request; the next, partial one misses it, and the whole
	// timeline shows the last outstanding request ended — a team-only request
	// withdrawn, or a personal one withdrawn after the user's review fulfilled the
	// team's. The kept request goes.
	for name, events := range map[string][]string{
		"team-only request withdrawn": {
			`{"event":"review_requested","created_at":"2026-01-01T09:00:00Z","requested_team":{"slug":"my-team"},"actor":{"login":"alice"}}`,
			`{"event":"review_request_removed","created_at":"2026-01-02T09:00:00Z","requested_team":{"slug":"my-team"},"actor":{"login":"alice"}}`,
		},
		"personal withdrawal after a fulfilled team request": {
			`{"event":"review_requested","created_at":"2026-01-01T09:00:00Z","requested_team":{"slug":"my-team"},"actor":{"login":"alice"}}`,
			`{"event":"reviewed","submitted_at":"2026-01-02T09:00:00Z","state":"approved","user":{"login":"me"}}`,
			`{"event":"review_requested","created_at":"2026-01-03T09:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}`,
			`{"event":"review_request_removed","created_at":"2026-01-04T09:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}`,
		},
	} {
		t.Run(name+" ends a carried request", func(t *testing.T) {
			sink := newSink()
			if first := reviewSnapshot(t, sink, true, "["+strings.Join(events[:len(events)-1], ",")+"]", false); !pending(first) {
				t.Fatalf("setup: the confirmed request was not recorded: reasons %v", first.Signals.Reasons)
			}
			if item := reviewSnapshot(t, sink, false, "["+strings.Join(events, ",")+"]", false); pending(item) {
				t.Fatalf("the last outstanding request ended, but reasons are %v", item.Signals.Reasons)
			}
		})
	}

	// The first refresh learns the bot requester from the timeline while its partial
	// search misses the request; the next one confirms the request but cannot read the
	// timeline. The known requester stays.
	t.Run("confirmed request keeps its timeline requester", func(t *testing.T) {
		sink := newSink()
		first := reviewSnapshot(t, sink, false, "["+requested+"]", false)
		if first.Signals.ReviewRequestedBy != "review-bot" {
			t.Fatalf("setup: want the timeline's requester, got %+v", first.Signals)
		}
		item := reviewSnapshot(t, sink, true, "", false)
		if item.Signals.ReviewRequestedBy != "review-bot" || !item.Signals.ReviewRequestedByBot {
			t.Fatalf("the confirmed review lost its requester: %q bot=%v", item.Signals.ReviewRequestedBy, item.Signals.ReviewRequestedByBot)
		}
	})
}

// A partial fetch retires nothing it merely did not see, but a stored pull request
// the fetch SAW as another contributor's draft is retired all the same (thread kept):
// otherwise an account whose issue search never completes would keep a converted
// draft on the radar as ready indefinitely.
func TestRunIngestPartialFetchRetiresOthersDrafts(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			switch {
			case strings.Contains(q, "is:issue"):
				// The issue search is partial, so the whole fetch is.
				_, _ = w.Write([]byte(`{"incomplete_results":true,"items":[]}`))
			case strings.Contains(q, "involves:@me"):
				_, _ = w.Write([]byte(`{"total_count":2,"items":[
					{"number":7,"title":"T","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/r","user":{"login":"someone"},"pull_request":{"url":"x"}},
					{"number":8,"title":"T","html_url":"u","state":"open","draft":true,"repository_url":"https://api.github.com/repos/o/r","user":{"login":"someone"},"pull_request":{"url":"x"}}]}`))
			default:
				_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
			}
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	thread := worklist.Message{Role: worklist.RoleUser, Content: "review?"}
	sink := &memSink{items: []worklist.WorkItem{prItem(7), prItem(8, thread), prItem(9)}}
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	draft := itemByID(t, sink.items, "github:o/r#8")
	if draft.Meta.CompletedAt.IsZero() {
		t.Fatal("another contributor's draft stayed on the radar after a partial fetch")
	}
	if len(draft.Thread) != 1 {
		t.Errorf("retiring must keep the thread; got %d turns", len(draft.Thread))
	}
	if got := itemByID(t, sink.items, "github:o/r#7"); !got.Meta.CompletedAt.IsZero() {
		t.Error("a ready pull request was retired")
	}
	if got := itemByID(t, sink.items, "github:o/r#9"); !got.Meta.CompletedAt.IsZero() {
		t.Error("an item missing from a PARTIAL fetch was retired")
	}
}

// draftServer answers the radar queries with one pull request, #8, shown as another
// contributor's draft last updated at draftAt. partial makes the issue search
// incomplete, and with it the whole fetch.
func draftServer(t *testing.T, draftAt time.Time, partial bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			switch {
			case strings.Contains(q, "is:issue"):
				fmt.Fprintf(w, `{"incomplete_results":%v,"total_count":0,"items":[]}`, partial)
			case strings.Contains(q, "involves:@me"):
				fmt.Fprintf(w, `{"total_count":1,"items":[{"number":8,"title":"T","html_url":"u","state":"open","draft":true,"updated_at":%q,"repository_url":"https://api.github.com/repos/o/r","user":{"login":"someone"},"pull_request":{"url":"x"}}]}`,
					draftAt.Format(time.RFC3339))
			default:
				_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
			}
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
}

// The search index can lag an item: a draft result older than the ready state the
// store already holds is stale, so neither a partial nor a complete refresh retires
// the ready PR on its strength. On a tie the draft wins (GitHub's timestamps are to
// the second), and a stored copy that is itself a draft is retired whatever its time.
func TestRunIngestStaleDraftResultKeepsNewerReadyPR(t *testing.T) {
	draftAt := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		storedAt  time.Time
		storedDft bool
		retired   bool
	}{
		{"stored ready is newer", draftAt.Add(4 * time.Minute), false, false},
		{"stored ready is older", draftAt.Add(-4 * time.Minute), false, true},
		{"tie", draftAt, false, true},
		{"stored copy is a newer draft", draftAt.Add(4 * time.Minute), true, true},
	}
	for _, partial := range []bool{true, false} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/partial=%v", tc.name, partial), func(t *testing.T) {
				srv := draftServer(t, draftAt, partial)
				defer srv.Close()
				stored := prItem(8, worklist.Message{Role: worklist.RoleUser, Content: "review?"})
				stored.GitHub.UpdatedAt, stored.GitHub.Draft = tc.storedAt, tc.storedDft
				sink := &memSink{items: []worklist.WorkItem{stored}}
				if err := Run(context.Background(),
					Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
					fakeVendor{token: "tok"}, sink); err != nil {
					t.Fatal(err)
				}
				got := itemByID(t, sink.items, "github:o/r#8")
				if retired := !got.Meta.CompletedAt.IsZero(); retired != tc.retired {
					t.Fatalf("retired = %v, want %v", retired, tc.retired)
				}
				if len(got.Thread) != 1 {
					t.Errorf("thread has %d turns, want 1", len(got.Thread))
				}
			})
		}
	}
}

// lifecycleServer answers the radar queries with the given pull request hits, from
// repository o/r, whose visibility lookup answers private as given. partial makes
// the issue search incomplete, and with it the whole fetch.
func lifecycleServer(t *testing.T, partial, private bool, hits ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := githubtest.RepoLookup(r); ok {
			fmt.Fprintf(w, `{"id":1,"full_name":%q,"private":%v}`, name, private)
			return
		}
		q := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case r.URL.Path == "/search/issues" && strings.Contains(q, "is:issue"):
			fmt.Fprintf(w, `{"incomplete_results":%v,"total_count":0,"items":[]}`, partial)
		case r.URL.Path == "/search/issues" && strings.Contains(q, "involves:@me"):
			fmt.Fprintf(w, `{"total_count":%d,"items":[%s]}`, len(hits), strings.Join(hits, ","))
		case r.URL.Path == "/search/issues":
			_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// lifecycleHit is another contributor's pull request #n at 12:mm.
func lifecycleHit(n int, draft bool, minute int) string {
	return fmt.Sprintf(`{"number":%d,"title":"T","html_url":"u","state":"open","draft":%v,"updated_at":"2026-11-23T12:%02d:00Z","repository_url":"https://api.github.com/repos/o/r","user":{"login":"someone"},"pull_request":{"url":"x"}}`,
		n, draft, minute)
}

func runLifecycleIngest(t *testing.T, srv *httptest.Server, sink *memSink) {
	t.Helper()
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
}

// Retiring a confirmed draft records its draft state and GitHub time, so a later
// refresh whose index still serves the older ready copy cannot reopen it; a ready
// copy GitHub updated after the draft is the PR marked ready, and resurfaces it with
// its thread. Partial and complete refreshes alike.
func TestRunIngestConfirmedDraftSurvivesStaleReadyResult(t *testing.T) {
	for _, partial := range []bool{true, false} {
		t.Run(fmt.Sprintf("partial=%v", partial), func(t *testing.T) {
			const id = "github:o/r#8"
			stored := prItem(8, worklist.Message{Role: worklist.RoleUser, Content: "note"})
			stored.GitHub.UpdatedAt = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
			sink := &memSink{items: []worklist.WorkItem{stored}}

			runLifecycleIngest(t, lifecycleServer(t, partial, false, lifecycleHit(8, true, 4)), sink)
			got := itemByID(t, sink.items, id)
			if got.OnRadar() {
				t.Fatal("the 12:04 draft was not retired")
			}
			if !got.GitHub.Draft || !got.GitHub.UpdatedAt.Equal(time.Date(2026, 11, 23, 12, 4, 0, 0, time.UTC)) {
				t.Errorf("retirement recorded draft=%v updated=%v, want the 12:04 draft", got.GitHub.Draft, got.GitHub.UpdatedAt)
			}
			retiredAt := got.Meta.CompletedAt

			runLifecycleIngest(t, lifecycleServer(t, partial, false, lifecycleHit(8, false, 0)), sink)
			if got := itemByID(t, sink.items, id); got.OnRadar() {
				t.Fatal("a stale 12:00 ready result reopened the 12:04 draft")
			}
			runLifecycleIngest(t, lifecycleServer(t, partial, false, lifecycleHit(8, false, 4)), sink)
			if got := itemByID(t, sink.items, id); got.OnRadar() {
				t.Fatal("a ready result tied with the draft reopened it")
			}
			// Seeing the same draft again writes nothing new.
			runLifecycleIngest(t, lifecycleServer(t, partial, false, lifecycleHit(8, true, 4)), sink)
			if got := itemByID(t, sink.items, id); !got.Meta.CompletedAt.Equal(retiredAt) {
				t.Errorf("re-seeing the draft re-stamped CompletedAt: %v -> %v", retiredAt, got.Meta.CompletedAt)
			}

			runLifecycleIngest(t, lifecycleServer(t, partial, false, lifecycleHit(8, false, 9)), sink)
			got = itemByID(t, sink.items, id)
			if !got.OnRadar() || got.GitHub.Draft {
				t.Fatalf("the 12:09 ready result did not resurface the PR (on radar %v, draft %v)", got.OnRadar(), got.GitHub.Draft)
			}
			if len(got.Thread) != 1 {
				t.Errorf("thread has %d turns, want 1", len(got.Thread))
			}
		})
	}
}

// A draft observation from a repository the gate confirms private is evidence of
// nothing, so it cannot keep a newer stored ready PR on the radar: a complete
// refresh retires it with everything else from that repository.
func TestRunIngestPrivateRepoDraftPreservesNothing(t *testing.T) {
	stored := prItem(8)
	stored.GitHub.UpdatedAt = time.Date(2026, 11, 23, 12, 4, 0, 0, time.UTC)
	sink := &memSink{items: []worklist.WorkItem{stored, prItem(9)}}
	runLifecycleIngest(t, lifecycleServer(t, false, true, lifecycleHit(8, true, 0), lifecycleHit(9, false, 4)), sink)
	if itemByID(t, sink.items, "github:o/r#9").OnRadar() {
		t.Fatal("a PR from a repository confirmed private stayed on the radar")
	}
	if itemByID(t, sink.items, "github:o/r#8").OnRadar() {
		t.Error("a draft result from a private repository kept a stored ready PR on the radar")
	}
}

// twoSearchServer answers the involves: and review-requested: pull request searches
// with independent snapshots of the same PR (an empty string answers nothing).
// partialReview makes the review-request search incomplete.
func twoSearchServer(t *testing.T, involved, review string, partialReview bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		if r.URL.Path != "/search/issues" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		hit, partial := "", false
		q := r.URL.Query().Get("q")
		switch {
		case strings.Contains(q, "review-requested:@me"):
			hit, partial = review, partialReview
		case strings.Contains(q, "is:pr") && strings.Contains(q, "involves:@me"):
			hit = involved
		}
		n := 0
		if hit != "" {
			n = 1
		}
		fmt.Fprintf(w, `{"total_count":%d,"items":[%s],"incomplete_results":%v}`, n, hit, partial)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A draft seen on first sight, with no stored row, is still remembered (as a row
// already retired, never on the radar), so a later partial refresh serving only the
// older ready copy cannot put it on the radar; a newer ready copy does.
func TestRunIngestRemembersFirstSeenDraft(t *testing.T) {
	const id = "github:o/r#8"
	sink := &memSink{}
	runLifecycleIngest(t, twoSearchServer(t, lifecycleHit(8, false, 0), lifecycleHit(8, true, 4), false), sink)
	got, ok := worklist.Find(sink.items, id)
	if !ok {
		t.Fatal("the first-seen draft was not remembered")
	}
	if got.OnRadar() || !got.GitHub.Draft {
		t.Fatalf("the first-seen draft is on radar %v, draft %v; want a retired draft", got.OnRadar(), got.GitHub.Draft)
	}
	runLifecycleIngest(t, twoSearchServer(t, lifecycleHit(8, false, 0), "", true), sink)
	if got := itemByID(t, sink.items, id); got.OnRadar() {
		t.Fatal("the older 12:00 ready copy put the 12:04 draft on the radar")
	}
	runLifecycleIngest(t, twoSearchServer(t, lifecycleHit(8, false, 9), "", false), sink)
	if got := itemByID(t, sink.items, id); !got.OnRadar() {
		t.Fatal("the newer 12:09 ready copy did not put the PR on the radar")
	}
}

// An older ready copy never rolls back the stored state: it is not written, but it
// still proves the item open. So a later draft result older than the newest ready
// state stored cannot retire the PR.
func TestRunIngestOlderReadyCopyKeepsNewerState(t *testing.T) {
	const id = "github:o/r#8"
	sink := &memSink{}
	runLifecycleIngest(t, twoSearchServer(t, lifecycleHit(8, false, 0), lifecycleHit(8, false, 9), false), sink)
	if got := itemByID(t, sink.items, id); got.GitHub.UpdatedAt.Minute() != 9 || !got.OnRadar() {
		t.Fatalf("the newest ready copy was not stored (updated %v, on radar %v)", got.GitHub.UpdatedAt, got.OnRadar())
	}
	runLifecycleIngest(t, twoSearchServer(t, lifecycleHit(8, false, 0), "", true), sink)
	if got := itemByID(t, sink.items, id); got.GitHub.UpdatedAt.Minute() != 9 {
		t.Fatalf("the older 12:00 copy rolled the stored state back to %v", got.GitHub.UpdatedAt)
	}
	runLifecycleIngest(t, twoSearchServer(t, lifecycleHit(8, true, 4), "", true), sink)
	if got := itemByID(t, sink.items, id); !got.OnRadar() {
		t.Fatal("a 12:04 draft result retired a PR stored ready at 12:09")
	}
	// A complete refresh serving only the older copy does not retire it as missing.
	runLifecycleIngest(t, twoSearchServer(t, lifecycleHit(8, false, 0), "", false), sink)
	if got := itemByID(t, sink.items, id); !got.OnRadar() || got.GitHub.UpdatedAt.Minute() != 9 {
		t.Fatalf("a complete refresh with an older copy: on radar %v, updated %v", got.OnRadar(), got.GitHub.UpdatedAt)
	}
}
