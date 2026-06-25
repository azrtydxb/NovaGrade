// Command audit is the NovaGrade audit-trail HTTP service.
//
// It exposes a single read endpoint:
//
//	GET /v1/audit?submission={uuid}
//
// The endpoint is RBAC-enforced (requires ActionViewResults) and tenant-isolated
// (only events belonging to the caller's tenant are returned). Authentication is
// via JWT (Authorization: Bearer <token>) or API key (X-API-Key: <key>),
// exactly as in the submission API service.
//
// # What this service does
//
//   - Serves the audit trail for a given submission ID.
//   - The write path (Record) is NOT exposed over HTTP — it is called directly
//     by pipeline stages (orchestrator, override handlers) that import the
//     domain/audit package. This keeps the append-only guarantee at the
//     network boundary: no external caller can write audit events.
//   - Returns events oldest-first (chronological order, matching the audit
//     trail narrative).
//
// # Configuration (environment variables — no secrets hardcoded)
//
//	HTTP_ADDR          bind address (default: :8082)
//	JWT_SIGNING_KEY    HMAC-SHA256 signing key for JWT tokens (required)
//	DEPLOY_MODE        "saas" or "onprem" (default: "onprem")
//	DB_HOST            Postgres host (default: localhost)
//	DB_PORT            Postgres port (default: 5432)
//	DB_USER            Postgres user (default: novagrade)
//	DB_PASSWORD        Postgres password (default: novagrade)
//	DB_NAME            Postgres database (default: novagrade)
//	DB_SSL_MODE        Postgres SSL mode (default: disable)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
)

func main() {
	ctx := context.Background()

	// ── Validate required env ─────────────────────────────────────────────────
	if os.Getenv("JWT_SIGNING_KEY") == "" {
		log.Fatal("audit: JWT_SIGNING_KEY must be set")
	}

	// ── Database ──────────────────────────────────────────────────────────────
	dbPort, _ := strconv.Atoi(getenv("DB_PORT", "5432"))
	st, err := store.NewStore(ctx, store.DBConfig{
		Host:     getenv("DB_HOST", "localhost"),
		Port:     dbPort,
		User:     getenv("DB_USER", "novagrade"),
		Password: getenv("DB_PASSWORD", "novagrade"),
		Database: getenv("DB_NAME", "novagrade"),
		SSLMode:  getenv("DB_SSL_MODE", "disable"),
	})
	if err != nil {
		log.Fatalf("audit: connect to postgres: %v", err)
	}
	defer st.Close()

	// Run migrations so the service is self-bootstrapping in dev/test.
	if err := st.MigrateUp(ctx); err != nil {
		log.Fatalf("audit: migrate: %v", err)
	}

	// ── Domain service ────────────────────────────────────────────────────────
	// The store satisfies domain.AuditStore because it implements both
	// InsertAuditEvent and ListAuditEventsBySubmission.
	auditSvc := domain.NewAuditService(st)

	// ── Auth ──────────────────────────────────────────────────────────────────
	// Static API keys can be registered here for scanner/service accounts.
	// For now we rely on JWT; the resolver is left empty.
	resolver := auth.NewAPIKeyResolver()

	// ── Handler ───────────────────────────────────────────────────────────────
	h := &api.AuditHandlers{
		Audit:      auditSvc,
		DeployMode: getenv("DEPLOY_MODE", "onprem"),
	}

	// ── Router ────────────────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(resolver))
		r.Get("/audit", h.GetAuditEvents)
	})

	addr := getenv("HTTP_ADDR", ":8082")
	log.Printf("audit: listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("audit: %v", err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
