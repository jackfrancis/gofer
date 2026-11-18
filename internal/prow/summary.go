package prow

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Bounds on a job summary.
const (
	maxJUnitFiles       = 5
	maxFailedTests      = 20
	maxFailureMsgBytes  = 1 << 10
	maxMarkers          = 8
	maxMarkerLines      = 25
	maxMarkerBytes      = 16 << 10 // log text across all markers, so a summary fits one tool result
	maxSummaryJSONBytes = 1 << 20
)

// Summary is what ci_job_summary reports: the job's outcome, its failing tests, where
// the build log shows failure, and the shape of its artifacts. Every string field
// comes from the job's own files and is untrusted.
type Summary struct {
	Job Job

	// finished.json / started.json
	Result   string
	Passed   *bool
	Revision string
	Started  time.Time
	Finished time.Time

	// prowjob.json
	JobName string
	JobType string
	State   string
	Refs    string // "org/repo base@sha, PR #n by author @sha"

	Missing []string // well-known files that were not found or could not be read

	JUnit JUnitSummary

	LogLines   int
	LogMarkers []Marker
	LogNote    string // why the log could not be scanned, if it could not

	JobFiles            []Entry    // files at the job root
	Artifacts           []DirCount // top level of artifacts/
	ArtifactsIncomplete bool
}

// JUnitSummary aggregates the top-level artifacts/junit*.xml files.
type JUnitSummary struct {
	Files    []string
	Tests    int
	Failed   int
	Skipped  int
	Failures []TestFailure // first maxFailedTests
	Note     string
}

// TestFailure is one failing test case.
type TestFailure struct {
	File    string
	Name    string
	Message string
}

// Marker is a place in the build log that shows failure.
type Marker struct {
	Kind  string
	Line  int // 1-based line of Lines[0]
	Lines []string
}

// DirCount is a top-level entry of artifacts/: a directory with its object count and
// size, or a file with its size.
type DirCount struct {
	Name    string
	Dir     bool
	Objects int
	Bytes   int64
}

