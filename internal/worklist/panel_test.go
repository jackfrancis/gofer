package worklist

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func panelThread() []Message {
	return []Message{
		{ID: "q1", Role: RoleUser, Content: "Can you review this PR?", Kind: KindReviewRequest, At: t0},
		{ID: "r1", Role: RoleAgent, ReplyTo: "q1", Content: "first review", At: t0},
		{ID: "p1", Role: RoleUser, Content: IndependentReviewPrompt, Kind: KindReviewRequest, At: t1},
		{ID: "r2", Role: RoleAgent, ReplyTo: "p1", Content: "blind review", At: t2},
	}
}

func TestChainSynthesis(t *testing.T) {
	cases := map[string]struct {
		edit     func(*WorkItem)
		answered string
		want     bool
	}{
		"panel review answered after an earlier review": {func(*WorkItem) {}, "p1", true},
		// The earlier review ran long and lands after the panel's blind one.
		"earlier review answered last": {func(w *WorkItem) {
			w.Thread = []Message{w.Thread[0], w.Thread[2], w.Thread[3], w.Thread[1]}
		}, "q1", true},
		"a turn that is not a review": {func(w *WorkItem) {
			w.Thread = append(w.Thread, Message{ID: "d1", Role: RoleUser, Content: "thoughts?", At: t2}, Message{ID: "d2", Role: RoleAgent, ReplyTo: "d1", Content: "some", At: t2})
		}, "d1", false},
		"panel text sent as a Discuss turn": {func(w *WorkItem) { w.Thread[2].Kind = "" }, "p1", false},
		"already covered by a synthesis": {func(w *WorkItem) {
			w.Thread = append(w.Thread, Message{ID: "s1", Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, Reviews: []string{"q1", "p1"}, At: t2})
		}, "q1", false},
		"covered by a synthesis stored before Reviews": {func(w *WorkItem) {
			w.Thread = append(w.Thread, Message{ID: "s1", Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, At: t2})
		}, "p1", false},
		"a second panel after a synthesis": {func(w *WorkItem) {
			w.Thread = append(w.Thread,
				Message{ID: "s1", Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, Reviews: []string{"q1", "p1"}, At: t2},
				Message{ID: "p2", Role: RoleUser, Content: IndependentReviewPrompt, Kind: KindReviewRequest, At: t2},
				Message{ID: "r3", Role: RoleAgent, ReplyTo: "p2", Content: "another blind review", Model: "c", At: t2})
		}, "p2", true},
		"every review by the same model": {func(w *WorkItem) {
			w.Thread[1].Model, w.Thread[3].Model = "b", "b"
		}, "p1", false},
		"reviews by different models": {func(w *WorkItem) {
			w.Thread[1].Model, w.Thread[3].Model = "a", "b"
		}, "p1", true},
		"panel review with no earlier review": {func(w *WorkItem) {
			w.Thread = w.Thread[2:]
		}, "p1", false},
		"earlier review hidden": {func(w *WorkItem) { w.Thread[1].Hidden = true }, "p1", false},
		"earlier review unanswered": {func(w *WorkItem) {
			w.Thread = append(w.Thread[:1], w.Thread[2:]...)
		}, "p1", false},
		"panel answered only by a notice": {func(w *WorkItem) {
			w.Thread[3].Notice = NoticeReconnect
		}, "p1", false},
		"panel review hidden": {func(w *WorkItem) { w.Thread[2].Hidden = true }, "p1", false},
		"item hidden":         {func(w *WorkItem) { w.Meta.HiddenAt = t2 }, "p1", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := WorkItem{ID: "a", Type: TypePullRequest, Thread: panelThread()}
			tc.edit(&w)
			n := len(w.Thread)
			key, ok := ChainSynthesis(&w, tc.answered, t2)
			if ok != tc.want {
				t.Fatalf("chained = %v, want %v", ok, tc.want)
			}
			if !ok {
				if len(w.Thread) != n || key != "" {
					t.Fatalf("nothing chained, so nothing appended; got %q and %+v", key, w.Thread)
				}
				return
			}
			last := w.Thread[len(w.Thread)-1]
			if len(w.Thread) != n+1 || last.ID != key || last.Kind != KindSynthesisRequest || last.Role != RoleUser || last.Content != SynthesisPrompt || !last.At.Equal(t2) {
				t.Fatalf("want one synthesis request appended under %q, got %+v", key, w.Thread)
			}
			var want []string
			for i, m := range w.Thread[:n] {
				if m.Role == RoleUser && m.Kind == KindReviewRequest && !m.Hidden && w.Answered(w.TurnKey(i)) {
					want = append(want, w.TurnKey(i))
				}
			}
			if !slices.Equal(last.Reviews, want) {
				t.Fatalf("the synthesis names every visible answered review: got %v, want %v", last.Reviews, want)
			}
			if !w.SynthesisReady(key) {
				t.Fatal("a freshly chained synthesis has its reviews")
			}
			if !w.ReplyPending(t2, 5*t1.Sub(t0)) {
				t.Fatal("the chained synthesis turn awaits its answer")
			}
		})
	}
}

