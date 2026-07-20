// Package markdown renders untrusted Markdown to sanitized HTML for display in
// the UI. The assistive conversation's replies are produced by an LLM from
// attacker-influenceable input (PR bodies, comments), so their Markdown is
// untrusted: it is parsed without raw-HTML passthrough and then run through a
// strict HTML sanitizer before it may be treated as trusted HTML. This is the
// single place that turns model text into HTML.
package markdown

import (
	"bytes"
	"html/template"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// md parses GitHub-flavored Markdown (tables, strikethrough, autolinks, task
// lists). Raw HTML in the source is escaped, not passed through (WithUnsafe is
// deliberately NOT set), so the only HTML reaching the sanitizer is what goldmark
// itself emits.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// policy is the sanitizer applied to the rendered HTML. UGCPolicy is bluemonday's
// allow-list for user-generated content: it permits common formatting, drops
// scripts/handlers/styles, and confines link and image URLs to safe schemes.
var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.RequireNoReferrerOnFullyQualifiedLinks(true)
	return p
}()

// ToSafeHTML renders Markdown and sanitizes the result. On a render error it
// falls back to escaped plain text, so output is always safe to embed as HTML.
func ToSafeHTML(src string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(policy.SanitizeBytes(buf.Bytes()))
}

// ToSafeHTMLString is ToSafeHTML for callers that serialize the HTML (e.g. a JSON
// field) rather than embed it in a template.
func ToSafeHTMLString(src string) string {
	return string(ToSafeHTML(src))
}
