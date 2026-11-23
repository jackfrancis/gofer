package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

var turnT0 = time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC)

type fakeTurnReader struct {
	mu      sync.Mutex
	read    []string
	outcome string
	fail    map[string]bool
}

func (f *fakeTurnReader) ReadTurn(_ context.Context, it worklist.WorkItem) (worklist.TurnReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = append(f.read, it.ID)
	if f.fail[it.ID] {
		return worklist.TurnReading{}, errors.New("model down")
	}
	// A reader that claims another last word is overruled by the stamp.
	return worklist.TurnReading{LastWord: "forged", Outcome: f.outcome}, nil
}

func waitingPR(n int, text string) worklist.WorkItem {
	it := prItem(n)
	it.GitHub.Author = "author"
	it.Signals.Reasons = []worklist.Reason{worklist.ReasonReviewRequested}
	it.Signals.AwaitingOthersSince = turnT0
	it.Signals.LastWord = worklist.NewLastWord("comment:1", turnT0, false, nil, text)
	return it
}

// The reading pass reads only last words with no current reading, stamps the reading
// with the last word it read, and writes back the reading alone: the stored proposal
// stays.
func TestRunReadTurns(t *testing.T) {
	store := worklist.NewMemoryStore()
	unread := waitingPR(1, "I'll take a look tomorrow")
	unread.Signals.Proposed = &worklist.AxisProposal{Relevance: 0.9, Rationale: "stored"}
	alreadyRead := waitingPR(2, "why?")
	alreadyRead.Signals.Turn = &worklist.TurnReading{LastWord: alreadyRead.Signals.LastWord.Key(), Outcome: worklist.TurnWaitingOn}
	decisive := waitingPR(3, "lgtm")
	decisive.Signals.LastWord.Decisive = true
	hidden := waitingPR(4, "hidden?")
	hidden.Meta.HiddenAt = turnT0
	failing := waitingPR(5, "fails")
	notWaiting := prItem(6)
	if err := store.Upsert(context.Background(), "u", unread, alreadyRead, decisive, hidden, failing, notWaiting); err != nil {
		t.Fatal(err)
	}
	reader := &fakeTurnReader{outcome: worklist.TurnMine, fail: map[string]bool{failing.ID: true}}
	if err := runReadTurns(context.Background(), Params{TurnReader: reader}, NewStoreSink(store, "u")); err != nil {
		t.Fatal(err)
	}
	if len(reader.read) != 2 {
		t.Fatalf("read %v, want only #1 and #5", reader.read)
	}
	got, _, _ := store.Get(context.Background(), "u", unread.ID)
	r := got.Signals.CurrentTurnReading()
	if r == nil || r.Outcome != worklist.TurnMine || r.ReadAt.IsZero() {
		t.Fatalf("reading = %+v, want a current my_turn", got.Signals.Turn)
	}
	if got.Signals.Proposed == nil || got.Signals.Proposed.Rationale != "stored" {
		t.Fatal("the reading's write-back dropped the stored proposal")
	}
	if _, waiting := got.Waiting(); waiting {
		t.Fatal("a my_turn reading left the item waiting")
	}
	if got, _, _ := store.Get(context.Background(), "u", failing.ID); got.Signals.Turn != nil {
		t.Fatal("a failed reading wrote something")
	}

	// No reader (no chat model): nothing to do, nothing written.
	if err := runReadTurns(context.Background(), Params{}, NewStoreSink(store, "u")); err != nil {
		t.Fatal(err)
	}
}

type idRanker struct {
	mu  sync.Mutex
	ids []string
}

func (c *idRanker) Propose(_ context.Context, it worklist.WorkItem) (worklist.AxisProposal, error) {
	c.mu.Lock()
	c.ids = append(c.ids, it.ID)
	c.mu.Unlock()
	return worklist.AxisProposal{Relevance: 0.5}, nil
}

// Rank spends nothing on an item waiting on someone else, and keeps its turn reading.
func TestRunRankSkipsWaiting(t *testing.T) {
	store := worklist.NewMemoryStore()
	waiting := waitingPR(1, "why?")
	mine := waitingPR(2, "I'll look tomorrow")
	mine.Signals.Turn = &worklist.TurnReading{LastWord: mine.Signals.LastWord.Key(), Outcome: worklist.TurnMine}
	if err := store.Upsert(context.Background(), "u", waiting, mine); err != nil {
		t.Fatal(err)
	}
	ranker := &idRanker{}
	if err := runRank(context.Background(), Params{Ranker: ranker}, NewStoreSink(store, "u")); err != nil {
		t.Fatal(err)
	}
	if len(ranker.ids) != 1 || ranker.ids[0] != mine.ID {
		t.Fatalf("ranked %v, want only the item that is the user's turn", ranker.ids)
	}
	got, _, _ := store.Get(context.Background(), "u", mine.ID)
	if got.Signals.Proposed == nil || got.Signals.CurrentTurnReading() == nil {
		t.Fatalf("rank's write-back: proposal %v, reading %v", got.Signals.Proposed, got.Signals.Turn)
	}
}

