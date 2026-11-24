package ghcomment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// This file is the pending review (ADR 0010): gofer creates a review with no event,
// which GitHub keeps PENDING, visible only to its author until they submit it on
// GitHub. It adds four calls to the client, on a public repository only:
//
//   - GET /repos/{repo}/pulls/{n}/reviews, to find a pending review already there;
//   - GET /repos/{repo}/pulls/{n}/files, to check the commented files' patches are
//     still the ones the comments were anchored to;
//   - GET /repos/{repo}/pulls/{n}, for its state, head commit and base branch, read
//     last, right before the write;
//   - POST /repos/{repo}/pulls/{n}/reviews, with no event.

// Bounds on a pending review.
const (
	// MaxReviewComments is the most inline comments one review may carry.
	MaxReviewComments = 50
	// maxReviewPages bounds how many pages (of 100) of reviews are read to look for
	// a pending one.
	maxReviewPages = 10
	// filesPerPage and maxFilePages bound the files read to check patches: GitHub
	// lists at most 3000 files. maxFilesPageBytes bounds one page, patches included.
	filesPerPage      = 50
	maxFilePages      = 60
	maxFilesPageBytes = 8 << 20
)

// Errors a caller distinguishes.
var (
	// ErrHeadMoved means the pull request changed under the draft: its head is no
	// longer the commit the comments are anchored to, its base branch is not the one
	// they were drafted against, or a commented file's patch is not the one they were
	// anchored to (a related change merged into the base moved the merge base). The
	// lines may mean something else now.
	ErrHeadMoved = errors.New("the pull request changed since the draft")
	// ErrNotOpen means the pull request is closed or merged.
	ErrNotOpen = errors.New("the pull request is not open")
	// ErrPendingExists means the user already has a pending review on the pull
	// request; GitHub allows one, and its REST API cannot add to it.
	ErrPendingExists = errors.New("a pending review already exists")
)

// ReviewComment is one inline comment of a pending review.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// Review is a pending review to create: the head commit and base branch it is
// anchored to, its summary and inline comments, and, for every commented file, the
// PatchDigest of the patch the comments were anchored to.
type Review struct {
	Commit   string
	BaseRef  string
	Body     string
	Comments []ReviewComment
	Patches  map[string]string // path -> PatchDigest
}

// PatchDigest identifies one file's patch as GitHub lists it on the pull request's
// files: hex SHA-256 of the patch text. The workload records it when it drafts
// (worklist.PatchDigest computes the same), and PendingReview compares it.
func PatchDigest(patch string) string {
	sum := sha256.Sum256([]byte(patch))
	return hex.EncodeToString(sum[:])
}

