package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackfrancis/gofer/internal/prow"
)

// ghaFailLog is job 42's log: steps 1 (lines 1-2), 2 (3-5), 3 (6-14, failing), 4 (15).
var ghaFailLog = strings.Join([]string{
	"2026-11-01T10:00:00.1000000Z Current runner version: '2.320.0'",
	"2026-11-01T10:00:01.5000000Z Runner name: 'runner-1'",
	"2026-11-01T10:00:02.2000000Z ##[group]Run actions/checkout@v4",
	"2026-11-01T10:00:03.0000000Z Syncing repository: o/r",
	"2026-11-01T10:00:04.9000000Z ##[endgroup]",
	"2026-11-01T10:00:05.1000000Z ##[group]Run go test ./...",
	"2026-11-01T10:00:05.1000000Z \x1b[36;1mgo test ./...\x1b[0m",
	"2026-11-01T10:00:06.0000000Z ##[endgroup]",
	"2026-11-01T10:00:10.0000000Z ok  \texample.com/a\t0.1s",
	"2026-11-01T10:00:15.0000000Z --- FAIL: TestThing (0.00s)",
	"2026-11-01T10:00:15.0000000Z     thing_test.go:12: want 1, got 2",
	"2026-11-01T10:00:16.0000000Z FAIL",
	"2026-11-01T10:00:16.0000000Z FAIL\texample.com/b\t0.2s",
	"2026-11-01T10:00:19.0000000Z ##[error]Process completed with exit code 1.",
	"2026-11-01T10:00:21.0000000Z Post job cleanup.",
}, "\r\n") + "\r\n"

// ghaQuietLog is job 44's: it fails without a marker the scan knows.
var ghaQuietLog = func() string {
	var b strings.Builder
	b.WriteString("2026-11-01T10:00:00.0000000Z Current runner version\n")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "2026-11-01T10:00:05.0000000Z lint output %d\n", i)
	}
	b.WriteString("2026-11-01T10:00:09.0000000Z something went wrong quietly\n")
	b.WriteString("2026-11-01T10:00:21.0000000Z Post job cleanup.\n")
	return b.String()
}()

// Job 45: steps 2 and 3 start in the same second, so the job log cannot split their
// lines; its failing step 3 has no marker, so the summary reads step 3's own log.
var ghaSameSecondLog = strings.Join([]string{
	"2026-11-01T10:00:00.1000000Z Current runner version",
	"2026-11-01T10:00:02.1000000Z ##[group]Run actions/setup-go@v5",
	"2026-11-01T10:00:02.3000000Z Go installed",
	"2026-11-01T10:00:02.6000000Z ##[group]Run golangci-lint run",
	"2026-11-01T10:00:09.0000000Z internal/x.go:3:1: unused variable",
	"2026-11-01T10:00:21.0000000Z Post job cleanup.",
}, "\n") + "\n"

var ghaSameSecondStep3 = "2026-11-01T10:00:02.6000000Z ##[group]Run golangci-lint run\n" +
	"2026-11-01T10:00:09.0000000Z internal/x.go:3:1: unused variable\n"

const ghaSameSecondSteps = `[
	{"number":1,"name":"Set up job","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:00Z","completed_at":"2026-11-01T10:00:02Z"},
	{"number":2,"name":"Setup Go","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:02Z","completed_at":"2026-11-01T10:00:02Z"},
	{"number":3,"name":"Lint","status":"completed","conclusion":"failure","started_at":"2026-11-01T10:00:02Z","completed_at":"2026-11-01T10:00:20Z"},
	{"number":4,"name":"Complete job","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:21Z","completed_at":"2026-11-01T10:00:21Z"}
]`

// Job 48: step 2 runs in the background while step 3 runs, and fails; its failure
// line lands, by time, inside step 3's range.
var ghaBackgroundLog = strings.Join([]string{
	"2026-11-01T10:00:00.1000000Z Current runner version",
	"2026-11-01T10:00:02.1000000Z ##[group]Run ./server",
	"2026-11-01T10:00:03.0000000Z server listening",
	"2026-11-01T10:00:05.2000000Z ##[group]Run go test ./...",
	"2026-11-01T10:00:06.0000000Z ok  \texample.com/a\t0.1s",
	"2026-11-01T10:00:10.0000000Z server: fatal: out of memory",
	"2026-11-01T10:00:21.0000000Z Post job cleanup.",
}, "\n") + "\n"

