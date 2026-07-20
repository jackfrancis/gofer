// Read-only GitHub tools for the assistive conversation. The runtime injects
// these behind the neutral worklist.ToolBox seam so the model can look up live
// data — a file at a ref, a PR/issue's state, a search — while gofer core imports
// no provider client. Every tool only ever reads, with the user's vended
// credential; access is bounded by that token's own scopes, and reach spans any
// repository the token can see.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// githubToolBox implements worklist.ToolBox with read-only GitHub operations.
type githubToolBox struct {
	gh          *github.Client
	token       string
	defaultRepo string // the item's repo, used when a call omits "repo"
}

var _ worklist.ToolBox = (*githubToolBox)(nil)

// newGitHubToolBox builds a toolbox over a GitHub client and the user's vended
// token. defaultRepo is the conversation item's repo, used when a tool call
// omits an explicit repo.
func newGitHubToolBox(gh *github.Client, token, defaultRepo string) *githubToolBox {
	return &githubToolBox{gh: gh, token: token, defaultRepo: defaultRepo}
}

// JSON Schemas for each tool's arguments. repo defaults to the item's repo, so
// it is optional; the model can target any repository it has access to.
var (
	schemaReadFile = json.RawMessage(`{"type":"object","properties":{` +
		`"repo":{"type":"string","description":"owner/name; defaults to the current item's repository"},` +
		`"path":{"type":"string","description":"file path within the repository, e.g. cluster-autoscaler/go.mod"},` +
		`"ref":{"type":"string","description":"branch, tag, or commit SHA; defaults to the repository's default branch"},` +
		`"offset":{"type":"integer","description":"byte offset to start reading from; pass the offset reported by a previous read to page through a file larger than one window (default 0)"}` +
		`},"required":["path"]}`)

	schemaGetPR = json.RawMessage(`{"type":"object","properties":{` +
		`"repo":{"type":"string","description":"owner/name; defaults to the current item's repository"},` +
		`"number":{"type":"integer","description":"pull request number"}` +
		`},"required":["number"]}`)

	schemaGetIssue = json.RawMessage(`{"type":"object","properties":{` +
		`"repo":{"type":"string","description":"owner/name; defaults to the current item's repository"},` +
		`"number":{"type":"integer","description":"issue number"}` +
		`},"required":["number"]}`)

	schemaSearch = json.RawMessage(`{"type":"object","properties":{` +
		`"query":{"type":"string","description":"GitHub issues/PRs search query, e.g. 'repo:kubernetes/autoscaler otel in:title' or 'org:kubernetes CVE-2026-24051'"}` +
		`},"required":["query"]}`)
)

// Definitions advertises the read-only GitHub tools to the model.
func (t *githubToolBox) Definitions() []worklist.ToolDef {
	return []worklist.ToolDef{
		{
			Name:        "github_read_file",
			Description: "Read a file's contents from a GitHub repository at a given ref (branch, tag, or commit SHA). Use to check current dependency versions, config, or code on a branch such as master/main — e.g. whether go.mod already bumped a dependency. Returns up to a 32 KB window; if the file is larger, the result reports the file size and a follow-up offset — call again with that offset to read further. Generated and vendored files (zz_generated.*, *.pb.go, anything under vendor/ or testdata/, and lockfiles) are large and low-signal for triage — read them only if the user specifically asks.",
			Parameters:  schemaReadFile,
		},
		{
			Name:        "github_get_pull_request",
			Description: "Get a pull request's current state: open or closed, merged or not (and when), title, base/head branches, and the head commit SHA — read its changed files at that SHA (a PR branch may live on a fork and not resolve in the base repo).",
			Parameters:  schemaGetPR,
		},
		{
			Name:        "github_get_issue",
			Description: "Get an issue's current state: open or closed, title, and when it was closed.",
			Parameters:  schemaGetIssue,
		},
		{
			Name:        "github_search",
			Description: "Search GitHub issues and pull requests with the GitHub search syntax. Use to find whether another PR or commit already addressed something (e.g. a dependency bump or a CVE). Returns the top matches.",
			Parameters:  schemaSearch,
		},
	}
}

// Invoke executes a named tool with JSON arguments and returns a text result for
// the model. Unknown tools and malformed arguments return an error, which the
// converser relays to the model as a tool error.
func (t *githubToolBox) Invoke(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "github_read_file":
		var a struct {
			Repo   string `json:"repo"`
			Path   string `json:"path"`
			Ref    string `json:"ref"`
			Offset int    `json:"offset"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		return t.gh.FileContents(ctx, t.token, t.repoOr(a.Repo), a.Path, a.Ref, a.Offset)

	case "github_get_pull_request":
		var a struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.Number == 0 {
			return "", fmt.Errorf("number is required")
		}
		return t.gh.PullRequestStatus(ctx, t.token, t.repoOr(a.Repo), a.Number)

	case "github_get_issue":
		var a struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.Number == 0 {
			return "", fmt.Errorf("number is required")
		}
		return t.gh.IssueStatus(ctx, t.token, t.repoOr(a.Repo), a.Number)

	case "github_search":
		var a struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		return t.gh.Search(ctx, t.token, a.Query)

	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func (t *githubToolBox) repoOr(r string) string {
	if r != "" {
		return r
	}
	return t.defaultRepo
}
