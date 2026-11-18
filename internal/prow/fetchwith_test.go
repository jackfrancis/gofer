package prow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
)

// staticFetch serves body as a non-Prow log and counts calls.
func staticFetch(body string, calls *int) FetchFunc {
	return func(_ context.Context, w io.Writer, max int64) (int64, error) {
		*calls++
		if int64(len(body)) > max {
			return 0, fmt.Errorf("%w: over the limit", ErrTooLarge)
		}
		n, err := io.WriteString(w, body)
		return int64(n), err
	}
}

func TestFetchWithSearchReadScan(t *testing.T) {
	s := NewSession(NewClient("http://127.0.0.1:1"), Limits{})
	t.Setenv("TMPDIR", t.TempDir())
	defer s.Close()
	ctx := context.Background()

	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("T%02d \x1b[31mstep output %d\x1b[0m", i, i))
	}
	lines[39] = "T40 ##[error]Process completed with exit code 1."
	body := strings.Join(lines, "\r\n") + "\n"

	calls := 0
	var seen []int
	opt := FetchOptions{LineHead: func(num int, head []byte) int {
		seen = append(seen, num)
		if len(head) >= 4 && head[0] == 'T' {
			return 4
		}
		return 0
	}}
	f, err := s.FetchWith(ctx, "test:job/1", "log", staticFetch(body, &calls), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 50 || seen[49] != 50 {
		t.Fatalf("LineHead saw %d lines", len(seen))
	}
	// A second fetch is the cached file: no download, no LineHead.
	f2, err := s.FetchWith(ctx, "test:job/1", "log", staticFetch(body, &calls), opt)
	if err != nil || f2 != f || calls != 1 || len(seen) != 50 {
		t.Fatalf("second fetch: same=%v calls=%d heads=%d err=%v", f2 == f, calls, len(seen), err)
	}

	res, err := s.SearchFile(ctx, f, SearchOptions{Pattern: "output 2\\d$", ContextLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 10 || res.Matches[0].Line != 20 || res.Matches[0].Text != "step output 20" {
		t.Fatalf("search = %+v", res)
	}
	if res.Matches[0].Before[0] != "step output 19" || res.Matches[0].Path != "log" {
		t.Errorf("context/path = %+v", res.Matches[0])
	}

	rd, err := s.ReadFile(f, -5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rd.First != 46 || len(rd.Lines) != 5 || rd.Lines[4] != "step output 50" || rd.TotalLines != 50 {
		t.Fatalf("read = %+v", rd)
	}

	pats := []LogPattern{{Kind: "actions error", Re: regexp.MustCompile(`^##\[error\]`), Before: 3, After: 1, Limit: 1}}
	scan, err := s.ScanLog(ctx, f, pats)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Lines != 50 || len(scan.Markers) != 1 {
		t.Fatalf("scan = %+v", scan)
	}
	m := scan.Markers[0]
	if m.Line != 37 || len(m.Lines) != 5 || m.Lines[3] != "##[error]Process completed with exit code 1." || m.Lines[4] != "step output 41" {
		t.Fatalf("marker = %+v", m)
	}
}

func TestFetchWithBudgets(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	calls := 0

	s := NewSession(NewClient("http://127.0.0.1:1"), Limits{MaxObjectBytes: 10})
	defer s.Close()
	if _, err := s.FetchWith(ctx, "test:big", "log", staticFetch(strings.Repeat("x", 11), &calls), FetchOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the object limit: err = %v", err)
	}
	// The failure is remembered.
	if _, err := s.FetchWith(ctx, "test:big", "log", staticFetch("ok", &calls), FetchOptions{}); !errors.Is(err, ErrTooLarge) || calls != 1 {
		t.Errorf("remembered failure: err = %v, calls = %d", err, calls)
	}

	s2 := NewSession(NewClient("http://127.0.0.1:1"), Limits{MaxObjects: 1})
	defer s2.Close()
	if _, err := s2.FetchWith(ctx, "test:a", "log", staticFetch("a", &calls), FetchOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.FetchWith(ctx, "test:b", "log", staticFetch("b", &calls), FetchOptions{}); !errors.Is(err, ErrLimit) {
		t.Errorf("objects per run: err = %v", err)
	}

	if _, err := s2.FetchWith(ctx, "bucket/object", "log", staticFetch("b", &calls), FetchOptions{}); err == nil {
		t.Error("an unqualified key could collide with a bucket object")
	}
}

// A Before window that would merge into an earlier block past maxMarkerLines goes in
// an adjacent block, so the hit's own line is never cut off.
func TestScanLogLongMergeKeepsHitLine(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := NewSession(NewClient("http://127.0.0.1:1"), Limits{})
	defer s.Close()
	var lines []string
	for i := 1; i <= 60; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines[4] = "Summarizing 1 Failure:" // line 5, After 20 → 25
	lines[34] = "##[error]exit 1"       // line 35, Before 15 → from 20; merged 5-37 is too long
	calls := 0
	f, err := s.FetchWith(context.Background(), "test:merge", "log", staticFetch(strings.Join(lines, "\n"), &calls), FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pats := append([]LogPattern{{Kind: "actions error", Re: regexp.MustCompile(`^##\[error\]`), Before: 15, After: 2, Limit: 6}}, DefaultLogPatterns...)
	scan, err := s.ScanLog(context.Background(), f, pats)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range scan.Markers {
		for i, l := range m.Lines {
			if l == "##[error]exit 1" && m.Line+i == 35 {
				found = true
				if m.Kind != "actions error" {
					t.Errorf("the error line's block is labelled %q", m.Kind)
				}
			}
		}
	}
	if !found || len(scan.Markers) != 2 || scan.Markers[0].Line != 5 || scan.Markers[1].Line != 26 || len(scan.Markers[0].Lines) != 21 {
		t.Fatalf("markers = %+v", scan.Markers)
	}
}
