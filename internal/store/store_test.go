package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// newTestStore spins up a Postgres testcontainer, runs goose migrations, and
// returns a *Store connected to it. The container is terminated at test cleanup.
// Gate: skipped when SKIP_DOCKER_TESTS is set or -short is passed.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("SKIP_DOCKER_TESTS") != "" || testing.Short() {
		t.Skip("requires Docker (set SKIP_DOCKER_TESTS to skip, or use -short flag)")
	}

	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "postgres:16-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "novagrade",
			"POSTGRES_PASSWORD": "novagrade",
			"POSTGRES_DB":       "novagrade_test",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	portNat, err := container.MappedPort(ctx, "5432")
	require.NoError(t, err)

	portNum, err := strconv.Atoi(portNat.Port())
	require.NoError(t, err)

	cfg := DBConfig{
		Host:     host,
		Port:     portNum,
		User:     "novagrade",
		Password: "novagrade",
		Database: "novagrade_test",
		SSLMode:  "disable",
	}

	st, err := NewStore(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	require.NoError(t, st.MigrateUp(ctx), "goose migrations must succeed")

	return st
}

// mustCreateSchool inserts a school row (tenant root) and returns its id.
func mustCreateSchool(t *testing.T, st *Store) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	_, err := st.pool.Exec(ctx,
		`INSERT INTO school (id, name, slug) VALUES ($1, $2, $3)`,
		id, fmt.Sprintf("Test School %s", id), id.String(),
	)
	require.NoError(t, err)
	return id
}

// mustCreateSubmission inserts a minimal submission (for the given tenant) and
// returns its id.
func mustCreateSubmission(t *testing.T, st *Store) uuid.UUID {
	t.Helper()
	tenantID := mustCreateSchool(t, st)
	ctx := context.Background()
	sub, err := st.CreateSubmission(ctx, CreateSubmissionParams{
		TenantID: tenantID,
	})
	require.NoError(t, err)
	return sub.ID
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestSetSubmissionStateAudited(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id := mustCreateSubmission(t, st)

	require.NoError(t, st.SetSubmissionState(ctx, id, contracts.StateTranscribing))

	got, err := st.GetSubmission(ctx, id)
	require.NoError(t, err)
	require.Equal(t, contracts.StateTranscribing, got.State)
}

func TestCreateSubmission_returnsUploadedState(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)
	pdfKey := "exams/2024/test.pdf"
	sub, err := st.CreateSubmission(ctx, CreateSubmissionParams{
		TenantID:     tenantID,
		SourcePDFKey: &pdfKey,
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, sub.ID)
	require.Equal(t, contracts.StateUploaded, sub.State)
	require.Equal(t, tenantID, sub.TenantID)
	require.NotNil(t, sub.SourcePDFKey)
	require.Equal(t, pdfKey, *sub.SourcePDFKey)
}

func TestGetSubmission_notFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	_, err := st.GetSubmission(ctx, uuid.New())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSetSubmissionState_notFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	err := st.SetSubmissionState(ctx, uuid.New(), contracts.StateTranscribing)
	require.True(t, errors.Is(err, ErrNotFound), "expected ErrNotFound, got: %v", err)
}

func TestInsertAuditEvent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, st)
	subID := uuid.New()

	ev, err := st.InsertAuditEvent(ctx, InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &subID,
		Actor:      "pipeline",
		Action:     "state_change",
		NewValue:   []byte(`{"state":"transcribing"}`),
		Reason:     "unit test",
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, ev.ID)
	require.Equal(t, "submission", ev.EntityType)
	require.Equal(t, "state_change", ev.Action)

	// Read the row back directly from Postgres to verify persistence (not just RETURNING echo).
	var (
		persistedActor      string
		persistedAction     string
		persistedEntityType string
	)
	row := st.pool.QueryRow(ctx,
		`SELECT actor, action, entity_type FROM audit_event WHERE id = $1`,
		ev.ID,
	)
	require.NoError(t, row.Scan(&persistedActor, &persistedAction, &persistedEntityType))
	require.Equal(t, "pipeline", persistedActor, "persisted actor must match inserted value")
	require.Equal(t, "state_change", persistedAction, "persisted action must match inserted value")
	require.Equal(t, "submission", persistedEntityType, "persisted entity_type must match inserted value")
}
