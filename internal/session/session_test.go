package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var testSecret = []byte("test-session-secret-0123456789ab")

// carry returns a request bearing the cookies a response set, as a browser would.
func carry(rec *httptest.ResponseRecorder) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		r.AddCookie(c)
	}
	return r
}

func login(t *testing.T, m *Manager, u *User) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := m.Authenticate(rec, httptest.NewRequest(http.MethodGet, "/", nil), u); err != nil {
		t.Fatal(err)
	}
	return carry(rec)
}

// recordingStore captures the keys a Manager uses, to prove it never hands a
// Store the raw session id.
type recordingStore struct {
	*MemoryStore
	keys []string
}

func (s *recordingStore) Save(ctx context.Context, key string, u User, exp time.Time) error {
	s.keys = append(s.keys, key)
	return s.MemoryStore.Save(ctx, key, u, exp)
}

func TestSessionSurvivesANewManagerOverTheSameStore(t *testing.T) {
	store := NewMemoryStore()
	req := login(t, NewManager(testSecret, false, WithStore(store)), &User{ID: "github:1", Login: "octo"})

	// A "restart": a new Manager with the same secret over the same store.
	restarted := NewManager(testSecret, false, WithStore(store))
	u := restarted.CurrentUser(req)
	if u == nil || u.ID != "github:1" || u.Login != "octo" {
		t.Fatalf("session did not survive: %+v", u)
	}

	// With a fresh (empty) store the same cookie resolves to nothing.
	if u := NewManager(testSecret, false).CurrentUser(req); u != nil {
		t.Fatalf("cookie resolved without a stored session: %+v", u)
	}
}

func TestStoreKeyIsAHashNotTheSessionID(t *testing.T) {
	store := &recordingStore{MemoryStore: NewMemoryStore()}
	m := NewManager(testSecret, false, WithStore(store))
	req := login(t, m, &User{ID: "github:1"})

	id := m.id(req)
	if id == "" || len(store.keys) != 1 {
		t.Fatalf("id=%q keys=%v", id, store.keys)
	}
	if store.keys[0] == id {
		t.Fatal("the store was given the raw session id")
	}
	if store.keys[0] != storeKey(id) || len(store.keys[0]) != 64 {
		t.Fatalf("store key %q is not the SHA-256 of the id", store.keys[0])
	}
}

func TestSessionExpires(t *testing.T) {
	store := NewMemoryStore()
	m := NewManager(testSecret, false, WithStore(store))
	now := time.Now()
	m.now = func() time.Time { return now }
	req := login(t, m, &User{ID: "github:1"})

	now = now.Add(defaultMaxAge - time.Second)
	if m.CurrentUser(req) == nil {
		t.Fatal("session expired early")
	}
	now = now.Add(2 * time.Second)
	if m.CurrentUser(req) != nil {
		t.Fatal("session outlived its max age")
	}
	// The expired entry was removed on that read.
	if _, _, err := store.Load(context.Background(), storeKey(m.id(req))); err != ErrNotFound {
		t.Fatalf("expired session still stored: %v", err)
	}
}

func TestSweepEvictsExpiredSessionsAndFlows(t *testing.T) {
	store := NewMemoryStore()
	m := NewManager(testSecret, false, WithStore(store))
	now := time.Now()
	m.now = func() time.Time { return now }
	req := login(t, m, &User{ID: "github:1"})
	m.StartOAuth(httptest.NewRecorder(), &OAuthFlow{Provider: "github"})

	now = now.Add(defaultMaxAge + time.Minute)
	m.sweep(context.Background())

	if _, _, err := store.Load(context.Background(), storeKey(m.id(req))); err != ErrNotFound {
		t.Fatalf("sweep left an expired session: %v", err)
	}
	if len(m.flows) != 0 {
		t.Fatalf("sweep left %d expired flows", len(m.flows))
	}
}

func TestOAuthFlowStaysInMemoryAndIsShortLived(t *testing.T) {
	store := &recordingStore{MemoryStore: NewMemoryStore()}
	m := NewManager(testSecret, false, WithStore(store))
	now := time.Now()
	m.now = func() time.Time { return now }

	rec := httptest.NewRecorder()
	m.StartOAuth(rec, &OAuthFlow{Provider: "github", State: "s", Verifier: "v"})
	req := carry(rec)

	if f := m.OAuthFlow(req); f == nil || f.State != "s" || f.Verifier != "v" {
		t.Fatalf("flow not found: %+v", f)
	}
	if len(store.keys) != 0 {
		t.Fatal("pre-auth flow state reached the store")
	}
	if m.CurrentUser(req) != nil {
		t.Fatal("a pre-auth session must not be an authenticated one")
	}
	now = now.Add(flowMaxAge + time.Second)
	if m.OAuthFlow(req) != nil {
		t.Fatal("flow outlived its max age")
	}
}

func TestAuthenticateRotatesTheIDAndDestroyEndsTheSession(t *testing.T) {
	m := NewManager(testSecret, false)
	rec := httptest.NewRecorder()
	m.StartOAuth(rec, &OAuthFlow{Provider: "github"})
	pre := carry(rec)

	rec = httptest.NewRecorder()
	if err := m.Authenticate(rec, pre, &User{ID: "github:1"}); err != nil {
		t.Fatal(err)
	}
	post := carry(rec)
	if m.id(pre) == m.id(post) {
		t.Fatal("session id was not rotated on login")
	}
	if m.OAuthFlow(pre) != nil || m.CurrentUser(pre) != nil {
		t.Fatal("the pre-auth session id is still live after login")
	}
	if m.CurrentUser(post) == nil {
		t.Fatal("no session after login")
	}

	m.Destroy(httptest.NewRecorder(), post)
	if m.CurrentUser(post) != nil {
		t.Fatal("session survived Destroy")
	}
}

func TestTamperedCookieIsRejected(t *testing.T) {
	m := NewManager(testSecret, false)
	req := login(t, m, &User{ID: "github:1"})
	c, _ := req.Cookie(cookieName)

	forged := httptest.NewRequest(http.MethodGet, "/", nil)
	forged.AddCookie(&http.Cookie{Name: cookieName, Value: c.Value + "x"})
	if m.CurrentUser(forged) != nil {
		t.Fatal("a cookie with a bad signature was accepted")
	}
}
