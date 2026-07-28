package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A completed batch records its size, its outcome, and its wall-clock.
func TestObserveReviewAllCompleted(t *testing.T) {
	m := New()
	m.ObserveReviewAll(42*time.Second, 5, true)

	body := scrape(t, m.Handler())
	for _, want := range []string{
		`gofer_review_all_total{outcome="completed"} 1`,
		`gofer_review_all_duration_seconds_count 1`,
		`gofer_review_all_batch_size_count 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}

// A timed-out batch counts under the timeout outcome and records its size, but does
// NOT record a duration — its true completion time is unknown, so the duration
// histogram stays a measure of completed batches only.
func TestObserveReviewAllTimeoutSkipsDuration(t *testing.T) {
	m := New()
	m.ObserveReviewAll(time.Minute, 3, false)

	body := scrape(t, m.Handler())
	if !strings.Contains(body, `gofer_review_all_total{outcome="timeout"} 1`) {
		t.Errorf("missing timeout counter\n%s", body)
	}
	if !strings.Contains(body, `gofer_review_all_duration_seconds_count 0`) {
		t.Errorf("a timed-out batch must not record a duration\n%s", body)
	}
	if !strings.Contains(body, `gofer_review_all_batch_size_count 1`) {
		t.Errorf("size should be recorded even on timeout\n%s", body)
	}
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", rec.Code)
	}
	return rec.Body.String()
}