// ApplyReplyAndChain stores the answer and its synthesis turn together, and only for
// a turn's first answer: a resent or late second answer chains nothing.
func TestApplyReplyAndChainOnce(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	th := panelThread()[:3]
	s.Seed("u1", WorkItem{ID: "a", Type: TypePullRequest, Thread: th})
	key, err := ApplyReplyAndChain(ctx, s, "u1", "a", "p1", Message{ID: "r2", Content: "blind review", At: t2}, t2)
	if err != nil || key == "" {
		t.Fatalf("want a synthesis turn, got %q, %v", key, err)
	}
	for _, again := range []Message{
		{ID: "r2", Content: "blind review", At: t2},          // a resend
		{ID: "r3", Content: "a late retry's answer", At: t2}, // a second answer
	} {
		if k, err := ApplyReplyAndChain(ctx, s, "u1", "a", "p1", again, t2); err != nil || k != "" {
			t.Fatalf("a second delivery must chain nothing, got %q, %v", k, err)
		}
	}
	got, _ := s.List(ctx, "u1")
	if n := len(got[0].Thread); n != 5 {
		t.Fatalf("want the answer and one synthesis turn, got %d turns: %+v", n, got[0].Thread)
	}
	// ApplyReply never chains.
	s.Seed("u1", WorkItem{ID: "b", Type: TypePullRequest, Thread: th})
	if err := ApplyReply(ctx, s, "u1", "b", "p1", Message{ID: "r2", Content: "blind review", At: t2}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.List(ctx, "u1")
	for _, it := range got {
		if it.ID == "b" && len(it.Thread) != 4 {
			t.Fatalf("ApplyReply must not chain, got %+v", it.Thread)
		}
	}
}

// The radar's verdict is the newest synthesis round's, read through its reply link,
// whatever order the answers landed in.
func TestReviewVerdictFollowsTheNewestRound(t *testing.T) {
	th := append(panelThread(),
		Message{ID: "s1", Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, Reviews: []string{"q1", "p1"}, At: t2},
		Message{ID: "p2", Role: RoleUser, Content: IndependentReviewPrompt, Kind: KindReviewRequest, At: t2},
		Message{ID: "r3", Role: RoleAgent, ReplyTo: "p2", Content: "third review", At: t2},
		Message{ID: "s2", Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, Reviews: []string{"q1", "p1", "p2"}, At: t2},
	)
	w := WorkItem{ID: "a", Type: TypePullRequest, Thread: th}
	newer := Message{ID: "v2", Role: RoleAgent, ReplyTo: "s2", Content: "they differ", Verdict: VerdictDisagree, At: t2}
	older := Message{ID: "v1", Role: RoleAgent, ReplyTo: "s1", Content: "they agree", Verdict: VerdictAgree, At: t2}

	w.Thread = append(w.Thread, older)
	if got := w.ReviewVerdict(); got != VerdictAgree {
		t.Fatalf("with only the first round answered, want its verdict, got %q", got)
	}
	w.Thread = append(th, newer, older) // the older round's answer lands last
	if got := w.ReviewVerdict(); got != VerdictDisagree {
		t.Fatalf("a late answer to an older round must not replace the newer verdict, got %q", got)
	}
}

// A synthesis is ready only while every review it names is visible and answered.
func TestSynthesisReady(t *testing.T) {
	base := append(panelThread(), Message{ID: "s1", Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, Reviews: []string{"q1", "p1"}, At: t2})
	cases := map[string]struct {
		edit func(*WorkItem)
		want bool
	}{
		"all there":                {func(*WorkItem) {}, true},
		"a review's answer hidden": {func(w *WorkItem) { w.Thread[1].Hidden = true }, false},
		"a review request hidden":  {func(w *WorkItem) { w.Thread[2].Hidden = true }, false},
		"a review gone":            {func(w *WorkItem) { w.Thread = w.Thread[2:] }, false},
		"legacy, naming none":      {func(w *WorkItem) { w.Thread[len(w.Thread)-1].Reviews = nil }, true},
		// A legacy request's comparison is rebuilt from the visible reviews before it:
		// with one hidden, a single review is left, which compares with nothing.
		"legacy, one review left": {func(w *WorkItem) { w.Thread[len(w.Thread)-1].Reviews = nil; w.Thread[1].Hidden = true }, false},
		"naming a single review":  {func(w *WorkItem) { w.Thread[len(w.Thread)-1].Reviews = []string{"p1"} }, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := WorkItem{ID: "a", Thread: slices.Clone(base)}
			tc.edit(&w)
			if got := w.SynthesisReady("s1"); got != tc.want {
				t.Fatalf("SynthesisReady = %v, want %v", got, tc.want)
			}
		})
	}
}

