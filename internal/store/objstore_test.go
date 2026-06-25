package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func testMinioCfg(t *testing.T) Config {
	t.Helper()
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "minio/minio",
		Cmd:          []string{"server", "/data"},
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"MINIO_ROOT_USER":     "minioadmin",
			"MINIO_ROOT_PASSWORD": "minioadmin",
		},
		WaitingFor: wait.ForHTTP("/minio/health/live").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "9000")
	require.NoError(t, err)

	return Config{
		Endpoint:  fmt.Sprintf("%s:%s", host, port.Port()),
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		UseSSL:    false,
	}
}

func TestObjStoreEnsureBucket(t *testing.T) {
	s, err := New(testMinioCfg(t))
	require.NoError(t, err)
	require.NoError(t, s.EnsureBucket(context.Background(), "exams"))
}

func TestObjStorePutGet(t *testing.T) {
	s, err := New(testMinioCfg(t))
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, s.EnsureBucket(ctx, "exams"))
	data := []byte("hello novagrade")
	require.NoError(t, s.Put(ctx, "exams", "test/hello.txt", data, "text/plain"))
	got, err := s.Get(ctx, "exams", "test/hello.txt")
	require.NoError(t, err)
	require.Equal(t, data, got)
}
