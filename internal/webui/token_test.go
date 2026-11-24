package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/comment"
)

type detailCommenter struct {
	fakeCommenter
	details comment.Details
}

func (d *detailCommenter) Details(context.Context, string) comment.Details { return d.details }

func radarPage(t *testing.T, c Commenter, query string) (string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	commentFixture(t, nil).withCommenter(c).Index(rec, httptest.NewRequest(http.MethodGet, "/"+query, nil))
	return rec.Body.String(), rec.Header()
}

func (h *Handler) withCommenter(c Commenter) *Handler {
	if c != nil {
		h.UseCommenter(c)
	}
	return h
}

// Where the deployment allows tokens, an unconnected user is offered the token form
// beside the OAuth connect link (and the page lets it post its real origin); a user
// connected with a token is told when it expires; and nobody else sees either.
func TestRadarTokenControls(t *testing.T) {
	offered, hdr := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}, details: comment.Details{TokensAllowed: true}}, "")
	for _, want := range []string{`href="/auth/github-write/login"`, `action="/auth/github-write/token"`, `type="password" name="token"`, `scopes=public_repo`} {
		if !strings.Contains(offered, want) {
			t.Errorf("unconnected radar lacks %s", want)
		}
	}
	if hdr.Get("Referrer-Policy") != "same-origin" {
		t.Errorf("Referrer-Policy = %q", hdr.Get("Referrer-Policy"))
	}
	notAllowed, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}}, "")
	if strings.Contains(notAllowed, "/auth/github-write/token") {
		t.Error("the token form is offered where tokens are not allowed")
	}
	expires := time.Date(2026, 12, 24, 12, 0, 0, 0, time.UTC)
	connected, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, details: comment.Details{TokensAllowed: true, Token: true, Expires: expires}}, "")
	if !strings.Contains(connected, "uses your personal access token, which expires December 24, 2026") || strings.Contains(connected, `action="/auth/github-write/token"`) {
		t.Error("a token-connected radar does not show the token's expiry, or still offers the form")
	}
	for query, want := range map[string]string{"?commenting=token": "connected with your personal access token", "?commenting=token-deleted": "cannot revoke a personal access token"} {
		if page, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}, details: comment.Details{TokensAllowed: true}}, query); !strings.Contains(page, want) {
			t.Errorf("%s: notice missing", query)
		}
	}
}
