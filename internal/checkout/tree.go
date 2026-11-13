package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Bounds on what the read-side operations return. They keep one tool result well
// under the converser's per-result clamp, so a result is never cut mid-line.
const (
	DefaultSearchResults = 60
	MaxSearchResults     = 200
	MaxContextLines      = 3
	maxSearchOutputBytes = 28 << 10 // matched text returned by one search
	maxLineBytes         = 300      // one matched (or context) line
	maxCountedMatches    = 20_000   // stop counting past this; the pattern is too broad
	maxMoreFiles         = 40       // files-with-matches listed beyond those shown
	maxSearchFileBytes   = 2 << 20  // larger files are not searched
	maxPatternBytes      = 1000
	binarySniffBytes     = 8000

	DefaultReadLines = 400
	MaxReadLines     = 1500
	maxReadBytes     = 32 << 10
	maxReadLineBytes = 2000 // one line in a read window (minified files)

	DefaultListEntries = 300
	MaxListEntries     = 1000
	MaxListDepth       = 4
)

// Tree is an unpacked checkout. Every operation resolves paths through an os.Root
// confined to the checkout directory.
type Tree struct {
	Stats
	dir  string
	root *os.Root
}

// Open wraps an unpacked directory. Close removes it.
func Open(dir string, st Stats) (*Tree, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Tree{Stats: st, dir: dir, root: root}, nil
}

// Close releases the tree and deletes its directory.
func (t *Tree) Close() error {
	_ = t.root.Close()
	return os.RemoveAll(t.dir)
}

// cleanPath normalises a caller-supplied (model-supplied, so untrusted) path to a
// repository-relative one. "" and "/" mean the repository root.
func cleanPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.ContainsAny(p, "\x00\\") {
		return "", fmt.Errorf("invalid path")
	}
	p = path.Clean("/" + p)[1:] // anchors at the root, so ".." cannot climb out
	if p == "" {
		return ".", nil
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", fmt.Errorf("path must be inside the repository")
	}
	return p, nil
}

// excludedDirs are directory names whose contents are skipped by a default search:
// third-party or fixture code that swamps results with matches the question is
// usually not about.
var excludedDirs = map[string]bool{
	"vendor": true, "testdata": true, "node_modules": true, "third_party": true,
}

// generatedNames reports whether a file name marks generated or lock content.
func generatedName(name string) bool {
	switch name {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.lock", "generated.pb.go":
		return true
	}
	for _, suf := range []string{".pb.go", ".pb.gw.go", "_generated.go", ".generated.go", ".min.js", ".min.css"} {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return strings.HasPrefix(name, "zz_generated.")
}

// generatedHeader reports the Go convention for generated files
// ("// Code generated ... DO NOT EDIT.") near the top of the file.
func generatedHeader(data []byte) bool {
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	return bytes.Contains(head, []byte("Code generated")) && bytes.Contains(head, []byte("DO NOT EDIT"))
}

// inExcludedDir reports whether rel lies inside (or is) a default-excluded directory.
func inExcludedDir(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if excludedDirs[seg] {
			return true
		}
	}
	return false
}

// SearchOptions select what a search looks for and where.
type SearchOptions struct {
	Pattern    string
	Literal    bool   // match Pattern as a literal string instead of an RE2 regex
	IgnoreCase bool   //
	Path       string // restrict to this directory or file
	Glob       string // file filter: "*.go" (base name) or "pkg/*/doc.go" (full path)
	// IncludeExcluded also searches vendor/, testdata/, node_modules/, third_party/
	// and generated files. A Path inside such a directory opts in by itself.
	IncludeExcluded bool
	MaxResults      int
	ContextLines    int
}

// Match is one matching line.
type Match struct {
	Path   string
	Line   int
	Text   string
	Before []string
	After  []string
}

// FileCount is a file and how many lines in it matched.
type FileCount struct {
	Path  string
	Count int
}

