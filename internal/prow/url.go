// Package prow reads the public logs and artifacts of a Prow CI job, so the
// conversation tools can say why a job failed. It is workload-only (imported by
// internal/agent, never by gofer core) and holds no credential: Prow job artifacts
// are public objects in Google Cloud Storage, read anonymously.
//
// The runtime's egress is open, so this package is deliberately narrow: it accepts
// only job URLs in an allowlisted bucket, builds every fetch URL itself, and talks to
// exactly one host. See docs/adr/0005-prow-ci-log-tools.md.
package prow

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// allowedBuckets are the GCS buckets whose job artifacts may be read. Adding one is a
// code change and a review, not configuration.
var allowedBuckets = map[string]bool{
	"kubernetes-ci-logs": true, // prow.k8s.io, current
	"kubernetes-jenkins": true, // prow.k8s.io, older jobs
}

// AcceptedForms describes the job URLs ParseJobURL accepts, for error messages.
const AcceptedForms = "https://prow.k8s.io/view/gs/<bucket>/<job path>, https://gcsweb.k8s.io/gcs/<bucket>/<job path>, https://storage.googleapis.com/<bucket>/<job path>, or gs://<bucket>/<job path>, in the kubernetes-ci-logs or kubernetes-jenkins bucket"

// ErrUnsupportedURL reports a URL that is not an allowlisted Prow job.
var ErrUnsupportedURL = errors.New("not a supported Prow job URL")

// Job identifies one Prow job run: its bucket and the object prefix of its run
// directory, without a trailing slash (e.g.
// "pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/6825/<job>/<build id>").
type Job struct {
	Bucket string
	Path   string
}

// Key is a stable identity for the job.
func (j Job) Key() string { return j.Bucket + "/" + j.Path }

// URL is the job's Prow page, rebuilt from the parsed parts (never the caller's text).
func (j Job) URL() string { return "https://prow.k8s.io/view/gs/" + j.Bucket + "/" + j.Path }

// Name is the job's name: the segment before the build ID.
func (j Job) Name() string {
	segs := strings.Split(j.Path, "/")
	if len(segs) < 2 {
		return ""
	}
	return segs[len(segs)-2]
}

// BuildID is the run's build ID: the last segment.
func (j Job) BuildID() string { return path.Base(j.Path) }

// Object returns the bucket object name of rel, a path inside the job directory.
// rel "" is the job directory itself (returned with a trailing slash).
func (j Job) Object(rel string) (string, error) {
	r, err := CleanRel(rel)
	if err != nil {
		return "", err
	}
	if r == "" {
		return j.Path + "/", nil
	}
	return j.Path + "/" + r, nil
}

