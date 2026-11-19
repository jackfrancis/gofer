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
	h := api.NewIngestHandler(p.store, nil)
	mux := http.NewServeMux()
	mux.Handle("POST /agent/worklist", auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Ingest)))
	mux.Handle("GET /agent/worklist", auth.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(h.List)))
	mux.Handle("GET /agent/worklist/item", auth.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(h.Item)))
	mux.Handle("POST /agent/worklist/reply", auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Reply)))
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
		if strings.HasPrefix(key, "/agent/worklist?") && !strings.Contains(key, "threads=0") {
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
	if it, ok, err := plane.client("u1").Item(ctx, "github:o/secret#1"); err != nil || ok {
		t.Fatalf("u1 read u2's item: (%+v, %v, %v)", it, ok, err)
	}
	if it, ok, err := plane.client("u2").Item(ctx, "github:o/secret#1"); err != nil || !ok || it.Thread[0].Content != "private" {
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
