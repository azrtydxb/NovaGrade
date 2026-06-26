// Command grade is the NovaGrade grade-stage worker.
//
// It consumes submission envelopes from "grade.q", loads the transcript
// produced by the transcribe stage from {tenant}/{submission}/transcript.v1.json,
// optionally loads a marking guide (priority order: DB guide store →
// obj-store guide.v1.json → LLMJudge fallback), grades the paper using
// the configured MarkScheme, persists the result as
// {tenant}/{submission}/graded.v1.json, and publishes a grade.result event to
// "results.q" carrying a compact summary.
//
// The core logic lives in internal/gradeworker; this binary is a thin wrapper
// responsible only for env/config parsing, infrastructure wiring, and signal handling.
//
// Configuration is entirely via environment variables — no secrets are hardcoded:
//
//	AMQP_URL           AMQP broker URL (default: amqp://guest:guest@localhost:5672/)
//	MINIO_ENDPOINT     host:port of the MinIO/S3 endpoint (required)
//	MINIO_ACCESS_KEY   access key ID (required)
//	MINIO_SECRET_KEY   secret access key (required)
//	MINIO_USE_SSL      "true" to connect over TLS (default: false)
//	MINIO_BUCKET       bucket name (default: submissions)
//	AI_GATEWAY_URL     base URL of the ai-gateway / vLLM endpoint (required)
//	AI_GATEWAY_KEY     bearer token for the ai-gateway (optional)
//	GRADE_MODEL        model name used for LLM grading calls (required)
//	DB_HOST            Postgres host (default: localhost)
//	DB_PORT            Postgres port (default: 5432)
//	DB_USER            Postgres user (default: novagrade)
//	DB_PASSWORD        Postgres password (default: novagrade)
//	DB_NAME            Postgres database name (default: novagrade)
//	DB_SSL_MODE        Postgres SSL mode (default: disable)
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/azrtydxb/novagrade/internal/gradeworker"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

func main() {
	amqpURL := envOrDefault("AMQP_URL", "amqp://guest:guest@localhost:5672/")
	minioEndpoint := mustEnv("MINIO_ENDPOINT")
	minioAccessKey := mustEnv("MINIO_ACCESS_KEY")
	minioSecretKey := mustEnv("MINIO_SECRET_KEY")
	minioUseSSL := envBool("MINIO_USE_SSL")
	bucket := envOrDefault("MINIO_BUCKET", "submissions")
	aiBaseURL := mustEnv("AI_GATEWAY_URL")
	aiKey := os.Getenv("AI_GATEWAY_KEY")
	gradeModel := mustEnv("GRADE_MODEL")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bus, err := queue.Connect(amqpURL)
	if err != nil {
		log.Fatalf("grade: connect to AMQP %s: %v", amqpURL, err)
	}
	defer func() { _ = bus.Close() }()

	if err := bus.DeclareTopology(); err != nil {
		log.Fatalf("grade: declare topology: %v", err)
	}

	obj, err := store.New(store.Config{
		Endpoint:  minioEndpoint,
		AccessKey: minioAccessKey,
		SecretKey: minioSecretKey,
		UseSSL:    minioUseSSL,
	})
	if err != nil {
		log.Fatalf("grade: connect to object store: %v", err)
	}
	if err := obj.EnsureBucket(ctx, bucket); err != nil {
		log.Fatalf("grade: ensure bucket %q: %v", bucket, err)
	}

	// ── Database Store ─────────────────────────────────────────────────────────
	dbPort, _ := strconv.Atoi(envOrDefault("DB_PORT", "5432"))
	dbCfg := store.DBConfig{
		Host:     envOrDefault("DB_HOST", "localhost"),
		Port:     dbPort,
		User:     envOrDefault("DB_USER", "novagrade"),
		Password: envOrDefault("DB_PASSWORD", "novagrade"),
		Database: envOrDefault("DB_NAME", "novagrade"),
		SSLMode:  envOrDefault("DB_SSL_MODE", "disable"),
	}
	var st *store.Store
	st, err = store.NewStore(ctx, dbCfg)
	if err != nil {
		log.Printf("grade: warning: could not connect to postgres (%v); DB guide loading disabled", err)
		st = nil
	} else {
		log.Printf("grade: connected to postgres (host=%s db=%s)", dbCfg.Host, dbCfg.Database)
		defer st.Close()
	}

	prov := providers.NewVLLMProvider(providers.VLLMConfig{
		BaseURL: aiBaseURL,
		APIKey:  aiKey,
	})

	log.Printf("grade: listening on grade.q (bucket=%s, model=%s)", bucket, gradeModel)

	deps := gradeworker.Deps{
		ObjStore:   obj,
		Store:      st,
		Provider:   prov,
		Bus:        bus,
		Bucket:     bucket,
		GradeModel: gradeModel,
	}

	handler := func(env contracts.Envelope) error {
		return gradeworker.HandleEnvelope(ctx, deps, env)
	}

	if err := bus.Consume(ctx, "grade.q", handler); err != nil {
		log.Fatalf("grade: start consumer: %v", err)
	}

	<-ctx.Done()
	log.Println("grade: shutting down")
}

// envOrDefault returns the value of the named environment variable, or def if
// it is unset or empty.
func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// mustEnv returns the value of the named environment variable and calls
// log.Fatalf if it is unset or empty.
func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("grade: required environment variable %s is not set", name)
	}
	return v
}

// envBool returns true if the named environment variable parses as a true bool.
func envBool(name string) bool {
	b, _ := strconv.ParseBool(os.Getenv(name))
	return b
}
