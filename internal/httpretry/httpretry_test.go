package httpretry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A Retry-After that cannot be waited out before the deadline returns the 429
// itself, body intact, instead of sleeping into the deadline's error.
func TestRetryThatCannotFitReturnsResponse(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"slow down"}`))
	}))
	defer srv.Close()
	c := Wrap(&http.Client{Timeout: 2 * time.Second})
	start := time.Now()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("err = %v, want the 429 response", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || string(body) != `{"message":"slow down"}` {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
	if hits.Load() != 1 || time.Since(start) > time.Second {
		t.Errorf("hits = %d after %s: it waited instead of returning", hits.Load(), time.Since(start))
	}
}

// A wait that fits is still taken, and the retry's answer returned.
func TestRetryThatFitsRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	for name, c := range map[string]*http.Client{
		"with a deadline": Wrap(&http.Client{Timeout: 5 * time.Second}),
		"without one":     Wrap(&http.Client{}),
	} {
		hits.Store(0)
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
			t.Errorf("%s: status %d after %d hits", name, resp.StatusCode, hits.Load())
		}
	}
}

// The retry goes out when Retry-After asked, not that long after the 429's body has
// been drained: a slow body must not push it past the deadline.
func TestRetryAfterCountsDrainTime(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) > 1 {
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":`))
		w.(http.Flusher).Flush()
		time.Sleep(time.Second) // the body takes a second to drain
		_, _ = w.Write([]byte(`"slow down"}`))
	}))
	defer srv.Close()
	// Retry-After (2s) fits the deadline; Retry-After after a 1s drain (3s) would not.
	c := Wrap(&http.Client{Timeout: 2600 * time.Millisecond})
	start := time.Now()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("err = %v after %s, want the retry's 200", err, time.Since(start))
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
		t.Errorf("status %d after %d hits", resp.StatusCode, hits.Load())
	}
	if d := time.Since(start); d < 2*time.Second || d > 2500*time.Millisecond {
		t.Errorf("retried after %s, want about 2s", d)
	}
}
