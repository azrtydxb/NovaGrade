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
//	MINIO_BUCKET         — MinIO bucket name (default "novagrade")
//	RABBITMQ_URL         — RabbitMQ AMQP URL
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/queue"
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
	bucket := getenv("MINIO_BUCKET", "novagrade")
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
	// Phase 1: register any static API keys from env (SCANNER_API_KEY=key:tenantID:principalID)
	// Production deployments inject keys via config; this is a placeholder.
	_ = domain.RoleScanner // ensure domain import used

	// ── Handlers ─────────────────────────────────────────────────────────────
	h := &api.Handlers{
		Store:      st,
		Bus:        bus,
		Objects:    &objStoreAdapter{store: objStore, bucket: bucket},
		DeployMode: getenv("DEPLOY_MODE", "onprem"),
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
	})

	addr := getenv("HTTP_ADDR", ":8080")
	log.Printf("api: listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("api: server: %v", err)
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
	return a.store.Put(ctx, a.bucket, key, data, "application/octet-stream")
}

func (a *objStoreAdapter) GetObject(ctx context.Context, key string) ([]byte, error) {
	data, err := a.store.Get(ctx, a.bucket, key)
	if err != nil {
		return nil, fmt.Errorf("objstore: get %q: %w", key, err)
	}
	return data, nil
}
