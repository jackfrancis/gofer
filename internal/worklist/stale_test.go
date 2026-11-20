package worklist

import (
	"testing"
	"time"
)

func TestResearchStale(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	research := func(m int, turns ...string) *ResearchAdjustment {
		return &ResearchAdjustment{Relevance: 1, Impact: 1, Engagement: 1, Urgency: 1, AppliedAt: at(m), Turns: turns}
	}
	turn := func(id, role string, m int, hidden bool) Message {
		return Message{ID: id, Role: role, Content: id, At: at(m), Hidden: hidden}
	}
	answered := []Message{turn("u1", RoleUser, 1, false), turn("a1", RoleAgent, 2, false)}

	cases := []struct {
		name string
		item WorkItem
		want bool
	}{
		{"no discussion", WorkItem{GitHub: GitHubRef{UpdatedAt: at(5)}}, false},
		{"only hidden turns", WorkItem{Thread: []Message{turn("u1", RoleUser, 1, true), turn("a1", RoleAgent, 2, true)}}, false},
		{"never researched", WorkItem{Thread: answered}, true},
		{"current research", WorkItem{GitHub: GitHubRef{UpdatedAt: at(0)}, Thread: answered,
			Signals: Signals{Research: research(3, "u1", "a1")}}, false},
		{"a reply after the research (a failed post-reply adjustment)", WorkItem{Thread: answered,
			Signals: Signals{Research: research(1, "u1")}}, true},
		{"a GitHub update after the research", WorkItem{GitHub: GitHubRef{UpdatedAt: at(10)}, Thread: answered,
			Signals: Signals{Research: research(3, "u1", "a1")}}, true},
		{"a later turn that is hidden", WorkItem{Thread: append(append([]Message{}, answered...), turn("u2", RoleUser, 9, true)),
			Signals: Signals{Research: research(3, "u1", "a1")}}, false},
		{"a turn the research read is now hidden", WorkItem{Thread: []Message{
			turn("u1", RoleUser, 1, true), turn("a1", RoleAgent, 2, false), turn("u2", RoleUser, 3, false)},
			Signals: Signals{Research: research(4, "u1", "a1", "u2")}}, true},
		{"legacy research without turns is judged by time", WorkItem{Thread: answered,
			Signals: Signals{Research: research(3)}}, false},
		{"hidden item", WorkItem{Thread: answered, Meta: Metadata{HiddenAt: at(4)}}, false},
		{"closed item", WorkItem{Thread: answered, Meta: Metadata{CompletedAt: at(4)}}, false},
		{"human override", WorkItem{Thread: answered, Meta: Metadata{Origin: OriginUser}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResearchStale(tc.item); got != tc.want {
				t.Fatalf("ResearchStale = %v, want %v", got, tc.want)
			}
		})
	}
}

// A research read (no turn) leaves hidden turns out of the window: the model never
// receives them.
func TestThreadWindowResearchOmitsHiddenTurns(t *testing.T) {
	item := WorkItem{ID: "i", Thread: []Message{
		{ID: "u1", Role: RoleUser, Content: "keep me"},
		{ID: "u2", Role: RoleUser, Content: "secret", Hidden: true},
		{ID: "a2", Role: RoleAgent, Content: "answer", ReplyTo: "u1"},
	}}
	w := ThreadWindow(item, "", MaxItemBytes)
	if len(w.Thread) != 2 || w.Thread[0].ID != "u1" || w.Thread[1].ID != "a2" {
		t.Fatalf("research window = %+v, want u1 and a2 only", w.Thread)
	}
	for _, m := range w.Thread {
		if m.Hidden || m.Content == "secret" {
			t.Fatalf("hidden turn in the research window: %+v", m)
		}
	}
}
