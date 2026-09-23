package httpapi

import (
	"net/http"

	"github.com/racetify/racetify-api/internal/httpapi/handlers"
	"github.com/racetify/racetify-api/internal/httpapi/routing"
)

// registerHealthRoutes wires the orchestrator liveness/readiness probes -
// unauthenticated by nature.
func registerHealthRoutes(mux *http.ServeMux, h *handlers.HealthHandler) {
	mux.Handle("GET /healthz", routing.Chain(h.Live))
	mux.Handle("GET /readyz", routing.Chain(h.Ready))
}
