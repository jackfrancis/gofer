package worklist

import (
	"context"
	"time"
)

// Worklist read statuses shared by the JSON API and the HTML UI.
const (
	StatusReady      = "ready"      // items are returned
	StatusProcessing = "processing" // empty, so a backfill was triggered
	StatusFailed     = "failed"     // the backfill run failed; see Resolution.Message
)

// Resolution is the shared read-model outcome for a user's worklist. Status is one
// of StatusReady, StatusProcessing, or StatusFailed; Items carries the ranked work
// items (StatusReady only); Message carries a human-facing explanation when the
// backfill run failed (StatusFailed only). OffRadar carries the stored items left
// out of Items because they are hidden or completed (StatusReady only), from the same
// read, so a caller that also needs them (the radar's Reset count) does not read the
// store a second time and cannot see the two halves disagree. Hidden is the subset of
// OffRadar the radar lists in its hidden section (WorkItem.InHiddenList), most
// recently hidden first, unscored. Waiting carries the items on the radar that are
// waiting on someone else (WorkItem.Waiting), ranked the same way and left out of
// Items, so the radar lists them after the rest and above the hidden section.
type Resolution struct {
	Status   string
	Items    []WorkItem
	Waiting  []WorkItem
	OffRadar []WorkItem
	Hidden   []WorkItem
	Message  string
}

// BackfillProber optionally reports whether an owner's in-flight backfill run has
// failed. Resolve consults it (when the ingestor implements it) so a backfill that
// keeps failing — e.g. because its provider class targets a substrate that is not
// installed — surfaces as StatusFailed with a message instead of an endless
// StatusProcessing.
type BackfillProber interface {
	// BackfillFailure reports whether ownerID's most recent backfill run has failed
	// and, if so, a human-facing message. A run still in flight or one that
	// succeeded reports failed=false.
	BackfillFailure(ctx context.Context, ownerID string) (failed bool, message string, err error)
}

// BackfillCompleter optionally reports that an owner's most recent backfill run
// finished SUCCESSFULLY. Resolve consults it (when the ingestor implements it) so an
// empty worklist a completed run genuinely produced — a user with no open work — reads
// as StatusReady with no items, instead of spinning on StatusProcessing forever.
// Without it, "finished and found nothing" is indistinguishable from "still working".
type BackfillCompleter interface {
	// BackfillSucceeded reports whether ownerID's most recent backfill run completed
	// successfully. A run still in flight, one that failed, or no tracked run at all
	// reports false.
	BackfillSucceeded(ctx context.Context, ownerID string) (bool, error)
}

// Resolve is the shared read model for a user's worklist, used by both the JSON
// API and the HTML UI so the two cannot drift. It loads the owner's items, and:
//
//   - if there are none, triggers an idempotent backfill and returns
//     StatusProcessing with no items — unless the ingestor reports the backfill has
//     reached a terminal state: a FAILED run returns StatusFailed with the run's
//     message so the failure is visible instead of a stuck spinner, and a SUCCEEDED
//     run returns StatusReady with no items, because the radar is genuinely empty
//     rather than still being discovered;
//   - otherwise rescores agent-derived items against now (time-dependent axes
//     decay between writes), preserving human overrides (OriginUser), splits off
//     the items waiting on someone else (Resolution.Waiting), sorts both by
//     key/desc, and returns StatusReady.
//
// Rescoring at read time keeps urgency/engagement fresh without re-fetching from
// the provider.
func Resolve(ctx context.Context, store Store, ingestor Ingestor, now time.Time, ownerID string, key SortKey, desc bool) (Resolution, error) {
	items, err := store.List(ctx, ownerID)
	if err != nil {
		return Resolution{}, err
	}
	if len(items) == 0 {
		if err := ingestor.EnsureBackfill(ctx, ownerID); err != nil {
			return Resolution{}, err
		}
		if prober, ok := ingestor.(BackfillProber); ok {
			if failed, msg, perr := prober.BackfillFailure(ctx, ownerID); perr == nil && failed {
				return Resolution{Status: StatusFailed, Message: msg}, nil
			}
		}
		// A backfill that COMPLETED and still left the worklist empty means the user
		// genuinely has no open work — not that discovery is still running. Report it
		// as ready-and-empty so the radar shows its empty state instead of spinning
		// forever (the run is not re-dispatched until it goes stale).
		if completer, ok := ingestor.(BackfillCompleter); ok {
			if done, cerr := completer.BackfillSucceeded(ctx, ownerID); cerr == nil && done {
				return Resolution{Status: StatusReady, Items: []WorkItem{}}, nil
			}
		}
		return Resolution{Status: StatusProcessing}, nil
	}
	for i := range items {
		items[i].Meta = Rescore(items[i], now) // human overrides are preserved verbatim
	}
	// Hidden items (user-set) and completed items (closed/merged, agent-set) stay
	// in the store, but are dropped from the ranked list. Hidden items still open
	// are listed apart (Hidden), for the owner to unhide. Items waiting on someone
	// else stay on the radar, listed after the rest (Waiting).
	visible := items[:0]
	var off, hidden, waiting []WorkItem
	for _, it := range items {
		_, wait := it.Waiting()
		switch {
		case !it.OnRadar():
			off = append(off, it)
			if it.InHiddenList() {
				hidden = append(hidden, it)
			}
		case wait:
			waiting = append(waiting, it)
		default:
			visible = append(visible, it)
		}
	}
	items = visible
	if err := Sort(items, key, desc); err != nil {
		return Resolution{}, err
	}
	if err := Sort(waiting, key, desc); err != nil {
		return Resolution{}, err
	}
	sortHidden(hidden)
	return Resolution{Status: StatusReady, Items: items, Waiting: waiting, OffRadar: off, Hidden: hidden}, nil
}

// OnRadar reports whether the item belongs on the owner's radar: it is neither
// hidden by the owner nor completed (closed or merged). Resolve shows only these,
// and every bulk action (Review all, the second-opinion batch) draws only from them.
func (w WorkItem) OnRadar() bool {
	return w.Meta.HiddenAt.IsZero() && w.Meta.CompletedAt.IsZero()
}

// Actionable reports whether the item is on the radar (OnRadar) and not waiting on
// someone else (Waiting): the pool the bulk review actions (Review all, the
// second-opinion batch) draw from. A waiting item has nothing new for the user to act
// on until someone answers.
func (w WorkItem) Actionable() bool {
	if !w.OnRadar() {
		return false
	}
	_, waiting := w.Waiting()
	return !waiting
}
