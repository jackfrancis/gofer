package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Bounds on a pull request diff read for a draft review (ADR 0010).
const (
	// maxDiffPages bounds the files pages read (100 files each). GitHub itself lists
	// at most 3000 files.
	maxDiffPages = 30
)

// PullDiff is a pull request's head commit, base branch, whether it is open, and its
// changed files with their patches, read for drafting review comments.
type PullDiff struct {
	HeadSHA string
	BaseRef string
	Open    bool
	Files   []DiffFile
	// Complete is false when the file list was cut at maxDiffPages.
	Complete bool
}

// DiffFile is one changed file. Patch is GitHub's unified-diff hunks for it, empty
// when GitHub sends none (a binary file, or a patch too large to show). SHA is the
// blob GitHub names for it, and PreviousPath its name before a rename.
type DiffFile struct {
	Path         string
	PreviousPath string
	Status       string
	SHA          string
	Patch        string
}

// Digest identifies the whole diff the pull request shows (DiffDigest of its
// files), or "" when the file list was cut (Complete is false): a diff gofer did not
// read whole is never vouched for.
func (d PullDiff) Digest() string {
	if !d.Complete {
		return ""
	}
	return DiffDigest(d.Files)
}

// DiffDigest identifies a pull request's diff as GitHub lists its files: hex SHA-256
// over the files sorted by path, one JSON line each of [path, previous path, status,
// blob SHA, SHA-256 of the patch]. It changes when the comparison does, as when a
// related change merged into the base moves the merge base, and not when the base
// branch merely advances. ghcomment.DiffDigest computes the same over the same list,
// so the web tier can check that an approval's diff is still the one reviewed.
func DiffDigest(files []DiffFile) string {
	sorted := append([]DiffFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		patch := sha256.Sum256([]byte(f.Patch))
		line, _ := json.Marshal([]string{f.Path, f.PreviousPath, f.Status, f.SHA, hex.EncodeToString(patch[:])})
		h.Write(append(line, '\n'))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PullDiff reads repo's pull request number: its head commit and state, then its
// changed files, page by page. A public repository only, as every call of this
// client (the PR answer is checked by getPull, the file pages by get).
func (c *Client) PullDiff(ctx context.Context, token, repo string, number int) (PullDiff, error) {
	repo, err := c.gate(ctx, token, repo)
	if err != nil {
		return PullDiff{}, err
	}
	body, err := c.getPull(ctx, token, repo, number)
	if err != nil {
		return PullDiff{}, err
	}
	var pr struct {
		State string `json:"state"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return PullDiff{}, err
	}
	if pr.Head.SHA == "" || pr.Base.Ref == "" {
		return PullDiff{}, errors.New("pull request has no head commit or base branch")
	}
	d := PullDiff{HeadSHA: pr.Head.SHA, BaseRef: pr.Base.Ref, Open: strings.EqualFold(pr.State, "open")}
	for page := 1; page <= maxDiffPages; page++ {
		body, hdr, err := c.getHeader(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page))
		if err != nil {
			return PullDiff{}, err
		}
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
			Status           string `json:"status"`
			SHA              string `json:"sha"`
			Patch            string `json:"patch"`
		}
		if err := json.Unmarshal(body, &files); err != nil {
			return PullDiff{}, err
		}
		for _, f := range files {
			d.Files = append(d.Files, DiffFile{Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status, SHA: f.SHA, Patch: f.Patch})
		}
		if !hasNextPage(hdr) {
			d.Complete = true
			return d, nil
		}
	}
	return d, nil
}
