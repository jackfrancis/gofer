package worklist

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var waitT0 = time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC)

// waitingItem is someone else's PR on which the user had the last word.
func waitingItem(text string, mentions ...string) WorkItem {
	return WorkItem{
		ID:     "github:o/r#1",
		Type:   TypePullRequest,
		GitHub: GitHubRef{Repo: "o/r", Number: 1, Author: "author"},
		Signals: Signals{
			Reasons:             []Reason{ReasonReviewRequested},
			AwaitingOthersSince: waitT0,
			LastWord:            NewLastWord("comment:1", waitT0, false, mentions, text),
		},
	}
}

func read(it WorkItem, outcome, on string) WorkItem {
	it.Signals.Turn = &TurnReading{LastWord: it.Signals.LastWord.Key(), Outcome: outcome, WaitingOn: on}
	return it
}

func TestMentions(t *testing.T) {
	text := "@alice can you look? cc @Bob, @alice again\n" +
		"> @quoted said this\n" +
		"mail me@example.com, see https://x/@path, `@code`, @org/team\n" +
		"```\n@fenced\n```\n" +
		"(@carol)"
	got := strings.Join(Mentions(text), ",")
	if got != "alice,Bob,carol" {
		t.Fatalf("Mentions = %q, want alice,Bob,carol", got)
	}
}

func TestWaitingFromTheTimeline(t *testing.T) {
	// Not waiting: no signal, or a review is awaiting the user.
	if _, ok := (WorkItem{}).Waiting(); ok {
		t.Fatal("an item with no signals is waiting")
	}
	it := waitingItem("thoughts?")
	it.Signals.AwaitingMeSince = waitT0.Add(time.Hour)
	if _, ok := it.Waiting(); ok {
		t.Fatal("an item awaiting the user is waiting on others")
	}

	// Who it waits on: the first mention, else the author.
	w, ok := waitingItem("@Third what do you think?", "Third").Waiting()
	if !ok || w.On != "Third" || !w.Since.Equal(waitT0) {
		t.Fatalf("Waiting = %+v, %v; want on Third", w, ok)
	}
	if w, _ := waitingItem("can you downscope?").Waiting(); w.On != "author" {
		t.Fatalf("On = %q, want the author", w.On)
	}
	// On the user's own item the author is not who it waits on.
	own := waitingItem("does this address it?")
	own.Signals.Reasons = []Reason{ReasonAuthor}
	if w, ok := own.Waiting(); !ok || w.On != "" {
		t.Fatalf("own item: Waiting = %+v, %v; want waiting on someone unknown", w, ok)
	}
	// A decisive review by someone else leaves no last word, and still waits.
	dec := WorkItem{GitHub: GitHubRef{Author: "author"}, Signals: Signals{AwaitingOthersSince: waitT0}}
	if w, ok := dec.Waiting(); !ok || w.On != "author" {
		t.Fatalf("decisive review: Waiting = %+v, %v", w, ok)
	}
}

func TestWaitingFollowsACurrentReading(t *testing.T) {
	base := waitingItem("@Third, I'll take a look tomorrow", "Third")
	if _, ok := read(base, TurnMine, "").Waiting(); ok {
		t.Fatal("a my_turn reading left the item waiting")
	}
	if w, ok := read(base, TurnNothingPending, "").Waiting(); !ok || !w.NothingPending {
		t.Fatalf("nothing_pending: %+v, %v", w, ok)
	}
	if w, _ := read(base, TurnWaitingOn, "author").Waiting(); w.On != "author" {
		t.Fatalf("waiting_on author: On = %q", w.On)
	}
	// A login the last word does not mention, and that is not the author, is ignored.
	if w, _ := read(base, TurnWaitingOn, "mallory").Waiting(); w.On != "Third" {
		t.Fatalf("waiting_on an unknown login: On = %q, want the timeline's Third", w.On)
	}
	// unclear, or an unknown outcome, leaves it to the timeline.
	if w, ok := read(base, TurnUnclear, "").Waiting(); !ok || w.On != "Third" {
		t.Fatalf("unclear: %+v, %v", w, ok)
	}
	// A reading of a replaced last word (a new comment, or this one edited) does not apply.
	stale := read(base, TurnMine, "")
	stale.Signals.LastWord = NewLastWord("comment:1", waitT0, false, []string{"Third"}, "@Third, edited: can you look?")
	if _, ok := stale.Waiting(); !ok {
		t.Fatal("a reading of an edited last word still applied")
	}
	if !stale.Signals.NeedsTurnReading() || read(base, TurnMine, "").Signals.NeedsTurnReading() {
		t.Fatal("NeedsTurnReading should ask for the edited last word only")
	}
}

