// Package session provides server-side sessions backed by signed cookies.
//
// A random opaque session ID is stored in an HttpOnly cookie that is
// authenticated with HMAC-SHA256 so it cannot be forged or tampered with.
// Session data itself is kept server-side, so no sensitive information is ever
// exposed to the client. The session ID rotates on login (anti-fixation).
//
// Authenticated sessions are held in a Store, keyed by a hash of the session ID
// (see store.go); the default Store is in memory and a durable one makes logins
// survive a restart.
package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	cookieName    = "gofer_session"
	idBytes       = 32
	defaultMaxAge = 24 * time.Hour
	// flowMaxAge bounds a pre-auth OAuth handshake: redirect to callback.
	flowMaxAge = 10 * time.Minute
)

// User is the authenticated principal stored in a session.
type User struct {
	ID       string `json:"id"`       // stable, namespaced as "provider:subject"
	Provider string `json:"provider"` // google | github | microsoft
	Email    string `json:"email"`
	Name     string `json:"name"`
	Login    string `json:"login,omitempty"` // provider handle (e.g. GitHub login), for @-mentions; may be empty
}

// OAuthFlow holds the transient state needed to complete an OAuth exchange.
type OAuthFlow struct {
	Provider string
	State    string
	Verifier string // PKCE code verifier
	// UserID and SessionKey bind a grant flow (StartGrant) to the signed-in user who
	// began it and to that very session (SessionKey, its hashed id): the callback
	// refuses to complete it for anyone else, or from another session of the same
	// user (one signed in again after a logout). Empty for a sign-in flow.
	UserID     string
	SessionKey string
}

// pendingFlow is a pre-auth session: an OAuth handshake in progress.
type pendingFlow struct {
	flow      *OAuthFlow
	expiresAt time.Time
}

// Manager issues and resolves sessions. Authenticated sessions live in a Store
// (in memory by default, durable with WithStore) keyed by a hash of the session
// id. Pre-auth OAuth flow state is always kept in memory: it lives only for the
// seconds between the redirect to the provider and the callback, so a restart
// costs the user one more click, and PKCE verifiers never reach disk.
type Manager struct {
	secret []byte
	secure bool
	maxAge time.Duration
	store  Store
	now    func() time.Time // the clock; replaced in tests
	allow  func(*User) bool // nil: every stored session is honoured

	mu     sync.Mutex
	flows  map[string]*pendingFlow // session id -> in-progress OAuth flow
	grants map[string]*pendingFlow // grant cookie id -> in-progress grant flow (grant.go)
}

// Option configures a Manager.
type Option func(*Manager)

// WithAllow makes CurrentUser honour a stored session only while allow(user) holds,
// so tightening the sign-in allow-list also ends existing sessions of accounts it no
// longer lists, instead of letting them run to expiry. The session itself is kept: an
// account put back on the list is signed in again.
func WithAllow(allow func(*User) bool) Option {
	return func(m *Manager) { m.allow = allow }
}

// WithStore makes authenticated sessions live in store instead of in memory, so
// logins survive a restart when the store is durable.
func WithStore(store Store) Option {
	return func(m *Manager) {
		if store != nil {
			m.store = store
		}
	}
}

// NewManager creates a session manager. secret authenticates cookies and must be
// kept private; secure controls the Secure cookie flag (enable for HTTPS).
func NewManager(secret []byte, secure bool, opts ...Option) *Manager {
	m := &Manager{
		secret: secret,
		secure: secure,
		maxAge: defaultMaxAge,
		store:  NewMemoryStore(),
		now:    time.Now,
		flows:  make(map[string]*pendingFlow),
		grants: make(map[string]*pendingFlow),
	}
	for _, opt := range opts {
		opt(m)
	}
	go m.gc()
	return m
}

// gc periodically evicts expired pre-auth flows and expired stored sessions.
func (m *Manager) gc() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.sweep(context.Background())
	}
}

// sweep runs one eviction pass.
func (m *Manager) sweep(ctx context.Context) {
	now := m.now()
	m.mu.Lock()
	for id, f := range m.flows {
		if now.After(f.expiresAt) {
			delete(m.flows, id)
		}
	}
	for id, f := range m.grants {
		if now.After(f.expiresAt) {
			delete(m.grants, id)
		}
	}
	m.mu.Unlock()
	if err := m.store.DeleteExpired(ctx, now); err != nil {
		slog.Default().Warn("session: expiring stored sessions failed", "err", err)
	}
}

// id returns the authenticated session id carried by the request's cookie, or "".
// The id is trusted only as far as its signature: whether a session exists for it
// is the caller's lookup.
func (m *Manager) id(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	id, ok := m.verify(c.Value)
	if !ok {
		return ""
	}
	return id
}

// storeKey is the key a session is stored under: the SHA-256 of its id. The raw
// id exists only in the user's cookie, so a copy of the store cannot be replayed
// as one. The id is 256 random bits, so an unsalted hash is sufficient.
func storeKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// setCookie writes the signed session cookie for id.
func (m *Manager) setCookie(w http.ResponseWriter, id string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    m.sign(id),
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}

