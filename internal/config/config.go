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
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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

	// AllowedGitHubLogins restricts sign-in to these GitHub logins, lowercased.
	// Empty allows any account of any configured provider. Set via
	// ALLOWED_GITHUB_LOGINS (comma-separated, case-insensitive). See SignInAllowed.
	AllowedGitHubLogins []string

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

	// DataDir is the directory holding gofer's SQLite database (DATA_DIR). Empty
	// means no persistent storage: the web tier keeps its state in memory and loses
	// it on restart, which is what `make run` and the tests use. When set,
	// VaultKey is required.
	DataDir string

	// VaultKey is the 32-byte AES-256 key that encrypts delegated provider tokens
	// at rest in the database (VAULT_ENCRYPTION_KEY, base64). It is only loaded,
	// and only required, when DataDir is set.
	VaultKey []byte

	// RefreshInterval is how often the web tier requests a GitHub refresh (a
	// github-ingest run) for every owner with stored work (REFRESH_INTERVAL, a Go
	// duration such as "15m"). With a default chat model, each tick also re-ranks
	// items whose discussion research is stale (ingest.Refresher.ResearchOnce). Zero,
	// the default, turns the schedule off: worklists then refresh only on the Refresh
	// button. Turn it on only against an AEI
	// control plane that contains provider failures (AEI archive-plan item 11): the
	// scheduled runs dispatch with nobody watching.
	RefreshInterval time.Duration
}

// MinRefreshInterval is the shortest REFRESH_INTERVAL gofer accepts. Each refresh is
// a full ingest run (a GitHub walk, then enrich and rank, which calls the model), so
// the interval trades freshness against GitHub and model cost.
const MinRefreshInterval = time.Minute

// parseRefreshInterval reads REFRESH_INTERVAL: empty or "0" is off; otherwise a Go
// duration of at least MinRefreshInterval.
func parseRefreshInterval(getenv func(string) string) (time.Duration, error) {
	s := strings.TrimSpace(getenv("REFRESH_INTERVAL"))
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("REFRESH_INTERVAL must be a duration such as \"15m\" (or empty or 0 to turn the schedule off): %w", err)
	}
	if d == 0 {
		return 0, nil
	}
	if d < MinRefreshInterval {
		return 0, fmt.Errorf("REFRESH_INTERVAL must be at least %s (or empty or 0 to turn the schedule off), got %s", MinRefreshInterval, d)
	}
	return d, nil
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
	// Gateway marks a connection served by gofer's model gateway (AI_GATEWAY_URL):
	// the gateway holds the upstream key, so Token is empty and a runtime presents
	// its run credential instead of vending a model token. Set at load.
	Gateway bool `json:"-"`
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
//
// The model-gateway profile (AI_GATEWAY_URL, docs/adr/0008-model-gateway.md) enables
// the model WITHOUT AI_TOKEN: a gateway holds the upstream key, and every connection
// must be served by it (same scheme and host), because a runtime presents its run
// credential to a gateway connection and that credential must never reach a model
// provider. AI_TOKEN must then be unset — the web tier holds no model key at all.
func LoadConnections(getenv func(string) string) ([]AIConnection, error) {
	token := strings.TrimSpace(getenv("AI_TOKEN"))
	gateway, err := gatewayOrigin(getenv("AI_GATEWAY_URL"))
	if err != nil {
		return nil, err
	}
	if gateway != "" && token != "" {
		return nil, fmt.Errorf("AI_GATEWAY_URL and AI_TOKEN are both set: with the model gateway, the gateway alone holds the upstream key (remove AI_TOKEN from the web tier's environment)")
	}
	if token == "" && gateway == "" {
		return nil, nil // AI disabled → stub ranker
	}
	raw := strings.TrimSpace(getenv("AI_CONNECTIONS"))
	if raw == "" {
		if gateway != "" {
			return nil, fmt.Errorf("AI_GATEWAY_URL is set but AI_CONNECTIONS is empty (provide a JSON array of {endpoint, models} served by the gateway)")
		}
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
		if gateway != "" {
			if o, err := gatewayOrigin(conns[i].Endpoint); err != nil || o != gateway {
				return nil, fmt.Errorf("AI_CONNECTIONS[%d] (%s): with AI_GATEWAY_URL set, every endpoint must be served by the gateway (%s)", i, conns[i].Endpoint, gateway)
			}
			conns[i].Gateway = true
		}
		conns[i].Token = token                  // fan out the shared token (empty behind the gateway)
		conns[i].ID = connID(conns[i].Endpoint) // stable, non-positional id for UI selection
	}
	return conns, nil
}

