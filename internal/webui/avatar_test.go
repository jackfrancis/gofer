package webui

import (
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/session"
)

// The avatar comes from the GitHub account id alone, on GitHub's avatar host, and
// only for a GitHub user whose id is the numeric one sign-in stores.
func TestAvatarURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    *session.User
		want string
	}{
		{"github", &session.User{ID: "github:583231", Provider: "github", Login: "octocat"}, AvatarOrigin + "/u/583231?s=48&v=4"},
		{"nil", nil, ""},
		{"other provider", &session.User{ID: "google:583231", Provider: "google"}, ""},
		{"provider mismatch", &session.User{ID: "google:583231", Provider: "github"}, ""},
		{"empty id", &session.User{ID: "github:", Provider: "github"}, ""},
		{"non-numeric id", &session.User{ID: "github:1/../x?y", Provider: "github"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := avatarURL(tc.u); got != tc.want {
				t.Fatalf("avatarURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// The header shows the GitHub user's avatar beside their name, and none for an
// account from another provider.
func TestHeaderAvatar(t *testing.T) {
	out := renderPage(t, pageData{View: "processing", User: &session.User{ID: "github:583231", Provider: "github", Name: "The Octocat"}})
	if !strings.Contains(out, `<img class="zz-avatar" src="https://avatars.githubusercontent.com/u/583231?s=48&amp;v=4"`) {
		t.Fatalf("header has no avatar:\n%s", out)
	}
	if !strings.Contains(out, "The Octocat</span>") {
		t.Fatalf("header lost the name:\n%s", out)
	}

	out = renderPage(t, pageData{View: "processing", User: &session.User{ID: "google:1", Provider: "google", Name: "G"}})
	if strings.Contains(out, "zz-avatar") {
		t.Fatalf("non-GitHub user got an avatar:\n%s", out)
	}
}
