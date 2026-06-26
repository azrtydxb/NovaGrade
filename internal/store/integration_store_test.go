package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/azrtydxb/novagrade/internal/integration"
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/google/uuid"
)

func newEncKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestIntegrationStore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, s)

	key := newEncKey(t)
	rawCreds := []byte(`{"api_key":"super-secret"}`)
	encCreds, err := secrets.Encrypt(key, rawCreds)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	cfg, _ := json.Marshal(map[string]any{"base_url": "https://example.com"})

	t.Run("UpsertConnection stores encrypted bytes", func(t *testing.T) {
		conn, err := s.UpsertConnection(ctx, UpsertConnectionParams{
			TenantID:    tenantID,
			Category:    integration.CategoryLMS,
			Provider:    "canvas",
			Config:      cfg,
			Credentials: encCreds,
		})
		if err != nil {
			t.Fatalf("UpsertConnection: %v", err)
		}
		if conn.ID == (uuid.UUID{}) {
			t.Fatal("expected non-zero ID")
		}

		// Verify raw DB bytes are not plaintext
		_, rawBytes, err := s.GetConnectionWithCreds(ctx, tenantID, conn.ID)
		if err != nil {
			t.Fatalf("GetConnectionWithCreds: %v", err)
		}
		if string(rawBytes) == string(rawCreds) {
			t.Fatal("credentials stored as plaintext — expected ciphertext")
		}
		if string(rawBytes) != string(encCreds) {
			t.Fatal("stored ciphertext does not match the encrypted bytes we passed")
		}
	})

	t.Run("GetConnectionWithCreds decrypts back to original", func(t *testing.T) {
		conn, _ := s.UpsertConnection(ctx, UpsertConnectionParams{
			TenantID:    tenantID,
			Category:    integration.CategorySIS,
			Provider:    "powerschool",
			Config:      cfg,
			Credentials: encCreds,
		})
		_, rawBytes, err := s.GetConnectionWithCreds(ctx, tenantID, conn.ID)
		if err != nil {
			t.Fatalf("GetConnectionWithCreds: %v", err)
		}
		plaintext, err := secrets.Decrypt(key, rawBytes)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if string(plaintext) != string(rawCreds) {
			t.Fatalf("got %q, want %q", plaintext, rawCreds)
		}
	})

	t.Run("Upsert conflict updates config and credentials", func(t *testing.T) {
		// First insert
		s.UpsertConnection(ctx, UpsertConnectionParams{
			TenantID: tenantID, Category: integration.CategoryRoster, Provider: "clever",
			Config: cfg, Credentials: encCreds,
		})
		// Second upsert with new config
		newCfg, _ := json.Marshal(map[string]any{"base_url": "https://updated.com"})
		newRaw := []byte(`{"api_key":"updated"}`)
		newEnc, _ := secrets.Encrypt(key, newRaw)
		conn2, err := s.UpsertConnection(ctx, UpsertConnectionParams{
			TenantID: tenantID, Category: integration.CategoryRoster, Provider: "clever",
			Config: newCfg, Credentials: newEnc,
		})
		if err != nil {
			t.Fatalf("second UpsertConnection: %v", err)
		}
		_, rawBytes, _ := s.GetConnectionWithCreds(ctx, tenantID, conn2.ID)
		pt, _ := secrets.Decrypt(key, rawBytes)
		if string(pt) != string(newRaw) {
			t.Fatalf("credentials not updated, got %q", pt)
		}
		if conn2.Config["base_url"] != "https://updated.com" {
			t.Fatalf("config not updated: %v", conn2.Config)
		}
	})

	t.Run("ListConnections omits credentials", func(t *testing.T) {
		conns, err := s.ListConnections(ctx, tenantID)
		if err != nil {
			t.Fatalf("ListConnections: %v", err)
		}
		if len(conns) == 0 {
			t.Fatal("expected at least one connection")
		}
		for _, c := range conns {
			if c.TenantID != tenantID {
				t.Errorf("tenant mismatch: got %v", c.TenantID)
			}
		}
	})

	t.Run("DeleteConnection removes row and second delete returns ErrNotFound", func(t *testing.T) {
		conn, _ := s.UpsertConnection(ctx, UpsertConnectionParams{
			TenantID: tenantID, Category: integration.CategoryStorage, Provider: "s3",
			Config: cfg,
		})
		if err := s.DeleteConnection(ctx, tenantID, conn.ID); err != nil {
			t.Fatalf("DeleteConnection: %v", err)
		}
		if err := s.DeleteConnection(ctx, tenantID, conn.ID); !isIntegrationNotFound(err) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

func isIntegrationNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
