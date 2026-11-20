package worklist

import (
	"strings"
	"testing"
)

// An item too large to read whole is projected to MaxBriefBytes, and the write-back
// of what a run read restores every cut field it left alone, keeps every field it
// rewrote, and never stores the projection.
func TestProjectionIsUndoneOnWriteBack(t *testing.T) {
	long := strings.Repeat("é", 200<<10) // 400 KiB
	stored := WorkItem{
		ID:      "github:o/r#1",
		GitHub:  GitHubRef{Repo: "o/r", Number: 1, Title: long},
		Signals: Signals{Labels: []string{long, "small"}, Participants: 2, Proposed: &AxisProposal{Relevance: 0.2, Rationale: long}},
		Meta:    Metadata{Rationale: long},
		Thread:  []Message{{ID: "m", Role: RoleUser, Content: "keep me"}},
	}
	if encodedSize(stored) <= MaxBriefBytes {
		t.Fatal("fixture must be over the brief limit")
	}
	read := Project(stored)
	if n := encodedSize(read); n > MaxBriefBytes {
		t.Fatalf("projected item is %d bytes, want ≤ %d", n, MaxBriefBytes)
	}
	if read.Thread != nil || len(read.Projected) == 0 || read.GitHub.Title == long {
		t.Fatalf("projection = %+v", read.Projected)
	}
	if !strings.HasPrefix(long, read.GitHub.Title) || len(read.GitHub.Title) == 0 {
		t.Fatal("a cut string must be a prefix of the original, on a character boundary")
	}
	if stored.Signals.Proposed.Rationale != long {
		t.Fatal("Project changed the stored item")
	}

	// The run enriches a signal and re-proposes the axes; it leaves the rest as read.
	run := read
	run.Signals.Participants = 5
	run.Signals.Proposed = &AxisProposal{Relevance: 0.9, Rationale: "fresh"}
	got := MergeRunItems([]WorkItem{stored}, []WorkItem{run})[0]
	if got.Projected != nil {
		t.Fatal("a projection must never be stored")
	}
	if got.GitHub.Title != long || got.Meta.Rationale != long || len(got.Signals.Labels) != 2 || got.Signals.Labels[0] != long {
		t.Fatal("fields the run left as read must take their stored values again")
	}
	if got.Signals.Participants != 5 || got.Signals.Proposed.Rationale != "fresh" || got.Signals.Proposed.Relevance != 0.9 {
		t.Fatalf("fields the run rewrote must keep the run's values: %+v", got.Signals.Proposed)
	}
	if len(got.Thread) != 1 {
		t.Fatal("the stored thread must stay")
	}

	// An item that fits is not projected, and a new item's marks are dropped.
	if small := Project(WorkItem{ID: "s", GitHub: GitHubRef{Title: "t"}}); small.Projected != nil {
		t.Fatal("an item that fits must not be projected")
	}
	if fresh := MergeRunItems(nil, []WorkItem{read})[0]; fresh.Projected != nil {
		t.Fatal("a new item must not store projection marks")
	}
}

func TestTruncateUTF8(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{{"héllo", 2, "h"}, {"héllo", 3, "hé"}, {"abc", 5, "abc"}, {"abc", 0, ""}, {"日本", 4, "日"}} {
		if got := truncateUTF8(c.in, c.n); got != c.want {
			t.Fatalf("truncateUTF8(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
