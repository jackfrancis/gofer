package markdown

import (
	"strings"
	"testing"
)

func TestRendersFormatting(t *testing.T) {
	out := string(ToSafeHTML("**bold** and _italic_"))
	if !strings.Contains(out, "<strong>bold</strong>") || !strings.Contains(out, "<em>italic</em>") {
		t.Fatalf("formatting not rendered: %s", out)
	}
}

// Untrusted Markdown must never yield executable HTML.
func TestSanitizesScript(t *testing.T) {
	out := string(ToSafeHTML("hi\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>"))
	if strings.Contains(out, "<script") || strings.Contains(out, "onerror") {
		t.Fatalf("dangerous HTML not sanitized: %s", out)
	}
}

func TestLinkIsSafe(t *testing.T) {
	out := string(ToSafeHTML("[x](https://example.com)"))
	if !strings.Contains(out, `href="https://example.com"`) {
		t.Fatalf("link not rendered: %s", out)
	}
	if !strings.Contains(out, `rel=`) {
		t.Fatalf("fully-qualified link should carry rel attributes: %s", out)
	}
}

// No reply may make the browser fetch anything: every image form renders without an
// <img>, as a link to its source where it can be one, so the page's one third-party
// image host (the header's GitHub avatar) cannot be reached from a reply.
func TestImagesRenderAsLinks(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"inline", "![the cat](https://avatars.githubusercontent.com/u/1?leak=secret)",
			`<a href="https://avatars.githubusercontent.com/u/1?leak=secret" rel="nofollow noreferrer noopener" target="_blank">the cat</a>`},
		{"reference", "![cat][c]\n\n[c]: https://example.com/c.png", `href="https://example.com/c.png"`},
		{"no alt", "![](https://example.com/c.png)", `>https://example.com/c.png</a>`},
		{"emphasis in alt", "![*a* b](https://example.com/c.png)", `>a b</a>`},
		{"inside a link", "[![badge](https://example.com/b.svg)](https://example.com/ci)", `<a href="https://example.com/ci" rel="nofollow noreferrer noopener" target="_blank">badge</a>`},
		{"dangerous scheme", "![x](javascript:alert(1))", `<p>x</p>`},
		{"data uri", "![x](data:image/png;base64,AAAA)", `<p>x</p>`},
		{"raw html", `<img src="https://avatars.githubusercontent.com/u/1">`, ""}, // goldmark omits raw HTML
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := string(ToSafeHTML(tc.src))
			if strings.Contains(out, "<img") {
				t.Fatalf("rendered an <img>: %s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("got %s, want it to contain %s", out, tc.want)
			}
		})
	}
}
