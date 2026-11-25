package ghcomment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

const (
	reviewHead  = "0123456789abcdef0123456789abcdef01234567"
	reviewPatch = "@@ -1,2 +1,2 @@\n a\n-b\n+B"
)

// reviewAPI serves a public pull request Org/Public#7 and records review POSTs.
type reviewAPI struct {
	state   string
	head    string
	base    string            // the base branch; "" = main
	patches map[string]string // the files listed, path -> patch; nil = a.go with reviewPatch
	filler  int               // unrelated files listed before them, to page the list
	reviews []map[string]string
	created map[string]any // the last review POST body
	answer  int            // the POST's status; 0 = 200
	calls   []string
	baseSHA string                    // the base commit; "" = base0
	changed int                       // changed_files reported; 0 = the files listed
	onPull  func(a *reviewAPI, n int) // called before the nth pull read (from 0) is answered
	pulls   int
}

func (a *reviewAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "public_repo")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/repos/org/public":
			_, _ = w.Write([]byte(`{"full_name":"Org/Public","private":false}`))
		case r.URL.Path == "/repos/Org/Public/pulls/7" && r.Method == http.MethodGet:
			base := a.base
			if base == "" {
				base = "main"
			}
			if a.onPull != nil {
				a.onPull(a, a.pulls)
			}
			a.pulls++
			n := len(a.patches)
			if a.patches == nil {
				n = 1
			}
			changed := a.filler + n
			if a.changed != 0 {
				changed = a.changed
			}
			baseSHA := a.baseSHA
			if baseSHA == "" {
				baseSHA = "base0"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"state": a.state, "head": map[string]any{"sha": a.head}, "base": map[string]any{"ref": base, "sha": baseSHA}, "changed_files": changed})
		case r.URL.Path == "/repos/Org/Public/pulls/7/files":
			patches := a.patches
			if patches == nil {
				patches = map[string]string{"a.go": reviewPatch}
			}
			var all []map[string]string
			for i := range a.filler {
				all = append(all, map[string]string{"filename": "filler/" + strconv.Itoa(i) + ".go", "patch": "@@ -1 +1 @@\n-x\n+y"})
			}
			for p, patch := range patches {
				all = append(all, map[string]string{"filename": p, "patch": patch})
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			from, to := (page-1)*per, page*per
			from, to = min(from, len(all)), min(to, len(all))
			_ = json.NewEncoder(w).Encode(all[from:to])
		case r.URL.Path == "/repos/Org/Public/pulls/7/reviews" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(a.reviews)
		case r.URL.Path == "/repos/Org/Public/pulls/7/reviews" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&a.created)
			if a.answer != 0 {
				w.WriteHeader(a.answer)
				_, _ = w.Write([]byte(`{"message":"Unprocessable Entity"}`))
				return
			}
			_, _ = w.Write([]byte(`{"state":"PENDING","html_url":"https://github.com/Org/Public/pull/7#pullrequestreview-1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testReview(body string) Review {
	return Review{
		Commit: reviewHead, BaseRef: "main", Body: body,
		Comments: []ReviewComment{{Path: "a.go", Line: 3, Side: "RIGHT", Body: "Rename this."}},
		Patches:  map[string]string{"a.go": PatchDigest(reviewPatch)},
	}
}

// The review is created with no event (so GitHub keeps it PENDING), anchored to the
// commit, on the canonical repository, after the files are checked and with the pull
// request read last; its link is returned.
func TestPendingReviewCreatesWithoutEvent(t *testing.T) {
	api := &reviewAPI{state: "open", head: reviewHead, filler: 120, reviews: []map[string]string{{"state": "COMMENTED"}}}
	c := New(api.server(t).URL, nil)
	link, err := c.PendingReview(context.Background(), token, "org/public", 7, testReview("Summary."))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(api.calls); n < 4 || api.calls[n-3] != "GET /repos/Org/Public/pulls/7/files" || api.calls[n-2] != "GET /repos/Org/Public/pulls/7" || api.calls[n-1] != "POST /repos/Org/Public/pulls/7/reviews" {
		t.Errorf("calls = %v", api.calls)
	}
	if link != "https://github.com/Org/Public/pull/7#pullrequestreview-1" {
		t.Errorf("link = %q", link)
	}
	if _, has := api.created["event"]; has {
		t.Fatalf("the review was sent with an event: %v", api.created)
	}
	if api.created["commit_id"] != reviewHead || api.created["body"] != "Summary." {
		t.Errorf("created = %v", api.created)
	}
	cs, _ := api.created["comments"].([]any)
	if len(cs) != 1 {
		t.Fatalf("comments = %v", api.created["comments"])
	}
	if c0 := cs[0].(map[string]any); c0["path"] != "a.go" || c0["line"] != float64(3) || c0["side"] != "RIGHT" {
		t.Errorf("comment = %v", c0)
	}
	// A summary-only review has no file to check.
	api = &reviewAPI{state: "open", head: reviewHead}
	c = New(api.server(t).URL, nil)
	rv := testReview("Summary only.")
	rv.Comments, rv.Patches = nil, nil
	if _, err := c.PendingReview(context.Background(), token, "org/public", 7, rv); err != nil {
		t.Fatal(err)
	}
	for _, call := range api.calls {
		if call == "GET /repos/Org/Public/pulls/7/files" {
			t.Error("a summary-only review read the files")
		}
	}
}

// Nothing is created on a moved head, a retargeted base, a commented file whose
// patch changed or that left the diff (a related change merged into the base), a
// closed pull request, an existing pending review, a private repository or invalid
// input.
func TestPendingReviewRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		api  reviewAPI
		repo string
		want error
	}{
		"head moved":     {reviewAPI{state: "open", head: "ffffffffffffffffffffffffffffffffffffffff"}, "org/public", ErrHeadMoved},
		"retargeted":     {reviewAPI{state: "open", head: reviewHead, base: "release-1.0"}, "org/public", ErrHeadMoved},
		"patch changed":  {reviewAPI{state: "open", head: reviewHead, patches: map[string]string{"a.go": "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c"}}, "org/public", ErrHeadMoved},
		"file left diff": {reviewAPI{state: "open", head: reviewHead, filler: 60, patches: map[string]string{"b.go": reviewPatch}}, "org/public", ErrHeadMoved},
		"patch gone":     {reviewAPI{state: "open", head: reviewHead, patches: map[string]string{"a.go": ""}}, "org/public", ErrHeadMoved},
		"closed":         {reviewAPI{state: "closed", head: reviewHead}, "org/public", ErrNotOpen},
		"pending exists": {reviewAPI{state: "open", head: reviewHead, reviews: []map[string]string{{"state": "PENDING"}}}, "org/public", ErrPendingExists},
		"not public":     {reviewAPI{state: "open", head: reviewHead}, "org/gone", ErrNotPublic},
	} {
		t.Run(name, func(t *testing.T) {
			api := tc.api
			c := New(api.server(t).URL, nil)
			_, err := c.PendingReview(context.Background(), token, tc.repo, 7, testReview(""))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if api.created != nil {
				t.Errorf("a review was created: %v", api.created)
			}
		})
	}
	api := &reviewAPI{state: "open", head: reviewHead}
	c := New(api.server(t).URL, nil)
	for name, comments := range map[string][]ReviewComment{
		"bad side":   {{Path: "a.go", Line: 1, Side: "UP", Body: "x"}},
		"no line":    {{Path: "a.go", Side: "RIGHT", Body: "x"}},
		"empty body": {{Path: "a.go", Line: 1, Side: "RIGHT"}},
		"no digest":  {{Path: "b.go", Line: 1, Side: "RIGHT", Body: "x"}},
	} {
		rv := testReview("")
		rv.Comments = comments
		if _, err := c.PendingReview(context.Background(), token, "org/public", 7, rv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	empty := testReview(" ")
	empty.Comments = nil
	if _, err := c.PendingReview(context.Background(), token, "org/public", 7, empty); err == nil {
		t.Error("an empty review was accepted")
	}
	if api.created != nil {
		t.Errorf("a review was created: %v", api.created)
	}
}

// GitHub's refusal of the review itself (a line it cannot resolve) comes back as a
// StatusError with its message; a server error on the POST is an unknown outcome,
// since it does not say the write was rolled back.
func TestPendingReviewPostAnswers(t *testing.T) {
	api := &reviewAPI{state: "open", head: reviewHead, answer: http.StatusUnprocessableEntity}
	c := New(api.server(t).URL, nil)
	_, err := c.PendingReview(context.Background(), token, "org/public", 7, testReview(""))
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnprocessableEntity || se.Message != "Unprocessable Entity" {
		t.Fatalf("422: err = %v", err)
	}
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout} {
		api := &reviewAPI{state: "open", head: reviewHead, answer: status}
		c := New(api.server(t).URL, nil)
		if _, err := c.PendingReview(context.Background(), token, "org/public", 7, testReview("")); !errors.Is(err, ErrOutcomeUnknown) {
			t.Errorf("%d on the POST: err = %v, want ErrOutcomeUnknown", status, err)
		}
	}
}

// An approval names the whole diff it approves: the review is created only while
// every file the pull request lists hashes to that digest. A stacked dependency
// merged into the base (a file added to the diff, same head and base) refuses it, as
// a moved patch does; an approval without comments is checked all the same.
func TestPendingReviewChecksTheApprovedDiff(t *testing.T) {
	files := func(patches map[string]string, filler int) []File {
		var out []File
		for i := range filler {
			out = append(out, File{Filename: "filler/" + strconv.Itoa(i) + ".go", Patch: "@@ -1 +1 @@\n-x\n+y"})
		}
		for p, patch := range patches {
			out = append(out, File{Filename: p, Patch: patch})
		}
		return out
	}
	reviewed := DiffDigest(files(map[string]string{"a.go": reviewPatch}, 120))

	api := &reviewAPI{state: "open", head: reviewHead, filler: 120}
	c := New(api.server(t).URL, nil)
	rv := testReview("/lgtm\n/approve")
	rv.Diff = reviewed
	if _, err := c.PendingReview(context.Background(), token, "org/public", 7, rv); err != nil {
		t.Fatalf("matching diff refused: %v", err)
	}
	approvalOnly := Review{Commit: reviewHead, BaseRef: "main", Body: "/lgtm\n/approve", Diff: reviewed}
	api = &reviewAPI{state: "open", head: reviewHead, filler: 120}
	if _, err := New(api.server(t).URL, nil).PendingReview(context.Background(), token, "org/public", 7, approvalOnly); err != nil {
		t.Fatalf("approval-only review refused: %v", err)
	}
	// The workload's boundary: 3000 files, thirty full pages, is read whole.
	api = &reviewAPI{state: "open", head: reviewHead, filler: 2999}
	big := approvalOnly
	big.Diff = DiffDigest(files(map[string]string{"a.go": reviewPatch}, 2999))
	if _, err := New(api.server(t).URL, nil).PendingReview(context.Background(), token, "org/public", 7, big); err != nil {
		t.Fatalf("a matching 3000-file diff refused: %v", err)
	}
	// A comment on a path the approved diff does not list is refused, though the
	// diff itself matches.
	api = &reviewAPI{state: "open", head: reviewHead, filler: 120}
	stray := testReview("/lgtm\n/approve")
	stray.Diff = reviewed
	stray.Comments = append(stray.Comments, ReviewComment{Path: "elsewhere.go", Line: 1, Side: "RIGHT", Body: "x"})
	stray.Patches["elsewhere.go"] = PatchDigest("@@ -1 +1 @@\n-x\n+y")
	if _, err := New(api.server(t).URL, nil).PendingReview(context.Background(), token, "org/public", 7, stray); !errors.Is(err, ErrHeadMoved) || api.created != nil {
		t.Errorf("a comment off the approved diff: err %v, created %v", err, api.created)
	}
	for name, patches := range map[string]map[string]string{
		"dependency's file now in the diff": {"a.go": reviewPatch, "dep.go": "@@ -0,0 +1 @@\n+dep"},
		"patch moved":                       {"a.go": "@@ -1,2 +1,2 @@\n a\n-b\n+C"},
	} {
		api := &reviewAPI{state: "open", head: reviewHead, filler: 120, patches: patches}
		_, err := New(api.server(t).URL, nil).PendingReview(context.Background(), token, "org/public", 7, approvalOnly)
		if !errors.Is(err, ErrHeadMoved) || api.created != nil {
			t.Errorf("%s: err %v, created %v", name, err, api.created)
		}
	}
}

// The base commit must hold still between reading an approved diff and the final
// read of the pull request: a stacked dependency merged in between changes a patch
// without moving the head, the base's name or the file count. When it moves, the
// diff is read again: an unrelated advance still creates the review, a merged
// dependency refuses it as out of date, and a base that keeps moving creates
// nothing and asks to try again.
func TestPendingReviewRechecksWhenTheBaseMoves(t *testing.T) {
	approval := Review{Commit: reviewHead, BaseRef: "main", Body: "/lgtm\n/approve", Diff: DiffDigest([]File{{Filename: "a.go", Patch: reviewPatch}})}
	run := func(onPull func(a *reviewAPI, n int)) (*reviewAPI, error) {
		api := &reviewAPI{state: "open", head: reviewHead, onPull: onPull}
		_, err := New(api.server(t).URL, nil).PendingReview(context.Background(), token, "org/public", 7, approval)
		return api, err
	}
	// Reads: 0 = checkDiff's, 1 = the final read, 2 = the retry's checkDiff, 3 = its final read.
	api, err := run(func(a *reviewAPI, n int) {
		if n == 1 {
			a.baseSHA = "base1" // unrelated: the files stay as they are
		}
	})
	if err != nil || api.created == nil || api.pulls != 4 {
		t.Errorf("unrelated advance: err %v, created %v, pull reads %d", err, api.created, api.pulls)
	}
	api, err = run(func(a *reviewAPI, n int) {
		if n == 1 { // the dependency merged after the files were read
			a.baseSHA = "base1"
			a.patches = map[string]string{"a.go": "@@ -1,2 +1,2 @@\n a\n-b2\n+B"}
		}
	})
	if !errors.Is(err, ErrHeadMoved) || api.created != nil {
		t.Errorf("merged dependency: err %v, created %v", err, api.created)
	}
	api, err = run(func(a *reviewAPI, n int) { a.baseSHA = "base" + strconv.Itoa(n) })
	if err == nil || errors.Is(err, ErrHeadMoved) || api.created != nil {
		t.Errorf("a base that keeps moving: err %v, created %v", err, api.created)
	}
}

// An approved diff was read whole, so a pull request now past the 3000-file cap is
// another diff: the draft is out of date (ErrHeadMoved), so the owner is offered a
// redraft rather than a retry that cannot succeed.
func TestPendingReviewPastTheCapIsOutOfDate(t *testing.T) {
	api := &reviewAPI{state: "open", head: reviewHead, changed: 3001}
	approval := Review{Commit: reviewHead, BaseRef: "main", Body: "/lgtm\n/approve", Diff: DiffDigest([]File{{Filename: "a.go", Patch: reviewPatch}})}
	if _, err := New(api.server(t).URL, nil).PendingReview(context.Background(), token, "org/public", 7, approval); !errors.Is(err, ErrHeadMoved) || api.created != nil {
		t.Errorf("err %v, created %v", err, api.created)
	}
}
