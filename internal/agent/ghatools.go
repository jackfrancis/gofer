// GitHub Actions job-log tools for the assistive conversation: gha_job_summary,
// gha_search and gha_read. They are the Actions counterpart of the Prow ci_* tools
// (citools.go) and share their machinery: a job log is fetched into the run's CI
// session (internal/prow), under the same per-run byte, file and time budgets, and
// is searched, read and scanned for failure markers by the same code, with the same
// output shapes and clamping.
//
// Logs are fetched with the run's vended token through internal/github, which sends
// the token to the API host only (the log itself comes from a pre-signed blob URL,
// fetched without it). Everything in a log or a job's metadata is untrusted — a pull
// request controls what its workflow prints and what its jobs and steps are called —
// so every result frames it as data.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/prow"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const (
	// maxGHAJobs bounds the distinct Actions job logs one run fetches (the Prow tools
	// allow as many Prow jobs).
	maxGHAJobs = 6
	// maxGHARunJobsShown bounds the jobs a run overview lists.
	maxGHARunJobsShown = 60
	// ghaTailLines is how much of a failing step's end a summary shows when the log has
	// no failure marker.
	ghaTailLines = 40
	// ghaLogName names a job log in search and read results.
	ghaLogName = "log"
)

// ghaLogPatterns are the failure lines of an Actions log: the runner's own error
// annotation (with the output before it, which is where the cause is), then the
// tool conventions the Prow scan knows.
var ghaLogPatterns = append([]prow.LogPattern{
	{Kind: "actions error", Re: regexp.MustCompile(`^##\[error\]`), Before: 15, After: 2, Limit: 6},
}, prow.DefaultLogPatterns...)

// ghaTimestamp is the prefix the runner writes on every log line.
var ghaTimestamp = regexp.MustCompile(`^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z) ?`)

// ghaLogs is this run's Actions job logs, by job.
type ghaLogs struct {
	mu   sync.Mutex
	jobs map[string]*ghaLog // by "repo/job id"
}

// ghaLog is one job's fetched log, with each step's approximate line range.
type ghaLog struct {
	repo  string
	job   github.ActionsJob
	file  *prow.File
	steps map[int]lineRange // by step number; absent when a step has no lines
}

type lineRange struct{ first, last int }

var (
	ghaTargetProps = `"url":{"type":"string","description":"a GitHub Actions link exactly as a check or page shows it: https://github.com/<owner>/<repo>/actions/runs/<run>/job/<job> (a job), …/actions/runs/<run> (a run), or a check run's https://github.com/<owner>/<repo>/runs/<id>"},` +
		`"repo":{"type":"string","description":"owner/name, with job_id or run_id instead of url; defaults to the current item's repository"},` +
		`"job_id":{"type":"integer","description":"instead of url: an Actions job ID (the same number as its check run ID)"},`

	schemaGHASummary = json.RawMessage(`{"type":"object","properties":{` + ghaTargetProps +
		`"run_id":{"type":"integer","description":"instead of url: a workflow run ID; lists its jobs and summarises the first failing one"},` +
		`"number":{"type":"integer","description":"instead of url: a pull request number; finds the failing GitHub Actions checks on its head commit and summarises the first"}` +
		`}}`)

	schemaGHASearch = json.RawMessage(`{"type":"object","properties":{` + ghaTargetProps +
		`"pattern":{"type":"string","description":"an RE2 regular expression (e.g. 'FAIL|panic:|Error:'), or an exact string when literal is true"},` +
		`"literal":{"type":"boolean","description":"treat pattern as an exact string (default false)"},` +
		`"ignore_case":{"type":"boolean","description":"case-insensitive match (default false)"},` +
		`"context_lines":{"type":"integer","description":"lines of context before and after each match, 0-5 (default 0)"},` +
		`"max_results":{"type":"integer","description":"matching lines to return, up to 200 (default 60)"}` +
		`},"required":["pattern"]}`)

	schemaGHARead = json.RawMessage(`{"type":"object","properties":{` + ghaTargetProps +
		`"step":{"type":"integer","description":"a step number (from gha_job_summary): read from that step's first log line"},` +
		`"start_line":{"type":"integer","description":"first line, 1-based (default 1, or the step's first line); negative counts from the end, e.g. -200 reads the last 200 lines"},` +
		`"max_lines":{"type":"integer","description":"lines to return, up to 1500 (default 400, or the whole step)"}` +
		`}}`)
)