// A failed synthesis stays retryable after the owner writes again, and after a
// notice declined it; once retried (re-stamped) it is pending, not retryable.
func TestRetryableSynthesis(t *testing.T) {
	stale := 5 * time.Minute
	now := t2.Add(time.Hour)
	th := append(panelThread(),
		Message{ID: "s1", Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, Reviews: []string{"q1", "p1"}, At: t2},
		Message{ID: "d1", Role: RoleUser, Content: "and the tests?", At: t2},
		Message{ID: "d2", Role: RoleAgent, ReplyTo: "d1", Content: "they pass", At: t2},
	)
	w := WorkItem{ID: "a", Thread: th}
	if _, ok := w.RetryableTurn(now, stale); ok {
		t.Fatal("the last turn is answered: RetryableTurn offers nothing")
	}
	i, ok := w.RetryableSynthesis(now, stale)
	if !ok || w.TurnKey(i) != "s1" {
		t.Fatalf("the stranded synthesis must be retryable, got %d %v", i, ok)
	}
	if _, ok := w.RetryableSynthesis(t2.Add(time.Minute), stale); ok {
		t.Fatal("a synthesis still plausibly in flight is not retryable")
	}

	// Declined for a hidden review: not retryable until it is unhidden.
	w.Thread = append(slices.Clone(th), Message{ID: "n1", Role: RoleAgent, ReplyTo: "s1", Notice: NoticePanelInputs, Content: PanelInputsNotice, At: t2.Add(time.Second)})
	w.Thread[1].Hidden = true
	if _, ok := w.RetryableSynthesis(t2.Add(time.Minute), stale); ok {
		t.Fatal("a synthesis missing a review is not retryable")
	}
	w.Thread[1].Hidden = false
	if _, ok := w.RetryableSynthesis(t2.Add(time.Minute), stale); !ok {
		t.Fatal("a declined synthesis is retryable at once once its reviews are back")
	}
	if w.ReplyPending(t2.Add(time.Minute), stale) {
		t.Fatal("a declined synthesis is not pending")
	}
	// The retry starts a new attempt: the notice ended the earlier one, so the turn is
	// pending again and not offered twice.
	retry := t2.Add(2 * time.Minute)
	stamp := w.Restamp(4, retry)
	if !w.ReplyPending(retry, stale) {
		t.Fatal("a retried synthesis is pending")
	}
	if _, ok := w.RetryableSynthesis(retry, stale); ok {
		t.Fatal("a just-retried synthesis is not retryable again")
	}
	// The earlier attempt's run finishes late and posts its notice after the retry
	// (whatever its clock says): the retry is still in flight.
	w.Thread = append(w.Thread, Message{ID: "n0", Role: RoleAgent, ReplyTo: "s1", Notice: NoticePanelInputs, Content: PanelInputsNotice, At: retry.Add(time.Second)})
	if !w.ReplyPending(retry, stale) {
		t.Fatal("a late notice from an earlier attempt must not end the retry")
	}
	// The retry's own notice ends it.
	w.Thread = append(w.Thread, Message{ID: "n2", Role: RoleAgent, ReplyTo: "s1", Notice: NoticePanelInputs, Attempt: stamp.Attempt, Content: PanelInputsNotice, At: retry})
	if w.ReplyPending(retry, stale) {
		t.Fatal("the retry's own notice ends it")
	}

	// Answered, or superseded by a newer synthesis: nothing to retry.
	w.Thread = append(slices.Clone(th), Message{ID: "v1", Role: RoleAgent, ReplyTo: "s1", Content: "agree", Verdict: VerdictAgree, At: t2})
	if _, ok := w.RetryableSynthesis(now, stale); ok {
		t.Fatal("an answered synthesis is not retryable")
	}
}

