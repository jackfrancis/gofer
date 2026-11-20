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
//     POST /agent/worklist, GET /agent/worklist/select, GET /agent/worklist/item),
//     authenticating every call with the run credential.
//
// A backend picks the shape its substrate needs and writes no plumbing of its own.

// maxSinkResponse bounds one read from the agent plane. It matches the server's own
// request limit. No read a run makes approaches it however large the worklist grows:
// a run reads one item with a window of its thread, at most worklist.MaxItemBytes,
// or a page of items without threads, at most worklist.MaxSelectBytes.
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

// Item returns the owner's item with id and a window of its thread that keeps turn
// (worklist.ReadItem); ok is false if it is gone.
func (s *StoreSink) Item(ctx context.Context, id, turn string) (worklist.WorkItem, bool, error) {
	return worklist.ReadItem(ctx, s.store, s.ownerID, id, turn)
}

// Select returns the owner's items that sel names (worklist.SelectFrom), under the
// same bounds the agent plane applies.
func (s *StoreSink) Select(ctx context.Context, sel worklist.Selection) ([]worklist.WorkItem, string, error) {
	return worklist.SelectFrom(ctx, s.store, s.ownerID, sel, time.Now().UTC())
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

// Item reads one of the acting user's items from the agent plane (GET
// /agent/worklist/item), with a window of its thread that keeps turn. The owner is
// the run credential's, so another user's item is simply not found. ok is false if
// the item is gone. The server answers a missing item with found=false, so a 404 (a
// web tier without the route) is an error rather than a silent "gone".
func (c *PlaneClient) Item(ctx context.Context, id, turn string) (worklist.WorkItem, bool, error) {
	q := url.Values{"id": {id}}
	if turn != "" {
		q.Set("turn", turn)
	}
	var out ItemResponse
	if err := c.getJSON(ctx, "/agent/worklist/item?"+q.Encode(), "read item", &out); err != nil {
		return worklist.WorkItem{}, false, err
	}
	if !out.Found {
		return worklist.WorkItem{}, false, nil
	}
	return out.Item, true, nil
}

// ItemResponse is the body of GET /agent/worklist/item.
type ItemResponse struct {
	Found bool              `json:"found"`
	Item  worklist.WorkItem `json:"item"`
}

// SelectResponse is the body of GET /agent/worklist/select.
type SelectResponse struct {
	Items []worklist.WorkItem `json:"items"`
	Next  string              `json:"next,omitempty"`
}

// Select reads a bounded selection of the acting user's items from the agent plane
// (GET /agent/worklist/select). The server may return fewer items than sel.Limit
// asks for; a non-empty next means there is more to read. A web tier without the
// route answers 404, which is an error: the client never falls back to the
// whole-worklist read, which would ignore the selection's bounds.
func (c *PlaneClient) Select(ctx context.Context, sel worklist.Selection) ([]worklist.WorkItem, string, error) {
	var out SelectResponse
	if err := c.getJSON(ctx, "/agent/worklist/select?"+sel.Query().Encode(), "select worklist", &out); err != nil {
		return nil, "", err
	}
	return out.Items, out.Next, nil
}

// getJSON reads one agent-plane resource into v, at most maxSinkResponse bytes.
func (c *PlaneClient) getJSON(ctx context.Context, path, what string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", what, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSinkResponse)).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
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
