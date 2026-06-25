// Command transcribe is the NovaGrade transcribe-stage worker.
//
// It consumes submission envelopes from "transcribe.q", downloads the page PNGs
// produced by the render stage from {tenant}/{submission}/pages/{n}.png, runs
// the hybrid OCR + reason + VLM pipeline (pipeline.Transcribe) against the
// configured AIProvider, persists the resulting TranscribedPaper as
// {tenant}/{submission}/transcript.v1.json, and publishes a transcribe.result
// event to "results.q" carrying quality flags.
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
//	AI_GATEWAY_URL     base URL of the ai-gateway / vLLM endpoint (required)
//	AI_GATEWAY_KEY     bearer token for the ai-gateway (optional)
//	TRANSCRIBE_SUBJECT default subject label when the envelope carries none
//	LOW_READ_CONF      read_confidence below which a question is flagged (default 0.5)
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

// transcriptVersion is the schema version embedded in the persisted transcript
// object key ({tenant}/{submission}/transcript.v{N}.json).
const transcriptVersion = 1

// renderResult mirrors the sidecar written by the render stage so the worker
// knows how many page PNGs to download.
type renderResult struct {
	PageCount int `json:"page_count"`
}

// transcribeFlags is the quality-flag payload published on the transcribe.result
// event. It lets downstream stages and operators decide whether the transcript
// needs review without re-reading the full transcript object.
type transcribeFlags struct {
	QuestionCount       int      `json:"question_count"`
	LowReadConfidence   int      `json:"low_read_confidence"`   // questions with read_confidence < threshold
	BlankAnswers        int      `json:"blank_answers"`         // questions with an empty student_answer
	DetectedTotal       float64  `json:"detected_total"`        // sum of max_marks across questions
	ExpectedTotal       *float64 `json:"expected_total"`        // from the paper's printed mark map (nil if none)
	ChecksumOK          bool     `json:"checksum_ok"`           // detected == expected (true when no expected total)
	ChecksumDifference  *float64 `json:"checksum_difference"`   // detected - expected (nil when no expected total)
	TranscriptObjectKey string   `json:"transcript_object_key"` // where the full transcript was persisted
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
	defaultSubject := envOrDefault("TRANSCRIBE_SUBJECT", "")
	lowReadConf := envFloat("LOW_READ_CONF", 0.5)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bus, err := queue.Connect(amqpURL)
	if err != nil {
		log.Fatalf("transcribe: connect to AMQP %s: %v", amqpURL, err)
	}
	defer func() { _ = bus.Close() }()

	if err := bus.DeclareTopology(); err != nil {
		log.Fatalf("transcribe: declare topology: %v", err)
	}

	obj, err := store.New(store.Config{
		Endpoint:  minioEndpoint,
		AccessKey: minioAccessKey,
		SecretKey: minioSecretKey,
		UseSSL:    minioUseSSL,
	})
	if err != nil {
		log.Fatalf("transcribe: connect to object store: %v", err)
	}
	if err := obj.EnsureBucket(ctx, bucket); err != nil {
		log.Fatalf("transcribe: ensure bucket %q: %v", bucket, err)
	}

	prov := providers.NewVLLMProvider(providers.VLLMConfig{
		BaseURL: aiBaseURL,
		APIKey:  aiKey,
	})

	log.Printf("transcribe: listening on transcribe.q (bucket=%s)", bucket)

	handler := func(env contracts.Envelope) error {
		return handleEnvelope(ctx, env, obj, bus, prov, bucket, defaultSubject, lowReadConf)
	}

	if err := bus.Consume(ctx, "transcribe.q", handler); err != nil {
		log.Fatalf("transcribe: start consumer: %v", err)
	}

	<-ctx.Done()
	log.Println("transcribe: shutting down")
}