var ghaBackgroundStep2 = "2026-11-01T10:00:02.1000000Z ##[group]Run ./server\n" +
	"2026-11-01T10:00:03.0000000Z server listening\n" +
	"2026-11-01T10:00:10.0000000Z server: fatal: out of memory\n"

const ghaBackgroundSteps = `[
	{"number":1,"name":"Set up job","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:00Z","completed_at":"2026-11-01T10:00:02Z"},
	{"number":2,"name":"Start server","status":"completed","conclusion":"failure","started_at":"2026-11-01T10:00:02Z","completed_at":"2026-11-01T10:00:20Z"},
	{"number":3,"name":"Run tests","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:05Z","completed_at":"2026-11-01T10:00:08Z"},
	{"number":4,"name":"Complete job","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:21Z","completed_at":"2026-11-01T10:00:21Z"}
]`

// ghaStep42of3 is job 42's step 3 alone (its own log).
var ghaStep42of3 = "2026-11-01T10:00:15.0000000Z --- FAIL: TestThing (0.00s)\n" +
	"2026-11-01T10:00:19.0000000Z ##[error]Process completed with exit code 1.\n"

const ghaSteps = `[
	{"number":1,"name":"Set up job","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:00Z","completed_at":"2026-11-01T10:00:02Z"},
	{"number":2,"name":"Checkout","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:02Z","completed_at":"2026-11-01T10:00:05Z"},
	{"number":3,"name":"Run tests","status":"completed","conclusion":"failure","started_at":"2026-11-01T10:00:05Z","completed_at":"2026-11-01T10:00:20Z"},
	{"number":5,"name":"Lint","status":"completed","conclusion":"skipped","started_at":"2026-11-01T10:00:20Z","completed_at":"2026-11-01T10:00:20Z"},
	{"number":4,"name":"Post Checkout","status":"completed","conclusion":"success","started_at":"2026-11-01T10:00:21Z","completed_at":"2026-11-01T10:00:21Z"}
]`

func ghaJobJSON(id int64, name, conclusion string) string {
	return ghaJobJSONSteps(id, name, conclusion, ghaSteps)
}

func ghaJobJSONSteps(id int64, name, conclusion, steps string) string {
	return fmt.Sprintf(`{"id":%d,"run_id":7,"run_attempt":1,"name":%q,"workflow_name":"CI","head_branch":"feature","head_sha":"0123456789abcdef0123",`+
		`"status":"completed","conclusion":%q,"html_url":"https://github.com/o/r/actions/runs/7/job/%d",`+
		`"started_at":"2026-11-01T10:00:00Z","completed_at":"2026-11-01T10:00:22Z","steps":%s}`, id, name, conclusion, id, steps)
}

type ghaFake struct {
	api, blob *httptest.Server
	logHits   atomic.Int32
	blobCreds atomic.Bool
}

