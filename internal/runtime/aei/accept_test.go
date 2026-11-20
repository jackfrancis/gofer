package aei

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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
// none: it answered with a refusal, or the request never reached it. A lost answer
// leaves acceptance unknown.
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
		{"server error", answer(http.StatusInternalServerError, ``), true},
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