// Summarize builds the summary of job. Missing optional files are reported, not
// errors; an error means nothing at all could be read.
func (s *Session) Summarize(ctx context.Context, job Job) (Summary, error) {
	sum := Summary{Job: job}

	root, _, rerr := s.List(ctx, job, "", false)
	if rerr == nil {
		for _, e := range root {
			if !e.Dir {
				sum.JobFiles = append(sum.JobFiles, e)
			}
		}
		if len(root) == 0 {
			return sum, fmt.Errorf("the job directory is empty or does not exist: %s", job.URL())
		}
	}

	var finished struct {
		Timestamp int64  `json:"timestamp"`
		Passed    *bool  `json:"passed"`
		Result    string `json:"result"`
		Revision  string `json:"revision"`
	}
	if s.readJSON(ctx, job, "finished.json", &finished, &sum) {
		sum.Result, sum.Passed, sum.Revision = finished.Result, finished.Passed, finished.Revision
		if finished.Timestamp > 0 {
			sum.Finished = time.Unix(finished.Timestamp, 0).UTC()
		}
	}
	var started struct {
		Timestamp int64 `json:"timestamp"`
	}
	if s.readJSON(ctx, job, "started.json", &started, &sum) && started.Timestamp > 0 {
		sum.Started = time.Unix(started.Timestamp, 0).UTC()
	}
	var pj struct {
		Spec struct {
			Type string `json:"type"`
			Job  string `json:"job"`
			Refs *struct {
				Org     string `json:"org"`
				Repo    string `json:"repo"`
				BaseRef string `json:"base_ref"`
				BaseSHA string `json:"base_sha"`
				Pulls   []struct {
					Number int    `json:"number"`
					Author string `json:"author"`
					SHA    string `json:"sha"`
				} `json:"pulls"`
			} `json:"refs"`
		} `json:"spec"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if s.readJSON(ctx, job, "prowjob.json", &pj, &sum) {
		sum.JobName, sum.JobType, sum.State = pj.Spec.Job, pj.Spec.Type, pj.Status.State
		if r := pj.Spec.Refs; r != nil {
			sum.Refs = fmt.Sprintf("%s/%s %s@%s", r.Org, r.Repo, r.BaseRef, short(r.BaseSHA))
			for _, p := range r.Pulls {
				sum.Refs += fmt.Sprintf(", PR #%d by %s @%s", p.Number, p.Author, short(p.SHA))
			}
		}
	}
	if sum.JobName == "" {
		sum.JobName = job.Name()
	}

	s.summarizeJUnit(ctx, job, &sum)
	s.scanBuildLog(ctx, job, &sum)

	arts, inc, aerr := s.List(ctx, job, "artifacts", true)
	if aerr == nil {
		sum.ArtifactsIncomplete = inc
		byName := map[string]*DirCount{}
		var order []string
		for _, e := range arts {
			r := strings.TrimPrefix(e.Rel, "artifacts/")
			top, rest, isDir := strings.Cut(r, "/")
			d, ok := byName[top]
			if !ok {
				d = &DirCount{Name: top, Dir: isDir && rest != ""}
				byName[top] = d
				order = append(order, top)
			}
			d.Objects++
			d.Bytes += e.Size
		}
		sort.Strings(order)
		for _, n := range order {
			sum.Artifacts = append(sum.Artifacts, *byName[n])
		}
	}
	if rerr != nil && sum.Result == "" && sum.JobType == "" && sum.LogLines == 0 {
		return sum, rerr
	}
	return sum, nil
}

// readJSON fetches and decodes a small JSON file, recording it as missing on failure.
func (s *Session) readJSON(ctx context.Context, job Job, rel string, v any, sum *Summary) bool {
	f, err := s.Fetch(ctx, job, rel)
	if err != nil || f.Binary || f.Size > maxSummaryJSONBytes {
		sum.Missing = append(sum.Missing, rel)
		return false
	}
	fh, err := s.open(f)
	if err != nil {
		sum.Missing = append(sum.Missing, rel)
		return false
	}
	defer fh.Close()
	if err := json.NewDecoder(io.LimitReader(fh, maxSummaryJSONBytes)).Decode(v); err != nil {
		sum.Missing = append(sum.Missing, rel+" (unreadable)")
		return false
	}
	return true
}

func (s *Session) summarizeJUnit(ctx context.Context, job Job, sum *Summary) {
	entries, _, err := s.List(ctx, job, "artifacts", false)
	if err != nil {
		sum.JUnit.Note = "artifacts/ could not be listed"
		return
	}
	var files []string
	for _, e := range entries {
		base := path.Base(e.Rel)
		if !e.Dir && strings.HasPrefix(base, "junit") && strings.HasSuffix(base, ".xml") {
			files = append(files, e.Rel)
		}
	}
	if len(files) == 0 {
		sum.JUnit.Note = "no artifacts/junit*.xml files"
		return
	}
	if len(files) > maxJUnitFiles {
		sum.JUnit.Note = fmt.Sprintf("%d JUnit files; only the first %d were read", len(files), maxJUnitFiles)
		files = files[:maxJUnitFiles]
	}
	seen := map[string]bool{}
	for _, rel := range files {
		f, err := s.Fetch(ctx, job, rel)
		if err != nil || f.Binary {
			sum.Missing = append(sum.Missing, rel)
			continue
		}
		fh, err := s.open(f)
		if err != nil {
			continue
		}
		perr := parseJUnit(fh, path.Base(rel), &sum.JUnit, seen)
		fh.Close()
		sum.JUnit.Files = append(sum.JUnit.Files, rel)
		if perr != nil {
			sum.Missing = append(sum.Missing, rel+" (partly unreadable)")
		}
	}
}

type junitCase struct {
	Name      string    `xml:"name,attr"`
	Classname string    `xml:"classname,attr"`
	Failure   *junitMsg `xml:"failure"`
	Error     *junitMsg `xml:"error"`
	Skipped   *struct{} `xml:"skipped"`
}

type junitMsg struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

// parseJUnit streams testcase elements out of a JUnit file, counting and collecting
// failures. Duplicate failing names (a retried case) are listed once.
func parseJUnit(r io.Reader, file string, js *JUnitSummary, seen map[string]bool) error {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "testcase" {
			continue
		}
		var tc junitCase
		if err := dec.DecodeElement(&tc, &se); err != nil {
			return err
		}
		js.Tests++
		msg := tc.Failure
		if msg == nil {
			msg = tc.Error
		}
		switch {
		case msg != nil:
			js.Failed++
			name := strings.TrimSpace(tc.Name)
			if tc.Classname != "" && !strings.Contains(name, tc.Classname) {
				name = tc.Classname + " " + name
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			if len(js.Failures) < maxFailedTests {
				// Ginkgo repeats the message attribute inside the body ("[FAILED] …"
				// followed by the location): keep the body alone when it already holds the
				// message, so the excerpt is not half duplicate.
				text := strings.TrimSpace(msg.Message)
				if body := strings.TrimSpace(msg.Body); body != "" && body != text {
					switch {
					case text == "" || strings.Contains(body, text):
						text = body
					default:
						text += "\n" + body
					}
				}
				js.Failures = append(js.Failures, TestFailure{File: file, Name: name,
					Message: clipLine(stripControls(text), 0, maxFailureMsgBytes)})
			}
		case tc.Skipped != nil:
			js.Skipped++
		}
	}
}

// stripControls removes ANSI sequences from text decoded out of XML (normalize has
// already run on the file, but an escaped ESC survives it as an entity).
func stripControls(s string) string {
	var b strings.Builder
	_, _ = normalize(context.Background(), &b, strings.NewReader(s))
	return b.String()
}

// LogPattern is a kind of log line that shows where and how a job failed. Before and
// After are how many neighbouring lines belong to its marker; Limit caps how many
// occurrences are kept (the first ones); Last keeps only the final occurrence.
type LogPattern struct {
	Kind          string
	Re            *regexp.Regexp
	Before, After int
	Limit         int
	Last          bool
}

// DefaultLogPatterns are the failure lines of the tools Kubernetes CI runs. They
// expect lines without a timestamp prefix (several are anchored at ^).
var DefaultLogPatterns = []LogPattern{
	{Kind: "ginkgo failure summary", Re: regexp.MustCompile(`Summarizing \d+ Failures?`), After: 20, Limit: 2},
	{Kind: "ginkgo result", Re: regexp.MustCompile(`^\s*Ran \d+ of \d+ Specs`), After: 2, Limit: 2, Last: true},
	{Kind: "go test failure", Re: regexp.MustCompile(`^\s*--- FAIL: `), After: 6, Limit: 6},
	{Kind: "go test package failure", Re: regexp.MustCompile(`^FAIL\s+\S+`), Limit: 6},
	{Kind: "panic", Re: regexp.MustCompile(`^panic: `), After: 12, Limit: 2},
	{Kind: "timeout", Re: regexp.MustCompile(`Process did not finish before|Entrypoint received interrupt|context deadline exceeded.*timed out|test timed out after`), After: 3, Limit: 3},
	{Kind: "make error", Re: regexp.MustCompile(`^make(\[\d+\])?: \*\*\* .*Error \d+`), Limit: 2, Last: true},
}

// LogScan is where a log shows failure.
type LogScan struct {
	Lines   int
	Markers []Marker
	Note    string // the scan is partial or its markers were cut short, if so
}

// scanBuildLog finds failure markers anywhere in build-log.txt, not only its tail:
// the useful block is often followed by hundreds of lines of clean-up output.
func (s *Session) scanBuildLog(ctx context.Context, job Job, sum *Summary) {
	f, err := s.Fetch(ctx, job, "build-log.txt")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			sum.LogNote = "no build-log.txt in this job"
		} else {
			sum.LogNote = "build-log.txt could not be read: " + err.Error()
		}
		return
	}
	if f.Binary {
		sum.LogNote = "build-log.txt is binary content"
		return
	}
	scan, err := s.ScanLog(ctx, f, DefaultLogPatterns)
	if err != nil {
		sum.LogNote = "build-log.txt could not be read"
		return
	}
	sum.LogLines, sum.LogMarkers, sum.LogNote = scan.Lines, scan.Markers, scan.Note
}

// ScanLog finds the lines of an already-fetched text file that match patterns, with
// their neighbouring lines, merged into marker blocks. Each line matches at most
// one pattern (the first in order). At most maxMarkers blocks are kept, the latest
// ones — the end of a log is where it failed — and their text is bounded so a
// summary fits one tool result.
func (s *Session) ScanLog(ctx context.Context, f *File, patterns []LogPattern) (LogScan, error) {
	var scan LogScan
	if f.Binary {
		return scan, fmt.Errorf("%s is binary content", f.Rel)
	}
	fh, err := s.open(f)
	if err != nil {
		return scan, err
	}
	defer fh.Close()

	type found struct {
		kind          string
		line          int
		before, after int
	}
	counts := make([]int, len(patterns))
	var hits []found
	var lastOf = map[int]int{} // pattern index → index in hits of its latest occurrence
	lr := newLineReader(fh)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if lr.num%4096 == 0 && ctx.Err() != nil {
			scan.Note = "the scan ran out of time; markers cover only part of the log"
			break
		}
		for i, p := range patterns {
			if !p.Re.Match(line) {
				continue
			}
			h := found{p.Kind, lr.num, p.Before, p.After}
			if p.Last {
				if j, ok := lastOf[i]; ok {
					hits[j] = h
					break
				}
				lastOf[i] = len(hits)
				hits = append(hits, h)
				break
			}
			if counts[i] < p.Limit {
				counts[i]++
				hits = append(hits, h)
			}
			break
		}
	}
	scan.Lines = lr.num
	if lr.err != nil {
		return scan, lr.err
	}
	if len(hits) == 0 {
		return scan, nil
	}
	// Each hit's window, ordered by where it starts (a Before window can start ahead
	// of an earlier hit), then merged where they overlap or touch, so a ginkgo summary
	// and the "Ran N of M" line right after it come back as one block.
	type window struct {
		kinds    []string
		from, to int
		at       int // the hit's own line (spans only)
	}
	spans := make([]window, len(hits))
	for i, h := range hits {
		spans[i] = window{kinds: []string{h.kind}, at: h.line,
			from: max(h.line-min(h.before, maxMarkerLines-1), 1),
			to:   h.line + min(h.after, maxMarkerLines-1)}
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
	// A block shows at most maxMarkerLines lines, from its start; a merge that would
	// run longer continues in an adjacent block instead, so no hit's line is cut off.
	var wins []window
	for _, sp := range spans {
		if n := len(wins); n > 0 && sp.from <= wins[n-1].to+1 {
			w := &wins[n-1]
			switch {
			case sp.to <= w.to: // inside the block already
			case sp.to-w.from < maxMarkerLines:
				w.to = sp.to
			default:
				next := window{from: w.to + 1, to: sp.to}
				if sp.at > w.to {
					next.kinds = sp.kinds
				}
				wins = append(wins, next)
				if sp.at > w.to {
					continue
				}
				w = &wins[n-1]
			}
			if !contains(w.kinds, sp.kinds[0]) {
				w.kinds = append(w.kinds, sp.kinds[0])
			}
			continue
		}
		wins = append(wins, sp)
	}
	if len(wins) > maxMarkers {
		wins = wins[len(wins)-maxMarkers:] // the end of a log is where it failed
		scan.Note = "more failure markers than shown; the earliest were dropped"
	}
	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		return scan, err
	}
	lr = newLineReader(fh)
	wi, used := 0, 0
	var cur *Marker
	for wi < len(wins) {
		line, ok := lr.next()
		if !ok {
			break
		}
		w := wins[wi]
		if lr.num < w.from {
			continue
		}
		if cur == nil {
			scan.Markers = append(scan.Markers, Marker{Kind: strings.Join(w.kinds, ", "), Line: lr.num})
			cur = &scan.Markers[len(scan.Markers)-1]
		}
		if len(cur.Lines) < maxMarkerLines && used < maxMarkerBytes {
			l := clipLine(string(line), 0, maxMatchLineBytes)
			used += len(l) + 24
			cur.Lines = append(cur.Lines, l)
		} else if used >= maxMarkerBytes && scan.Note == "" {
			scan.Note = "failure markers were cut short to fit; read around them"
		}
		if lr.num >= w.to {
			cur = nil
			wi++
		}
	}
	return scan, lr.err
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
