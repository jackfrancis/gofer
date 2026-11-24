package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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
	store  worklist.Store
	noter  WritebackNoter
	policy worklist.RunPolicy
	synth  Synthesizer
	now    func() time.Time
}

// Synthesizer dispatches the review panel's consensus synthesis turn, which Reply
// chains onto the thread when a panel review's answer lands (ingest.Ingestor). Reply
// calls it off its request, under a context detached from it.
type Synthesizer interface {
	Synthesize(ctx context.Context, ownerID string, ref worklist.TurnRef)
}

// UseSynthesizer turns on the review panel's consensus synthesis: once a panel
// review is answered, Reply chains the synthesis turn and hands it to s. Without one
// nothing is chained, since no run would answer the turn.
func (h *IngestHandler) UseSynthesizer(s Synthesizer) { h.synth = s }

// WritebackNoter is notified when a runtime writes its result back, with the run's
// id (the token's JobID). The batch tracker uses it to time a Review-all batch by
// the exact instant each review lands. Optional; a nil noter disables the hook.
type WritebackNoter interface {
	NoteWriteback(runID string)
}

// NewIngestHandler constructs an IngestHandler. noter (optional; may be nil) is
// notified of each write-back so a batch action can be timed to the last review.
// policy is applied to every write-back before it is merged (worklist.RunPolicy).
func NewIngestHandler(store worklist.Store, noter WritebackNoter, policy worklist.RunPolicy) *IngestHandler {
	return &IngestHandler{store: store, noter: noter, policy: policy, now: time.Now}
}

type ingestRequest struct {
	Items []worklist.WorkItem `json:"items"`
}