// ghaToolDefs advertises the Actions log tools.
func ghaToolDefs() []worklist.ToolDef {
	return []worklist.ToolDef{
		{
			Name:        "gha_job_summary",
			Description: "Summarise a GitHub Actions job from its URL (a failing check's details link, https://github.com/<owner>/<repo>/actions/runs/<run>/job/<job>): its result, the commit it tested, every step with its result and approximate log line range, and the places in the job's log that show the failure (the runner's ##[error] lines with the output just before them, go test FAIL lines, panics, make errors), or the end of the failing step when there are none. A run URL lists the run's jobs and summarises the first failing one; a pull request number does the same for the failing Actions checks on its head commit. Start here for any question about why a GitHub Actions job failed, then follow up with gha_search or gha_read.",
			Parameters:  schemaGHASummary,
		},
		{
			Name:        "gha_search",
			Description: "Search one GitHub Actions job's log for a pattern and return matching lines as log:line: text, optionally with context (timestamps are removed). Use it to find an error message or a failing test's output instead of reading a long log page by page.",
			Parameters:  schemaGHASearch,
		},
		{
			Name:        "gha_read",
			Description: "Read a window of lines, with line numbers, from one GitHub Actions job's log. Use it to read around a gha_search match or a gha_job_summary marker, or a whole step (step=N); a negative start_line reads from the end.",
			Parameters:  schemaGHARead,
		},
	}
}

type ghaArgs struct {
	URL          string `json:"url"`
	Repo         string `json:"repo"`
	JobID        int64  `json:"job_id"`
	RunID        int64  `json:"run_id"`
	Number       int    `json:"number"`
	Pattern      string `json:"pattern"`
	Literal      bool   `json:"literal"`
	IgnoreCase   bool   `json:"ignore_case"`
	ContextLines int    `json:"context_lines"`
	MaxResults   int    `json:"max_results"`
	Step         int    `json:"step"`
	StartLine    int    `json:"start_line"`
	MaxLines     int    `json:"max_lines"`
}

// runGHATool runs one Actions tool, unclamped. ok is false when name is not one.
func (t *githubToolBox) runGHATool(ctx context.Context, name string, args json.RawMessage) (out string, ok bool, err error) {
	switch name {
	case "gha_job_summary", "gha_search", "gha_read":
	default:
		return "", false, nil
	}
	var a ghaArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", true, fmt.Errorf("bad arguments: %w", err)
	}
	if name == "gha_search" && a.Pattern == "" {
		return "", true, fmt.Errorf("pattern is required")
	}
	if t.ci == nil {
		return "", true, fmt.Errorf("CI logs are not available in this run")
	}
	switch name {
	case "gha_job_summary":
		out, err = t.ghaSummary(ctx, a)
	case "gha_search":
		out, err = t.ghaSearch(ctx, a)
	case "gha_read":
		out, err = t.ghaRead(ctx, a)
	}
	return out, true, err
}

// ghaTarget is what a call names: one job, or a run (jobs listed), in repo.
type ghaTarget struct {
	repo  string
	job   *github.ActionsJob // set when one job is named (or the run has only one)
	run   int64
	jobs  []github.ActionsJob // the run's jobs, when a run is named
	whole bool                // jobs is the run's whole job list
	step  int                 // a step the URL linked
}

// resolve turns url / repo+job_id / repo+run_id into a target.
func (t *githubToolBox) ghaResolve(ctx context.Context, a ghaArgs) (ghaTarget, error) {
	var tg ghaTarget
	var ref github.ActionsRef
	switch {
	case strings.TrimSpace(a.URL) != "":
		r, err := github.ParseActionsURL(a.URL)
		if err != nil {
			if _, _, perr := prow.ParseJobURL(a.URL); perr == nil {
				return tg, fmt.Errorf("that is a Prow job URL; open it with ci_job_summary")
			}
			return tg, err
		}
		ref = r
	case a.JobID > 0:
		ref = github.ActionsRef{Repo: t.repoOr(a.Repo), JobID: a.JobID}
	case a.RunID > 0:
		ref = github.ActionsRef{Repo: t.repoOr(a.Repo), RunID: a.RunID}
	default:
		return tg, fmt.Errorf("url (an Actions job or run link), job_id or run_id is required")
	}
	tg.repo, tg.run, tg.step = ref.Repo, ref.RunID, ref.Step
	if ref.JobID > 0 {
		j, err := t.gh.ActionsJob(ctx, t.token, ref.Repo, ref.JobID)
		if err != nil {
			return tg, err
		}
		tg.job, tg.run = &j, j.RunID
		return tg, nil
	}
	jobs, whole, err := t.gh.ActionsRunJobs(ctx, t.token, ref.Repo, ref.RunID, ref.Attempt)
	if err != nil {
		return tg, err
	}
	if len(jobs) == 0 {
		return tg, fmt.Errorf("Actions run %d in %s has no jobs", ref.RunID, ref.Repo)
	}
	sortGHAJobs(jobs)
	tg.jobs, tg.whole = jobs, whole
	if len(jobs) == 1 {
		tg.job = &jobs[0]
	}
	return tg, nil
}

