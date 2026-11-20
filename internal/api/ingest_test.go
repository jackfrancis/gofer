package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/agent"
	"github.com/jackfrancis/gofer/internal/authn"
	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// A gofer run credential — an audience-bound Ed25519 token a runtime's control plane
// mints via gofer's authority — authenticates gofer's own agent plane: a runtime
// writes results to /agent/worklist with that token. This exercises the agent-plane
// verifier (the token is minted in the test to stand in for a backend's minter).
func TestAgentIngestWithRunToken(t *testing.T) {
	pub, priv, err := identity.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	authority := identity.NewEd25519Authority(priv, "gofer-agent", time.Minute)
	auth := authn.New(
		session.NewManager([]byte("test-session-secret-0123456789ab"), false),
		authn.NewIdentityValidator(identity.NewEd25519Verifier(pub, "gofer-agent")),
	)

	store := worklist.NewMemoryStore()
	h := NewIngestHandler(store, nil, worklist.NewRunPolicy(nil))
	ingest := auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Ingest))

	body, _ := json.Marshal(map[string]any{"items": []worklist.WorkItem{
		{ID: "github:o/r#1", GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r"}},
	}})

	// A full run token (has metadata:write) writes, scoped to the acting user.
	tok, _ := authority.Mint(identity.Claims{RunID: "run1", Subject: "u1", Scopes: []string{"signals:read", "metadata:write"}})
	req := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	ingest.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	items, _ := store.List(context.Background(), "u1")
	if len(items) != 1 || items[0].GitHub.Number != 1 {
		t.Fatalf("expected the ingested item stored for u1, got %+v", items)
	}

	// A token without metadata:write is rejected.
	weak, _ := authority.Mint(identity.Claims{RunID: "run2", Subject: "u1", Scopes: []string{"signals:read"}})
	req2 := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+weak)
	rec2 := httptest.NewRecorder()
	ingest.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a token without metadata:write, got %d", rec2.Code)
	}

	// A browser session (no bearer) never reaches the agent plane.
	req3 := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
	rec3 := httptest.NewRecorder()
	ingest.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for no credential, got %d", rec3.Code)
	}
}

// planeFixture is an agent plane with a memory store and a minter for run tokens.
type planeFixture struct {
	store    *worklist.MemoryStore
	ingest   http.Handler
	reply    http.Handler
	research http.Handler
	mint     func(scopes ...string) string
}

func newPlane(t *testing.T) planeFixture {
	t.Helper()
	pub, priv, err := identity.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	authority := identity.NewEd25519Authority(priv, "gofer-agent", time.Minute)
	auth := authn.New(
		session.NewManager([]byte("test-session-secret-0123456789ab"), false),
		authn.NewIdentityValidator(identity.NewEd25519Verifier(pub, "gofer-agent")),
	)
	store := worklist.NewMemoryStore()
	h := NewIngestHandler(store, nil, worklist.NewRunPolicy(nil))
	return planeFixture{
		store:    store,
		ingest:   auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Ingest)),
		reply:    auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Reply)),
		research: auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Research)),
		mint: func(scopes ...string) string {
			tok, err := authority.Mint(identity.Claims{RunID: "run", Subject: "u1", Scopes: scopes})
			if err != nil {
				t.Fatal(err)
			}
			return tok
		},
	}
}

func (f planeFixture) post(t *testing.T, h http.Handler, path, token string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// A run that read an item before the owner posted a message, and writes back its
// stale copy, updates the signals and leaves the owner's newer message in place.
func TestAgentWriteBackKeepsStoredThread(t *testing.T) {
	f := newPlane(t)
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	f.store.Seed("u1", worklist.WorkItem{ID: "a", Thread: []worklist.Message{
		{Role: worklist.RoleUser, Content: "q", At: t0},
		{Role: worklist.RoleAgent, Content: "r", At: t0.Add(time.Minute)},
		{Role: worklist.RoleUser, Content: "posted while the run worked", At: t0.Add(2 * time.Minute)},
	}})
	stale := worklist.WorkItem{ID: "a", Signals: worklist.Signals{Comments: 9}, Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "q", At: t0}}}
	if code := f.post(t, f.ingest, "/agent/worklist", f.mint("signals:read", "metadata:write"), map[string]any{"items": []worklist.WorkItem{stale}}); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	got, _ := f.store.List(context.Background(), "u1")
	if got[0].Signals.Comments != 9 {
		t.Fatalf("the run's signals must land, got %+v", got[0].Signals)
	}
	if len(got[0].Thread) != 3 || got[0].Thread[2].Content != "posted while the run worked" {
		t.Fatalf("the stored thread must survive the write-back, got %+v", got[0].Thread)
	}
}

