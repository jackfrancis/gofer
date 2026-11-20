package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// An item with no thread yields a neutral (all-1.0) adjustment without a model call.
func TestResearchNeutralWithoutThread(t *testing.T) {
	r := NewResearchRanker(Config{Token: "t"})
	adj, err := r.Research(context.Background(), worklist.WorkItem{})
	if err != nil {
		t.Fatal(err)
	}
	if adj.Relevance != 1 || adj.Impact != 1 || adj.Engagement != 1 || adj.Urgency != 1 {
		t.Fatalf("expected neutral adjustment, got %+v", adj)
	}
}

// An omitted multiplier defaults to 1.0; an explicit one is honored.
func TestResearchParsesMultipliers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"relevance\":1.5,\"urgency\":0.5,\"rationale\":\"x\"}"}}]}`))
	}))
	defer srv.Close()

	r := NewResearchRanker(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	adj, err := r.Research(context.Background(), worklist.WorkItem{
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if adj.Relevance != 1.5 || adj.Urgency != 0.5 {
		t.Fatalf("explicit multipliers wrong: rel=%v urg=%v", adj.Relevance, adj.Urgency)
	}
	if adj.Impact != 1 || adj.Engagement != 1 {
		t.Fatalf("omitted multipliers should default to 1.0, got imp=%v eng=%v", adj.Impact, adj.Engagement)
	}
}

// The adjustment records the keys of the turns it read, positional for a turn
// stored before turns had IDs, so a reader can check they still belong.
func TestResearchRecordsTheTurnsItRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"relevance\":1.2,\"rationale\":\"x\"}"}}]}`))
	}))
	defer srv.Close()
	r := NewResearchRanker(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	adj, err := r.Research(context.Background(), worklist.WorkItem{Thread: []worklist.Message{
		{Role: worklist.RoleUser, Content: "legacy"},
		{ID: "a", Role: worklist.RoleAgent, Content: "answer"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(adj.Turns, ",") != "pos:0,a" {
		t.Fatalf("Turns = %v, want [pos:0 a]", adj.Turns)
	}
}

// A turn the owner hid never reaches the model, and the adjustment does not claim to
// have read it: hiding a turn removes it from future model context, as Discuss does.
func TestResearchOmitsHiddenTurns(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"relevance\":1.2,\"rationale\":\"x\"}"}}]}`))
	}))
	defer srv.Close()
	r := NewResearchRanker(Config{Endpoint: srv.URL, Model: "m", Token: "t", Client: srv.Client()})
	adj, err := r.Research(context.Background(), worklist.WorkItem{Thread: []worklist.Message{
		{ID: "u1", Role: worklist.RoleUser, Content: "visible question"},
		{ID: "u2", Role: worklist.RoleUser, Content: "HIDDEN-SECRET-TURN", Hidden: true},
		{ID: "a1", Role: worklist.RoleAgent, Content: "visible answer", ReplyTo: "u1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "HIDDEN-SECRET-TURN") {
		t.Fatalf("a hidden turn was sent to the model: %s", body)
	}
	if !strings.Contains(body, "visible question") || !strings.Contains(body, "visible answer") {
		t.Fatalf("visible turns missing from the research input: %s", body)
	}
	if strings.Join(adj.Turns, ",") != "u1,a1" {
		t.Fatalf("Turns = %v, want [u1 a1]", adj.Turns)
	}
}

// A thread whose every turn is hidden has nothing to research: a neutral adjustment,
// without calling the model.
func TestResearchAllHiddenSkipsTheModel(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"relevance\":2}"}}]}`))
	}))
	defer srv.Close()
	r := NewResearchRanker(Config{Endpoint: srv.URL, Model: "m", Token: "t", Client: srv.Client()})
	adj, err := r.Research(context.Background(), worklist.WorkItem{Thread: []worklist.Message{
		{ID: "u1", Role: worklist.RoleUser, Content: "gone", Hidden: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("the model was called for a thread with no visible turn")
	}
	if adj.Relevance != 1 || len(adj.Turns) != 0 {
		t.Fatalf("expected a neutral adjustment reading no turns, got %+v", adj)
	}
}
