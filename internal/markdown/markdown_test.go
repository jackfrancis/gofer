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
