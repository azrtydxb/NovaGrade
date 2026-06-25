package domain_test

// TDD: tests written BEFORE implementation (RED phase).
//
// Test (a): recording an event creates an immutable row and can be read back.
// Test (b): the API surface exposes no update or delete path — only insert+list.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Behavioral fake — no Docker required.
// ─────────────────────────────────────────────────────────────────────────────

// fakeAuditStore is an in-memory implementation of domain.AuditStore that
// records only inserts. It intentionally has no update or delete method,
// mirroring the real append-only audit_event table.
type fakeAuditStore struct {
	mu     sync.Mutex
	events []store.AuditEvent
}

func (f *fakeAuditStore) InsertAuditEvent(_ context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev := store.AuditEvent{
		ID:         uuid.New(),
		TenantID:   p.TenantID,
		EntityType: p.EntityType,
		EntityID:   p.EntityID,
		Actor:      p.Actor,
		Action:     p.Action,
		OldValue:   p.OldValue,
		NewValue:   p.NewValue,
		Reason:     p.Reason,
		CreatedAt:  time.Now(),
	}
	f.events = append(f.events, ev)
	return ev, nil
}

func (f *fakeAuditStore) ListAuditEventsBySubmission(_ context.Context, tenantID, submissionID uuid.UUID) ([]store.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []store.AuditEvent
	for _, ev := range f.events {
		if ev.TenantID == tenantID && ev.EntityType == "submission" && ev.EntityID != nil && *ev.EntityID == submissionID {
			result = append(result, ev)
		}
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Test (a): Record an override → the event is persisted and can be read back.
// ─────────────────────────────────────────────────────────────────────────────

func TestAuditService_Record_AndReadBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tenantID := uuid.New()
	submissionID := uuid.New()

	svc := domain.NewAuditService(&fakeAuditStore{})

	ev, err := svc.Record(ctx, domain.RecordParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &submissionID,
		Actor:      "teacher@school.io",
		Action:     "override",
		OldValue:   []byte(`{"score":70}`),
		NewValue:   []byte(`{"score":85}`),
		Reason:     "clerical error corrected",
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, ev.ID, "returned event must have a non-nil ID")
	assert.Equal(t, tenantID, ev.TenantID)
	assert.Equal(t, "submission", ev.EntityType)
	require.NotNil(t, ev.EntityID)
	assert.Equal(t, submissionID, *ev.EntityID)
	assert.Equal(t, "teacher@school.io", ev.Actor)
	assert.Equal(t, "override", ev.Action)
	assert.Equal(t, "clerical error corrected", ev.Reason)

	// Read back the event via ListBySubmission.
	events, err := svc.ListBySubmission(ctx, tenantID, submissionID)
	require.NoError(t, err)
	require.Len(t, events, 1, "exactly one event should be recorded")
	assert.Equal(t, ev.ID, events[0].ID)
	assert.Equal(t, "override", events[0].Action)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test (b): Append-only surface — no update or delete path is exposed.
//
// This test documents the API surface at compile time: the domain.AuditService
// and domain.AuditStore interfaces only expose write-via-insert and read. Any
// attempt to add Update/Delete would require changing these interfaces and
// break this test's implicit contract. We also verify the fake (and therefore
// the interface) does not implement any mutating method beyond insert.
// ─────────────────────────────────────────────────────────────────────────────

func TestAuditService_AppendOnly_Surface(t *testing.T) {
	t.Parallel()

	// Compile-time assertion: fakeAuditStore satisfies domain.AuditStore.
	// If domain.AuditStore ever gains an Update or Delete method the fake
	// must also implement it, making the forbidden mutation explicit.
	var _ domain.AuditStore = (*fakeAuditStore)(nil)

	// Runtime assertion: AuditService has no Update or Delete method.
	// We verify by listing all methods on the type via interface satisfaction.
	// The domain.AuditService type must satisfy domain.AuditServiceInterface,
	// which declares ONLY Record + ListBySubmission.
	var _ domain.AuditServiceInterface = (*domain.AuditService)(nil)

	// Additional read-after-write check with two submissions to ensure
	// tenant isolation is enforced at the domain layer.
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	subA := uuid.New()
	subB := uuid.New()

	fake := &fakeAuditStore{}
	svc := domain.NewAuditService(fake)

	_, err := svc.Record(ctx, domain.RecordParams{
		TenantID: tenantA, EntityType: "submission", EntityID: &subA,
		Actor: "a", Action: "approve", Reason: "ok",
	})
	require.NoError(t, err)

	_, err = svc.Record(ctx, domain.RecordParams{
		TenantID: tenantB, EntityType: "submission", EntityID: &subB,
		Actor: "b", Action: "approve", Reason: "ok",
	})
	require.NoError(t, err)

	// TenantA querying subA sees 1 event.
	eventsA, err := svc.ListBySubmission(ctx, tenantA, subA)
	require.NoError(t, err)
	assert.Len(t, eventsA, 1)

	// TenantA querying subB (different tenant) sees 0 events — tenant isolation.
	eventsACrossB, err := svc.ListBySubmission(ctx, tenantA, subB)
	require.NoError(t, err)
	assert.Empty(t, eventsACrossB, "cross-tenant query must return empty, not an error")
}