// SearchResult is a bounded search outcome: the first matches in path order, plus
// enough accounting for the caller to say what was NOT shown or not searched.
type SearchResult struct {
	Matches       []Match
	TotalMatches  int         // every matching line found, shown or not
	FilesMatched  int         //
	FilesSearched int         //
	MoreFiles     []FileCount // files with matches that are not (fully) shown
	MoreFilesCut  bool        // MoreFiles itself was truncated
	SkippedDirs   []string    // default-excluded directories that were not entered
	SkippedDirsN  int         //
	Generated     int         // generated/lock files skipped by default
	Incomplete    string      // why the search stopped early, if it did
}

// Search scans the tree for opt.Pattern. Matching is per file with (?m), so ^ and $
// anchor at line boundaries; a match is reported at the line it starts on. Results
// are in lexical path order, so repeated searches agree.
func (t *Tree) Search(ctx context.Context, opt SearchOptions) (SearchResult, error) {
	var res SearchResult
	if opt.Pattern == "" {
		return res, fmt.Errorf("pattern is required")
	}
	if len(opt.Pattern) > maxPatternBytes {
		return res, fmt.Errorf("pattern is too long (max %d bytes)", maxPatternBytes)
	}
	expr := opt.Pattern
	if opt.Literal {
		expr = regexp.QuoteMeta(expr)
	}
	flags := "(?m)"
	if opt.IgnoreCase {
		flags = "(?mi)"
	}
	// RE2: linear time in the input, so an untrusted pattern cannot be made to blow up.
	re, err := regexp.Compile(flags + expr)
	if err != nil {
		return res, fmt.Errorf("bad pattern (RE2 syntax; set literal=true to search for an exact string): %v", err)
	}
	if opt.Glob != "" {
		if _, err := path.Match(opt.Glob, "x"); err != nil {
			return res, fmt.Errorf("bad glob %q", opt.Glob)
		}
	}
	start, err := cleanPath(opt.Path)
	if err != nil {
		return res, err
	}
	if _, err := t.root.Stat(start); err != nil {
		return res, fmt.Errorf("path %q does not exist in this checkout", opt.Path)
	}
	max := opt.MaxResults
	if max <= 0 {
		max = DefaultSearchResults
	}
	if max > MaxSearchResults {
		max = MaxSearchResults
	}
	ctxLines := opt.ContextLines
	if ctxLines < 0 {
		ctxLines = 0
	}
	if ctxLines > MaxContextLines {
		ctxLines = MaxContextLines
	}
	includeAll := opt.IncludeExcluded || inExcludedDir(start)
	outBytes := 0

	walkErr := fs.WalkDir(t.root.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip it, keep searching
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			if !includeAll && p != start && excludedDirs[d.Name()] {
				res.SkippedDirsN++
				if len(res.SkippedDirs) < 6 {
					res.SkippedDirs = append(res.SkippedDirs, p+"/")
				}
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !globMatch(opt.Glob, p) {
			return nil
		}
		if !includeAll && generatedName(d.Name()) {
			res.Generated++
			return nil
		}
		if info, ierr := d.Info(); ierr != nil || info.Size() > maxSearchFileBytes || info.Size() == 0 {
			return nil
		}
		data, rerr := t.readAll(p)
		if rerr != nil || isBinary(data) {
			return nil
		}
		if !includeAll && generatedHeader(data) {
			res.Generated++
			return nil
		}
		res.FilesSearched++
		locs := re.FindAllIndex(data, -1)
		if len(locs) == 0 {
			return nil
		}
		lines := matchLines(data, locs)
		res.FilesMatched++
		res.TotalMatches += len(lines)
		shown := 0
		for _, ln := range lines {
			if len(res.Matches) >= max || outBytes >= maxSearchOutputBytes {
				break
			}
			m := Match{Path: p, Line: ln.num, Text: clipLine(ln.text, ln.col)}
			outBytes += len(m.Path) + len(m.Text) + 12
			if ctxLines > 0 {
				m.Before, m.After = contextLines(data, ln, ctxLines)
				for _, c := range append(append([]string(nil), m.Before...), m.After...) {
					outBytes += len(c) + 8
				}
			}
			res.Matches = append(res.Matches, m)
			shown++
		}
		if shown < len(lines) {
			if len(res.MoreFiles) < maxMoreFiles {
				res.MoreFiles = append(res.MoreFiles, FileCount{Path: p, Count: len(lines) - shown})
			} else {
				res.MoreFilesCut = true
			}
		}
		if res.TotalMatches >= maxCountedMatches {
			res.Incomplete = fmt.Sprintf("stopped after %d matches: the pattern is too broad", res.TotalMatches)
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, context.DeadlineExceeded) || errors.Is(walkErr, context.Canceled) {
			res.Incomplete = "search ran out of time; results cover only part of the tree"
			return res, nil
		}
		return res, walkErr
	}
	return res, nil
}

// globMatch applies the file filter: a glob without "/" matches the base name, one
// with "/" matches the whole repository-relative path.
func globMatch(glob, p string) bool {
	if glob == "" {
		return true
	}
	target := p
	if !strings.Contains(glob, "/") {
		target = path.Base(p)
	}
	ok, _ := path.Match(glob, target)
	return ok
}

func isBinary(data []byte) bool {
	head := data
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	return bytes.IndexByte(head, 0) >= 0
}

type lineRef struct {
	num        int
	start, end int // byte range of the line in the file, without its newline
	col        int // byte offset of the match within the line
	text       string
}

// matchLines maps match offsets to distinct lines, in order.
func matchLines(data []byte, locs [][]int) []lineRef {
	var out []lineRef
	line, pos := 1, 0
	for _, loc := range locs {
		off := loc[0]
		line += bytes.Count(data[pos:off], []byte{'\n'})
		pos = off
		if n := len(out); n > 0 && out[n-1].num == line {
			continue
		}
		s := bytes.LastIndexByte(data[:off], '\n') + 1
		e := len(data)
		if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
			e = off + i
		}
		out = append(out, lineRef{num: line, start: s, end: e, col: off - s,
			text: strings.TrimRight(string(data[s:e]), "\r")})
	}
	return out
}