// A refresh keeps whose turn it is for an item GitHub has not updated, and drops it
// for one GitHub has.
func TestCarryTurnSignals(t *testing.T) {
	stored := waitingPR(1, "why?")
	stored.GitHub.UpdatedAt = turnT0
	fetched := prItem(1)
	fetched.GitHub.UpdatedAt = turnT0
	carryTurnSignals(&fetched, stored)
	if fetched.Signals.LastWord == nil || !fetched.Signals.AwaitingOthersSince.Equal(turnT0) {
		t.Fatal("an unchanged item lost its turn signals across a refresh")
	}
	updated := prItem(1)
	updated.GitHub.UpdatedAt = turnT0.Add(time.Hour)
	carryTurnSignals(&updated, stored)
	if updated.Signals.LastWord != nil || !updated.Signals.AwaitingOthersSince.IsZero() {
		t.Fatal("an updated item kept a stale turn")
	}
}

// The enrich job records the user's last word (ignoring a listed bot's comment after
// it) and chains into the reading pass.
func TestRunEnrichRecordsAndReadsTheLastWord(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.Contains(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`[
				{"event":"commented","id":7,"created_at":"2026-01-02T00:00:00Z","actor":{"login":"me"},"body":"@author can you split this up?"},
				{"event":"commented","id":8,"created_at":"2026-01-03T00:00:00Z","actor":{"login":"k8s-ci-robot"},"body":"PR needs rebase."}
			]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#1", Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Author: "author"},
	}}}
	reader := &fakeTurnReader{outcome: worklist.TurnWaitingOn}
	err := Run(context.Background(),
		Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client(),
			Bots: []string{"k8s-ci-robot"}, TurnReader: reader},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	it := sink.items[0]
	if lw := it.Signals.LastWord; lw == nil || lw.ID != "comment:7" || len(lw.Mentions) != 1 || lw.Mentions[0] != "author" {
		t.Fatalf("last word = %+v", it.Signals.LastWord)
	}
	if len(reader.read) != 1 || it.Signals.CurrentTurnReading() == nil {
		t.Fatalf("reading pass: read %v, reading %+v", reader.read, it.Signals.Turn)
	}
	if w, ok := it.Waiting(); !ok || w.On != "author" {
		t.Fatalf("Waiting = %+v, %v", w, ok)
	}
}

type lateReader struct{ meanwhile func() }

func (r lateReader) ReadTurn(context.Context, worklist.WorkItem) (worklist.TurnReading, error) {
	r.meanwhile()
	return worklist.TurnReading{Outcome: worklist.TurnWaitingOn}, nil
}

// A reading that lands after the author answered (a refresh replaced the last word)
// is dropped, and nothing it read is written back.
func TestRunReadTurnsLateAfterAnAnswer(t *testing.T) {
	ctx := context.Background()
	store := worklist.NewMemoryStore()
	old := waitingPR(1, "Can you update this?")
	store.Seed("u", old)
	sink := NewStoreSink(store, "u")
	reader := lateReader{func() {
		fresh := old
		fresh.Signals.LastWord, fresh.Signals.AwaitingOthersSince = nil, time.Time{}
		fresh.GitHub.Title = "author answered"
		if err := sink.Ingest(ctx, []worklist.WorkItem{fresh}); err != nil {
			t.Error(err)
		}
	}}
	if err := runReadTurns(ctx, Params{TurnReader: reader}, sink); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.Get(ctx, "u", old.ID)
	if _, waiting := got.Waiting(); waiting || got.GitHub.Title != "author answered" || got.Signals.Turn != nil {
		t.Fatalf("late reading restored the old item: title %q, turn %+v", got.GitHub.Title, got.Signals.Turn)
	}
}

// A reading that lands after a hide and unhide leaves the item awaiting its refresh.
func TestRunReadTurnsLateAcrossUnhide(t *testing.T) {
	ctx := context.Background()
	store := worklist.NewMemoryStore()
	old := waitingPR(1, "Can you update this?")
	store.Seed("u", old)
	reader := lateReader{func() {
		_ = store.Update(ctx, "u", func(items []worklist.WorkItem) ([]worklist.WorkItem, error) {
			worklist.HideItem(&items[0], time.Now())
			return items, nil
		})
		_ = store.Update(ctx, "u", func(items []worklist.WorkItem) ([]worklist.WorkItem, error) {
			worklist.UnhideItem(&items[0])
			return items, nil
		})
	}}
	if err := runReadTurns(ctx, Params{TurnReader: reader}, NewStoreSink(store, "u")); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.Get(ctx, "u", old.ID)
	if _, waiting := got.Waiting(); waiting || !got.Meta.AwaitingRefresh {
		t.Fatalf("after hide/unhide: waiting=%v awaiting_refresh=%v", waiting, got.Meta.AwaitingRefresh)
	}
}

// With room for one proposal, a high-scoring waiting PR leaves the slot to the next
// item that is the user's turn.
func TestRunRankGivesWaitingSlotsAway(t *testing.T) {
	ctx := context.Background()
	store := worklist.NewMemoryStore()
	waiting := waitingPR(1, "Can you update this?")
	waiting.Signals.Labels = []string{"security"}
	mine := prItem(2)
	mine.Signals.Reasons = []worklist.Reason{worklist.ReasonInvolved}
	if worklist.BaselineScore(waiting, time.Now()).Rank <= worklist.BaselineScore(mine, time.Now()).Rank {
		t.Fatal("fixture: the waiting PR must outrank the other")
	}
	store.Seed("u", waiting, mine)
	r := &idRanker{}
	if err := runRank(ctx, Params{Ranker: r, RankLimit: 1}, NewStoreSink(store, "u")); err != nil {
		t.Fatal(err)
	}
	if len(r.ids) != 1 || r.ids[0] != mine.ID {
		t.Fatalf("ranked %v, want %s", r.ids, mine.ID)
	}
}