// gatewayOrigin returns raw's scheme://host, lowercased, or "" when raw is blank. It
// is how a gateway connection is matched to AI_GATEWAY_URL: only the origin counts,
// since the gateway serves each API at its own path.
func gatewayOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("AI_GATEWAY_URL: %q is not an http(s) URL with a host", raw)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
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
	Google    OAuthApp
	GitHub    OAuthApp
	Microsoft OAuthApp
	// GitHubKind says what kind of client GitHub names (GITHUB_CLIENT_KIND):
	// GitHubOAuthApp (the default) or GitHubApp. gofer never guesses it from the
	// client ID. See ADR 0007.
	GitHubKind      string
	MicrosoftTenant string
	// GitHubWrite is the optional, separate classic OAuth app a user connects to let
	// gofer post their own comments (GITHUB_WRITE_CLIENT_ID/SECRET, scope public_repo;
	// ADR 0009). Its token never leaves the web tier and is never vended to a run.
	// Unset: commenting is off.
	GitHubWrite OAuthApp
	// GitHubWriteTokens lets a user connect commenting with a classic personal access
	// token instead of the write client's OAuth grant (GITHUB_WRITE_TOKENS=true; ADR
	// 0011): for organizations that block unapproved OAuth apps. Off by default, and
	// only with the write client configured, which stays the default way to connect.
	GitHubWriteTokens bool
}

// CommentingEnabled reports whether gofer can post comments for users who connect
// the write grant: GitHub sign-in and the separate write client are both configured.
func (p Providers) CommentingEnabled() bool { return p.GitHub.Enabled() && p.GitHubWrite.Enabled() }