// newGHAFake serves the API (o/r: run 7 with jobs 42 failing and 43 passing; job 44
// failing quietly; PR 5 whose head has a failing Actions check and a failing Prow
// status) and, on a separate host, the pre-signed log blobs.
func newGHAFake(t *testing.T) *ghaFake {
	t.Helper()
	f := &ghaFake{}
	f.blob = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			f.blobCreds.Store(true)
		}
		if r.URL.Query().Get("sig") != "presigned" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/42":
			_, _ = w.Write([]byte(ghaFailLog))
		case "/44":
			_, _ = w.Write([]byte(ghaQuietLog))
		case "/45":
			_, _ = w.Write([]byte(ghaSameSecondLog))
		case "/45-3":
			_, _ = w.Write([]byte(ghaSameSecondStep3))
		case "/48":
			_, _ = w.Write([]byte(ghaBackgroundLog))
		case "/48-2":
			_, _ = w.Write([]byte(ghaBackgroundStep2))
		case "/48-3":
			_, _ = w.Write([]byte("2026-11-01T10:00:05.2000000Z ##[group]Run go test ./...\n"))
		case "/42-3":
			_, _ = w.Write([]byte(ghaStep42of3))
		default:
			http.NotFound(w, r)
		}
	}))
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret-token" {
			t.Errorf("API call without the token: %s", r.URL.Path)
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/repos/o/r/actions/jobs/"); ok && strings.HasSuffix(rest, "/logs") {
			// …/jobs/<id>/logs → blob /<id>; …/jobs/<id>/steps/<n>/logs → blob /<id>-<n>
			parts := strings.Split(strings.TrimSuffix(rest, "/logs"), "/")
			name := parts[0]
			if len(parts) == 3 && parts[1] == "steps" {
				name += "-" + parts[2]
			} else {
				f.logHits.Add(1)
			}
			http.Redirect(w, r, f.blob.URL+"/"+name+"?sig=presigned", http.StatusFound)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/actions/jobs/45":
			_, _ = w.Write([]byte(ghaJobJSONSteps(45, "lint", "failure", ghaSameSecondSteps)))
		case "/repos/o/r/actions/jobs/48":
			_, _ = w.Write([]byte(ghaJobJSONSteps(48, "e2e", "failure", ghaBackgroundSteps)))
		case "/repos/o/r/actions/jobs/47":
			_, _ = w.Write([]byte(ghaJobJSON(47, "no log", "failure")))
		case "/repos/o/r/actions/runs/9/jobs":
			var jobs []string
			for i := range 50 {
				jobs = append(jobs, ghaJobJSON(int64(2000+i), fmt.Sprintf("matrix-%02d", i), "failure"))
			}
			fmt.Fprintf(w, `{"total_count":50,"jobs":[%s]}`, strings.Join(jobs, ","))
		case "/repos/o/r/actions/jobs/42":
			_, _ = w.Write([]byte(ghaJobJSON(42, "test (ubuntu)", "failure")))
		case "/repos/o/r/actions/jobs/44":
			_, _ = w.Write([]byte(ghaJobJSON(44, "lint", "failure")))
		case "/repos/o/r/actions/runs/7/jobs":
			fmt.Fprintf(w, `{"total_count":2,"jobs":[%s,%s]}`, ghaJobJSON(43, "build", "success"), ghaJobJSON(42, "test (ubuntu)", "failure"))
		case "/repos/o/r/actions/runs/8/jobs":
			fmt.Fprintf(w, `{"total_count":1,"jobs":[%s]}`, ghaJobJSON(42, "test (ubuntu)", "failure"))
		case "/repos/o/r/pulls/5":
			_, _ = w.Write([]byte(`{"head":{"ref":"feature","sha":"0123456789abcdef0123"}}`))
		case "/repos/o/r/commits/0123456789abcdef0123/status":
			_, _ = w.Write([]byte(`{"sha":"0123456789abcdef0123","total_count":1,"statuses":[{"context":"pull-e2e","state":"failure","target_url":"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/o_r/5/pull-e2e/1"}]}`))
		case "/repos/o/r/commits/0123456789abcdef0123/check-runs":
			// codecov's check has no details URL, so its generic /runs/<id> page is listed:
			// the same URL form as an Actions check, but not an Actions job.
			_, _ = w.Write([]byte(`{"total_count":3,"check_runs":[` +
				`{"name":"codecov","status":"completed","conclusion":"failure","html_url":"https://github.com/o/r/runs/999","app":{"slug":"codecov"}},` +
				`{"name":"test (ubuntu)","status":"completed","conclusion":"failure","details_url":"https://github.com/o/r/actions/runs/7/job/42","app":{"slug":"github-actions"}},` +
				`{"name":"build","status":"completed","conclusion":"success","details_url":"https://github.com/o/r/actions/runs/7/job/43","app":{"slug":"github-actions"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.api.Close)
	t.Cleanup(f.blob.Close)
	return f
}

func ghaBox(t *testing.T) (*githubToolBox, *ghaFake) {
	t.Helper()
	f := newGHAFake(t)
	box, _ := ciBox(t, f.api)
	return box, f
}

func ghaCheckClean(t *testing.T, f *ghaFake, out string) {
	t.Helper()
	if f.blobCreds.Load() {
		t.Error("the blob host received the token")
	}
	for _, leak := range []string{"s3cret-token", "presigned", f.blob.URL} {
		if strings.Contains(out, leak) {
			t.Errorf("output contains %q:\n%s", leak, out)
		}
	}
}

func TestGHAToolDefinitions(t *testing.T) {
	box, _ := ciBox(t, nil)
	got := map[string]bool{}
	for _, d := range box.Definitions() {
		got[d.Name] = true
	}
	for _, want := range []string{"gha_job_summary", "gha_search", "gha_read"} {
		if !got[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestGHAJobSummary(t *testing.T) {
	box, f := ghaBox(t)
	out := ciInvoke(t, box, "gha_job_summary", `{"url":"https://github.com/o/r/actions/runs/7/job/42?pr=5"}`)
	assertFramed(t, out, ciUntrustedOpen)
	ghaCheckClean(t, f, out)
	for _, want := range []string{
		"gha_job_summary for Actions job 42 in o/r (run 7).",
		"job: test (ubuntu) (workflow CI)",
		"result: failure",
		"workflow head: feature @0123456789ab (what the run was triggered on, not necessarily the commit tested",
		"ran 22s",
		"  1 Set up job: success (≈ log lines 1-2)",
		"  2 Checkout: success (≈ log lines 3-5)",
		"✗ 3 Run tests: failure (≈ log lines 6-14)",
		"  5 Lint: skipped\n",
		"  4 Post Checkout: success (≈ log lines 15-15)",
		"15 lines; 1 failure marker block(s):",
		"log:10: --- FAIL: TestThing (0.00s)",
		"log:14: ##[error]Process completed with exit code 1.",
		"log:7: go test ./...", // colour codes and timestamps removed
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "2026-11-01T10:00") && strings.Contains(out, "log:") && strings.Contains(out, "Z ##[error]") {
		t.Errorf("timestamps not stripped:\n%s", out)
	}

	// The same job again uses the fetched log.
	ciInvoke(t, box, "gha_job_summary", `{"job_id":42}`)
	if n := f.logHits.Load(); n != 1 {
		t.Errorf("log fetched %d times, want 1", n)
	}
}

func TestGHAJobSummaryTailWithoutMarkers(t *testing.T) {
	box, f := ghaBox(t)
	out := ciInvoke(t, box, "gha_job_summary", `{"repo":"o/r","job_id":44}`)
	ghaCheckClean(t, f, out)
	if !strings.Contains(out, "no failure markers found") || !strings.Contains(out, "the end of step 3:") {
		t.Fatalf("no tail of the failing step:\n%s", out)
	}
	if !strings.Contains(out, "log:62: something went wrong quietly") || strings.Contains(out, "log:63:") {
		t.Errorf("tail does not end at the failing step's last line:\n%s", out)
	}
	if strings.Count(out, "\n    log:") != ghaTailLines {
		t.Errorf("tail has %d lines, want %d", strings.Count(out, "\n    log:"), ghaTailLines)
	}
}

func TestGHARunSummary(t *testing.T) {
	box, f := ghaBox(t)
	out := ciInvoke(t, box, "gha_job_summary", `{"url":"https://github.com/o/r/actions/runs/7"}`)
	ghaCheckClean(t, f, out)
	fail := strings.Index(out, "failing  test (ubuntu) (job 42: failure) — failed step(s): 3 Run tests")
	pass := strings.Index(out, "passing  build (job 43: success)")
	if fail < 0 || pass < 0 || fail > pass {
		t.Fatalf("run listing wrong or not failing-first:\n%s", out)
	}
	if !strings.Contains(out, "2 jobs, 1 failing, 0 pending, 1 passing") || !strings.Contains(out, "The first failing job:\ngha_job_summary for Actions job 42") {
		t.Errorf("run summary:\n%s", out)
	}
}

func TestGHAPRSummary(t *testing.T) {
	box, f := ghaBox(t)
	out := ciInvoke(t, box, "gha_job_summary", `{"number":5}`)
	ghaCheckClean(t, f, out)
	for _, want := range []string{
		"gha_job_summary for o/r PR #5 (commit 0123456789ab): 1 failing GitHub Actions job(s); 2 other failing check(s)",
		"failing  test (ubuntu) (job 42: failure)",
		"log:14: ##[error]Process completed with exit code 1.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PR summary lacks %q:\n%s", want, out)
		}
	}
}

func TestGHASearchAndRead(t *testing.T) {
	box, f := ghaBox(t)
	job := `"url":"https://github.com/o/r/actions/runs/7/job/42"`
	out := ciInvoke(t, box, "gha_search", `{`+job+`,"pattern":"^FAIL","context_lines":1}`)
	assertFramed(t, out, ciUntrustedOpen)
	ghaCheckClean(t, f, out)
	for _, want := range []string{
		"gha_search log in Actions job 42 in o/r: 2 matching lines",
		"log:12: FAIL\n",
		"log-11-     thing_test.go:12: want 1, got 2",
		"log:13: FAIL\texample.com/b\t0.2s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("search lacks %q:\n%s", want, out)
		}
	}

	out = ciInvoke(t, box, "gha_read", `{`+job+`,"step":3}`)
	assertFramed(t, out, ciUntrustedOpen)
	if !strings.HasPrefix(out, "Step 3 is log lines 6-14.\n") ||
		!strings.Contains(out, "gha_read log in Actions job 42 in o/r: lines 6-14 of 15.\n") ||
		!strings.Contains(out, "    14\t##[error]Process completed") || strings.Contains(out, "    15\t") || strings.Contains(out, "Continue") {
		t.Errorf("read of step 3:\n%s", out)
	}
	// A larger max_lines still stops at the step's last line.
	out = ciInvoke(t, box, "gha_read", `{`+job+`,"step":3,"max_lines":40}`)
	if !strings.Contains(out, "lines 6-14 of 15.\n") || strings.Contains(out, "    15\t") || strings.Contains(out, "Continue") {
		t.Errorf("read of step 3 with max_lines=40:\n%s", out)
	}
	// A smaller one pages inside it.
	out = ciInvoke(t, box, "gha_read", `{`+job+`,"step":3,"max_lines":2}`)
	if !strings.Contains(out, "lines 6-7 of 15. Continue with start_line=8.") {
		t.Errorf("read of step 3 with max_lines=2:\n%s", out)
	}

	// A "#step:N" link reads that step.
	out = ciInvoke(t, box, "gha_read", `{"url":"https://github.com/o/r/actions/runs/7/job/42#step:2:1"}`)
	if !strings.Contains(out, "lines 3-5 of 15") {
		t.Errorf("read from a step link:\n%s", out)
	}
	out = ciInvoke(t, box, "gha_read", `{"job_id":42,"start_line":-2}`)
	if !strings.Contains(out, "lines 14-15 of 15") || !strings.Contains(out, "Post job cleanup.") {
		t.Errorf("read from the end:\n%s", out)
	}

	// A run with one job resolves to it; with several, a job must be named.
	out = ciInvoke(t, box, "gha_search", `{"url":"https://github.com/o/r/actions/runs/8","pattern":"TestThing","literal":true}`)
	if !strings.Contains(out, "log:10: --- FAIL: TestThing") {
		t.Errorf("single-job run search:\n%s", out)
	}
	if f.logHits.Load() != 1 {
		t.Errorf("log fetched %d times, want 1", f.logHits.Load())
	}
}

func TestGHAToolErrors(t *testing.T) {
	box, _ := ghaBox(t)
	for _, c := range []struct{ name, args, want string }{
		{"gha_search", `{"url":"https://github.com/o/r/actions/runs/7","pattern":"x"}`, "has 2 jobs; name one"},
		{"gha_search", `{"job_id":42}`, "pattern is required"},
		{"gha_read", `{"job_id":42,"step":5}`, "step 5 did not run (skipped)"},
		{"gha_read", `{"job_id":42,"step":9}`, "job 42 has no step 9"},
		{"gha_job_summary", `{"url":"https://github.com/o/r/runs/999"}`, "can also be another app's check"},
		{"gha_read", `{}`, "url (an Actions job or run link), job_id or run_id is required"},
		{"gha_job_summary", `{"url":"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/o_r/5/pull-e2e/1"}`, "open it with ci_job_summary"},
		{"gha_job_summary", `{"url":"https://example.com/o/r/actions/runs/1"}`, "not github.com"},
		{"gha_job_summary", `{"job_id":99}`, "not found"},
	} {
		_, err := box.Invoke(context.Background(), c.name, json.RawMessage(c.args))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s(%s): err = %v, want %q", c.name, c.args, err, c.want)
		}
	}
}

func TestGHAJobLimit(t *testing.T) {
	box, _ := ghaBox(t)
	box.gha.reserved = map[string]bool{}
	for i := range maxGHAJobs {
		box.gha.reserved[fmt.Sprintf("o/r/%d", 1000+i)] = true
	}
	out := ciInvoke(t, box, "gha_job_summary", `{"job_id":42}`)
	if !strings.Contains(out, "could not be read: at most 6 GitHub Actions job logs per answer") {
		t.Errorf("job limit not reported:\n%s", out)
	}
}

// A job whose log could not be fetched still takes its slot, and a retry reuses it.
func TestGHAJobLimitCountsFailedFetches(t *testing.T) {
	box, _ := ghaBox(t)
	out := ciInvoke(t, box, "gha_job_summary", `{"job_id":47}`)
	if !strings.Contains(out, "could not be read: job log download: status 404, again with a fresh download link") {
		t.Fatalf("missing log not reported:\n%s", out)
	}
	if !box.gha.reserved["o/r/47"] || box.gha.jobs["o/r/47"] != nil {
		t.Fatalf("reserved = %v, jobs = %v", box.gha.reserved, box.gha.jobs)
	}
	ciInvoke(t, box, "gha_job_summary", `{"job_id":47}`)
	if len(box.gha.reserved) != 1 {
		t.Errorf("a retry took another slot: %v", box.gha.reserved)
	}
}

// Steps that start in the same second share a range, and the failing one is read
// from its own log.
func TestGHAStepsStartingTogether(t *testing.T) {
	box, f := ghaBox(t)
	out := ciInvoke(t, box, "gha_job_summary", `{"job_id":45}`)
	assertFramed(t, out, ciUntrustedOpen)
	ghaCheckClean(t, f, out)
	for _, want := range []string{
		"  1 Set up job: success (≈ log lines 1-1)",
		"  2 Setup Go: success (≈ log lines 2-5, mixed with step(s) 3, which started the same second or ran at the same time; gha_read step=2 reads its own log)",
		"✗ 3 Lint: failure (≈ log lines 2-5, mixed with step(s) 2, which started the same second or ran at the same time; gha_read step=3 reads its own log)",
		"  4 Complete job: success (≈ log lines 6-6)",
		"the end of step 3, from its own log (step-3.log, numbered within the step",
		"step-3.log:2: internal/x.go:3:1: unused variable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "step-3.log:3:") {
		t.Errorf("step tail runs past the step's own log:\n%s", out)
	}

	out = ciInvoke(t, box, "gha_read", `{"job_id":45,"step":3}`)
	assertFramed(t, out, ciUntrustedOpen)
	if !strings.HasPrefix(out, "Step 3's own log (the job log cannot place its lines)") ||
		!strings.Contains(out, "gha_read step-3.log in Actions job 45 in o/r: lines 1-2 of 2.") ||
		!strings.Contains(out, "     2\tinternal/x.go:3:1: unused variable") {
		t.Errorf("read of step 3:\n%s", out)
	}
	// An unambiguous step is still read from the job log.
	out = ciInvoke(t, box, "gha_read", `{"job_id":45,"step":4}`)
	if !strings.Contains(out, "gha_read log in Actions job 45 in o/r: lines 6-6 of 6.") {
		t.Errorf("read of step 4:\n%s", out)
	}
}

// A background step overlaps the steps after it: neither gets a clean range, and the
// failing background step is read from its own log.
func TestGHABackgroundStep(t *testing.T) {
	box, f := ghaBox(t)
	out := ciInvoke(t, box, "gha_job_summary", `{"job_id":48}`)
	ghaCheckClean(t, f, out)
	for _, want := range []string{
		"✗ 2 Start server: failure (≈ log lines 2-3, mixed with step(s) 3, which started the same second or ran at the same time; gha_read step=2 reads its own log)",
		"  3 Run tests: success (≈ log lines 4-6, mixed with step(s) 2, which started the same second or ran at the same time; gha_read step=3 reads its own log)",
		"  4 Complete job: success (≈ log lines 7-7)",
		"the end of step 2, from its own log (step-2.log, numbered within the step; the job log cannot place it)",
		"step-2.log:3: server: fatal: out of memory",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	out = ciInvoke(t, box, "gha_read", `{"job_id":48,"step":3}`)
	if !strings.HasPrefix(out, "Step 3's own log (the job log cannot place its lines)") {
		t.Errorf("read of a step overlapped by a background step:\n%s", out)
	}
}

// When the job log is over the per-file limit, an explicit step read (and the
// summary's failing-step tail) uses the step's own, smaller log.
func TestGHAStepLogWhenJobLogTooLarge(t *testing.T) {
	box, f := ghaBox(t)
	box.ci.Close()
	box.ci = prow.NewSession(prow.NewClient("http://127.0.0.1:1"), prow.Limits{MaxObjectBytes: int64(len(ghaStep42of3))})

	out := ciInvoke(t, box, "gha_read", `{"job_id":42,"step":3}`)
	ghaCheckClean(t, f, out)
	if !strings.HasPrefix(out, "Step 3's own log (the job log is too large to fetch)") ||
		!strings.Contains(out, "gha_read step-3.log in Actions job 42 in o/r: lines 1-2 of 2.") {
		t.Errorf("step read with an oversized job log:\n%s", out)
	}
	// Without a step there is nothing smaller to read.
	if _, err := box.Invoke(context.Background(), "gha_read", json.RawMessage(`{"job_id":42}`)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("whole-log read: err = %v", err)
	}

	out = ciInvoke(t, box, "gha_job_summary", `{"job_id":42}`)
	if !strings.Contains(out, "could not be read: job log too large") ||
		!strings.Contains(out, "the end of step 3, from its own log (step-3.log, numbered within the step; the job log is too large to fetch)") ||
		!strings.Contains(out, "step-3.log:2: ##[error]Process completed with exit code 1.") {
		t.Errorf("summary with an oversized job log:\n%s", out)
	}
}

// The diagnosis comes before a long job list, which is capped.
func TestGHAOverviewDiagnosisFirst(t *testing.T) {
	box, _ := ghaBox(t)
	out := ciInvoke(t, box, "gha_job_summary", `{"run_id":9}`)
	diag := strings.Index(out, "The first failing job:\ngha_job_summary for Actions job 2000")
	list := strings.Index(out, "Jobs of this run")
	if diag < 0 || list < 0 || diag > list {
		t.Fatalf("diagnosis not ahead of the list:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("… and %d more jobs", 50-maxGHAJobsListed)) || strings.Contains(out, "matrix-45") {
		t.Errorf("list not capped:\n%s", out)
	}

	out = ciInvoke(t, box, "gha_job_summary", `{"number":5}`)
	diag = strings.Index(out, "The first failing job:\ngha_job_summary for Actions job 42")
	list = strings.Index(out, "Failing GitHub Actions jobs on this pull request:")
	if diag < 0 || list < 0 || diag > list {
		t.Fatalf("PR: diagnosis not ahead of the list:\n%s", out)
	}
}

func TestCIStatusTagsActions(t *testing.T) {
	box, _ := ghaBox(t)
	out := ciInvoke(t, box, "github_get_ci_status", `{"number":5}`)
	if !strings.Contains(out, "[actions] https://github.com/o/r/actions/runs/7/job/42") || !strings.Contains(out, "[prow] https://prow.k8s.io/") {
		t.Errorf("status tags:\n%s", out)
	}
	// Another app's check on the generic /runs/<id> page is not tagged.
	if !strings.Contains(out, "\n           https://github.com/o/r/runs/999\n") {
		t.Errorf("third-party check URL tagged or missing:\n%s", out)
	}
}
