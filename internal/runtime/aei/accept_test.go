package aei

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackfrancis/gofer/internal/ingest"
)

func dispatcherFor(t *testing.T, endpoint string) *Dispatcher {
	t.Helper()
	d, err := New(Config{Endpoint: endpoint, App: "gofer", Token: "sa-token"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var ingestSpec = ingest.RunSpec{TaskRef: "github-ingest", Subject: "github:1"}

// Submit says the run was not accepted only when the control plane certainly holds
// none: it answered with a 3xx or 4xx refusal, or the request never reached it. A
// 5xx (a gateway may answer one after the control plane accepted the POST) or a lost
// answer leaves acceptance unknown.
func TestSubmitTellsARefusalFromALostAnswer(t *testing.T) {
	answer := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		refused bool
	}{
		{"overloaded", answer(http.StatusTooManyRequests, ``), true},
		{"forbidden", answer(http.StatusForbidden, `{"error":"not a permitted dispatcher"}`), true},
		{"bad request", answer(http.StatusBadRequest, ``), true},
		{"redirect not followed", answer(http.StatusNotModified, ``), true},
		{"server error", answer(http.StatusInternalServerError, ``), false},
		{"bad gateway", answer(http.StatusBadGateway, ``), false},
		{"unavailable", answer(http.StatusServiceUnavailable, ``), false},
		{"gateway timeout", answer(http.StatusGatewayTimeout, ``), false},
		{"accepted, unreadable id", answer(http.StatusAccepted, `{"runId":`), false},
		{"connection dropped after the request", func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := dispatcherFor(t, srv.URL).Submit(context.Background(), ingestSpec)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ingest.ErrNotAccepted); got != tc.refused {
				t.Fatalf("refused = %v, want %v (err: %v)", got, tc.refused, err)
			}
		})
	}

	t.Run("nothing listening", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		_, err = dispatcherFor(t, "http://"+addr).Submit(context.Background(), ingestSpec)
		if !errors.Is(err, ingest.ErrNotAccepted) {
			t.Fatalf("a refused connection is a refusal: %v", err)
		}
	})

	t.Run("no task ref", func(t *testing.T) {
		_, err := dispatcherFor(t, "http://127.0.0.1:1").Submit(context.Background(), ingest.RunSpec{})
		if !errors.Is(err, ingest.ErrNotAccepted) {
			t.Fatalf("a spec never sent is a refusal: %v", err)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		srv := httptest.NewServer(answer(http.StatusAccepted, `{"runId":"run-1"}`))
		defer srv.Close()
		if id, err := dispatcherFor(t, srv.URL).Submit(context.Background(), ingestSpec); id != "run-1" || err != nil {
			t.Fatalf("Submit = %q, %v", id, err)
		}
	})
}

// The backend watches its own copy of a caller's client; the caller's is unchanged.
func TestSubmitLeavesTheCallersClientAlone(t *testing.T) {
	hc := &http.Client{}
	if _, err := New(Config{Endpoint: "http://127.0.0.1:1", App: "gofer", Token: "t", HTTPClient: hc}); err != nil {
		t.Fatal(err)
	}
	if hc.Transport != nil {
		t.Fatal("New changed the caller's HTTP client")
	}
}

// Only the final attempt counts: a redirect followed to a target that loses its
// answer leaves acceptance unknown, while one followed to a refusal is a refusal.
func TestSubmitJudgesTheFinalAttempt(t *testing.T) {
	lost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	defer lost.Close()
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer busy.Close()
	for _, tc := range []struct {
		name    string
		target  string
		refused bool
	}{{"to a lost answer", lost.URL, false}, {"to a refusal", busy.URL, true}} {
		t.Run(tc.name, func(t *testing.T) {
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, tc.target+r.URL.Path, http.StatusTemporaryRedirect)
			}))
			defer front.Close()
			_, err := dispatcherFor(t, front.URL).Submit(context.Background(), ingestSpec)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ingest.ErrNotAccepted); got != tc.refused {
				t.Fatalf("refused = %v, want %v (err: %v)", got, tc.refused, err)
			}
		})
	}
}

// A dispatch whose context is already done is never sent, and says so.
func TestSubmitWithADoneContextIsARefusal(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dispatcherFor(t, srv.URL).Submit(ctx, ingestSpec)
	if !errors.Is(err, ingest.ErrNotAccepted) || hits.Load() != 0 {
		t.Fatalf("err %v, requests %d; want a refusal and no request", err, hits.Load())
	}
}
