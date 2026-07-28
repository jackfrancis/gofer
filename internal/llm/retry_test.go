package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// The ranker's client (wrapped by NewRanker via httpretry) retries a 429 rate
// limit instead of failing the run: the endpoint 429s once, then returns a valid
// score, and Propose succeeds. This is the fix for a "Review all PRs" burst that
// otherwise leaves rate-limited items stalled.
func TestRankerRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests) // first attempt: rate limited
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"relevance\":0.5,\"impact\":0.5,\"engagement\":0.5,\"urgency\":0.5,\"confidence\":1,\"rationale\":\"ok\"}"}}]}`))
	}))
	defer srv.Close()

	r := NewRanker(Config{Endpoint: srv.URL, Model: "m", Token: "t"})
	prop, err := r.Propose(context.Background(), worklist.WorkItem{ID: "x"})
	if err != nil {
		t.Fatalf("Propose should recover after a 429 retry, got: %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected a retry (>=2 calls), got %d", calls.Load())
	}
	if prop.Relevance != 0.5 || prop.Confidence != 1 {
		t.Fatalf("unexpected proposal after retry: %+v", prop)
	}
}

// Every model client shares the same generous safety-net timeout, and a
// caller-supplied client is honored as-is. A tight default here is the regression
// this guards: it aborts a slow reasoning turn (a tool-calling round re-reads the
// whole transcript) while the run still has minutes of deadline left, and reports
// it as a bare "context deadline exceeded" — see defaultModelTimeout.
func TestModelClientTimeout(t *testing.T) {
	if defaultModelTimeout < 2*time.Minute {
		t.Fatalf("defaultModelTimeout = %v; a tool-calling reasoning turn needs minutes", defaultModelTimeout)
	}
	cfg := Config{Endpoint: "https://example.invalid/chat/completions", Model: "m", Token: "t"}
	for name, c := range map[string]*http.Client{
		"ranker":         NewRanker(cfg).client,
		"converser":      NewConverser(cfg).client,
		"researchRanker": NewResearchRanker(cfg).client,
	} {
		if c.Timeout != defaultModelTimeout {
			t.Errorf("%s default client timeout = %v, want %v", name, c.Timeout, defaultModelTimeout)
		}
	}

	own := &http.Client{Timeout: 7 * time.Second}
	if got := modelClient(Config{Client: own}).Timeout; got != 7*time.Second {
		t.Fatalf("a caller-supplied client's timeout = %v, want 7s", got)
	}
}
