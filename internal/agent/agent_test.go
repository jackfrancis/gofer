package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
type memSink struct {
	items      []worklist.WorkItem
	ingests    int // write-back calls, so batching is observable
	replies    int // Reply calls
	researches int // Research calls

	// beforeIngest, if set, runs once just before the first write-back is merged.
	beforeIngest func()
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
