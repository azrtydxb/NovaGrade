package store

// appeal_store_test.go — integration tests for the appeal / regrade workflow.
//
// Requires Docker (testcontainers). Set SKIP_DOCKER_TESTS or pass -short to skip.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// mustCreateAppeal inserts an appeal for the given tenant and submission,
// and returns it. Fails the test on error.
func mustCreateAppeal(t *testing.T, st *Store, tenantID, submissionID uuid.UUID, reason, requestedBy string) Appeal {
	t.Helper()
	a, err := st.CreateAppeal(context.Background(), CreateAppealParams{
		TenantID:     tenantID,
		SubmissionID: submissionID,
		Reason:       reason,
		RequestedBy:  requestedBy,
	})
	require.NoError(t, err)
	return a
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateAppeal_Open verifies that a newly created appeal has status "open".
func TestCreateAppeal_Open(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)
	subID := uuid.New()

	a, err := st.CreateAppeal(ctx, CreateAppealParams{
		TenantID:     tenantID,
		SubmissionID: subID,
		Reason:       "My score is wrong",
		RequestedBy:  "student@school.com",
	})
	require.NoError(t, err)

	assert.NotEqual(t, uuid.Nil, a.ID)
	assert.Equal(t, tenantID, a.TenantID)
	assert.Equal(t, subID, a.SubmissionID)
	assert.Equal(t, "open", a.Status, "newly created appeal must have status 'open'")
	assert.Equal(t, "My score is wrong", a.Reason)
	assert.Equal(t, "student@school.com", a.RequestedBy)
	assert.Equal(t, "", a.Resolution)
	assert.False(t, a.CreatedAt.IsZero())
	assert.False(t, a.UpdatedAt.IsZero())
}

// TestListAppeals_ByStatus verifies that listing by status filters correctly,
// and that an empty status returns all appeals for the tenant.
func TestListAppeals_ByStatus(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)
	subID := uuid.New()

	// Create two appeals in "open" status.
	mustCreateAppeal(t, st, tenantID, subID, "Reason A", "student-a")
	a2 := mustCreateAppeal(t, st, tenantID, subID, "Reason B", "student-b")

	// Resolve one of them.
	err := st.UpdateAppealStatus(ctx, tenantID, a2.ID, "resolved", "Marks recounted")
	require.NoError(t, err)

	// List only open appeals.
	open, err := st.ListAppeals(ctx, tenantID, "open")
	require.NoError(t, err)
	assert.Len(t, open, 1, "should find exactly 1 open appeal")
	assert.Equal(t, "open", open[0].Status)

	// List only resolved appeals.
	resolved, err := st.ListAppeals(ctx, tenantID, "resolved")
	require.NoError(t, err)
	assert.Len(t, resolved, 1, "should find exactly 1 resolved appeal")
	assert.Equal(t, "resolved", resolved[0].Status)

	// List all appeals (status = "").
	all, err := st.ListAppeals(ctx, tenantID, "")
	require.NoError(t, err)
	assert.Len(t, all, 2, "empty status filter should return all appeals for tenant")
}

// TestGetAppeal_NotFound verifies that GetAppeal returns ErrNotFound for a
// non-existent appeal.
func TestGetAppeal_NotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)

	_, err := st.GetAppeal(ctx, tenantID, uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "expected ErrNotFound, got: %v", err)
}

// TestUpdateAppealStatus_NotFound verifies that UpdateAppealStatus returns
// ErrNotFound when no matching appeal row exists.
func TestUpdateAppealStatus_NotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)

	err := st.UpdateAppealStatus(ctx, tenantID, uuid.New(), "resolved", "nothing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "expected ErrNotFound, got: %v", err)
}

// TestAppeal_TenantFiltering verifies strict tenant isolation:
//   - An appeal created for tenantA cannot be retrieved with tenantB's GetAppeal.
//   - ListAppeals for tenantB returns an empty slice (not tenantA's appeals).
func TestAppeal_TenantFiltering(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantA := mustCreateSchool(t, st)
	tenantB := mustCreateSchool(t, st)
	subID := uuid.New()

	a := mustCreateAppeal(t, st, tenantA, subID, "Appeal from A", "student@a.com")

	// GetAppeal with tenantB must return ErrNotFound.
	_, err := st.GetAppeal(ctx, tenantB, a.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"cross-tenant GetAppeal must return ErrNotFound, got: %v", err)

	// ListAppeals for tenantB must return an empty slice.
	appeals, err := st.ListAppeals(ctx, tenantB, "")
	require.NoError(t, err)
	assert.Empty(t, appeals, "tenantB must not see tenantA's appeals")
}
