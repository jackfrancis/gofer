package github

import (
	"context"
	"strings"
	"testing"
	"time"
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
