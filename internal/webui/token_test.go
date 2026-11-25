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

// An expired token is named on the radar, with a Disconnect for gofer's copy, beside
// the form to paste a new one.
func TestRadarExpiredToken(t *testing.T) {
	expired := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	page, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}, details: comment.Details{TokensAllowed: true, TokenExpired: true, Expires: expired}}, "")
	for _, want := range []string{"Your commenting token expired on November 1, 2026", `action="/auth/github-write/disconnect"`, `action="/auth/github-write/token"`} {
		if !strings.Contains(page, want) {
			t.Errorf("expired-token radar lacks %s", want)
		}
	}
	for query, want := range map[string]string{"?commenting=disconnect-pending": "could not finish deleting", "?commenting=unconfirmed-any": "could not tell what GitHub still holds"} {
		if page, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}}, query); !strings.Contains(page, want) {
			t.Errorf("%s: notice missing", query)
		}
	}
}

// Review (round 3): a token stored while tokens were allowed, once they are not, gets
// a Disconnect and no OAuth connect link (the callback would refuse while it is
// held); a disconnect whose cleanup is pending gets a Retry disconnect.
func TestRadarStrandedCredentials(t *testing.T) {
	disabled, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}, details: comment.Details{TokenDisabled: true, Expires: time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)}}, "")
	if !strings.Contains(disabled, "no longer accepts personal access tokens") || !strings.Contains(disabled, `action="/auth/github-write/disconnect"`) {
		t.Error("a disabled token is not offered Disconnect")
	}
	if strings.Contains(disabled, `href="/auth/github-write/login"`) {
		t.Error("the connect link is offered while a disabled token is in its way")
	}
	pending, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}, details: comment.Details{TokensAllowed: true, CleanupPending: true}}, "")
	for _, want := range []string{"Retry disconnect", `action="/auth/github-write/disconnect"`, `href="/auth/github-write/login"`, `action="/auth/github-write/token"`} {
		if !strings.Contains(pending, want) {
			t.Errorf("cleanup-pending radar lacks %s", want)
		}
	}
	if page, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}}, "?commenting=revoked-pending"); !strings.Contains(page, "access is revoked, but gofer could not finish deleting") {
		t.Error("revoked-pending notice missing")
	}
	// Review (round 4): a grant from a previous commenting app gets Disconnect, and
	// neither way to connect (the token form would refuse while it is held).
	other, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}, details: comment.Details{TokensAllowed: true, OtherClient: true}}, "")
	if !strings.Contains(other, "previous commenting app") || !strings.Contains(other, `action="/auth/github-write/disconnect"`) {
		t.Error("another app's grant is not offered Disconnect")
	}
	if strings.Contains(other, `href="/auth/github-write/login"`) || strings.Contains(other, `action="/auth/github-write/token"`) {
		t.Error("a way to connect is offered while another app's grant is in its way")
	}
	for query, want := range map[string]string{"?commenting=token-unrevoked": "did not confirm it was revoked", "?commenting=token-replaced": "delete the old one"} {
		if page, _ := radarPage(t, &detailCommenter{fakeCommenter: fakeCommenter{enabled: true}}, query); !strings.Contains(page, want) {
			t.Errorf("%s: notice missing", query)
		}
	}
}
