package httpretry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// An HTTP-date Retry-After equal to the request's deadline cannot be met: the 429
// is returned at once, not drained and waited on into the deadline's error. This
// is the end-to-end behaviour; it catches a second clock reading only when the
// clock ticks between the two (about a third of runs here), so the deterministic
// guard is TestRetryAfterUsesGivenClock.
func TestRetryAfterDateEqualToDeadline(t *testing.T) {
	// A whole second, so the HTTP-date (second resolution) is exactly the deadline.
	deadline := time.Now().Truncate(time.Second).Add(2 * time.Second)
	var calls atomic.Int32
	c := Wrap(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		h := http.Header{}
		h.Set("Retry-After", deadline.UTC().Format(http.TimeFormat))
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h,
			Body: io.NopCloser(strings.NewReader("slow down")), Request: r}, nil
	})})
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test/", nil)
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("err = %v after %s, want the 429", err, time.Since(start))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || string(body) != "slow down" || calls.Load() != 1 {
		t.Errorf("status %d, body %q, %d calls", resp.StatusCode, body, calls.Load())
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("returned after %s: it waited", d)
	}
}

// retryAfter measures an HTTP-date from the clock reading it is given, never a
// fresh one, so the loop's delay and retry moment share one reading.
func TestRetryAfterUsesGivenClock(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	resp := &http.Response{Header: http.Header{"Retry-After": {now.Add(3 * time.Second).Format(http.TimeFormat)}}}
	if d, ok := retryAfter(resp, now); !ok || d != 3*time.Second {
		t.Errorf("retryAfter = %s, %v; want exactly 3s", d, ok)
	}
}