// handleEnvelope processes a single transcribe command.
//
// Object-key conventions:
//
//	Render sidecar: env.PayloadRef (e.g. "{tenant}/{submission}/render-result.json")
//	Page PNGs:      {tenant}/{submission}/pages/{n}.png  (1-indexed)
//	Transcript:     {tenant}/{submission}/transcript.v{N}.json
//
// On success it publishes a transcribe.result Envelope to "results.q" whose
// PayloadRef points to the persisted transcript object.
func handleEnvelope(
	ctx context.Context,
	env contracts.Envelope,
	obj *store.ObjStore,
	bus *queue.Bus,
	prov providers.AIProvider,
	bucket, defaultSubject string,
	lowReadConf float64,
) error {
	log.Printf("transcribe: processing submission %s/%s (attempt %d)", env.TenantID, env.SubmissionID, env.Attempt)

	// 1. Read the render sidecar to learn the page count.
	pageCount, err := readPageCount(ctx, obj, bucket, env)
	if err != nil {
		return err
	}
	if pageCount <= 0 {
		return fmt.Errorf("transcribe: %s/%s has no pages to transcribe", env.TenantID, env.SubmissionID)
	}

	// 2. Download each page PNG (1-indexed).
	pages := make([][]byte, 0, pageCount)
	for n := 1; n <= pageCount; n++ {
		key := fmt.Sprintf("%s/%s/pages/%d.png", env.TenantID, env.SubmissionID, n)
		data, err := obj.Get(ctx, bucket, key)
		if err != nil {
			return fmt.Errorf("transcribe: get page %s: %w", key, err)
		}
		pages = append(pages, data)
	}

	// 3. Resolve the subject: prefer a future envelope-carried subject, fall
	//    back to the configured default.
	subject := defaultSubject

	// 4. Run the hybrid pipeline. Per-item isolation means this returns a
	//    best-effort paper and a nil error in normal operation.
	paper, err := pipeline.Transcribe(ctx, prov, pages, subject)
	if err != nil {
		return fmt.Errorf("transcribe: pipeline for %s/%s: %w", env.TenantID, env.SubmissionID, err)
	}
	// Record provenance: the render sidecar this transcript derives from.
	paper.SourcePDF = env.PayloadRef
	log.Printf("transcribe: %s/%s → %d questions", env.TenantID, env.SubmissionID, len(paper.Questions))

	// 5. Persist transcript.v{N}.json.
	transcriptKey := fmt.Sprintf("%s/%s/transcript.v%d.json", env.TenantID, env.SubmissionID, transcriptVersion)
	transcriptJSON, err := json.Marshal(paper)
	if err != nil {
		return fmt.Errorf("transcribe: marshal transcript: %w", err)
	}
	if err := obj.Put(ctx, bucket, transcriptKey, transcriptJSON, "application/json"); err != nil {
		return fmt.Errorf("transcribe: upload transcript: %w", err)
	}

	// 6. Compute quality flags and publish the transcribe.result event with the
	//    flags inline as its JSON payload.
	flags := computeFlags(paper, lowReadConf, transcriptKey)
	flagsJSON, err := json.Marshal(flags)
	if err != nil {
		return fmt.Errorf("transcribe: marshal flags: %w", err)
	}
	flagsKey := fmt.Sprintf("%s/%s/transcribe-result.json", env.TenantID, env.SubmissionID)
	if err := obj.Put(ctx, bucket, flagsKey, flagsJSON, "application/json"); err != nil {
		return fmt.Errorf("transcribe: upload flags: %w", err)
	}

	result := contracts.Envelope{
		TenantID:      env.TenantID,
		Principal:     env.Principal,
		SubmissionID:  env.SubmissionID,
		BatchID:       env.BatchID,
		Stage:         contracts.StageTranscribeResult,
		Attempt:       env.Attempt,
		CorrelationID: env.CorrelationID,
		PayloadRef:    flagsKey,
	}
	if err := bus.Publish(ctx, "results.q", result); err != nil {
		return fmt.Errorf("transcribe: publish result: %w", err)
	}

	log.Printf("transcribe: done %s/%s, questions=%d low_read_conf=%d blank=%d checksum_ok=%t",
		env.TenantID, env.SubmissionID, flags.QuestionCount, flags.LowReadConfidence, flags.BlankAnswers, flags.ChecksumOK)
	return nil
}

// readPageCount fetches the render sidecar referenced by env.PayloadRef and
// returns its page count.
func readPageCount(ctx context.Context, obj *store.ObjStore, bucket string, env contracts.Envelope) (int, error) {
	data, err := obj.Get(ctx, bucket, env.PayloadRef)
	if err != nil {
		return 0, fmt.Errorf("transcribe: get render sidecar %q: %w", env.PayloadRef, err)
	}
	var rr renderResult
	if err := json.Unmarshal(data, &rr); err != nil {
		return 0, fmt.Errorf("transcribe: parse render sidecar %q: %w", env.PayloadRef, err)
	}
	return rr.PageCount, nil
}

// computeFlags derives the transcribe.result quality flags from the paper. The
// checksum compares the detected mark total (sum of max_marks) against the
// paper's printed expected total: when they disagree the transcript likely
// missed or mis-marked questions.
func computeFlags(paper contracts.TranscribedPaper, lowReadConf float64, transcriptKey string) transcribeFlags {
	var detected float64
	lowReads := 0
	blanks := 0
	for _, q := range paper.Questions {
		detected += q.MaxMarks
		if q.ReadConfidence < lowReadConf {
			lowReads++
		}
		if q.StudentAnswer == "" {
			blanks++
		}
	}

	flags := transcribeFlags{
		QuestionCount:       len(paper.Questions),
		LowReadConfidence:   lowReads,
		BlankAnswers:        blanks,
		DetectedTotal:       detected,
		ExpectedTotal:       paper.ExpectedTotal,
		ChecksumOK:          true,
		TranscriptObjectKey: transcriptKey,
	}
	if paper.ExpectedTotal != nil {
		diff := detected - *paper.ExpectedTotal
		flags.ChecksumDifference = &diff
		flags.ChecksumOK = diff == 0
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
		log.Fatalf("transcribe: required environment variable %s is not set", name)
	}
	return v
}

// envBool returns true if the named environment variable parses as a true bool.
func envBool(name string) bool {
	b, _ := strconv.ParseBool(os.Getenv(name))
	return b
}

// envFloat returns the named environment variable parsed as a float, or def if
// unset or unparseable.
func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
