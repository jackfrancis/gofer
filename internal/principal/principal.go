// Package principal defines the authenticated actor abstraction shared across
// authentication mechanisms (interactive sessions and workload tokens).
//
// A Principal is produced by the authentication layer and carried in the request
// context so downstream handlers are agnostic to *how* the caller authenticated —
// only *who* they are and *what scopes* they hold.
package principal

import "context"

// Kind distinguishes the category of actor behind a request.
type Kind string

const (
	// KindUser is an interactive human authenticated via an OAuth session.
	KindUser Kind = "user"
	// KindWorkload is a non-interactive runtime authenticated via a token.
	KindWorkload Kind = "workload"
)

// Scope is a coarse capability a Principal may hold.
type Scope string

const (
	// ScopeAll grants every capability; held by interactive users acting on their
	// own data.
	ScopeAll Scope = "*"
	// ScopeSignalsRead permits reading user-contextualized source signals.
	ScopeSignalsRead Scope = "signals:read"
	// ScopeMetadataWrite permits writing gofer metadata.
	ScopeMetadataWrite Scope = "metadata:write"
)

// Principal is the authenticated actor for a request.
type Principal struct {
	Kind Kind `json:"kind"`
	// Subject is the stable identity of the actor: a user ID for users, or a
	// runtime/workload ID for workloads.
	Subject string `json:"subject"`
	// ActingUserID is the user whose data is in scope for this request. For an
	// interactive user it equals Subject; for a workload it is the user the runtime
	// was authorized to act on behalf of.
	ActingUserID string `json:"acting_user_id"`
	// JobID ties a workload principal to the run it was minted for, so a runtime's
	// completion report can be correlated back to that run. Empty for users.
	JobID string `json:"job_id,omitempty"`
	// Scopes are the capabilities granted to this principal.
	Scopes []Scope `json:"scopes"`
}

// HasScope reports whether the principal holds the given scope (ScopeAll
// satisfies any scope check).
func (p *Principal) HasScope(s Scope) bool {
	for _, have := range p.Scopes {
		if have == ScopeAll || have == s {
			return true
		}
	}
	return false
}

type ctxKey struct{}

// NewContext returns a copy of ctx carrying the principal.
func NewContext(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext extracts the principal from ctx, if present.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok
}