// Ingest handles POST /agent/worklist. It merges the runtime's items into the
// acting user's stored items in one atomic update (worklist.MergeRunItems): the run's
// derived fields land, and the owner's thread and other state are kept as stored now,
// not as the run read them. Before the merge, the web tier's policy stamps the items
// (worklist.RunPolicy.Apply): their metadata is agent-derived, and a review request
// is classified against the bot list.
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
	h.policy.Apply(body.Items)
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
// there, and at most once. With a Synthesizer, the answer to a review panel's blind
// review also chains the consensus synthesis turn in the same update
// (worklist.ApplyReplyAndChain), which is then dispatched.
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
	var synth string
	var err error
	if h.synth != nil {
		synth, err = worklist.ApplyReplyAndChain(r.Context(), h.store, p.ActingUserID, body.ItemID, body.InReplyTo, body.Message, h.now().UTC())
	} else {
		err = worklist.ApplyReply(r.Context(), h.store, p.ActingUserID, body.ItemID, body.InReplyTo, body.Message)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not persist reply")
		return
	}
	if synth != "" {
		// The answer was a panel review's, and its synthesis turn is stored: dispatch
		// it without holding up the run that is reporting its reply.
		go h.synth.Synthesize(context.WithoutCancel(r.Context()), p.ActingUserID, worklist.TurnRef{Item: body.ItemID, Turn: synth})
	}
	// The reply is the run's result landing (see Ingest).
	if h.noter != nil && p.JobID != "" {
		h.noter.NoteWriteback(p.JobID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// researchRequest is the body of POST /agent/worklist/research.
type researchRequest struct {
	ItemID     string              `json:"item_id"`
	Adjustment *researchAdjustment `json:"adjustment"`
}

// researchAdjustment is worklist.ResearchAdjustment as posted, with each multiplier
// a pointer so a missing one is told apart from an explicit zero: a missing
// multiplier decoding as 0 would silently zero that ranking axis.
type researchAdjustment struct {
	Relevance  *float64  `json:"relevance"`
	Impact     *float64  `json:"impact"`
	Engagement *float64  `json:"engagement"`
	Urgency    *float64  `json:"urgency"`
	Rationale  string    `json:"rationale"`
	AppliedAt  time.Time `json:"applied_at"`
	Turns      []string  `json:"turns"`
	// GitHubUpdatedAt is the GitHub version the research read (see
	// worklist.ResearchAdjustment); zero from a runtime that does not send it.
	GitHubUpdatedAt time.Time `json:"github_updated_at"`
}

// complete reports whether every multiplier and the stamp are present.
func (a *researchAdjustment) complete() bool {
	return a != nil && a.Relevance != nil && a.Impact != nil && a.Engagement != nil && a.Urgency != nil && !a.AppliedAt.IsZero()
}

func (a *researchAdjustment) value() worklist.ResearchAdjustment {
	return worklist.ResearchAdjustment{
		Relevance: *a.Relevance, Impact: *a.Impact, Engagement: *a.Engagement, Urgency: *a.Urgency,
		Rationale: a.Rationale, AppliedAt: a.AppliedAt, Turns: a.Turns,
		GitHubUpdatedAt: a.GitHubUpdatedAt,
	}
}

// maxResearchBody bounds a research request. Besides four multipliers and a
// rationale it lists the key of every thread turn the research read, so it is sized
// like a write-back (Ingest), which carries whole threads: a long conversation's
// research must not be refused after its reply has landed.
const maxResearchBody = 8 << 20

// Research handles POST /agent/worklist/research. A converse or research run writes
// the research adjustment it derived here (worklist.ApplyResearch), and only that:
// the rest of the item stays as stored now, so a run that read the item before a
// refresh cannot write back its older GitHub fields. An adjustment for an item that
// is gone, or older than the stored one (a resend), is accepted and dropped.
func (h *IngestHandler) Research(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body researchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResearchBody)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.ItemID == "" || !body.Adjustment.complete() {
		writeError(w, http.StatusBadRequest, "item_id and an adjustment with relevance, impact, engagement, urgency and applied_at are required")
		return
	}
	if err := worklist.ApplyResearch(r.Context(), h.store, p.ActingUserID, body.ItemID, body.Adjustment.value()); err != nil {
		writeError(w, http.StatusBadGateway, "could not persist research adjustment")
		return
	}
	// The adjustment is the run's result landing (see Ingest).
	if h.noter != nil && p.JobID != "" {
		h.noter.NoteWriteback(p.JobID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// turnsRequest is the body of POST /agent/worklist/turns.
type turnsRequest struct {
	Readings []worklist.ItemTurn `json:"readings"`
}

// maxTurnsBody bounds a turn-readings request: up to worklist.MaxTurnBatch readings,
// each an item ID, a last-word key, an outcome and a login.
const maxTurnsBody = 2 << 20

// Turns handles POST /agent/worklist/turns. A reading pass writes the turn readings
// it derived here, in one update (worklist.ApplyTurns), and only those: the rest of
// each item stays as stored now, so a reading the model took a while over never
// restores the item it read. A reading for an item that is gone or hidden, or of a
// last word that is no longer current, is accepted and dropped.
func (h *IngestHandler) Turns(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body turnsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTurnsBody)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.Readings) > worklist.MaxTurnBatch {
		writeError(w, http.StatusBadRequest, "too many readings in one request")
		return
	}
	for _, t := range body.Readings {
		if t.ItemID == "" || t.Reading.LastWord == "" {
			writeError(w, http.StatusBadRequest, "every reading needs an item_id and the last_word it read")
			return
		}
	}
	if err := worklist.ApplyTurns(r.Context(), h.store, p.ActingUserID, body.Readings); err != nil {
		writeError(w, http.StatusBadGateway, "could not persist turn readings")
		return
	}
	if h.noter != nil && p.JobID != "" {
		h.noter.NoteWriteback(p.JobID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// List handles GET /agent/worklist: the acting user's whole worklist, threads
// included. No current run reads it (it outgrows the agent plane's response limit);
// a run reads one item (Item) or a bounded selection (Select). It stays for a
// runtime from before those reads.
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
	// Classify by the current bot list, so a run ranks by the same classification
	// its write-back will store.
	h.policy.Classify(items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Select handles GET /agent/worklist/select: a bounded selection of the acting
// user's items, without threads (worklist.ParseSelection, worklist.SelectFrom), as
// {"items": [...], "next": "<cursor>"}, classified by the current bot list like List.
// It is its own route so that a runtime talking
// to a web tier without it fails (404) instead of receiving the whole worklist.
func (h *IngestHandler) Select(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sel, err := worklist.ParseSelection(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, next, err := worklist.SelectFrom(r.Context(), h.policy.ClassifyReads(h.store), p.ActingUserID, sel, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not load work items")
		return
	}
	resp := map[string]any{"items": page}
	if next != "" {
		resp["next"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

// Item handles GET /agent/worklist/item?id=&turn=. A Discuss or research run reads
// the one item it was dispatched for, with a bounded window of its thread that keeps
// the turn it answers (worklist.ReadItem). The lookup is scoped to the token's
// ActingUserID, so another user's item is not found: the answer is {"found": false},
// the same as for an item that is gone.
func (h *IngestHandler) Item(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	q := r.URL.Query()
	id := q.Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	it, found, err := worklist.ReadItem(r.Context(), h.policy.ClassifyReads(h.store), p.ActingUserID, id, q.Get("turn"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not load work item")
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": true, "item": it})
}
