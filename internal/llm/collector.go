package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Collector counts what a run spent on the model: how many model calls it made, the
// wall time during which at least one was in flight, and how many tool calls the model
// asked for and the workload ran. The runtime attaches one to the run's context
// (WithCollector) and reports its totals on completion, so the control plane can tell
// time spent in the model from time spent waiting for a provider. It is safe for
// concurrent use.
//
// A model call is one request through chat, whichever transport answers it, and it is
// counted whether it succeeds or fails. Retries happen inside the HTTP client
// (httpretry), so a retried request counts once and its time includes the backoff.
//
// Model time is elapsed time, not summed request time: ranking issues several requests
// at once, and eight one-second requests running together are one second of the run
// spent in the model, not eight. So ModelDuration never exceeds the run's runtime.
type Collector struct {
	modelCalls atomic.Int64
	toolCalls  atomic.Int64

	mu     sync.Mutex
	active int           // model calls in flight
	since  time.Time     // when active last rose from zero
	busy   time.Duration // closed stretches with at least one call in flight
}

// ModelDuration is the wall time during which at least one model call was in flight,
// including a stretch still open.
func (c *Collector) ModelDuration() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.busy
	if c.active > 0 {
		d += time.Since(c.since)
	}
	return d
}

// ModelCalls is the number of model calls made.
func (c *Collector) ModelCalls() int { return int(c.modelCalls.Load()) }

// ToolCalls is the number of tool calls the model requested and the workload ran.
func (c *Collector) ToolCalls() int { return int(c.toolCalls.Load()) }

// beginModelCall marks a model call in flight and returns the func that ends it.
func (c *Collector) beginModelCall() func() {
	c.mu.Lock()
	if c.active == 0 {
		c.since = time.Now()
	}
	c.active++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.active--
		if c.active == 0 {
			c.busy += time.Since(c.since)
		}
		c.mu.Unlock()
		c.modelCalls.Add(1)
	}
}

type collectorKey struct{}

// WithCollector returns a context that records model and tool calls into a new
// Collector, and that Collector. Calls made under a context without one record
// nothing.
func WithCollector(ctx context.Context) (context.Context, *Collector) {
	c := &Collector{}
	return context.WithValue(ctx, collectorKey{}, c), c
}

func collectorFrom(ctx context.Context) *Collector {
	c, _ := ctx.Value(collectorKey{}).(*Collector)
	return c
}

// beginModelCall marks a model call in flight on the context's Collector and returns
// the func that ends it; both are no-ops without a Collector.
func beginModelCall(ctx context.Context) func() {
	if c := collectorFrom(ctx); c != nil {
		return c.beginModelCall()
	}
	return func() {}
}

func recordToolCall(ctx context.Context) {
	if c := collectorFrom(ctx); c != nil {
		c.toolCalls.Add(1)
	}
}
