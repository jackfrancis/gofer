package config

import (
	"strings"
	"testing"
)

// The model-gateway profile (AI_GATEWAY_URL) enables the model with no AI_TOKEN: the
// gateway holds the upstream key. These pin the rules that keep a run credential,
// which a runtime presents to a gateway connection, from ever reaching a provider.
func TestLoadConnectionsGateway(t *testing.T) {
	const gw = "http://gofer-model-gateway.gofer.svc.cluster.local"
	const conns = `[
		{"endpoint":"http://gofer-model-gateway.gofer.svc.cluster.local/v1/chat/completions","models":["a","b"]},
		{"endpoint":"http://GOFER-MODEL-GATEWAY.gofer.svc.cluster.local/v1/responses","models":["c"]}
	]`

	t.Run("enabled without a token; every connection is a gateway connection", func(t *testing.T) {
		got, err := LoadConnections(env(map[string]string{"AI_GATEWAY_URL": gw, "AI_CONNECTIONS": conns}))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d connections, want 2", len(got))
		}
		for i, c := range got {
			if !c.Gateway || c.Token != "" {
				t.Errorf("connection %d: Gateway=%v Token=%q, want a tokenless gateway connection", i, c.Gateway, c.Token)
			}
		}
	})

	t.Run("the web tier may not also hold the key", func(t *testing.T) {
		_, err := LoadConnections(env(map[string]string{"AI_GATEWAY_URL": gw, "AI_TOKEN": "k", "AI_CONNECTIONS": conns}))
		if err == nil || !strings.Contains(err.Error(), "AI_TOKEN") {
			t.Fatalf("err = %v, want a refusal naming AI_TOKEN", err)
		}
	})

	t.Run("a connection the gateway does not serve is refused", func(t *testing.T) {
		// A stale AI_CONNECTIONS that still names the provider would send each run's
		// credential to it. Startup fails instead.
		for _, ep := range []string{
			"https://api.githubcopilot.com/chat/completions",
			"https://gofer-model-gateway.gofer.svc.cluster.local/v1/chat/completions", // scheme differs
			"http://gofer-model-gateway.gofer.svc.cluster.local:8080/v1/responses",    // port differs
		} {
			_, err := LoadConnections(env(map[string]string{
				"AI_GATEWAY_URL": gw,
				"AI_CONNECTIONS": `[{"endpoint":"` + ep + `","models":["a"]}]`,
			}))
			if err == nil || !strings.Contains(err.Error(), "served by the gateway") {
				t.Errorf("%s: err = %v, want a refusal", ep, err)
			}
		}
	})

	t.Run("the gateway needs connections", func(t *testing.T) {
		_, err := LoadConnections(env(map[string]string{"AI_GATEWAY_URL": gw}))
		if err == nil || !strings.Contains(err.Error(), "AI_CONNECTIONS") {
			t.Fatalf("err = %v, want a refusal naming AI_CONNECTIONS", err)
		}
	})

	t.Run("a malformed gateway URL is refused", func(t *testing.T) {
		for _, u := range []string{"gofer-model-gateway", "ftp://gw", "http://user:pw@gw", "http://"} {
			_, err := LoadConnections(env(map[string]string{"AI_GATEWAY_URL": u, "AI_CONNECTIONS": conns}))
			if err == nil || !strings.Contains(err.Error(), "AI_GATEWAY_URL") {
				t.Errorf("%q: err = %v, want a refusal", u, err)
			}
		}
	})

	t.Run("without the gateway, connections carry the token and no gateway mark", func(t *testing.T) {
		got, err := LoadConnections(env(map[string]string{"AI_TOKEN": "k", "AI_CONNECTIONS": `[{"endpoint":"https://x/chat","models":["a"]}]`}))
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Gateway || got[0].Token != "k" {
			t.Fatalf("got %+v, want a vended-token connection", got[0])
		}
	})
}
