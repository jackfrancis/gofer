package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// runEnrich fetches per-item timeline signals and updates the item's Signals.
func TestRunEnrichUpdatesSignals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.Contains(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`[{"event":"review_requested","created_at":"2026-01-02T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#1", Source: "github",
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1},
	}}}
	err := Run(context.Background(),
		Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	s := sink.items[0].Signals
	if s.AwaitingMeSince.IsZero() {
		t.Fatal("expected AwaitingMeSince set after enrich")
	}
	if s.ReviewRequestedBy != "bob" {
		t.Fatalf("expected ReviewRequestedBy=bob, got %q", s.ReviewRequestedBy)
	}
}
