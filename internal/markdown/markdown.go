// Package markdown renders untrusted Markdown to sanitized HTML for display in
// the UI. The assistive conversation's replies are produced by an LLM from
// attacker-influenceable input (PR bodies, comments), so their Markdown is
// untrusted: it is parsed without raw-HTML passthrough and then run through a
// strict HTML sanitizer before it may be treated as trusted HTML. This is the
// single place that turns model text into HTML.
package markdown

import (
	"bufio"
	"bytes"
	"html/template"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

// md parses GitHub-flavored Markdown (tables, strikethrough, autolinks, task
// lists). Raw HTML in the source is escaped, not passed through (WithUnsafe is
// deliberately NOT set), so the only HTML reaching the sanitizer is what goldmark
// itself emits. Images are rendered as links (imagesAsLinks), so no reply ever
// makes the browser fetch anything.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(imagesAsLinks{}, 100))),
)

// imagesAsLinks renders a Markdown image as a link to its source, with the alt text
// as the link's text, instead of an <img>. A reply's image would otherwise be
// fetched as soon as the thread opens, sending a model-chosen URL (path and query)
// to its host with no click; and the page's CSP admits one third-party image host
// (GitHub's avatars, for the header and each item's author), so a URL there would
// load. With no <img> in any reply, that exception serves those avatars alone. Goldmark's image renderer is
// the only source of <img> here (raw HTML is escaped), so replacing it suffices.
type imagesAsLinks struct{}

func (imagesAsLinks) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindImage, renderImageAsLink)
}

func renderImageAsLink(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.Image)
	dest := util.URLEscape(n.Destination, true)
	label := labelHTML(source, n)
	if len(label) == 0 {
		label = util.EscapeHTML(n.Destination)
	}
	// Inside a link ([![alt](img)](url)) the alt text joins the outer link: anchors
	// cannot nest. A dangerous scheme gets no link (the sanitizer would drop it too).
	if inLink(n) || len(dest) == 0 || html.IsDangerousURL(dest) {
		_, _ = w.Write(label)
		return ast.WalkSkipChildren, nil
	}
	_, _ = w.WriteString(`<a href="`)
	_, _ = w.Write(util.EscapeHTML(dest))
	_ = w.WriteByte('"')
	if n.Title != nil {
		_, _ = w.WriteString(` title="`)
		_, _ = w.Write(util.EscapeHTML(n.Title))
		_ = w.WriteByte('"')
	}
	_ = w.WriteByte('>')
	_, _ = w.Write(label)
	_, _ = w.WriteString("</a>")
	return ast.WalkSkipChildren, nil
}

// labelHTML is the text of n's descendants as escaped HTML without markup, as
// goldmark renders an image's alt text: through its text writer, which resolves
// entities and backslash escapes (&amp; shows as &, \* as *) and escapes the
// result. A line break inside the label becomes a space.
func labelHTML(source []byte, n ast.Node) []byte {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	writeLabel(w, source, n)
	_ = w.Flush()
	return buf.Bytes()
}

func writeLabel(w util.BufWriter, source []byte, n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			if t.IsRaw() {
				html.DefaultWriter.RawWrite(w, t.Segment.Value(source))
			} else {
				html.DefaultWriter.Write(w, t.Segment.Value(source))
			}
			if t.SoftLineBreak() || t.HardLineBreak() {
				_ = w.WriteByte(' ')
			}
		case *ast.String:
			switch {
			case t.IsCode():
				_, _ = w.Write(t.Value) // already HTML (a typographer entity such as &hellip;)
			case t.IsRaw():
				html.DefaultWriter.RawWrite(w, t.Value)
			default:
				html.DefaultWriter.Write(w, t.Value)
			}
		default:
			writeLabel(w, source, c)
		}
	}
}

// inLink reports whether n sits inside a link.
func inLink(n ast.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if _, ok := p.(*ast.Link); ok {
			return true
		}
	}
	return false
}

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
