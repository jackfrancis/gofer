package prow

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds on what search and read return, so one tool result stays well under the
// converser's per-result clamp and is never cut mid-line.
const (
	DefaultSearchResults = 60
	MaxSearchResults     = 200
	MaxContextLines      = 5
	maxSearchOutputBytes = 28 << 10
	maxMatchLineBytes    = 300
	maxCountedMatches    = 20_000
	maxPatternBytes      = 1000
	maxMoreFiles         = 30
	// maxSearchFiles bounds the files one directory search fetches.
	maxSearchFiles = 40

	DefaultReadLines = 400
	MaxReadLines     = 1500
	maxReadBytes     = 32 << 10
	maxReadLineBytes = 2000
)

// skipExts are artifact types never fetched for a search: archives, images and other
// binaries that hold no searchable text.
var skipExts = map[string]bool{
	".gz": true, ".tgz": true, ".tar": true, ".zip": true, ".bz2": true, ".xz": true, ".zst": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true, ".webp": true,
	".bin": true, ".pb": true, ".so": true, ".exe": true, ".pprof": true, ".prof": true,
}

// SearchOptions select what a search looks for and where.
type SearchOptions struct {
	Pattern      string
	Literal      bool
	IgnoreCase   bool
	Path         string // a file, or a directory searched recursively ("" = the whole job)
	Glob         string // file filter: "*.log" (base name) or "artifacts/*/logs/*" (whole path)
	MaxResults   int
	ContextLines int
	// Timeout bounds the whole search (0 = only the run's deadline). Pass it here
	// rather than narrowing ctx, so the session's time reserve is still measured
	// from the run's deadline.
	Timeout time.Duration
}

// Match is one matching line.
type Match struct {
	Path   string
	Line   int
	Text   string
	Before []string
	After  []string
}

// FileCount is a file and how many of its matches were not shown.
type FileCount struct {
	Path  string
	Count int
}

// SearchResult is a bounded search outcome with enough accounting to say what was
// not shown and what was not searched.
type SearchResult struct {
	Matches       []Match
	TotalMatches  int
	FilesMatched  int
	FilesSearched int
	MoreFiles     []FileCount
	// Not searched, by reason (counts).
	SkippedBinary  int // binary content or a binary file type
	SkippedLarge   int // over the per-file limit
	SkippedLimit   int // beyond the files one search may fetch, or the run's budget
	SkippedFailed  int // the fetch failed
	ListIncomplete bool
	Incomplete     string
}

// Search searches one file, or every file under a directory, in job. A path that is
// not a file is tried as a directory.
func (s *Session) Search(ctx context.Context, job Job, opt SearchOptions) (SearchResult, error) {
	var res SearchResult
	if opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = withBudget(ctx, opt.Timeout)
		defer cancel()
	}
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
	if opt.IgnoreCase {
		expr = "(?i)" + expr
	}
	// RE2: linear time, so an untrusted pattern cannot stall the run.
	re, err := regexp.Compile(expr)
	if err != nil {
		return res, fmt.Errorf("bad pattern (RE2 syntax; set literal=true to search for an exact string): %v", err)
	}
	if opt.Glob != "" {
		if _, err := path.Match(opt.Glob, "x"); err != nil {
			return res, fmt.Errorf("bad glob %q", opt.Glob)
		}
	}
	rel, err := CleanRel(opt.Path)
	if err != nil {
		return res, err
	}
	max := opt.MaxResults
	if max <= 0 {
		max = DefaultSearchResults
	}
	max = min(max, MaxSearchResults)
	ctxLines := min(max0(opt.ContextLines), MaxContextLines)
	sr := &searcher{re: re, max: max, ctxLines: ctxLines, res: &res}

	// A single file: fetch it directly.
	if rel != "" && !strings.HasSuffix(opt.Path, "/") {
		f, ferr := s.Fetch(ctx, job, rel)
		switch {
		case ferr == nil:
			if f.Binary {
				return res, fmt.Errorf("%s is binary content, not searchable", rel)
			}
			return res, s.searchFile(ctx, f, sr)
		case !errors.Is(ferr, ErrNotFound):
			return res, ferr
		}
		// Not a file: fall through and treat it as a directory.
	}

	entries, incomplete, err := s.List(ctx, job, rel, true)
	if err != nil {
		return res, err
	}
	res.ListIncomplete = incomplete
	var files []Entry
	for _, e := range entries {
		if !e.Dir && globMatch(opt.Glob, e.Rel) {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		if rel != "" && len(entries) == 0 {
			return res, notFoundError(rel)
		}
		return res, nil
	}
	fetchedHere := 0
	for _, e := range files {
		if ctx.Err() != nil {
			res.Incomplete = "search ran out of time; results cover only some files"
			break
		}
		switch {
		case skipExts[strings.ToLower(path.Ext(e.Rel))]:
			res.SkippedBinary++
			continue
		case e.Size > s.lim.MaxObjectBytes:
			res.SkippedLarge++
			continue
		case e.Size == 0:
			continue
		case fetchedHere >= maxSearchFiles:
			res.SkippedLimit++
			continue
		}
		fetchedHere++
		f, ferr := s.Fetch(ctx, job, e.Rel)
		if ferr != nil {
			if errors.Is(ferr, ErrTooLarge) || errors.Is(ferr, ErrLimit) {
				res.SkippedLimit++
			} else {
				res.SkippedFailed++
			}
			continue
		}
		if f.Binary {
			res.SkippedBinary++
			continue
		}
		if err := s.searchFile(ctx, f, sr); err != nil {
			if ctx.Err() != nil {
				res.Incomplete = "search ran out of time; results cover only some files"
				break
			}
			res.SkippedFailed++
		}
		if res.Incomplete != "" {
			break
		}
	}
	return res, nil
}

