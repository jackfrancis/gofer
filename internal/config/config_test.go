package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadAI(t *testing.T) {
	t.Run("disabled without a token", func(t *testing.T) {
		// Endpoint/model present but no token -> AI off (coordinates are inert).
		ai, err := LoadAI(env(map[string]string{"AI_ENDPOINT": "https://x", "AI_MODEL": "m"}))
		if err != nil {
			t.Fatal(err)
		}
		if ai != nil {
			t.Fatalf("expected nil (disabled), got %+v", ai)
		}
	})

	t.Run("enabled with all three", func(t *testing.T) {
		ai, err := LoadAI(env(map[string]string{"AI_ENDPOINT": "https://x", "AI_MODEL": "m", "AI_TOKEN": "t"}))
		if err != nil {
			t.Fatal(err)
		}
		if ai == nil || ai.Endpoint != "https://x" || ai.Model != "m" || ai.Token != "t" {
			t.Fatalf("unexpected config: %+v", ai)
		}
	})

	t.Run("token without coordinates fails fast", func(t *testing.T) {
		_, err := LoadAI(env(map[string]string{"AI_TOKEN": "t"}))
		if err == nil || !strings.Contains(err.Error(), "AI_ENDPOINT") || !strings.Contains(err.Error(), "AI_MODEL") {
			t.Fatalf("expected error naming both missing vars, got %v", err)
		}
	})

	t.Run("token with endpoint but no model fails fast", func(t *testing.T) {
		_, err := LoadAI(env(map[string]string{"AI_TOKEN": "t", "AI_ENDPOINT": "https://x"}))
		if err == nil || !strings.Contains(err.Error(), "AI_MODEL") {
			t.Fatalf("expected AI_MODEL error, got %v", err)
		}
	})

	t.Run("whitespace is treated as unset", func(t *testing.T) {
		ai, err := LoadAI(env(map[string]string{"AI_TOKEN": "   "}))
		if err != nil {
			t.Fatal(err)
		}
		if ai != nil {
			t.Fatalf("whitespace token should disable AI, got %+v", ai)
		}
	})
}