func TestNeedsTurnReading(t *testing.T) {
	if !waitingItem("why?").Signals.NeedsTurnReading() {
		t.Fatal("an unread last word needs reading")
	}
	dec := waitingItem("lgtm")
	dec.Signals.LastWord.Decisive = true
	empty := waitingItem("   ")
	noLast := WorkItem{Signals: Signals{AwaitingOthersSince: waitT0}}
	for name, it := range map[string]WorkItem{"decisive": dec, "empty": empty, "no last word": noLast} {
		if it.Signals.NeedsTurnReading() {
			t.Errorf("%s: needs no reading", name)
		}
	}
}

func TestLastWordBounds(t *testing.T) {
	long := strings.Repeat("é", MaxLastWordBytes) + " can you look?"
	var ms []string
	for i := 0; i < 2*MaxMentions; i++ {
		ms = append(ms, "user"+strings.Repeat("x", i))
	}
	lw := NewLastWord("comment:1", waitT0, false, append([]string{"bad login!", "user"}, ms...), long)
	if len(lw.Text) > MaxLastWordBytes || !utf8.ValidString(lw.Text) || !strings.HasSuffix(lw.Text, "can you look?") {
		t.Fatalf("text not cut to its head and tail: %d bytes, suffix %q", len(lw.Text), lw.Text[len(lw.Text)-20:])
	}
	if len(lw.Mentions) != MaxMentions || lw.Mentions[0] != "user" {
		t.Fatalf("mentions = %v", lw.Mentions)
	}

	s := Signals{
		LastWord: &LastWord{ID: "x", Text: long, Mentions: []string{"ok", "not ok"}},
		Turn:     &TurnReading{Outcome: "take over the world", WaitingOn: "@not a login"},
	}
	BoundTurnSignals(&s)
	if len(s.LastWord.Text) > MaxLastWordBytes || len(s.LastWord.Mentions) != 1 {
		t.Fatalf("BoundTurnSignals left %d bytes, mentions %v", len(s.LastWord.Text), s.LastWord.Mentions)
	}
	if s.Turn.Outcome != TurnUnclear || s.Turn.WaitingOn != "" {
		t.Fatalf("BoundTurnSignals left reading %+v", *s.Turn)
	}
}

// A write-back that carries no reading keeps the stored one; one that carries a
// reading replaces it.
func TestMergeKeepsTheTurnReading(t *testing.T) {
	stored := read(waitingItem("why?"), TurnMine, "")
	incoming := waitingItem("why?")
	out := MergeRunItems([]WorkItem{stored}, []WorkItem{incoming})
	if out[0].Signals.Turn == nil || out[0].Signals.Turn.Outcome != TurnMine {
		t.Fatal("a write-back without a reading dropped the stored one")
	}
	incoming = read(waitingItem("why?"), TurnWaitingOn, "author")
	out = MergeRunItems([]WorkItem{stored}, []WorkItem{incoming})
	if out[0].Signals.Turn.Outcome != TurnWaitingOn {
		t.Fatal("a write-back's reading did not replace the stored one")
	}
}

type readyIngestor struct{}

func (readyIngestor) EnsureBackfill(context.Context, string) error { return nil }

