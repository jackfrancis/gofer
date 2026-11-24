package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// recordSynth records the synthesis turns the agent plane hands off.
type recordSynth struct{ refs chan worklist.TurnRef }

func (s recordSynth) Synthesize(_ context.Context, ownerID string, ref worklist.TurnRef) {
	if ownerID != "u1" {
		panic("synthesis for the wrong owner: " + ownerID)
	}
	s.refs <- ref
}

// The answer to a review panel's blind review chains the consensus synthesis turn in
// the same update and hands it to the synthesizer, once: a resent reply chains
// nothing more. A panel review with no earlier review to weigh chains nothing.
func TestAgentReplyChainsPanelSynthesis(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	panel := worklist.Message{ID: "p1", Role: worklist.RoleUser, Content: worklist.IndependentReviewPrompt, Kind: worklist.KindReviewRequest, At: t0.Add(time.Minute)}
	reply := map[string]any{
		"item_id":     "a",
		"in_reply_to": "p1",
		"message":     worklist.Message{ID: "r2", Content: "blind review", Model: "alt", At: t0.Add(2 * time.Minute)},
	}

	t.Run("after an earlier review", func(t *testing.T) {
		f := newPlane(t)
		synth := recordSynth{refs: make(chan worklist.TurnRef, 4)}
		f.handler.UseSynthesizer(synth)
		f.store.Seed("u1", worklist.WorkItem{ID: "a", Type: worklist.TypePullRequest, Thread: []worklist.Message{
			{ID: "q1", Role: worklist.RoleUser, Content: "Can you review this PR?", Kind: worklist.KindReviewRequest, At: t0},
			{ID: "r1", Role: worklist.RoleAgent, ReplyTo: "q1", Content: "first review", Model: "default", At: t0.Add(30 * time.Second)},
			panel,
		}})
		full := f.mint("signals:read", "metadata:write")
		for range 2 { // the second post is a resend
			if code := f.post(t, f.reply, "/agent/worklist/reply", full, reply); code != http.StatusOK {
				t.Fatalf("want 200, got %d", code)
			}
		}
		var ref worklist.TurnRef
		select {
		case ref = <-synth.refs:
		case <-time.After(5 * time.Second):
			t.Fatal("the synthesis turn was never dispatched")
		}
		got, _ := f.store.List(context.Background(), "u1")
		th := got[0].Thread
		if len(th) != 5 {
			t.Fatalf("want the reply and one synthesis turn appended, got %+v", th)
		}
		last := th[4]
		if last.Role != worklist.RoleUser || last.Kind != worklist.KindSynthesisRequest || last.Content != worklist.SynthesisPrompt || last.ID == "" {
			t.Fatalf("want a synthesis request last, got %+v", last)
		}
		if ref != (worklist.TurnRef{Item: "a", Turn: last.ID}) {
			t.Fatalf("dispatched %+v, want the stored synthesis turn %q", ref, last.ID)
		}
		select {
		case extra := <-synth.refs:
			t.Fatalf("a resent reply must not chain again, got %+v", extra)
		case <-time.After(50 * time.Millisecond):
		}
	})

	t.Run("panel first", func(t *testing.T) {
		f := newPlane(t)
		synth := recordSynth{refs: make(chan worklist.TurnRef, 1)}
		f.handler.UseSynthesizer(synth)
		f.store.Seed("u1", worklist.WorkItem{ID: "a", Type: worklist.TypePullRequest, Thread: []worklist.Message{panel}})
		if code := f.post(t, f.reply, "/agent/worklist/reply", f.mint("signals:read", "metadata:write"), reply); code != http.StatusOK {
			t.Fatalf("want 200, got %d", code)
		}
		got, _ := f.store.List(context.Background(), "u1")
		if th := got[0].Thread; len(th) != 2 || th[1].Content != "blind review" {
			t.Fatalf("want only the reply appended, got %+v", th)
		}
		select {
		case ref := <-synth.refs:
			t.Fatalf("nothing to weigh, so nothing to dispatch; got %+v", ref)
		case <-time.After(50 * time.Millisecond):
		}
	})
}
