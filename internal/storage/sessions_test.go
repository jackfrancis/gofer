package storage

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
)

func readIfExists(dir, name string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

func sessionImpls(t *testing.T) map[string]session.Store {
	return map[string]session.Store{
		"memory": session.NewMemoryStore(),
		"sqlite": open(t, t.TempDir()).Sessions(),
	}
}

func TestSessionStoreParity(t *testing.T) {
	alice := session.User{ID: "github:1", Provider: "github", Email: "a@example.com", Name: "Alice", Login: "alice"}
	bob := session.User{ID: "github:2", Provider: "github"}
	base := time.Unix(1_800_000_000, 0)

	cases := []struct {
		name string
		run  func(t *testing.T, s session.Store)
	}{
		{"load of a missing key is ErrNotFound", func(t *testing.T, s session.Store) {
			if _, _, err := s.Load(ctx, "nope"); !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		}},
		{"save then load round-trips the user and expiry", func(t *testing.T, s session.Store) {
			if err := s.Save(ctx, "k1", alice, base); err != nil {
				t.Fatal(err)
			}
			u, exp, err := s.Load(ctx, "k1")
			if err != nil {
				t.Fatal(err)
			}
			if u != alice || !exp.Equal(base) {
				t.Fatalf("got %+v until %v", u, exp)
			}
		}},
		{"keys are independent; save replaces", func(t *testing.T, s session.Store) {
			_ = s.Save(ctx, "k1", alice, base)
			_ = s.Save(ctx, "k2", bob, base)
			_ = s.Save(ctx, "k1", bob, base.Add(time.Hour))
			u, exp, _ := s.Load(ctx, "k1")
			if u != bob || !exp.Equal(base.Add(time.Hour)) {
				t.Fatalf("k1 = %+v until %v", u, exp)
			}
			if u, _, _ := s.Load(ctx, "k2"); u != bob {
				t.Fatalf("k2 = %+v", u)
			}
		}},
		{"delete removes one key and tolerates a missing one", func(t *testing.T, s session.Store) {
			_ = s.Save(ctx, "k1", alice, base)
			_ = s.Save(ctx, "k2", bob, base)
			if err := s.Delete(ctx, "k1"); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, "k1"); err != nil {
				t.Fatalf("second delete: %v", err)
			}
			if _, _, err := s.Load(ctx, "k1"); !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("err = %v", err)
			}
			if _, _, err := s.Load(ctx, "k2"); err != nil {
				t.Fatalf("k2 was deleted too: %v", err)
			}
		}},
		{"DeleteExpired removes sessions at or past their expiry, and only those", func(t *testing.T, s session.Store) {
			_ = s.Save(ctx, "past", alice, base.Add(-time.Hour))
			_ = s.Save(ctx, "now", alice, base)
			_ = s.Save(ctx, "future", alice, base.Add(time.Hour))
			if err := s.DeleteExpired(ctx, base); err != nil {
				t.Fatal(err)
			}
			for key, gone := range map[string]bool{"past": true, "now": true, "future": false} {
				_, _, err := s.Load(ctx, key)
				if gone != errors.Is(err, session.ErrNotFound) {
					t.Errorf("%s: err = %v, want gone=%v", key, err, gone)
				}
			}
		}},
	}
	for _, tc := range cases {
		for name, s := range sessionImpls(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) { tc.run(t, s) })
		}
	}
}

// The property the feature exists for: a login made before a restart is honored
// after it, by a new process (new DB handle, new Manager) with the same cookie.
func TestLoginSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	secret := []byte("test-session-secret-0123456789ab")

	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m := session.NewManager(secret, false, session.WithStore(db.Sessions()))
	if err := m.Authenticate(rec, httptest.NewRequest(http.MethodGet, "/", nil), &session.User{ID: "github:1", Login: "alice"}); err != nil {
		t.Fatal(err)
	}
	cookie := rec.Result().Cookies()[0]

	// Before closing: the session id in the cookie is not in the database file or
	// its write-ahead log; only its hash is.
	rawID := cookie.Value[:bytes.IndexByte([]byte(cookie.Value), '.')]
	for _, name := range []string{FileName, FileName + "-wal"} {
		raw, err := readIfExists(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(rawID)) {
			t.Fatalf("%s contains the raw session id", name)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	restarted := session.NewManager(secret, false, session.WithStore(open(t, dir).Sessions()))
	u := restarted.CurrentUser(req)
	if u == nil || u.ID != "github:1" || u.Login != "alice" {
		t.Fatalf("login did not survive the restart: %+v", u)
	}

	// Logging out removes the row, so the cookie is dead after the next restart too.
	restarted.Destroy(httptest.NewRecorder(), req)
	if restarted.CurrentUser(req) != nil {
		t.Fatal("session survived logout")
	}
}
