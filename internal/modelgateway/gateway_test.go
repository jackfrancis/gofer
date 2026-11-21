// Package modelgateway holds the end-to-end check of gofer's optional model gateway
// (docs/adr/0008-model-gateway.md): the real agentgateway binary, run on the config
// the deployment ships (deploy/k8s/model-gateway/config.yaml), in front of a fake
// model provider, authorizing calls against gofer's real router, and called by the
// workload's real model clients. It needs the gateway binary and skips without it:
//
//	AGENTGATEWAY_BIN=/path/to/agentgateway go test ./internal/modelgateway/
//
// (`make model-gateway-test` fetches the pinned release and runs it.)
package modelgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/llm"
	"github.com/jackfrancis/gofer/internal/server"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const upstreamKey = "upstream-model-key"

// upstreamCall is what the fake provider saw of one request.
type upstreamCall struct {
	Path, Model, Auth string
}

// fakeProvider answers chat-completions and Responses requests the way a provider
// does, and records each call: the path (which API), the model, and the bearer.
type fakeProvider struct {
	mu    sync.Mutex
	calls []upstreamCall
}

func (p *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p.mu.Lock()
	p.calls = append(p.calls, upstreamCall{Path: r.URL.Path, Model: body.Model, Auth: r.Header.Get("Authorization")})
	p.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+upstreamKey {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	text := "review from " + body.Model
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": time.Now().Unix(), "model": body.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": text}}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	case strings.HasSuffix(r.URL.Path, "/responses"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-1", "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": body.Model,
			"output": []map[string]any{{"type": "message", "id": "msg-1", "status": "completed", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}},
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	default:
		http.NotFound(w, r)
	}
}

func (p *fakeProvider) snapshot() []upstreamCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]upstreamCall(nil), p.calls...)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// renderConfig fills in the shipped config exactly as `make model-gateway-apply`
// does, and moves its listeners and key file to test-local ones.
func renderConfig(t *testing.T, upstream, authzHost string, burst int, port, ready, stats int, keyFile string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "model-gateway", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(raw)
	for old, repl := range map[string]string{
		"@UPSTREAM@":                   upstream,
		"@AUTHZ_HOST@":                 authzHost,
		"@BURST@":                      fmt.Sprint(burst),
		"@REFILL@":                     "1",
		"@INTERVAL@":                   "1h", // no refill during the test: the budget only drains
		"port: 4000":                   fmt.Sprintf("port: %d", port),
		"readinessAddr: 0.0.0.0:15021": fmt.Sprintf("readinessAddr: 127.0.0.1:%d", ready),
		"statsAddr: 0.0.0.0:15020":     fmt.Sprintf("statsAddr: 127.0.0.1:%d", stats),
		"file: /var/run/secrets/model-gateway/AI_TOKEN": "file: " + keyFile,
	} {
		if !strings.Contains(cfg, old) {
			t.Fatalf("shipped config no longer contains %q; update this test with it", old)
		}
		cfg = strings.ReplaceAll(cfg, old, repl)
	}
	return cfg
}

