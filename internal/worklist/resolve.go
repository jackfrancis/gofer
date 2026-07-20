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
// backfill run failed (StatusFailed only).
type Resolution struct {
	Status  string
	Items   []WorkItem
	Message string
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

// Resolve is the shared read model for a user's worklist, used by both the JSON
// API and the HTML UI so the two cannot drift. It loads the owner's items, and:
//
//   - if there are none, triggers an idempotent backfill and returns
//     StatusProcessing with no items — unless the ingestor is a BackfillProber and
//     reports the backfill run has failed, in which case it returns StatusFailed
//     with the run's message so the failure is visible instead of a stuck spinner;
//   - otherwise rescores agent-derived items against now (time-dependent axes
//     decay between writes), preserving human overrides (OriginUser), sorts by
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
		return Resolution{Status: StatusProcessing}, nil
	}
	for i := range items {
		if items[i].Meta.Origin == OriginUser {
			continue // human overrides are preserved verbatim
		}
		hidden := items[i].Meta.HiddenAt       // survives the rescore below
		completed := items[i].Meta.CompletedAt // survives the rescore below
		scored := Score(items[i], now)
		scored.UpdatedAt = items[i].Meta.UpdatedAt // preserve persisted write time
		scored.HiddenAt = hidden
		scored.CompletedAt = completed
		items[i].Meta = scored
	}
	// Hidden items (user-set) and completed items (closed/merged, agent-set) stay
	// in the store so an agent can still see and revive them, but are dropped from
	// the user-facing list.
	visible := items[:0]
	for _, it := range items {
		if it.Meta.HiddenAt.IsZero() && it.Meta.CompletedAt.IsZero() {
			visible = append(visible, it)
		}
	}
	items = visible
	if err := Sort(items, key, desc); err != nil {
		return Resolution{}, err
	}
	return Resolution{Status: StatusReady, Items: items}, nil
}

// HiddenAfter returns the HiddenAt to keep for a re-ingested item: it preserves a
// prior hidden timestamp, but clears it (auto-unhide) when the underlying item
// has been updated since it was hidden, so a changed item resurfaces. updatedAt
// is the GitHub item's updated_at.
func HiddenAfter(prevHiddenAt, updatedAt time.Time) time.Time {
	if prevHiddenAt.IsZero() || updatedAt.After(prevHiddenAt) {
		return time.Time{}
	}
	return prevHiddenAt
}
