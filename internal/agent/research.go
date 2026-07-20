package agent

import (
	"context"
	"fmt"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// runResearch re-weights a single item's ranking axes (p.ItemID) from its
// conversation thread: it reads the item, asks the ResearchRanker for per-axis
// multipliers, and writes them back as Signals.Research, which Score applies at
// read time. The GitHub-metadata foundation stays authoritative; research only
// nuances it. It reads and writes gofer only.
func runResearch(ctx context.Context, p Params, sink Sink) error {
	if p.Researcher == nil {
		return fmt.Errorf("agent: github-research requires a researcher")
	}
	if p.ItemID == "" {
		return fmt.Errorf("agent: github-research requires an item id")
	}
	items, err := sink.List(ctx)
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	idx := -1
	for i := range items {
		if items[i].ID == p.ItemID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil // item gone
	}
	item := items[idx]

	adj, err := p.Researcher.Research(ctx, item)
	if err != nil {
		return fmt.Errorf("research: %w", err)
	}
	item.Signals.Research = &adj
	if err := sink.Ingest(ctx, []worklist.WorkItem{item}); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
