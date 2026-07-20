package llm

import "testing"

func TestTargetsCopilot(t *testing.T) {
	cases := map[string]bool{
		"https://api.githubcopilot.com/chat/completions": true,
		"https://API.GithubCopilot.com/v1/chat":          true, // host match is case-insensitive
		"https://api.openai.com/v1/chat/completions":     false,
		"http://gateway.internal:8080/chat":              false,
		"":                                               false,
	}
	for endpoint, want := range cases {
		if got := targetsCopilot(endpoint); got != want {
			t.Errorf("targetsCopilot(%q) = %v, want %v", endpoint, got, want)
		}
	}
}
