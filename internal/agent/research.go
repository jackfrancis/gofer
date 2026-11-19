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
	// Read just the dispatched item, thread included, not the worklist.
	item, ok, err := sink.Item(ctx, p.ItemID)
	if err != nil {
		return fmt.Errorf("read item: %w", err)
	}
	if !ok {
		return nil // item gone
	}

	adj, err := p.Researcher.Research(ctx, item)
	if err != nil {
		return fmt.Errorf("research: %w", err)
	}
	item.Signals.Research = &adj
	// The write-back never changes the thread (worklist.MergeRunItems keeps the
	// stored one), so it does not carry it either.
	item.Thread = nil
	if err := sink.Ingest(ctx, []worklist.WorkItem{item}); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
