package ghcomment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const reviewHead = "0123456789abcdef0123456789abcdef01234567"

// reviewAPI serves a public pull request Org/Public#7 and records review POSTs.
type reviewAPI struct {
	state   string
	head    string
	reviews []map[string]string
	created map[string]any // the last review POST body
	answer  int            // the POST's status; 0 = 200
}

func (a *reviewAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/repos/org/public":
			_, _ = w.Write([]byte(`{"full_name":"Org/Public","private":false}`))
		case r.URL.Path == "/repos/Org/Public/pulls/7" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"state": a.state, "head": map[string]any{"sha": a.head}})
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

var reviewComments = []ReviewComment{{Path: "a.go", Line: 3, Side: "RIGHT", Body: "Rename this."}}

// The review is created with no event (so GitHub keeps it PENDING), anchored to the
// commit, on the canonical repository, and its link is returned.
func TestPendingReviewCreatesWithoutEvent(t *testing.T) {
	api := &reviewAPI{state: "open", head: reviewHead, reviews: []map[string]string{{"state": "COMMENTED"}}}
	c := New(api.server(t).URL, nil)
	link, err := c.PendingReview(context.Background(), token, "org/public", 7, reviewHead, "Summary.", reviewComments)
	if err != nil {
		t.Fatal(err)
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
}

// Nothing is created on a moved head, a closed pull request, an existing pending
// review, a private repository or invalid input.
func TestPendingReviewRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		api  reviewAPI
		repo string
		want error
	}{
		"head moved":     {reviewAPI{state: "open", head: "ffffffffffffffffffffffffffffffffffffffff"}, "org/public", ErrHeadMoved},
		"closed":         {reviewAPI{state: "closed", head: reviewHead}, "org/public", ErrNotOpen},
		"pending exists": {reviewAPI{state: "open", head: reviewHead, reviews: []map[string]string{{"state": "PENDING"}}}, "org/public", ErrPendingExists},
		"not public":     {reviewAPI{state: "open", head: reviewHead}, "org/gone", ErrNotPublic},
	} {
		t.Run(name, func(t *testing.T) {
			api := tc.api
			c := New(api.server(t).URL, nil)
			_, err := c.PendingReview(context.Background(), token, tc.repo, 7, reviewHead, "", reviewComments)
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
	} {
		if _, err := c.PendingReview(context.Background(), token, "org/public", 7, reviewHead, "", comments); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := c.PendingReview(context.Background(), token, "org/public", 7, reviewHead, " ", nil); err == nil {
		t.Error("an empty review was accepted")
	}
	if api.created != nil {
		t.Errorf("a review was created: %v", api.created)
	}
}

// GitHub's refusal of the review itself (a line it cannot resolve) comes back as a
// StatusError with its message.
func TestPendingReviewUnprocessable(t *testing.T) {
	api := &reviewAPI{state: "open", head: reviewHead, answer: http.StatusUnprocessableEntity}
	c := New(api.server(t).URL, nil)
	_, err := c.PendingReview(context.Background(), token, "org/public", 7, reviewHead, "", reviewComments)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnprocessableEntity || se.Message != "Unprocessable Entity" {
		t.Fatalf("err = %v", err)
	}
}
