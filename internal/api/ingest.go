package api

import (
	"encoding/json"
	"net/http"

	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// IngestHandler is the agent-plane sink: a runtime posts the work items it
// produced (POST /agent/worklist) and reads back the acting user's persisted work
// (GET /agent/worklist). Both are scoped to the token's ActingUserID — the store
// re-scopes every written item to the owner — so a runtime cannot touch another
// user's data. The routes are behind RequireScope, so only a workload token (never
// a browser session) reaches them.
type IngestHandler struct {
	store worklist.Store
	noter WritebackNoter
}

// WritebackNoter is notified when a runtime writes its result back, with the run's
// id (the token's JobID). The batch tracker uses it to time a Review-all batch by
// the exact instant each review lands. Optional; a nil noter disables the hook.
type WritebackNoter interface {
	NoteWriteback(runID string)
}

// NewIngestHandler constructs an IngestHandler. noter (optional; may be nil) is
// notified of each write-back so a batch action can be timed to the last review.
func NewIngestHandler(store worklist.Store, noter WritebackNoter) *IngestHandler {
	return &IngestHandler{store: store, noter: noter}
}

type ingestRequest struct {
	Items []worklist.WorkItem `json:"items"`
}

// Ingest handles POST /agent/worklist. It merges the runtime's items into the
// acting user's stored items in one atomic update (worklist.MergeRunItems): the run's
// derived fields land, and the owner's thread and other state are kept as stored now,
// not as the run read them.
func (h *IngestHandler) Ingest(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body ingestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	err := h.store.Update(r.Context(), p.ActingUserID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		return worklist.MergeRunItems(current, body.Items), nil
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not persist work items")
		return
	}
	// A workload write-back is a run reporting its result; let the batch tracker time
	// Review-all by the instant each review lands (JobID is the run's id, ADR 0002).
	if h.noter != nil && p.JobID != "" {
		h.noter.NoteWriteback(p.JobID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ingested": len(body.Items)})
}

// replyRequest is the body of POST /agent/worklist/reply.
type replyRequest struct {
	ItemID    string           `json:"item_id"`
	InReplyTo string           `json:"in_reply_to"` // the answered user turn's Key
	Message   worklist.Message `json:"message"`
}

// maxReplyBody bounds a reply request; a model's answer is far smaller.
const maxReplyBody = 1 << 20

// Reply handles POST /agent/worklist/reply. A converse run appends its answer to an
// item's thread here (worklist.ApplyReply): it is added as an agent turn to the thread
// as stored now, linked to the user turn it answers, only if that turn is still
// there, and at most once.
// A reply to an item or turn that is gone is accepted and dropped.
func (h *IngestHandler) Reply(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body replyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReplyBody)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.ItemID == "" || body.InReplyTo == "" || body.Message.Content == "" || body.Message.At.IsZero() {
		writeError(w, http.StatusBadRequest, "item_id, in_reply_to, message.content and message.at are required")
		return
	}
	if err := worklist.ApplyReply(r.Context(), h.store, p.ActingUserID, body.ItemID, body.InReplyTo, body.Message); err != nil {
		writeError(w, http.StatusBadGateway, "could not persist reply")
		return
	}
	// The reply is the run's result landing (see Ingest).
	if h.noter != nil && p.JobID != "" {
		h.noter.NoteWriteback(p.JobID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// List handles GET /agent/worklist. A runtime reads its acting user's persisted
// work to augment it in place rather than re-deriving it from the provider.
func (h *IngestHandler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	items, err := h.store.List(r.Context(), p.ActingUserID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not load work items")
		return
	}
	if items == nil {
		items = []worklist.WorkItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
