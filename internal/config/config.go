// Package config loads gofer's environment configuration. Only configured OAuth
// providers are enabled; unset ones stay disabled.
package config

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
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

	// SinkURL is the in-cluster URL an agent runtime would call back to reach gofer's
	// agent plane (/agent/worklist, /agent/credential). It differs from BaseURL (the
	// browser-facing URL): a runtime pod resolves gofer's Service, not the
	// port-forward. Carried to each run in its parameters as gofer_url (GOFER_SINK_URL).
	SinkURL string

	// Audience binds a run credential to gofer's agent plane; a runtime's token must
	// carry this audience to authenticate there (AGENT_AUDIENCE).
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

	// AIConnections holds the configured chat-model connections (see LoadConnection).
	// gofer is the model-token BROKER: the web tier holds each connection's token and
	// vends it to a runtime per run (GET /agent/credential?provider=ai), while the
	// non-secret endpoint/model travel as run parameters — so the sandbox never holds a
	// standing model secret. Config.Load always populates exactly one connection from
	// the environment (element 0, disabled when AI is off); multi-connection support
	// will grow this list, so callers read element 0 for now.
	AIConnections []AIConnection

	// MintPublicKey verifies run credentials on the agent plane. gofer's web tier
	// holds ONLY the public half: a runtime's control plane is the sole minter, so the
	// web tier authenticates a runtime's token but can never forge one. Provisioned
	// explicitly via MINT_PUBLIC_KEY.
	MintPublicKey ed25519.PublicKey
}

// DefaultConnection returns the default chat-model connection: the first configured.
// The default is positional — changing it is a re-ordering of the AI_CONNECTIONS list
// (element 0 wins), not a separate selector (a UI could later drive that re-order). It
// returns a zero (disabled) connection when none are configured, so callers may use it
// unconditionally.
func (c *Config) DefaultConnection() AIConnection {
	if len(c.AIConnections) == 0 {
		return AIConnection{}
	}
	return c.AIConnections[0]
}

// ModelChoice is one selectable (connection, model) option for the UI: the model id,
// the connection it belongs to (its stable ConnID and Endpoint, for routing), and a
// display Label. Label is the bare model id, except when the same id is offered by
// more than one connection, in which case it is suffixed with the connection's Label
// (e.g. "gpt-5.4 (OpenAI)") so the colliding options stay distinguishable.
type ModelChoice struct {
	ConnID   string
	Endpoint string
	Model    string
	Label    string
}

// ModelChoices flattens every connection's models into one ordered list of selectable
// choices (connections in priority order, models in order within each). A model id
// offered by more than one connection is disambiguated in its Label with the
// connection's Label; unique ids stay bare. Choice 0 is the default model of the
// default connection.
func (c *Config) ModelChoices() []ModelChoice {
	connsPerModel := map[string]int{}
	for _, conn := range c.AIConnections {
		for _, m := range conn.Models {
			connsPerModel[m]++
		}
	}
	var out []ModelChoice
	for _, conn := range c.AIConnections {
		for _, m := range conn.Models {
			label := m
			if connsPerModel[m] > 1 {
				label = m + " (" + conn.Label() + ")"
			}
			out = append(out, ModelChoice{ConnID: conn.ID, Endpoint: conn.Endpoint, Model: m, Label: label})
		}
	}
	return out
}

// AIConnection is a configured connection to one chat-model API: an endpoint and the
// model ids reachable over it, plus a bearer token. The structured config
// (AI_CONNECTIONS) carries only endpoint+models per connection; the token is the
// shared AI_TOKEN, fanned out to every connection at load, so Token is never part of
// the wire format (json:"-"). Models[0] is the default model (used when a run names
// none); the rest are selectable alternatives (e.g. a "second opinion" review). The
// zero value means the connection is disabled.
type AIConnection struct {
	Endpoint string   `json:"endpoint"` // OpenAI-compatible chat-completions endpoint
	Models   []string `json:"models"`   // model ids in order; Models[0] is the default
	Token    string   `json:"-"`        // shared AI_TOKEN, fanned out at load; not wire data
	ID       string   `json:"-"`        // stable, non-positional id (hash of Endpoint); set at load
}

// Enabled reports whether any chat model is configured.
func (c AIConnection) Enabled() bool { return len(c.Models) > 0 }

// Default returns the default model id — the first configured — or "" when the
// connection is disabled.
func (c AIConnection) Default() string {
	if len(c.Models) == 0 {
		return ""
	}
	return c.Models[0]
}

// Others returns the alternative (non-default) model ids in declaration order — the
// pool a "second opinion" review draws from. It is nil for a single-model setup.
func (c AIConnection) Others() []string {
	if len(c.Models) <= 1 {
		return nil
	}
	return append([]string(nil), c.Models[1:]...)
}

