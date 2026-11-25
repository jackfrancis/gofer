package ghcomment_test

import (
	"testing"

	"github.com/jackfrancis/gofer/internal/ghcomment"
	"github.com/jackfrancis/gofer/internal/github"
)

// The workload records a review's diff with github.DiffDigest, and the web tier
// checks an approval against ghcomment.DiffDigest of the files it reads before
// creating the review: the two must agree on the same list, whatever its order.
func TestDiffDigestsAgree(t *testing.T) {
	wl := []github.DiffFile{
		{Path: "b.go", Status: "modified", SHA: "blob-b", Patch: "@@ -1 +1 @@\n-x\n+y"},
		{Path: "new.go", PreviousPath: "old.go", Status: "renamed", SHA: "blob-n"},
		{Path: "a.png", Status: "added", SHA: "blob-a"},
	}
	web := []ghcomment.File{
		{Filename: "a.png", Status: "added", SHA: "blob-a"},
		{Filename: "new.go", PreviousFilename: "old.go", Status: "renamed", SHA: "blob-n"},
		{Filename: "b.go", Status: "modified", SHA: "blob-b", Patch: "@@ -1 +1 @@\n-x\n+y"},
	}
	if g, w := github.DiffDigest(wl), ghcomment.DiffDigest(web); g != w {
		t.Fatalf("github.DiffDigest %s != ghcomment.DiffDigest %s", g, w)
	}
	web[2].Patch += "\n+z"
	if github.DiffDigest(wl) == ghcomment.DiffDigest(web) {
		t.Error("a changed patch leaves the digest unchanged")
	}
	if (github.PullDiff{Files: wl}).Digest() != "" {
		t.Error("a diff not read whole has a digest")
	}
}
