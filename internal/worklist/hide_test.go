package worklist

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"
)

// A hidden item stays hidden through every write-back, whatever GitHub shows, and
// keeps only the run's GitHub fields, completion and labels: the signals and score a
// run sends are stripped, and its thread stays as stored.
func TestMergeRunItemsKeepsHiddenItemsHiddenAndUnscored(t *testing.T) {
	thread := []Message{{ID: "m1", Role: RoleUser, Content: "q", At: t0}, {ID: "m2", Role: RoleAgent, Content: "a", ReplyTo: "m1", At: t1}}
	stored := WorkItem{ID: "a", Thread: thread, CreatedAt: t0, Meta: Metadata{HiddenAt: t2},
		GitHub: GitHubRef{Title: "old", UpdatedAt: t1}, Signals: Signals{Labels: []string{"old"}}}
	for _, updated := range []time.Time{{}, t1, t2, t3} {
		run := WorkItem{ID: "a", GitHub: GitHubRef{Title: "new", UpdatedAt: updated},
			Signals: Signals{Labels: []string{"bug"}, Reasons: []Reason{ReasonAuthor}, Comments: 4, ObservedAt: t3,
				Proposed: &AxisProposal{Relevance: 1}, Research: &ResearchAdjustment{AppliedAt: t3}},
			Meta: Metadata{Rank: 0.9, Relevance: 1, Rationale: "on your radar as a comment you left", Origin: OriginAgent, CompletedAt: t3}}
		got := MergeRunItems([]WorkItem{stored}, []WorkItem{run})[0]
		if !got.Meta.HiddenAt.Equal(t2) || got.OnRadar() {
			t.Fatalf("updated %v: HiddenAt = %v, want the stored %v", updated, got.Meta.HiddenAt, t2)
		}
		if want := (Signals{Labels: []string{"bug"}}); !reflect.DeepEqual(got.Signals, want) {
			t.Fatalf("updated %v: signals = %+v, want only the run's labels", updated, got.Signals)
		}
		if want := (Metadata{HiddenAt: t2, CompletedAt: t3}); !reflect.DeepEqual(got.Meta, want) {
			t.Fatalf("updated %v: meta = %+v, want only the hide and completion marks", updated, got.Meta)
		}
		if got.GitHub.Title != "new" || len(got.Thread) != 2 || !got.CreatedAt.Equal(t0) {
			t.Fatalf("updated %v: want the run's GitHub fields and the stored thread, got %+v", updated, got)
		}
	}
}

// A hidden pull request keeps its CI summary, a GitHub field: through hiding, and
// through a write-back that carries none (KeepCI) as well as one that carries a
// fresh summary.
func TestHiddenItemKeepsCI(t *testing.T) {
	ci := &CISummary{SHA: "abc", Passed: 3, Failed: []string{"lint"}, FailedURLs: []string{"https://ci.example/lint"}}
	it := WorkItem{ID: "a", Type: TypePullRequest, GitHub: GitHubRef{CI: ci}, Meta: Metadata{Rank: 0.4}}
	HideItem(&it, t1)
	if !it.GitHub.CI.Equal(ci) {
		t.Fatalf("hiding keeps the CI summary, got %+v", it.GitHub.CI)
	}
	got := MergeRunItems([]WorkItem{it}, []WorkItem{{ID: "a", Type: TypePullRequest, Meta: Metadata{Rank: 0.9}}})[0]
	if !got.GitHub.CI.Equal(ci) || got.Meta.Rank != 0 {
		t.Fatalf("a write-back with no CI keeps the stored one and is still unscored, got %+v %+v", got.GitHub.CI, got.Meta)
	}
	fresh := &CISummary{SHA: "def", Passed: 4}
	got = MergeRunItems([]WorkItem{it}, []WorkItem{{ID: "a", Type: TypePullRequest, GitHub: GitHubRef{CI: fresh}}})[0]
	if !got.GitHub.CI.Equal(fresh) {
		t.Fatalf("a write-back's fresh CI lands on a hidden item, got %+v", got.GitHub.CI)
	}

	// Unhidden, the pre-hide summary is kept but marked stale, without touching the
	// stored copy; a refresh (which carries no CI) keeps it stale, and enrich's next
	// read replaces it, even when the jobs read the same.
	stored := it
	UnhideItem(&it)
	if c := it.GitHub.CI; c == nil || !c.Stale || c.Passed != 3 || stored.GitHub.CI.Stale {
		t.Fatalf("unhide keeps the pre-hide CI summary, marked stale (and only its own copy), got %+v / stored %+v", c, stored.GitHub.CI)
	}
	got = MergeRunItems([]WorkItem{it}, []WorkItem{{ID: "a", Type: TypePullRequest, Meta: Metadata{Rank: 0.5}}})[0]
	if c := got.GitHub.CI; c == nil || !c.Stale {
		t.Fatalf("a refresh after unhide keeps the stale CI, got %+v", c)
	}
	same := cloneCI(ci)
	if same.Equal(got.GitHub.CI) {
		t.Fatal("a fresh read must differ from the stale summary, or enrich would not write it")
	}
	got = MergeRunItems([]WorkItem{got}, []WorkItem{{ID: "a", Type: TypePullRequest, GitHub: GitHubRef{CI: same}}})[0]
	if c := got.GitHub.CI; c == nil || c.Stale {
		t.Fatalf("enrich's fresh read replaces the stale summary, got %+v", c)
	}
}