// Resolve lists waiting items apart from the rest, ranked the same way, and the bulk
// review actions skip them.
func TestResolveSplitsWaiting(t *testing.T) {
	store := NewMemoryStore()
	mine := WorkItem{ID: "github:o/r#2", Type: TypePullRequest, GitHub: GitHubRef{Repo: "o/r", Number: 2},
		Signals: Signals{Reasons: []Reason{ReasonReviewRequested}}}
	waitHigh := waitingItem("why?")
	waitHigh.ID = "github:o/r#3"
	waitHigh.Signals.Labels = []string{"kind/bug"}
	waitLow := waitingItem("and this?")
	mineAfterAll := read(waitingItem("I'll look tomorrow"), TurnMine, "")
	mineAfterAll.ID = "github:o/r#4"
	hidden := waitingItem("hidden")
	hidden.ID, hidden.Meta.HiddenAt = "github:o/r#5", waitT0
	if err := store.Upsert(context.Background(), "u", mine, waitLow, waitHigh, mineAfterAll, hidden); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(context.Background(), store, readyIngestor{}, waitT0.Add(24*time.Hour), "u", DefaultSort, true)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(items []WorkItem) string {
		var s []string
		for _, it := range items {
			s = append(s, it.ID)
		}
		return strings.Join(s, " ")
	}
	if got := ids(res.Waiting); got != "github:o/r#3 github:o/r#1" {
		t.Fatalf("Waiting = %q, want #3 (higher rank) then #1", got)
	}
	if got := ids(res.Items); strings.Contains(got, "#1") || strings.Contains(got, "#3") || !strings.Contains(got, "#2") || !strings.Contains(got, "#4") {
		t.Fatalf("Items = %q", got)
	}
	if got := ids(res.OffRadar); got != "github:o/r#5" {
		t.Fatalf("OffRadar = %q", got)
	}
	if waitLow.Actionable() || waitLow.NeedsReview(waitT0, time.Hour) || !mine.NeedsReview(waitT0, time.Hour) {
		t.Fatal("the bulk review pool should skip waiting items only")
	}
}

// On the user's own PR, a wait with no recorded last word is the older reading of a
// request for changes (before LastWord existed): the user owes the changes.
func TestWaitingLegacyOwnPR(t *testing.T) {
	it := WorkItem{Type: TypePullRequest, Signals: Signals{Reasons: []Reason{ReasonAuthor}, AwaitingOthersSince: waitT0}}
	if _, waiting := it.Waiting(); waiting {
		t.Fatal("a legacy request for changes on the user's own PR moved to waiting")
	}
	it.Signals.LastWord = NewLastWord("comment:1", waitT0, false, []string{"rev"}, "@rev ok now?")
	if w, waiting := it.Waiting(); !waiting || w.On != "rev" {
		t.Fatalf("own PR with my last word: %+v, %v", w, waiting)
	}
}

// An undecisive last word whose text could not be read says nothing about whose turn
// it is.
func TestWaitingNeedsTheLastWordsText(t *testing.T) {
	it := waitingItem("")
	if _, waiting := it.Waiting(); waiting {
		t.Fatal("a last word with no text made the item wait")
	}
	it.Signals.LastWord.Decisive = true
	if _, waiting := it.Waiting(); !waiting {
		t.Fatal("a decisive review with no summary should still wait")
	}
}

func TestMentionsSkipBotAccounts(t *testing.T) {
	if got := Mentions("@dependabot[bot] rebase, @renovate[bot] too, cc @alice"); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("Mentions = %v, want [alice]", got)
	}
}

