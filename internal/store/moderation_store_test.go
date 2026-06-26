package store

// moderation_store_test.go — integration tests for the moderation workflow.
//
// Requires Docker (testcontainers). Set SKIP_DOCKER_TESTS or pass -short to skip.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// mustCreateAVForTenant inserts an assessment_version for the given tenant and
// returns the assessment_version id. The tenant must already exist.
func mustCreateAVForTenant(t *testing.T, st *Store, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	assessID := uuid.New()
	_, err := st.pool.Exec(ctx,
		`INSERT INTO assessment (id, tenant_id, title) VALUES ($1, $2, $3)`,
		assessID, tenantID, "Test Assessment",
	)
	require.NoError(t, err)

	avID := uuid.New()
	_, err = st.pool.Exec(ctx,
		`INSERT INTO assessment_version (id, tenant_id, assessment_id, version_number) VALUES ($1, $2, $3, $4)`,
		avID, tenantID, assessID, 1,
	)
	require.NoError(t, err)

	return avID
}

// mustCreateSubmissionForAV inserts a submission tied to the given
// assessment_version and tenant, and returns the submission id.
func mustCreateSubmissionForAV(t *testing.T, st *Store, tenantID, avID uuid.UUID) uuid.UUID {
	t.Helper()
	sub, err := st.CreateSubmission(context.Background(), CreateSubmissionParams{
		TenantID:            tenantID,
		AssessmentVersionID: &avID,
	})
	require.NoError(t, err)
	return sub.ID
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateModerationSession_SamplesSubmissions verifies that:
//   - A session is created with the given parameters.
//   - The sample contains at most sample_size submissions.
//   - Sampling is deterministic (ORDER BY id).
//   - The sampled ids are also returned by ListModerationSubmissions.
func TestCreateModerationSession_SamplesSubmissions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)
	avID := mustCreateAVForTenant(t, st, tenantID)

	// Create 5 submissions for the AV.
	var allSubIDs []uuid.UUID
	for i := 0; i < 5; i++ {
		subID := mustCreateSubmissionForAV(t, st, tenantID, avID)
		allSubIDs = append(allSubIDs, subID)
	}

	sampleSize := 3
	sess, sampledIDs, err := st.CreateModerationSession(ctx, CreateModerationSessionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avID,
		CreatedBy:           "teacher@school.com",
		SampleSize:          sampleSize,
	})
	require.NoError(t, err)

	// Session fields.
	assert.NotEqual(t, uuid.Nil, sess.ID)
	assert.Equal(t, tenantID, sess.TenantID)
	assert.Equal(t, avID, sess.AssessmentVersionID)
	assert.Equal(t, "teacher@school.com", sess.CreatedBy)
	assert.Equal(t, sampleSize, sess.SampleSize)
	assert.Equal(t, "open", sess.Status)

	// Sampled ids.
	assert.LessOrEqual(t, len(sampledIDs), sampleSize, "sampled count must not exceed sample_size")
	assert.Equal(t, sampleSize, len(sampledIDs), "expected exactly sample_size when enough submissions exist")

	// Verify that the sampled ids are a subset of all submitted ids.
	allSubIDSet := make(map[uuid.UUID]bool, len(allSubIDs))
	for _, id := range allSubIDs {
		allSubIDSet[id] = true
	}
	for _, id := range sampledIDs {
		assert.True(t, allSubIDSet[id], "sampled id %s must be a real submission id", id)
	}

	// ListModerationSubmissions must return the same set.
	listed, err := st.ListModerationSubmissions(ctx, tenantID, sess.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, sampledIDs, listed)
}

// TestCreateModerationSession_SmallPool verifies that when fewer submissions
// exist than sample_size, all of them are sampled (no error).
func TestCreateModerationSession_SmallPool(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)
	avID := mustCreateAVForTenant(t, st, tenantID)

	// Only 2 submissions, but sample_size = 5.
	mustCreateSubmissionForAV(t, st, tenantID, avID)
	mustCreateSubmissionForAV(t, st, tenantID, avID)

	sess, sampledIDs, err := st.CreateModerationSession(ctx, CreateModerationSessionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avID,
		CreatedBy:           "teacher@school.com",
		SampleSize:          5,
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, sess.ID)
	assert.Equal(t, 2, len(sampledIDs), "only 2 submissions exist, so 2 should be sampled")
}

