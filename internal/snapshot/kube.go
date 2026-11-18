package snapshot

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The in-cluster ServiceAccount files every pod gets.
const (
	saDir           = "/var/run/secrets/kubernetes.io/serviceaccount"
	saTokenFile     = saDir + "/token"
	saCAFile        = saDir + "/ca.crt"
	saNamespaceFile = saDir + "/namespace"
)

// Kube is a minimal Kubernetes API client: JSON over HTTPS, authenticated with a
// bearer token. gofer imports no client-go; the snapshotter needs four calls.
type Kube struct {
	base  string
	token func() (string, error)
	http  *http.Client
}

// InCluster returns a client for the API server the pod runs under, authenticating
// with the pod's ServiceAccount token (re-read per request, since the kubelet rotates
// it), and the namespace the pod runs in.
func InCluster() (*Kube, string, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, "", errors.New("snapshot: not running in a cluster (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	ca, err := os.ReadFile(saCAFile)
	if err != nil {
		return nil, "", fmt.Errorf("snapshot: read cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, "", errors.New("snapshot: cluster CA has no certificates")
	}
	ns, err := os.ReadFile(saNamespaceFile)
	if err != nil {
		return nil, "", fmt.Errorf("snapshot: read namespace: %w", err)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	token := func() (string, error) {
		b, err := os.ReadFile(saTokenFile)
		return strings.TrimSpace(string(b)), err
	}
	return NewKube("https://"+net.JoinHostPort(host, port), token, client), strings.TrimSpace(string(ns)), nil
}

// NewKube returns a client for the API server at base. Tests point it at a fake.
func NewKube(base string, token func() (string, error), client *http.Client) *Kube {
	return &Kube{base: strings.TrimRight(base, "/"), token: token, http: client}
}

// StatusError is a non-2xx answer from the API server.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("kubernetes API: status %d: %s", e.Code, e.Message)
}

// IsNotFound reports whether err is a 404 from the API server.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// IsConflict reports whether err is a 409 from the API server, as for a delete whose
// preconditions no longer hold.
func IsConflict(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusConflict
}

// statusCode is the HTTP status of an API server error, or 0 if err is not one.
func statusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// refused reports whether the API server answered a request with a 4xx: the request
// was not applied. Anything else that fails (a transport error, a 5xx) may or may not
// have been applied.
func refused(err error) bool {
	c := statusCode(err)
	return c >= 400 && c < 500
}

// do sends one request with an optional JSON body and decodes a JSON answer into out
// (if non-nil).
func (k *Kube) do(ctx context.Context, method, path, contentType string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return err
	}
	tok, err := k.token()
	if err != nil {
		return fmt.Errorf("snapshot: read service account token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &st)
		if st.Message == "" {
			st.Message = strings.TrimSpace(string(data))
		}
		return &StatusError{Code: resp.StatusCode, Message: st.Message}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