// ghaJob resolves a call that must name one job (search, read).
func (t *githubToolBox) ghaJob(ctx context.Context, a ghaArgs) (ghaTarget, error) {
	tg, err := t.ghaResolve(ctx, a)
	if err != nil {
		return tg, err
	}
	if tg.job == nil {
		return tg, fmt.Errorf("Actions run %d in %s has %d jobs; name one with its job URL or job_id (gha_job_summary on the run lists them)", tg.run, tg.repo, len(tg.jobs))
	}
	return tg, nil
}

// sortGHAJobs orders jobs failing first, then pending, then passing; by name within.
func sortGHAJobs(jobs []github.ActionsJob) {
	rank := func(j github.ActionsJob) int {
		switch {
		case j.Failing():
			return 0
		case j.Status != "completed":
			return 1
		}
		return 2
	}
	sort.SliceStable(jobs, func(i, k int) bool {
		if ri, rk := rank(jobs[i]), rank(jobs[k]); ri != rk {
			return ri < rk
		}
		return strings.ToLower(jobs[i].Name) < strings.ToLower(jobs[k].Name)
	})
}

// ghaLabel names a job in result headers. Only validated values go here (the repo
// and IDs); job and step names are untrusted and stay inside the frame.
func ghaLabel(repo string, job github.ActionsJob) string {
	return fmt.Sprintf("Actions job %d in %s", job.ID, repo)
}

// ghaOpen fetches a job's log into the run's CI session, once per job (and again
// only if the job was not finished the first time). Opens are serialised: the
// session serialises downloads anyway, and a job's step ranges are filled in by the
// one fetch that downloads it.
func (t *githubToolBox) ghaOpen(ctx context.Context, repo string, job github.ActionsJob) (*ghaLog, error) {
	key := fmt.Sprintf("%s/%d", repo, job.ID)
	t.gha.mu.Lock()
	defer t.gha.mu.Unlock()
	if t.gha.jobs == nil {
		t.gha.jobs = map[string]*ghaLog{}
	}
	prev, seen := t.gha.jobs[key]
	if seen && prev.job.Status == "completed" {
		return prev, nil
	}
	if !seen && len(t.gha.jobs) >= maxGHAJobs {
		return nil, fmt.Errorf("at most %d GitHub Actions job logs per answer; use the ones already opened", maxGHAJobs)
	}

	l := &ghaLog{repo: repo, job: job, steps: map[int]lineRange{}}
	mapper := newStepMapper(job.Steps, l.steps)
	fetch := func(ctx context.Context, w io.Writer, max int64) (int64, error) {
		return t.gh.ActionsJobLog(ctx, t.token, repo, job.ID, w, max)
	}
	// A job not yet finished may have a partial log; key it by status so the finished
	// log is fetched once it is.
	fkey := fmt.Sprintf("gha:%s/%d/%s", repo, job.ID, job.Status)
	f, err := t.ci.FetchWith(ctx, fkey, ghaLogName, fetch, prow.FetchOptions{LineHead: mapper.lineHead})
	if err != nil {
		return nil, err
	}
	if seen && prev.file == f {
		return prev, nil // the same unfinished log, already mapped
	}
	if f.Binary {
		return nil, fmt.Errorf("the job log of Actions job %d is binary content", job.ID)
	}
	l.file = f
	t.gha.jobs[key] = l
	return l, nil
}

// stepMapper assigns log lines to steps by time: the runner stamps every line, and
// the API gives each step's start (to the second). A line belongs to the latest step
// started at or before it; unstamped lines belong to the step of the line before.
// The ranges are approximate at step boundaries that fall within one second.
type stepMapper struct {
	order []github.ActionsStep // steps with output, by start
	cur   int
	out   map[int]lineRange
}