// Has reports whether id is one of the enabled models.
func (c AIConnection) Has(id string) bool {
	for _, x := range c.Models {
		if x == id {
			return true
		}
	}
	return false
}

// Label is a short human name for the connection, derived from its endpoint host —
// used only to disambiguate a model id offered by more than one connection. It is a
// display hint, not an identity (that is ID).
func (c AIConnection) Label() string {
	host := c.Endpoint
	if u, err := url.Parse(c.Endpoint); err == nil && u.Host != "" {
		host = u.Host
	}
	switch h := strings.ToLower(host); {
	case strings.Contains(h, "githubcopilot"):
		return "GitHub Copilot"
	case strings.Contains(h, "openai"):
		return "OpenAI"
	case strings.Contains(h, "azure") || strings.Contains(h, ".services.ai."):
		return "Azure"
	default:
		return host
	}
}

// LoadConnections reads the chat-model configuration into an ordered list of
// connections. AI is enabled by AI_TOKEN (a secret): with it unset AI is off and the
// caller uses the deterministic stub ranker.
//
// Configuration:
//   - AI_TOKEN is the shared bearer token, fanned out to every connection (endpoints
//     conventionally serve many models behind one credential).
//   - AI_CONNECTIONS is a JSON array of {endpoint, models} objects in priority order —
//     element 0 is the default connection, and each connection's models[0] is its
//     default model. The connections carry no token (that is AI_TOKEN, shared).
//
// AI_CONNECTIONS (with at least one valid {endpoint, models}) is required whenever
// AI_TOKEN is set, so a half-configured deployment fails fast.
func LoadConnections(getenv func(string) string) ([]AIConnection, error) {
	token := strings.TrimSpace(getenv("AI_TOKEN"))
	if token == "" {
		return nil, nil // AI disabled → stub ranker
	}
	raw := strings.TrimSpace(getenv("AI_CONNECTIONS"))
	if raw == "" {
		return nil, fmt.Errorf("AI_TOKEN is set but AI_CONNECTIONS is empty (provide a JSON array of {endpoint, models}, or unset AI_TOKEN to disable the chat model)")
	}
	var conns []AIConnection
	if err := json.Unmarshal([]byte(raw), &conns); err != nil {
		return nil, fmt.Errorf("AI_CONNECTIONS is not valid JSON (want an array of {endpoint, models}): %w", err)
	}
	if len(conns) == 0 {
		return nil, fmt.Errorf("AI_CONNECTIONS has no connections (provide at least one {endpoint, models})")
	}
	for i := range conns {
		conns[i].Endpoint = strings.TrimSpace(conns[i].Endpoint)
		conns[i].Models = cleanModels(conns[i].Models)
		if conns[i].Endpoint == "" {
			return nil, fmt.Errorf("AI_CONNECTIONS[%d]: endpoint is required", i)
		}
		if len(conns[i].Models) == 0 {
			return nil, fmt.Errorf("AI_CONNECTIONS[%d] (%s): at least one model is required", i, conns[i].Endpoint)
		}
		conns[i].Token = token                  // fan out the shared token across every connection
		conns[i].ID = connID(conns[i].Endpoint) // stable, non-positional id for UI selection
	}
	return conns, nil
}

// cleanModels trims each model id, drops empties, and removes later duplicates while
// preserving order.
func cleanModels(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// connID derives a stable, non-positional id for a connection from its endpoint, so a
// UI selection survives reordering of AI_CONNECTIONS. It is short (12 hex chars) and
// collision-safe across the handful of endpoints a deployment configures.
func connID(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:6])
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

	conns, err := LoadConnections(os.Getenv)
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
		SinkURL:             getEnv("GOFER_SINK_URL", "http://gofer.gofer.svc.cluster.local:8080"),
		Audience:            getEnv("AGENT_AUDIENCE", "gofer-agent"),
		BotReviewers:        botReviewers,
		ConversationEnabled: len(conns) > 0,
		// The ordered list of chat-model connections from AI_CONNECTIONS, each sharing
		// AI_TOKEN; nil when AI is disabled. Callers read the default via DefaultConnection.
		AIConnections: conns,
		MintPublicKey: mintPub,
	}, nil
}

// loadVerifyKey resolves the Ed25519 public key the web tier uses to verify run
// credentials on the agent plane. gofer's web tier is verify-only: a runtime's
// control plane holds the private key and is the sole minter, so MINT_PUBLIC_KEY is
// required and there is deliberately no private-key or derived-key path here — a web
// tier that could derive the private key would not be verify-only.
func loadVerifyKey() (ed25519.PublicKey, error) {
	s := os.Getenv("MINT_PUBLIC_KEY")
	if s == "" {
		return nil, fmt.Errorf("MINT_PUBLIC_KEY must be set: the web tier verifies a runtime's run credentials with this public key (base64-encoded %d-byte Ed25519 public key)", ed25519.PublicKeySize)
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
