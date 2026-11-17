package prow

import (
	"errors"
	"testing"
)

const jobPath = "pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/6825/pull-cluster-api-provider-azure-e2e-workload-upgrade-v1beta1/2121629446245978112"

func TestParseJobURLAccepted(t *testing.T) {
	for _, tc := range []struct {
		raw, wantPath, wantRel string
	}{
		{"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath, jobPath, ""},
		{"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath + "/", jobPath, ""},
		{"prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath, jobPath, ""},
		{"https://prow.k8s.io/view/gcs/kubernetes-ci-logs/" + jobPath, jobPath, ""},
		{"https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/" + jobPath + "/artifacts/", jobPath, "artifacts"},
		{"https://storage.googleapis.com/kubernetes-ci-logs/" + jobPath + "/build-log.txt", jobPath, "build-log.txt"},
		{"gs://kubernetes-ci-logs/" + jobPath + "/artifacts/junit.xml", jobPath, "artifacts/junit.xml"},
		{"https://PROW.K8S.IO/view/gs/kubernetes-ci-logs/" + jobPath, jobPath, ""},
		{"https://prow.k8s.io/view/gs/kubernetes-ci-logs/logs/ci-capz-periodic/1999", "logs/ci-capz-periodic/1999", ""},
		{"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/batch/pull-x/77", "pr-logs/pull/batch/pull-x/77", ""},
		{"https://prow.k8s.io/view/gs/kubernetes-jenkins/logs/old-job/5", "logs/old-job/5", ""},
	} {
		job, rel, err := ParseJobURL(tc.raw)
		if err != nil {
			t.Errorf("ParseJobURL(%q): %v", tc.raw, err)
			continue
		}
		if job.Path != tc.wantPath || rel != tc.wantRel {
			t.Errorf("ParseJobURL(%q) = %q, %q; want %q, %q", tc.raw, job.Path, rel, tc.wantPath, tc.wantRel)
		}
	}
	job, _, _ := ParseJobURL("https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath)
	if job.Name() != "pull-cluster-api-provider-azure-e2e-workload-upgrade-v1beta1" || job.BuildID() != "2121629446245978112" {
		t.Errorf("Name/BuildID = %q/%q", job.Name(), job.BuildID())
	}
	if job.URL() != "https://prow.k8s.io/view/gs/kubernetes-ci-logs/"+jobPath {
		t.Errorf("URL = %q", job.URL())
	}
}

func TestParseJobURLRejected(t *testing.T) {
	for _, raw := range []string{
		"",
		"http://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath,                   // not https
		"https://evil.example/view/gs/kubernetes-ci-logs/" + jobPath,                 // other host
		"https://prow.k8s.io.evil.example/view/gs/kubernetes-ci-logs/" + jobPath,     // look-alike host
		"https://169.254.169.254/kubernetes-ci-logs/" + jobPath,                      // metadata endpoint
		"https://user:pw@prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath,          // userinfo
		"https://prow.k8s.io:8443/view/gs/kubernetes-ci-logs/" + jobPath,             // port
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath + "?x=1",         // query
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath + "#frag",        // fragment
		"https://prow.k8s.io/view/gs/some-other-bucket/" + jobPath,                   // bucket
		"https://prow.k8s.io/pr?query=is%3Apr",                                       // not a job view
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/o_r/42/job",     // no build id
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/o_r/42/job/abc", // non-numeric build
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/other/job/1",                 // unknown layout
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath + "/../../x",     // traversal
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath + "/%2e%2e/x",    // encoded traversal
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath + "/a%5Cb",       // backslash
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath + "/a%0Ab",       // control char
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/" + jobPath + "//x",          // empty segment
		"gs://kubernetes-ci-logs/" + jobPath + "?x",                                  // gs with query
		"file:///etc/passwd",
	} {
		if job, rel, err := ParseJobURL(raw); err == nil {
			t.Errorf("ParseJobURL(%q) = %+v, %q; want an error", raw, job, rel)
		} else if !errors.Is(err, ErrUnsupportedURL) {
			t.Errorf("ParseJobURL(%q) error %v does not wrap ErrUnsupportedURL", raw, err)
		}
	}
}

func TestCleanRel(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", ".": "", "/artifacts/": "artifacts", " build-log.txt ": "build-log.txt", "a/b/c.log": "a/b/c.log"} {
		if got, err := CleanRel(in); err != nil || got != want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "a/../b", "a/./b", "a//b", "a\\b", "a\x00b", "a\nb"} {
		if got, err := CleanRel(in); err == nil {
			t.Errorf("CleanRel(%q) = %q; want an error", in, got)
		}
	}
}
