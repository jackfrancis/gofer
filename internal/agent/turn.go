package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

const (
	// turnReadLimit is a SPEND ceiling on model calls per pass. A reading is cached
	// against the last word it read (worklist.Signals.CurrentTurnReading), so a pass
	// only reads last words written since the previous one; the ceiling bounds a
	// first pass over a large worklist, and the next pass reads the rest.
	turnReadLimit       = 50
	turnReadConcurrency = 4
	// turnReadReserve is the slice of the run's remaining time the pass leaves for
	// writing its readings back and ranking.
	turnReadReserve = 45 * time.Second
)

// runReadTurns asks the TurnReader to read the user's last word on every open item
// waiting on others whose last word has no current reading
// (worklist.Signals.NeedsTurnReading), and writes back the readings. It is
// best-effort, like rank: an item it does not read is left to the timeline, which
// already counts it as waiting. With no TurnReader (no chat model) it does nothing.
//
// The readings are written alone (Sink.Turns), never the items they were read from,
// in one write once every model call has answered: a refresh, a hide or an unhide
// that landed while the model worked stands, and a reading of a last word that is no
// longer current is dropped.
func runReadTurns(ctx context.Context, p Params, sink Sink) error {
	if p.TurnReader == nil {
		return nil
	}
	log := p.logger()
	items, err := readAll(ctx, p, sink, worklist.Selection{Open: true})
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	var todo []worklist.WorkItem
	for _, it := range items {
		if it.OnRadar() && it.Signals.NeedsTurnReading() {
			todo = append(todo, it)
		}
	}
	deferred := 0
	if len(todo) > turnReadLimit {
		deferred = len(todo) - turnReadLimit
		todo = todo[:turnReadLimit]
	}
	if len(todo) == 0 {
		return nil
	}
	log.Info("turns: reading last words", "items", len(todo), "deferred", deferred)

	var (
		mu       sync.Mutex
		readings []worklist.ItemTurn
		wg       sync.WaitGroup
		sem      = make(chan struct{}, turnReadConcurrency)
		skipped  int
	)
	for i := range todo {
		if !hasBudget(ctx, turnReadReserve) {
			skipped = len(todo) - i
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it worklist.WorkItem) {
			defer wg.Done()
			defer func() { <-sem }()
			rctx, cancel := reserveContext(ctx, turnReadReserve)
			r, err := p.TurnReader.ReadTurn(rctx, it)
			cancel()
			if err != nil {
				log.Warn("turns: reading failed", "item", it.ID, "err", err)
				return // best-effort: the timeline decides
			}
			r.LastWord = it.Signals.LastWord.Key()
			r.ReadAt = time.Now().UTC()
			mu.Lock()
			readings = append(readings, worklist.ItemTurn{ItemID: it.ID, Reading: r})
			mu.Unlock()
		}(todo[i])
	}
	wg.Wait()
	log.Info("turns: sending readings", "readings", len(readings), "skipped_out_of_time", skipped)
	if err := sink.Turns(ctx, readings); err != nil {
		return fmt.Errorf("turns: %w", err)
	}
	return nil
}
