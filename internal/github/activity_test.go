package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func timelineServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/timeline") {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
}

// A review requested of "me" with no engagement since ⇒ the ball is in my court.
func TestItemActivityAwaitingMe(t *testing.T) {
	srv := timelineServer(`[
		{"event":"commented","created_at":"2026-01-01T00:00:00Z","actor":{"login":"bob"}},
		{"event":"review_requested","created_at":"2026-01-02T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}
	]`)
	defer srv.Close()

	a, err := NewClient(srv.Client(), srv.URL).ItemActivity(context.Background(), "tok", "o/r", 1, "me")
	if err != nil {
		t.Fatal(err)
	}
	if a.AwaitingMeSince.IsZero() {
		t.Fatal("expected AwaitingMeSince to be set")
	}
	if !a.AwaitingOthersSince.IsZero() {
		t.Fatal("should not be awaiting others")
	}
	if a.RequestedByLogin != "bob" {
		t.Fatalf("expected RequestedByLogin=bob, got %q", a.RequestedByLogin)
	}
	if a.Participants != 1 {
		t.Fatalf("expected 1 participant, got %d", a.Participants)
	}
}

// "me" left the last decisive review ⇒ the ball is in the author's court.
func TestItemActivityAwaitingOthers(t *testing.T) {
	srv := timelineServer(`[
		{"event":"review_requested","created_at":"2026-01-01T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}},
		{"event":"reviewed","state":"changes_requested","submitted_at":"2026-01-03T00:00:00Z","user":{"login":"me"}},
		{"event":"cross-referenced","created_at":"2026-01-02T00:00:00Z"}
	]`)
	defer srv.Close()

	a, err := NewClient(srv.Client(), srv.URL).ItemActivity(context.Background(), "tok", "o/r", 1, "me")
	if err != nil {
		t.Fatal(err)
	}
	if !a.AwaitingMeSince.IsZero() {
		t.Fatal("should not be awaiting me (I reviewed last)")
	}
	if a.AwaitingOthersSince.IsZero() {
		t.Fatal("expected AwaitingOthersSince to be set")
	}
	if a.InboundRefs != 1 {
		t.Fatalf("expected 1 inbound ref, got %d", a.InboundRefs)
	}
}
