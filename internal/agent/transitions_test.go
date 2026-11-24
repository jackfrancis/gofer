package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// githubSnapshot is one state of GitHub for a pull request. Each refresh runs the
// whole ingest pipeline (search, ingest, enrich, reading, rank) against it, and the
// store survives between refreshes, as it does for an owner in production.
type githubSnapshot struct {
	timeline               []string
	head                   string // empty: the pull request lookup fails
	reviewHit              bool   // the review-requested:@me search returns the PR
	reviewSearchIncomplete bool
}

const (
	snapOwner    = "github:16"
	snapHeadA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snapHeadB    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	snapQuestion = `{"event":"commented","id":19,"actor":{"login":"me"},"body":"@author please add a test","created_at":"2026-01-01T10:00:00Z"}`
	snapRequest  = `{"event":"review_requested","actor":{"login":"author"},"requested_reviewer":{"login":"me"},"created_at":"2026-01-04T10:00:00Z"}`
	snapRemoved  = `{"event":"review_request_removed","actor":{"login":"author"},"requested_reviewer":{"login":"me"},"created_at":"2026-01-05T10:00:00Z"}`
)

func (snap githubSnapshot) refresh(t *testing.T, store worklist.Store, updated string) worklist.WorkItem {
	t.Helper()
	hit := fmt.Sprintf(`{"number":7,"title":"Changes","state":"open","updated_at":%q,"repository_url":"https://api.github.com/repos/o/r","user":{"login":"author"},"pull_request":{"url":"x"}}`, updated)
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"me"}`)
		case r.URL.Path == "/search/issues" && strings.Contains(q, "review-requested:@me"):
			if snap.reviewHit {
				fmt.Fprintf(w, `{"total_count":1,"items":[%s]}`, hit)
			} else {
				fmt.Fprintf(w, `{"total_count":0,"incomplete_results":%t,"items":[]}`, snap.reviewSearchIncomplete)
			}
		case r.URL.Path == "/search/issues" && strings.Contains(q, "is:pr") && strings.Contains(q, "involves:@me"):
			fmt.Fprintf(w, `{"total_count":1,"items":[%s]}`, hit)
		case r.URL.Path == "/search/issues":
			fmt.Fprint(w, `{"total_count":0,"items":[]}`)
		case r.URL.Path == "/repos/o/r/issues/7/timeline":
			fmt.Fprintf(w, "[%s]", strings.Join(snap.timeline, ","))
		case r.URL.Path == "/repos/o/r/pulls/7" && snap.head != "":
			fmt.Fprintf(w, `{"head":{"sha":%q}}`, snap.head)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := Run(ctx, Params{JobType: JobIngest, GitHubBaseURL: srv.URL, Client: srv.Client()}, fakeVendor{token: "test"}, NewStoreSink(store, snapOwner)); err != nil {
		t.Fatal(err)
	}
	it, ok, err := store.Get(ctx, snapOwner, "github:o/r#7")
	if err != nil || !ok {
		t.Fatalf("read back item: found=%v, err=%v", ok, err)
	}
	return it
}

// answeredByPush refreshes once while my question waits, then once after the author
// pushes (a commit dated before the question), and returns that second snapshot.
func answeredByPush(t *testing.T, store worklist.Store) githubSnapshot {
	t.Helper()
	s := githubSnapshot{timeline: []string{snapQuestion}, head: snapHeadA}
	if _, waiting := s.refresh(t, store, "2026-01-01T10:00:00Z").Waiting(); !waiting {
		t.Fatal("setup: my question should wait on the author")
	}
	s.head = snapHeadB
	s.timeline = append(s.timeline, `{"event":"committed","sha":"`+snapHeadB+`","committer":{"date":"2026-01-02T10:00:00Z"}}`)
	if _, waiting := s.refresh(t, store, "2026-01-02T10:00:00Z").Waiting(); waiting {
		t.Fatal("setup: the push should answer my question")
	}
	return s
}

// A failed head lookup does not revive a question a push already answered.
func TestRefreshAnsweredStaysAnsweredWithoutAHead(t *testing.T) {
	store := worklist.NewMemoryStore()
	s := answeredByPush(t, store)
	s.head = ""
	if _, waiting := s.refresh(t, store, "2026-01-02T10:00:00Z").Waiting(); waiting {
		t.Fatal("a failed head lookup revived a wait a push had answered")
	}
}

// A review request that comes and goes does not revive an answered question either.
func TestRefreshAnsweredStaysAnsweredAcrossARequest(t *testing.T) {
	store := worklist.NewMemoryStore()
	s := answeredByPush(t, store)
	s.timeline, s.reviewHit = append(s.timeline, snapRequest), true
	if _, waiting := s.refresh(t, store, "2026-01-04T10:00:00Z").Waiting(); waiting {
		t.Fatal("setup: a direct request should be actionable")
	}
	s.timeline, s.reviewHit = append(s.timeline, snapRemoved), false
	if _, waiting := s.refresh(t, store, "2026-01-05T10:00:00Z").Waiting(); waiting {
		t.Fatal("withdrawing the request revived the answered question")
	}
}

// Editing the question after the push asks something new: it waits again.
func TestRefreshEditedQuestionWaitsAgain(t *testing.T) {
	store := worklist.NewMemoryStore()
	s := answeredByPush(t, store)
	s.timeline[0] = `{"event":"commented","id":19,"actor":{"login":"me"},"body":"@author thanks for the test; can you add a release note too?","created_at":"2026-01-01T10:00:00Z","updated_at":"2026-01-03T10:00:00Z"}`
	if _, waiting := s.refresh(t, store, "2026-01-03T10:00:00Z").Waiting(); !waiting {
		t.Fatal("an edited question after the push was treated as already answered")
	}
}

// A fresh search hit and two team requests since my review: I owe a review whichever
// team names me, so the PR stays actionable, with no requester named.
func TestRefreshConfirmedAmbiguousTeamRequest(t *testing.T) {
	store := worklist.NewMemoryStore()
	s := githubSnapshot{head: snapHeadA, reviewHit: true, timeline: []string{
		`{"event":"reviewed","id":11,"state":"changes_requested","user":{"login":"me"},"commit_id":"` + snapHeadA + `","submitted_at":"2026-01-01T10:00:00Z"}`,
		`{"event":"review_requested","actor":{"login":"lead1"},"requested_team":{"slug":"red"},"created_at":"2026-01-02T10:00:00Z"}`,
		`{"event":"review_requested","actor":{"login":"lead2"},"requested_team":{"slug":"blue"},"created_at":"2026-01-03T10:00:00Z"}`,
	}}
	it := s.refresh(t, store, "2026-01-03T10:00:00Z")
	if !it.Actionable() || it.Signals.ReviewRequestedBy != "" {
		t.Fatalf("actionable %v, requester %q", it.Actionable(), it.Signals.ReviewRequestedBy)
	}
}

// A request carried over from an earlier search (this one was incomplete) is no
// evidence that a later team request names me.
func TestRefreshCarriedRequestIsNoTeamEvidence(t *testing.T) {
	store := worklist.NewMemoryStore()
	s := githubSnapshot{head: snapHeadA, reviewHit: true, timeline: []string{
		`{"event":"review_requested","requested_reviewer":{"login":"me"},"actor":{"login":"author"},"created_at":"2026-01-01T10:00:00Z"}`,
	}}
	if got := s.refresh(t, store, "2026-01-01T10:00:00Z").Signals.ReviewRequestedBy; got != "author" {
		t.Fatalf("setup: requester %q", got)
	}
	s.reviewHit, s.reviewSearchIncomplete = false, true
	s.timeline = append(s.timeline,
		`{"event":"reviewed","id":11,"state":"changes_requested","user":{"login":"me"},"commit_id":"`+snapHeadA+`","submitted_at":"2026-01-02T10:00:00Z"}`,
		`{"event":"review_requested","actor":{"login":"another-lead"},"requested_team":{"slug":"not-my-team"},"created_at":"2026-01-03T10:00:00Z"}`)
	it := s.refresh(t, store, "2026-01-03T10:00:00Z")
	if !it.Signals.AwaitingMeSince.IsZero() || it.Signals.ReviewRequestedBy == "another-lead" {
		t.Fatalf("a carried request attributed a team's: requester %q, awaiting me since %v", it.Signals.ReviewRequestedBy, it.Signals.AwaitingMeSince)
	}
}
