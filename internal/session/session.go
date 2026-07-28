// Package session provides server-side sessions backed by signed cookies.
//
// A random opaque session ID is stored in an HttpOnly cookie that is
// authenticated with HMAC-SHA256 so it cannot be forged or tampered with.
// Session data itself is kept server-side, so no sensitive information is ever
// exposed to the client. The session ID rotates on login (anti-fixation).
package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	cookieName    = "gofer_session"
	idBytes       = 32
	defaultMaxAge = 24 * time.Hour
)

// User is the authenticated principal stored in a session.
type User struct {
	ID       string `json:"id"`       // stable, namespaced as "provider:subject"
	Provider string `json:"provider"` // google | github | microsoft
	Email    string `json:"email"`
	Name     string `json:"name"`
	Login    string `json:"login,omitempty"` // provider handle (e.g. GitHub login), for @-mentions; may be empty
}

// data is the server-side payload associated with a session ID.
type data struct {
	user      *User
	oauth     *OAuthFlow // transient, only set during the login handshake
	expiresAt time.Time
}

// OAuthFlow holds the transient state needed to complete an OAuth exchange.
type OAuthFlow struct {
	Provider string
	State    string
	Verifier string // PKCE code verifier
}

// Manager is a concurrency-safe, in-memory session store.
type Manager struct {
	secret []byte
	secure bool
	maxAge time.Duration

	mu       sync.Mutex
	sessions map[string]*data
}

// NewManager creates a session manager. secret authenticates cookies and must be
// kept private; secure controls the Secure cookie flag (enable for HTTPS).
func NewManager(secret []byte, secure bool) *Manager {
	m := &Manager{
		secret:   secret,
		secure:   secure,
		maxAge:   defaultMaxAge,
		sessions: make(map[string]*data),
	}
	go m.gc()
	return m
}

// gc periodically evicts expired sessions.
func (m *Manager) gc() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		for id, d := range m.sessions {
			if now.After(d.expiresAt) {
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
	}
}

// get returns the live session for the request, or nil if none/expired.
func (m *Manager) get(r *http.Request) (string, *data) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", nil
	}
	id, ok := m.verify(c.Value)
	if !ok {
		return "", nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.sessions[id]
	if d == nil || time.Now().After(d.expiresAt) {
		delete(m.sessions, id)
		return "", nil
	}
	return id, d
}

// newSession allocates a fresh session ID and writes the signed cookie.
func (m *Manager) newSession(w http.ResponseWriter, d *data) {
	id := randomToken(idBytes)
	d.expiresAt = time.Now().Add(m.maxAge)
	m.mu.Lock()
	m.sessions[id] = d
	m.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    m.sign(id),
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(m.maxAge.Seconds()),
	})
}

// StartOAuth creates a pre-auth session that remembers the OAuth flow state.
func (m *Manager) StartOAuth(w http.ResponseWriter, flow *OAuthFlow) {
	m.newSession(w, &data{oauth: flow})
}

// OAuthFlow returns the in-progress OAuth flow for the request, if any.
func (m *Manager) OAuthFlow(r *http.Request) *OAuthFlow {
	_, d := m.get(r)
	if d == nil {
		return nil
	}
	return d.oauth
}

// Authenticate upgrades the current session to an authenticated one. A new
// session ID is issued to prevent session fixation.
func (m *Manager) Authenticate(w http.ResponseWriter, r *http.Request, u *User) {
	if id, _ := m.get(r); id != "" {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}
	m.newSession(w, &data{user: u})
}

// CurrentUser returns the authenticated user for the request, or nil.
func (m *Manager) CurrentUser(r *http.Request) *User {
	_, d := m.get(r)
	if d == nil {
		return nil
	}
	return d.user
}

// Destroy removes the session and clears the cookie.
func (m *Manager) Destroy(w http.ResponseWriter, r *http.Request) {
	if id, _ := m.get(r); id != "" {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// sign returns "id.signature" where signature authenticates the id.
func (m *Manager) sign(id string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(id))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return id + "." + sig
}

// verify checks the signature and returns the session id if valid.
func (m *Manager) verify(value string) (string, bool) {
	dot := -1
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot == len(value)-1 {
		return "", false
	}
	id, sig := value[:dot], value[dot+1:]
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(id))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return "", false
	}
	return id, true
}

// randomToken returns a hex-encoded cryptographically random token.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("session: failed to read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewToken exposes a cryptographically random token for OAuth state/PKCE.
func NewToken() string { return randomToken(idBytes) }
