package checkout

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLiveCheckout exercises the real path — download a GitHub tarball, unpack it
// into the process's temp directory, search it — against a public repository. It is
// skipped unless GOFER_LIVE_TARBALL names an archive URL, e.g.
//
//	GOFER_LIVE_TARBALL=https://api.github.com/repos/kubernetes/autoscaler/tarball/cluster-autoscaler-1.35.3
//
// Its purpose is to be compiled (go test -c) and run INSIDE the runtime's sandbox
// (the runtime image's base, non-root, under the provider's RuntimeClass) to verify
// what unit tests cannot: that the sandbox has writable scratch space, that it can
// reach GitHub's archive host, and how long a real checkout and search take there.
// The request is unauthenticated, so it needs no credential.
func TestLiveCheckout(t *testing.T) {
	url := os.Getenv("GOFER_LIVE_TARBALL")
	if url == "" {
		t.Skip("GOFER_LIVE_TARBALL not set")
	}
	pattern := os.Getenv("GOFER_LIVE_PATTERN")
	if pattern == "" {
		pattern = `grpc\.NewServer\(`
	}
	t.Logf("uid=%d tmpdir=%s", os.Getuid(), os.TempDir())

	began := time.Now()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	dir, err := os.MkdirTemp("", "gofer-src-")
	if err != nil {
		t.Fatalf("no writable scratch space: %v", err)
	}
	st, err := Unpack(resp.Body, dir, Limits{})
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("unpack: %v (after %s; %d files, %s)", err, time.Since(began), st.Files, human(st.Bytes))
	}
	tr, err := Open(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checkout: commit=%s files=%d dirs=%d bytes=%s symlinks=%d oversize=%d rejected=%d in %s",
		st.Commit, st.Files, st.Dirs, human(st.Bytes), st.Symlinks, len(st.Oversize), st.Rejected, time.Since(began).Round(time.Millisecond))

	for _, all := range []bool{false, true} {
		began = time.Now()
		res, err := tr.Search(context.Background(), SearchOptions{Pattern: pattern, IncludeExcluded: all})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("search %q include_excluded=%v: %d lines in %d files, %d files searched, skipped dirs=%d generated=%d, incomplete=%q, in %s",
			pattern, all, res.TotalMatches, res.FilesMatched, res.FilesSearched, res.SkippedDirsN, res.Generated, res.Incomplete, time.Since(began).Round(time.Millisecond))
		for i, m := range res.Matches {
			if i == 8 {
				break
			}
			t.Logf("  %s:%d: %s", m.Path, m.Line, m.Text)
		}
	}

	began = time.Now()
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("checkout not removed: %v", err)
	}
	t.Logf("removed in %s", time.Since(began).Round(time.Millisecond))
}
