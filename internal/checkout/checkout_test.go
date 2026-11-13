package checkout

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// entry is one tar member for a test archive.
type entry struct {
	name string
	body string
	typ  byte // 0 = regular file
	link string
	mode int64
}

// archive builds a gzip'd tar the way GitHub does: a pax global header carrying the
// commit, then the members.
func archive(t testing.TB, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
		PAXRecords: map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(e.body)), Linkname: e.link}
		if e.mode != 0 {
			h.Mode = e.mode
		}
		if e.typ != 0 {
			h.Typeflag = e.typ
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// unpack extracts into a fresh directory NESTED in a temp dir, so a test can assert
// nothing was written beside it.
func unpack(t testing.TB, data []byte, lim Limits) (dir string, st Stats, err error) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err = Unpack(bytes.NewReader(data), dir, lim)
	return dir, st, err
}

func tree(t testing.TB, entries ...entry) *Tree {
	t.Helper()
	dir, st, err := unpack(t, archive(t, entries...), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := Open(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestUnpackStripsTopLevelAndRecordsCommit(t *testing.T) {
	dir, st, err := unpack(t, archive(t,
		entry{name: "o-r-0123456/", typ: tar.TypeDir},
		entry{name: "o-r-0123456/go.mod", body: "module x\n"},
		entry{name: "o-r-0123456/pkg/a/a.go", body: "package a\n"}, // parent dirs implied
		entry{name: "o-r-0123456/run.sh", body: "#!/bin/sh\n", mode: 0o4755},
	), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Commit != "0123456789abcdef0123456789abcdef01234567" || st.Files != 3 || st.Rejected != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pkg/a/a.go")); err != nil || string(b) != "package a\n" {
		t.Fatalf("pkg/a/a.go = %q, %v", b, err)
	}
	// Modes from the archive are never honoured: no exec, no setuid.
	if fi, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil || fi.Mode() != 0o644 {
		t.Fatalf("run.sh mode = %v, %v", fi.Mode(), err)
	}
}

// Names that would resolve outside the checkout are refused, and nothing lands
// beside the checkout directory.
func TestUnpackRejectsPathTraversal(t *testing.T) {
	dir, st, err := unpack(t, archive(t,
		entry{name: "top/ok.txt", body: "ok"},
		entry{name: "top/../../evil.txt", body: "x"},
		entry{name: "top/a/../../../evil2.txt", body: "x"},
		entry{name: "../evil3.txt", body: "x"},
		entry{name: "/etc/evil4.txt", body: "x"},
		entry{name: "top/..", typ: tar.TypeDir},
		entry{name: "top/a\\..\\b.txt", body: "x"},
		entry{name: "top//double.txt", body: "x"},
	), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 1 || st.Rejected != 7 {
		t.Fatalf("files=%d rejected=%d, want 1 and 7", st.Files, st.Rejected)
	}
	assertOnly(t, filepath.Dir(dir), "checkout")
	assertOnly(t, dir, "ok.txt")
}

// Links are never created, so nothing can point out of the tree — including a later
// entry that tries to write THROUGH an earlier symlink.
func TestUnpackNeverCreatesLinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, st, err := unpack(t, archive(t,
		entry{name: "top/escape", typ: tar.TypeSymlink, link: outside},
		entry{name: "top/escape/pwned.txt", body: "x"},
		entry{name: "top/rel", typ: tar.TypeSymlink, link: "../../.."},
		entry{name: "top/hard", typ: tar.TypeLink, link: "/etc/passwd"},
		entry{name: "top/fifo", typ: tar.TypeFifo},
		entry{name: "top/dev", typ: tar.TypeChar},
	), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Symlinks != 3 || st.Rejected != 2 {
		t.Fatalf("symlinks=%d rejected=%d, want 3 and 2", st.Symlinks, st.Rejected)
	}
	assertOnly(t, outside)
	// "escape" exists only as a real directory holding the file written under it.
	if fi, err := os.Lstat(filepath.Join(dir, "escape")); err != nil || !fi.IsDir() {
		t.Fatalf("escape = %v, %v; want a plain directory", fi, err)
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !d.Type().IsRegular() {
			t.Errorf("non-regular entry in checkout: %s (%v)", p, d.Type())
		}
		return nil
	})
}

func TestUnpackLimits(t *testing.T) {
	big := strings.Repeat("x", 4096)
	many := []entry{}
	for i := 0; i < 20; i++ {
		many = append(many, entry{name: fmt.Sprintf("top/f%02d.txt", i), body: big})
	}
	cases := []struct {
		name string
		lim  Limits
		want string
	}{
		{"compressed size", Limits{MaxArchiveBytes: 64}, "compressed"},
		{"unpacked size", Limits{MaxUnpackedBytes: 10 * 4096}, "unpacked"},
		{"entry count", Limits{MaxEntries: 5}, "more than 5 files"},
		// Every file is over the per-file cap, so none is written — but the stream is
		// still bounded, which is what stops an archive of nothing but skipped bytes.
		{"decompressed stream", Limits{MaxFileBytes: 16, MaxUnpackedBytes: 4096}, "expands past"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := unpack(t, archive(t, many...), c.lim)
			if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want ErrTooLarge containing %q", err, c.want)
			}
		})
	}
}

// One oversize file is left out and remembered; the rest of the tree is usable, and
// reading the missing file says why it is missing.
func TestUnpackSkipsOversizeFile(t *testing.T) {
	dir, st, err := unpack(t, archive(t,
		entry{name: "top/small.go", body: "package x\n"},
		entry{name: "top/blob.bin", body: strings.Repeat("b", 2000)},
	), Limits{MaxFileBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 1 || st.Oversize["blob.bin"] != 2000 {
		t.Fatalf("stats = %+v", st)
	}
	tr, err := Open(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if _, err := tr.Read("blob.bin", 0, 0); err == nil || !strings.Contains(err.Error(), "left out of the checkout") {
		t.Fatalf("Read(blob.bin) err = %v", err)
	}
}

func TestUnpackRejectsCorruptArchive(t *testing.T) {
	if _, _, err := unpack(t, []byte("not a gzip stream"), Limits{}); err == nil {
		t.Fatal("want an error for a non-gzip body")
	}
	data := archive(t, entry{name: "top/a.txt", body: strings.Repeat("a", 5000)})
	if _, _, err := unpack(t, data[:len(data)/2], Limits{}); err == nil {
		t.Fatal("want an error for a truncated archive")
	}
}

func TestCloseRemovesDirectory(t *testing.T) {
	dir, st, err := unpack(t, archive(t, entry{name: "top/a.txt", body: "a"}), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := Open(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("checkout directory still exists: %v", err)
	}
}

func searchTree(t testing.TB) *Tree {
	return tree(t,
		entry{name: "top/cmd/main.go", body: "package main\n\nfunc main() {\n\ts := grpc.NewServer()\n\t_ = s\n}\n"},
		entry{name: "top/pkg/srv/srv.go", body: "package srv\n// grpc.NewServer is wrapped here\nvar x = grpc.NewServer(opts...)\n"},
		entry{name: "top/pkg/srv/srv_test.go", body: "package srv\nvar y = grpc.NewServer()\n"},
		entry{name: "top/vendor/google.golang.org/grpc/server.go", body: "package grpc\nfunc NewServer() {}\n// grpc.NewServer\n"},
		entry{name: "top/pkg/testdata/fixture.go", body: "grpc.NewServer()\n"},
		entry{name: "top/pkg/api/zz_generated.deepcopy.go", body: "grpc.NewServer()\n"},
		entry{name: "top/pkg/api/mock.go", body: "// Code generated by mockgen. DO NOT EDIT.\ngrpc.NewServer()\n"},
		entry{name: "top/go.sum", body: "grpc.NewServer h1:abc\n"},
		entry{name: "top/bin/tool", body: "\x7fELF\x00\x00grpc.NewServer\x00"},
		entry{name: "top/README.md", body: "Uses GRPC.newserver under the hood.\n"},
	)
}

func paths(ms []Match) string {
	var s []string
	for _, m := range ms {
		s = append(s, fmt.Sprintf("%s:%d", m.Path, m.Line))
	}
	return strings.Join(s, " ")
}

func TestSearchDefaultsSkipVendoredGeneratedAndBinary(t *testing.T) {
	tr := searchTree(t)
	res, err := tr.Search(context.Background(), SearchOptions{Pattern: `grpc\.NewServer`})
	if err != nil {
		t.Fatal(err)
	}
	// Path order, one entry per matching line, with file and line.
	if got, want := paths(res.Matches), "cmd/main.go:4 pkg/srv/srv.go:2 pkg/srv/srv.go:3 pkg/srv/srv_test.go:2"; got != want {
		t.Fatalf("matches = %s\nwant      %s", got, want)
	}
	if res.TotalMatches != 4 || res.FilesMatched != 3 {
		t.Errorf("totals = %d matches in %d files", res.TotalMatches, res.FilesMatched)
	}
	if res.SkippedDirsN != 2 || res.Generated != 3 {
		t.Errorf("skipped dirs=%d (%v) generated=%d, want 2 and 3", res.SkippedDirsN, res.SkippedDirs, res.Generated)
	}
	if res.Matches[0].Text != "s := grpc.NewServer()" && !strings.Contains(res.Matches[0].Text, "grpc.NewServer()") {
		t.Errorf("match text = %q", res.Matches[0].Text)
	}
}

func TestSearchOptIns(t *testing.T) {
	tr := searchTree(t)
	ctx := context.Background()

	all, err := tr.Search(ctx, SearchOptions{Pattern: "grpc.NewServer", Literal: true, IncludeExcluded: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"vendor/google.golang.org/grpc/server.go:3", "pkg/testdata/fixture.go:1", "pkg/api/zz_generated.deepcopy.go:1", "pkg/api/mock.go:2", "go.sum:1"} {
		if !strings.Contains(paths(all.Matches), want) {
			t.Errorf("include_excluded search missing %s in %s", want, paths(all.Matches))
		}
	}
	if strings.Contains(paths(all.Matches), "bin/tool") {
		t.Error("binary file was searched")
	}

	// A path inside vendor/ opts in by itself.
	v, err := tr.Search(ctx, SearchOptions{Pattern: `^func NewServer`, Path: "vendor/google.golang.org/grpc"})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(v.Matches); got != "vendor/google.golang.org/grpc/server.go:2" {
		t.Errorf("vendor path search = %q", got)
	}

	// Path + glob filters, and a single-file path.
	g, _ := tr.Search(ctx, SearchOptions{Pattern: "NewServer", Path: "pkg", Glob: "*_test.go"})
	if got := paths(g.Matches); got != "pkg/srv/srv_test.go:2" {
		t.Errorf("glob search = %q", got)
	}
	f, _ := tr.Search(ctx, SearchOptions{Pattern: "NewServer", Path: "/cmd/main.go"})
	if got := paths(f.Matches); got != "cmd/main.go:4" {
		t.Errorf("file path search = %q", got)
	}

	// Case-insensitive, and literal metacharacters.
	ci, _ := tr.Search(ctx, SearchOptions{Pattern: "grpc.newserver", Literal: true, IgnoreCase: true, Glob: "*.md"})
	if got := paths(ci.Matches); got != "README.md:1" {
		t.Errorf("ignore-case search = %q", got)
	}
	lit, _ := tr.Search(ctx, SearchOptions{Pattern: "NewServer(opts...)", Literal: true})
	if got := paths(lit.Matches); got != "pkg/srv/srv.go:3" {
		t.Errorf("literal search = %q", got)
	}
}

func TestSearchContextLines(t *testing.T) {
	tr := searchTree(t)
	res, err := tr.Search(context.Background(), SearchOptions{Pattern: "NewServer", Path: "cmd", ContextLines: 99})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Matches[0]
	if len(m.Before) != MaxContextLines || len(m.After) != 2 {
		t.Fatalf("context = %q / %q", m.Before, m.After)
	}
	if m.Before[2] != "func main() {" || m.After[0] != "\t_ = s" || m.After[1] != "}" {
		t.Errorf("context = %q / %q", m.Before, m.After)
	}
}

// The result is bounded however many lines match: a capped number of matches, the
// remaining files named with counts, and totals that still tell the truth.
func TestSearchBounds(t *testing.T) {
	var es []entry
	for i := 0; i < 70; i++ {
		es = append(es, entry{name: fmt.Sprintf("top/f%03d.go", i), body: strings.Repeat("needle here\n", 10)})
	}
	es = append(es, entry{name: "top/long.txt", body: strings.Repeat("a", 5000) + "needle" + strings.Repeat("b", 5000) + "\n"})
	tr := tree(t, es...)
	ctx := context.Background()

	res, err := tr.Search(ctx, SearchOptions{Pattern: "needle", MaxResults: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 25 || res.TotalMatches != 701 || res.FilesMatched != 71 {
		t.Fatalf("shown=%d total=%d files=%d", len(res.Matches), res.TotalMatches, res.FilesMatched)
	}
	if len(res.MoreFiles) != maxMoreFiles || !res.MoreFilesCut {
		t.Errorf("more files = %d cut=%v", len(res.MoreFiles), res.MoreFilesCut)
	}
	if res.MoreFiles[0] != (FileCount{Path: "f002.go", Count: 5}) {
		t.Errorf("first partially-shown file = %+v", res.MoreFiles[0])
	}

	// max_results is clamped, and the total text returned is bounded.
	res, _ = tr.Search(ctx, SearchOptions{Pattern: "needle", MaxResults: 100000})
	if len(res.Matches) != MaxSearchResults {
		t.Errorf("clamped results = %d, want %d", len(res.Matches), MaxSearchResults)
	}

	// A very long line is clipped around the match.
	res, _ = tr.Search(ctx, SearchOptions{Pattern: "needle", Path: "long.txt"})
	if txt := res.Matches[0].Text; len(txt) > maxLineBytes+8 || !strings.Contains(txt, "needle") {
		t.Errorf("long line clipped to %d bytes, contains needle=%v", len(txt), strings.Contains(txt, "needle"))
	}

	// A pattern that matches everything stops counting instead of running on.
	big := tree(t, entry{name: "top/big.txt", body: strings.Repeat("x\n", maxCountedMatches+50)})
	res, _ = big.Search(ctx, SearchOptions{Pattern: "x"})
	if res.Incomplete == "" || len(res.Matches) != DefaultSearchResults {
		t.Errorf("broad pattern: incomplete=%q shown=%d", res.Incomplete, len(res.Matches))
	}
}

func TestSearchErrorsAndCancellation(t *testing.T) {
	tr := searchTree(t)
	ctx := context.Background()
	for _, c := range []struct {
		opt  SearchOptions
		want string
	}{
		{SearchOptions{}, "pattern is required"},
		{SearchOptions{Pattern: "grpc.NewServer("}, "bad pattern"},
		{SearchOptions{Pattern: strings.Repeat("a", maxPatternBytes+1)}, "too long"},
		{SearchOptions{Pattern: "x", Glob: "[bad"}, "bad glob"},
		{SearchOptions{Pattern: "x", Path: "nope"}, "does not exist"},
		// ".." is anchored at the repository root, so it cannot name anything outside.
		{SearchOptions{Pattern: "x", Path: "../../etc"}, "does not exist"},
	} {
		if _, err := tr.Search(ctx, c.opt); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Search(%+v) err = %v, want %q", c.opt, err, c.want)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	res, err := tr.Search(cctx, SearchOptions{Pattern: "NewServer"})
	if err != nil || res.Incomplete == "" {
		t.Errorf("cancelled search: incomplete=%q err=%v", res.Incomplete, err)
	}
}

func TestListAndRead(t *testing.T) {
	tr := searchTree(t)
	es, truncated, err := tr.List("", 1, 0)
	if err != nil || truncated {
		t.Fatal(err, truncated)
	}
	var names []string
	for _, e := range es {
		n := e.Path
		if e.Dir {
			n += "/"
		}
		names = append(names, n)
	}
	if got, want := strings.Join(names, " "), "bin/ cmd/ pkg/ vendor/ README.md go.sum"; got != want {
		t.Errorf("list = %q, want %q", got, want)
	}
	deep, _, _ := tr.List("pkg", 2, 0)
	if len(deep) != 8 {
		t.Errorf("depth-2 listing of pkg has %d entries, want 8", len(deep))
	}
	if _, truncated, _ := tr.List("", 4, 3); !truncated {
		t.Error("listing capped at 3 entries should report truncation")
	}
	if _, _, err := tr.List("cmd/main.go", 1, 0); err == nil {
		t.Error("listing a file should fail")
	}
	if _, _, err := tr.List("../..", 1, 0); err != nil {
		t.Errorf("'..' should clamp to the repository root, got %v", err)
	}

	r, err := tr.Read("cmd/main.go", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.First != 3 || r.TotalLines != 6 || !r.More || strings.Join(r.Lines, "|") != "func main() {|\ts := grpc.NewServer()" {
		t.Errorf("read = %+v", r)
	}
	if r, _ := tr.Read("cmd/main.go", 99, 0); len(r.Lines) != 0 || r.TotalLines != 6 {
		t.Errorf("read past EOF = %+v", r)
	}
	if r, err := tr.Read("bin/tool", 0, 0); err != nil || !r.Binary {
		t.Errorf("binary read = %+v, %v", r, err)
	}
	for _, p := range []string{"nope.go", "cmd", "../../../etc/passwd"} {
		if _, err := tr.Read(p, 0, 0); err == nil {
			t.Errorf("Read(%q) should fail", p)
		}
	}

	// A read is bounded in bytes as well as lines.
	wide := tree(t, entry{name: "top/wide.txt", body: strings.Repeat(strings.Repeat("w", 1000)+"\n", 200)})
	wr, err := wide.Read("wide.txt", 1, MaxReadLines)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Join(wr.Lines, "\n")); n > maxReadBytes+1024 || !wr.More {
		t.Errorf("wide read returned %d bytes, more=%v", n, wr.More)
	}
}

// assertOnly fails unless dir contains exactly the named entries.
func assertOnly(t *testing.T, dir string, want ...string) {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range des {
		got = append(got, d.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s contains %v, want %v", dir, got, want)
	}
}
