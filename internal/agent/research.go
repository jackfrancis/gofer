package agent

import (
	"context"
	"fmt"
	"time"

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
	// Read just the dispatched item, with a bounded window of its thread, not the
	// worklist.
	item, ok, err := sink.Item(ctx, p.ItemID, "")
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
	// Write back only the adjustment: the item was read before the model call, and
	// a refresh may have landed since.
	if err := sink.Research(ctx, item.ID, stampResearch(adj, item)); err != nil {
		return fmt.Errorf("research: %w", err)
	}
	return nil
}

// stampResearch gives an adjustment its AppliedAt if the researcher left it unset,
// and records the GitHub version of the item the research read (read), so a GitHub
// update stored after that read still makes it stale (worklist.ResearchStale). The
// stamp is fixed before the write is sent, so a resend carries the same one and
// cannot be taken for a newer result (worklist.ApplyResearch).
func stampResearch(adj worklist.ResearchAdjustment, read worklist.WorkItem) worklist.ResearchAdjustment {
	if adj.AppliedAt.IsZero() {
		adj.AppliedAt = time.Now().UTC()
	}
	adj.GitHubUpdatedAt = read.GitHub.UpdatedAt
	return adj
}
