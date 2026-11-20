package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/vault"
)

// brokenVault fails every read, as a vault gofer cannot open would.
type brokenVault struct{ vault.Vault }

func (brokenVault) Get(context.Context, string, string) (vault.Credential, error) {
	return vault.Credential{}, errors.New("storage: open credential (key id k1): authentication failed")
}

func allowed(t *testing.T, h *Handler, provider string) bool {
	t.Helper()
	ok, err := h.ScheduledRunAllowed(bg, user, provider)
	if err != nil {
		t.Fatalf("ScheduledRunAllowed: %v", err)
	}
	return ok
}

// Sign-in records the login on the credential, and a refresh keeps it.
func TestCredentialKeepsTheSignInLogin(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	signIn(t, h)
	if got := stored(t, vlt).Login; got != "octocat" {
		t.Fatalf("stored login = %q, want octocat", got)
	}
	c := stored(t, vlt)
	c.Expiry = time.Now().Add(10 * time.Minute) // due for an early refresh
	put(t, gh, vlt, c)
	if _, err := h.Credential(bg, user, "github", VendRequest{}); err != nil {
		t.Fatal(err)
	}
	if s := stored(t, vlt); s.Version == c.Version || s.Login != "octocat" {
		t.Fatalf("after a refresh: version %q (was %q), login %q", s.Version, c.Version, s.Login)
	}
}

// A scheduled run starts only for an account gofer could act for now: a vendable
// credential, no pending logout, and a login the allow-list still admits.
func TestScheduledRunAllowed(t *testing.T) {
	gh := newFakeGitHub(t)

	t.Run("open sign-in", func(t *testing.T) {
		h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
		if allowed(t, h, "github") {
			t.Fatal("allowed with no credential")
		}
		put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 8*time.Hour)) // no login recorded
		if !allowed(t, h, "github") {
			t.Fatal("with no allow-list, a vendable credential is enough")
		}
		if allowed(t, h, "google") {
			t.Fatal("allowed for a provider gofer does not sign in with")
		}
		put(t, gh, vlt, vault.Credential{AccessToken: "gho_other", ClientID: "other-client", Version: "v1"})
		if allowed(t, h, "github") {
			t.Fatal("allowed with another client's credential")
		}
	})

	t.Run("allow-list", func(t *testing.T) {
		h, _, cfg, vlt := newKindHandler(t, gh, config.GitHubApp, "octocat")
		c := appCred("ghu_a", "ghr_a", "v1", 8*time.Hour)
		c.Login = "OctoCat"
		put(t, gh, vlt, c)
		if !allowed(t, h, "github") {
			t.Fatal("a listed login (any case) is allowed")
		}
		cfg.AllowedGitHubLogins = []string{"someone-else"} // removed from the allow-list
		if allowed(t, h, "github") {
			t.Fatal("an account removed from ALLOWED_GITHUB_LOGINS must not get scheduled runs")
		}
		cfg.AllowedGitHubLogins = []string{"octocat"}
		c.Login = ""
		put(t, gh, vlt, c)
		if allowed(t, h, "github") {
			t.Fatal("with an allow-list set, a credential with no recorded login is refused")
		}
	})

	t.Run("pending logout", func(t *testing.T) {
		h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
		put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 8*time.Hour))
		if err := h.ledger.MarkLogout(bg, user, "github"); err != nil {
			t.Fatal(err)
		}
		if allowed(t, h, "github") {
			t.Fatal("allowed while a logout is pending")
		}
	})

	t.Run("unreadable vault", func(t *testing.T) {
		h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
		put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 8*time.Hour))
		h.vault = brokenVault{vlt}
		ok, err := h.ScheduledRunAllowed(bg, user, "github")
		if ok || err == nil {
			t.Fatalf("an unreadable credential: ok=%v err=%v, want false with the error", ok, err)
		}
		if h.SignInRequired(bg, user, "github") {
			t.Fatal("(the page's SignInRequired answers false here, which is why the scheduler cannot use it)")
		}
	})
}

// The login survives the SQLite vault's sealed record.
func TestSealedCredentialKeepsLogin(t *testing.T) {
	gh := newFakeGitHub(t)
	_, vlt := sqliteHandler(t, gh, t.TempDir())
	c := appCred("ghu_a", "ghr_a", "v1", time.Hour)
	c.Login = "octocat"
	put(t, gh, vlt, c)
	if got := stored(t, vlt).Login; got != "octocat" {
		t.Fatalf("login after a SQLite round trip = %q", got)
	}
}
