// Command feedback is the NovaGrade feedback-stage worker.
//
// It consumes submission envelopes from "feedback.q", loads the graded paper
// produced by the grade stage from {tenant}/{submission}/graded.v1.json,
// calls pipeline.DraftFeedback to populate per-question Feedback fields via
// the configured AI model, persists the result back to the SAME key
// ({tenant}/{submission}/graded.v1.json — in-place update), and publishes a
// feedback.result event to "results.q".
//
// Persistence strategy: the graded artifact is updated in-place so that
// downstream stages (teacher_review, export) see feedback alongside marks
// without any path changes.
//
// Configuration is entirely via environment variables — no secrets are hardcoded:
//
//	AMQP_URL              AMQP broker URL (default: amqp://guest:guest@localhost:5672/)
//	MINIO_ENDPOINT        host:port of the MinIO/S3 endpoint (required)
//	MINIO_ACCESS_KEY      access key ID (required)
//	MINIO_SECRET_KEY      secret access key (required)
//	MINIO_USE_SSL         "true" to connect over TLS (default: false)
//	MINIO_BUCKET          bucket name (default: submissions)
//	AI_GATEWAY_URL        base URL of the ai-gateway / vLLM endpoint (required)
//	AI_GATEWAY_KEY        bearer token for the ai-gateway (optional)
//	FEEDBACK_MODEL        model name used for feedback calls (required)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/azrtydxb/novagrade/internal/pipeline"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

const gradedVersion = 1

// feedbackResultSummary is the inline payload published on the feedback.result event.
type feedbackResultSummary struct {
	QuestionCount    int    `json:"question_count"`
	FeedbackCount    int    `json:"feedback_count"` // questions that received feedback
	GradedKey        string `json:"graded_key"`
}

func main() {
	amqpURL := envOrDefault("AMQP_URL", "amqp://guest:guest@localhost:5672/")
	minioEndpoint := mustEnv("MINIO_ENDPOINT")
	minioAccessKey := mustEnv("MINIO_ACCESS_KEY")
	minioSecretKey := mustEnv("MINIO_SECRET_KEY")
	minioUseSSL := envBool("MINIO_USE_SSL")
	bucket := envOrDefault("MINIO_BUCKET", "submissions")
	aiBaseURL := mustEnv("AI_GATEWAY_URL")
	aiKey := os.Getenv("AI_GATEWAY_KEY")
	feedbackModel := mustEnv("FEEDBACK_MODEL")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bus, err := queue.Connect(amqpURL)
	if err != nil {
		log.Fatalf("feedback: connect to AMQP %s: %v", amqpURL, err)
	}
	defer func() { _ = bus.Close() }()

	if err := bus.DeclareTopology(); err != nil {
		log.Fatalf("feedback: declare topology: %v", err)
	}

	obj, err := store.New(store.Config{
		Endpoint:  minioEndpoint,
		AccessKey: minioAccessKey,
		SecretKey: minioSecretKey,
		UseSSL:    minioUseSSL,
	})
	if err != nil {
		log.Fatalf("feedback: connect to object store: %v", err)
	}
	if err := obj.EnsureBucket(ctx, bucket); err != nil {
		log.Fatalf("feedback: ensure bucket %q: %v", bucket, err)
	}

	prov := providers.NewVLLMProvider(providers.VLLMConfig{
		BaseURL: aiBaseURL,
		APIKey:  aiKey,
	})

	log.Printf("feedback: listening on feedback.q (bucket=%s, model=%s)", bucket, feedbackModel)

	handler := func(env contracts.Envelope) error {
		return handleEnvelope(ctx, env, obj, bus, prov, bucket, feedbackModel)
	}

	if err := bus.Consume(ctx, "feedback.q", handler); err != nil {
		log.Fatalf("feedback: start consumer: %v", err)
	}

	<-ctx.Done()
	log.Println("feedback: shutting down")
}

// handleEnvelope processes a single feedback command.
//
// Object-key conventions:
//
//	Graded (in):  {tenant}/{submission}/graded.v1.json
//	Graded (out): {tenant}/{submission}/graded.v1.json  (updated in-place)
//
// On success it publishes a feedback.result Envelope to "results.q".
func handleEnvelope(
	ctx context.Context,
	env contracts.Envelope,
	obj *store.ObjStore,
	bus *queue.Bus,
	prov providers.AIProvider,
	bucket, feedbackModel string,
) error {
	log.Printf("feedback: processing submission %s/%s (attempt %d)", env.TenantID, env.SubmissionID, env.Attempt)

	// 1. Load graded paper.
	gradedKey := fmt.Sprintf("%s/%s/graded.v%d.json", env.TenantID, env.SubmissionID, gradedVersion)
	gradedData, err := obj.Get(ctx, bucket, gradedKey)
	if err != nil {
		return fmt.Errorf("feedback: get graded paper %q: %w", gradedKey, err)
	}
	var paper contracts.GradedPaper
	if err := json.Unmarshal(gradedData, &paper); err != nil {
		return fmt.Errorf("feedback: parse graded paper %q: %w", gradedKey, err)
	}

	// 2. Draft feedback (marks are never changed by DraftFeedback).
	withFeedback, err := pipeline.DraftFeedback(ctx, prov, feedbackModel, paper)
	if err != nil {
		return fmt.Errorf("feedback: draft feedback for %s/%s: %w", env.TenantID, env.SubmissionID, err)
	}

	// Count how many questions received feedback.
	feedbackCount := 0
	for _, q := range withFeedback.Questions {
		if q.Feedback != "" {
			feedbackCount++
		}
	}
	log.Printf("feedback: %s/%s → %d/%d questions got feedback",
		env.TenantID, env.SubmissionID, feedbackCount, len(withFeedback.Questions))

	// 3. Persist graded paper in-place (same key, now with Feedback fields).
	updatedJSON, err := json.Marshal(withFeedback)
	if err != nil {
		return fmt.Errorf("feedback: marshal updated graded paper: %w", err)
	}
	if err := obj.Put(ctx, bucket, gradedKey, updatedJSON, "application/json"); err != nil {
		return fmt.Errorf("feedback: upload updated graded paper: %w", err)
	}

	// 4. Publish the feedback.result summary.
	summary := feedbackResultSummary{
		QuestionCount: len(withFeedback.Questions),
		FeedbackCount: feedbackCount,
		GradedKey:     gradedKey,
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("feedback: marshal summary: %w", err)
	}
	summaryKey := fmt.Sprintf("%s/%s/feedback-result.json", env.TenantID, env.SubmissionID)
	if err := obj.Put(ctx, bucket, summaryKey, summaryJSON, "application/json"); err != nil {
		return fmt.Errorf("feedback: upload summary: %w", err)
	}

	result := contracts.Envelope{
		TenantID:      env.TenantID,
		Principal:     env.Principal,
		SubmissionID:  env.SubmissionID,
		BatchID:       env.BatchID,
		Stage:         contracts.StageFeedbackResult,
		Attempt:       env.Attempt,
		CorrelationID: env.CorrelationID,
		PayloadRef:    summaryKey,
	}
	if err := bus.Publish(ctx, "results.q", result); err != nil {
		return fmt.Errorf("feedback: publish result: %w", err)
	}

	log.Printf("feedback: done %s/%s, feedback=%d/%d",
		env.TenantID, env.SubmissionID, feedbackCount, len(withFeedback.Questions))
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
		log.Fatalf("feedback: required environment variable %s is not set", name)
	}
	return v
}

// envBool returns true if the named environment variable parses as a true bool.
func envBool(name string) bool {
	b, _ := strconv.ParseBool(os.Getenv(name))
	return b
}
