package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
)

func timelineServer(body string) *httptest.Server {
	return httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
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

// timelinePages serves the timeline in pages of timelinePerPage, oldest first, and
// counts the timeline requests.
func timelinePages(t *testing.T, events []string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/timeline") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		*calls++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		lo := min((page-1)*timelinePerPage, len(events))
		hi := min(lo+timelinePerPage, len(events))
		_, _ = w.Write([]byte("[" + strings.Join(events[lo:hi], ",") + "]"))
	}))
}

// filler is n comment events that leave the review state alone.
func filler(n int, at time.Time) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`{"event":"commented","created_at":%q,"actor":{"login":"carol"}}`, at.Add(time.Duration(i)*time.Second).Format(time.RFC3339))
	}
	return out
}

// The pending request's requester is the latest one, even when the earlier bot
// request is on the first page and the human re-request on the second.
func TestItemActivityLatestRequesterAcrossPages(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events := []string{`{"event":"review_requested","created_at":"2026-01-01T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"k8s-ci-robot"}}`}
	events = append(events, filler(timelinePerPage, base.Add(time.Hour))...)
	events = append(events, `{"event":"review_requested","created_at":"2026-01-03T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"alice"}}`)
	var calls int
	srv := timelinePages(t, events, &calls)
	defer srv.Close()

	a, err := NewClient(srv.Client(), srv.URL).ItemActivity(context.Background(), "tok", "o/r", 1, "me")
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestedByLogin != "alice" {
		t.Fatalf("RequestedByLogin = %q, want the latest requester alice", a.RequestedByLogin)
	}
	if want := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC); !a.AwaitingMeSince.Equal(want) {
		t.Fatalf("AwaitingMeSince = %v, want %v", a.AwaitingMeSince, want)
	}
	if calls != 2 {
		t.Fatalf("timeline calls = %d, want 2", calls)
	}
}

// A request withdrawn by review_request_removed is no longer pending: no requester,
// and the ball is not in login's court. A later request makes it pending again.
func TestItemActivityRequestRemoved(t *testing.T) {
	removed := `[
		{"event":"review_requested","created_at":"2026-01-01T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"k8s-ci-robot"}},
		{"event":"review_request_removed","created_at":"2026-01-02T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}
	]`
	srv := timelineServer(removed)
	defer srv.Close()
	a, err := NewClient(srv.Client(), srv.URL).ItemActivity(context.Background(), "tok", "o/r", 1, "me")
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestedByLogin != "" || !a.AwaitingMeSince.IsZero() {
		t.Fatalf("withdrawn request still pending: requester %q since %v", a.RequestedByLogin, a.AwaitingMeSince)
	}

	// Another reviewer's removal does not withdraw login's request.
	srv2 := timelineServer(`[
		{"event":"review_requested","created_at":"2026-01-01T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"k8s-ci-robot"}},
		{"event":"review_request_removed","created_at":"2026-01-02T00:00:00Z","requested_reviewer":{"login":"dave"},"actor":{"login":"bob"}},
		{"event":"review_requested","created_at":"2026-01-01T12:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"alice"}}
	]`)
	defer srv2.Close()
	a, err = NewClient(srv2.Client(), srv2.URL).ItemActivity(context.Background(), "tok", "o/r", 1, "me")
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestedByLogin != "alice" {
		t.Fatalf("RequestedByLogin = %q, want alice", a.RequestedByLogin)
	}
}

// A timeline longer than the pages read leaves the latest events unseen, so the
// requester is unknown rather than an older request's actor.
func TestItemActivityTruncatedTimelineRequesterUnknown(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events := []string{`{"event":"review_requested","created_at":"2026-01-01T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"k8s-ci-robot"}}`}
	events = append(events, filler(timelinePerPage*timelineMaxPages, base.Add(time.Hour))...)
	var calls int
	srv := timelinePages(t, events, &calls)
	defer srv.Close()
	a, err := NewClient(srv.Client(), srv.URL).ItemActivity(context.Background(), "tok", "o/r", 1, "me")
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestedByLogin != "" {
		t.Fatalf("RequestedByLogin = %q from a truncated timeline, want unknown", a.RequestedByLogin)
	}
	if calls != timelineMaxPages {
		t.Fatalf("timeline calls = %d, want the cap %d", calls, timelineMaxPages)
	}
}