// A retired PR (closed, merged) unhidden keeps its CI as the last known result and
// awaits no refresh, since none revisits it until it reopens; a retirement learned
// after the unhide clears the flag too.
func TestUnhideRetiredItem(t *testing.T) {
	ci := &CISummary{SHA: "abc", Passed: 1}
	closed := WorkItem{ID: "a", Type: TypePullRequest, GitHub: GitHubRef{CI: ci}, Meta: Metadata{HiddenAt: t1, CompletedAt: t1}}
	UnhideItem(&closed)
	if closed.Meta.AwaitingRefresh || closed.GitHub.CI == nil || closed.GitHub.CI.Passed != 1 {
		t.Fatalf("a retired item keeps its CI and awaits no refresh, got %+v %+v", closed.Meta, closed.GitHub.CI)
	}

	open := WorkItem{ID: "b", Type: TypePullRequest, GitHub: GitHubRef{CI: ci}, Meta: Metadata{HiddenAt: t1}}
	UnhideItem(&open)
	if !open.Meta.AwaitingRefresh {
		t.Fatal("test premise: an open item awaits its refresh")
	}
	// The next complete fetch finds it closed: the retirement echoes the copy it read.
	retired := open
	retired.Meta.CompletedAt = t2
	retired.GitHub.CI = nil // retiring reads no CI
	got := MergeRunItems([]WorkItem{open}, []WorkItem{retired})[0]
	if got.Meta.AwaitingRefresh || got.GitHub.CI == nil || !got.GitHub.CI.Stale {
		t.Fatalf("a retired item awaits nothing and keeps its last known CI, got %+v %+v", got.Meta, got.GitHub.CI)
	}
}

// Hide deletes the item's judgment and keeps the first hide time; Unhide brings back
// an empty item awaiting a refresh. Neither touches the GitHub fields or thread.
func TestHideAndUnhideItem(t *testing.T) {
	it := WorkItem{ID: "a", GitHub: GitHubRef{Title: "t", LabelColors: map[string]string{"bug": "d73a4a"}},
		Thread:  []Message{{ID: "m", Role: RoleUser, Content: "q", At: t0}},
		Signals: Signals{Labels: []string{"bug"}, Reasons: []Reason{ReasonCommented}, ReviewRequestedBy: "x", Proposed: &AxisProposal{}},
		Meta:    Metadata{Rank: 0.5, Relevance: 0.5, Rationale: "on your radar as a comment you left", Origin: OriginUser, CompletedAt: t1}}
	if !HideItem(&it, t2) {
		t.Fatal("hiding a visible item changes it")
	}
	if !reflect.DeepEqual(it.Signals, Signals{Labels: []string{"bug"}}) || !reflect.DeepEqual(it.Meta, Metadata{HiddenAt: t2, CompletedAt: t1}) {
		t.Fatalf("hide must delete the signals and score, got %+v %+v", it.Signals, it.Meta)
	}
	if it.GitHub.Title != "t" || it.GitHub.LabelColor("bug") == "" || len(it.Thread) != 1 {
		t.Fatalf("hide keeps the GitHub fields and thread, got %+v", it)
	}
	if HideItem(&it, t3) || !it.Meta.HiddenAt.Equal(t2) {
		t.Fatalf("a repeated hide changes nothing and keeps the first time, HiddenAt = %v", it.Meta.HiddenAt)
	}
	it.Meta.CompletedAt = time.Time{} // it reopened while hidden (TestUnhideRetiredItem covers a closed one)
	if !UnhideItem(&it) {
		t.Fatal("unhiding a hidden item changes it")
	}
	if it.IsHidden() || !it.Meta.AwaitingRefresh || it.Meta.Rank != 0 || len(it.Signals.Reasons) != 0 {
		t.Fatalf("unhide brings back an empty item awaiting a refresh, got %+v", it.Meta)
	}
	if UnhideItem(&it) {
		t.Fatal("unhiding a visible item changes nothing")
	}
	// The refresh's write-back carries its own Metadata, which clears the flag.
	got := MergeRunItems([]WorkItem{it}, []WorkItem{{ID: "a", Meta: Metadata{Rank: 0.4}}})[0]
	if got.Meta.AwaitingRefresh || got.Meta.Rank != 0.4 {
		t.Fatalf("a run's write-back fills the item in, got %+v", got.Meta)
	}
}

