package worklist

import "time"

// ResearchStale reports whether item's research adjustment (Signals.Research) should
// be derived again, so the scheduled research pass (ingest.Refresher) dispatches a
// github-research run for it. Only an item with a visible discussion to research is
// eligible: at least one turn the owner has not hidden. Hidden turns are withheld from
// the model, so a thread of only hidden turns has nothing to research.
//
// An eligible item is stale when it has never been researched, or when the research
// no longer matches what it would read now:
//
//   - GitHub: the stored item is newer than the GitHub version the research read
//     (ResearchAdjustment.GitHubUpdatedAt). The comparison is with what was read,
//     not with when the research was applied, so a refresh that lands while the
//     model works still counts.
//   - The thread: a turn the research read is gone or hidden now, or a visible turn
//     newer than the oldest one it read was not read: posted after the read, or
//     hidden at the read and unhidden since. A research read is the newest visible
//     turns that fit (ThreadWindow), so visible turns older than the oldest one read
//     were left out for size, and do not make it stale.
//
// Research stored before it recorded these is judged by time: applied before the
// GitHub update, or before the newest visible turn.
//
// Items off the radar (hidden by the owner or closed on GitHub) and items carrying a
// human ranking override (OriginUser, which rescoring leaves alone, so research would
// change nothing) are not eligible.
func ResearchStale(item WorkItem) bool {
	if !item.OnRadar() || item.Meta.Origin == OriginUser {
		return false
	}
	visible := visibleTurns(item)
	if len(visible) == 0 {
		return false // no visible discussion to research
	}
	r := item.Signals.Research
	if r == nil {
		return true
	}
	if r.GitHubUpdatedAt.IsZero() {
		if r.AppliedAt.Before(item.GitHub.UpdatedAt) {
			return true
		}
	} else if item.GitHub.UpdatedAt.After(r.GitHubUpdatedAt) {
		return true
	}
	if len(r.Turns) == 0 {
		if !r.GitHubUpdatedAt.IsZero() {
			return true // it read no turn, and there is one to read now
		}
		return r.AppliedAt.Before(latestAt(item, visible))
	}
	return !readTheVisibleThread(item, visible, r.Turns)
}

// readTheVisibleThread reports whether research that read the turns keyed by read
// still reflects item's visible thread: every turn it read is present and visible,
// and every visible turn after the oldest one it read was read.
func readTheVisibleThread(item WorkItem, visible []int, read []string) bool {
	wasRead := make(map[string]bool, len(read))
	for _, k := range read {
		wasRead[k] = true
	}
	oldest := -1
	isVisible := make(map[string]bool, len(visible))
	for _, i := range visible {
		k := item.TurnKey(i)
		isVisible[k] = true
		if oldest < 0 && wasRead[k] {
			oldest = i
		}
	}
	for _, k := range read {
		if !isVisible[k] {
			return false // gone, or hidden since
		}
	}
	for _, i := range visible {
		if i > oldest && !wasRead[item.TurnKey(i)] {
			return false // posted since, or unhidden since
		}
	}
	return true
}

// visibleTurns returns the positions of the turns not hidden, oldest first.
func visibleTurns(item WorkItem) []int {
	var idx []int
	for i, m := range item.Thread {
		if !m.Hidden {
			idx = append(idx, i)
		}
	}
	return idx
}

// latestAt returns the newest time among the turns at positions idx.
func latestAt(item WorkItem, idx []int) time.Time {
	var latest time.Time
	for _, i := range idx {
		if at := item.Thread[i].At; at.After(latest) {
			latest = at
		}
	}
	return latest
}
