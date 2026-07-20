package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeVendor struct{ token string }

func (f fakeVendor) Vend(context.Context, string) (string, error) { return f.token, nil }

type memSink struct{ items []worklist.WorkItem }

func (m *memSink) List(context.Context) ([]worklist.WorkItem, error) { return m.items, nil }

func (m *memSink) Ingest(_ context.Context, items []worklist.WorkItem) error {
	for _, it := range items {
		found := false
		for i := range m.items {
			if m.items[i].ID == it.ID {
				m.items[i] = it
				found = true
				break
			}
		}
		if !found {
			m.items = append(m.items, it)
		}
	}
	return nil
}

// runIngest vends a token, fetches from GitHub, and writes to the sink.
func TestRunIngestFetchesAndWrites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("q"), "review-requested:@me") {
			_, _ = w.Write([]byte(`{"items":[{"number":7,"title":"T","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	sink := &memSink{}
	err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.items) != 1 || sink.items[0].GitHub.Number != 7 {
		t.Fatalf("expected 1 fetched item (#7), got %+v", sink.items)
	}
}

// runRank asks the ranker to propose axes and writes the proposal back.
func TestRunRankProposesAxes(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:      "github:o/r#1",
		Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}},
	}}}
	err := Run(context.Background(), Params{JobType: JobRank}, fakeVendor{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.items[0].Signals.Proposed == nil {
		t.Fatal("expected the stub ranker to set a proposal")
	}
}

func TestRunUnknownJobType(t *testing.T) {
	if err := Run(context.Background(), Params{JobType: "nope"}, fakeVendor{}, &memSink{}); err == nil {
		t.Fatal("expected an error for an unknown job type")
	}
}
