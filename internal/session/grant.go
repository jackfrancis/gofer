package session

import (
	"net/http"
)

// A grant flow is an OAuth handshake a signed-in user runs to give gofer an extra,
// separate grant (the write grant, ADR 0009) without signing in again. It must not
// touch the session cookie: StartOAuth replaces that cookie with a pre-auth one,
// which would sign the user out. So a grant flow has its own cookie, scoped to the
// grant's routes, and its own map, so a grant cookie's id never resolves as a
// sign-in flow or a session.

const grantCookieName = "gofer_grant"

// StartGrant remembers flow under a new grant cookie scoped to path. flow.UserID
// should name the signed-in user who began it.
func (m *Manager) StartGrant(w http.ResponseWriter, path string, flow *OAuthFlow) {
	id := randomToken(idBytes)
	m.mu.Lock()
	m.grants[id] = &pendingFlow{flow: flow, expiresAt: m.now().Add(flowMaxAge)}
	m.mu.Unlock()
	m.setGrantCookie(w, path, m.sign(id), int(flowMaxAge.Seconds()))
}

// SessionKey is the hashed id of the session the request's cookie names (what a
// Store keys it by), or "" for none. It says which session, not whether it is
// still valid: pair it with Lookup.
func (m *Manager) SessionKey(r *http.Request) string {
	id := m.id(r)
	if id == "" {
		return ""
	}
	return storeKey(id)
}

// GrantFlow returns the request's in-progress grant flow, or nil.
func (m *Manager) GrantFlow(r *http.Request) *OAuthFlow {
	id := m.grantID(r)
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.grants[id]
	if f == nil {
		return nil
	}
	if m.now().After(f.expiresAt) {
		delete(m.grants, id)
		return nil
	}
	return f.flow
}

// EndGrant forgets the request's grant flow (it can complete at most once) and
// clears its cookie.
func (m *Manager) EndGrant(w http.ResponseWriter, r *http.Request, path string) {
	if id := m.grantID(r); id != "" {
		m.mu.Lock()
		delete(m.grants, id)
		m.mu.Unlock()
	}
	m.setGrantCookie(w, path, "", -1)
}

func (m *Manager) grantID(r *http.Request) string {
	c, err := r.Cookie(grantCookieName)
	if err != nil {
		return ""
	}
	id, ok := m.verify(c.Value)
	if !ok {
		return ""
	}
	return id
}

func (m *Manager) setGrantCookie(w http.ResponseWriter, path, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     grantCookieName,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		Secure:   m.secure,
		// Lax, like the session cookie: the provider's redirect back to the callback
		// is a top-level GET, which Lax admits.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}
