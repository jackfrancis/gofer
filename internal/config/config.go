// Package config loads gofer's environment configuration. Only configured OAuth
// providers are enabled; unset ones stay disabled.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// Config holds all runtime configuration for the web tier and the dispatch tier.
type Config struct {
	Addr           string
	BaseURL        string
	AllowedOrigins []string
	SessionSecret  []byte
	CookieSecure   bool
	Providers      Providers

	// DispatchEndpoint is the AEI dispatch API base URL (the pre-installed
	// aei-controller), e.g. "http://aei-controller.aei-system.svc:8080". gofer POSTs
	// runs there through the app SDK (AEI_DISPATCH_ENDPOINT).
	DispatchEndpoint string

	// App is the AgentApp name gofer dispatches as (AEI_APP). The control plane
	// bounds every run by that app's policy and fixes the credential's audience.
	App string

	// SinkURL is the in-cluster URL a runtime calls back to reach gofer's agent
	// plane (/agent/worklist, /agent/credential). It differs from BaseURL (the
	// browser-facing URL): a runtime Job pod resolves gofer's Service, not the
	// port-forward. Carried to each run in its parameters (GOFER_SINK_URL).
	SinkURL string

	// Audience binds run credentials to the agent plane (ADR 0002); it must match
	// the AgentApp's spec.identity.audience so the token the control plane mints
	// verifies here.
	Audience string

	// BotReviewers are GitHub logins whose review requests are automated rather
	// than an explicit human ask (e.g. Prow's "k8s-ci-robot"). gofer treats a
	// review requested by one of these as a bare assignment, so it does not
	// inflate the radar. Configurable via BOT_REVIEWERS; defaults to k8s-ci-robot.
	BotReviewers []string

	// ConversationEnabled reports whether the assistive conversation UI (Discuss)
	// is offered. gofer brokers the model token to runtimes (see AI), so this is
	// gated on the full model configuration being present (AI != nil).
	ConversationEnabled bool

	// AI holds the chat-model coordinates and token, when configured (all of
	// AI_ENDPOINT, AI_MODEL, AI_TOKEN). gofer is the model-token BROKER: the web
	// tier holds the token and vends it to a runtime per run
	// (GET /agent/credential?provider=ai), while the non-secret endpoint/model
	// travel as run parameters — so the sandbox never holds a standing model
	// secret. Nil when AI is not configured (runtimes then use the stub ranker).
	AI *AIConfig

	// MintPublicKey verifies run credentials. gofer's web tier holds ONLY this: the
	// AEI control plane (configured with gofer's Ed25519 authority) is the sole
	// minter, so the web tier authenticates a runtime's token but can never forge
	// one (ADR 0002). Provisioned explicitly via MINT_PUBLIC_KEY.
	MintPublicKey ed25519.PublicKey
}

// AIConfig configures the chat model. The endpoint speaks the OpenAI-compatible
// chat-completions API. There are NO defaults: endpoint and model are required
// whenever AI is enabled (see LoadAI). The token is a secret and is the enabler.
type AIConfig struct {
	Endpoint string
	Model    string
	Token    string
}

// LoadAI reads and validates the AI_* environment. AI is enabled by AI_TOKEN (a
// secret): without it LoadAI returns (nil, nil) — AI is off and the caller uses
// the deterministic stub ranker. With a token, the provider coordinates
// AI_ENDPOINT and AI_MODEL must both be set too (there are no defaults), so a
// half-configured model fails fast rather than pointing somewhere surprising.
func LoadAI(getenv func(string) string) (*AIConfig, error) {
	endpoint := strings.TrimSpace(getenv("AI_ENDPOINT"))
	model := strings.TrimSpace(getenv("AI_MODEL"))
	token := strings.TrimSpace(getenv("AI_TOKEN"))
	if token == "" {
		return nil, nil // AI disabled → stub ranker
	}
	var missing []string
	if endpoint == "" {
		missing = append(missing, "AI_ENDPOINT")
	}
	if model == "" {
		missing = append(missing, "AI_MODEL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("AI_TOKEN is set but %s missing: set all of AI_ENDPOINT, AI_MODEL, AI_TOKEN to enable the model, or unset AI_TOKEN to use the stub ranker", strings.Join(missing, " and "))
	}
	return &AIConfig{Endpoint: endpoint, Model: model, Token: token}, nil
}

// Providers holds the OAuth client credentials for each supported provider. A
// provider with empty credentials is treated as disabled.
type Providers struct {
	Google          OAuthApp
	GitHub          OAuthApp
	Microsoft       OAuthApp
	MicrosoftTenant string
}

// OAuthApp holds the credentials for a single OAuth application.
type OAuthApp struct {
	ClientID     string
	ClientSecret string
}

// Enabled reports whether the OAuth app has been configured.
func (a OAuthApp) Enabled() bool { return a.ClientID != "" && a.ClientSecret != "" }

// Load reads configuration from environment variables and validates it.
func Load() (*Config, error) {
	secret := os.Getenv("SESSION_SECRET")
	if len(secret) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be set to at least 32 bytes")
	}

	mintPub, err := loadVerifyKey()
	if err != nil {
		return nil, err
	}

	botReviewers := splitAndTrim(os.Getenv("BOT_REVIEWERS"))
	if len(botReviewers) == 0 {
		botReviewers = []string{"k8s-ci-robot"}
	}

	ai, err := LoadAI(os.Getenv)
	if err != nil {
		return nil, err
	}

	return &Config{
		Addr:           getEnv("ADDR", ":8080"),
		BaseURL:        strings.TrimRight(getEnv("BASE_URL", "http://localhost:8080"), "/"),
		AllowedOrigins: splitAndTrim(os.Getenv("ALLOWED_ORIGINS")),
		SessionSecret:  []byte(secret),
		CookieSecure:   getEnv("COOKIE_SECURE", "false") == "true",
		Providers: Providers{
			Google: OAuthApp{
				ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
				ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			},
			GitHub: OAuthApp{
				ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
				ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
			},
			Microsoft: OAuthApp{
				ClientID:     os.Getenv("MICROSOFT_CLIENT_ID"),
				ClientSecret: os.Getenv("MICROSOFT_CLIENT_SECRET"),
			},
			MicrosoftTenant: getEnv("MICROSOFT_TENANT", "common"),
		},
		DispatchEndpoint:    os.Getenv("AEI_DISPATCH_ENDPOINT"),
		App:                 getEnv("AEI_APP", "gofer"),
		SinkURL:             getEnv("GOFER_SINK_URL", "http://gofer.gofer.svc.cluster.local:8080"),
		Audience:            getEnv("AEI_AUDIENCE", "gofer-agent"),
		BotReviewers:        botReviewers,
		ConversationEnabled: ai != nil,
		AI:                  ai,
		MintPublicKey:       mintPub,
	}, nil
}

// loadVerifyKey resolves the Ed25519 public key the web tier uses to verify run
// credentials (ADR 0002). gofer's web tier is verify-only: the AEI control plane
// holds the private key and is the sole minter, so MINT_PUBLIC_KEY is required and
// there is deliberately no private-key or derived-key path here — a web tier that
// could derive the private key would not be verify-only.
func loadVerifyKey() (ed25519.PublicKey, error) {
	s := os.Getenv("MINT_PUBLIC_KEY")
	if s == "" {
		return nil, fmt.Errorf("MINT_PUBLIC_KEY must be set: the web tier verifies run credentials with the AEI control plane's gofer public key (base64-encoded %d-byte Ed25519 public key)", ed25519.PublicKeySize)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("MINT_PUBLIC_KEY must be a base64-encoded %d-byte Ed25519 public key", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