// contextLines returns up to n lines before and after ln.
func contextLines(data []byte, ln lineRef, n int) (before, after []string) {
	s := ln.start
	for i := 0; i < n && s > 0; i++ {
		e := s - 1
		s = bytes.LastIndexByte(data[:e], '\n') + 1
		before = append([]string{clipLine(strings.TrimRight(string(data[s:e]), "\r"), 0)}, before...)
	}
	e := ln.end
	for i := 0; i < n && e < len(data); i++ {
		s := e + 1
		if s > len(data) {
			break
		}
		e = len(data)
		if j := bytes.IndexByte(data[s:], '\n'); j >= 0 {
			e = s + j
		}
		if s == e && e == len(data) {
			break // the file's trailing newline, not a line
		}
		after = append(after, clipLine(strings.TrimRight(string(data[s:e]), "\r"), 0))
	}
	return before, after
}

// clipLine bounds a line to maxLineBytes, keeping the window around byte col (the
// match) and never splitting a UTF-8 sequence. Invalid UTF-8 is replaced.
func clipLine(s string, col int) string {
	if len(s) > maxLineBytes {
		from := 0
		if col > maxLineBytes/2 {
			from = col - maxLineBytes/3
		}
		to := from + maxLineBytes
		if to > len(s) {
			to = len(s)
		}
		for from > 0 && from < len(s) && !utf8.RuneStart(s[from]) {
			from++
		}
		for to < len(s) && !utf8.RuneStart(s[to]) {
			to--
		}
		clipped := s[from:to]
		if from > 0 {
			clipped = "…" + clipped
		}
		if to < len(s) {
			clipped += "…"
		}
		s = clipped
	}
	return strings.ToValidUTF8(s, "�")
}

// readAll reads a whole file through the root. Callers have already bounded its size.
func (t *Tree) readAll(p string) ([]byte, error) {
	f, err := t.root.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, DefaultLimits.MaxFileBytes+1))
}