// SecureOrLoopback reports whether a browser reaches gofer at baseURL over HTTPS, or
// over plain HTTP only on this machine (localhost, a *.localhost name, 127.0.0.0/8 or
// ::1), so a secret typed into gofer's page never crosses a network in cleartext.
func SecureOrLoopback(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// validateGitHubWrite refuses a write client that would put write scope on the
// sign-in token: a classic OAuth grant accumulates scopes per (user, app), so the
// write grant must come from a different app than sign-in (ADR 0009).
func validateGitHubWrite(p Providers) error {
	w := p.GitHubWrite
	if w.ClientID == "" && w.ClientSecret == "" {
		if p.GitHubWriteTokens {
			return fmt.Errorf("GITHUB_WRITE_TOKENS needs commenting configured (GITHUB_WRITE_CLIENT_ID/SECRET): a token is an alternative to the write client's grant, not a replacement (ADR 0011)")
		}
		return nil
	}
	if !w.Enabled() {
		return fmt.Errorf("GITHUB_WRITE_CLIENT_ID and GITHUB_WRITE_CLIENT_SECRET must be set together")
	}
	if !p.GitHub.Enabled() {
		return fmt.Errorf("GITHUB_WRITE_CLIENT_ID needs GitHub sign-in (GITHUB_CLIENT_ID/SECRET): the write grant is connected by a signed-in GitHub user")
	}
	if w.ClientID == p.GitHub.ClientID {
		return fmt.Errorf("GITHUB_WRITE_CLIENT_ID must name a different OAuth app than GITHUB_CLIENT_ID: the sign-in token is vended to runs and must stay read-only (ADR 0009)")
	}
	return nil
}

// GITHUB_CLIENT_KIND values.
const (
	// GitHubOAuthApp is a classic OAuth app: sign-in asks for read:user user:email,
	// and its tokens never expire.
	GitHubOAuthApp = "oauth-app"
	// GitHubApp is a GitHub App used only for user sign-in: no scopes (the app's
	// permissions decide), expiring user tokens with a rotating refresh token.
	GitHubApp = "github-app"
)

// IsGitHubApp reports whether the GitHub client is a GitHub App.
func (p Providers) IsGitHubApp() bool { return p.GitHubKind == GitHubApp }

// parseGitHubKind validates GITHUB_CLIENT_KIND; empty means GitHubOAuthApp.
func parseGitHubKind(v string) (string, error) {
	switch v = strings.TrimSpace(v); v {
	case "", GitHubOAuthApp:
		return GitHubOAuthApp, nil
	case GitHubApp:
		return GitHubApp, nil
	}
	return "", fmt.Errorf("GITHUB_CLIENT_KIND must be %q or %q, not %q", GitHubOAuthApp, GitHubApp, v)
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

	githubKind, err := parseGitHubKind(os.Getenv("GITHUB_CLIENT_KIND"))
	if err != nil {
		return nil, err
	}

	refreshInterval, err := parseRefreshInterval(os.Getenv)
	if err != nil {
		return nil, err
	}

	dataDir := strings.TrimSpace(os.Getenv("DATA_DIR"))
	vaultKey, err := loadVaultKey(dataDir, os.Getenv)
	if err != nil {
		return nil, err
	}

	providers := Providers{
		Google: OAuthApp{
			ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		},
		GitHub: OAuthApp{
			ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
			ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		},
		GitHubKind: githubKind,
		Microsoft: OAuthApp{
			ClientID:     os.Getenv("MICROSOFT_CLIENT_ID"),
			ClientSecret: os.Getenv("MICROSOFT_CLIENT_SECRET"),
		},
		MicrosoftTenant: getEnv("MICROSOFT_TENANT", "common"),
		GitHubWrite: OAuthApp{
			ClientID:     strings.TrimSpace(os.Getenv("GITHUB_WRITE_CLIENT_ID")),
			ClientSecret: strings.TrimSpace(os.Getenv("GITHUB_WRITE_CLIENT_SECRET")),
		},
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_WRITE_TOKENS")); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			// Never echo the value: an operator who mistook this for a credential
			// setting may have put a token here, and startup errors are logged.
			return nil, fmt.Errorf("GITHUB_WRITE_TOKENS must be true or false")
		}
		providers.GitHubWriteTokens = on
	}
	if err := validateGitHubWrite(providers); err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(getEnv("BASE_URL", "http://localhost:8080"), "/")
	if providers.GitHubWriteTokens && !SecureOrLoopback(baseURL) {
		return nil, fmt.Errorf("GITHUB_WRITE_TOKENS needs an https BASE_URL (or a loopback one for local development): a personal access token pasted over plain HTTP crosses the network in cleartext, and gofer cannot revoke it (ADR 0011)")
	}

	return &Config{
		DataDir:             dataDir,
		VaultKey:            vaultKey,
		Addr:                getEnv("ADDR", ":8080"),
		BaseURL:             baseURL,
		AllowedOrigins:      splitAndTrim(os.Getenv("ALLOWED_ORIGINS")),
		SessionSecret:       []byte(secret),
		CookieSecure:        getEnv("COOKIE_SECURE", "false") == "true",
		Providers:           providers,
		SinkURL:             getEnv("GOFER_SINK_URL", "http://gofer.gofer.svc.cluster.local:8080"),
		Audience:            getEnv("AGENT_AUDIENCE", "gofer-agent"),
		BotReviewers:        botReviewers,
		AllowedGitHubLogins: lowerAll(splitAndTrim(os.Getenv("ALLOWED_GITHUB_LOGINS"))),
		ConversationEnabled: len(conns) > 0,
		// The ordered list of chat-model connections from AI_CONNECTIONS, each sharing
		// AI_TOKEN; nil when AI is disabled. Callers read the default via DefaultConnection.
		AIConnections:   conns,
		MintPublicKey:   mintPub,
		RefreshInterval: refreshInterval,
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

// vaultKeySize is the AES-256 key size VAULT_ENCRYPTION_KEY must decode to.
const vaultKeySize = 32

// loadVaultKey resolves the key that encrypts delegated provider tokens at rest.
// Persistent storage without it fails fast: tokens must never be written to disk
// in the clear, and a key generated on the fly would be lost with the pod. With no
// data directory the vault is in memory and the key is not read at all.
func loadVaultKey(dataDir string, getenv func(string) string) ([]byte, error) {
	if dataDir == "" {
		return nil, nil
	}
	s := strings.TrimSpace(getenv("VAULT_ENCRYPTION_KEY"))
	if s == "" {
		return nil, fmt.Errorf("VAULT_ENCRYPTION_KEY must be set when DATA_DIR is set: delegated provider tokens are encrypted at rest with it (base64-encoded %d random bytes, e.g. `openssl rand -base64 %d`)", vaultKeySize, vaultKeySize)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != vaultKeySize {
		// Never echo the value: it is a secret even when malformed.
		return nil, fmt.Errorf("VAULT_ENCRYPTION_KEY must be base64-encoded %d random bytes (e.g. `openssl rand -base64 %d`)", vaultKeySize, vaultKeySize)
	}
	return raw, nil
}

// SignInAllowed reports whether an account may sign in and hold a session. With no
// allow-list configured, everyone may. With one, only a GitHub account whose login
// is listed may (case-insensitively); other providers have no GitHub login to match,
// so they are refused, as is a GitHub account whose login is empty.
func (c *Config) SignInAllowed(provider, login string) bool {
	if len(c.AllowedGitHubLogins) == 0 {
		return true
	}
	if provider != "github" || login == "" {
		return false
	}
	login = strings.ToLower(login)
	for _, l := range c.AllowedGitHubLogins {
		if l == login {
			return true
		}
	}
	return false
}

// lowerAll lowercases each element in place and returns the slice.
func lowerAll(in []string) []string {
	for i := range in {
		in[i] = strings.ToLower(in[i])
	}
	return in
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
