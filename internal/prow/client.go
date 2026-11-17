package prow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/httpretry"
)

// DefaultBaseURL is the one host this package ever contacts.
const DefaultBaseURL = "https://storage.googleapis.com"

// ErrNotFound reports an object that does not exist.
var ErrNotFound = errors.New("not found")

// ErrTooLarge reports an object or listing over a limit.
var ErrTooLarge = errors.New("too large")

const maxListBody = 4 << 20 // one listing page of 1,000 objects is well under this

// Client reads public GCS objects and listings anonymously. It is never the GitHub
// client: it sends no Authorization or cookie, follows no redirect, and refuses any
// request to a host other than its base URL's.
type Client struct {
	http *http.Client
	base *url.URL
}

// NewClient builds a client for baseURL (empty uses DefaultBaseURL; tests point it
// at a stub). It always builds its own transport: reusing a caller's could put a
// credential-adding RoundTripper (an OAuth transport) underneath the guard.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Host == "" {
		base, _ = url.Parse(DefaultBaseURL)
	}
	var rt http.RoundTripper = http.DefaultTransport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		rt = t.Clone()
	}
	c := &http.Client{
		Timeout:   60 * time.Second,
		Transport: &guard{base: rt, scheme: base.Scheme, host: base.Host},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not followed")
		},
	}
	return &Client{http: httpretry.Wrap(c), base: base}
}

// guard is the last RoundTripper before the network: it strips credentials and
// refuses any host but the configured one, so no caller mistake can send a token or
// reach another service through this client.
type guard struct {
	base         http.RoundTripper
	scheme, host string
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != g.scheme || req.URL.Host != g.host {
		return nil, fmt.Errorf("prow client: refusing request to %s://%s", req.URL.Scheme, req.URL.Host)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("Proxy-Authorization") != "" {
		req = req.Clone(req.Context())
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		req.Header.Del("Proxy-Authorization")
	}
	return g.base.RoundTrip(req)
}

func (c *Client) newRequest(ctx context.Context, escapedPath, rawQuery string) (*http.Request, error) {
	full := c.base.Scheme + "://" + c.base.Host + escapedPath
	if rawQuery != "" {
		full += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gofer-agent")
	return req, nil
}

// escapeObject escapes each segment of an object name for a URL path.
func escapeObject(object string) string {
	segs := strings.Split(object, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Fetch streams bucket/object into w, failing with ErrTooLarge past max bytes. It
// returns the bytes written. The body is decompressed transparently when GCS serves
// it gzip-encoded, so max bounds what lands on disk.
func (c *Client) Fetch(ctx context.Context, bucket, object string, w io.Writer, max int64) (int64, error) {
	req, err := c.newRequest(ctx, "/"+url.PathEscape(bucket)+"/"+escapeObject(object), "")
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, transportError(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return 0, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	case resp.ContentLength > max:
		return 0, fmt.Errorf("%w: %s, over the %s limit", ErrTooLarge, human(resp.ContentLength), human(max))
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return n, transportError(err)
	}
	if n > max {
		return n, fmt.Errorf("%w: over the %s limit", ErrTooLarge, human(max))
	}
	return n, nil
}

// Object is one listed object.
type Object struct {
	Name string // full object name
	Size int64
}

// ListPage is one page of a listing.
type ListPage struct {
	Objects  []Object
	Prefixes []string // "subdirectories" when listed with a delimiter
	Next     string   // page token; empty when this is the last page
}

// List lists one page (up to 1,000 objects) under prefix. With delimiter, objects in
// deeper "directories" are rolled up into Prefixes.
func (c *Client) List(ctx context.Context, bucket, prefix string, delimiter bool, pageToken string) (ListPage, error) {
	q := url.Values{}
	q.Set("prefix", prefix)
	q.Set("maxResults", "1000")
	q.Set("fields", "items(name,size),prefixes,nextPageToken")
	if delimiter {
		q.Set("delimiter", "/")
	}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	req, err := c.newRequest(ctx, "/storage/v1/b/"+url.PathEscape(bucket)+"/o", q.Encode())
	if err != nil {
		return ListPage{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ListPage{}, transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ListPage{}, fmt.Errorf("listing: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBody+1))
	if err != nil {
		return ListPage{}, transportError(err)
	}
	if len(body) > maxListBody {
		return ListPage{}, fmt.Errorf("%w: listing response", ErrTooLarge)
	}
	var raw struct {
		Items []struct {
			Name string `json:"name"`
			Size string `json:"size"` // GCS encodes uint64 as a string
		} `json:"items"`
		Prefixes      []string `json:"prefixes"`
		NextPageToken string   `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ListPage{}, fmt.Errorf("listing: %v", err)
	}
	page := ListPage{Prefixes: raw.Prefixes, Next: raw.NextPageToken}
	for _, it := range raw.Items {
		n, _ := strconv.ParseInt(it.Size, 10, 64)
		page.Objects = append(page.Objects, Object{Name: it.Name, Size: n})
	}
	return page, nil
}

// transportError drops the URL from a *url.Error: the URL is ours, but the message
// is shorter and reads better to the model without it.
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("fetch failed: %v", ue.Err)
	}
	return err
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Human formats a byte count for display.
func Human(n int64) string { return human(n) }
