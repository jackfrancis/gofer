package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// Each GitHub label renders as a pill in GitHub's order, right after the priority,
// with its color class; one with no known color keeps the neutral outline. The page
// links the stylesheet holding exactly the colors it uses, sorted.
func TestWorklistRendersGitHubLabels(t *testing.T) {
	items := []worklist.WorkItem{
		{ID: "github:o/r#1", Type: worklist.TypePullRequest,
			GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR",
				LabelColors: map[string]string{"kind/bug": "d73a4a", "size/S": "0E8A16", "evil": "x;}*{"}},
			Signals: worklist.Signals{Labels: []string{"size/S", "kind/bug", "legacy", "evil"}}},
		{ID: "github:o/r#2", Type: worklist.TypeIssue,
			GitHub:  worklist.GitHubRef{Number: 2, Repo: "o/r", Title: "An issue", LabelColors: map[string]string{"kind/bug": "d73a4a"}},
			Signals: worklist.Signals{Labels: []string{"kind/bug"}}},
		{ID: "github:o/r#3", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Number: 3, Repo: "o/r", Title: "Plain"}},
	}
	out := renderPage(t, pageData{View: "worklist", Items: items, LabelCSS: labelStylesheet(items)})

	if want := `<link rel="stylesheet" href="/labels.css?c=0e8a16,d73a4a">`; !strings.Contains(out, want) {
		t.Fatalf("missing %s in:\n%s", want, out)
	}
	first := out[strings.Index(out, "A PR"):strings.Index(out, "An issue")]
	want := `<span class="Label zz-priority Label--secondary">unranked</span>
            <span class="zz-labels"><span class="Label zz-ghlabel ghl-0e8a16">size/S</span><span class="Label zz-ghlabel ghl-d73a4a">kind/bug</span><span class="Label zz-ghlabel">legacy</span><span class="Label zz-ghlabel">evil</span></span>`
	if !strings.Contains(first, want) {
		t.Fatalf("labels not rendered after the priority, in order, with their colors:\n%s", first)
	}
	if plain := out[strings.Index(out, "Plain"):]; strings.Contains(plain[:strings.Index(plain, "zz-bar")], "zz-labels") {
		t.Fatal("an item without labels must render no label group")
	}
	if labelStylesheet(items[2:]) != "" {
		t.Fatal("a page with no colored label must link no label stylesheet")
	}
}

// The stylesheet holds a rule per well-formed color the query names, and nothing
// else, whatever the query carries; it is cacheable for good.
func TestLabelCSS(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	q := "?c=d73a4a,FFFFFF,d73a4a,0e8a16,%7D*%7Bcolor:red,url(x),123"
	h.LabelCSS(rec, httptest.NewRequest(http.MethodGet, LabelStylesheetPath+q, nil))
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	if ct := res.Header.Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("cache control %q", res.Header.Get("Cache-Control"))
	}
	want := ".ghl-d73a4a{background-color:#d73a4a;color:#ffffff;border-color:#d73a4a}\n" +
		".ghl-ffffff{background-color:#ffffff;color:#000000;border-color:#bfbfbf}\n" +
		".ghl-0e8a16{background-color:#0e8a16;color:#ffffff;border-color:#0e8a16}\n"
	if string(body) != want {
		t.Fatalf("stylesheet:\n%s\nwant:\n%s", body, want)
	}
}

// Text is black on a light label and white on a dark one, as GitHub draws them; only
// a nearly white label gets a darker outline.
func TestLabelRuleContrast(t *testing.T) {
	for c, want := range map[string]string{
		"000000": "color:#ffffff;border-color:#000000",
		"fbca04": "color:#000000;border-color:#fbca04", // GitHub's yellow
		"0075ca": "color:#ffffff;border-color:#0075ca", // GitHub's documentation blue
		"a2eeef": "color:#000000;border-color:#a2eeef", // GitHub's enhancement cyan
		"ededed": "color:#000000;border-color:#ededed",
		"f9f9f9": "color:#000000;border-color:#b9b9b9",
	} {
		if got := labelRule(c); !strings.Contains(got, want) {
			t.Errorf("labelRule(%s) = %s, want %s", c, got, want)
		}
	}
}
