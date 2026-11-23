package worklist

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
)

// A label color reaches a stylesheet, so only six hex digits pass, normalized.
func TestNormalizeLabelColor(t *testing.T) {
	for in, want := range map[string]string{
		"d73a4a":  "d73a4a",
		"D73A4A":  "d73a4a",
		"#0e8a16": "0e8a16",
		"":        "",
		"fff":     "",
		"0e8a1":   "",
		"0e8a166": "",
		"red":     "",
		"zzzzzz":  "",
		"12345;":  "",
		"##12345": "",
		"12}*{x":  "",
	} {
		if got := NormalizeLabelColor(in); got != want {
			t.Errorf("NormalizeLabelColor(%q) = %q, want %q", in, got, want)
		}
	}
	g := GitHubRef{LabelColors: map[string]string{"bug": "D73A4A", "bad": "url(x)"}}
	if g.LabelColor("bug") != "d73a4a" || g.LabelColor("bad") != "" || g.LabelColor("none") != "" {
		t.Fatalf("LabelColor: %q %q %q", g.LabelColor("bug"), g.LabelColor("bad"), g.LabelColor("none"))
	}
}

// Projection keeps the colors of the labels it keeps, and only well-formed ones, so
// the colors are bounded as the labels are; the write-back restores them whole.
func TestProjectionCutsLabelColorsWithLabels(t *testing.T) {
	var labels []string
	colors := map[string]string{"bad": "not-a-color"}
	for i := range 3 * projectedList {
		l := fmt.Sprintf("label-%03d", i)
		labels = append(labels, l)
		colors[l] = fmt.Sprintf("%06x", i)
	}
	labels = append(labels, "bad")
	stored := WorkItem{ID: "big", GitHub: GitHubRef{Title: strings.Repeat("t", MaxBriefBytes), LabelColors: colors},
		Signals: Signals{Labels: labels}}
	if encodedSize(stored) <= MaxBriefBytes {
		t.Fatal("fixture must be over the brief limit")
	}
	read := Project(stored)
	if _, cut := read.Projected["github.label_colors"]; !cut {
		t.Fatalf("label colors not marked cut: %v", read.Projected)
	}
	if len(read.GitHub.LabelColors) == 0 || len(read.GitHub.LabelColors) > len(read.Signals.Labels) {
		t.Fatalf("%d colors for %d labels", len(read.GitHub.LabelColors), len(read.Signals.Labels))
	}
	for l, c := range read.GitHub.LabelColors {
		if colors[l] != c || NormalizeLabelColor(c) != c {
			t.Fatalf("kept color %q for %q", c, l)
		}
	}
	got := MergeRunItems([]WorkItem{stored}, []WorkItem{read})[0]
	if !maps.Equal(got.GitHub.LabelColors, colors) {
		t.Fatal("the write-back must restore the stored colors")
	}
}

// An oversized item whose only colored label falls past the labels projection keeps
// leaves the run no colors; the run's unchanged write-back, through JSON as the agent
// plane carries it, must restore the stored color rather than erase it.
func TestProjectionRestoresColorsNoKeptLabelHas(t *testing.T) {
	var labels []string
	for i := range projectedList {
		labels = append(labels, fmt.Sprintf("plain-%02d", i))
	}
	labels = append(labels, "colored") // the 33rd: past the cut, every weight equal
	colors := map[string]string{"colored": "d73a4a"}
	stored := WorkItem{ID: "big", GitHub: GitHubRef{Title: strings.Repeat("t", MaxBriefBytes), LabelColors: colors},
		Signals: Signals{Labels: labels}}
	read := Project(stored)
	if len(read.Signals.Labels) != projectedList || read.GitHub.LabelColors != nil {
		t.Fatalf("fixture: want %d labels and no colors read, got %d and %v",
			projectedList, len(read.Signals.Labels), read.GitHub.LabelColors)
	}
	b, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	var back WorkItem
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got := MergeRunItems([]WorkItem{stored}, []WorkItem{back})[0]
	if !maps.Equal(got.GitHub.LabelColors, colors) || len(got.Signals.Labels) != len(labels) {
		t.Fatalf("write-back stored %d labels and colors %v, want %d and %v",
			len(got.Signals.Labels), got.GitHub.LabelColors, len(labels), colors)
	}
}
