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
	"errors"
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
	// maxGHAJobs bounds the distinct Actions jobs whose logs one run fetches (the Prow
	// tools allow as many Prow jobs). A job counts once its fetch is attempted.
	maxGHAJobs = 6
	// maxGHAJobsListed bounds the jobs a run or pull request overview lists. The
	// overview puts the first failing job's diagnosis ahead of its list, so a wide
	// failing matrix cannot crowd it out of the result.
	maxGHAJobsListed = 40
	// ghaTailLines is how much of a failing step's end a summary shows when the log has
	// no failure marker.
	ghaTailLines = 40
	// ghaLogName names a job log in search and read results.
	ghaLogName = "log"
	// ghaApp is the check-run app slug of GitHub Actions.
	ghaApp = "github-actions"
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
	mu       sync.Mutex
	jobs     map[string]*ghaLog // fetched, by "repo/job id"
	reserved map[string]bool    // every job a fetch was attempted for: the per-answer limit
}

// ghaLog is one job's fetched log, with where each step's lines sit in it.
type ghaLog struct {
	repo  string
	job   github.ActionsJob
	file  *prow.File
	steps map[int]stepLines // by step number; absent when a step has no lines
}

type lineRange struct{ first, last int }

// stepLines is where a step's output sits in the job log, by the runner's
// timestamps. Steps that started in the same second, or ran at the same time (a
// background step), cannot be told apart that way: each carries the range its time
// gave it (zero when none) and names the steps its lines may be mixed with.
type stepLines struct {
	lineRange
	shared []int // the other steps whose lines may be in this range
}

func (s stepLines) ambiguous() bool { return len(s.shared) > 0 }

var (
	ghaTargetProps = `"url":{"type":"string","description":"a GitHub Actions link exactly as a check or page shows it: https://github.com/<owner>/<repo>/actions/runs/<run>/job/<job> (a job), …/actions/runs/<run> (a run), or a GitHub Actions check run's https://github.com/<owner>/<repo>/runs/<id>"},` +
		`"repo":{"type":"string","description":"owner/name, with job_id or run_id instead of url; defaults to the current item's repository"},` +
		`"job_id":{"type":"integer","description":"instead of url: an Actions job ID (the same number as its check run ID)"},`

	schemaGHASummary = json.RawMessage(`{"type":"object","properties":{` + ghaTargetProps +
		`"run_id":{"type":"integer","description":"instead of url: a workflow run ID; summarises its first failing job and lists its jobs"},` +
		`"number":{"type":"integer","description":"instead of url: a pull request number; summarises the first failing GitHub Actions check on its head commit and lists the others"}` +
		`}}`)

	schemaGHASearch = json.RawMessage(`{"type":"object","properties":{` + ghaTargetProps +
		`"pattern":{"type":"string","description":"an RE2 regular expression (e.g. 'FAIL|panic:|Error:'), or an exact string when literal is true"},` +
		`"literal":{"type":"boolean","description":"treat pattern as an exact string (default false)"},` +
		`"ignore_case":{"type":"boolean","description":"case-insensitive match (default false)"},` +
		`"context_lines":{"type":"integer","description":"lines of context before and after each match, 0-5 (default 0)"},` +
		`"max_results":{"type":"integer","description":"matching lines to return, up to 200 (default 60)"}` +
		`},"required":["pattern"]}`)

	schemaGHARead = json.RawMessage(`{"type":"object","properties":{` + ghaTargetProps +
		`"step":{"type":"integer","description":"a step number (from gha_job_summary): read that step. Its lines in the job log, from its first; or, when the job log cannot place them (steps that started in the same second or ran at the same time), the step's own log (step-N.log, numbered from 1), in which start_line then counts"},` +
		`"start_line":{"type":"integer","description":"first line, 1-based (default 1, or the step's first line); negative counts from the end, e.g. -200 reads the last 200 lines"},` +
		`"max_lines":{"type":"integer","description":"lines to return, up to 1500 (default 400, or the whole step)"}` +
		`}}`)
)

