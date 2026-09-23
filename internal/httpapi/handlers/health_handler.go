package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/racetify/racetify-api/internal/httpapi/respond"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
)

type HealthHandler struct {
	db    *database.DB
	redis *rediscli.Client
}

func NewHealthHandler(db *database.DB, redis *rediscli.Client) *HealthHandler {
	return &HealthHandler{db: db, redis: redis}
}

// Live handles GET /healthz - process is up, no dependency checks. Used
// by an orchestrator's liveness probe.
func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	respond.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready handles GET /readyz - verifies Postgres and Redis are reachable.
// Used by an orchestrator's readiness probe / load balancer health check.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	status := map[string]string{}
	healthy := true

	if err := h.db.PingContext(ctx); err != nil {
		status["postgres"] = "unreachable"
		healthy = false
	} else {
		status["postgres"] = "ok"
	}

	if err := h.redis.Ping(ctx); err != nil {
		status["redis"] = "unreachable"
		healthy = false
	} else {
		status["redis"] = "ok"
	}

	if !healthy {
		respond.JSON(w, http.StatusServiceUnavailable, status)
		return
	}
	respond.JSON(w, http.StatusOK, status)
}
