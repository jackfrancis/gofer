package github

import (
	"context"
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
