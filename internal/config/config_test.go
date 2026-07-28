package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConnections(t *testing.T) {
	const oneConn = `[{"endpoint":"https://x/chat","models":["a","b"]}]`

	t.Run("disabled without a token", func(t *testing.T) {
		// Connections present but no token -> AI off (the structured input is inert).
		conns, err := LoadConnections(env(map[string]string{"AI_CONNECTIONS": oneConn}))
		if err != nil {
			t.Fatal(err)
		}
		if len(conns) != 0 {
			t.Fatalf("expected disabled (no connections), got %+v", conns)
		}
	})

	t.Run("the shared token fans out; order sets the default connection", func(t *testing.T) {
		conns, err := LoadConnections(env(map[string]string{
			"AI_TOKEN": "t",
			"AI_CONNECTIONS": `[
				{"endpoint":"https://one/chat","models":["a","b"]},
				{"endpoint":"https://two/chat","models":["c"]}
			]`,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if len(conns) != 2 {
			t.Fatalf("expected 2 connections, got %d", len(conns))
		}
		// Element 0 is the default connection; its models[0] is its default model.
		if conns[0].Endpoint != "https://one/chat" || conns[0].Default() != "a" {
			t.Fatalf("default connection wrong: %+v", conns[0])
		}
		if others := conns[0].Others(); len(others) != 1 || others[0] != "b" {
			t.Fatalf("default connection alternatives = %v, want [b]", others)
		}
		if conns[1].Endpoint != "https://two/chat" || conns[1].Default() != "c" {
			t.Fatalf("second connection wrong: %+v", conns[1])
		}
		// AI_TOKEN is dispatched to every connection.
		for i, c := range conns {
			if c.Token != "t" {
				t.Errorf("connection %d token = %q, want the shared t", i, c.Token)
			}
		}
	})

	t.Run("a token without connections fails fast", func(t *testing.T) {
		_, err := LoadConnections(env(map[string]string{"AI_TOKEN": "t"}))
		if err == nil || !strings.Contains(err.Error(), "AI_CONNECTIONS") {
			t.Fatalf("expected an AI_CONNECTIONS error, got %v", err)
		}
	})

	t.Run("invalid JSON fails fast", func(t *testing.T) {
		_, err := LoadConnections(env(map[string]string{"AI_TOKEN": "t", "AI_CONNECTIONS": "not json"}))
		if err == nil || !strings.Contains(err.Error(), "valid JSON") {
			t.Fatalf("expected a JSON error, got %v", err)
		}
	})

	t.Run("a connection without an endpoint fails fast", func(t *testing.T) {
		_, err := LoadConnections(env(map[string]string{"AI_TOKEN": "t", "AI_CONNECTIONS": `[{"models":["a"]}]`}))
		if err == nil || !strings.Contains(err.Error(), "endpoint is required") {
			t.Fatalf("expected an endpoint error, got %v", err)
		}
	})

	t.Run("a connection without models fails fast", func(t *testing.T) {
		_, err := LoadConnections(env(map[string]string{"AI_TOKEN": "t", "AI_CONNECTIONS": `[{"endpoint":"https://x/chat"}]`}))
		if err == nil || !strings.Contains(err.Error(), "at least one model") {
			t.Fatalf("expected a models error, got %v", err)
		}
	})

	t.Run("whitespace token disables AI", func(t *testing.T) {
		conns, err := LoadConnections(env(map[string]string{"AI_TOKEN": "   ", "AI_CONNECTIONS": oneConn}))
		if err != nil {
			t.Fatal(err)
		}
		if len(conns) != 0 {
			t.Fatalf("whitespace token should disable AI, got %+v", conns)
		}
	})

	t.Run("model ids are trimmed and de-duplicated per connection", func(t *testing.T) {
		conns, err := LoadConnections(env(map[string]string{
			"AI_TOKEN":       "t",
			"AI_CONNECTIONS": `[{"endpoint":"https://x/chat","models":[" a ","a","b"," "]}]`,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if len(conns) != 1 || len(conns[0].Models) != 2 || conns[0].Models[0] != "a" || conns[0].Models[1] != "b" {
			t.Fatalf("expected [a b], got %+v", conns[0].Models)
		}
	})
}

func TestModelChoices(t *testing.T) {
	load := func(connsJSON string) *Config {
		conns, err := LoadConnections(env(map[string]string{"AI_TOKEN": "t", "AI_CONNECTIONS": connsJSON}))
		if err != nil {
			t.Fatal(err)
		}
		return &Config{AIConnections: conns}
	}
	labels := func(cs []ModelChoice) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.Label
		}
		return out
	}
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	t.Run("no collision: bare model labels across connections", func(t *testing.T) {
		c := load(`[
			{"endpoint":"https://api.githubcopilot.com/chat/completions","models":["claude-opus-4.8","gpt-5.4"]},
			{"endpoint":"https://api.openai.com/v1/responses","models":["gpt-5.6"]}
		]`)
		if got, want := labels(c.ModelChoices()), []string{"claude-opus-4.8", "gpt-5.4", "gpt-5.6"}; !eq(got, want) {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	})

	t.Run("collision: the shared id is disambiguated by connection", func(t *testing.T) {
		c := load(`[
			{"endpoint":"https://api.githubcopilot.com/chat/completions","models":["claude-opus-4.8","gpt-5.4"]},
			{"endpoint":"https://api.openai.com/v1/responses","models":["gpt-5.4"]}
		]`)
		if got, want := labels(c.ModelChoices()), []string{"claude-opus-4.8", "gpt-5.4 (GitHub Copilot)", "gpt-5.4 (OpenAI)"}; !eq(got, want) {
			t.Fatalf("labels = %v, want %v", got, want)
		}
		// Each choice carries its own connection's endpoint for routing.
		for _, ch := range c.ModelChoices() {
			if ch.Label == "gpt-5.4 (OpenAI)" && ch.Endpoint != "https://api.openai.com/v1/responses" {
				t.Fatalf("OpenAI gpt-5.4 endpoint = %q", ch.Endpoint)
			}
		}
	})
}
