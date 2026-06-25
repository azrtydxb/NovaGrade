// Command grade is the NovaGrade grade-stage worker.
//
// It consumes submission envelopes from "grade.q", loads the transcript
// produced by the transcribe stage from {tenant}/{submission}/transcript.v1.json,
// optionally loads a marking guide from {tenant}/{submission}/guide.v1.json
// (falling back to LLMJudge when the guide is absent), grades the paper using
// the configured MarkScheme, persists the result as
// {tenant}/{submission}/graded.v1.json, and publishes a grade.result event to
// "results.q" carrying a compact summary.
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
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/azrtydxb/novagrade/internal/pipeline/grade"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

const (
	transcriptVersion = 1
	gradedVersion     = 1
	guideVersion      = 1
)

// gradeResultSummary is the inline payload published on the grade.result event.
// It lets downstream stages and operators assess paper quality without loading
// the full graded JSON.
type gradeResultSummary struct {
	QuestionCount int      `json:"question_count"`
	TotalMarks    float64  `json:"total_marks"`
	MaxMarks      float64  `json:"max_marks"`
	Score100      float64  `json:"score_100"`
	Flags         []string `json:"flags"` // unique flags across all questions
	GradedKey     string   `json:"graded_key"`
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

	prov := providers.NewVLLMProvider(providers.VLLMConfig{
		BaseURL: aiBaseURL,
		APIKey:  aiKey,
	})

	log.Printf("grade: listening on grade.q (bucket=%s, model=%s)", bucket, gradeModel)

	handler := func(env contracts.Envelope) error {
		return handleEnvelope(ctx, env, obj, bus, prov, bucket, gradeModel)
	}

	if err := bus.Consume(ctx, "grade.q", handler); err != nil {
		log.Fatalf("grade: start consumer: %v", err)
	}

	<-ctx.Done()
	log.Println("grade: shutting down")
}

// handleEnvelope processes a single grade command.
//
// Object-key conventions:
//
//	Transcript:    {tenant}/{submission}/transcript.v1.json
//	Guide (opt.):  {tenant}/{submission}/guide.v1.json
//	Graded:        {tenant}/{submission}/graded.v1.json
//
// On success it publishes a grade.result Envelope to "results.q" whose
// PayloadRef points to the persisted graded JSON object.
func handleEnvelope(
	ctx context.Context,
	env contracts.Envelope,
	obj *store.ObjStore,
	bus *queue.Bus,
	prov providers.AIProvider,
	bucket, gradeModel string,
) error {
	log.Printf("grade: processing submission %s/%s (attempt %d)", env.TenantID, env.SubmissionID, env.Attempt)

	// 1. Load transcript.
	transcriptKey := fmt.Sprintf("%s/%s/transcript.v%d.json", env.TenantID, env.SubmissionID, transcriptVersion)
	transcriptData, err := obj.Get(ctx, bucket, transcriptKey)
	if err != nil {
		return fmt.Errorf("grade: get transcript %q: %w", transcriptKey, err)
	}
	var paper contracts.TranscribedPaper
	if err := json.Unmarshal(transcriptData, &paper); err != nil {
		return fmt.Errorf("grade: parse transcript %q: %w", transcriptKey, err)
	}

	// 2. Build mark scheme: try to load guide; fall back to LLMJudge.
	llmJudge := grade.NewLLMJudge(prov, gradeModel)
	var scheme grade.MarkScheme = llmJudge

	guideKey := fmt.Sprintf("%s/%s/guide.v%d.json", env.TenantID, env.SubmissionID, guideVersion)
	guideData, err := obj.Get(ctx, bucket, guideKey)
	if err == nil {
		// Guide found: parse and use GuideMarkScheme.
		g, parseErr := grade.LoadGuideFromJSON(guideData)
		if parseErr != nil {
			log.Printf("grade: warning: could not parse guide %q (%v); falling back to LLMJudge", guideKey, parseErr)
		} else {
			scheme = grade.NewGuideMarkScheme(g, llmJudge, prov, gradeModel)
			log.Printf("grade: loaded guide with %d entries", len(g))
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		// An actual storage error (not a missing object) is fatal.
		return fmt.Errorf("grade: get guide %q: %w", guideKey, err)
	} else {
		log.Printf("grade: no guide found at %q; using LLMJudge", guideKey)
	}

	// 3. Grade the paper.
	gradedPaper, err := grade.GradePaper(ctx, scheme, paper)
	if err != nil {
		return fmt.Errorf("grade: grade paper for %s/%s: %w", env.TenantID, env.SubmissionID, err)
	}
	log.Printf("grade: %s/%s → %d questions, score=%.1f%%",
		env.TenantID, env.SubmissionID, len(gradedPaper.Questions), gradedPaper.Score100)

	// 4. Persist graded.v{N}.json.
	gradedKey := fmt.Sprintf("%s/%s/graded.v%d.json", env.TenantID, env.SubmissionID, gradedVersion)
	gradedJSON, err := json.Marshal(gradedPaper)
	if err != nil {
		return fmt.Errorf("grade: marshal graded paper: %w", err)
	}
	if err := obj.Put(ctx, bucket, gradedKey, gradedJSON, "application/json"); err != nil {
		return fmt.Errorf("grade: upload graded paper: %w", err)
	}

	// 5. Collect unique flags across all questions for the summary.
	flags := collectUniqueFlags(gradedPaper)

	// 6. Publish the grade.result sidecar and envelope.
	summary := gradeResultSummary{
		QuestionCount: len(gradedPaper.Questions),
		TotalMarks:    gradedPaper.Total,
		MaxMarks:      gradedPaper.MaxTotal,
		Score100:      gradedPaper.Score100,
		Flags:         flags,
		GradedKey:     gradedKey,
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("grade: marshal summary: %w", err)
	}
	summaryKey := fmt.Sprintf("%s/%s/grade-result.json", env.TenantID, env.SubmissionID)
	if err := obj.Put(ctx, bucket, summaryKey, summaryJSON, "application/json"); err != nil {
		return fmt.Errorf("grade: upload summary: %w", err)
	}

	result := contracts.Envelope{
		TenantID:      env.TenantID,
		Principal:     env.Principal,
		SubmissionID:  env.SubmissionID,
		BatchID:       env.BatchID,
		Stage:         contracts.StageGradeResult,
		Attempt:       env.Attempt,
		CorrelationID: env.CorrelationID,
		PayloadRef:    summaryKey,
	}
	if err := bus.Publish(ctx, "results.q", result); err != nil {
		return fmt.Errorf("grade: publish result: %w", err)
	}

	log.Printf("grade: done %s/%s, questions=%d total=%.1f max=%.1f score=%.1f%%",
		env.TenantID, env.SubmissionID,
		summary.QuestionCount, summary.TotalMarks, summary.MaxMarks, summary.Score100)
	return nil
}

// collectUniqueFlags gathers all unique flag strings across all graded questions
// in document order, deduplicating while preserving first-seen order.
func collectUniqueFlags(paper contracts.GradedPaper) []string {
	seen := map[string]bool{}
	var flags []string
	for _, q := range paper.Questions {
		for _, f := range q.Flags {
			if !seen[f] {
				seen[f] = true
				flags = append(flags, f)
			}
		}
	}
	if flags == nil {
		flags = []string{}
	}
	return flags
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
