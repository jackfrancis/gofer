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
//     GET/POST /agent/worklist and its reply/research routes), authenticating every call with the run credential.
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

var (
	_ Vendor           = (*VaultVendor)(nil)
	_ CredentialVendor = (*VaultVendor)(nil)
)

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
	vc, err := v.VendCredential(ctx, provider, "")
	return vc.AccessToken, err
}

// VendCredential returns the stored credential's token and version. In-process
// there is no broker to refresh or confirm a rejection, so rejected is ignored and
// the stored token comes back (a 401 then stands). A missing credential is a
// ReconnectError, as the broker's "sign-in required" is out of process.
func (v *VaultVendor) VendCredential(ctx context.Context, provider, _ string) (VendedCredential, error) {
	cred, err := v.vault.Get(ctx, v.ownerID, provider)
	if errors.Is(err, vault.ErrNotFound) || errors.Is(err, vault.ErrSignInRequired) {
		return VendedCredential{}, &ReconnectError{Provider: provider}
	}
	if err != nil {
		return VendedCredential{}, fmt.Errorf("vend %s credential: %w", provider, err)
	}
	if cred.AccessToken == "" {
		return VendedCredential{}, fmt.Errorf("agent: empty %s credential for %s", provider, v.ownerID)
	}
	return VendedCredential{AccessToken: cred.AccessToken, Version: cred.Version}, nil
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

// Ingest merges a run's items into the owner's stored items (worklist.MergeRunItems),
// atomically. An empty batch is a no-op.
func (s *StoreSink) Ingest(ctx context.Context, items []worklist.WorkItem) error {
	if len(items) == 0 {
		return nil
	}
	return s.store.Update(ctx, s.ownerID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		return worklist.MergeRunItems(current, items), nil
	})
}

// Reply appends an agent turn to the owner's item, atomically (worklist.AppendReply).
func (s *StoreSink) Reply(ctx context.Context, itemID, replyTo string, reply worklist.Message) error {
	return worklist.ApplyReply(ctx, s.store, s.ownerID, itemID, replyTo, reply)
}

// Research sets the owner's item's research adjustment alone, atomically
// (worklist.ApplyResearch).
func (s *StoreSink) Research(ctx context.Context, itemID string, adj worklist.ResearchAdjustment) error {
	return worklist.ApplyResearch(ctx, s.store, s.ownerID, itemID, adj)
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
	_ Vendor           = (*PlaneClient)(nil)
	_ CredentialVendor = (*PlaneClient)(nil)
	_ Sink             = (*PlaneClient)(nil)
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
	vc, err := c.VendCredential(ctx, provider, "")
	return vc.AccessToken, err
}

// VendCredential fetches the token and its credential version
// (GET /agent/credential). A non-empty rejected names the version GitHub answered
// with 401, so the broker confirms it and refreshes if it must. The broker's
// "sign-in required" is a ReconnectError.
func (c *PlaneClient) VendCredential(ctx context.Context, provider, rejected string) (VendedCredential, error) {
	q := url.Values{"provider": {provider}}
	if rejected != "" {
		q.Set("rejected", rejected)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agent/credential?"+q.Encode(), nil)
	if err != nil {
		return VendedCredential{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return VendedCredential{}, err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Version     string `json:"credential_version"`
		Code        string `json:"code"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	if resp.StatusCode == signInRequiredStatus && out.Code == signInRequiredCode {
		return VendedCredential{}, &ReconnectError{Provider: provider}
	}
	if resp.StatusCode != http.StatusOK {
		return VendedCredential{}, fmt.Errorf("vend %s credential: status %d", provider, resp.StatusCode)
	}
	if decodeErr != nil {
		return VendedCredential{}, decodeErr
	}
	if out.AccessToken == "" {
		return VendedCredential{}, fmt.Errorf("vend %s credential: empty token", provider)
	}
	return VendedCredential{AccessToken: out.AccessToken, Version: out.Version}, nil
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

// Ingest writes items back through the agent plane, which merges them into the
// stored items. An empty batch is a no-op.
func (c *PlaneClient) Ingest(ctx context.Context, items []worklist.WorkItem) error {
	if len(items) == 0 {
		return nil
	}
	return c.post(ctx, "/agent/worklist", "ingest", map[string]any{"items": items})
}

// Reply appends an agent turn to an item's thread through the agent plane. The
// request is idempotent (a repeat is recognized by the reply's ID), so resending it
// after a lost response cannot duplicate the turn.
func (c *PlaneClient) Reply(ctx context.Context, itemID, replyTo string, reply worklist.Message) error {
	return c.post(ctx, "/agent/worklist/reply", "reply", ReplyRequest{ItemID: itemID, InReplyTo: replyTo, Message: reply})
}

// ReplyRequest is the body of POST /agent/worklist/reply.
type ReplyRequest struct {
	ItemID    string           `json:"item_id"`
	InReplyTo string           `json:"in_reply_to"` // the answered user turn's Key
	Message   worklist.Message `json:"message"`
}

// Research writes an item's research adjustment, and nothing else, through the
// agent plane (POST /agent/worklist/research). Setting the same value twice is
// harmless, so resending after a lost response is safe.
func (c *PlaneClient) Research(ctx context.Context, itemID string, adj worklist.ResearchAdjustment) error {
	return c.post(ctx, "/agent/worklist/research", "research", ResearchRequest{ItemID: itemID, Adjustment: adj})
}

// ResearchRequest is the body of POST /agent/worklist/research.
type ResearchRequest struct {
	ItemID     string                      `json:"item_id"`
	Adjustment worklist.ResearchAdjustment `json:"adjustment"`
}

// post sends one JSON body to the agent plane and expects 200.
func (c *PlaneClient) post(ctx context.Context, path, what string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
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
		return fmt.Errorf("%s: status %d", what, resp.StatusCode)
	}
	return nil
}

// do authenticates and sends one agent-plane request.
func (c *PlaneClient) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.client.Do(req)
}
