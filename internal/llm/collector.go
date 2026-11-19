package llm

import (
	"context"
	"sync/atomic"
	"time"
)

// Collector counts what a run spent on the model: how many model calls it made, their
// summed wall time, and how many tool calls the model asked for and the workload ran.
// The runtime attaches one to the run's context (WithCollector) and reports its totals
// on completion, so the control plane can tell time spent in the model from time spent
// waiting for a provider. It is safe for concurrent use: ranking calls the model from
// several goroutines.
//
// A model call is one request through chat, whichever transport answers it, and it is
// counted whether it succeeds or fails. Retries happen inside the HTTP client
// (httpretry), so a retried request counts once and its time includes the backoff.
type Collector struct {
	modelNanos atomic.Int64
	modelCalls atomic.Int64
	toolCalls  atomic.Int64
}

// ModelDuration is the summed wall time of every model call.
func (c *Collector) ModelDuration() time.Duration { return time.Duration(c.modelNanos.Load()) }

// ModelCalls is the number of model calls made.
func (c *Collector) ModelCalls() int { return int(c.modelCalls.Load()) }

// ToolCalls is the number of tool calls the model requested and the workload ran.
func (c *Collector) ToolCalls() int { return int(c.toolCalls.Load()) }

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

func recordModelCall(ctx context.Context, d time.Duration) {
	if c := collectorFrom(ctx); c != nil {
		c.modelNanos.Add(int64(d))
		c.modelCalls.Add(1)
	}
}

func recordToolCall(ctx context.Context) {
	if c := collectorFrom(ctx); c != nil {
		c.toolCalls.Add(1)
	}
}
