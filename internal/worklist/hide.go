package worklist

import (
	"cmp"
	"slices"
	"time"
)

// Hiding an item is the owner's decision and it is permanent: nothing a run writes
// unhides it, however the item changes on GitHub. A hidden item keeps its GitHub
// fields (title, state, labels, which a refresh keeps current) and its conversation
// thread, but carries no gofer judgment: its signals and scoring metadata are deleted
// when it is hidden, and every write-back while it stays hidden is stripped the same
// way (MergeRunItems), so no score is kept, shown or derived for it. Hidden items
// that are still open are listed apart, at the bottom of the radar
// (Resolution.Hidden), where the owner can unhide one. An unhidden item comes back
// empty, flagged AwaitingRefresh, and the next refresh fills it in.

// IsHidden reports whether the owner has hidden the item.
func (w WorkItem) IsHidden() bool { return !w.Meta.HiddenAt.IsZero() }

// InHiddenList reports whether the item belongs in the radar's hidden section:
// hidden by the owner and still open on GitHub. A hidden item that closes leaves the
// section, still hidden; if it reopens it returns there, not to the radar.
func (w WorkItem) InHiddenList() bool { return w.IsHidden() && w.Meta.CompletedAt.IsZero() }

// HideItem hides w at at and deletes its gofer judgment (StripScoring). Hiding an
// item that is already hidden changes nothing, so a repeated Hide (a stale tab, a
// double submit) keeps the original hide time. It reports whether w changed.
func HideItem(w *WorkItem, at time.Time) bool {
	if w.IsHidden() {
		return false
	}
	w.Meta.HiddenAt = at
	StripScoring(w)
	return true
}

// UnhideItem brings a hidden item back to the radar as an empty item: no signals and
// no score, flagged AwaitingRefresh until a refresh writes it back. Unhiding an item
// that is not hidden changes nothing. It reports whether w changed.
func UnhideItem(w *WorkItem) bool {
	if !w.IsHidden() {
		return false
	}
	w.Meta.HiddenAt = time.Time{}
	StripScoring(w)
	w.Meta.AwaitingRefresh = true
	return true
}

// StripScoring deletes w's gofer judgment: every signal but the item's GitHub labels
// (display data, kept with the GitHub fields), and all scoring metadata, including
// any human override, the model proposal and the research adjustment. It keeps the
// hide and completion marks, which are not judgment.
func StripScoring(w *WorkItem) {
	w.Signals = Signals{Labels: w.Signals.Labels}
	w.Meta = Metadata{HiddenAt: w.Meta.HiddenAt, CompletedAt: w.Meta.CompletedAt}
}

// sortHidden orders the hidden section: most recently hidden first, ties in the
// store's order.
func sortHidden(items []WorkItem) {
	slices.SortStableFunc(items, func(a, b WorkItem) int { return cmp.Compare(b.Meta.HiddenAt.UnixNano(), a.Meta.HiddenAt.UnixNano()) })
}
