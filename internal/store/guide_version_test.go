package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// mustCreateAssessmentVersion creates all required parent rows (school, assessment,
// assessment_version) and returns (tenantID, assessmentVersionID).
func mustCreateAssessmentVersion(t *testing.T, st *Store) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)

	assessmentID := uuid.New()
	_, err := st.pool.Exec(ctx,
		`INSERT INTO assessment (id, tenant_id, title) VALUES ($1, $2, $3)`,
		assessmentID, tenantID, "Test Assessment",
	)
	require.NoError(t, err)

	avID := uuid.New()
	_, err = st.pool.Exec(ctx,
		`INSERT INTO assessment_version (id, tenant_id, assessment_id, version_number) VALUES ($1, $2, $3, $4)`,
		avID, tenantID, assessmentID, 1,
	)
	require.NoError(t, err)

	return tenantID, avID
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestInsertGuideVersion_AutoIncrements(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID, avID := mustCreateAssessmentVersion(t, st)

	// Insert first guide version.
	g1, err := st.InsertGuideVersion(ctx, InsertGuideVersionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avID,
		Name:                "Initial guide",
		Content:             []byte(`{"questions":[]}`),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, g1.ID)
	require.Equal(t, 1, g1.Version)
	require.Equal(t, "Initial guide", g1.Name)
	require.False(t, g1.Locked)
	require.Nil(t, g1.LockedAt)

	// Insert second guide version — version must auto-increment to 2.
	g2, err := st.InsertGuideVersion(ctx, InsertGuideVersionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avID,
		Name:                "Updated guide",
		Content:             []byte(`{"questions":[{"id":1}]}`),
	})
	require.NoError(t, err)
	require.Equal(t, 2, g2.Version)
	require.Equal(t, "Updated guide", g2.Name)

	// GetLatestGuide must return version 2 with its content.
	latest, err := st.GetLatestGuide(ctx, tenantID, avID)
	require.NoError(t, err)
	require.Equal(t, 2, latest.Version)
	require.Equal(t, g2.ID, latest.ID)
	require.JSONEq(t, `{"questions":[{"id":1}]}`, string(latest.Content))
}

func TestListGuideVersions_DescOrder(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID, avID := mustCreateAssessmentVersion(t, st)

	_, err := st.InsertGuideVersion(ctx, InsertGuideVersionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avID,
		Name:                "v1",
		Content:             []byte(`{"v":1}`),
	})
	require.NoError(t, err)

	_, err = st.InsertGuideVersion(ctx, InsertGuideVersionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avID,
		Name:                "v2",
		Content:             []byte(`{"v":2}`),
	})
	require.NoError(t, err)

	guides, err := st.ListGuideVersions(ctx, tenantID, avID)
	require.NoError(t, err)
	require.Len(t, guides, 2)
	// Must be descending: v2 first, v1 second.
	require.Equal(t, 2, guides[0].Version)
	require.Equal(t, 1, guides[1].Version)
}

func TestLockGuide(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID, avID := mustCreateAssessmentVersion(t, st)

	g, err := st.InsertGuideVersion(ctx, InsertGuideVersionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avID,
		Name:                "lockable guide",
		Content:             []byte(`{}`),
	})
	require.NoError(t, err)
	require.False(t, g.Locked)

	// Lock the guide.
	require.NoError(t, st.LockGuide(ctx, tenantID, g.ID))

	// Read back to verify.
	latest, err := st.GetLatestGuide(ctx, tenantID, avID)
	require.NoError(t, err)
	require.True(t, latest.Locked)
	require.NotNil(t, latest.LockedAt)

	// LockGuide on a random uuid must return ErrNotFound.
	err = st.LockGuide(ctx, tenantID, uuid.New())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGetLatestGuide_None(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID, avID := mustCreateAssessmentVersion(t, st)

	_, err := st.GetLatestGuide(ctx, tenantID, avID)
	require.ErrorIs(t, err, ErrNotFound)
}
