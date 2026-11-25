package webui

import (
	"strings"
	"testing"
	"time"

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
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR", Author: "dev", AuthorAvatar: "u/7"}}
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
		if n := strings.Count(out, "data-avatar="); n > 2 {
			t.Fatalf("%s: %d avatar URLs on the page, want at most the header's and the author's", u.Provider, n)
		}
		if strings.Contains(out, `data-avatar="`+leak) {
			t.Fatalf("%s: the reply's URL became an avatar:\n%s", u.Provider, out)
		}
	}
}

// avatar.js is served from /static, where the CSP's script-src 'self' admits it.
func TestAvatarScriptServed(t *testing.T) {
	b, err := staticFS.ReadFile("static/avatar.js")
	if err != nil {
		t.Fatalf("avatar.js not embedded: %v", err)
	}
	for _, want := range []string{`".zz-user[data-avatar], .zz-author[data-avatar]"`, `"load"`, `"no-referrer"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("avatar.js lacks %s", want)
		}
	}
}

// Each item's author shows with their avatar's URL for avatar.js, on the radar (the
// ranked, waiting and hidden lists) and the Discuss page, and never as an <img>. The
// URL is built only from a stored account or app id on the avatar host; any other
// stored value shows the login alone, as does an item with no recorded avatar.
func TestAuthorAvatar(t *testing.T) {
	gh := &session.User{ID: "github:583231", Provider: "github", Name: "Me"}
	item := func(id, avatar string) worklist.WorkItem {
		return worklist.WorkItem{ID: id, Type: worklist.TypePullRequest,
			GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "T " + id, State: "open", Author: "dev-" + id, AuthorAvatar: avatar}}
	}
	const user = `<span class="zz-author" title="Opened by dev-a" data-avatar="https://avatars.githubusercontent.com/u/7?s=40&amp;v=4">dev-a</span>`
	const bot = `<span class="zz-author" title="Opened by dev-b" data-avatar="https://avatars.githubusercontent.com/in/29110?s=40&amp;v=4">dev-b</span>`

	hidden := item("h", "u/9")
	hidden.Meta.HiddenAt = time.Now()
	out := renderPage(t, pageData{View: "worklist", User: gh,
		Items:       []worklist.WorkItem{item("a", "u/7"), item("b", "in/29110"), item("c", "https://evil.example/x.png")},
		Waiting:     []worklist.WorkItem{item("w", "u/8")},
		HiddenItems: []worklist.WorkItem{hidden}})
	for _, want := range []string{user, bot,
		`<span class="zz-author" title="Opened by dev-c">dev-c</span>`,
		`data-avatar="https://avatars.githubusercontent.com/u/8?s=40&amp;v=4">dev-w</span>`,
		`data-avatar="https://avatars.githubusercontent.com/u/9?s=40&amp;v=4">dev-h</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("radar lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "evil.example") || strings.Contains(out, "<img") {
		t.Fatalf("radar leaked a stored URL or rendered an <img>:\n%s", out)
	}

	out = renderPage(t, pageData{View: "thread", User: gh, Item: item("a", "u/7")})
	if !strings.Contains(out, user) || strings.Contains(out, "<img") {
		t.Fatalf("thread has no author avatar, or an <img>:\n%s", out)
	}

	out = renderPage(t, pageData{View: "worklist", User: gh, Items: []worklist.WorkItem{item("n", "")}})
	if !strings.Contains(out, `<span class="zz-author" title="Opened by dev-n">dev-n</span>`) {
		t.Fatalf("an item with no avatar should show the login alone:\n%s", out)
	}
	// The author's avatar does not depend on the viewer's: a session from another
	// provider still loads avatar.js on the radar and the Discuss page, though its
	// header carries no avatar.
	g := &session.User{ID: "google:1", Provider: "google", Name: "G"}
	for _, data := range []pageData{
		{View: "worklist", User: g, Items: []worklist.WorkItem{item("a", "u/7")}},
		{View: "thread", User: g, Item: item("a", "u/7")},
	} {
		out := renderPage(t, data)
		if !strings.Contains(out, user) || !strings.Contains(out, `<script src="/static/avatar.js" defer></script>`) {
			t.Fatalf("%s: a non-GitHub viewer does not load the author's avatar:\n%s", data.View, out)
		}
		if strings.Contains(out, `class="zz-user" data-avatar`) {
			t.Fatalf("%s: a non-GitHub viewer got a header avatar:\n%s", data.View, out)
		}
	}

	none := item("x", "u/1")
	none.GitHub.Author = ""
	if out := renderPage(t, pageData{View: "worklist", User: gh, Items: []worklist.WorkItem{none}}); strings.Contains(out, "zz-author") {
		t.Fatalf("an item with no author should show none:\n%s", out)
	}
}
