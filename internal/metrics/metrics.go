// Package metrics exposes gofer's Prometheus metrics: app-level aggregates that
// only the web tier can see. Per-run execution timing is AEI's to report — it owns
// dispatch and holds the authoritative run timestamps (aeiapp.Result.Timing). gofer
// measures what AEI structurally cannot: the wall-clock of a whole app action, such
// as the "Review all PRs" batch, which is one gofer action fanned out to many
// independent AEI runs. Handler serves the metrics at /metrics for Prometheus.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Outcome label values for a "Review all PRs" batch.
const (
	outcomeCompleted = "completed" // every dispatched review reached a terminal phase
	outcomeTimeout   = "timeout"   // gofer stopped waiting before all reviews finished
)

// durationBuckets span seconds to ~30m, sized for a fan-out of slow model runs:
// each github-converse run has a ~15m deadline, and a large radar runs in
// concurrency-bounded waves, so a batch can outlast a single run.
var durationBuckets = []float64{1, 2, 5, 10, 20, 30, 60, 120, 180, 300, 600, 900, 1200, 1800}

// sizeBuckets span a handful of PRs to a large radar.
var sizeBuckets = []float64{1, 2, 3, 5, 10, 20, 50, 100}

// Metrics holds gofer's Prometheus instruments over a private registry.
type Metrics struct {
	reg            *prometheus.Registry
	reviewDuration prometheus.Histogram
	reviewSize     prometheus.Histogram
	reviewTotal    *prometheus.CounterVec
}

// New builds the metrics over a fresh registry that also carries the standard Go
// and process collectors, so the same /metrics endpoint reports runtime health.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		reviewDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gofer_review_all_duration_seconds",
			Help:    "Wall-clock of a completed Review-all-PRs batch: from dispatch until every review run reached a terminal phase.",
			Buckets: durationBuckets,
		}),
		reviewSize: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gofer_review_all_batch_size",
			Help:    "Number of pull requests dispatched in a Review-all-PRs batch.",
			Buckets: sizeBuckets,
		}),
		reviewTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gofer_review_all_total",
			Help: "Review-all-PRs batches by outcome (completed = all reviews finished; timeout = gofer stopped waiting).",
		}, []string{"outcome"}),
	}
	reg.MustRegister(
		m.reviewDuration,
		m.reviewSize,
		m.reviewTotal,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// ObserveReviewAll records one Review-all-PRs batch: its size and outcome always,
// and its wall-clock only when it completed (a timed-out batch has no true
// completion time, so recording the wait window would skew the duration). It
// satisfies ingest.BatchObserver and is safe for concurrent use.
func (m *Metrics) ObserveReviewAll(d time.Duration, size int, completed bool) {
	outcome := outcomeTimeout
	if completed {
		outcome = outcomeCompleted
		m.reviewDuration.Observe(d.Seconds())
	}
	m.reviewSize.Observe(float64(size))
	m.reviewTotal.WithLabelValues(outcome).Inc()
}

// Handler serves the collected metrics in the Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}