// TestRecordModerationMark_RoundTrip verifies that a moderator mark round-trips
// through insert and list without loss of precision.
func TestRecordModerationMark_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)
	avID := mustCreateAVForTenant(t, st, tenantID)
	subID := mustCreateSubmissionForAV(t, st, tenantID, avID)

	sess, _, err := st.CreateModerationSession(ctx, CreateModerationSessionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avID,
		CreatedBy:           "teacher@school.com",
		SampleSize:          10,
	})
	require.NoError(t, err)

	// Record two marks for the same submission, different questions.
	m1, err := st.RecordModerationMark(ctx, RecordModerationMarkParams{
		TenantID:       tenantID,
		SessionID:      sess.ID,
		SubmissionID:   subID,
		QuestionNo:     "Q1",
		ModeratorMarks: 8.5,
		Moderator:      "moderator@school.com",
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, m1.ID)
	assert.Equal(t, 8.5, m1.ModeratorMarks)
	assert.Equal(t, "Q1", m1.QuestionNo)
	assert.Equal(t, "moderator@school.com", m1.Moderator)
	assert.Equal(t, sess.ID, m1.SessionID)
	assert.Equal(t, subID, m1.SubmissionID)
	assert.Equal(t, tenantID, m1.TenantID)

	m2, err := st.RecordModerationMark(ctx, RecordModerationMarkParams{
		TenantID:       tenantID,
		SessionID:      sess.ID,
		SubmissionID:   subID,
		QuestionNo:     "Q2",
		ModeratorMarks: 4.0,
		Moderator:      "moderator@school.com",
	})
	require.NoError(t, err)
	assert.Equal(t, "Q2", m2.QuestionNo)

	// ListModerationMarks must return both, in chronological order.
	marks, err := st.ListModerationMarks(ctx, tenantID, sess.ID)
	require.NoError(t, err)
	require.Len(t, marks, 2)
	assert.Equal(t, m1.ID, marks[0].ID)
	assert.Equal(t, m2.ID, marks[1].ID)
}

// TestGetModerationSession_NotFound verifies ErrNotFound for unknown id.
func TestGetModerationSession_NotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)
	_, err := st.GetModerationSession(ctx, tenantID, uuid.New())
	require.ErrorIs(t, err, ErrNotFound)
}

// TestGetModerationSession_CrossTenant verifies that a session belonging to
// tenantA is not visible to tenantB (ErrNotFound).
func TestGetModerationSession_CrossTenant(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantA := mustCreateSchool(t, st)
	tenantB := mustCreateSchool(t, st)

	avID := mustCreateAVForTenant(t, st, tenantA)
	mustCreateSubmissionForAV(t, st, tenantA, avID)

	sess, _, err := st.CreateModerationSession(ctx, CreateModerationSessionParams{
		TenantID:            tenantA,
		AssessmentVersionID: avID,
		CreatedBy:           "teacher@a.com",
		SampleSize:          1,
	})
	require.NoError(t, err)

	// tenantB cannot see tenantA's session.
	_, err = st.GetModerationSession(ctx, tenantB, sess.ID)
	require.ErrorIs(t, err, ErrNotFound, "cross-tenant session lookup must return ErrNotFound")
}

// TestListModerationSubmissions_TenantFiltered verifies that
// ListModerationSubmissions is tenant-filtered.
func TestListModerationSubmissions_TenantFiltered(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantA := mustCreateSchool(t, st)
	tenantB := mustCreateSchool(t, st)

	avID := mustCreateAVForTenant(t, st, tenantA)
	mustCreateSubmissionForAV(t, st, tenantA, avID)

	sess, _, err := st.CreateModerationSession(ctx, CreateModerationSessionParams{
		TenantID:            tenantA,
		AssessmentVersionID: avID,
		CreatedBy:           "teacher@a.com",
		SampleSize:          1,
	})
	require.NoError(t, err)

	// tenantB sees no submissions for tenantA's session.
	ids, err := st.ListModerationSubmissions(ctx, tenantB, sess.ID)
	require.NoError(t, err)
	assert.Empty(t, ids, "cross-tenant ListModerationSubmissions must return empty")
}
