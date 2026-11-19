package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// This file is the runtime half of ADR 0007's "re-vend on 401": a run's GitHub calls
// get their token from a tokenHolder, which re-vends once when GitHub rejects it,
// naming the rejected credential version so the broker can confirm and refresh it.

// ReconnectError reports that the broker answered "sign-in required": gofer holds no
// usable credential for the user, and only their signing in again helps. It matches
// worklist.ErrSignInRequired under errors.Is, so the converser's tool loop and
// runConverse recognise it however it is wrapped.
type ReconnectError struct {
	Provider string
}

func (e *ReconnectError) Error() string {
	return fmt.Sprintf("sign-in required: gofer can't reach %s for this user; they must sign in to gofer again", e.Provider)
}

// Is makes errors.Is(err, worklist.ErrSignInRequired) true.
func (e *ReconnectError) Is(target error) bool { return target == worklist.ErrSignInRequired }

// VendedCredential is a vended access token and the version of the stored
// credential it came from (not a secret).
type VendedCredential struct {
	AccessToken string
	Version     string
}

// CredentialVendor is a Vendor that also reports the credential version and can
// re-vend naming a version GitHub rejected. PlaneClient and VaultVendor implement
// it; a Vendor that does not simply gets no re-vend.
type CredentialVendor interface {
	VendCredential(ctx context.Context, provider, rejected string) (VendedCredential, error)
}

// signInRequiredStatus and signInRequiredCode are the broker's "sign-in required"
// answer (api.StatusSignInRequired, api.CodeSignInRequired). The workload does not
// import the web tier, so they are repeated here; a test in internal/api holds the
// two sides together.
const (
	signInRequiredStatus = http.StatusGone
	signInRequiredCode   = "sign_in_required"
)

// defaultGitHubAPIHost is the API host a run talks to when GitHubBaseURL is empty.
const defaultGitHubAPIHost = "api.github.com"

// tokenHolder holds one run's GitHub token. Its transport (client) puts the current
// token on every request to the GitHub API host that carries a token the holder
// handed out, and on a 401 re-vends once per holder with rejected=<version>:
//
//   - a different token comes back: the request is retried with it;
//   - the same token comes back (the broker confirmed it is valid): the 401 stands;
//   - "sign-in required": the request fails with a ReconnectError, and so does every
//     later request through the holder, without reaching GitHub.
//
// Callers keep passing the token string they were given; the transport swaps it,
// so internal/github needs no change. Requests to other hosts (codeload, the log
// blob host) are untouched: net/http drops Authorization on a cross-host redirect,
// and the holder never adds one.
type tokenHolder struct {
	vendor   Vendor
	provider string
	apiHost  string

	mu        sync.Mutex
	current   string
	version   string
	issued    map[string]bool // every token this holder handed out
	revended  bool
	reconnect error // sticky ReconnectError
}

// newTokenHolder vends the run's first token. A "sign-in required" answer is a
// ReconnectError.
func newTokenHolder(ctx context.Context, vendor Vendor, provider, apiBase string) (*tokenHolder, error) {
	host := defaultGitHubAPIHost
	if apiBase != "" {
		u, err := url.Parse(apiBase)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("agent: bad GitHub base URL")
		}
		host = u.Host
	}
	h := &tokenHolder{vendor: vendor, provider: provider, apiHost: host, issued: map[string]bool{}}
	var vc VendedCredential
	var err error
	if cv, ok := vendor.(CredentialVendor); ok {
		vc, err = cv.VendCredential(ctx, provider, "")
	} else {
		vc.AccessToken, err = vendor.Vend(ctx, provider)
	}
	if err != nil {
		return nil, err
	}
	if vc.AccessToken == "" {
		return nil, fmt.Errorf("vend %s credential: empty token", provider)
	}
	h.current, h.version = vc.AccessToken, vc.Version
	h.issued[vc.AccessToken] = true
	return h, nil
}

// Token returns the current token.
func (h *tokenHolder) Token() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current
}

// Reconnect returns the ReconnectError once the broker has answered "sign-in
// required", else nil.
func (h *tokenHolder) Reconnect() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reconnect
}

// held reports whether req carries a token this holder handed out, to the API host.
func (h *tokenHolder) held(req *http.Request) bool {
	if req.URL.Host != h.apiHost {
		return false
	}
	tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.issued[tok]
}

// reject handles a 401 for rejected. It returns the token to retry with ("" or
// rejected itself means the 401 stands), or the ReconnectError.
func (h *tokenHolder) reject(ctx context.Context, rejected string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reconnect != nil {
		return "", h.reconnect
	}
	if h.current != rejected {
		return h.current, nil // a concurrent call already re-vended
	}
	cv, ok := h.vendor.(CredentialVendor)
	if h.revended || !ok {
		return "", nil
	}
	h.revended = true
	vc, err := cv.VendCredential(ctx, h.provider, h.version)
	if err != nil {
		if errors.Is(err, worklist.ErrSignInRequired) {
			h.reconnect = err
			return "", err
		}
		slog.WarnContext(ctx, "re-vend after a 401 failed", "provider", h.provider, "err", err)
		return "", nil
	}
	if vc.AccessToken == "" {
		return "", nil
	}
	h.current, h.version = vc.AccessToken, vc.Version
	h.issued[vc.AccessToken] = true
	return vc.AccessToken, nil
}

// client returns a copy of base whose transport is the holder's.
func (h *tokenHolder) client(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	c := *base
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = &holderTransport{h: h, base: rt}
	return &c
}

type holderTransport struct {
	h    *tokenHolder
	base http.RoundTripper
}

func (t *holderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.h.held(req) {
		return t.base.RoundTrip(req)
	}
	if err := t.h.Reconnect(); err != nil {
		closeBody(req)
		return nil, err
	}
	cur := t.h.Token()
	resp, err := t.base.RoundTrip(withToken(req, cur))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	next, rerr := t.h.reject(req.Context(), cur)
	if rerr != nil {
		drainClose(resp)
		return nil, rerr
	}
	if next == "" || next == cur {
		return resp, nil
	}
	retry := withToken(req, next)
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return resp, nil // cannot resend the body; the 401 stands
		}
		body, err := req.GetBody()
		if err != nil {
			return resp, nil
		}
		retry.Body = body
	}
	drainClose(resp)
	return t.base.RoundTrip(retry)
}

// withToken is a shallow clone of req carrying token. A RoundTripper must not
// modify the request it was given.
func withToken(req *http.Request, token string) *http.Request {
	r := req.Clone(req.Context())
	r.Body = req.Body
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// reconnectBox wraps a ToolBox so a tool call that finds the user's access gone
// ends the turn: it returns the ReconnectError however the tool reported the
// failure (the GitHub client may have flattened it into a status message).
type reconnectBox struct {
	worklist.ToolBox
	h *tokenHolder
}

func (b reconnectBox) Invoke(ctx context.Context, name string, args json.RawMessage) (string, error) {
	out, err := b.ToolBox.Invoke(ctx, name, args)
	if rerr := b.h.Reconnect(); rerr != nil {
		return "", rerr
	}
	return out, err
}

// isSignInRequired reports whether err is the broker's "sign-in required".
func isSignInRequired(err error) bool { return errors.Is(err, worklist.ErrSignInRequired) }
