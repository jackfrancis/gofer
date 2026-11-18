// Package githubtest has test helpers for code that talks to GitHub through
// internal/github. It imports nothing of gofer's, so the github package's own tests
// can use it too.
package githubtest

import (
	"encoding/json"
	"net/http"
	"strings"
)

// PublicRepos wraps a stub GitHub API handler so it answers the public-only gate's
// repository lookup, GET /repos/{owner}/{name}, as a public repository whose
// canonical name is the one asked for. Every other request goes to h.
func PublicRepos(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := RepoLookup(r); ok {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "full_name": name, "private": false})
			return
		}
		h(w, r)
	})
}

// RepoLookup reports whether r is the gate's repository lookup, and for which
// owner/name.
func RepoLookup(r *http.Request) (string, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/repos/")
	if !ok || r.Method != http.MethodGet {
		return "", false
	}
	segs := strings.Split(rest, "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		return "", false
	}
	return segs[0] + "/" + segs[1], true
}
