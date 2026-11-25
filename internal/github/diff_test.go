package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
)

const diffHead = "0123456789abcdef0123456789abcdef01234567"

// diffServer serves o/r#1 with n text files (100 a page, no Link header), reporting
// changed files; heads names the head each successive pull read returns (the last
// one repeats).
func diffServer(t *testing.T, n, changed int, heads ...string) *httptest.Server {
	t.Helper()
	reads := 0
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/1":
			head := heads[min(reads, len(heads)-1)]
			reads++
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "open", "head": map[string]any{"sha": head}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}}, "changed_files": changed})
		case "/repos/o/r/pulls/1/files":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			var out []map[string]string
			for i := (page - 1) * 100; i < min(page*100, n); i++ {
				out = append(out, map[string]string{"filename": fmt.Sprintf("f%04d.go", i), "status": "modified", "sha": "blob" + strconv.Itoa(i), "patch": "@@ -1 +1 @@\n-a\n+b"})
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A diff is complete when as many files were read as the pull request reports,
// right up to GitHub's 3000 (thirty full pages with no next link); past that it is
// cut, and a cut list has no digest.
func TestPullDiffCompleteness(t *testing.T) {
	for _, tc := range []struct {
		n, changed int
		complete   bool
	}{
		{3000, 3000, true},
		{3000, 3001, false},
		{150, 150, true},
		{150, 151, false},
		{0, 0, true},
	} {
		srv := diffServer(t, tc.n, tc.changed, diffHead)
		d, err := newTestClient(srv).PullDiff(context.Background(), "t", "o/r", 1)
		if err != nil {
			t.Fatal(err)
		}
		if d.Complete != tc.complete || (d.Digest() != "") != tc.complete {
			t.Errorf("%d files of %d: complete %v, digest %q", tc.n, tc.changed, d.Complete, d.Digest())
		}
	}
}

// A push while the files are read leaves the snapshot unsettled: its files may not
// be the comparison of the head it names, so it has no digest.
func TestPullDiffSettled(t *testing.T) {
	srv := diffServer(t, 2, 2, diffHead, "fedcba9876543210fedcba9876543210fedcba98")
	d, err := newTestClient(srv).PullDiff(context.Background(), "t", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Settled || d.Digest() != "" || d.Unverifiable() == "" {
		t.Errorf("settled %v, digest %q", d.Settled, d.Digest())
	}
}

// A file with no patch other than a new one (a binary change) names only its head
// blob, so a moved merge base would leave the digest unchanged: such a diff is
// unverifiable. A new binary file has no base side, and is fine.
func TestPullDiffBinaryChange(t *testing.T) {
	base := PullDiff{HeadSHA: diffHead, BaseRef: "main", Complete: true, Settled: true}
	added := base
	added.Files = []DiffFile{{Path: "logo.png", Status: "added", SHA: "b1"}}
	if added.Digest() == "" {
		t.Errorf("a new binary file is unverifiable: %s", added.Unverifiable())
	}
	for _, status := range []string{"modified", "renamed", "removed", "changed"} {
		d := base
		d.Files = []DiffFile{{Path: "icon.png", Status: status, SHA: "b1"}}
		if d.Digest() != "" {
			t.Errorf("%s binary file has a digest", status)
		}
	}
}