// ParseJobURL parses a Prow job URL into the job and the path inside the job it points
// at ("" for the job itself; "artifacts/junit.xml" for a link to one artifact).
func ParseJobURL(raw string) (Job, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Job{}, "", fmt.Errorf("%w: empty URL", ErrUnsupportedURL)
	}
	var rest string
	if strings.HasPrefix(raw, "gs://") {
		rest = strings.TrimPrefix(raw, "gs://")
		if strings.ContainsAny(rest, "?#") {
			return Job{}, "", fmt.Errorf("%w: query strings and fragments are not accepted", ErrUnsupportedURL)
		}
	} else {
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw // the model sometimes drops the scheme
		}
		u, err := url.Parse(raw)
		if err != nil {
			return Job{}, "", fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
		}
		switch {
		case u.Scheme != "https":
			return Job{}, "", fmt.Errorf("%w: only https URLs are accepted", ErrUnsupportedURL)
		case u.User != nil:
			return Job{}, "", fmt.Errorf("%w: URLs with user information are not accepted", ErrUnsupportedURL)
		case u.Port() != "":
			return Job{}, "", fmt.Errorf("%w: URLs with a port are not accepted", ErrUnsupportedURL)
		case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
			return Job{}, "", fmt.Errorf("%w: query strings and fragments are not accepted", ErrUnsupportedURL)
		}
		p := u.Path
		switch strings.ToLower(u.Hostname()) {
		case "prow.k8s.io":
			switch {
			case strings.HasPrefix(p, "/view/gs/"):
				rest = strings.TrimPrefix(p, "/view/gs/")
			case strings.HasPrefix(p, "/view/gcs/"):
				rest = strings.TrimPrefix(p, "/view/gcs/")
			default:
				return Job{}, "", fmt.Errorf("%w: a prow.k8s.io link must be a job view (/view/gs/…)", ErrUnsupportedURL)
			}
		case "gcsweb.k8s.io":
			if !strings.HasPrefix(p, "/gcs/") {
				return Job{}, "", fmt.Errorf("%w: a gcsweb.k8s.io link must start with /gcs/", ErrUnsupportedURL)
			}
			rest = strings.TrimPrefix(p, "/gcs/")
		case "storage.googleapis.com":
			rest = strings.TrimPrefix(p, "/")
		default:
			return Job{}, "", fmt.Errorf("%w: host %q is not a supported Prow deployment; accepted: %s", ErrUnsupportedURL, u.Hostname(), AcceptedForms)
		}
	}

	bucket, objPath, _ := strings.Cut(rest, "/")
	if !allowedBuckets[bucket] {
		return Job{}, "", fmt.Errorf("%w: bucket %q is not allowed; accepted: %s", ErrUnsupportedURL, bucket, AcceptedForms)
	}
	segs, err := segments(strings.TrimSuffix(objPath, "/"))
	if err != nil {
		return Job{}, "", fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
	}
	n, err := jobDepth(segs)
	if err != nil {
		return Job{}, "", fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
	}
	return Job{Bucket: bucket, Path: strings.Join(segs[:n], "/")}, strings.Join(segs[n:], "/"), nil
}

// jobDepth returns how many leading segments name the job run directory, by the
// layouts Prow writes:
//
//	logs/<job>/<build>                          periodic and postsubmit
//	pr-logs/pull/batch/<job>/<build>            batch presubmit
//	pr-logs/pull/<org_repo>/<pr>/<job>/<build>  presubmit
func jobDepth(segs []string) (int, error) {
	var n int
	switch {
	case len(segs) >= 1 && segs[0] == "logs":
		n = 3
	case len(segs) >= 3 && segs[0] == "pr-logs" && segs[1] == "pull" && segs[2] == "batch":
		n = 5
	case len(segs) >= 2 && segs[0] == "pr-logs" && segs[1] == "pull":
		n = 6
		if len(segs) >= 4 && !digits(segs[3]) {
			return 0, fmt.Errorf("expected a pull request number in the job path")
		}
	default:
		return 0, fmt.Errorf("the job path must start with logs/ or pr-logs/pull/")
	}
	if len(segs) < n {
		return 0, fmt.Errorf("the URL names a job listing, not one run: it must end at a build ID")
	}
	if !digits(segs[n-1]) {
		return 0, fmt.Errorf("expected a numeric build ID where the URL has %q", segs[n-1])
	}
	return n, nil
}

// CleanRel validates a path inside a job directory (model-supplied, so untrusted)
// and returns it normalised, without leading or trailing slashes. "" is the job root.
func CleanRel(rel string) (string, error) {
	rel = strings.Trim(strings.TrimSpace(rel), "/")
	if rel == "" || rel == "." {
		return "", nil
	}
	segs, err := segments(rel)
	if err != nil {
		return "", err
	}
	return strings.Join(segs, "/"), nil
}

// segments splits p on "/" and rejects anything that is not a plain, printable,
// relative path: empty, "." or ".." segments, backslashes, NULs and control
// characters.
func segments(p string) ([]string, error) {
	if p == "" {
		return nil, fmt.Errorf("empty path")
	}
	if len(p) > 1024 {
		return nil, fmt.Errorf("path is too long")
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch s {
		case "":
			return nil, fmt.Errorf("path has an empty segment")
		case ".", "..":
			return nil, fmt.Errorf("path must not contain %q", s)
		}
		for _, r := range s {
			if r < 0x20 || r == 0x7f || r == '\\' {
				return nil, fmt.Errorf("path contains a disallowed character")
			}
		}
	}
	return segs, nil
}

func digits(s string) bool {
	if s == "" || len(s) > 25 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
