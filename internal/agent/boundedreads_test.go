package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/api"
	"github.com/jackfrancis/gofer/internal/authn"
	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// countingWriter counts the bytes a handler writes.
type countingWriter struct {
	http.ResponseWriter
	n int
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += n
	return n, err
}

// boundedPlane is the real agent plane (api.IngestHandler behind run-credential
// auth) over a memory store, recording the size of every worklist read it answers.
type boundedPlane struct {
	store *worklist.MemoryStore
	srv   *httptest.Server
	mint  func(subject string) string

	mu    sync.Mutex
	reads map[string]int // GET path?query → largest response, bytes
}

func newBoundedPlane(t *testing.T) *boundedPlane {
	t.Helper()
	pub, priv, err := identity.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	authority := identity.NewEd25519Authority(priv, "gofer-agent", time.Hour)
	auth := authn.New(
		session.NewManager([]byte("test-session-secret-0123456789ab"), false),
		authn.NewIdentityValidator(identity.NewEd25519Verifier(pub, "gofer-agent")),
	)
	p := &boundedPlane{store: worklist.NewMemoryStore(), reads: map[string]int{}}
	h := api.NewIngestHandler(p.store, nil, worklist.NewRunPolicy(nil))
	mux := http.NewServeMux()
	mux.Handle("POST /agent/worklist", auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Ingest)))
	mux.Handle("GET /agent/worklist", auth.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(h.List)))
	mux.Handle("GET /agent/worklist/select", auth.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(h.Select)))
	mux.Handle("GET /agent/worklist/item", auth.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(h.Item)))
	mux.Handle("POST /agent/worklist/reply", auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Reply)))
	mux.Handle("POST /agent/worklist/research", auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Research)))
	mux.Handle("GET /agent/credential", auth.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"gh-tok","credential_version":"v1"}`))
	})))
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/agent/worklist") {
			mux.ServeHTTP(w, r)
			return
		}
		cw := &countingWriter{ResponseWriter: w}
		mux.ServeHTTP(cw, r)
		key := r.URL.Path + "?" + r.URL.RawQuery
		p.mu.Lock()
		p.reads[key] = max(p.reads[key], cw.n)
		p.mu.Unlock()
	}))
	t.Cleanup(p.srv.Close)
	p.mint = func(subject string) string {
		tok, err := authority.Mint(identity.Claims{RunID: "run-" + subject, Subject: subject, Scopes: []string{"signals:read", "metadata:write"}})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	return p
}

func (p *boundedPlane) client(subject string) *PlaneClient {
	return NewPlaneClient(p.srv.URL, p.mint(subject), p.srv.Client())
}

func (p *boundedPlane) largestRead() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var key string
	n := -1
	for k, v := range p.reads {
		if v > n {
			key, n = k, v
		}
	}
	return key, n
}

// boundedGitHub serves the GitHub calls ingest and enrich make: a complete fetch of
// the open items 1..open, and a timeline for each that names bob as the requester.
func boundedGitHub(t *testing.T, open int) (*httptest.Server, func() map[int]bool) {
	t.Helper()
	var (
		mu       sync.Mutex
		enriched = map[int]bool{}
	)
	hits := make([]string, 0, open)
	for i := 1; i <= open; i++ {
		hits = append(hits, fmt.Sprintf(`{"number":%d,"title":"T%d","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}`, i, i))
	}
	search := `{"total_count":` + strconv.Itoa(open) + `,"items":[` + strings.Join(hits, ",") + `]}`
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.HasPrefix(r.URL.Path, "/search/"):
			if strings.Contains(r.URL.Query().Get("q"), "review-requested:@me") && r.URL.Query().Get("page") <= "1" {
				_, _ = w.Write([]byte(search))
				return
			}
			_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			parts := strings.Split(r.URL.Path, "/") // /repos/o/r/issues/N/timeline
			n, _ := strconv.Atoi(parts[len(parts)-2])
			mu.Lock()
			enriched[n] = true
			mu.Unlock()
			_, _ = w.Write([]byte(`[{"event":"review_requested","created_at":"2026-01-02T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[int]bool {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[int]bool, len(enriched))
		for k, v := range enriched {
			out[k] = v
		}
		return out
	}
}

// Every job completes when the owner's full worklist is far past the agent plane's
// 8 MiB response limit, because none of them reads it whole: Discuss and research
// read one item, ranking the top N, and ingest and enrichment page through the items
// without their threads. Enrichment still reaches every open item, the stored
// threads survive every write-back, and no single read approaches the limit.
func TestEveryJobCompletesPastTheResponseLimit(t *testing.T) {
	const (
		open    = 36
		retired = 4
		turn    = 300 << 10 // one large turn per item: 40 × 300 KiB ≈ 11.7 MiB of threads
	)
	plane := newBoundedPlane(t)
	ctx := context.Background()
	big := strings.Repeat("x", turn)
	now := time.Now().UTC()
	var seed []worklist.WorkItem
	for i := 1; i <= open+retired; i++ {
		it := worklist.WorkItem{
			ID: fmt.Sprintf("github:o/r#%d", i), OwnerID: "u1", Source: "github", Type: worklist.TypePullRequest,
			GitHub: worklist.GitHubRef{Repo: "o/r", Number: i, Title: "T" + strconv.Itoa(i)},
			Thread: []worklist.Message{
				{ID: fmt.Sprintf("m%d-a", i), Role: worklist.RoleUser, Content: big, At: now.Add(-2 * time.Hour)},
				{ID: fmt.Sprintf("m%d-b", i), Role: worklist.RoleAgent, Content: "ok", ReplyTo: fmt.Sprintf("m%d-a", i), At: now.Add(-time.Hour)},
			},
		}
		if i > open {
			it.Meta.CompletedAt = now.Add(-24 * time.Hour)
		}
		seed = append(seed, it)
	}
	// The Discuss item has a fresh, unanswered user turn.
	seed[0].Thread = append(seed[0].Thread, worklist.Message{ID: "ask", Role: worklist.RoleUser, Content: "what next?", At: now})
	if err := plane.store.Upsert(ctx, "u1", seed...); err != nil {
		t.Fatal(err)
	}

	// The premise: the whole worklist, read the way every job used to, is past the cap.
	whole := httptest.NewRequest(http.MethodGet, "/agent/worklist", nil)
	whole.Header.Set("Authorization", "Bearer "+plane.mint("u1"))
	rec := httptest.NewRecorder()
	plane.srv.Config.Handler.ServeHTTP(rec, whole)
	if rec.Code != http.StatusOK || rec.Body.Len() <= maxSinkResponse {
		t.Fatalf("whole-worklist read = %d bytes (status %d), want past the %d-byte limit", rec.Body.Len(), rec.Code, maxSinkResponse)
	}
	plane.reads = map[string]int{}

	gh, enriched := boundedGitHub(t, open)
	sink := plane.client("u1")
	base := Params{Provider: "github", GitHubBaseURL: gh.URL, Client: gh.Client(), ReadPageSize: 7, RankLimit: 5}
	run := func(p Params) {
		t.Helper()
		if err := Run(ctx, p, sink, sink); err != nil {
			t.Fatalf("%s: %v", p.JobType, err)
		}
	}

	p := base
	p.JobType = JobIngest // chains enrich and rank
	run(p)
	p.JobType = JobEnrich
	run(p)
	p.JobType = JobRank
	run(p)
	fc := &fakeConverser{reply: "here is what's next"}
	p = base
	p.JobType, p.ItemID, p.Turn, p.Converser = JobConverse, "github:o/r#1", "ask", fc
	run(p)
	p = base
	p.JobType, p.ItemID, p.Researcher = JobResearch, "github:o/r#2", &fakeResearcher{adj: worklist.ResearchAdjustment{Rationale: "researched"}}
	run(p)

	if key, n := plane.largestRead(); n <= 0 || n > 1<<20 {
		t.Fatalf("largest worklist read was %d bytes (%s); want every read small (≤ 1 MiB)", n, key)
	}
	for key := range plane.reads {
		if strings.HasPrefix(key, "/agent/worklist?") {
			t.Fatalf("a run read the whole worklist: %s", key)
		}
	}

	got, err := plane.store.List(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]worklist.WorkItem{}
	for _, it := range got {
		byID[it.ID] = it
	}
	// Enrichment reached every open item, and only the open ones.
	seen := enriched()
	for i := 1; i <= open+retired; i++ {
		if want := i <= open; seen[i] != want {
			t.Errorf("item %d enriched = %v, want %v", i, seen[i], want)
		}
		it := byID[fmt.Sprintf("github:o/r#%d", i)]
		if i <= open && it.Signals.ReviewRequestedBy != "bob" {
			t.Errorf("item %d lost its enrichment: %+v", i, it.Signals)
		}
		// No write-back, thread-less as they now are, erased a stored thread.
		if len(it.Thread) < 2 || it.Thread[0].Content != big {
			t.Errorf("item %d thread was not preserved (%d turns)", i, len(it.Thread))
		}
	}
	// Discuss answered its turn; research recorded its adjustment.
	th := byID["github:o/r#1"].Thread
	if last := th[len(th)-1]; last.Role != worklist.RoleAgent || last.Content != fc.reply || last.ReplyTo != "ask" {
		t.Fatalf("Discuss reply = %+v, want the answer to turn ask", last)
	}
	if r := byID["github:o/r#2"].Signals.Research; r == nil || r.Rationale != "researched" {
		t.Fatalf("research adjustment = %+v", r)
	}
	// Ranking read only the top N.
	var rankRead bool
	for key := range plane.reads {
		if strings.Contains(key, "top=5") {
			rankRead = true
		}
	}
	if !rankRead {
		t.Fatalf("ranking did not read the top N: %v", plane.reads)
	}
}

// The single-item lookup is scoped to the run credential's owner: another owner's
// item is not found, and its content never crosses, while the owner's own item is.
func TestItemLookupIsOwnerScoped(t *testing.T) {
	plane := newBoundedPlane(t)
	ctx := context.Background()
	if err := plane.store.Upsert(ctx, "u2", worklist.WorkItem{ID: "github:o/secret#1", OwnerID: "u2", Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "private"}}}); err != nil {
		t.Fatal(err)
	}
	if err := plane.store.Upsert(ctx, "u1", worklist.WorkItem{ID: "github:o/r#1", OwnerID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if it, ok, err := plane.client("u1").Item(ctx, "github:o/secret#1", ""); err != nil || ok {
		t.Fatalf("u1 read u2's item: (%+v, %v, %v)", it, ok, err)
	}
	if it, ok, err := plane.client("u2").Item(ctx, "github:o/secret#1", ""); err != nil || !ok || it.Thread[0].Content != "private" {
		t.Fatalf("u2 Item = (%+v, %v, %v), want its own item", it, ok, err)
	}
	// Nor does a selection reach across owners.
	items, _, err := plane.client("u1").Select(ctx, worklist.Selection{})
	if err != nil || len(items) != 1 || items[0].ID != "github:o/r#1" {
		t.Fatalf("u1 Select = (%+v, %v), want only its own item", items, err)
	}
	// A Discuss run dispatched for another owner's item answers nothing and writes
	// nothing.
	fc := &fakeConverser{reply: "leaked"}
	if err := Run(ctx, Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/secret#1", Converser: fc}, plane.client("u1"), plane.client("u1")); err != nil {
		t.Fatal(err)
	}
	stored, _ := plane.store.List(ctx, "u2")
	if len(stored[0].Thread) != 1 || fc.gotUser != "" {
		t.Fatalf("a u1 run touched u2's item: thread %+v, converser saw %q", stored[0].Thread, fc.gotUser)
	}
	var raw struct {
		Found bool `json:"found"`
	}
	req := httptest.NewRequest(http.MethodGet, "/agent/worklist/item?id=github:o/secret%231", nil)
	req.Header.Set("Authorization", "Bearer "+plane.mint("u1"))
	rec := httptest.NewRecorder()
	plane.srv.Config.Handler.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || raw.Found || strings.Contains(rec.Body.String(), "private") {
		t.Fatalf("raw item response for another owner = %q", rec.Body.String())
	}
}

// countingRanker counts model proposals.
type countingRanker struct {
	mu    sync.Mutex
	calls int
}

func (r *countingRanker) Propose(context.Context, worklist.WorkItem) (worklist.AxisProposal, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return worklist.AxisProposal{}, nil
}

// A runtime that reaches a web tier from before the bounded reads (the AgentApp's
// image can roll out first) must fail its reads, not take the whole worklist as one
// final page: the old tier serves only GET /agent/worklist, which ignores a
// selection, so ranking would spend a model call on every item, retired ones
// included, past its ceiling.
func TestPreviousWebTierFailsTheBoundedReads(t *testing.T) {
	var items []worklist.WorkItem
	for i := 1; i <= 300; i++ {
		it := worklist.WorkItem{ID: fmt.Sprintf("github:o/r#%d", i), Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: i}}
		if i%3 == 0 {
			it.Meta.CompletedAt = time.Now().Add(-time.Hour)
		}
		items = append(items, it)
	}
	whole, _ := json.Marshal(map[string]any{"items": items})
	var writes int
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/agent/credential":
			_, _ = w.Write([]byte(`{"access_token":"gh-tok"}`))
		case r.URL.Path == "/agent/worklist" && r.Method == http.MethodGet:
			_, _ = w.Write(whole) // the previous tier: every query ignored
		case r.URL.Path == "/agent/worklist" && r.Method == http.MethodPost:
			writes++
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer old.Close()
	gh, enriched := boundedGitHub(t, 0)
	sink := NewPlaneClient(old.URL, "cred", old.Client())
	ctx := context.Background()

	ranker := &countingRanker{}
	if err := Run(ctx, Params{JobType: JobRank, Ranker: ranker}, sink, sink); err == nil {
		t.Fatal("rank against the previous web tier succeeded")
	}
	if ranker.calls != 0 {
		t.Fatalf("rank made %d model calls against the previous web tier, want none", ranker.calls)
	}
	if err := Run(ctx, Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: gh.URL, Client: gh.Client()}, sink, sink); err == nil {
		t.Fatal("enrich against the previous web tier succeeded")
	}
	if n := len(enriched()); n != 0 {
		t.Fatalf("enrich reached %d items against the previous web tier", n)
	}
	fc := &fakeConverser{reply: "x"}
	if err := Run(ctx, Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc}, sink, sink); err == nil || fc.gotUser != "" {
		t.Fatalf("converse against the previous web tier = %v (converser saw %q), want an error", err, fc.gotUser)
	}
	// Ingest: a complete fetch with no open items would retire every stored open
	// item. Reading none of them must fail the run before it writes, not pass for an
	// empty worklist and report success with nothing retired.
	if err := Run(ctx, Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: gh.URL, Client: gh.Client()}, sink, sink); err == nil {
		t.Fatal("ingest against the previous web tier succeeded")
	}
	if writes != 0 {
		t.Fatalf("the runs made %d write-backs against the previous web tier, want none", writes)
	}
}

// One long conversation cannot break Discuss or research: the item read carries a
// bounded window of the thread that keeps the dispatched turn and what answers it,
// for turns with IDs and for legacy turns keyed by position, while the stored thread
// stays whole.
func TestOneLongConversationStaysReadable(t *testing.T) {
	plane := newBoundedPlane(t)
	ctx := context.Background()
	big := strings.Repeat("q", 16<<10)
	reply := strings.Repeat("r", 2<<10)
	var modern, legacy []worklist.Message
	for i := range 600 { // ≈ 10.5 MiB per thread
		modern = append(modern,
			worklist.Message{ID: fmt.Sprintf("u%d", i), Role: worklist.RoleUser, Content: big},
			worklist.Message{ID: fmt.Sprintf("a%d", i), Role: worklist.RoleAgent, Content: reply, ReplyTo: fmt.Sprintf("u%d", i)})
		legacy = append(legacy,
			worklist.Message{Role: worklist.RoleUser, Content: big},
			worklist.Message{Role: worklist.RoleAgent, Content: reply})
	}
	modern = append(modern, worklist.Message{ID: "ask", Role: worklist.RoleUser, Content: "and now?"})
	legacy = append(legacy, worklist.Message{Role: worklist.RoleUser, Content: "legacy ask"})
	if err := plane.store.Upsert(ctx, "u1",
		worklist.WorkItem{ID: "github:o/r#1", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1}, Thread: modern},
		worklist.WorkItem{ID: "github:o/r#2", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2}, Thread: legacy},
	); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(modern); len(b) <= maxSinkResponse {
		t.Fatalf("fixture thread is %d bytes, want past %d", len(b), maxSinkResponse)
	}
	sink := plane.client("u1")
	run := func(p Params) {
		t.Helper()
		if err := Run(ctx, p, sink, sink); err != nil {
			t.Fatalf("%s %s: %v", p.JobType, p.ItemID, err)
		}
	}
	fc := &fakeConverser{reply: "modern answer"}
	run(Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Turn: "ask", Converser: fc})
	if fc.gotUser != "and now?" || len(fc.gotHistory) == 0 {
		t.Fatalf("converser saw %q with %d history turns", fc.gotUser, len(fc.gotHistory))
	}
	lc := &fakeConverser{reply: "legacy answer"}
	run(Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#2", Converser: lc})
	if lc.gotUser != "legacy ask" {
		t.Fatalf("legacy converser saw %q", lc.gotUser)
	}
	run(Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: &fakeResearcher{adj: worklist.ResearchAdjustment{Rationale: "long"}}})

	if key, n := plane.largestRead(); n > maxSinkResponse/2+(1<<20) {
		t.Fatalf("largest read %d bytes (%s)", n, key)
	}
	got, _ := plane.store.List(ctx, "u1")
	byID := map[string]worklist.WorkItem{}
	for _, it := range got {
		byID[it.ID] = it
	}
	m := byID["github:o/r#1"]
	if len(m.Thread) != len(modern)+1 || !m.Answered("ask") || m.Signals.Research == nil {
		t.Fatalf("modern item: %d turns (want %d), answered %v, research %v", len(m.Thread), len(modern)+1, m.Answered("ask"), m.Signals.Research)
	}
	l := byID["github:o/r#2"]
	askKey := l.TurnKey(len(legacy) - 1)
	if len(l.Thread) != len(legacy)+1 || !l.Answered(askKey) || l.Thread[len(l.Thread)-1].Content != "legacy answer" {
		t.Fatalf("legacy item: %d turns, answered %s = %v", len(l.Thread), askKey, l.Answered(askKey))
	}
}

// Items without threads can still be large (labels, model rationales). A page ends
// by encoded size, so enrichment at the default page size still reaches every item
// and ranking still reads its top N, with every read under the response limit.
func TestPagedReadsStayUnderTheLimitWithLargeItems(t *testing.T) {
	const n = 600 // × ~17 KiB ≈ 10 MiB without threads
	plane := newBoundedPlane(t)
	ctx := context.Background()
	label := strings.Repeat("l", 17<<10)
	var seed []worklist.WorkItem
	for i := 1; i <= n; i++ {
		seed = append(seed, worklist.WorkItem{
			ID: fmt.Sprintf("github:o/r#%d", i), Source: "github",
			GitHub:  worklist.GitHubRef{Repo: "o/r", Number: i},
			Signals: worklist.Signals{Labels: []string{label}},
		})
	}
	if err := plane.store.Upsert(ctx, "u1", seed...); err != nil {
		t.Fatal(err)
	}
	gh, enriched := boundedGitHub(t, 0)
	sink := plane.client("u1")
	if err := Run(ctx, Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: gh.URL, Client: gh.Client()}, sink, sink); err != nil {
		t.Fatal(err)
	}
	if got := len(enriched()); got != n {
		t.Fatalf("enriched %d items, want all %d", got, n)
	}
	ranker := &countingRanker{}
	if err := Run(ctx, Params{JobType: JobRank, Ranker: ranker}, sink, sink); err != nil {
		t.Fatal(err)
	}
	if ranker.calls != defaultRankLimit {
		t.Fatalf("rank made %d proposals, want its ceiling of %d", ranker.calls, defaultRankLimit)
	}
	if key, size := plane.largestRead(); size > worklist.MaxSelectBytes+(64<<10) {
		t.Fatalf("largest read %d bytes (%s), want ≤ about %d", size, key, worklist.MaxSelectBytes)
	}
}

// The in-process sink holds a selection to the same bounds as the agent plane.
func TestSinksApplyTheSameSelectionBounds(t *testing.T) {
	plane := newBoundedPlane(t)
	ctx := context.Background()
	var seed []worklist.WorkItem
	for i := range worklist.MaxSelectLimit + 500 {
		seed = append(seed, worklist.WorkItem{ID: fmt.Sprintf("i%05d", i)})
	}
	if err := plane.store.Upsert(ctx, "u1", seed...); err != nil {
		t.Fatal(err)
	}
	for name, sink := range map[string]Sink{"in-process": NewStoreSink(plane.store, "u1"), "plane": plane.client("u1")} {
		for _, sel := range []worklist.Selection{{}, {Limit: 5000}} {
			page, next, err := sink.Select(ctx, sel)
			if err != nil || len(page) != worklist.MaxSelectLimit || next != page[len(page)-1].ID {
				t.Fatalf("%s %+v: %d items, next %q, err %v; want %d and a cursor", name, sel, len(page), next, err, worklist.MaxSelectLimit)
			}
		}
	}
}

// Data stored before the bounds (a message past the form limit, an item whose
// labels alone pass a page) still reads: an oversized turn is cut with a note, an
// oversized item is projected, every job completes, and no write-back stores a cut
// value. A delayed Discuss turn still sees the history it refers to.
func TestOversizedStoredDataStaysReadable(t *testing.T) {
	plane := newBoundedPlane(t)
	ctx := context.Background()
	huge := strings.Repeat("m", 9<<20)
	label := strings.Repeat("l", 5<<20)
	later := strings.Repeat("y", 16<<10)
	delayed := []worklist.Message{
		{ID: "q1", Role: worklist.RoleUser, Content: "is this safe to merge?"},
		{ID: "r1", Role: worklist.RoleAgent, Content: "not yet: the migration is unguarded", ReplyTo: "q1"},
		{ID: "q2", Role: worklist.RoleUser, Content: "what did you mean about the migration?"},
	}
	for i := range 400 {
		delayed = append(delayed,
			worklist.Message{ID: fmt.Sprintf("u%d", i), Role: worklist.RoleUser, Content: later},
			worklist.Message{ID: fmt.Sprintf("a%d", i), Role: worklist.RoleAgent, Content: "ok", ReplyTo: fmt.Sprintf("u%d", i)})
	}
	if err := plane.store.Upsert(ctx, "u1",
		worklist.WorkItem{ID: "github:o/r#1", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1},
			Thread: []worklist.Message{{ID: "big", Role: worklist.RoleUser, Content: huge}}},
		worklist.WorkItem{ID: "github:o/r#2", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2},
			Signals: worklist.Signals{Labels: []string{label}}},
		worklist.WorkItem{ID: "github:o/r#3", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 3}, Thread: delayed},
	); err != nil {
		t.Fatal(err)
	}
	gh, enriched := boundedGitHub(t, 3)
	sink := plane.client("u1")
	run := func(p Params) {
		t.Helper()
		if err := Run(ctx, p, sink, sink); err != nil {
			t.Fatalf("%s %s: %v", p.JobType, p.ItemID, err)
		}
	}
	base := Params{Provider: "github", GitHubBaseURL: gh.URL, Client: gh.Client()}
	stored := func(id string) worklist.WorkItem {
		t.Helper()
		it, _, err := plane.store.Get(ctx, "u1", id)
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	// Enrich and rank write back the projected item: the cut labels must not land.
	for _, job := range []string{JobEnrich, JobRank} {
		p := base
		p.JobType = job
		run(p)
	}
	two := stored("github:o/r#2")
	if len(two.Signals.Labels) != 1 || two.Signals.Labels[0] != label || two.Projected != nil {
		t.Fatalf("a projected item's write-back stored the cut labels (%d labels)", len(two.Signals.Labels))
	}
	if two.Signals.ReviewRequestedBy != "bob" || two.Signals.Proposed == nil {
		t.Fatalf("the projected item lost the runs' own results: %+v", two.Signals)
	}
	if n := len(enriched()); n != 3 {
		t.Fatalf("enriched %d items, want 3", n)
	}
	// Ingest reads every item, the oversized ones included (GitHub's fresh fields then
	// replace the stored ones, as on any refresh).
	p := base
	p.JobType = JobIngest
	run(p)
	big := &fakeConverser{reply: "answered the long one"}
	run(Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Turn: "big", Converser: big})
	if !strings.Contains(big.gotUser, "[gofer: this message was cut from") || !strings.HasPrefix(big.gotUser, "mmm") {
		t.Fatalf("the oversized turn reached the model as %d bytes without the cut note", len(big.gotUser))
	}
	dc := &fakeConverser{reply: "the migration runs without a lock"}
	run(Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#3", Turn: "q2", Converser: dc})
	if len(dc.gotHistory) != 2 || dc.gotHistory[1].Content != "not yet: the migration is unguarded" {
		t.Fatalf("the delayed turn's history = %+v, want q1 and r1", dc.gotHistory)
	}
	run(Params{JobType: JobResearch, ItemID: "github:o/r#2", Researcher: &fakeResearcher{adj: worklist.ResearchAdjustment{Rationale: "r"}}})

	if key, n := plane.largestRead(); n > worklist.MaxItemBytes+(64<<10) {
		t.Fatalf("largest read %d bytes (%s)", n, key)
	}
	got, _ := plane.store.List(ctx, "u1")
	byID := map[string]worklist.WorkItem{}
	for _, it := range got {
		byID[it.ID] = it
	}
	if it := byID["github:o/r#1"]; it.Thread[0].Content != huge || !it.Answered("big") {
		t.Fatal("the oversized message must stay whole in the store, and be answered")
	}
	if two := byID["github:o/r#2"]; two.Signals.Research == nil || two.Projected != nil {
		t.Fatalf("research on the item was not stored cleanly: %+v", two.Signals.Research)
	}
	if it := byID["github:o/r#3"]; !it.Answered("q2") {
		t.Fatal("the delayed turn was not answered")
	}
}

// A Discuss run dispatched without a turn answers the newest user turn, even when a
// large question (stored before the form limit) is followed by an out-of-order reply
// to an older one: the read keeps the question, so the run answers it instead of
// finding no turn and reporting success.
func TestTurnlessDiscussAnswersTheLatestQuestion(t *testing.T) {
	plane := newBoundedPlane(t)
	ctx := context.Background()
	question := strings.Repeat("w", worklist.MaxItemBytes)
	if err := plane.store.Upsert(ctx, "u1", worklist.WorkItem{ID: "github:o/r#1", Source: "github",
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1},
		Thread: []worklist.Message{
			{ID: "q1", Role: worklist.RoleUser, Content: "first question"},
			{ID: "q2", Role: worklist.RoleUser, Content: question},
			{ID: "r1", Role: worklist.RoleAgent, Content: "late answer to q1", ReplyTo: "q1"},
		}}); err != nil {
		t.Fatal(err)
	}
	fc := &fakeConverser{reply: "answer to q2"}
	sink := plane.client("u1")
	if err := Run(ctx, Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc}, sink, sink); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fc.gotUser, "www") {
		t.Fatalf("the converser was asked %.20q, want the newest question", fc.gotUser)
	}
	it, _, err := plane.store.Get(ctx, "u1", "github:o/r#1")
	if err != nil || !it.Answered("q2") {
		t.Fatalf("q2 was not answered (err %v)", err)
	}
	if key, n := plane.largestRead(); n > worklist.MaxItemBytes+(64<<10) {
		t.Fatalf("largest read %d bytes (%s)", n, key)
	}
}

// A projected item keeps the label that sets its impact, so a ranking pass and
// Discuss see the radar's ranking: the stub ranker's proposal (which echoes the
// baseline) and the item Discuss explains score as the stored item does, and the
// write-back keeps every stored label.
func TestProjectedItemKeepsItsRanking(t *testing.T) {
	plane := newBoundedPlane(t)
	ctx := context.Background()
	labels := make([]string, 0, 33)
	for i := range 32 {
		labels = append(labels, fmt.Sprintf("area/%d", i))
	}
	labels = append(labels, "security") // past the projection's cut
	if err := plane.store.Upsert(ctx, "u1", worklist.WorkItem{ID: "github:o/r#1", Source: "github",
		GitHub:  worklist.GitHubRef{Repo: "o/r", Number: 1},
		Signals: worklist.Signals{Labels: labels},
		Meta:    worklist.Metadata{Rationale: strings.Repeat("r", worklist.MaxBriefBytes)}, // forces projection
		Thread:  []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "why is this ranked here?"}},
	}); err != nil {
		t.Fatal(err)
	}
	stored := func() worklist.WorkItem {
		t.Helper()
		it, _, err := plane.store.Get(ctx, "u1", "github:o/r#1")
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	now := time.Now().UTC()
	radar := worklist.Score(stored(), now)
	if radar.Impact != 1 {
		t.Fatalf("fixture: stored impact %.2f, want 1", radar.Impact)
	}
	sink := plane.client("u1")
	if err := Run(ctx, Params{JobType: JobRank}, sink, sink); err != nil { // the stub ranker
		t.Fatal(err)
	}
	ranked := stored()
	if p := ranked.Signals.Proposed; p == nil || p.Impact != 1 {
		t.Fatalf("the ranking pass proposed %+v, want impact 1", ranked.Signals.Proposed)
	}
	if got := worklist.Score(ranked, now); got.Rank != radar.Rank || len(ranked.Signals.Labels) != 33 {
		t.Fatalf("after ranking: rank %.3f with %d labels, want %.3f with 33", got.Rank, len(ranked.Signals.Labels), radar.Rank)
	}
	fc := &fakeConverser{reply: "it is a security issue"}
	if err := Run(ctx, Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Turn: "q", Converser: fc}, sink, sink); err != nil {
		t.Fatal(err)
	}
	if fc.gotItem.Projected == nil {
		t.Fatal("fixture: Discuss read the item whole, want it projected")
	}
	if got := worklist.Score(fc.gotItem, now); got.Rank != worklist.Score(stored(), now).Rank || got.Impact != 1 {
		t.Fatalf("Discuss explains rank %.3f (impact %.2f), the radar shows %.3f", got.Rank, got.Impact, worklist.Score(stored(), now).Rank)
	}
}
