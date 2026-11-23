package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
)

func ciServer(t *testing.T, checkRunsStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/pulls/42":
			_, _ = w.Write([]byte(`{"head":{"ref":"fix/x","sha":"abc123"}}`))
		case "/repos/o/r/commits/abc123/status":
			_, _ = w.Write([]byte(`{"sha":"abc123","total_count":3,"statuses":[
				{"context":"pull-o-r-unit","state":"success","target_url":"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/o_r/42/pull-o-r-unit/1"},
				{"context":"pull-o-r-e2e","state":"failure","description":"Job failed.","target_url":"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/o_r/42/pull-o-r-e2e/2"},
				{"context":"tide","state":"pending","description":"Not mergeable."}]}`))
		case "/repos/o/r/commits/abc123/check-runs":
			if checkRunsStatus != http.StatusOK {
				http.Error(w, "forbidden", checkRunsStatus)
				return
			}
			_, _ = w.Write([]byte(`{"total_count":3,"check_runs":[
				{"name":"lint","status":"completed","conclusion":"success","app":{"slug":"github-actions"}},
				{"name":"build","status":"completed","conclusion":"timed_out","details_url":"https://github.com/o/r/actions/runs/9","output":{"title":"Timed out"},"app":{"slug":"github-actions"}},
				{"name":"coverage","status":"in_progress","app":{"slug":"github-actions"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCIStatusForPR(t *testing.T) {
	srv := ciServer(t, http.StatusOK)
	st, err := NewClient(srv.Client(), srv.URL).CIStatusForPR(context.Background(), "tok", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA != "abc123" || st.Ref != "fix/x" || len(st.Partial) != 0 {
		t.Errorf("status = %+v", st)
	}
	var got []string
	for _, c := range st.Checks {
		got = append(got, c.Class+":"+c.Name+":"+c.State)
	}
	want := "failing:build:timed_out failing:pull-o-r-e2e:failure pending:coverage:in_progress pending:tide:pending passing:lint:success passing:pull-o-r-unit:success"
	if strings.Join(got, " ") != want {
		t.Errorf("checks =\n  %s\nwant\n  %s", strings.Join(got, " "), want)
	}
	if c := st.Checks[1]; c.Kind != "status" || !strings.HasPrefix(c.URL, "https://prow.k8s.io/") || c.Description != "Job failed." {
		t.Errorf("prow status = %+v", c)
	}
	if c := st.Checks[0]; c.Kind != "check" || c.App != "github-actions" || c.Description != "Timed out" {
		t.Errorf("check run = %+v", c)
	}
}

func TestCIStatusCheckRunsRefused(t *testing.T) {
	srv := ciServer(t, http.StatusForbidden)
	st, err := NewClient(srv.Client(), srv.URL).CIStatusForRef(context.Background(), "tok", "o/r", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Checks) != 3 || len(st.Partial) != 1 || !strings.Contains(st.Partial[0], "check runs") {
		t.Errorf("status = %+v", st)
	}
}

func TestCIStatusErrors(t *testing.T) {
	srv := ciServer(t, http.StatusOK)
	c := NewClient(srv.Client(), srv.URL)
	if _, err := c.CIStatusForPR(context.Background(), "tok", "o/r", 7); err == nil {
		t.Error("a missing PR must be an error")
	}
	if _, err := c.CIStatusForRef(context.Background(), "bad", "o/r", "abc123"); err == nil {
		t.Error("an unauthorized call must be an error")
	}
}

// Summary counts passing and pending jobs and names every failing one, failing jobs
// in the order the checks are sorted; a status read with part missing says so.
func TestCIStatusSummary(t *testing.T) {
	srv := ciServer(t, http.StatusOK)
	st, err := NewClient(srv.Client(), srv.URL).CIStatusForPR(context.Background(), "tok", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	sum := st.Summary()
	if sum.SHA != "abc123" || sum.Passed != 2 || sum.Pending != 2 || sum.Partial ||
		strings.Join(sum.Failed, ",") != "build,pull-o-r-e2e" || sum.FailedOmitted != 0 {
		t.Errorf("summary = %+v", sum)
	}
	// Each failed job keeps the link GitHub shows for it on the PR: a check run's
	// details URL, a status's target URL (the Prow job page).
	if len(sum.FailedURLs) != 2 || sum.FailedURLs[0] != "https://github.com/o/r/actions/runs/9" ||
		!strings.HasPrefix(sum.FailedURLs[1], "https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/o_r/42/pull-o-r-e2e/") {
		t.Errorf("failed links = %q", sum.FailedURLs)
	}

	srv = ciServer(t, http.StatusForbidden)
	st, err = NewClient(srv.Client(), srv.URL).CIStatusForPR(context.Background(), "tok", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if sum := st.Summary(); !sum.Partial || sum.Passed != 1 || sum.Pending != 1 || strings.Join(sum.Failed, ",") != "pull-o-r-e2e" {
		t.Errorf("partial summary = %+v", sum)
	}
}

// Once the head commit is known, statuses and check runs are read independently: a
// refused status read still shows the check runs, marked partial; only both
// failing is an error.
func TestCIStatusStatusesRefused(t *testing.T) {
	checks := http.StatusOK
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/abc123/status":
			http.Error(w, "forbidden", http.StatusForbidden)
		case "/repos/o/r/commits/abc123/check-runs":
			if checks != http.StatusOK {
				http.Error(w, "forbidden", checks)
				return
			}
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"failure"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	st, err := c.CIStatusForRef(context.Background(), "tok", "o/r", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if sum := st.Summary(); !sum.Partial || strings.Join(sum.Failed, ",") != "build" {
		t.Errorf("summary = %+v (partial %v)", sum, st.Partial)
	}
	checks = http.StatusForbidden
	if _, err := c.CIStatusForRef(context.Background(), "tok", "o/r", "abc123"); err == nil {
		t.Error("neither source readable must be an error")
	}
}

// The counts follow the outcome GitHub reported: a skipped, neutral or stale check
// neither succeeded nor is still running.
func TestCIStatusSummaryCountsOutcomes(t *testing.T) {
	st := CIStatus{SHA: "s", Checks: []CICheck{
		{Name: "a", State: "success", Class: CIPassing},
		{Name: "b", State: "skipped", Class: CIPassing},
		{Name: "c", State: "neutral", Class: CIPassing},
		{Name: "d", State: "stale", Class: CIPending},
		{Name: "e", State: "queued", Class: CIPending},
		{Name: "f", State: "in_progress", Class: CIPending},
		{Name: "g", State: "pending", Class: CIPending},
		{Name: "h", State: "cancelled", Class: CIFailing},
		{Name: "i", State: "error", Class: CIFailing},
		{Name: "j", State: "completed", Class: CIPending}, // completed with no conclusion
	}}
	if sum := st.Summary(); sum.Passed != 1 || sum.Pending != 3 || strings.Join(sum.Failed, ",") != "h,i" {
		t.Errorf("summary = %+v", sum)
	}
}

// A branch can advance while its CI is paged: every page after the first is pinned
// to the commit the first one resolved, for statuses and for check runs (by the
// first run's head_sha when the statuses could not be read).
func TestCIStatusPinsPagesToOneCommit(t *testing.T) {
	runs := func(sha string, n int) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"name":"job-%s-%d","head_sha":"%s","status":"completed","conclusion":"success"}`, sha, i, sha)
		}
		return b.String()
	}
	statuses := func(n int) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"context":"s-%d","state":"success"}`, i)
		}
		return b.String()
	}
	for _, statusOK := range []bool{true, false} {
		srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			switch r.URL.Path {
			case "/repos/o/r/commits/main/status", "/repos/o/r/commits/aaa/status":
				if !statusOK {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				sha, n := "aaa", 100
				if page == "2" {
					if strings.Contains(r.URL.Path, "/main/") {
						sha = "bbb" // main has advanced
					}
					n = 1
				}
				fmt.Fprintf(w, `{"sha":%q,"total_count":101,"statuses":[%s]}`, sha, statuses(n))
			case "/repos/o/r/commits/main/check-runs", "/repos/o/r/commits/aaa/check-runs":
				if page == "1" {
					fmt.Fprintf(w, `{"total_count":101,"check_runs":[%s]}`, runs("aaa", 100))
					return
				}
				sha := "aaa"
				if strings.Contains(r.URL.Path, "/main/") {
					sha = "bbb"
				}
				fmt.Fprintf(w, `{"total_count":101,"check_runs":[%s]}`, runs(sha, 1))
			default:
				http.NotFound(w, r)
			}
		}))
		st, err := NewClient(srv.Client(), srv.URL).CIStatusForRef(context.Background(), "tok", "o/r", "main")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if st.SHA != "aaa" {
			t.Errorf("statusOK=%v: SHA = %q, want the resolved commit", statusOK, st.SHA)
		}
		for _, c := range st.Checks {
			if strings.Contains(c.Name, "bbb") {
				t.Errorf("statusOK=%v: mixed in %s from the advanced branch", statusOK, c.Name)
				break
			}
		}
		want := 101 // check runs
		if statusOK {
			want += 101 // and statuses
		}
		if len(st.Checks) != want {
			t.Errorf("statusOK=%v: %d checks, want %d", statusOK, len(st.Checks), want)
		}
	}
}

// A ref no response resolved is not reported as a commit; a PR lookup knows its
// head commit even then.
func TestCIStatusUnresolvedRefHasNoSHA(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/pulls/42":
			_, _ = w.Write([]byte(`{"head":{"ref":"fix/x","sha":"abc123"}}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			http.Error(w, "forbidden", http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	st, err := c.CIStatusForRef(context.Background(), "tok", "o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA != "" || st.Ref != "main" || len(st.Partial) == 0 {
		t.Errorf("status = %+v", st)
	}
	st, err = c.CIStatusForPR(context.Background(), "tok", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA != "abc123" {
		t.Errorf("PR lookup SHA = %q, want its head commit", st.SHA)
	}
}