// ApplyTurns writes the readings alone, in one update, and each only for the current
// last word of an item that is not hidden.
func TestApplyTurns(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	it := waitingItem("I'll look tomorrow")
	it.GitHub.Title = "stored title"
	store.Seed("u", it)
	key := it.Signals.LastWord.Key()

	if err := ApplyTurns(ctx, store, "u", []ItemTurn{{ItemID: it.ID, Reading: TurnReading{LastWord: "comment:1#stale", Outcome: TurnMine}}}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Get(ctx, "u", it.ID); got.Signals.Turn != nil {
		t.Fatal("a reading of another last word was stored")
	}
	if err := ApplyTurns(ctx, store, "u", []ItemTurn{{ItemID: it.ID, Reading: TurnReading{LastWord: key, Outcome: "anything"}}, {ItemID: "gone", Reading: TurnReading{LastWord: key}}}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.Get(ctx, "u", it.ID)
	if got.Signals.Turn == nil || got.Signals.Turn.Outcome != TurnUnclear || got.GitHub.Title != "stored title" {
		t.Fatalf("reading %+v, title %q", got.Signals.Turn, got.GitHub.Title)
	}
	hidden := waitingItem("x")
	HideItem(&hidden, waitT0)
	hidden.Signals.LastWord = NewLastWord("comment:1", waitT0, false, nil, "x")
	if SetTurn(&hidden, TurnReading{LastWord: hidden.Signals.LastWord.Key()}) {
		t.Fatal("a reading was set on a hidden item")
	}
}

// The ranker's top N leaves out waiting items before the cutoff.
func TestSelectTopSkipsWaiting(t *testing.T) {
	waiting := waitingItem("why?")
	waiting.Signals.Labels = []string{"security"}
	mine := WorkItem{ID: "github:o/r#2", Type: TypePullRequest, Signals: Signals{Reasons: []Reason{ReasonInvolved}}}
	page, _ := Select([]WorkItem{waiting, mine}, Selection{Open: true, Top: 1}, waitT0)
	if len(page) != 1 || page[0].ID != mine.ID {
		t.Fatalf("top 1 = %v, want the item that is the user's turn", page)
	}
	page, _ = Select([]WorkItem{waiting, mine}, Selection{Open: true}, waitT0)
	if len(page) != 2 {
		t.Fatal("a paged read should still include waiting items (enrich reads them)")
	}
}

// A last word a push answered never waits, whatever the timestamps say.
func TestWaitingAnsweredLastWord(t *testing.T) {
	it := waitingItem("can you add a test?")
	it.Signals.LastWord.Answered = true
	if _, waiting := it.Waiting(); waiting {
		t.Fatal("an answered last word made the item wait")
	}
}

// The key covers the whole body, including the part a long one loses when cut.
func TestLastWordKeyCoversTheCutMiddle(t *testing.T) {
	long := func(middle string) string {
		return strings.Repeat("a", MaxLastWordBytes) + middle + strings.Repeat("z", MaxLastWordBytes)
	}
	a := NewLastWord("comment:1", time.Time{}, false, nil, long("first"))
	b := NewLastWord("comment:1", time.Time{}, false, nil, long("second"))
	if a.Text != b.Text {
		t.Fatal("setup: the cut texts should be equal")
	}
	if a.Key() == b.Key() {
		t.Error("two versions differing only in the cut middle share a key")
	}
	legacy := LastWord{ID: "comment:1", Text: "short"}
	if legacy.Key() != NewLastWord("comment:1", time.Time{}, false, nil, "short").Key() {
		t.Error("a last word stored without a digest keys differently from a new one of the same text")
	}
	bad := LastWord{ID: "comment:1", Text: "short", Digest: "not hex"}
	s := Signals{LastWord: &bad, TeamConfirmed: &TeamConfirmation{Requests: []ConfirmedRequest{{Team: "", At: time.Now()}}}}
	BoundTurnSignals(&s)
	if s.LastWord.Digest != "" || s.TeamConfirmed != nil {
		t.Errorf("bounds kept digest %q, confirmation %+v", s.LastWord.Digest, s.TeamConfirmed)
	}
}

// A confirmation is kept in a stable order, stands only while each of its requests
// is outstanding as the same request, and is dropped whole if any request is malformed.
func TestTeamConfirmation(t *testing.T) {
	t1, t2 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	c := NewTeamConfirmation([]ConfirmedRequest{{Team: "red", At: t2}, {Team: "blue", At: t1}})
	if c == nil || c.Requests[0].Team != "blue" {
		t.Fatalf("confirmation %+v", c)
	}
	out := map[string]time.Time{"blue": t1, "red": t2, "green": t2}
	look := func(team string) (time.Time, bool) { at, ok := out[team]; return at, ok }
	if !c.StillOutstanding(look) {
		t.Error("all confirmed requests outstanding (and another team asked): want standing")
	}
	out["red"] = t2.Add(time.Hour) // red asked again: a different request
	if c.StillOutstanding(look) {
		t.Error("a re-requested team still counted as the confirmed request")
	}
	delete(out, "red")
	if c.StillOutstanding(look) {
		t.Error("a withdrawn request still counted")
	}
	if NewTeamConfirmation([]ConfirmedRequest{{Team: "red", At: t1}, {Team: "", At: t1}}) != nil {
		t.Error("a confirmation with a malformed request was kept")
	}
}
