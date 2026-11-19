package llm

import (
	"context"
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
