package worklist

import "time"

// ResearchStale reports whether item's research adjustment (Signals.Research) should
// be derived again, so the scheduled research pass (ingest.Refresher) dispatches a
// github-research run for it. Only an item with a visible discussion to research is
// eligible: at least one turn the owner has not hidden. Hidden turns are withheld from
// the model, so a thread of only hidden turns has nothing to research.
//
// An eligible item is stale when it has never been researched, when its research
// predates the newest input it would read (a later visible turn, or a GitHub update
// that likely moved the foundation it nuances), or when a turn the research read has
// since been hidden. It keys off the research's own AppliedAt, not the score's write
// time, which a refresh or a rank pass also moves.
//
// Items off the radar (hidden by the owner or closed on GitHub) and items carrying a
// human ranking override (OriginUser, which rescoring leaves alone, so research would
// change nothing) are not eligible.
func ResearchStale(item WorkItem) bool {
	if !item.OnRadar() || item.Meta.Origin == OriginUser {
		return false
	}
	latest, ok := latestVisibleTurn(item)
	if !ok {
		return false // no visible discussion to research
	}
	r := item.Signals.Research
	if r == nil {
		return true
	}
	freshest := item.GitHub.UpdatedAt
	if latest.After(freshest) {
		freshest = latest
	}
	if r.AppliedAt.Before(freshest) {
		return true
	}
	// Research that read a turn since hidden still carries what the owner withdrew.
	// Research stored before it recorded its turns cannot be checked, and is judged by
	// time alone.
	return len(r.Turns) > 0 && !ResearchReflectsVisibleThread(item)
}

// latestVisibleTurn returns the time of the newest turn not hidden, and whether there
// is one.
func latestVisibleTurn(item WorkItem) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, m := range item.Thread {
		if m.Hidden {
			continue
		}
		found = true
		if m.At.After(latest) {
			latest = m.At
		}
	}
	return latest, found
}
