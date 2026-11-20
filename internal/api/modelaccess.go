package api

import (
	"log/slog"
	"net/http"

	"github.com/jackfrancis/gofer/internal/principal"
)

// ModelAccess handles /agent/model-access, the model gateway's authorization check
// (the AI_GATEWAY_URL profile, docs/adr/0008-model-gateway.md). The gateway holds the
// upstream model key; before it attaches that key to a model call, it forwards the
// caller's Authorization header here (extAuthz). The route is behind the same
// RequireScope as the credential broker, so exactly the runs that could vend the
// model token without the gateway may use the model through it, and a browser session
// never can. It answers 204 to allow; RequireScope has already answered 401/403
// otherwise. It reads no body and returns nothing secret.
func ModelAccess(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slog.DebugContext(r.Context(), "model gateway call authorized", "run", p.JobID, "user", p.ActingUserID)
	w.WriteHeader(http.StatusNoContent)
}
