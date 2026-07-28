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

// Ingest handles POST /agent/worklist. It persists the runtime's items for the
// acting user.
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
	if err := h.store.Upsert(r.Context(), p.ActingUserID, body.Items...); err != nil {
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
