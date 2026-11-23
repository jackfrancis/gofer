package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
)

func activityFor(t *testing.T, timeline string, opts ActivityOptions) Activity {
	t.Helper()
	srv := timelineServer(timeline)
	defer srv.Close()
	a, err := NewClient(srv.Client(), srv.URL).ItemActivityFor(context.Background(), "tok", "o/r", 1, "me", opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// My question to the author is the last human word: the item waits on others, and
// the last word records the comment, its text and whom it mentions.
func TestItemActivityLastWordIsMyQuestion(t *testing.T) {
	a := activityFor(t, `[
		{"event":"commented","id":11,"created_at":"2026-01-01T00:00:00Z","actor":{"login":"author"},"body":"here is the fix"},
		{"event":"commented","id":12,"created_at":"2026-01-02T00:00:00Z","actor":{"login":"me"},"body":"@author can you downscope this? cc @me @Third, mail me@example.com"}
	]`, ActivityOptions{Author: "author"})
	if a.AwaitingOthersSince.IsZero() || !a.AwaitingMeSince.IsZero() {
		t.Fatalf("want waiting on others, got me=%v others=%v", a.AwaitingMeSince, a.AwaitingOthersSince)
	}
	lw := a.LastWord
	if lw == nil {
		t.Fatal("no last word recorded")
	}
	if lw.ID != "comment:12" || lw.Decisive || !lw.At.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("last word = %+v", *lw)
	}
	if !strings.Contains(lw.Text, "downscope") {
		t.Fatalf("text not recorded: %q", lw.Text)
	}
	if got := strings.Join(lw.Mentions, ","); got != "author,Third" {
		t.Fatalf("mentions = %q, want author,Third (not me, not an email)", got)
	}
}

// A bot's comment after mine (prow's "needs rebase", codecov, a [bot] account) does not
// take the last word.
func TestItemActivityBotCommentsDoNotTakeTheLastWord(t *testing.T) {
	for name, bot := range map[string]string{
		"listed":    `{"login":"k8s-ci-robot","type":"User"}`,
		"bot type":  `{"login":"netlify","type":"Bot"}`,
		"bot login": `{"login":"codecov[bot]","type":"User"}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := activityFor(t, `[
				{"event":"reviewed","id":5,"state":"changes_requested","submitted_at":"2026-01-02T00:00:00Z","user":{"login":"me"},"body":"@author are you willing to downscope?"},
				{"event":"commented","id":6,"created_at":"2026-01-03T00:00:00Z","actor":`+bot+`,"body":"PR needs rebase."}
			]`, ActivityOptions{Author: "author", Bots: []string{"K8s-Ci-Robot"}})
			if a.AwaitingOthersSince.IsZero() {
				t.Fatal("a bot's comment took the last word")
			}
			if a.LastWord == nil || a.LastWord.ID != "review:5" || !a.LastWord.Decisive {
				t.Fatalf("last word = %+v, want my decisive review", a.LastWord)
			}
		})
	}
}

// On someone else's PR, the author pushing after my review answers it: the ball is
// back with me, whether by a new commit, a force-push or marking it ready.
func TestItemActivityAuthorPushAnswers(t *testing.T) {
	review := `{"event":"reviewed","id":5,"state":"commented","submitted_at":"2026-01-02T00:00:00Z","user":{"login":"me"},"body":"a few nits"}`
	for name, push := range map[string]string{
		"commit":     `{"event":"committed","sha":"abc","committer":{"date":"2026-01-03T00:00:00Z"}}`,
		"force-push": `{"event":"head_ref_force_pushed","created_at":"2026-01-03T00:00:00Z","actor":{"login":"author"}}`,
		"ready":      `{"event":"ready_for_review","created_at":"2026-01-03T00:00:00Z","actor":{"login":"author"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := activityFor(t, "["+review+","+push+"]", ActivityOptions{Author: "author"})
			if !a.AwaitingOthersSince.IsZero() || a.LastWord != nil {
				t.Fatalf("still waiting on others after the author's %s", name)
			}
		})
	}
	// A commit dated before my review (it was pushed before I looked) answers nothing.
	a := activityFor(t, `[{"event":"committed","sha":"abc","committer":{"date":"2026-01-01T00:00:00Z"}},`+review+`]`, ActivityOptions{Author: "author"})
	if a.AwaitingOthersSince.IsZero() {
		t.Fatal("an older commit took the last word")
	}
}

// On my own PR, my pushes are not an answer to me, and a reviewer's request for
// changes leaves the next step with me, not with "the author".
func TestItemActivityOwnPR(t *testing.T) {
	a := activityFor(t, `[
		{"event":"commented","id":1,"created_at":"2026-01-01T00:00:00Z","actor":{"login":"me"},"body":"@rev does this address it?"},
		{"event":"committed","sha":"abc","committer":{"date":"2026-01-02T00:00:00Z"}}
	]`, ActivityOptions{Author: "me"})
	if a.AwaitingOthersSince.IsZero() || a.LastWord == nil || a.LastWord.Mentions[0] != "rev" {
		t.Fatalf("my own push answered my question: %+v", a)
	}
	a = activityFor(t, `[
		{"event":"reviewed","id":2,"state":"changes_requested","submitted_at":"2026-01-02T00:00:00Z","user":{"login":"rev"},"body":"please rename"}
	]`, ActivityOptions{Author: "me"})
	if !a.AwaitingOthersSince.IsZero() {
		t.Fatal("a request for changes on my own PR was counted as waiting on others")
	}
	// Without the author known, a decisive review by someone else still reads as the
	// author's turn (the behaviour before ActivityOptions).
	a = activityFor(t, `[
		{"event":"reviewed","id":2,"state":"approved","submitted_at":"2026-01-02T00:00:00Z","user":{"login":"rev"}}
	]`, ActivityOptions{})
	if a.AwaitingOthersSince.IsZero() || a.LastWord != nil {
		t.Fatalf("decisive review by another: others=%v lastWord=%+v", a.AwaitingOthersSince, a.LastWord)
	}
}

// The author's reply after my comment, inline (a "reviewed" event) or not, ends the wait.
func TestItemActivityReplyEndsTheWait(t *testing.T) {
	a := activityFor(t, `[
		{"event":"commented","id":1,"created_at":"2026-01-01T00:00:00Z","actor":{"login":"me"},"body":"why?"},
		{"event":"reviewed","id":2,"state":"commented","submitted_at":"2026-01-02T00:00:00Z","user":{"login":"author"},"body":""}
	]`, ActivityOptions{Author: "author"})
	if !a.AwaitingOthersSince.IsZero() || a.LastWord != nil {
		t.Fatal("the author's reply did not end the wait")
	}
}

// routesServer serves the timeline and, by path, the pull request and a review's
// comments; any other path answers [] (an unreadable pull request).
func routesServer(timeline string, routes map[string]string) *httptest.Server {
	return httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/timeline") {
			_, _ = w.Write([]byte(timeline))
			return
		}
		if body, ok := routes[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
}

func activityRoutes(t *testing.T, timeline string, routes map[string]string, opts ActivityOptions) Activity {
	t.Helper()
	srv := routesServer(timeline, routes)
	defer srv.Close()
	a, err := NewClient(srv.Client(), srv.URL).ItemActivityFor(context.Background(), "tok", "o/r", 1, "me", opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// My personal review request, unanswered by me, outranks another reviewer's approval.
func TestItemActivityPendingPersonalRequestOutranksAReview(t *testing.T) {
	a := activityFor(t, `[
		{"event":"review_requested","requested_reviewer":{"login":"me"},"actor":{"login":"owner"},"created_at":"2026-01-01T00:00:00Z"},
		{"event":"reviewed","id":1,"state":"approved","user":{"login":"another-reviewer"},"submitted_at":"2026-01-02T00:00:00Z"}
	]`, ActivityOptions{Author: "owner"})
	if a.AwaitingMeSince.IsZero() || !a.AwaitingOthersSince.IsZero() {
		t.Fatalf("awaiting me=%v others=%v; want my pending request to win", a.AwaitingMeSince, a.AwaitingOthersSince)
	}
}

// A team request made after my review awaits me when the review-requested:@me search
// confirms it may include me; without that word team membership is unknown.
func TestItemActivityTeamRequestAfterMyReview(t *testing.T) {
	timeline := `[
		{"event":"reviewed","id":1,"state":"approved","user":{"login":"me"},"submitted_at":"2026-01-01T00:00:00Z"},
		{"event":"review_requested","requested_team":{"slug":"my-team"},"actor":{"login":"owner"},"created_at":"2026-01-02T00:00:00Z"}
	]`
	a := activityFor(t, timeline, ActivityOptions{Author: "owner", ReviewRequested: true})
	if a.AwaitingMeSince.IsZero() || !a.AwaitingOthersSince.IsZero() {
		t.Fatalf("confirmed team request: awaiting me=%v others=%v", a.AwaitingMeSince, a.AwaitingOthersSince)
	}
	a = activityFor(t, timeline, ActivityOptions{Author: "owner"})
	if a.AwaitingOthersSince.IsZero() {
		t.Fatal("an unconfirmed team request displaced my review's last word")
	}
}

// A commit made before my review but pushed after it changes the head: the review's
// last word stands only while the head is the commit it reviewed.
func TestItemActivityReviewStandsOnlyOnItsCommit(t *testing.T) {
	timeline := `[
		{"event":"reviewed","id":1,"commit_id":"first","state":"changes_requested","user":{"login":"me"},"submitted_at":"2026-01-03T00:00:00Z","body":"please split"},
		{"event":"committed","sha":"second","committer":{"date":"2026-01-02T00:00:00Z"}}
	]`
	if a := activityRoutes(t, timeline, map[string]string{"/repos/o/r/pulls/1": `{"head":{"sha":"second"}}`}, ActivityOptions{Author: "owner"}); !a.AwaitingOthersSince.IsZero() {
		t.Fatal("still waiting after the head moved past the reviewed commit")
	}
	if a := activityRoutes(t, timeline, map[string]string{"/repos/o/r/pulls/1": `{"head":{"sha":"first"}}`}, ActivityOptions{Author: "owner"}); a.AwaitingOthersSince.IsZero() || a.LastWord == nil {
		t.Fatal("the head is the reviewed commit, yet the review's last word was dropped")
	}
	if a := activityRoutes(t, timeline, nil, ActivityOptions{Author: "owner"}); !a.AwaitingOthersSince.IsZero() {
		t.Fatal("an unreadable head left the item waiting")
	}
	// On my own PR my pushes are no answer to me: no head read.
	own := `[{"event":"commented","id":2,"created_at":"2026-01-03T00:00:00Z","actor":{"login":"me"},"body":"@rev ok now?"}]`
	if a := activityRoutes(t, own, nil, ActivityOptions{Author: "me"}); a.AwaitingOthersSince.IsZero() {
		t.Fatal("my own PR: my last comment no longer waits")
	}
}

// A review with no summary is read through its inline comments.
func TestItemActivityInlineOnlyReview(t *testing.T) {
	timeline := `[{"event":"reviewed","id":5,"commit_id":"h","submitted_at":"2026-01-01T00:00:00Z","user":{"login":"me"},"body":"","state":"commented"}]`
	a := activityRoutes(t, timeline, map[string]string{
		"/repos/o/r/pulls/1":                    `{"head":{"sha":"h"}}`,
		"/repos/o/r/pulls/1/reviews/5/comments": `[{"id":6,"body":"I'll fix this tomorrow"},{"id":7,"body":"@owner also this?"}]`,
	}, ActivityOptions{Author: "owner"})
	if a.LastWord == nil || !strings.Contains(a.LastWord.Text, "fix this tomorrow") || len(a.LastWord.Mentions) != 1 {
		t.Fatalf("inline-only review: last word %+v", a.LastWord)
	}
	// Comments that cannot be read leave the last word without text.
	a = activityRoutes(t, timeline, map[string]string{"/repos/o/r/pulls/1": `{"head":{"sha":"h"}}`, "/repos/o/r/pulls/1/reviews/5/comments": `{`}, ActivityOptions{Author: "owner"})
	if a.LastWord == nil || a.LastWord.Text != "" {
		t.Fatalf("unreadable inline comments: last word %+v", a.LastWord)
	}
}

// @dependabot[bot] names a bot account, not the user "dependabot".
func TestItemActivityBotMention(t *testing.T) {
	a := activityFor(t, `[{"event":"commented","id":1,"body":"@dependabot[bot] rebase please","actor":{"login":"me"},"created_at":"2026-01-01T00:00:00Z"}]`, ActivityOptions{Author: "owner"})
	if a.LastWord == nil || len(a.LastWord.Mentions) > 0 {
		t.Fatalf("bot mention parsed as %v", a.LastWord)
	}
}
