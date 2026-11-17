package prow

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveJob exercises the real path — parse a Prow job URL, list and fetch its
// public artifacts from GCS, summarise and search them — against a real job. It is
// skipped unless GOFER_LIVE_PROW_JOB names a job URL, e.g.
//
//	GOFER_LIVE_PROW_JOB=https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/6825/pull-cluster-api-provider-azure-e2e-workload-upgrade-v1beta1/2121629446245978112
//
// Like checkout's live test, it is meant to be compiled (go test -c) and run INSIDE
// the runtime's sandbox, to verify what unit tests cannot: that the sandbox can reach
// storage.googleapis.com and has scratch space, and how long a summary takes there.
func TestLiveJob(t *testing.T) {
	raw := os.Getenv("GOFER_LIVE_PROW_JOB")
	if raw == "" {
		t.Skip("GOFER_LIVE_PROW_JOB not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s := NewSession(NewClient(""), Limits{})
	defer s.Close()
	job, _, err := s.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	sum, err := s.Summarize(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("summary in %s: result=%s job=%s type=%s refs=%q log=%d lines, %d markers, junit %d tests %d failed, %d artifact dirs, missing=%v",
		time.Since(began).Round(time.Millisecond), sum.Result, sum.JobName, sum.JobType, sum.Refs,
		sum.LogLines, len(sum.LogMarkers), sum.JUnit.Tests, sum.JUnit.Failed, len(sum.Artifacts), sum.Missing)
	for _, m := range sum.LogMarkers {
		t.Logf("marker %s at line %d:\n  %s", m.Kind, m.Line, strings.Join(m.Lines, "\n  "))
	}
	for _, f := range sum.JUnit.Failures {
		t.Logf("failed: %s\n  %.300s", f.Name, f.Message)
	}
	for _, a := range sum.Artifacts {
		t.Logf("artifact %s dir=%v objects=%d bytes=%d", a.Name, a.Dir, a.Objects, a.Bytes)
	}
	if pattern := os.Getenv("GOFER_LIVE_PROW_PATTERN"); pattern != "" {
		began = time.Now()
		res, err := s.Search(ctx, job, SearchOptions{Pattern: pattern, Path: os.Getenv("GOFER_LIVE_PROW_PATH"), MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("search %q in %s: %d matches in %d files (%d searched), skipped binary=%d large=%d limit=%d failed=%d",
			pattern, time.Since(began).Round(time.Millisecond), res.TotalMatches, res.FilesMatched, res.FilesSearched,
			res.SkippedBinary, res.SkippedLarge, res.SkippedLimit, res.SkippedFailed)
		for _, m := range res.Matches {
			t.Logf("  %s:%d: %s", m.Path, m.Line, m.Text)
		}
	}
}