// The read a synthesis run gets keeps the reviews it compares, however much
// discussion came since; one hidden is left out, and the run declines.
func TestThreadWindowKeepsSynthesisReviews(t *testing.T) {
	th := panelThread()
	for i := range 40 {
		id := strings.Repeat("x", 3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		th = append(th,
			Message{ID: "u" + id, Role: RoleUser, Content: strings.Repeat("chat ", 400), At: t2},
			Message{ID: "g" + id, Role: RoleAgent, ReplyTo: "u" + id, Content: strings.Repeat("reply ", 400), At: t2})
	}
	th = append(th, Message{ID: "s1", Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, Reviews: []string{"q1", "p1"}, At: t2})
	w := WorkItem{ID: "a", Type: TypePullRequest, Thread: th}

	budget := encodedSize(Project(w)) + 20<<10 // room for a few turns only
	win := ThreadWindow(w, "s1", budget)
	if !win.SynthesisReady("s1") {
		t.Fatalf("the window must keep both reviews and their answers, got %d turns", len(win.Thread))
	}
	if win.Thread[len(win.Thread)-1].ID != "s1" || len(win.Thread) >= len(th) {
		t.Fatalf("the window ends at the synthesis and is cut, got %d of %d turns", len(win.Thread), len(th))
	}
	for i := 1; i < len(win.Thread); i++ {
		a, _ := w.TurnIndex(win.Thread[i-1].ID)
		b, _ := w.TurnIndex(win.Thread[i].ID)
		if a >= b {
			t.Fatal("the window keeps thread order")
		}
	}

	w.Thread[3].Hidden = true // the blind review's answer
	if win := ThreadWindow(w, "s1", budget); win.SynthesisReady("s1") {
		t.Fatal("a hidden review is not sent, so the run must decline")
	}
}

// A draft-review request's read keeps the latest review, request and answer, however
// long the discussion since: the run decides from it whether the draft may approve,
// so a review pushed out of the window would read as no review at all. An answer too
// long for the room left is cut, keeping the revision it recorded.
func TestThreadWindowKeepsTheLatestReviewForADraft(t *testing.T) {
	th := []Message{
		{ID: "q1", Role: RoleUser, Content: "review", Kind: KindReviewRequest, At: t1},
		{ID: "a1", Role: RoleAgent, ReplyTo: "q1", Content: "old review", Head: "aaaa", Base: "main", Diff: strings.Repeat("1", 64), At: t1},
		{ID: "q2", Role: RoleUser, Content: "review", Kind: KindReviewRequest, At: t2},
		{ID: "a2", Role: RoleAgent, ReplyTo: "q2", Content: strings.Repeat("finding ", 4000), Head: "bbbb", Base: "main", Diff: strings.Repeat("2", 64), At: t2},
	}
	for i := range 40 {
		id := string(rune('a'+i%26)) + string(rune('a'+i/26))
		th = append(th,
			Message{ID: "u" + id, Role: RoleUser, Content: strings.Repeat("chat ", 400), At: t2},
			Message{ID: "g" + id, Role: RoleAgent, ReplyTo: "u" + id, Content: strings.Repeat("reply ", 400), At: t2})
	}
	th = append(th, Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, At: t2})
	w := WorkItem{ID: "a", Type: TypePullRequest, Thread: th}

	for name, room := range map[string]int{"fits": 40 << 10, "cut": 12 << 10} {
		win := ThreadWindow(w, "d", encodedSize(Project(w))+room)
		if len(win.Thread) >= len(th) || win.Thread[len(win.Thread)-1].ID != "d" {
			t.Fatalf("%s: the window ends at the draft request and is cut, got %d of %d turns", name, len(win.Thread), len(th))
		}
		j := win.LatestReviewAt()
		if j < 0 || win.Thread[j].ID != "a2" || win.ReviewedRevision() != w.ReviewedRevision() {
			t.Fatalf("%s: the window lost the latest review (at %d)", name, j)
		}
		if name == "cut" && !strings.Contains(win.Thread[j].Content, "was cut from") {
			t.Errorf("cut: the review answer should be cut with a note")
		}
		if encodedSize(win) > encodedSize(Project(w))+room {
			t.Errorf("%s: the window is over budget", name)
		}
		for i := 1; i < len(win.Thread); i++ {
			a, _ := w.TurnIndex(win.Thread[i-1].ID)
			b, _ := w.TurnIndex(win.Thread[i].ID)
			if a >= b {
				t.Fatalf("%s: the window keeps thread order", name)
			}
		}
	}
}

