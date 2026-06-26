// Package gradeworker implements the core grade-stage processing logic.
//
// It is extracted from cmd/grade/main.go so that integration tests and other
// callers can exercise the REAL guide-loading and grading code without
// duplicating it or importing package main.
//
// # Guide-load priority order
//
//  1. DB guide store: GetSubmission → AssessmentVersionID → ListGuideVersions →
//     pick locked version (or lock-and-use latest) → grade against that version.
//  2. Object-store guide.v1.json in the submission's prefix.
//  3. LLMJudge fallback: ask the AI provider to grade each question.
//
// # Lock-on-grading semantics
//
// When more than one guide version exists:
//   - If any version is LOCKED, the highest locked version is used. This pins the
//     entire batch to the first-locked guide, regardless of later imports.
//   - If NO version is locked, the latest version is used and immediately locked
//     (lock-on-grading-start). Subsequent submissions in the same batch will then
//     find a locked version and use it.
//
// LockGuide failure is log-only (non-fatal) to avoid blocking grading on a
// transient DB error.
//
// # Object-key conventions
//
//	Transcript:   {tenant}/{submission}/transcript.v1.json
//	Guide (opt.): {tenant}/{submission}/guide.v1.json
//	Graded:       {tenant}/{submission}/graded.v1.json
//	Summary:      {tenant}/{submission}/grade-result.json
package gradeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/pipeline/grade"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

const (
	transcriptVersion = 1
	gradedVersion     = 1
	guideVersion      = 1
)

// Publisher is the minimal messaging port that HandleEnvelope requires.
// It is satisfied by *queue.Bus.
type Publisher interface {
	Publish(ctx context.Context, queue string, env contracts.Envelope) error
}

// Deps bundles the infrastructure dependencies required by HandleEnvelope.
// All fields except Store are required; Store may be nil (DB guide loading is
// skipped and the worker falls back to obj-store guide or LLMJudge).
type Deps struct {
	// ObjStore is the object-store adapter used to fetch transcripts and guides
	// and to persist graded output. Required.
	ObjStore *store.ObjStore

	// Store is the Postgres DB store used to look up the submission's
	// assessment_version_id, fetch the latest guide, and lock the guide after
	// grading starts. May be nil — in that case DB guide loading is skipped.
	Store *store.Store

	// Provider is the AI provider used by LLMJudge (and by GuideMarkScheme for
	// questions not covered by the guide). Required.
	Provider providers.AIProvider

	// Bus is the message publisher used to emit the grade.result envelope after
	// successful grading. Required.
	Bus Publisher

	// Bucket is the object-store bucket that all artifacts are read from and
	// written to. Required.
	Bucket string

	// GradeModel is the model name passed to the LLM judge for questions not
	// covered by a guide. Required.
	GradeModel string
}

// GradeResultSummary is the inline payload published on the grade.result event.
// It lets downstream stages and operators assess paper quality without loading
// the full graded JSON.
type GradeResultSummary struct {
	QuestionCount int      `json:"question_count"`
	TotalMarks    float64  `json:"total_marks"`
	MaxMarks      float64  `json:"max_marks"`
	Score100      float64  `json:"score_100"`
	Flags         []string `json:"flags"` // unique flags across all questions
	GradedKey     string   `json:"graded_key"`
}