// The plan's Verify clause for the model gateway, against the real gateway:
//  1. two concurrent review runs make model calls without receiving the upstream key;
//  2. both consume the same rate limit;
//  3. a second opinion still reaches a different model.
func TestModelGateway(t *testing.T) {
	bin := os.Getenv("AGENTGATEWAY_BIN")
	if bin == "" {
		t.Skip("set AGENTGATEWAY_BIN to an agentgateway binary (make model-gateway-test) to run the model gateway check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	provider := &fakeProvider{}
	upstream := httptest.NewServer(provider)
	defer upstream.Close()

	gwPort, readyPort, statsPort := freePort(t), freePort(t), freePort(t)
	gw := fmt.Sprintf("http://127.0.0.1:%d", gwPort)
	completions, responses := gw+"/v1/chat/completions", gw+"/v1/responses"

	// gofer's real router in the gateway profile: its config loader decides the
	// connections, and the gateway asks its /agent/model-access whether a call may
	// proceed.
	conns, err := config.LoadConnections(func(k string) string {
		return map[string]string{
			"AI_GATEWAY_URL": gw,
			"AI_CONNECTIONS": `[{"endpoint":"` + completions + `","models":["model-a"]},{"endpoint":"` + responses + `","models":["model-b"]}]`,
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := identity.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	router, _, err := server.New(&config.Config{
		SessionSecret: []byte(strings.Repeat("s", 32)), Audience: "gofer-agent", MintPublicKey: pub,
		ConversationEnabled: true, AIConnections: conns,
	}, log, ingest.NoopDispatcher{}, vault.NewMemoryVault(), worklist.NewMemoryStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var authzCalls sync.Map
	gofer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/model-access" {
			authzCalls.Store(r.Header.Get("Authorization"), true)
		}
		router.ServeHTTP(w, r)
	}))
	defer gofer.Close()

	keyFile := filepath.Join(t.TempDir(), "AI_TOKEN")
	if err := os.WriteFile(keyFile, []byte(upstreamKey), 0o600); err != nil {
		t.Fatal(err)
	}
	const burst = 3
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	cfg := renderConfig(t, upstream.URL, strings.TrimPrefix(gofer.URL, "http://"), burst, gwPort, readyPort, statsPort, keyFile)
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var gwLog strings.Builder
	cmd := exec.CommandContext(ctx, bin, "-f", cfgFile)
	cmd.Stdout, cmd.Stderr = &gwLog, &gwLog
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")} // no ambient provider keys
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("agentgateway output:\n%s", gwLog.String())
		}
	}()
	waitReady(t, ctx, fmt.Sprintf("http://127.0.0.1:%d/healthz/ready", readyPort))

	// Each review run holds only its own run credential, as a dispatched run does.
	mint := identity.NewEd25519Authority(priv, "gofer-agent", 0)
	runCred := func(id string) string {
		tok, err := mint.Mint(identity.Claims{RunID: id, Subject: "github:1", Scopes: []string{"signals:read", "metadata:write"}})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	credA, credB := runCred("review-a"), runCred("review-b")

	// 1 and 3: two review runs at once, as Review-all and the review panel dispatch
	// them: run A asks the default model over chat-completions, run B is a second
	// opinion from model-b over the Responses API. Both use the workload's real model
	// client, configured as the runtime configures it behind the gateway.
	item := worklist.WorkItem{ID: "github:o/r#7", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 7, Title: "T"}}
	review := func(endpoint, model, cred string) (string, error) {
		c := llm.NewConverser(llm.Config{Endpoint: endpoint, Model: model, Token: cred, Logger: log})
		return c.Reply(ctx, item, "alice", "", nil, "review this", nil)
	}
	var wg sync.WaitGroup
	var replyA, replyB string
	var errA, errB error
	wg.Add(2)
	go func() { defer wg.Done(); replyA, errA = review(completions, "model-a", credA) }()
	go func() { defer wg.Done(); replyB, errB = review(responses, "model-b", credB) }()
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("concurrent reviews through the gateway: A=%v B=%v", errA, errB)
	}
	if !strings.Contains(replyA, "model-a") || !strings.Contains(replyB, "model-b") {
		t.Fatalf("replies = %q / %q, want each from its own model", replyA, replyB)
	}
	calls := provider.snapshot()
	if len(calls) != 2 {
		t.Fatalf("provider saw %d calls, want 2: %+v", len(calls), calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		// The gateway, not the run, supplied the key, and the run credential stopped
		// at the gateway.
		if c.Auth != "Bearer "+upstreamKey {
			t.Errorf("provider got Authorization %q, want the gateway's upstream key", c.Auth)
		}
		seen[c.Path+" "+c.Model] = true
	}
	// 3: the second opinion reached the other model, on its own API.
	if !seen["/chat/completions model-a"] || !seen["/responses model-b"] {
		t.Fatalf("provider calls = %+v, want model-a on /chat/completions and model-b on /responses", calls)
	}
	// Each run was authorized by its own run credential.
	for name, cred := range map[string]string{"A": credA, "B": credB} {
		if _, ok := authzCalls.Load("Bearer " + cred); !ok {
			t.Errorf("gofer never authorized run %s's credential", name)
		}
	}

	// 2: one budget. burst is 3 and two calls are spent, so run A's next call takes
	// the last token, and then neither run gets another: had each run its own
	// bucket, run B would still have two.
	raw := func(cred, endpoint, model string) int {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		if strings.HasSuffix(endpoint, "/responses") {
			body = `{"model":"` + model + `","input":"hi"}`
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cred != "" {
			req.Header.Set("Authorization", "Bearer "+cred)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), upstreamKey) {
			t.Errorf("a response to the run carried the upstream key: %s", b)
		}
		return resp.StatusCode
	}
	if code := raw(credA, completions, "model-a"); code != http.StatusOK {
		t.Fatalf("run A's third call: %d, want 200 (the budget's last token)", code)
	}
	if code := raw(credB, responses, "model-b"); code != http.StatusTooManyRequests {
		t.Fatalf("run B after the shared budget is spent: %d, want 429", code)
	}
	if code := raw(credA, completions, "model-a"); code != http.StatusTooManyRequests {
		t.Fatalf("run A after the shared budget is spent: %d, want 429", code)
	}
	if n := len(provider.snapshot()); n != burst {
		t.Fatalf("provider saw %d calls, want %d (refused calls never reach it)", n, burst)
	}

	// Without a run credential the gateway refuses, and the provider never hears of it.
	for name, cred := range map[string]string{"no bearer": "", "forged": "eyJ.forged"} {
		if code := raw(cred, completions, "model-a"); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("%s: %d, want 401 or 403", name, code)
		}
	}
	if n := len(provider.snapshot()); n != burst {
		t.Fatalf("provider saw %d calls after refused ones, want %d", n, burst)
	}
}

func waitReady(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("agentgateway never became ready at %s", url)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
