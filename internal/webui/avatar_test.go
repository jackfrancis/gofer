package webui

import (
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
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

// The header carries the GitHub user's avatar URL for avatar.js and no <img>: an
// image in the markup would delay the load event, and with it a meta refresh. A
// user from another provider gets neither the URL nor the script.
func TestHeaderAvatar(t *testing.T) {
	gh := &session.User{ID: "github:583231", Provider: "github", Name: "The Octocat"}
	for _, data := range []pageData{
		{View: "processing", User: gh, RefreshSecs: 3},
		{View: "worklist", User: gh},
	} {
		out := renderPage(t, data)
		if !strings.Contains(out, `<span class="zz-user" data-avatar="https://avatars.githubusercontent.com/u/583231?s=48&amp;v=4">The Octocat</span>`) {
			t.Fatalf("%s: header has no avatar URL:\n%s", data.View, out)
		}
		if !strings.Contains(out, `<script src="/static/avatar.js" defer></script>`) {
			t.Fatalf("%s: page does not load avatar.js:\n%s", data.View, out)
		}
		if strings.Contains(out, "<img") {
			t.Fatalf("%s: page has an <img> that would delay the load event:\n%s", data.View, out)
		}
	}

	out := renderPage(t, pageData{View: "processing", User: &session.User{ID: "google:1", Provider: "google", Name: "G"}})
	if strings.Contains(out, "data-avatar") || strings.Contains(out, "avatar.js") {
		t.Fatalf("non-GitHub user got an avatar:\n%s", out)
	}
}

// Only the header can reach the avatar host, the one third-party image origin the
// CSP admits: a reply that points an image there renders as a link, not an <img>,
// whether or not the header has an avatar, and on a thread polling for a reply.
func TestReplyCannotLoadAvatarHost(t *testing.T) {
	const leak = "https://avatars.githubusercontent.com/u/1?leak=secret"
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}
	pr.Thread = []worklist.Message{
		{Role: worklist.RoleUser, Content: "Review this"},
		{Role: worklist.RoleAgent, Content: "Here: ![avatar](" + leak + ")"},
		{Role: worklist.RoleUser, Content: "And again?"},
	}
	for _, u := range []*session.User{
		{ID: "github:583231", Provider: "github", Name: "Octo"},
		{ID: "google:1", Provider: "google", Name: "G"},
	} {
		out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, User: u, RefreshSecs: 3})
		if strings.Contains(out, "<img") {
			t.Fatalf("%s: a reply rendered an <img>:\n%s", u.Provider, out)
		}
		if !strings.Contains(out, `<a href="https://avatars.githubusercontent.com/u/1?leak=secret"`) {
			t.Fatalf("%s: the reply's image should render as a link:\n%s", u.Provider, out)
		}
		if n := strings.Count(out, "data-avatar="); n > 1 {
			t.Fatalf("%s: %d avatar URLs on the page, want at most the header's", u.Provider, n)
		}
	}
}

// avatar.js is served from /static, where the CSP's script-src 'self' admits it.
func TestAvatarScriptServed(t *testing.T) {
	b, err := staticFS.ReadFile("static/avatar.js")
	if err != nil {
		t.Fatalf("avatar.js not embedded: %v", err)
	}
	for _, want := range []string{`".zz-user[data-avatar]"`, `"load"`, `"no-referrer"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("avatar.js lacks %s", want)
		}
	}
}
