package worklist

import (
	"cmp"
	"errors"
	"slices"
)

// SortKey selects the ordering field. Adding a new sort is a two-line change:
// add a constant here and register its comparator in comparators below.
type SortKey string

const (
	SortRank       SortKey = "rank"
	SortPriority   SortKey = "priority"
	SortImpact     SortKey = "impact"
	SortRelevance  SortKey = "relevance"
	SortEngagement SortKey = "engagement"
	SortUrgency    SortKey = "urgency"
	SortUpdated    SortKey = "updated"
)

// DefaultSort is applied when no sort is requested.
const DefaultSort = SortRank

// ErrUnknownSort is returned for an unregistered sort key.
var ErrUnknownSort = errors.New("unknown sort key")

// comparators order items ascending by the chosen field; Sort applies the
// requested direction.
var comparators = map[SortKey]func(a, b WorkItem) int{
	SortRank:       func(a, b WorkItem) int { return cmp.Compare(a.Meta.Rank, b.Meta.Rank) },
	SortImpact:     func(a, b WorkItem) int { return cmp.Compare(a.Meta.Impact, b.Meta.Impact) },
	SortRelevance:  func(a, b WorkItem) int { return cmp.Compare(a.Meta.Relevance, b.Meta.Relevance) },
	SortEngagement: func(a, b WorkItem) int { return cmp.Compare(a.Meta.Engagement, b.Meta.Engagement) },
	SortUrgency:    func(a, b WorkItem) int { return cmp.Compare(a.Meta.Urgency, b.Meta.Urgency) },
	SortPriority: func(a, b WorkItem) int {
		return cmp.Compare(priorityWeight(a.Meta.Priority), priorityWeight(b.Meta.Priority))
	},
	SortUpdated: func(a, b WorkItem) int { return a.GitHub.UpdatedAt.Compare(b.GitHub.UpdatedAt) },
}

// Valid reports whether key is a registered sort key.
func (k SortKey) Valid() bool {
	_, ok := comparators[k]
	return ok
}

// SortKeys returns the registered sort keys, sorted, for validation messages
// and API discovery.
func SortKeys() []SortKey {
	keys := make([]SortKey, 0, len(comparators))
	for k := range comparators {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func priorityWeight(p Priority) int {
	switch p {
	case PriorityHigh:
		return 3
	case PriorityMedium:
		return 2
	case PriorityLow:
		return 1
	default:
		return 0
	}
}

// Sort orders items in place by key. desc=true puts the highest value first
// (the natural "most important first" ordering). Ties break by most-recently
// updated, then by ID, for a stable, deterministic result.
//
// Items with an unread agent reply float to the top of every sort, regardless of
// key or direction: a pending response the user has not seen outranks the chosen
// ordering. The requested ordering still applies within the unread group and
// within the rest.
func Sort(items []WorkItem, key SortKey, desc bool) error {
	cmpFn, ok := comparators[key]
	if !ok {
		return ErrUnknownSort
	}
	slices.SortStableFunc(items, func(a, b WorkItem) int {
		if ua, ub := a.HasUnreadReply(), b.HasUnreadReply(); ua != ub {
			if ua {
				return -1
			}
			return 1
		}
		c := cmpFn(a, b)
		if desc {
			c = -c
		}
		if c != 0 {
			return c
		}
		if t := b.GitHub.UpdatedAt.Compare(a.GitHub.UpdatedAt); t != 0 {
			return t
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return nil
}
