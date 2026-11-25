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
// one repeats), with base commit "base0".
func diffServer(t *testing.T, n, changed int, heads ...string) *httptest.Server {
	t.Helper()
	return revServer(t, n, changed, func(read int) (string, string) { return heads[min(read, len(heads)-1)], "base0" })
}

// revServer is diffServer with rev naming the head and base commit of each
// successive pull read.
func revServer(t *testing.T, n, changed int, rev func(read int) (head, baseSHA string)) *httptest.Server {
	t.Helper()
	reads := 0
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/1":
			head, baseSHA := rev(reads)
			reads++
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "open", "head": map[string]any{"sha": head}, "base": map[string]any{"ref": "main", "sha": baseSHA, "repo": map[string]any{"full_name": "o/r"}}, "changed_files": changed})
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

// A push while the files are read moves the comparison: the read is tried again
// and settles on the new head. One that keeps moving never settles, and an
// unsettled snapshot has no digest.
func TestPullDiffSettled(t *testing.T) {
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	d, err := newTestClient(diffServer(t, 2, 2, diffHead, pushed)).PullDiff(context.Background(), "t", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Settled || d.HeadSHA != pushed || d.Digest() == "" {
		t.Errorf("after one push: settled %v on %s", d.Settled, d.HeadSHA)
	}
	moving := revServer(t, 2, 2, func(read int) (string, string) { return fmt.Sprintf("%040d", read), "base0" })
	if d, _ := newTestClient(moving).PullDiff(context.Background(), "t", "o/r", 1); d.Settled || d.Digest() != "" || d.Unverifiable() == "" {
		t.Errorf("a head that keeps moving settled: %+v", d)
	}
}

// The base commit is part of the comparison: a stacked dependency merged while the
// files are read moves it without moving the head, the base's name or the count.
// The read is tried again, so a base that moved once (an unrelated advance) still
// settles; one that keeps moving does not.
func TestPullDiffSettlesOnTheBaseCommit(t *testing.T) {
	once := revServer(t, 2, 2, func(read int) (string, string) {
		if read == 0 {
			return diffHead, "base0"
		}
		return diffHead, "base1"
	})
	if d, _ := newTestClient(once).PullDiff(context.Background(), "t", "o/r", 1); !d.Settled || d.Digest() == "" {
		t.Errorf("a base that moved once did not settle: %+v", d)
	}
	moving := revServer(t, 2, 2, func(read int) (string, string) { return diffHead, fmt.Sprintf("base%d", read) })
	if d, _ := newTestClient(moving).PullDiff(context.Background(), "t", "o/r", 1); d.Settled || d.Digest() != "" {
		t.Errorf("a base that keeps moving settled: %+v", d)
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