// Entry is one directory entry in a listing.
type Entry struct {
	Path  string // repository-relative
	Dir   bool
	Size  int64
	Depth int
}

// List returns the entries under dir, depth levels deep (1 = the directory's own
// children), directories first then files, each level in name order. It returns at
// most max entries and reports whether more exist.
func (t *Tree) List(dir string, depth, max int) (entries []Entry, truncated bool, err error) {
	start, err := cleanPath(dir)
	if err != nil {
		return nil, false, err
	}
	info, err := t.root.Stat(start)
	if err != nil {
		return nil, false, fmt.Errorf("path %q does not exist in this checkout", dir)
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("path %q is a file, not a directory", dir)
	}
	if depth < 1 {
		depth = 1
	}
	if depth > MaxListDepth {
		depth = MaxListDepth
	}
	if max <= 0 {
		max = DefaultListEntries
	}
	if max > MaxListEntries {
		max = MaxListEntries
	}
	var walk func(d string, level int) error
	walk = func(d string, level int) error {
		des, err := fs.ReadDir(t.root.FS(), d)
		if err != nil {
			return nil
		}
		sort.SliceStable(des, func(i, j int) bool { return des[i].IsDir() && !des[j].IsDir() })
		for _, de := range des {
			if len(entries) >= max {
				truncated = true
				return fs.SkipAll
			}
			p := path.Join(d, de.Name())
			e := Entry{Path: p, Dir: de.IsDir(), Depth: level}
			if !e.Dir {
				if fi, ierr := de.Info(); ierr == nil {
					e.Size = fi.Size()
				}
			}
			entries = append(entries, e)
			if e.Dir && level < depth {
				if err := walk(p, level+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	_ = walk(start, 1)
	return entries, truncated, nil
}

// ReadResult is a line window of one file.
type ReadResult struct {
	Lines      []string // the window, without line terminators
	First      int      // 1-based number of Lines[0]
	TotalLines int
	Size       int64
	Binary     bool
	More       bool // lines remain after the window
}

// Read returns up to maxLines lines of a file starting at startLine (1-based),
// bounded to maxReadBytes of text so one read fits a tool result.
func (t *Tree) Read(p string, startLine, maxLines int) (ReadResult, error) {
	var res ReadResult
	rel, err := cleanPath(p)
	if err != nil {
		return res, err
	}
	if size, ok := t.Oversize[rel]; ok {
		return res, fmt.Errorf("%q is %s and was left out of the checkout (files over %s are skipped)", rel, human(size), human(DefaultLimits.MaxFileBytes))
	}
	info, err := t.root.Stat(rel)
	if err != nil {
		return res, fmt.Errorf("path %q does not exist in this checkout", p)
	}
	if info.IsDir() {
		return res, fmt.Errorf("path %q is a directory; list it instead", p)
	}
	data, err := t.readAll(rel)
	if err != nil {
		return res, fmt.Errorf("read %q: %v", rel, err)
	}
	res.Size = int64(len(data))
	if isBinary(data) {
		res.Binary = true
		return res, nil
	}
	if startLine < 1 {
		startLine = 1
	}
	if maxLines <= 0 {
		maxLines = DefaultReadLines
	}
	if maxLines > MaxReadLines {
		maxLines = MaxReadLines
	}
	all := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	res.TotalLines = len(all)
	res.First = startLine
	if startLine > len(all) {
		return res, nil
	}
	used := 0
	for i := startLine - 1; i < len(all) && len(res.Lines) < maxLines; i++ {
		line := strings.ToValidUTF8(strings.TrimRight(all[i], "\r"), "�")
		if len(line) > maxReadLineBytes {
			cut := maxReadLineBytes
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut] + "… (line truncated)"
		}
		if used+len(line) > maxReadBytes && len(res.Lines) > 0 {
			break
		}
		used += len(line) + 8
		res.Lines = append(res.Lines, line)
	}
	res.More = startLine-1+len(res.Lines) < len(all)
	return res, nil
}
