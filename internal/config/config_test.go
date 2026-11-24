package config

import (
	"strings"
	"testing"
	"time"
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

func TestLoadVaultKey(t *testing.T) {
	// base64 of 32 bytes.
	const good = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

	t.Run("not read without a data directory", func(t *testing.T) {
		key, err := loadVaultKey("", env(map[string]string{"VAULT_ENCRYPTION_KEY": "not base64"}))
		if err != nil || key != nil {
			t.Fatalf("in-memory mode must ignore the key: key=%v err=%v", key, err)
		}
	})

	t.Run("required with a data directory", func(t *testing.T) {
		if _, err := loadVaultKey("/data", env(nil)); err == nil {
			t.Fatal("expected an error: persistent storage without an encryption key")
		}
	})

	t.Run("decodes a 32-byte key", func(t *testing.T) {
		key, err := loadVaultKey("/data", env(map[string]string{"VAULT_ENCRYPTION_KEY": good + "\n"}))
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != 32 || key[31] != 31 {
			t.Fatalf("unexpected key: % x", key)
		}
	})

	t.Run("rejects a malformed key without echoing it", func(t *testing.T) {
		for _, bad := range []string{"not-base64!", "c2hvcnQ="} {
			_, err := loadVaultKey("/data", env(map[string]string{"VAULT_ENCRYPTION_KEY": bad}))
			if err == nil {
				t.Fatalf("expected an error for %q", bad)
			}
			if strings.Contains(err.Error(), bad) {
				t.Fatalf("error echoes the key material: %v", err)
			}
		}
	})
}

func TestSignInAllowed(t *testing.T) {
	open := &Config{}
	listed := &Config{AllowedGitHubLogins: lowerAll(splitAndTrim(" OctoCat , hubot,,"))}
	cases := []struct {
		cfg             *Config
		provider, login string
		want            bool
	}{
		{open, "github", "anyone", true},
		{open, "google", "", true},
		{listed, "github", "octocat", true},
		{listed, "github", "OCTOCAT", true}, // case-insensitive
		{listed, "github", "hubot", true},
		{listed, "github", "mallory", false},
		{listed, "github", "", false},        // no login to match
		{listed, "google", "octocat", false}, // only GitHub logins can be listed
		{listed, "microsoft", "", false},
	}
	for _, c := range cases {
		if got := c.cfg.SignInAllowed(c.provider, c.login); got != c.want {
			t.Errorf("SignInAllowed(%q, %q) with %v = %v, want %v", c.provider, c.login, c.cfg.AllowedGitHubLogins, got, c.want)
		}
	}
	if len(listed.AllowedGitHubLogins) != 2 {
		t.Errorf("parsed allow-list = %q, want [octocat hubot]", listed.AllowedGitHubLogins)
	}
}

func TestParseGitHubKind(t *testing.T) {
	for in, want := range map[string]string{"": GitHubOAuthApp, " oauth-app ": GitHubOAuthApp, "github-app": GitHubApp} {
		got, err := parseGitHubKind(in)
		if err != nil || got != want {
			t.Errorf("parseGitHubKind(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// gofer never guesses the kind, so anything else is refused at startup.
	for _, in := range []string{"app", "GitHub-App", "Iv1.abc"} {
		if _, err := parseGitHubKind(in); err == nil {
			t.Errorf("parseGitHubKind(%q) accepted", in)
		}
	}
	if (Providers{GitHubKind: GitHubApp}).IsGitHubApp() != true || (Providers{}).IsGitHubApp() {
		t.Error("IsGitHubApp")
	}
}

func TestParseRefreshInterval(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "0", want: 0},
		{in: "0s", want: 0},
		{in: " 15m ", want: 15 * time.Minute},
		{in: "1m", want: time.Minute},
		{in: "59s", wantErr: true},
		{in: "-5m", wantErr: true},
		{in: "soon", wantErr: true},
		{in: "15", wantErr: true},
	} {
		got, err := parseRefreshInterval(env(map[string]string{"REFRESH_INTERVAL": tc.in}))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("REFRESH_INTERVAL=%q: got %v, %v; want %v (error %v)", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
	if got, err := parseRefreshInterval(env(nil)); err != nil || got != 0 {
		t.Fatalf("unset REFRESH_INTERVAL must leave the schedule off: %v, %v", got, err)
	}
}

// The write client (ADR 0009) is optional, needs GitHub sign-in, comes as a pair, and
// must be a different app from sign-in, whose token runs hold.
func TestValidateGitHubWrite(t *testing.T) {
	signIn := OAuthApp{ClientID: "cid", ClientSecret: "csec"}
	cases := []struct {
		name    string
		p       Providers
		wantErr bool
		enabled bool
	}{
		{"unset", Providers{GitHub: signIn}, false, false},
		{"set", Providers{GitHub: signIn, GitHubWrite: OAuthApp{ClientID: "wcid", ClientSecret: "wsec"}}, false, true},
		{"id only", Providers{GitHub: signIn, GitHubWrite: OAuthApp{ClientID: "wcid"}}, true, false},
		{"secret only", Providers{GitHub: signIn, GitHubWrite: OAuthApp{ClientSecret: "wsec"}}, true, false},
		{"no sign-in", Providers{GitHubWrite: OAuthApp{ClientID: "wcid", ClientSecret: "wsec"}}, true, false},
		{"same app as sign-in", Providers{GitHub: signIn, GitHubWrite: OAuthApp{ClientID: "cid", ClientSecret: "csec"}}, true, true},
		// A token is an alternative to the write client's grant, never a replacement.
		{"tokens with the write client", Providers{GitHub: signIn, GitHubWrite: OAuthApp{ClientID: "wcid", ClientSecret: "wsec"}, GitHubWriteTokens: true}, false, true},
		{"tokens without the write client", Providers{GitHub: signIn, GitHubWriteTokens: true}, true, false},
	}
	for _, tc := range cases {
		err := validateGitHubWrite(tc.p)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want error %v", tc.name, err, tc.wantErr)
		}
		if !tc.wantErr && tc.p.CommentingEnabled() != tc.enabled {
			t.Errorf("%s: CommentingEnabled = %v", tc.name, tc.p.CommentingEnabled())
		}
	}
}
