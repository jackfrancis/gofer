package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// This file binds the workload's two seams — Vendor and Sink — to gofer, so an
// agent-runtime backend never re-implements them. There are two shapes of the same
// contract, and agent.Run behaves identically against either:
//
//   - IN-PROCESS (VaultVendor + StoreSink): a backend that executes the workload
//     inside the web tier reads gofer's vault and worklist store directly.
//   - OUT-OF-PROCESS (PlaneClient): a backend that executes the workload in another
//     process or sandbox reaches gofer over its agent plane (GET /agent/credential,
//     GET/POST /agent/worklist), authenticating every call with the run credential.
//
// A backend picks the shape its substrate needs and writes no plumbing of its own.

// maxSinkResponse bounds a worklist read from the agent plane. It matches the
// server's own request limit: every item carries its whole conversation thread, so a
// full worklist is large but bounded.
const maxSinkResponse = 8 << 20

// defaultPlaneTimeout bounds a single call to gofer's agent plane. These are gofer's
// own in-cluster endpoints (no model, no provider), so they answer quickly.
const defaultPlaneTimeout = 30 * time.Second

// VaultVendor is the in-process Vendor: it reads the acting user's delegated
// credential straight from gofer's vault, and brokers the shared chat-model token as
// provider "ai" — the same policy gofer's GET /agent/credential serves to an
// out-of-process runtime.
type VaultVendor struct {
	vault   vault.Vault
	ownerID string
	aiToken string
}

var _ Vendor = (*VaultVendor)(nil)

// NewVaultVendor binds a vendor to one run's acting user. aiToken is the shared
// chat-model token vended as provider "ai"; empty disables the model.
func NewVaultVendor(v vault.Vault, ownerID, aiToken string) *VaultVendor {
	return &VaultVendor{vault: v, ownerID: ownerID, aiToken: aiToken}
}

// Vend returns the acting user's token for provider, or the shared chat-model token
// for the pseudo-provider "ai".
func (v *VaultVendor) Vend(ctx context.Context, provider string) (string, error) {
	if provider == "ai" {
		if v.aiToken == "" {
			return "", errors.New("agent: no chat-model token configured")
		}
		return v.aiToken, nil
	}
	cred, err := v.vault.Get(ctx, v.ownerID, provider)
	if err != nil {
		return "", fmt.Errorf("vend %s credential: %w", provider, err)
	}
	if cred.AccessToken == "" {
		return "", fmt.Errorf("agent: empty %s credential for %s", provider, v.ownerID)
	}
	return cred.AccessToken, nil
}

// StoreSink is the in-process Sink: it reads and writes gofer's worklist store,
// scoping every call to the run's owner — the same scoping the agent plane enforces
// from the run credential.
type StoreSink struct {
	store   worklist.Store
	ownerID string
}

var _ Sink = (*StoreSink)(nil)

// NewStoreSink binds a sink to one run's owner.
func NewStoreSink(s worklist.Store, ownerID string) *StoreSink {
	return &StoreSink{store: s, ownerID: ownerID}
}

// List returns the owner's persisted work.
func (s *StoreSink) List(ctx context.Context) ([]worklist.WorkItem, error) {
	return s.store.List(ctx, s.ownerID)
}

// Ingest writes items back for the owner. An empty batch is a no-op.
func (s *StoreSink) Ingest(ctx context.Context, items []worklist.WorkItem) error {
	if len(items) == 0 {
		return nil
	}
	return s.store.Upsert(ctx, s.ownerID, items...)
}

// PlaneClient is the out-of-process Vendor and Sink: it reaches gofer's agent plane
// over HTTP, authenticating every call with the run credential (the audience-bound
// token the runtime carries). The owner is implied by that credential, so — unlike
// the in-process adapters — no owner id is passed: gofer scopes the call to the
// principal it verifies.
type PlaneClient struct {
	base   string
	token  string
	client *http.Client
}

var (
	_ Vendor = (*PlaneClient)(nil)
	_ Sink   = (*PlaneClient)(nil)
)

// NewPlaneClient builds a client for gofer's agent plane at baseURL, authenticating
// with runCredential. A nil client gets one bounded by defaultPlaneTimeout.
func NewPlaneClient(baseURL, runCredential string, client *http.Client) *PlaneClient {
	if client == nil {
		client = &http.Client{Timeout: defaultPlaneTimeout}
	}
	return &PlaneClient{base: strings.TrimRight(baseURL, "/"), token: runCredential, client: client}
}

// Vend fetches the acting user's delegated provider token from gofer. The runtime
// holds it for the run only — never a standing secret.
func (c *PlaneClient) Vend(ctx context.Context, provider string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agent/credential?provider="+url.QueryEscape(provider), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vend %s credential: status %d", provider, resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("vend %s credential: empty token", provider)
	}
	return out.AccessToken, nil
}

// List reads the acting user's persisted work from the agent plane.
func (c *PlaneClient) List(ctx context.Context) ([]worklist.WorkItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agent/worklist", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list worklist: status %d", resp.StatusCode)
	}
	var out struct {
		Items []worklist.WorkItem `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSinkResponse)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Ingest writes items back through the agent plane. An empty batch is a no-op.
func (c *PlaneClient) Ingest(ctx context.Context, items []worklist.WorkItem) error {
	if len(items) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/agent/worklist", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ingest: status %d", resp.StatusCode)
	}
	return nil
}

// do authenticates and sends one agent-plane request.
func (c *PlaneClient) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.client.Do(req)
}
