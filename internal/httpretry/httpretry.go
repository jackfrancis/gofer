// Package httpretry wraps an http.Client so transient connectivity failures — a
// flaky cluster DNS resolver, a provider hiccup — are retried instead of failing
// the call outright. It is shared by the agent runtime (its GitHub/model calls)
// and the web tier's OAuth client (the login token exchange), which would
// otherwise turn a one-off "server misbehaving" DNS blip into a hard failure.
//
// The retry scope is deliberately conservative to avoid duplicating a write: a
// connection-phase failure (DNS or dial — the request never reached the server)
// and a 429/502/503 (refused or unavailable — not processed) are retried for any
// method, while a mid-flight error or a 504 (the upstream may have processed the
// request) is retried only for an idempotent method. The unavailable family
// (429/502/503) is retried on its own, more patient budget, and a 429's
// Retry-After header is honored (capped) as the backoff.
package httpretry

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Default retry policy for a transient failure (a connection blip or an
// idempotent 5xx): a few attempts with jittered exponential backoff.
const (
	DefaultAttempts    = 3
	DefaultBaseBackoff = 200 * time.Millisecond
	DefaultMaxBackoff  = 2 * time.Second
)

// Default retry policy for a temporarily-unavailable server — a 429 (rate limit)
// or a 502/503: more attempts and a longer backoff than a connection blip. The
// request context / client Timeout still bounds the total.
const (
	DefaultRateLimitAttempts    = 8
	DefaultRateLimitBaseBackoff = 1 * time.Second
	DefaultRateLimitMaxBackoff  = maxRetryAfter
)

// maxRetryAfter caps how long a Retry-After header can make one request wait.
const maxRetryAfter = 30 * time.Second

// Wrap returns a copy of c whose transport retries transient connectivity
// failures with the default policy. A nil client gets a fresh one; an
// already-wrapped client is returned unchanged.
func Wrap(c *http.Client) *http.Client {
	return wrap(c,
		DefaultAttempts, DefaultBaseBackoff, DefaultMaxBackoff,
		DefaultRateLimitAttempts, DefaultRateLimitBaseBackoff, DefaultRateLimitMaxBackoff)
}

// WrapN is Wrap with explicit knobs applied uniformly to both the transient and
// the rate-limit retry path (tests use a tiny backoff). attempts < 1 collapses to
// a single try.
func WrapN(c *http.Client, attempts int, base, max time.Duration) *http.Client {
	return wrap(c, attempts, base, max, attempts, base, max)
}

// wrap installs the retry transport with distinct transient and rate-limit
// budgets. It preserves an already-wrapped client and the client's other fields.
func wrap(c *http.Client, attempts int, base, max time.Duration, rlAttempts int, rlBase, rlMax time.Duration) *http.Client {
	if c == nil {
		c = &http.Client{}
	}
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	if _, ok := rt.(*transport); ok {
		return c
	}
	clone := *c
	clone.Transport = &transport{
		base:        rt,
		attempts:    attempts,
		baseBackoff: base,
		maxBackoff:  max,
		rlAttempts:  rlAttempts,
		rlBase:      rlBase,
		rlMax:       rlMax,
	}
	return &clone
}

// transport wraps a base RoundTripper with the bounded retry policy.
type transport struct {
	base http.RoundTripper

	attempts    int
	baseBackoff time.Duration
	maxBackoff  time.Duration

	rlAttempts int
	rlBase     time.Duration
	rlMax      time.Duration
}

// RoundTrip sends req, retrying transient failures and rate limits on separate
// budgets. The whole loop runs inside one Client.Do call, so the client's Timeout
// (and the request context deadline) bound all attempts and backoffs together.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	transientLeft := t.attempts - 1
	if transientLeft < 0 {
		transientLeft = 0
	}
	rateLimitLeft := t.rlAttempts - 1
	if rateLimitLeft < 0 {
		rateLimitLeft = 0
	}

	var (
		resp *http.Response
		err  error
	)
	for {
		resp, err = t.base.RoundTrip(req)
		if !shouldRetry(req.Method, resp, err) {
			return resp, err
		}

		var (
			left      *int
			total     int
			base, max time.Duration
		)
		if err == nil && resp != nil && isUnavailable(resp.StatusCode) {
			left, total, base, max = &rateLimitLeft, t.rlAttempts, t.rlBase, t.rlMax
		} else {
			left, total, base, max = &transientLeft, t.attempts, t.baseBackoff, t.maxBackoff
		}
		if *left <= 0 {
			return resp, err
		}
		attempt := total - *left
		*left--

		rewound, rerr := rewind(req)
		if rerr != nil {
			return resp, err
		}
		req = rewound
		drain(resp)
		if werr := waitBeforeRetry(req.Context(), resp, base, max, attempt); werr != nil {
			return nil, werr
		}
	}
}

// rewind returns req ready to be sent again. A bodyless request is returned
// as-is; a body request needs GetBody so the body can be re-read.
func rewind(req *http.Request) (*http.Request, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("httpretry: request body is not rewindable")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, nil
}

func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
}

// waitBeforeRetry blocks before a retry, returning early if the context is
// cancelled. A 429 with a Retry-After header uses that delay (capped);
// otherwise an exponentially growing, half-jittered backoff.
func waitBeforeRetry(ctx context.Context, prev *http.Response, base, max time.Duration, attempt int) error {
	wait := jitteredBackoff(base, max, attempt)
	if d, ok := retryAfter(prev); ok {
		wait = d
		if wait > maxRetryAfter {
			wait = maxRetryAfter
		}
	}
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// jitteredBackoff is an exponentially growing, half-jittered delay for the given
// (1-based) attempt, capped at max.
func jitteredBackoff(base, max time.Duration, attempt int) time.Duration {
	d := base << (attempt - 1)
	if d <= 0 || d > max {
		d = max
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// retryAfter parses a response's Retry-After header (seconds or an HTTP-date).
func retryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// shouldRetry decides whether to repeat a request given its result. A request
// that never reached the server (connection-phase error) is always safe to
// repeat; anything that may have been processed is repeated only for an
// idempotent method.
func shouldRetry(method string, resp *http.Response, err error) bool {
	if err != nil {
		if isConnectionError(err) {
			return true
		}
		return isIdempotent(method)
	}
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
		return true
	case http.StatusGatewayTimeout:
		return isIdempotent(method)
	}
	return false
}

// isUnavailable reports whether a status means the server was temporarily unable
// to process the request — rate-limited (429) or unavailable (502/503).
func isUnavailable(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// isConnectionError reports whether err occurred before the request could be
// delivered — a DNS failure or a dial/connect failure.
func isConnectionError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}