func TestAgentReply(t *testing.T) {
	f := newPlane(t)
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	f.store.Seed("u1", worklist.WorkItem{ID: "a", Thread: []worklist.Message{
		{ID: "q1", Role: worklist.RoleUser, Content: "q", At: t0},
		{ID: "q2", Role: worklist.RoleUser, Content: "q2", At: t0.Add(time.Minute)},
	}})
	full := f.mint("signals:read", "metadata:write")
	reply := map[string]any{
		"item_id":     "a",
		"in_reply_to": "q1",
		"message":     worklist.Message{ID: "r1", Role: worklist.RoleAgent, Content: "answer", Model: "m", At: t0.Add(2 * time.Minute)},
	}
	thread := func() []worklist.Message {
		got, _ := f.store.List(context.Background(), "u1")
		return got[0].Thread
	}

	if code := f.post(t, f.reply, "/agent/worklist/reply", full, reply); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if th := thread(); len(th) != 3 || th[2].Content != "answer" || th[2].ReplyTo != "q1" || th[1].Content != "q2" {
		t.Fatalf("reply must be appended after the newer turn, linked to q1, got %+v", th)
	}

	// A repeat (a resend after a lost response) is not added twice.
	if code := f.post(t, f.reply, "/agent/worklist/reply", full, reply); code != http.StatusOK {
		t.Fatalf("want 200 on repeat, got %d", code)
	}
	if n := len(thread()); n != 3 {
		t.Fatalf("a repeated reply must not duplicate, got %d turns", n)
	}

	// A reply to a turn that is gone (the thread was cleared) is accepted and dropped.
	gone := map[string]any{"item_id": "a", "in_reply_to": "cleared", "message": worklist.Message{ID: "r2", Content: "late", At: t0.Add(3 * time.Minute)}}
	if code := f.post(t, f.reply, "/agent/worklist/reply", full, gone); code != http.StatusOK {
		t.Fatalf("want 200 for a dropped reply, got %d", code)
	}
	if n := len(thread()); n != 3 {
		t.Fatalf("a reply to a missing turn must be dropped, got %d turns", n)
	}

	// Malformed requests are rejected.
	if code := f.post(t, f.reply, "/agent/worklist/reply", full, map[string]any{"item_id": "a"}); code != http.StatusBadRequest {
		t.Fatalf("want 400 for a request missing fields, got %d", code)
	}

	// A token without metadata:write cannot reply.
	if code := f.post(t, f.reply, "/agent/worklist/reply", f.mint("signals:read"), reply); code != http.StatusForbidden {
		t.Fatalf("want 403 without metadata:write, got %d", code)
	}
}