// StartOAuth creates a pre-auth session that remembers the OAuth flow state.
func (m *Manager) StartOAuth(w http.ResponseWriter, flow *OAuthFlow) {
	id := randomToken(idBytes)
	m.mu.Lock()
	m.flows[id] = &pendingFlow{flow: flow, expiresAt: m.now().Add(flowMaxAge)}
	m.mu.Unlock()
	m.setCookie(w, id, flowMaxAge)
}

// OAuthFlow returns the in-progress OAuth flow for the request, if any.
func (m *Manager) OAuthFlow(r *http.Request) *OAuthFlow {
	id := m.id(r)
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.flows[id]
	if f == nil {
		return nil
	}
	if m.now().After(f.expiresAt) {
		delete(m.flows, id)
		return nil
	}
	return f.flow
}

// drop removes whatever the request's session id refers to: a pre-auth flow, a
// stored session, or neither.
func (m *Manager) drop(r *http.Request) {
	id := m.id(r)
	if id == "" {
		return
	}
	m.mu.Lock()
	delete(m.flows, id)
	m.mu.Unlock()
	if err := m.store.Delete(r.Context(), storeKey(id)); err != nil {
		slog.Default().Warn("session: deleting stored session failed", "err", err)
	}
}

// Authenticate upgrades the current session to an authenticated one. A new
// session ID is issued to prevent session fixation. It fails, setting no cookie,
// if the session cannot be stored.
func (m *Manager) Authenticate(w http.ResponseWriter, r *http.Request, u *User) error {
	m.drop(r)
	id := randomToken(idBytes)
	if err := m.store.Save(r.Context(), storeKey(id), *u, m.now().Add(m.maxAge)); err != nil {
		return err
	}
	m.setCookie(w, id, m.maxAge)
	return nil
}

// CurrentUser returns the authenticated user for the request, or nil.
func (m *Manager) CurrentUser(r *http.Request) *User {
	u, err := m.Lookup(r)
	if err != nil {
		slog.Default().Warn("session: loading stored session failed", "err", err)
		return nil
	}
	return u
}

// Lookup is CurrentUser that tells "no session" (nil, nil) apart from a store that
// could not answer (nil, err). A caller about to end the session needs the
// difference: a store failure must not look like a missing session.
func (m *Manager) Lookup(r *http.Request) (*User, error) {
	return m.lookup(r, true)
}

// LookupOwner is Lookup without the sign-in allow-list: whose valid, unexpired
// session this is, by its signed cookie, even if the account may no longer sign in.
// Logout uses it, so an account removed from the allow-list can still revoke what
// it granted. Never use it to grant interactive access.
func (m *Manager) LookupOwner(r *http.Request) (*User, error) {
	return m.lookup(r, false)
}

func (m *Manager) lookup(r *http.Request, applyAllow bool) (*User, error) {
	id := m.id(r)
	if id == "" {
		return nil, nil
	}
	key := storeKey(id)
	u, expiresAt, err := m.store.Load(r.Context(), key)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if m.now().After(expiresAt) {
		_ = m.store.Delete(r.Context(), key)
		return nil, nil
	}
	if applyAllow && m.allow != nil && !m.allow(&u) {
		return nil, nil
	}
	return &u, nil
}

// Consumer is a Store that deletes an entry and reports whether it was there, in
// one step. MemoryStore implements it.
type Consumer interface {
	Consume(ctx context.Context, key string) (bool, error)
}

// ConsumeStored deletes the stored session under storeKey and reports whether this
// call removed it, so of two overlapping callers exactly one sees true. A store
// that is not a Consumer gets a Load then a Delete, which is not atomic.
func (m *Manager) ConsumeStored(ctx context.Context, storeKey string) (bool, error) {
	if c, ok := m.store.(Consumer); ok {
		return c.Consume(ctx, storeKey)
	}
	if _, _, err := m.store.Load(ctx, storeKey); errors.Is(err, ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, m.store.Delete(ctx, storeKey)
}

// EndWith ends the request's session through commit, which must delete the stored
// session (storeKey, the hashed id a Store sees) together with whatever the caller
// records about the session's end, in one commit where the store allows it. Only if
// commit succeeds is the session forgotten and its cookie cleared; if it fails,
// nothing changes and its error is returned, so the caller can report that sign-out
// did not happen. The store's own Delete follows a successful commit as a backstop
// for a commit that could not reach this manager's store (an in-memory one); its
// failure is logged. storeKey is "" when the request carries no session.
func (m *Manager) EndWith(w http.ResponseWriter, r *http.Request, commit func(ctx context.Context, storeKey string) error) error {
	id := m.id(r)
	key := ""
	if id != "" {
		key = storeKey(id)
	}
	if err := commit(r.Context(), key); err != nil {
		return err
	}
	m.drop(r)
	m.clearCookie(w)
	return nil
}

// Destroy removes the session and clears the cookie.
func (m *Manager) Destroy(w http.ResponseWriter, r *http.Request) {
	m.drop(r)
	m.clearCookie(w)
}

// clearCookie expires the session cookie.
func (m *Manager) clearCookie(w http.ResponseWriter) {
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