// ghaToolDefs advertises the Actions log tools.
func ghaToolDefs() []worklist.ToolDef {
	return []worklist.ToolDef{
		{
			Name:        "gha_job_summary",
			Description: "Summarise a GitHub Actions job from its URL (a failing check's details link, https://github.com/<owner>/<repo>/actions/runs/<run>/job/<job>): its result, the workflow's head commit (what the run was triggered on; the commit actually tested is whatever the workflow checked out — a pull_request run tests a merge commit — which its checkout step's log shows), every step with its result and approximate log line range, and the places in the job's log that show the failure (the runner's ##[error] lines with the output just before them, go test FAIL lines, panics, make errors), or the end of the failing step when there are none. A run URL summarises the run's first failing job and lists its jobs; a pull request number does the same for the failing Actions checks on its head commit. Start here for any question about why a GitHub Actions job failed, then follow up with gha_search or gha_read.",
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

// ghaCheckRef returns the Actions job (or run) a check links to, when the check is
// GitHub Actions'. A /runs/<id> or /commit/<sha>/checks/<id> link is the generic
// check-run page GitHub gives every app's check, so it counts only for a check by
// the github-actions app; an /actions/runs/… link is Actions' own.
func ghaCheckRef(c github.CICheck) (github.ActionsRef, bool) {
	if c.Kind != "check" {
		return github.ActionsRef{}, false
	}
	ref, err := github.ParseActionsURL(c.URL)
	if err != nil || (ref.RunID == 0 && c.App != ghaApp) {
		return github.ActionsRef{}, false
	}
	return ref, true
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

// ghaResolve turns url / repo+job_id / repo+run_id into a target.
func (t *githubToolBox) ghaResolve(ctx context.Context, a ghaArgs) (ghaTarget, error) {
	var tg ghaTarget
	var ref github.ActionsRef
	generic := false // a generic check-run link: maybe not an Actions job at all
	switch {
	case strings.TrimSpace(a.URL) != "":
		r, err := github.ParseActionsURL(a.URL)
		if err != nil {
			if _, _, perr := prow.ParseJobURL(a.URL); perr == nil {
				return tg, fmt.Errorf("that is a Prow job URL; open it with ci_job_summary")
			}
			return tg, err
		}
		ref, generic = r, r.RunID == 0
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
			if generic {
				err = fmt.Errorf("%w (a /runs/<id> link can also be another app's check, which has no Actions log; github_get_ci_status marks Actions checks [actions])", err)
			}
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

// stripTimestamp is a LineHead that drops the runner's timestamp.
func stripTimestamp(_ int, head []byte) int {
	if loc := ghaTimestamp.FindIndex(head); loc != nil {
		return loc[1]
	}
	return 0
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
	if t.gha.reserved == nil {
		t.gha.reserved = map[string]bool{}
	}
	prev, seen := t.gha.jobs[key]
	if seen && prev.job.Status == "completed" {
		return prev, nil
	}
	// The slot is taken when the fetch is attempted, whatever its outcome: a failed,
	// oversized or binary download spent budget too. A retry reuses it.
	if !t.gha.reserved[key] {
		if len(t.gha.reserved) >= maxGHAJobs {
			return nil, fmt.Errorf("at most %d GitHub Actions job logs per answer; use the ones already opened", maxGHAJobs)
		}
		t.gha.reserved[key] = true
	}

	mapper := newStepMapper(job.Steps)
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
	l := &ghaLog{repo: repo, job: job, file: f, steps: mapper.result()}
	t.gha.jobs[key] = l
	return l, nil
}

// ghaStepLog fetches one step's own log (its lines alone, numbered from 1). It is
// how a step is read when the job log cannot place it. It counts against the
// session's file and byte budgets, under its job's slot.
func (t *githubToolBox) ghaStepLog(ctx context.Context, l *ghaLog, step int) (*prow.File, error) {
	fetch := func(ctx context.Context, w io.Writer, max int64) (int64, error) {
		return t.gh.ActionsStepLog(ctx, t.token, l.repo, l.job.ID, step, w, max)
	}
	fkey := fmt.Sprintf("gha:%s/%d/step/%d/%s", l.repo, l.job.ID, step, l.job.Status)
	f, err := t.ci.FetchWith(ctx, fkey, fmt.Sprintf("step-%d.log", step), fetch, prow.FetchOptions{LineHead: stripTimestamp})
	if err != nil {
		return nil, err
	}
	if f.Binary {
		return nil, fmt.Errorf("the log of step %d is binary content", step)
	}
	return f, nil
}

// stepMapper assigns log lines to steps by time: the runner stamps every line, and
// the API gives each step's start and end (to the second). A line belongs to the
// latest step start at or before it; unstamped lines belong with the line before.
// Steps that started in the same second form one group, whose lines cannot be split
// between them, and a step that overlaps another in time (a background step runs
// alongside the steps after it) has its lines mixed into theirs. Both are reported
// as such, never as a clean range. The ranges are approximate at a boundary within
// one second.
type stepMapper struct {
	order  []github.ActionsStep // steps that ran, by start
	groups [][]int              // step numbers, by group, groups in start order
	starts []time.Time          // each group's start second
	ranges []lineRange
	cur    int
}

func newStepMapper(steps []github.ActionsStep) *stepMapper {
	var order []github.ActionsStep
	for _, s := range steps {
		if s.StartedAt.IsZero() || s.Conclusion == "skipped" {
			continue
		}
		order = append(order, s)
	}
	sec := func(s github.ActionsStep) time.Time { return s.StartedAt.Truncate(time.Second) }
	sort.SliceStable(order, func(i, k int) bool {
		if a, b := sec(order[i]), sec(order[k]); !a.Equal(b) {
			return a.Before(b)
		}
		return order[i].Number < order[k].Number
	})
	m := &stepMapper{cur: -1, order: order}
	for _, s := range order {
		if n := len(m.starts); n > 0 && m.starts[n-1].Equal(sec(s)) {
			m.groups[n-1] = append(m.groups[n-1], s.Number)
			continue
		}
		m.groups = append(m.groups, []int{s.Number})
		m.starts = append(m.starts, sec(s))
	}
	m.ranges = make([]lineRange, len(m.groups))
	return m
}

func (m *stepMapper) lineHead(num int, head []byte) int {
	drop := 0
	if loc := ghaTimestamp.FindSubmatchIndex(head); loc != nil {
		drop = loc[1]
		if ts, err := time.Parse(time.RFC3339Nano, string(head[loc[2]:loc[3]])); err == nil {
			s := ts.Truncate(time.Second)
			for m.cur+1 < len(m.starts) && !s.Before(m.starts[m.cur+1]) {
				m.cur++
			}
		}
	}
	if m.cur >= 0 {
		r := &m.ranges[m.cur]
		if r.first == 0 {
			r.first = num
		}
		r.last = num
	}
	return drop
}

// result is each step's lines, by step number. A step whose lines may be mixed with
// others' (same start second, or overlapping in time) names them; a step that
// overlaps others but was given no lines is listed with a zero range.
func (m *stepMapper) result() map[int]stepLines {
	mixed := map[int]map[int]bool{}
	link := func(a, b int) {
		if mixed[a] == nil {
			mixed[a] = map[int]bool{}
		}
		mixed[a][b] = true
	}
	for _, nums := range m.groups {
		for _, a := range nums {
			for _, b := range nums {
				if a != b {
					link(a, b)
				}
			}
		}
	}
	// Overlap in time: a later step that starts before an earlier one has ended (in
	// whole seconds, so a step starting in the second the previous one ended is not
	// counted). An earlier step that has not ended overlaps everything after it.
	sec := func(t time.Time) time.Time { return t.Truncate(time.Second) }
	for i, a := range m.order {
		for _, b := range m.order[i+1:] {
			if a.CompletedAt.IsZero() || sec(b.StartedAt).Before(sec(a.CompletedAt)) {
				link(a.Number, b.Number)
				link(b.Number, a.Number)
			}
		}
	}
	others := func(n int) []int {
		var out []int
		for o := range mixed[n] {
			out = append(out, o)
		}
		sort.Ints(out)
		return out
	}
	out := map[int]stepLines{}
	for g, nums := range m.groups {
		r := m.ranges[g]
		for _, n := range nums {
			if r.first == 0 && len(mixed[n]) == 0 {
				continue
			}
			out[n] = stepLines{lineRange: r, shared: others(n)}
		}
	}
	return out
}

// tooLarge reports a log over the per-file or per-run byte budget.
func tooLarge(err error) bool {
	return errors.Is(err, github.ErrLogTooLarge) || errors.Is(err, prow.ErrTooLarge)
}

// checkStep reports a step that cannot have a log.
func checkStep(job github.ActionsJob, n int) error {
	s, ok := stepOf(job, n)
	switch {
	case !ok:
		return fmt.Errorf("job %d has no step %d; see gha_job_summary for its steps", job.ID, n)
	case s.Conclusion == "skipped" || s.StartedAt.IsZero():
		return fmt.Errorf("step %d did not run (%s), so it has no log", n, oneLine(ghaState(s.Status, s.Conclusion), 40))
	}
	return nil
}

func stepOf(job github.ActionsJob, n int) (github.ActionsStep, bool) {
	for _, s := range job.Steps {
		if s.Number == n {
			return s, true
		}
	}
	return github.ActionsStep{}, false
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
	head, list := formatGHARun(tg)
	for _, j := range tg.jobs {
		if j.Failing() {
			head += "\nThe first failing job:\n" + t.ghaJobSummary(ctx, tg.repo, j) + "\n\n"
			break
		}
	}
	return head + list, nil
}

// ghaSummaryForPR summarises the first failing Actions check on a pull request's
// head commit and lists the others.
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
		ref, ok := ghaCheckRef(c)
		if !ok || ref.JobID == 0 {
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
	b.WriteString("\n")
	first := fails[0].ref
	if job, err := t.gh.ActionsJob(ctx, t.token, first.Repo, first.JobID); err != nil {
		fmt.Fprintf(&b, "\nThe first failing job (%d) could not be read: %s\n", first.JobID, oneLine(err.Error(), 300))
	} else {
		b.WriteString("\nThe first failing job:\n" + t.ghaJobSummary(ctx, first.Repo, job) + "\n")
	}
	b.WriteString("\nFailing GitHub Actions jobs on this pull request:\n" + untrustedOpen)
	for i, f := range fails {
		if i >= maxGHAJobsListed {
			fmt.Fprintf(&b, "  … and %d more\n", len(fails)-i)
			break
		}
		fmt.Fprintf(&b, "  failing  %s (job %d: %s)\n", oneLine(f.check.Name, 200), f.ref.JobID, oneLine(f.check.State, 40))
		fmt.Fprintf(&b, "           %s\n", oneLine(f.check.URL, 500))
	}
	b.WriteString(untrustedClose)
	return b.String(), nil
}

// formatGHARun renders a run's header (counts) and its job list, separately, so the
// first failing job's diagnosis can go between them.
func formatGHARun(tg ghaTarget) (head, list string) {
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
	var h strings.Builder
	fmt.Fprintf(&h, "gha_job_summary for Actions run %d in %s: %d jobs, %d failing, %d pending, %d passing.\n", tg.run, tg.repo, len(tg.jobs), failing, pending, passing)
	if !tg.whole {
		h.WriteString("Partial: the run holds more jobs than were read.\n")
	}
	var b strings.Builder
	b.WriteString("Jobs of this run (open one with gha_job_summary on its job URL or job_id):\n" + untrustedOpen)
	for i, j := range tg.jobs {
		if i >= maxGHAJobsListed {
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
	return h.String(), b.String()
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

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ", ")
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
		fmt.Fprintf(&b, "    workflow head: %s @%s (what the run was triggered on, not necessarily the commit tested: a pull_request run tests a merge commit, and the checkout step's log shows what was checked out)\n",
			oneLine(job.HeadBranch, 200), shortSHA(job.HeadSHA))
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
				switch {
				case r.ambiguous() && r.first == 0:
					fmt.Fprintf(&b, " (ran at the same time as step(s) %s, whose log lines its output is mixed into; gha_read step=%d reads its own log)", joinInts(r.shared), s.Number)
				case r.ambiguous():
					fmt.Fprintf(&b, " (≈ log lines %d-%d, mixed with step(s) %s, which started the same second or ran at the same time; gha_read step=%d reads its own log)", r.first, r.last, joinInts(r.shared), s.Number)
				default:
					fmt.Fprintf(&b, " (≈ log lines %d-%d)", r.first, r.last)
				}
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("  log:\n")
	if logErr != nil {
		fmt.Fprintf(&b, "    could not be read: %s\n", oneLine(logErr.Error(), 300))
		if tooLarge(logErr) {
			// The failing step's own log is usually far smaller than the job's.
			for _, s := range job.Steps {
				if ghaStepFailed(s) && checkStep(job, s.Number) == nil {
					t.ghaStepTail(ctx, &b, &ghaLog{repo: repo, job: job}, s.Number, "the job log is too large to fetch")
					break
				}
			}
		}
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
		t.ghaTail(ctx, &b, l, scan.Lines)
	}
	b.WriteString(untrustedClose)
	return b.String()
}

// ghaTail writes the last lines of the first failing step: from the job log when
// it places the step, from the step's own log when it cannot, and the end of the
// job log when there is no failing step (or its own log cannot be read).
func (t *githubToolBox) ghaTail(ctx context.Context, b *strings.Builder, l *ghaLog, total int) {
	for _, s := range l.job.Steps {
		if !ghaStepFailed(s) {
			continue
		}
		if r, ok := l.steps[s.Number]; ok && !r.ambiguous() {
			from := max(r.last-ghaTailLines+1, r.first, 1)
			ghaWriteLines(t, b, l.file, ghaLogName, from, r.last-from+1, fmt.Sprintf("the end of step %d", s.Number))
			return
		}
		if t.ghaStepTail(ctx, b, l, s.Number, "the job log cannot place it") {
			return
		}
		break
	}
	ghaWriteLines(t, b, l.file, ghaLogName, max(total-ghaTailLines+1, 1), ghaTailLines, "the end of the log")
}

// ghaStepTail writes the end of one step's own log, saying why it was used; false
// when that log could not be read (the reason is written instead).
func (t *githubToolBox) ghaStepTail(ctx context.Context, b *strings.Builder, l *ghaLog, step int, why string) bool {
	f, err := t.ghaStepLog(ctx, l, step)
	if err != nil {
		fmt.Fprintf(b, "    step %d's own log could not be read: %s\n", step, oneLine(err.Error(), 300))
		return false
	}
	ghaWriteLines(t, b, f, f.Rel, -ghaTailLines, ghaTailLines,
		fmt.Sprintf("the end of step %d, from its own log (%s, numbered within the step; %s)", step, f.Rel, why))
	return true
}

// ghaWriteLines writes a window of f as name:line: text lines under a heading.
func ghaWriteLines(t *githubToolBox, b *strings.Builder, f *prow.File, name string, from, n int, heading string) {
	res, err := t.ci.ReadFile(f, from, n)
	if err != nil || len(res.Lines) == 0 {
		return
	}
	fmt.Fprintf(b, "    %s:\n", heading)
	for i, line := range res.Lines {
		fmt.Fprintf(b, "    %s:%d: %s\n", name, res.First+i, oneLine(line, 300))
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
	label := ghaLabel(tg.repo, *tg.job)
	start, n := a.StartLine, a.MaxLines
	step := a.Step
	if step == 0 && start == 0 {
		step = tg.step // the step a "#step:N" link named
	}
	if step > 0 {
		if err := checkStep(*tg.job, step); err != nil {
			return "", err
		}
	}
	l, err := t.ghaOpen(ctx, tg.repo, *tg.job)
	if err != nil {
		if step > 0 && tooLarge(err) {
			// The step's own log may fit where the whole job's does not.
			return t.ghaReadStepLog(ctx, &ghaLog{repo: tg.repo, job: *tg.job}, label, step, start, n, "the job log is too large to fetch")
		}
		return "", err
	}
	stepEnd := 0
	if step > 0 {
		r, mapped := l.steps[step]
		if !mapped || r.ambiguous() {
			return t.ghaReadStepLog(ctx, l, label, step, start, n, "the job log cannot place its lines")
		}
		if start == 0 {
			// The step's window: from its first line, never past its last.
			length := r.last - r.first + 1
			start, stepEnd = r.first, r.last
			if n <= 0 || n > length {
				n = length
			}
		}
	}
	res, err := t.ci.ReadFile(l.file, start, n)
	if err != nil {
		return "", err
	}
	out := formatCIRead("gha_read", label, res)
	if stepEnd > 0 {
		if res.First+len(res.Lines)-1 >= stepEnd {
			res.More = false // the step is read to its end; what follows is another step
			out = formatCIRead("gha_read", label, res)
		}
		r := l.steps[step]
		out = fmt.Sprintf("Step %d is log lines %d-%d.\n", step, r.first, r.last) + out
	}
	return out, nil
}

// ghaReadStepLog reads a window of one step's own log, in which start counts.
func (t *githubToolBox) ghaReadStepLog(ctx context.Context, l *ghaLog, label string, step, start, n int, why string) (string, error) {
	f, err := t.ghaStepLog(ctx, l, step)
	if err != nil {
		return "", err
	}
	res, err := t.ci.ReadFile(f, start, n)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Step %d's own log (%s); page it with step=%d and start_line.\n", step, why, step) +
		formatCIRead("gha_read", label, res), nil
}