// The agent plane's reads are bounded and owner-scoped: a selection returns a page
// without threads and the next cursor, a malformed one is refused, and the
// single-item read finds only the acting user's item.
func TestAgentReadsAreBoundedAndOwnerScoped(t *testing.T) {
	f := newPlane(t)
	h := NewIngestHandler(f.store, nil, worklist.NewRunPolicy(nil))
	ctx := context.Background()
	_ = f.store.Upsert(ctx, "u1",
		worklist.WorkItem{ID: "a", Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "secret-a"}}},
		worklist.WorkItem{ID: "b"},
		worklist.WorkItem{ID: "c"},
	)
	_ = f.store.Upsert(ctx, "u2", worklist.WorkItem{ID: "z", Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "secret-z"}}})
	get := func(hf http.HandlerFunc, target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req = req.WithContext(principal.NewContext(req.Context(), &principal.Principal{ActingUserID: "u1"}))
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}

	rec := get(h.Select, "/agent/worklist/select?limit=2")
	var page struct {
		Items []worklist.WorkItem `json:"items"`
		Next  string              `json:"next"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("selection: %d %v", rec.Code, err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "a" || page.Next != "b" || bytes.Contains(rec.Body.Bytes(), []byte("secret")) {
		t.Fatalf("selection page = %s", rec.Body.String())
	}
	if rec := get(h.Select, "/agent/worklist/select?limit=x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a malformed selection got %d, want 400", rec.Code)
	}

	rec = get(h.Item, "/agent/worklist/item?id=a")
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"found":true`)) || !bytes.Contains(rec.Body.Bytes(), []byte("secret-a")) {
		t.Fatalf("own item = %d %s, want it with its thread", rec.Code, rec.Body.String())
	}
	rec = get(h.Item, "/agent/worklist/item?id=z")
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"found":false`)) || bytes.Contains(rec.Body.Bytes(), []byte("secret-z")) {
		t.Fatalf("another owner's item = %d %s, want not found", rec.Code, rec.Body.String())
	}
	if rec := get(h.Item, "/agent/worklist/item"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no id got %d, want 400", rec.Code)
	}
}

// A research write-back sets the adjustment and nothing else: an item refreshed after
// the run read it keeps its refreshed GitHub fields, signals and thread. The workload's
// plane client and this handler agree on the request.
func TestAgentResearch(t *testing.T) {
	f := newPlane(t)
	prop := &worklist.AxisProposal{Relevance: 0.8, Rationale: "model"}
	f.store.Seed("u1", worklist.WorkItem{
		ID:      "a",
		GitHub:  worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "Refreshed title", State: "closed"},
		Signals: worklist.Signals{Comments: 9, Proposed: prop},
		Thread:  []worklist.Message{{ID: "q1", Role: worklist.RoleUser, Content: "q"}},
	})
	full := f.mint("signals:read", "metadata:write")
	adj := worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 0.5, Rationale: "discussed", AppliedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC), Turns: []string{"q1"}}

	srv := httptest.NewServer(f.research)
	defer srv.Close()
	plane := agent.NewPlaneClient(srv.URL, full, srv.Client())
	if err := plane.Research(context.Background(), "a", adj); err != nil {
		t.Fatalf("Research: %v", err)
	}
	got, _ := f.store.List(context.Background(), "u1")
	it := got[0]
	if r := it.Signals.Research; r == nil || !r.AppliedAt.Equal(adj.AppliedAt) || r.Relevance != 1.5 || r.Rationale != "discussed" || len(r.Turns) != 1 || r.Turns[0] != "q1" {
		t.Fatalf("the adjustment must land, got %+v", it.Signals.Research)
	}
	if it.GitHub.Title != "Refreshed title" || it.GitHub.State != "closed" || it.Signals.Comments != 9 || it.Signals.Proposed == nil || len(it.Thread) != 1 {
		t.Fatalf("everything but the adjustment must stay as stored, got %+v", it)
	}

	// An adjustment for an item that is gone is accepted and dropped.
	if code := f.post(t, f.research, "/agent/worklist/research", full, map[string]any{"item_id": "gone", "adjustment": adj}); code != http.StatusOK {
		t.Fatalf("want 200 for a dropped adjustment, got %d", code)
	}
	// A resend of an older adjustment (A, then B, then A again) is accepted and dropped.
	later := adj
	later.AppliedAt = adj.AppliedAt.Add(time.Hour)
	later.Rationale = "later"
	for _, a := range []worklist.ResearchAdjustment{later, adj} {
		if code := f.post(t, f.research, "/agent/worklist/research", full, map[string]any{"item_id": "a", "adjustment": a}); code != http.StatusOK {
			t.Fatalf("want 200, got %d", code)
		}
	}
	if got, _ := f.store.List(context.Background(), "u1"); got[0].Signals.Research.Rationale != "later" {
		t.Fatalf("a resent older adjustment must not replace a newer one, got %+v", got[0].Signals.Research)
	}

	// An explicit zero multiplier is a valid adjustment.
	zero := map[string]any{"relevance": 0, "impact": 1, "engagement": 1, "urgency": 1, "applied_at": later.AppliedAt.Add(time.Hour)}
	if code := f.post(t, f.research, "/agent/worklist/research", full, map[string]any{"item_id": "a", "adjustment": zero}); code != http.StatusOK {
		t.Fatalf("want 200 for an explicit zero multiplier, got %d", code)
	}
	if got, _ := f.store.List(context.Background(), "u1"); got[0].Signals.Research.Relevance != 0 || got[0].Signals.Research.Impact != 1 {
		t.Fatalf("the explicit zero must land, got %+v", got[0].Signals.Research)
	}

	// Incomplete adjustments are rejected: a missing multiplier would decode as zero.
	for name, partial := range map[string]map[string]any{
		"empty":           {},
		"missing urgency": {"relevance": 1, "impact": 1, "engagement": 1, "applied_at": later.AppliedAt},
		"unstamped":       {"relevance": 1, "impact": 1, "engagement": 1, "urgency": 1},
	} {
		if code := f.post(t, f.research, "/agent/worklist/research", full, map[string]any{"item_id": "a", "adjustment": partial}); code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", name, code)
		}
	}

	// Malformed requests are rejected.
	if code := f.post(t, f.research, "/agent/worklist/research", full, map[string]any{"item_id": "a"}); code != http.StatusBadRequest {
		t.Fatalf("want 400 without an adjustment, got %d", code)
	}
	if code := f.post(t, f.research, "/agent/worklist/research", full, map[string]any{"adjustment": adj}); code != http.StatusBadRequest {
		t.Fatalf("want 400 without an item id, got %d", code)
	}
	// A token without metadata:write cannot write it.
	if code := f.post(t, f.research, "/agent/worklist/research", f.mint("signals:read"), map[string]any{"item_id": "a", "adjustment": adj}); code != http.StatusForbidden {
		t.Fatalf("want 403 without metadata:write, got %d", code)
	}
}

// The agent-plane write-back applies the web tier's policy: a review requested by a
// configured bot is stored as an automated assignment, a human's request is stored
// as a human ask whatever the run claims, and a run's metadata is stored as
// agent-derived even when the run claims the human override.
func TestAgentIngestAppliesRunPolicy(t *testing.T) {
	store := worklist.NewMemoryStore()
	h := NewIngestHandler(store, nil, worklist.NewRunPolicy([]string{"k8s-ci-robot"}))
	now := time.Now().UTC()

	body, _ := json.Marshal(map[string]any{"items": []worklist.WorkItem{
		{ID: "github:o/r#1", GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", UpdatedAt: now},
			Signals: worklist.Signals{ReviewRequestedBy: "k8s-ci-robot", AwaitingMeSince: now.Add(-48 * time.Hour)},
			Meta:    worklist.Metadata{Origin: worklist.OriginUser, Rank: 0.99}},
		{ID: "github:o/r#2", GitHub: worklist.GitHubRef{Number: 2, Repo: "o/r", UpdatedAt: now},
			Signals: worklist.Signals{ReviewRequestedBy: "alice", ReviewRequestedByBot: true, AwaitingMeSince: now.Add(-48 * time.Hour)}},
	}})
	req := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
	req = req.WithContext(principal.NewContext(req.Context(), &principal.Principal{
		Kind: principal.KindWorkload, Subject: "run1", ActingUserID: "u1", Scopes: []principal.Scope{principal.ScopeMetadataWrite},
	}))
	rec := httptest.NewRecorder()
	h.Ingest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}

	items, _ := store.List(context.Background(), "u1")
	byID := map[string]worklist.WorkItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	bot, human := byID["github:o/r#1"], byID["github:o/r#2"]
	if !bot.Signals.ReviewRequestedByBot {
		t.Error("a review requested by a configured bot was not stored as automated")
	}
	if human.Signals.ReviewRequestedByBot {
		t.Error("a human's review request was stored as automated")
	}
	for _, it := range items {
		if it.Meta.Origin != worklist.OriginAgent {
			t.Errorf("%s: stored Origin = %q, want %q", it.ID, it.Meta.Origin, worklist.OriginAgent)
		}
	}
	// The forged override does not survive the read model either.
	res, err := worklist.Resolve(context.Background(), store, nil, now, "u1", worklist.SortRank, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.ID == "github:o/r#1" && it.Meta.Rank == 0.99 {
			t.Error("Resolve served the run's claimed rank instead of rescoring")
		}
	}
}

// Every agent-plane read (the legacy whole list, a selection, a top-N selection and
// the single-item read) serves the bot flag by the current policy, not the stored
// value, so a run ranks by the classification its write-back will store (after an
// upgrade or a change to BOT_REVIEWERS).
func TestAgentListClassifiesByCurrentPolicy(t *testing.T) {
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "stale-human", OwnerID: "u1", Signals: worklist.Signals{ReviewRequestedBy: "k8s-ci-robot"}},
		worklist.WorkItem{ID: "stale-bot", OwnerID: "u1", Signals: worklist.Signals{ReviewRequestedBy: "alice", ReviewRequestedByBot: true}},
	)
	h := NewIngestHandler(store, nil, worklist.NewRunPolicy([]string{"k8s-ci-robot"}))
	get := func(hf http.HandlerFunc, target string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req = req.WithContext(principal.NewContext(req.Context(), &principal.Principal{
			Kind: principal.KindWorkload, Subject: "run1", ActingUserID: "u1", Scopes: []principal.Scope{principal.ScopeSignalsRead},
		}))
		rec := httptest.NewRecorder()
		hf(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", target, rec.Code)
		}
		return rec.Body.Bytes()
	}
	for _, target := range []string{"/agent/worklist", "/agent/worklist/select", "/agent/worklist/select?top=2"} {
		hf := h.Select
		if target == "/agent/worklist" {
			hf = h.List
		}
		var body struct {
			Items []worklist.WorkItem `json:"items"`
		}
		if err := json.Unmarshal(get(hf, target), &body); err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, it := range body.Items {
			got[it.ID] = it.Signals.ReviewRequestedByBot
		}
		if len(got) != 2 || !got["stale-human"] || got["stale-bot"] {
			t.Fatalf("%s served flags %v, want stale-human=true stale-bot=false", target, got)
		}
	}
	for id, want := range map[string]bool{"stale-human": true, "stale-bot": false} {
		var body struct {
			Item worklist.WorkItem `json:"item"`
		}
		if err := json.Unmarshal(get(h.Item, "/agent/worklist/item?id="+id), &body); err != nil {
			t.Fatal(err)
		}
		if body.Item.Signals.ReviewRequestedByBot != want {
			t.Fatalf("item %s served flag %v, want %v", id, body.Item.Signals.ReviewRequestedByBot, want)
		}
	}
}

// Research on a long conversation lists one key per turn; a valid adjustment must
// fit the route's budget (the reply it follows has already landed).
func TestAgentResearchLongThread(t *testing.T) {
	f := newPlane(t)
	thread := make([]worklist.Message, 2500)
	turns := make([]string, len(thread))
	for i := range thread {
		thread[i] = worklist.Message{ID: worklist.NewMessageID(), Role: worklist.RoleUser, Content: "q"}
		turns[i] = thread[i].ID
	}
	f.store.Seed("u1", worklist.WorkItem{ID: "a", Thread: thread})
	adj := worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1, AppliedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC), Turns: turns}
	if code := f.post(t, f.research, "/agent/worklist/research", f.mint("signals:read", "metadata:write"), map[string]any{"item_id": "a", "adjustment": adj}); code != http.StatusOK {
		t.Fatalf("want 200 for research on a 2,500-turn thread, got %d", code)
	}
	if got, _ := f.store.List(context.Background(), "u1"); got[0].Signals.Research == nil || len(got[0].Signals.Research.Turns) != len(turns) {
		t.Fatal("the long thread's research must land with all its turns")
	}
}
