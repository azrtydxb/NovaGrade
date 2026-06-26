// Command api is the HTTP API service for NovaGrade.
// It exposes endpoints for uploading exam PDFs, querying submission status,
// and retrieving graded results. Authentication is via JWT or API key.
//
// # Configuration (environment variables)
//
//	HTTP_ADDR            — bind address for the HTTP server (default :8080)
//	JWT_SIGNING_KEY      — HMAC-SHA256 signing key for JWT tokens (required)
//	DEPLOY_MODE          — "saas" or "onprem" (default "onprem")
//	DB_HOST              — Postgres host
//	DB_PORT              — Postgres port (default 5432)
//	DB_USER              — Postgres user
//	DB_PASSWORD          — Postgres password
//	DB_NAME              — Postgres database name
//	DB_SSL_MODE          — Postgres SSL mode (default "disable")
//	MINIO_ENDPOINT       — MinIO/S3 endpoint (host:port)
//	MINIO_ACCESS_KEY     — MinIO access key
//	MINIO_SECRET_KEY     — MinIO secret key
//	MINIO_BUCKET         — MinIO bucket name (default "submissions")
//	SCANNER_API_KEY      — comma-separated static scanner keys, each
//	                       formatted "key:tenantID:principalID" (optional)
//	RABBITMQ_URL         — RabbitMQ AMQP URL
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/integration"
	integrationcsv "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/azrtydxb/novagrade/internal/integration/oneroster"
	"github.com/azrtydxb/novagrade/internal/integration/webhook"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

func main() {
	ctx := context.Background()

	// ── Database ──────────────────────────────────────────────────────────────
	dbPort, _ := strconv.Atoi(getenv("DB_PORT", "5432"))
	dbCfg := store.DBConfig{
		Host:     getenv("DB_HOST", "localhost"),
		Port:     dbPort,
		User:     getenv("DB_USER", "novagrade"),
		Password: getenv("DB_PASSWORD", "novagrade"),
		Database: getenv("DB_NAME", "novagrade"),
		SSLMode:  getenv("DB_SSL_MODE", "disable"),
	}
	st, err := store.NewStore(ctx, dbCfg)
	if err != nil {
		log.Fatalf("api: connect to postgres: %v", err)
	}
	defer st.Close()

	if err := st.MigrateUp(ctx); err != nil {
		log.Fatalf("api: migrate: %v", err)
	}

	// ── Object Store ──────────────────────────────────────────────────────────
	objCfg := store.Config{
		Endpoint:  getenv("MINIO_ENDPOINT", "localhost:9000"),
		AccessKey: getenv("MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: getenv("MINIO_SECRET_KEY", "minioadmin"),
	}
	objStore, err := store.New(objCfg)
	if err != nil {
		log.Fatalf("api: connect to minio: %v", err)
	}
	bucket := getenv("MINIO_BUCKET", "submissions")
	if err := objStore.EnsureBucket(ctx, bucket); err != nil {
		log.Fatalf("api: ensure bucket: %v", err)
	}

	// ── Message Bus ───────────────────────────────────────────────────────────
	rabbitURL := getenv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	bus, err := queue.Connect(rabbitURL)
	if err != nil {
		log.Fatalf("api: connect to rabbitmq: %v", err)
	}

	// ── Auth ──────────────────────────────────────────────────────────────────
	if os.Getenv("JWT_SIGNING_KEY") == "" {
		log.Fatal("api: JWT_SIGNING_KEY must be set")
	}
	resolver := auth.NewAPIKeyResolver()
	// Register static scanner API keys from SCANNER_API_KEY. The value is a
	// comma-separated list of "key:tenantID:principalID" entries; each is
	// registered as a scanner principal so the middleware's X-API-Key branch
	// resolves it. An empty/unset env is fine (no scanner keys).
	registerScannerKeys(resolver, os.Getenv("SCANNER_API_KEY"))

	// ── Handlers ─────────────────────────────────────────────────────────────
	deployMode := getenv("DEPLOY_MODE", "onprem")
	objAdapter := &objStoreAdapter{store: objStore, bucket: bucket}

	h := &api.Handlers{
		Store:      st,
		Bus:        bus,
		Objects:    objAdapter,
		DeployMode: deployMode,
	}

	rh := &api.ReviewHandlers{
		Store:      st,
		Objects:    objAdapter,
		DeployMode: deployMode,
	}

	// ── Webhook setup ─────────────────────────────────────────────────────────
	// encKey may be nil in dev/test — handlers nil-guard against this.
	encKey, err := secrets.KeyFromEnv("INTEGRATION_ENC_KEY")
	if err != nil {
		log.Printf("api: INTEGRATION_ENC_KEY not set or invalid (%v) — webhook features disabled", err)
		encKey = nil
	}

	webhookSender := webhook.NewSender(10*time.Second, 3)

	wh := &api.WebhookHandlers{
		Store:      st,
		EncKey:     encKey,
		DeployMode: deployMode,
	}

	ah := &api.ApprovalHandlers{
		Store:         st,
		Objects:       objAdapter,
		Bus:           bus,
		DeployMode:    deployMode,
		WebhookSender: webhookSender,
		WebhookStore:  st,
		WebhookKey:    encKey,
	}

	eh := &api.ExportHandlers{
		Store:      st,
		Objects:    objAdapter,
		DeployMode: deployMode,
	}

	gh := &api.GuideHandlers{
		Store:      st,
		DeployMode: deployMode,
	}

	gph := &api.GuidePreviewHandlers{
		DeployMode: deployMode,
	}

	// ── Integration registry (built-in connectors) ────────────────────────────
	reg := integration.NewRegistry()
	integrationcsv.Register(reg)
	oneroster.Register(reg)

	ih := &api.IntegrationHandlers{
		Store:      st,
		Registry:   reg,
		DeployMode: deployMode,
	}

	roh := &api.RosterHandlers{
		Store:      st,
		Registry:   reg,
		DeployMode: deployMode,
	}

	crh := &api.ClassResultsHandlers{
		Store:      st,
		Objects:    objAdapter,
		DeployMode: deployMode,
	}

	anah := &api.AnalyticsHandlers{
		Store:      st,
		Objects:    objAdapter,
		DeployMode: deployMode,
	}

	// ── Router ────────────────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(resolver))
		r.Post("/submissions", h.PostSubmission)
		r.Get("/submissions", h.ListSubmissions)
		r.Get("/submissions/{id}", h.GetSubmission)
		r.Get("/submissions/{id}/result", h.GetResult)
		r.Get("/submissions/{id}/review", rh.GetReview)
		r.Patch("/submissions/{id}/questions/{qno}", rh.PatchQuestion)
		r.Post("/submissions/{id}/approve", ah.Approve)
		r.Post("/submissions/{id}/publish", ah.Publish)
		r.Post("/submissions/{id}/export", ah.Export)
		r.Get("/submissions/{id}/export.csv", eh.ExportCSV)
		// Guide management
		r.Post("/assessment-versions/{avid}/guides", gh.ImportGuide)
		r.Get("/assessment-versions/{avid}/guides", gh.ListGuides)
		r.Get("/assessment-versions/{avid}/guides/latest", gh.GetLatestGuide)
		r.Post("/guides/{id}/lock", gh.LockGuide)
		// Guide preview (stateless, no store, no provider)
		r.Post("/guides/preview", gph.Preview)
		// Integration connection management
		r.Post("/integrations", ih.UpsertIntegration)
		r.Get("/integrations", ih.ListIntegrations)
		r.Delete("/integrations/{id}", ih.DeleteIntegration)
		// Webhook subscription management
		r.Post("/webhooks", wh.Create)
		r.Get("/webhooks", wh.List)
		r.Delete("/webhooks/{id}", wh.Delete)
		// Roster import
		r.Post("/rosters/import", roh.ImportRoster)
		// Class-results CSV export
		r.Get("/assessment-versions/{avid}/results.csv", crh.ClassResultsCSV)
		// Analytics
		r.Get("/assessment-versions/{avid}/analytics", anah.GetAnalytics)
		r.Get("/assessment-versions/{avid}/override-stats", anah.GetOverrideStats)
	})

	addr := getenv("HTTP_ADDR", ":8080")
	log.Printf("api: listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("api: server: %v", err)
	}
}

