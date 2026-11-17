// Package prowtest is a fake Google Cloud Storage server for tests of the Prow log
// tools: it serves objects and the JSON listing API the way storage.googleapis.com
// does, and records every request so a test can check what was sent.
package prowtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Request is one recorded request.
type Request struct {
	Path          string
	Query         string
	Authorization string
	Cookie        string
}

// Server is a fake GCS.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	objects  map[string]string // "bucket/object" → content
	requests []Request
	gets     map[string]int
}

// New serves objects, keyed "bucket/object name".
func New(t *testing.T, objects map[string]string) *Server {
	t.Helper()
	s := &Server{objects: objects, gets: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Requests returns every request so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Gets returns how many times "bucket/object" was downloaded.
func (s *Server) Gets(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[key]
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Path: r.URL.Path, Query: r.URL.RawQuery,
		Authorization: r.Header.Get("Authorization"), Cookie: r.Header.Get("Cookie")})
	s.mu.Unlock()

	if rest, ok := strings.CutPrefix(r.URL.Path, "/storage/v1/b/"); ok {
		bucket, tail, _ := strings.Cut(rest, "/")
		if tail != "o" {
			http.NotFound(w, r)
			return
		}
		s.list(w, bucket, r.URL.Query().Get("prefix"), r.URL.Query().Get("delimiter"))
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	if key == "redirect/me" {
		http.Redirect(w, r, "https://example.invalid/elsewhere", http.StatusFound)
		return
	}
	s.mu.Lock()
	body, ok := s.objects[key]
	if ok {
		s.gets[key]++
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "No such object", http.StatusNotFound)
		return
	}
	_, _ = w.Write([]byte(body))
}

func (s *Server) list(w http.ResponseWriter, bucket, prefix, delimiter string) {
	type item struct {
		Name string `json:"name"`
		Size string `json:"size"`
	}
	var out struct {
		Items    []item   `json:"items,omitempty"`
		Prefixes []string `json:"prefixes,omitempty"`
	}
	seen := map[string]bool{}
	s.mu.Lock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		b, name, _ := strings.Cut(k, "/")
		if b != bucket || !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if delimiter != "" {
			if i := strings.Index(rest, delimiter); i >= 0 {
				p := prefix + rest[:i+1]
				if !seen[p] {
					seen[p] = true
					out.Prefixes = append(out.Prefixes, p)
				}
				continue
			}
		}
		s.mu.Lock()
		size := len(s.objects[k])
		s.mu.Unlock()
		out.Items = append(out.Items, item{Name: name, Size: strconv.Itoa(size)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
