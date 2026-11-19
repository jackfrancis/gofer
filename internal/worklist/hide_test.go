package worklist

import (
	"testing"
	"time"
)

// A hidden item stays hidden through any write-back whose GitHub update time is not
// after the hide, and resurfaces, thread intact, once a run brings a newer one.
func TestMergeRunItemsAutoUnhide(t *testing.T) {
	thread := []Message{{ID: "m1", Role: RoleUser, Content: "q", At: t0}, {ID: "m2", Role: RoleAgent, Content: "a", ReplyTo: "m1", At: t1}}
	stored := WorkItem{ID: "a", Thread: thread, CreatedAt: t0, Meta: Metadata{HiddenAt: t2},
		GitHub: GitHubRef{Title: "old", UpdatedAt: t1}}
	merge := func(updated time.Time) WorkItem {
		run := WorkItem{ID: "a", GitHub: GitHubRef{Title: "new", UpdatedAt: updated}}
		return MergeRunItems([]WorkItem{stored}, []WorkItem{run})[0]
	}

	for _, tc := range []struct {
		name    string
		updated time.Time
	}{
		{"no GitHub update time (a scoring run)", time.Time{}},
		{"unchanged since before the hide", t1},
		{"updated at the very instant of the hide", t2},
	} {
		if got := merge(tc.updated); !got.Meta.HiddenAt.Equal(t2) || got.OnRadar() {
			t.Errorf("%s: HiddenAt = %v, want the stored %v (still hidden)", tc.name, got.Meta.HiddenAt, t2)
		}
	}

	got := merge(t3)
	if !got.Meta.HiddenAt.IsZero() || !got.OnRadar() {
		t.Fatalf("a GitHub update after the hide must unhide the item, HiddenAt = %v", got.Meta.HiddenAt)
	}
	if len(got.Thread) != 2 || got.Thread[1].Content != "a" || !got.CreatedAt.Equal(t0) || got.GitHub.Title != "new" {
		t.Fatalf("a resurfaced item keeps its thread and takes the run's GitHub fields, got %+v", got)
	}
}

// OnRadar is the single rule for what the radar shows and what bulk actions draw
// from: neither hidden nor completed.
func TestOnRadarGatesBulkPredicates(t *testing.T) {
	now := t3
	stale := time.Minute
	unreviewed := WorkItem{ID: "u", Type: TypePullRequest}
	reviewed := WorkItem{ID: "r", Type: TypePullRequest, Thread: []Message{
		{ID: "q", Role: RoleUser, Kind: KindReviewRequest, At: t0},
		{ID: "a", Role: RoleAgent, ReplyTo: "q", At: t1},
	}}
	if !unreviewed.OnRadar() || !unreviewed.NeedsReview(now, stale) || !reviewed.NeedsSecondOpinion(now, stale) {
		t.Fatal("a visible, open PR is on the radar and eligible for the bulk actions")
	}
	for name, off := range map[string]func(*WorkItem){
		"hidden": func(w *WorkItem) { w.Meta.HiddenAt = t2 },
		"closed": func(w *WorkItem) { w.Meta.CompletedAt = t2 },
	} {
		u, r := unreviewed, reviewed
		off(&u)
		off(&r)
		if u.OnRadar() || r.OnRadar() {
			t.Errorf("%s: a %s item is off the radar", name, name)
		}
		if u.NeedsReview(now, stale) {
			t.Errorf("%s: Review all must not draw a %s PR", name, name)
		}
		if r.NeedsSecondOpinion(now, stale) {
			t.Errorf("%s: the second-opinion batch must not draw a %s PR", name, name)
		}
	}
}
