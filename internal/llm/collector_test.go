package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// toolThenAnswer is a chat-completions model that asks for one tool on its first turn
// and answers on the next, sleeping delay on each so model time is measurable.
func toolThenAnswer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	var (
		mu    sync.Mutex
		turns int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()
		if n == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"github_read_file","arguments":"{\"path\":\"a\"}"}},`+
				`{"id":"c2","type":"function","function":{"name":"github_read_file","arguments":"{\"path\":\"b\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"done"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A tool-using review records each model round and each tool call the workload ran,
// and the model's wall time, on the run's collector.
func TestCollectorCountsAToolUsingReview(t *testing.T) {
	const delay = 20 * time.Millisecond
	srv := toolThenAnswer(t, delay)
	ctx, col := WithCollector(context.Background())

	box := &fakeToolBox{result: "contents"}
	c := NewConverser(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	item := worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "o/r", Title: "T"}, Type: worklist.TypePullRequest}
	if _, err := c.Reply(ctx, item, "alice", "", nil, "review this", box); err != nil {
		t.Fatal(err)
	}
	if got := col.ModelCalls(); got != 2 {
		t.Errorf("model calls = %d, want 2 (a tool round and the answer)", got)
	}
	if got, want := col.ToolCalls(), len(box.invoked); got != want || got != 2 {
		t.Errorf("tool calls = %d, want %d (every call the toolbox ran)", got, want)
	}
	if got := col.ModelDuration(); got < 2*delay {
		t.Errorf("model time = %v, want at least %v (two calls of %v)", got, 2*delay, delay)
	}
}

// Both transports are counted: chat dispatches to the Responses API by endpoint path,
// and that call is a model call like any other.
func TestCollectorCountsTheResponsesTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
	}))
	defer srv.Close()
	ctx, col := WithCollector(context.Background())

	if _, err := chat(ctx, srv.Client(), srv.URL+"/v1/responses", "tok", chatRequest{Model: "m", Messages: []chatMessage{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if got := col.ModelCalls(); got != 1 {
		t.Errorf("model calls = %d, want 1", got)
	}
}

// A failed model call still spent model time, so it is counted.
func TestCollectorCountsAFailedCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	ctx, col := WithCollector(context.Background())

	if _, err := chat(ctx, srv.Client(), srv.URL, "tok", chatRequest{Model: "m"}); err == nil {
		t.Fatal("want an error from a 400")
	}
	if got := col.ModelCalls(); got != 1 {
		t.Errorf("model calls = %d, want 1", got)
	}
}

// Concurrent model calls (ranking fans out) are all counted.
func TestCollectorIsSafeForConcurrentCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	ctx, col := WithCollector(context.Background())

	const n = 16
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = chat(ctx, srv.Client(), srv.URL, "tok", chatRequest{Model: "m"})
		}()
	}
	wg.Wait()
	if got := col.ModelCalls(); got != n {
		t.Errorf("model calls = %d, want %d", got, n)
	}
}

// Overlapping model calls count their elapsed time once: n requests held in flight
// together for d report about d of model time, not n*d, while every request still
// counts as a call. Ranking runs requests concurrently, so a sum would overstate the
// model's share of the run.
func TestCollectorCountsOverlappingTimeOnce(t *testing.T) {
	const (
		n = 8
		d = 100 * time.Millisecond
	)
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Done()
		<-release
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	ctx, col := WithCollector(context.Background())

	var done sync.WaitGroup
	for range n {
		done.Add(1)
		go func() {
			defer done.Done()
			_, _ = chat(ctx, srv.Client(), srv.URL, "tok", chatRequest{Model: "m"})
		}()
	}
	arrived.Wait() // all n in flight at once
	time.Sleep(d)
	close(release)
	done.Wait()

	if got := col.ModelCalls(); got != n {
		t.Errorf("model calls = %d, want %d", got, n)
	}
	if got := col.ModelDuration(); got < d || got >= 2*d {
		t.Errorf("model time = %v for %d overlapping calls held %v, want about %v (not %v)", got, n, d, d, n*d)
	}
}

// Model time adds up across calls that do not overlap, and an open stretch counts
// while a call is still in flight.
func TestCollectorAddsSequentialStretches(t *testing.T) {
	var c Collector
	end := c.beginModelCall()
	time.Sleep(20 * time.Millisecond)
	if got := c.ModelDuration(); got < 20*time.Millisecond {
		t.Errorf("open stretch = %v, want at least 20ms", got)
	}
	end()
	end = c.beginModelCall()
	time.Sleep(20 * time.Millisecond)
	end()
	if got := c.ModelDuration(); got < 40*time.Millisecond {
		t.Errorf("model time = %v over two 20ms calls, want at least 40ms", got)
	}
	if got := c.ModelCalls(); got != 2 {
		t.Errorf("model calls = %d, want 2", got)
	}
}

// Without a collector on the context (tests, any caller outside a run), the tool loop
// runs as before: recording is a no-op, not a nil dereference.
func TestToolLoopRunsWithoutACollector(t *testing.T) {
	srv := toolThenAnswer(t, 0)
	box := &fakeToolBox{result: "contents"}
	c := NewConverser(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	if _, err := c.Reply(context.Background(), worklist.WorkItem{}, "", "", nil, "q", box); err != nil {
		t.Fatal(err)
	}
	if len(box.invoked) != 2 {
		t.Fatalf("tools invoked %d times, want 2", len(box.invoked))
	}
}
