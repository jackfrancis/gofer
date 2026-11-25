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
	// maxSnapshotAttempts bounds re-reading the diff while the comparison moves.
	maxSnapshotAttempts = 3
)

// PullDiff is a pull request's head commit, base branch, whether it is open, and its
// changed files with their patches, read for drafting review comments and for
// recording what a review read.
type PullDiff struct {
	HeadSHA string
	BaseRef string
	Open    bool
	Files   []DiffFile
	// Complete is true when every changed file was read: as many as the pull
	// request reports (changed_files), which GitHub lists only up to 3000.
	Complete bool
	// Settled is true when the pull request read again after its files showed the
	// same head, base branch, base commit and file count, so the files are the
	// comparison of that head with that base as it stood.
	Settled bool
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

// Unverifiable says why the diff cannot be identified by a digest, or "" when it
// can: the list was cut, the pull request moved while it was read, or a file other
// than a new one has no patch (a binary change, or one too large to show), since
// GitHub then names only its head blob, and a merge base that moved under it would
// leave the digest unchanged.
func (d PullDiff) Unverifiable() string {
	switch {
	case !d.Complete:
		return "this pull request has more files than gofer reads"
	case !d.Settled:
		return "the pull request changed while gofer read it"
	}
	for _, f := range d.Files {
		if f.Patch == "" && f.Status != "added" {
			return fmt.Sprintf("GitHub shows no diff for `%s` (a binary file, or a change too large to show), so gofer cannot tell which version it was compared against", f.Path)
		}
	}
	return ""
}

// Digest identifies the whole diff the pull request shows (DiffDigest of its
// files), or "" when it cannot be identified (Unverifiable): a diff gofer cannot
// identify is never vouched for.
func (d PullDiff) Digest() string {
	if d.Unverifiable() != "" {
		return ""
	}
	return DiffDigest(d.Files)
}

// ChangedPaths returns the changed files' paths, as many as a discussion lists.
func (d PullDiff) ChangedPaths() []string {
	var out []string
	for i, f := range d.Files {
		if i >= maxChangedFiles {
			break
		}
		out = append(out, f.Path)
	}
	return out
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

// pullRevision is what the pull request object says about the comparison.
type pullRevision struct {
	State string `json:"state"`
	Head  struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	ChangedFiles int `json:"changed_files"`
}

// same reports whether two reads of the pull request name the same comparison:
// head, base branch and its commit, and file count.
func (a pullRevision) same(b pullRevision) bool {
	return a.Head.SHA == b.Head.SHA && a.Base.Ref == b.Base.Ref && a.Base.SHA == b.Base.SHA && a.ChangedFiles == b.ChangedFiles
}

func (c *Client) readPullRevision(ctx context.Context, token, repo string, number int) (pullRevision, error) {
	body, err := c.getPull(ctx, token, repo, number)
	if err != nil {
		return pullRevision{}, err
	}
	var pr pullRevision
	if err := json.Unmarshal(body, &pr); err != nil {
		return pullRevision{}, err
	}
	if pr.Head.SHA == "" || pr.Base.Ref == "" {
		return pullRevision{}, errors.New("pull request has no head commit or base branch")
	}
	return pr, nil
}

// PullRevision reads repo's pull request number's head commit and base branch now.
func (c *Client) PullRevision(ctx context.Context, token, repo string, number int) (head, base string, err error) {
	repo, err = c.gate(ctx, token, repo)
	if err != nil {
		return "", "", err
	}
	pr, err := c.readPullRevision(ctx, token, repo, number)
	return pr.Head.SHA, pr.Base.Ref, err
}

// PullDiff reads repo's pull request number: its head commit, base branch, state and
// changed-file count, then its changed files, 100 a page, until it has that many
// (Complete), then the pull request again. The snapshot is Settled when the second
// read names the same comparison (head, base branch, base commit, file count): a
// stacked dependency merged into the base while the files were read changes a patch
// without moving the head, the base's name or the count, but it moves the base
// commit. If the comparison moved, the whole read is tried again, up to
// maxSnapshotAttempts, so an unrelated advance of the base does not cost the
// snapshot. A public repository only, as every call of this client (the PR answer
// is checked by getPull, the file pages by get). ghcomment.PendingReview reads an
// approved diff with the same pages and the same count.
func (c *Client) PullDiff(ctx context.Context, token, repo string, number int) (PullDiff, error) {
	repo, err := c.gate(ctx, token, repo)
	if err != nil {
		return PullDiff{}, err
	}
	pr, err := c.readPullRevision(ctx, token, repo, number)
	if err != nil {
		return PullDiff{}, err
	}
	var d PullDiff
	for attempt := 1; ; attempt++ {
		d = PullDiff{HeadSHA: pr.Head.SHA, BaseRef: pr.Base.Ref, Open: strings.EqualFold(pr.State, "open")}
		for page := 1; page <= maxDiffPages && len(d.Files) < pr.ChangedFiles; page++ {
			body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page))
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
			if len(files) < 100 {
				break
			}
		}
		d.Complete = len(d.Files) == pr.ChangedFiles
		again, err := c.readPullRevision(ctx, token, repo, number)
		if err != nil {
			return d, nil // unsettled
		}
		if d.Settled = again.same(pr); d.Settled || attempt == maxSnapshotAttempts {
			return d, nil
		}
		pr = again
	}
}