// The first reviewer is the first visible one: a hidden review, or a review whose
// answer is hidden, is out of the conversation and does not count.
func TestFirstReviewModelIsTheFirstVisible(t *testing.T) {
	th := panelThread()
	th[1].Model, th[3].Model = "a", "b"
	w := WorkItem{ID: "a", Thread: th}
	if got := w.FirstReviewModel(); got != "a" {
		t.Fatalf("FirstReviewModel = %q, want a", got)
	}
	w.Thread[1].Hidden = true
	if got := w.FirstReviewModel(); got != "b" {
		t.Fatalf("with a's answer hidden, FirstReviewModel = %q, want b", got)
	}
	w.Thread[1].Hidden, w.Thread[0].Hidden = false, true
	if got := w.FirstReviewModel(); got != "b" {
		t.Fatalf("with a's request hidden, FirstReviewModel = %q, want b", got)
	}
}

// A legacy synthesis (no recorded reviews) names its comparison from the whole thread
// before the read is trimmed: when one of six large reviews does not fit, the read
// still names all six, so it is not ready and the run declines rather than compare
// five.
func TestThreadWindowNamesLegacyReviewsBeforeTrimming(t *testing.T) {
	big := strings.Repeat("r", 750<<10)
	var th []Message
	for i := range 6 {
		q := "q" + string(rune('0'+i))
		th = append(th,
			Message{ID: q, Role: RoleUser, Kind: KindReviewRequest, Content: "Can you review this PR?", At: t0},
			Message{ID: "r" + q, ReplyTo: q, Role: RoleAgent, Content: big, At: t0})
	}
	th = append(th, Message{ID: "s1", Role: RoleUser, Kind: KindSynthesisRequest, Content: SynthesisPrompt, At: t1})
	w := WorkItem{ID: "a", Type: TypePullRequest, Thread: th}
	if !w.SynthesisReady("s1") {
		t.Fatal("the stored thread holds all six reviews")
	}
	win := ThreadWindow(w, "s1", MaxItemBytes)
	if got := win.Thread[len(win.Thread)-1].Reviews; len(got) != 6 {
		t.Fatalf("the read must name all six reviews, got %v", got)
	}
	if win.SynthesisReady("s1") || w.SynthesisFits("s1") {
		t.Fatal("five of six reviews must not pass as the comparison")
	}
	w.Thread = append(slices.Clone(th[2:12]), th[12]) // five reviews fit
	if !w.SynthesisFits("s1") {
		t.Fatal("five reviews that fit are a comparison")
	}
	if w.Thread[len(w.Thread)-1].Reviews != nil {
		t.Fatal("the read names the reviews on its copy; the stored turn is unchanged")
	}
}