// registerScannerKeys parses a comma-separated SCANNER_API_KEY value, where each
// entry is "key:tenantID:principalID", and registers each as a scanner principal
// on the resolver. Malformed or empty entries are skipped with a warning so a
// single bad entry does not prevent the service from starting.
func registerScannerKeys(resolver *auth.APIKeyResolver, raw string) {
	if raw == "" {
		return
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			log.Printf("api: ignoring malformed SCANNER_API_KEY entry (want key:tenantID:principalID)")
			continue
		}
		key, tenantID, principalID := parts[0], parts[1], parts[2]
		resolver.Register(key, auth.Principal{
			ID:       principalID,
			TenantID: tenantID,
			Roles:    []domain.Role{domain.RoleScanner},
		})
		log.Printf("api: registered scanner API key for tenant %s principal %s", tenantID, principalID)
	}
}

// getenv returns the environment variable named by key, or fallback if not set.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// objStoreAdapter adapts store.ObjStore to the api.ObjectStore interface.
type objStoreAdapter struct {
	store  *store.ObjStore
	bucket string
}

func (a *objStoreAdapter) PutObject(ctx context.Context, key string, data []byte) error {
	return a.store.Put(ctx, a.bucket, key, data, "application/pdf")
}

func (a *objStoreAdapter) GetObject(ctx context.Context, key string) ([]byte, error) {
	data, err := a.store.Get(ctx, a.bucket, key)
	if err != nil {
		return nil, fmt.Errorf("objstore: get %q: %w", key, err)
	}
	return data, nil
}