// A hidden item takes no research adjustment.
func TestSetResearchSkipsHiddenItem(t *testing.T) {
	it := WorkItem{ID: "a", Meta: Metadata{HiddenAt: t1}}
	if changed, err := SetResearch(&it, ResearchAdjustment{AppliedAt: t2}); err != nil || changed || it.Signals.Research != nil {
		t.Fatalf("SetResearch on a hidden item = %v, %v, research %+v; want nothing set", changed, err, it.Signals.Research)
	}
}

// A run's on-radar selection (enrichment, ranking) leaves hidden items out, as the
// stores' brief reads do.
func TestSelectOpenSkipsHiddenItems(t *testing.T) {
	items := []WorkItem{{ID: "v"}, {ID: "h", Meta: Metadata{HiddenAt: t1}}, {ID: "c", Meta: Metadata{CompletedAt: t1}}}
	for _, sel := range []Selection{{Open: true}, {Open: true, Top: 10}} {
		page, _ := Select(items, sel, t3)
		if len(page) != 1 || page[0].ID != "v" {
			t.Fatalf("Select(%+v) = %+v, want only the item on the radar", sel, page)
		}
	}
	// An item awaiting its post-unhide refresh is on the radar and enriched (paged
	// reads), but holds no signals to rank, so the ranker's top-N leaves it out.
	back := WorkItem{ID: "b", Meta: Metadata{AwaitingRefresh: true}}
	withBack := append(slices.Clone(items), back)
	if page, _ := Select(withBack, Selection{Open: true}, t3); len(page) != 2 {
		t.Fatalf("a paged read includes the item awaiting refresh, got %+v", page)
	}
	if page, _ := Select(withBack, Selection{Open: true, Top: 10}, t3); len(page) != 1 || page[0].ID != "v" {
		t.Fatalf("the ranker's top-N leaves out the item awaiting refresh, got %+v", page)
	}
	if got := ApplyBriefQuery(items, BriefQuery{Open: true}); len(got) != 1 || got[0].ID != "v" {
		t.Fatalf("ApplyBriefQuery(open) = %+v, want only the item on the radar", got)
	}
	if got := ApplyBriefQuery(items, BriefQuery{}); len(got) != 3 {
		t.Fatalf("an unfiltered read still returns every item, got %d", len(got))
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

// Resolve lists only on-radar items, and hands back the hidden and completed ones it
// left out from the same read (OffRadar).
func TestResolveSplitsOffRadarItems(t *testing.T) {
	store := NewMemoryStore()
	hidden := WorkItem{ID: "h", Type: TypePullRequest, Meta: Metadata{HiddenAt: t1}}
	closed := WorkItem{ID: "c", Type: TypePullRequest, Meta: Metadata{CompletedAt: t1}}
	store.Seed("u1", hidden, WorkItem{ID: "v", Type: TypePullRequest}, closed)
	res, err := Resolve(context.Background(), store, nil, t3, "u1", DefaultSort, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != "v" {
		t.Fatalf("Items = %+v, want only the visible item", res.Items)
	}
	off := map[string]bool{}
	for _, it := range res.OffRadar {
		off[it.ID] = true
	}
	if len(res.OffRadar) != 2 || !off["h"] || !off["c"] {
		t.Fatalf("OffRadar = %+v, want the hidden and the closed item", res.OffRadar)
	}
	if len(res.Hidden) != 1 || res.Hidden[0].ID != "h" {
		t.Fatalf("Hidden = %+v, want the open hidden item", res.Hidden)
	}
}

// The hidden section lists open hidden items, most recently hidden first; a hidden
// item that closed leaves it, still hidden.
func TestResolveListsHiddenSection(t *testing.T) {
	store := NewMemoryStore()
	store.Seed("u1",
		WorkItem{ID: "early", Meta: Metadata{HiddenAt: t1}},
		WorkItem{ID: "closed", Meta: Metadata{HiddenAt: t3, CompletedAt: t3}},
		WorkItem{ID: "late", Meta: Metadata{HiddenAt: t2}},
		WorkItem{ID: "v"},
	)
	res, err := Resolve(context.Background(), store, nil, t3, "u1", DefaultSort, true)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range res.Hidden {
		ids = append(ids, it.ID)
		if it.Meta.Rank != 0 || it.Meta.ScoredAt != (time.Time{}) {
			t.Fatalf("a hidden item is never scored, got %+v", it.Meta)
		}
	}
	if !reflect.DeepEqual(ids, []string{"late", "early"}) {
		t.Fatalf("Hidden = %v, want [late early]", ids)
	}
}
