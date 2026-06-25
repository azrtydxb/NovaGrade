// Command render is the NovaGrade render-stage worker.
//
// It consumes submission envelopes from "render.q", downloads the source PDF
// from the object store, renders each page to a PNG via pdftoppm, drops
// near-blank pages, uploads the content-page PNGs back to the store, writes
// a JSON sidecar with the page count, and publishes a render.result event to
// "results.q".
//
// Configuration is entirely via environment variables — no secrets are
// hardcoded:
//
//	AMQP_URL           AMQP broker URL (default: amqp://guest:guest@localhost:5672/)
//	MINIO_ENDPOINT     host:port of the MinIO/S3 endpoint (required)
//	MINIO_ACCESS_KEY   access key ID (required)
//	MINIO_SECRET_KEY   secret access key (required)
//	MINIO_USE_SSL      "true" to connect over TLS (default: false)
//	MINIO_BUCKET       bucket name (default: submissions)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/azrtydxb/novagrade/internal/pipeline"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// renderResult is written as a JSON sidecar to the object store so downstream
// stages (e.g. the transcribe worker) know how many pages to process.
type renderResult struct {
	PageCount int `json:"page_count"`
}

func main() {
	amqpURL := envOrDefault("AMQP_URL", "amqp://guest:guest@localhost:5672/")
	minioEndpoint := mustEnv("MINIO_ENDPOINT")
	minioAccessKey := mustEnv("MINIO_ACCESS_KEY")
	minioSecretKey := mustEnv("MINIO_SECRET_KEY")
	minioUseSSL := envBool("MINIO_USE_SSL")
	bucket := envOrDefault("MINIO_BUCKET", "submissions")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bus, err := queue.Connect(amqpURL)
	if err != nil {
		log.Fatalf("render: connect to AMQP %s: %v", amqpURL, err)
	}
	defer func() { _ = bus.Close() }()

	if err := bus.DeclareTopology(); err != nil {
		log.Fatalf("render: declare topology: %v", err)
	}

	obj, err := store.New(store.Config{
		Endpoint:  minioEndpoint,
		AccessKey: minioAccessKey,
		SecretKey: minioSecretKey,
		UseSSL:    minioUseSSL,
	})
	if err != nil {
		log.Fatalf("render: connect to object store: %v", err)
	}

	if err := obj.EnsureBucket(ctx, bucket); err != nil {
		log.Fatalf("render: ensure bucket %q: %v", bucket, err)
	}

	log.Printf("render: listening on render.q (bucket=%s)", bucket)

	handler := func(env contracts.Envelope) error {
		return handleEnvelope(ctx, env, obj, bus, bucket)
	}

	if err := bus.Consume(ctx, "render.q", handler); err != nil {
		log.Fatalf("render: start consumer: %v", err)
	}

	// Block until signal.
	<-ctx.Done()
	log.Println("render: shutting down")
}

// handleEnvelope processes a single render command.
//
// Object-key conventions:
//
//	Input PDF:    env.PayloadRef  (e.g. "tenant123/sub456/source.pdf")
//	Output PNGs:  {tenant}/{submission}/pages/{n}.png  (1-indexed)
//	Sidecar JSON: {tenant}/{submission}/render-result.json
//
// On success it publishes a render.result Envelope to "results.q" with
// PayloadRef pointing to the sidecar JSON object.
func handleEnvelope(ctx context.Context, env contracts.Envelope, obj *store.ObjStore, bus *queue.Bus, bucket string) error {
	log.Printf("render: processing submission %s/%s (attempt %d)", env.TenantID, env.SubmissionID, env.Attempt)

	// 1. Download the PDF from the object store.
	//
	// Memory trade-off: ObjStore.Get returns []byte, so the entire PDF is held
	// in memory before being written to disk. This doubles peak memory usage for
	// large PDFs. A future improvement is to change Get to return an io.ReadCloser
	// so we can stream directly to the temp file (TODO: update ObjStore interface).
	pdfData, err := obj.Get(ctx, bucket, env.PayloadRef)
	if err != nil {
		return fmt.Errorf("render: get PDF %q: %w", env.PayloadRef, err)
	}

	// 2. Write to a temporary file (pdftoppm requires a file path).
	tmpDir, err := os.MkdirTemp("", "render-*")
	if err != nil {
		return fmt.Errorf("render: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	pdfFile := filepath.Join(tmpDir, "source.pdf")
	if err := os.WriteFile(pdfFile, pdfData, 0o600); err != nil {
		return fmt.Errorf("render: write temp PDF: %w", err)
	}

	outDir := filepath.Join(tmpDir, "pages")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("render: create pages dir: %w", err)
	}

	// 3. Render content pages (blank pages are dropped by RenderPDF).
	pages, err := pipeline.RenderPDF(ctx, pdfFile, outDir)
	if err != nil {
		return fmt.Errorf("render: RenderPDF: %w", err)
	}
	if len(pages) == 0 {
		return fmt.Errorf("render: submission %s/%s produced zero content pages — PDF may be corrupt or entirely blank", env.TenantID, env.SubmissionID)
	}
	log.Printf("render: %s/%s → %d content pages", env.TenantID, env.SubmissionID, len(pages))

	// 4. Upload each content-page PNG to the object store.
	for i, pngPath := range pages {
		// Memory trade-off: os.ReadFile loads the entire PNG into memory before upload.
		// For large images or high page counts, this increases peak memory usage.
		// A future improvement is to change Put to accept an io.Reader so we can stream
		// directly from disk (TODO: update ObjStore interface, matching PDF download).
		data, err := os.ReadFile(pngPath)
		if err != nil {
			return fmt.Errorf("render: read page PNG %s: %w", pngPath, err)
		}
		// Object key: {tenant}/{submission}/pages/{n}.png  (1-indexed)
		key := fmt.Sprintf("%s/%s/pages/%d.png", env.TenantID, env.SubmissionID, i+1)
		if err := obj.Put(ctx, bucket, key, data, "image/png"); err != nil {
			return fmt.Errorf("render: upload page %d: %w", i+1, err)
		}
	}

	// 5. Write render-result.json sidecar with the page count.
	sidecarKey := fmt.Sprintf("%s/%s/render-result.json", env.TenantID, env.SubmissionID)
	sidecar, err := json.Marshal(renderResult{PageCount: len(pages)})
	if err != nil {
		return fmt.Errorf("render: marshal sidecar: %w", err)
	}
	if err := obj.Put(ctx, bucket, sidecarKey, sidecar, "application/json"); err != nil {
		return fmt.Errorf("render: upload sidecar: %w", err)
	}

	// 6. Publish the render.result event.
	result := contracts.Envelope{
		TenantID:      env.TenantID,
		Principal:     env.Principal,
		SubmissionID:  env.SubmissionID,
		BatchID:       env.BatchID,
		Stage:         contracts.StageRenderResult,
		Attempt:       env.Attempt,
		CorrelationID: env.CorrelationID,
		PayloadRef:    sidecarKey,
	}
	if err := bus.Publish(ctx, "results.q", result); err != nil {
		return fmt.Errorf("render: publish result: %w", err)
	}

	log.Printf("render: done %s/%s, page_count=%d", env.TenantID, env.SubmissionID, len(pages))
	return nil
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
		log.Fatalf("render: required environment variable %s is not set", name)
	}
	return v
}

// envBool returns true if the named environment variable is the string "true"
// (case-insensitive via strconv.ParseBool).
func envBool(name string) bool {
	v := os.Getenv(name)
	b, _ := strconv.ParseBool(v)
	return b
}