// lineCost is what one shown line (a match or a context line) costs in the formatted
// result, which prints every line with its file path and line number:
// "<path>:<line>: <text>" or "<path>-<line>- <text>".
func lineCost(path string, textLen int) int { return len(path) + textLen + 16 }

type searcher struct {
	re       *regexp.Regexp
	max      int
	ctxLines int
	res      *SearchResult
	outBytes int
}

func (s *Session) searchFile(ctx context.Context, f *File, sr *searcher) error {
	fh, err := s.open(f)
	if err != nil {
		return err
	}
	defer fh.Close()
	res := sr.res
	res.FilesSearched++
	lr := newLineReader(fh)
	var (
		before  []string // ring of the previous ctxLines lines
		pending []int    // indexes into res.Matches still collecting After lines
		matched int
		shown   int
	)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if lr.num%4096 == 0 && ctx.Err() != nil {
			res.Incomplete = "search ran out of time; results cover only part of the file"
			break
		}
		text := string(line)
		if sr.ctxLines > 0 && len(pending) > 0 {
			kept := pending[:0]
			for _, i := range pending {
				m := &res.Matches[i]
				m.After = append(m.After, clipLine(text, 0, maxMatchLineBytes))
				sr.outBytes += lineCost(f.Rel, min(len(text), maxMatchLineBytes))
				if len(m.After) < sr.ctxLines {
					kept = append(kept, i)
				}
			}
			pending = kept
		}
		if loc := sr.re.FindStringIndex(text); loc != nil {
			matched++
			res.TotalMatches++
			if len(res.Matches) < sr.max && sr.outBytes < maxSearchOutputBytes {
				m := Match{Path: f.Rel, Line: lr.num, Text: clipLine(text, loc[0], maxMatchLineBytes)}
				sr.outBytes += lineCost(m.Path, len(m.Text))
				if sr.ctxLines > 0 {
					m.Before = append([]string(nil), before...)
					for _, b := range before {
						sr.outBytes += lineCost(m.Path, len(b))
					}
				}
				res.Matches = append(res.Matches, m)
				shown++
				if sr.ctxLines > 0 {
					pending = append(pending, len(res.Matches)-1)
				}
			}
			if res.TotalMatches >= maxCountedMatches {
				res.Incomplete = fmt.Sprintf("stopped after %d matches: the pattern is too broad", res.TotalMatches)
				break
			}
		}
		if sr.ctxLines > 0 {
			before = append(before, clipLine(text, 0, maxMatchLineBytes))
			if len(before) > sr.ctxLines {
				before = before[1:]
			}
		}
	}
	if lr.err != nil {
		return lr.err
	}
	if matched > 0 {
		res.FilesMatched++
	}
	if matched > shown {
		if len(res.MoreFiles) < maxMoreFiles {
			res.MoreFiles = append(res.MoreFiles, FileCount{Path: f.Rel, Count: matched - shown})
		}
	}
	return nil
}

// ReadResult is a line window of one file.
type ReadResult struct {
	Path       string
	Lines      []string
	First      int // 1-based number of Lines[0]
	TotalLines int
	Size       int64
	Binary     bool
	More       bool
}

// Read returns up to maxLines lines of rel starting at startLine (1-based). A
// negative startLine counts from the end: -200 reads the last 200 lines.
func (s *Session) Read(ctx context.Context, job Job, rel string, startLine, maxLines int) (ReadResult, error) {
	r, err := CleanRel(rel)
	if err != nil {
		return ReadResult{}, err
	}
	if r == "" {
		return ReadResult{}, fmt.Errorf("path is required")
	}
	f, err := s.Fetch(ctx, job, r)
	if err != nil {
		return ReadResult{}, err
	}
	res := ReadResult{Path: f.Rel, Size: f.Size, Binary: f.Binary}
	if f.Binary {
		return res, nil
	}
	if maxLines <= 0 {
		maxLines = DefaultReadLines
	}
	maxLines = min(maxLines, MaxReadLines)

	// One pass to count lines, so a negative start can be resolved and the result can
	// say how many lines the file has.
	total, err := s.countLines(f)
	if err != nil {
		return res, err
	}
	res.TotalLines = total
	if startLine < 0 {
		startLine = max(total+startLine+1, 1)
	}
	if startLine < 1 {
		startLine = 1
	}
	res.First = startLine
	if startLine > total {
		return res, nil
	}
	fh, err := s.open(f)
	if err != nil {
		return res, err
	}
	defer fh.Close()
	lr := newLineReader(fh)
	used := 0
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if lr.num < startLine {
			continue
		}
		if len(res.Lines) >= maxLines {
			break
		}
		text := strings.ToValidUTF8(string(line), "�")
		if len(text) > maxReadLineBytes {
			cut := maxReadLineBytes
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut] + "… (line truncated)"
		}
		if used+len(text) > maxReadBytes && len(res.Lines) > 0 {
			break
		}
		used += len(text) + 8
		res.Lines = append(res.Lines, text)
	}
	res.More = startLine-1+len(res.Lines) < total
	return res, lr.err
}

func (s *Session) countLines(f *File) (int, error) {
	fh, err := s.open(f)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	lr := newLineReader(fh)
	for {
		if _, ok := lr.next(); !ok {
			break
		}
	}
	return lr.num, lr.err
}

// globMatch applies the file filter: a glob without "/" matches the base name, one
// with "/" matches the whole job-relative path.
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

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
