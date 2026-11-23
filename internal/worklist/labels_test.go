package worklist

import (
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