func newStepMapper(steps []github.ActionsStep, out map[int]lineRange) *stepMapper {
	m := &stepMapper{cur: -1, out: out}
	for _, s := range steps {
		if s.StartedAt.IsZero() || s.Conclusion == "skipped" {
			continue
		}
		m.order = append(m.order, s)
	}
	sort.SliceStable(m.order, func(i, k int) bool {
		if !m.order[i].StartedAt.Equal(m.order[k].StartedAt) {
			return m.order[i].StartedAt.Before(m.order[k].StartedAt)
		}
		return m.order[i].Number < m.order[k].Number
	})
	return m
}

func (m *stepMapper) lineHead(num int, head []byte) int {
	drop := 0
	if loc := ghaTimestamp.FindSubmatchIndex(head); loc != nil {
		drop = loc[1]
		if ts, err := time.Parse(time.RFC3339Nano, string(head[loc[2]:loc[3]])); err == nil {
			sec := ts.Truncate(time.Second)
			for m.cur+1 < len(m.order) && !sec.Before(m.order[m.cur+1].StartedAt) {
				m.cur++
			}
		}
	}
	if m.cur >= 0 {
		n := m.order[m.cur].Number
		r, ok := m.out[n]
		if !ok {
			r.first = num
		}
		r.last = num
		m.out[n] = r
	}
	return drop
}

// ghaSummary runs gha_job_summary.
func (t *githubToolBox) ghaSummary(ctx context.Context, a ghaArgs) (string, error) {
	if strings.TrimSpace(a.URL) == "" && a.JobID == 0 && a.RunID == 0 && a.Number > 0 {
		return t.ghaSummaryForPR(ctx, t.repoOr(a.Repo), a.Number)
	}
	tg, err := t.ghaResolve(ctx, a)
	if err != nil {
		return "", err
	}
	if tg.job != nil && len(tg.jobs) <= 1 {
		return t.ghaJobSummary(ctx, tg.repo, *tg.job), nil
	}
	out := formatGHARun(tg)
	for _, j := range tg.jobs {
		if j.Failing() {
			out += "\n\nThe first failing job:\n" + t.ghaJobSummary(ctx, tg.repo, j)
			break
		}
	}
	return out, nil
}