// HandleEnvelope processes a single grade command envelope end-to-end:
//
//  1. Loads transcript.v{N}.json from object storage.
//  2. Builds a MarkScheme (DB guide → obj-store guide → LLMJudge).
//  3. Grades the paper via grade.GradePaper.
//  4. Persists graded.v{N}.json and a grade-result.json summary.
//  5. Publishes a StageGradeResult envelope to "results.q".
func HandleEnvelope(ctx context.Context, deps Deps, env contracts.Envelope) error {
	log.Printf("grade: processing submission %s/%s (attempt %d)", env.TenantID, env.SubmissionID, env.Attempt)

	// 1. Load transcript.
	transcriptKey := fmt.Sprintf("%s/%s/transcript.v%d.json", env.TenantID, env.SubmissionID, transcriptVersion)
	transcriptData, err := deps.ObjStore.Get(ctx, deps.Bucket, transcriptKey)
	if err != nil {
		return fmt.Errorf("grade: get transcript %q: %w", transcriptKey, err)
	}
	var paper contracts.TranscribedPaper
	if err := json.Unmarshal(transcriptData, &paper); err != nil {
		return fmt.Errorf("grade: parse transcript %q: %w", transcriptKey, err)
	}

	// 2. Build mark scheme: try DB store guide → obj-store guide → LLMJudge.
	llmJudge := grade.NewLLMJudge(deps.Provider, deps.GradeModel)
	var scheme grade.MarkScheme = llmJudge
	var guideLoaded bool

	// 2a. If the DB store is available and the submission has an
	// assessment_version_id, try the DB guide store.
	if deps.Store != nil {
		submissionUID, parseErr := uuid.Parse(env.SubmissionID)
		if parseErr != nil {
			log.Printf("grade: warning: cannot parse submission_id %q as UUID (%v); skipping DB guide lookup", env.SubmissionID, parseErr)
		} else {
			sub, subErr := deps.Store.GetSubmission(ctx, submissionUID)
			if subErr != nil && !errors.Is(subErr, store.ErrNotFound) {
				return fmt.Errorf("grade: get submission %s: %w", env.SubmissionID, subErr)
			}
			if subErr == nil {
				// Tenant-consistency assert: the submission's tenant must match the envelope tenant.
				// Grade a submission only when the tenant is unambiguous to prevent cross-tenant leakage.
				if sub.TenantID.String() != env.TenantID {
					log.Printf("grade: SECURITY: submission %s tenant %s does not match envelope tenant %s; rejecting",
						env.SubmissionID, sub.TenantID, env.TenantID)
					return fmt.Errorf("grade: tenant mismatch for submission %s: submission tenant %s != envelope tenant %s",
						env.SubmissionID, sub.TenantID, env.TenantID)
				}
			}
			if subErr == nil && sub.AssessmentVersionID != nil {
				avid := *sub.AssessmentVersionID
				tenantUID, tenantParseErr := uuid.Parse(env.TenantID)
				if tenantParseErr != nil {
					log.Printf("grade: warning: cannot parse tenant_id %q as UUID (%v); skipping DB guide lookup", env.TenantID, tenantParseErr)
				} else {
					// Fix 3: lock-on-grading pins the batch to the locked version.
					// Use ListGuideVersions (ordered DESC by version) to pick:
					//   - The highest locked version if any exists.
					//   - The latest version otherwise (and then lock it).
					mg, lockFirst, listErr := selectLockedOrLatestGuide(ctx, deps.Store, tenantUID, avid)
					if listErr == nil {
						g, guideParseErr := grade.LoadGuideFromJSON(mg.Content)
						if guideParseErr != nil {
							log.Printf("grade: warning: could not parse DB guide %s (%v); trying obj-store guide", mg.ID, guideParseErr)
						} else {
							scheme = grade.NewGuideMarkScheme(g, llmJudge, deps.Provider, deps.GradeModel)
							guideLoaded = true
							log.Printf("grade: loaded DB guide %s (v%d, locked=%v, %d entries) for assessment_version %s",
								mg.ID, mg.Version, mg.Locked, len(g), avid)
							// Lock-on-grading-start: idempotent, log-only on failure.
							if lockFirst {
								if lockErr := deps.Store.LockGuide(ctx, tenantUID, mg.ID); lockErr != nil {
									log.Printf("grade: warning: could not lock guide %s: %v", mg.ID, lockErr)
								} else {
									log.Printf("grade: locked guide %s (lock-on-grading-start)", mg.ID)
								}
							}
						}
					} else if !errors.Is(listErr, store.ErrNotFound) {
						log.Printf("grade: warning: DB guide lookup for av=%s: %v; trying obj-store guide", avid, listErr)
					}
				}
			}
		}
	}

	// 2b. Fallback: try obj-store guide.v{N}.json.
	if !guideLoaded {
		guideKey := fmt.Sprintf("%s/%s/guide.v%d.json", env.TenantID, env.SubmissionID, guideVersion)
		guideData, guideErr := deps.ObjStore.Get(ctx, deps.Bucket, guideKey)
		if guideErr == nil {
			g, parseErr := grade.LoadGuideFromJSON(guideData)
			if parseErr != nil {
				log.Printf("grade: warning: could not parse obj-store guide %q (%v); falling back to LLMJudge", guideKey, parseErr)
			} else {
				scheme = grade.NewGuideMarkScheme(g, llmJudge, deps.Provider, deps.GradeModel)
				guideLoaded = true
				log.Printf("grade: loaded obj-store guide with %d entries", len(g))
			}
		} else if !errors.Is(guideErr, store.ErrNotFound) {
			// An actual storage error (not a missing object) is fatal.
			return fmt.Errorf("grade: get guide %q: %w", guideKey, guideErr)
		} else {
			log.Printf("grade: no guide found at %q; using LLMJudge", guideKey)
		}
	}
	_ = guideLoaded // used by log statements above

	// 3. Grade the paper.
	gradedPaper, err := grade.GradePaper(ctx, scheme, paper)
	if err != nil {
		return fmt.Errorf("grade: grade paper for %s/%s: %w", env.TenantID, env.SubmissionID, err)
	}
	log.Printf("grade: %s/%s → %d questions, score=%.1f%%",
		env.TenantID, env.SubmissionID, len(gradedPaper.Questions), gradedPaper.Score100)

	// 4. Persist graded.v{N}.json.
	gradedKey := fmt.Sprintf("%s/%s/graded.v%d.json", env.TenantID, env.SubmissionID, gradedVersion)

	// 4a. Archive-before-overwrite: if graded.v1.json already exists (regrade),
	// copy the existing bytes to graded.archive.{N}.json before overwriting.
	// N is chosen as the next free archive index (try 1, 2, 3, ... until one
	// doesn't exist). On the first grade (no prior artifact), no archive is made.
	if archiveErr := archivePriorGradedArtifact(ctx, deps.ObjStore, deps.Bucket, env.TenantID, env.SubmissionID); archiveErr != nil {
		return fmt.Errorf("grade: archive prior graded artifact: %w", archiveErr)
	}

	gradedJSON, err := json.Marshal(gradedPaper)
	if err != nil {
		return fmt.Errorf("grade: marshal graded paper: %w", err)
	}
	if err := deps.ObjStore.Put(ctx, deps.Bucket, gradedKey, gradedJSON, "application/json"); err != nil {
		return fmt.Errorf("grade: upload graded paper: %w", err)
	}

	// 5. Collect unique flags across all questions for the summary.
	flags := collectUniqueFlags(gradedPaper)

	// 6. Publish the grade.result sidecar and envelope.
	summary := GradeResultSummary{
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
	if err := deps.ObjStore.Put(ctx, deps.Bucket, summaryKey, summaryJSON, "application/json"); err != nil {
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
	if err := deps.Bus.Publish(ctx, "results.q", result); err != nil {
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

// objStorer is the minimal object-store interface required by
// archivePriorGradedArtifact. *store.ObjStore satisfies it.
// Defining it here (rather than in the store package) keeps the interface
// small and co-located with its only consumer, and lets tests inject a
// lightweight fake without a real MinIO server.
type objStorer interface {
	Get(ctx context.Context, bucket, key string) ([]byte, error)
	Put(ctx context.Context, bucket, key string, data []byte, contentType string) error
}

// archivePriorGradedArtifact copies the current graded.v1.json (if any) to the
// next free archive slot graded.archive.{N}.json before it is overwritten by a
// regrade.
//
// Index selection: start at N=1 and increment until an archive key does not
// exist. This is O(number of prior regrades) per submission — acceptable since
// regrades are rare. The caller is responsible for writing the new graded.v1.json
// afterwards; this function only reads and copies.
//
// If graded.v1.json does not yet exist (first grade), this is a no-op.
func archivePriorGradedArtifact(ctx context.Context, obj objStorer, bucket, tenantID, submissionID string) error {
	priorKey := fmt.Sprintf("%s/%s/graded.v%d.json", tenantID, submissionID, gradedVersion)
	priorData, err := obj.Get(ctx, bucket, priorKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// First grade — nothing to archive.
			return nil
		}
		return fmt.Errorf("read prior graded artifact %q: %w", priorKey, err)
	}

	// Find the next free archive index.
	var archiveIndex int
	for archiveIndex = 1; ; archiveIndex++ {
		archiveKey := fmt.Sprintf("%s/%s/graded.archive.%d.json", tenantID, submissionID, archiveIndex)
		_, checkErr := obj.Get(ctx, bucket, archiveKey)
		if errors.Is(checkErr, store.ErrNotFound) {
			// This index is free — use it.
			if putErr := obj.Put(ctx, bucket, archiveKey, priorData, "application/json"); putErr != nil {
				return fmt.Errorf("write archive %q: %w", archiveKey, putErr)
			}
			log.Printf("grade: archived prior graded artifact to %q (regrade, archive index %d)", archiveKey, archiveIndex)
			return nil
		}
		if checkErr != nil {
			return fmt.Errorf("check archive slot %d for %s/%s: %w", archiveIndex, tenantID, submissionID, checkErr)
		}
		// Slot exists — try the next one.
	}
}

// selectLockedOrLatestGuide implements Fix 3 (lock-on-grading pins batch to locked version).
//
// It calls ListGuideVersions (versions ordered DESC by version number) and
// selects the guide to grade against:
//   - If any version is LOCKED, the highest locked version (first locked found in
//     DESC order) is returned. lockFirst is false — no locking needed.
//   - If NONE is locked, the latest version (versions[0]) is returned and
//     lockFirst is true, indicating the caller should lock it.
//
// Returns (selectedGuide, lockFirst, error).
// Returns store.ErrNotFound (wrapped) when no versions exist.
func selectLockedOrLatestGuide(ctx context.Context, s *store.Store, tenantUID, avid uuid.UUID) (store.MarkingGuide, bool, error) {
	versions, err := s.ListGuideVersions(ctx, tenantUID, avid)
	if err != nil {
		return store.MarkingGuide{}, false, fmt.Errorf("ListGuideVersions av=%s: %w", avid, err)
	}
	if len(versions) == 0 {
		return store.MarkingGuide{}, false, fmt.Errorf("no guide versions for av=%s: %w", avid, store.ErrNotFound)
	}

	// Versions are ordered DESC by version number (newest first).
	// Find the first (highest-version) locked entry.
	for _, v := range versions {
		if v.Locked {
			log.Printf("grade: using locked guide %s (v%d) for assessment_version %s", v.ID, v.Version, avid)
			return v, false, nil
		}
	}

	// No locked version found: use the latest (versions[0]) and signal the caller to lock it.
	latest := versions[0]
	log.Printf("grade: no locked guide found for av=%s; using latest v%d and will lock it", avid, latest.Version)
	return latest, true, nil
}