// PendingReview creates rv as a PENDING review on repo's pull request number. First
// it checks the repository is public and the user has no pending review there. Then
// it checks every commented file's patch is still the one its comments were anchored
// to, so a change merged into the base that moved the merge base refuses the review,
// while one that left these files' hunks alone does not. Last, as close to the write
// as the API allows, it checks the pull request is open with its head still
// rv.Commit and its base branch still rv.BaseRef. GitHub takes commit_id as an
// anchor, not a precondition (it accepts an older commit), so these checks are
// gofer's. It returns the review's web URL when GitHub gave one. A POST whose answer
// was lost, or that got a server error, is ErrOutcomeUnknown.
func (c *Client) PendingReview(ctx context.Context, token, repo string, number int, rv Review) (string, error) {
	if number <= 0 {
		return "", errors.New("github: no pull request number")
	}
	if rv.Commit == "" || rv.BaseRef == "" {
		return "", errors.New("github: no commit and base branch to anchor the review to")
	}
	if strings.TrimSpace(rv.Body) == "" && len(rv.Comments) == 0 {
		return "", errors.New("github: an empty review")
	}
	if len(rv.Body) > MaxBodyBytes || len(rv.Comments) > MaxReviewComments {
		return "", errors.New("github: the review is too large")
	}
	want := map[string]string{}
	for _, cm := range rv.Comments {
		if cm.Path == "" || cm.Line < 1 || (cm.Side != "RIGHT" && cm.Side != "LEFT") || cm.Body == "" || len(cm.Body) > MaxBodyBytes {
			return "", errors.New("github: an invalid review comment")
		}
		d := rv.Patches[cm.Path]
		if d == "" {
			return "", errors.New("github: a review comment on a file with no patch to check")
		}
		want[cm.Path] = d
	}
	canon, err := c.PublicRepo(ctx, token, repo)
	if err != nil {
		return "", err
	}
	pull := "/repos/" + canon + "/pulls/" + strconv.Itoa(number)
	pending, err := c.hasPendingReview(ctx, token, pull)
	if err != nil {
		return "", err
	}
	if pending {
		return "", ErrPendingExists
	}
	if err := c.checkPatches(ctx, token, pull, want); err != nil {
		return "", err
	}
	// The pull request is read after the lists are paged, right before the POST, so
	// its state, head and base are as fresh as the API lets gofer check. A push since
	// the files were read shows here as a new head.
	raw, err := c.do(ctx, token, http.MethodGet, pull, nil, http.StatusOK)
	if err != nil {
		return "", err
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
	if err := json.Unmarshal(raw, &pr); err != nil {
		return "", errors.New("github: unreadable pull request")
	}
	if !strings.EqualFold(pr.State, "open") {
		return "", ErrNotOpen
	}
	if !strings.EqualFold(pr.Head.SHA, rv.Commit) || pr.Base.Ref != rv.BaseRef {
		return "", ErrHeadMoved
	}
	// No "event": GitHub creates the review PENDING.
	payload, err := json.Marshal(struct {
		CommitID string          `json:"commit_id"`
		Body     string          `json:"body,omitempty"`
		Comments []ReviewComment `json:"comments,omitempty"`
	}{rv.Commit, rv.Body, rv.Comments})
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, token, http.MethodPost, pull+"/reviews", payload, http.StatusOK)
	if err != nil && !errors.Is(err, errBodyLost) {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
	}
	_ = json.Unmarshal(resp, &out)
	if u, perr := url.Parse(out.HTMLURL); perr != nil || u.Scheme != "https" {
		out.HTMLURL = ""
	}
	if out.State != "" && !strings.EqualFold(out.State, "PENDING") {
		// GitHub never submits a review without an event; say so if it ever did.
		return out.HTMLURL, fmt.Errorf("github: the review was created %s, not pending", strings.ToLower(out.State))
	}
	return out.HTMLURL, nil
}

// checkPatches pages the pull request's files until every path in want is found,
// and refuses (ErrHeadMoved) if one is gone from the diff, has no patch now, or has a
// patch whose PatchDigest differs. A list too long to read whole is an error, never a
// pass.
func (c *Client) checkPatches(ctx context.Context, token, pull string, want map[string]string) error {
	if len(want) == 0 {
		return nil
	}
	left := len(want)
	for page := 1; page <= maxFilePages; page++ {
		raw, err := c.doLimit(ctx, token, http.MethodGet, pull+"/files?per_page="+strconv.Itoa(filesPerPage)+"&page="+strconv.Itoa(page), nil, http.StatusOK, maxFilesPageBytes)
		if err != nil {
			return err
		}
		var files []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		if err := json.Unmarshal(raw, &files); err != nil {
			return errors.New("github: unreadable file list")
		}
		for _, f := range files {
			d, ok := want[f.Filename]
			if !ok {
				continue
			}
			if f.Patch == "" || PatchDigest(f.Patch) != d {
				return ErrHeadMoved
			}
			delete(want, f.Filename)
			left--
		}
		if left == 0 {
			return nil
		}
		if len(files) < filesPerPage {
			return ErrHeadMoved // a commented file is no longer in the diff
		}
	}
	return errors.New("github: too many files to check the commented ones")
}

// hasPendingReview reports whether the pull request (its API path) has a pending
// review. GitHub lists a pending review only to its author, so any one is the
// user's own.
func (c *Client) hasPendingReview(ctx context.Context, token, pull string) (bool, error) {
	for page := 1; page <= maxReviewPages; page++ {
		raw, err := c.do(ctx, token, http.MethodGet, pull+"/reviews?per_page=100&page="+strconv.Itoa(page), nil, http.StatusOK)
		if err != nil {
			return false, err
		}
		var reviews []struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &reviews); err != nil {
			return false, errors.New("github: unreadable review list")
		}
		for _, r := range reviews {
			if strings.EqualFold(r.State, "PENDING") {
				return true, nil
			}
		}
		if len(reviews) < 100 {
			return false, nil
		}
	}
	// Too many reviews to tell: refuse rather than risk GitHub's refusal halfway.
	return false, errors.New("github: too many reviews to check for a pending one")
}