// ghaSummaryForPR lists the failing Actions checks on a pull request's head commit
// and summarises the first.
func (t *githubToolBox) ghaSummaryForPR(ctx context.Context, repo string, number int) (string, error) {
	st, err := t.gh.CIStatusForPR(ctx, t.token, repo, number)
	if err != nil {
		return "", err
	}
	type failing struct {
		check github.CICheck
		ref   github.ActionsRef
	}
	var fails []failing
	others := 0
	for _, c := range st.Checks {
		if c.Class != github.CIFailing {
			continue
		}
		ref, perr := github.ParseActionsURL(c.URL)
		if c.Kind != "check" || perr != nil || ref.JobID == 0 {
			others++
			continue
		}
		fails = append(fails, failing{c, ref})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gha_job_summary for %s PR #%d", repo, number)
	if st.SHA != "" {
		fmt.Fprintf(&b, " (commit %s)", shortSHA(st.SHA))
	}
	fmt.Fprintf(&b, ": %d failing GitHub Actions job(s)", len(fails))
	if others > 0 {
		fmt.Fprintf(&b, "; %d other failing check(s) (see github_get_ci_status)", others)
	}
	b.WriteString(".")
	for _, p := range st.Partial {
		fmt.Fprintf(&b, "\nPartial: %s.", p)
	}
	if len(fails) == 0 {
		return b.String(), nil
	}
	b.WriteString("\n\n" + untrustedOpen)
	for _, f := range fails {
		fmt.Fprintf(&b, "  failing  %s (job %d: %s)\n", oneLine(f.check.Name, 200), f.ref.JobID, oneLine(f.check.State, 40))
		fmt.Fprintf(&b, "           %s\n", oneLine(f.check.URL, 500))
	}
	b.WriteString(untrustedClose)
	first := fails[0].ref
	job, err := t.gh.ActionsJob(ctx, t.token, first.Repo, first.JobID)
	if err != nil {
		fmt.Fprintf(&b, "\n\nThe first failing job could not be read: %s", oneLine(err.Error(), 300))
		return b.String(), nil
	}
	b.WriteString("\n\nThe first failing job:\n" + t.ghaJobSummary(ctx, first.Repo, job))
	return b.String(), nil
}

// formatGHARun renders a run's job list.
func formatGHARun(tg ghaTarget) string {
	var failing, pending, passing int
	for _, j := range tg.jobs {
		switch {
		case j.Failing():
			failing++
		case j.Status != "completed":
			pending++
		default:
			passing++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gha_job_summary for Actions run %d in %s: %d jobs, %d failing, %d pending, %d passing.\n", tg.run, tg.repo, len(tg.jobs), failing, pending, passing)
	if !tg.whole {
		b.WriteString("Partial: the run holds more jobs than were read.\n")
	}
	b.WriteString("Open a job with gha_job_summary (its job URL or job_id).\n\n" + untrustedOpen)
	for i, j := range tg.jobs {
		if i >= maxGHARunJobsShown {
			fmt.Fprintf(&b, "  … and %d more jobs\n", len(tg.jobs)-i)
			break
		}
		class := "passing"
		switch {
		case j.Failing():
			class = "failing"
		case j.Status != "completed":
			class = "pending"
		}
		fmt.Fprintf(&b, "  %-8s %s (job %d: %s)", class, oneLine(j.Name, 200), j.ID, oneLine(ghaState(j.Status, j.Conclusion), 40))
		if steps := failedSteps(j); steps != "" {
			fmt.Fprintf(&b, " — failed step(s): %s", steps)
		}
		b.WriteString("\n")
		if class != "passing" && j.HTMLURL != "" {
			fmt.Fprintf(&b, "           %s\n", oneLine(j.HTMLURL, 500))
		}
	}
	b.WriteString(untrustedClose)
	return b.String()
}

func ghaState(status, conclusion string) string {
	if status != "completed" || conclusion == "" {
		return status
	}
	return conclusion
}

func ghaStepFailed(s github.ActionsStep) bool {
	switch s.Conclusion {
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure":
		return true
	}
	return false
}

func failedSteps(j github.ActionsJob) string {
	var parts []string
	for _, s := range j.Steps {
		if ghaStepFailed(s) {
			parts = append(parts, fmt.Sprintf("%d %s", s.Number, oneLine(s.Name, 120)))
		}
	}
	return strings.Join(parts, "; ")
}

// ghaJobSummary renders one job: outcome, steps, and where its log shows failure. A
// log that cannot be fetched is reported in the summary, not as an error: the job's
// metadata still answers part of the question.
func (t *githubToolBox) ghaJobSummary(ctx context.Context, repo string, job github.ActionsJob) string {
	l, logErr := t.ghaOpen(ctx, repo, job)
	var b strings.Builder
	fmt.Fprintf(&b, "gha_job_summary for %s (run %d", ghaLabel(repo, job), job.RunID)
	if job.RunAttempt > 1 {
		fmt.Fprintf(&b, ", attempt %d", job.RunAttempt)
	}
	b.WriteString(").\n\n" + ciUntrustedOpen)

	b.WriteString("  outcome:\n")
	fmt.Fprintf(&b, "    job: %s", oneLine(job.Name, 200))
	if job.WorkflowName != "" {
		fmt.Fprintf(&b, " (workflow %s)", oneLine(job.WorkflowName, 200))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "    result: %s\n", oneLine(ghaState(job.Status, job.Conclusion), 40))
	if job.HeadSHA != "" || job.HeadBranch != "" {
		fmt.Fprintf(&b, "    tested: %s @%s\n", oneLine(job.HeadBranch, 200), shortSHA(job.HeadSHA))
	}
	if !job.StartedAt.IsZero() {
		fmt.Fprintf(&b, "    started: %s", job.StartedAt.UTC().Format(time.RFC3339))
		if job.CompletedAt.After(job.StartedAt) {
			fmt.Fprintf(&b, ", ran %s", job.CompletedAt.Sub(job.StartedAt).Round(time.Second))
		}
		b.WriteString("\n")
	}
	if job.HTMLURL != "" {
		fmt.Fprintf(&b, "    url: %s\n", oneLine(job.HTMLURL, 500))
	}

	b.WriteString("  steps:\n")
	if len(job.Steps) == 0 {
		b.WriteString("    (none reported)\n")
	}
	for _, s := range job.Steps {
		mark := " "
		if ghaStepFailed(s) {
			mark = "✗"
		}
		fmt.Fprintf(&b, "    %s %d %s: %s", mark, s.Number, oneLine(s.Name, 200), oneLine(ghaState(s.Status, s.Conclusion), 40))
		if l != nil {
			if r, ok := l.steps[s.Number]; ok {
				fmt.Fprintf(&b, " (≈ log lines %d-%d)", r.first, r.last)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("  log:\n")
	if logErr != nil {
		fmt.Fprintf(&b, "    could not be read: %s\n", oneLine(logErr.Error(), 300))
		b.WriteString(untrustedClose)
		return b.String()
	}
	if job.Status != "completed" {
		b.WriteString("    the job has not finished; the log may be partial\n")
	}
	scan, err := t.ci.ScanLog(ctx, l.file, ghaLogPatterns)
	if err != nil {
		fmt.Fprintf(&b, "    could not be scanned: %s\n", oneLine(err.Error(), 300))
		b.WriteString(untrustedClose)
		return b.String()
	}
	fmt.Fprintf(&b, "    %d lines; ", scan.Lines)
	if len(scan.Markers) > 0 {
		fmt.Fprintf(&b, "%d failure marker block(s):\n", len(scan.Markers))
	} else {
		b.WriteString("no failure markers found\n")
	}
	if scan.Note != "" {
		fmt.Fprintf(&b, "    %s\n", oneLine(scan.Note, 300))
	}
	for _, m := range scan.Markers {
		fmt.Fprintf(&b, "    [%s] at line %d:\n", m.Kind, m.Line)
		for i, line := range m.Lines {
			fmt.Fprintf(&b, "    %s:%d: %s\n", ghaLogName, m.Line+i, line)
		}
	}
	if len(scan.Markers) == 0 && scan.Lines > 0 {
		t.ghaTail(&b, l, scan.Lines)
	}
	b.WriteString(untrustedClose)
	return b.String()
}

// ghaTail writes the last lines of the first failing step (or of the log).
func (t *githubToolBox) ghaTail(b *strings.Builder, l *ghaLog, total int) {
	from, to, what := total-ghaTailLines+1, total, "the log"
	for _, s := range l.job.Steps {
		if r, ok := l.steps[s.Number]; ok && ghaStepFailed(s) {
			from, to, what = r.last-ghaTailLines+1, r.last, fmt.Sprintf("step %d", s.Number)
			if from < r.first {
				from = r.first
			}
			break
		}
	}
	from = max(from, 1)
	res, err := t.ci.ReadFile(l.file, from, to-from+1)
	if err != nil || len(res.Lines) == 0 {
		return
	}
	fmt.Fprintf(b, "    the end of %s:\n", what)
	for i, line := range res.Lines {
		fmt.Fprintf(b, "    %s:%d: %s\n", ghaLogName, res.First+i, oneLine(line, 300))
	}
}

// ghaSearch runs gha_search.
func (t *githubToolBox) ghaSearch(ctx context.Context, a ghaArgs) (string, error) {
	tg, err := t.ghaJob(ctx, a)
	if err != nil {
		return "", err
	}
	l, err := t.ghaOpen(ctx, tg.repo, *tg.job)
	if err != nil {
		return "", err
	}
	res, err := t.ci.SearchFile(ctx, l.file, prow.SearchOptions{
		Pattern: a.Pattern, Literal: a.Literal, IgnoreCase: a.IgnoreCase,
		MaxResults: a.MaxResults, ContextLines: a.ContextLines, Timeout: ciSearchTimeout,
	})
	if err != nil {
		return "", err
	}
	return formatCISearch("gha_search", ghaLabel(tg.repo, *tg.job), ghaLogName, res), nil
}

// ghaRead runs gha_read.
func (t *githubToolBox) ghaRead(ctx context.Context, a ghaArgs) (string, error) {
	tg, err := t.ghaJob(ctx, a)
	if err != nil {
		return "", err
	}
	l, err := t.ghaOpen(ctx, tg.repo, *tg.job)
	if err != nil {
		return "", err
	}
	start, n := a.StartLine, a.MaxLines
	step := a.Step
	if step == 0 && start == 0 {
		step = tg.step // the step a "#step:N" link named
	}
	if step > 0 && start == 0 {
		r, ok := l.steps[step]
		if !ok {
			return "", fmt.Errorf("step %d has no lines in the log (skipped, no output, or no such step); see gha_job_summary for the steps", step)
		}
		start = r.first
		if n == 0 {
			n = r.last - r.first + 1
		}
	}
	res, err := t.ci.ReadFile(l.file, start, n)
	if err != nil {
		return "", err
	}
	return formatCIRead("gha_read", ghaLabel(tg.repo, *tg.job), res), nil
}

// ghaURLTag reports whether a check URL is an Actions job or run link.
func ghaURLTag(u string) bool {
	_, err := github.ParseActionsURL(u)
	return err == nil
}