// A retry that shared a run already in flight puts the earlier attempt back and keeps
// the new time; a later attempt, or an answer, wins.
func TestRevertAttempt(t *testing.T) {
	now := t2.Add(time.Hour)
	w := WorkItem{ID: "i", Thread: []Message{{ID: "t1", Role: RoleUser, Content: "q", Attempt: "a0", At: t0}}}
	s := w.Restamp(0, now)
	if !w.RevertAttempt(s) || w.Thread[0].Attempt != "a0" || !w.Thread[0].At.Equal(now) {
		t.Fatalf("RevertAttempt must restore the attempt and keep the time, got %+v", w.Thread[0])
	}
	s = w.Restamp(0, now)
	w.Restamp(0, now)
	if w.RevertAttempt(s) {
		t.Fatal("RevertAttempt must leave a later attempt alone")
	}
}

// Reviews that overlapped answer out of position; the comparison still pairs each
// request with its own answer, so the model gets alternating turns.
func TestSynthesisInputsPairsOverlappingReviews(t *testing.T) {
	w := WorkItem{ID: "a", Thread: []Message{
		{ID: "q1", Role: RoleUser, Kind: KindReviewRequest, Content: "Can you review this PR?", At: t0},
		{ID: "p1", Role: RoleUser, Kind: KindReviewRequest, Content: IndependentReviewPrompt, At: t0},
		{ID: "rp", ReplyTo: "p1", Role: RoleAgent, Content: "blind review", At: t1},
		{ID: "rq", ReplyTo: "q1", Role: RoleAgent, Content: "first review", At: t1},
		{ID: "s1", Role: RoleUser, Kind: KindSynthesisRequest, Content: SynthesisPrompt, Reviews: []string{"p1", "q1"}, At: t2},
	}}
	for _, legacy := range []bool{false, true} {
		if legacy {
			w.Thread[4].Reviews = nil
		}
		in, ok := w.SynthesisInputs("s1")
		var got []string
		for _, m := range in {
			got = append(got, m.ID)
		}
		if want := []string{"q1", "rq", "p1", "rp"}; !ok || !slices.Equal(got, want) {
			t.Fatalf("legacy=%v: inputs = %v %v, want request/answer pairs %v", legacy, got, ok, want)
		}
	}
}

// A legacy request's comparison is the reviews answered before it was asked: one
// that finished after the consensus was requested is not part of that round, so the
// round is still ready (and fits) without it.
func TestLegacySynthesisLeavesOutLateAnswers(t *testing.T) {
	w := WorkItem{ID: "a", Type: TypePullRequest, Thread: append(panelThread(),
		Message{ID: "q3", Role: RoleUser, Kind: KindReviewRequest, Content: "Can you review this PR?", At: t2},
		Message{ID: "s1", Role: RoleUser, Kind: KindSynthesisRequest, Content: SynthesisPrompt, At: t2},
		Message{ID: "r3", ReplyTo: "q3", Role: RoleAgent, Content: "a late review", At: t2},
	)}
	in, ok := w.SynthesisInputs("s1")
	if !ok || len(in) != 4 {
		t.Fatalf("want the two reviews answered before the round, got %+v %v", in, ok)
	}
	if !w.SynthesisFits("s1") {
		t.Fatal("a short legacy round must fit, so Retry is offered")
	}
}
