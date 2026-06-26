package secrets_test

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/azrtydxb/novagrade/internal/secrets"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := newKey(t)
	plaintext := []byte("hello, NovaGrade!")
	blob, err := secrets.Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := secrets.Decrypt(key, blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	key := newKey(t)
	blob, _ := secrets.Encrypt(key, []byte("secret"))
	wrongKey := newKey(t)
	if _, err := secrets.Decrypt(wrongKey, blob); err == nil {
		t.Fatal("expected error with wrong key, got nil")
	}
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	key := newKey(t)
	blob, _ := secrets.Encrypt(key, []byte("secret"))
	// flip a byte in the middle (past the 12-byte nonce)
	mid := len(blob) / 2
	blob[mid] ^= 0xFF
	if _, err := secrets.Decrypt(key, blob); err == nil {
		t.Fatal("expected error with tampered ciphertext, got nil")
	}
}

func TestNon32ByteKeyErrors(t *testing.T) {
	shortKey := make([]byte, 16)
	if _, err := secrets.Encrypt(shortKey, []byte("x")); err == nil {
		t.Fatal("expected error for short key in Encrypt")
	}
	if _, err := secrets.Decrypt(shortKey, []byte("x")); err == nil {
		t.Fatal("expected error for short key in Decrypt")
	}
}

func TestKeyFromEnv(t *testing.T) {
	key := newKey(t)
	encoded := base64.StdEncoding.EncodeToString(key)
	t.Setenv("TEST_SECRET_KEY", encoded)

	got, err := secrets.KeyFromEnv("TEST_SECRET_KEY")
	if err != nil {
		t.Fatalf("KeyFromEnv: %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("decoded key mismatch")
	}
}

func TestDecryptShortBlobFails(t *testing.T) {
	key := newKey(t)
	short := make([]byte, 10) // < nonceSize(12) + gcm.Overhead(16) = 28
	if _, err := secrets.Decrypt(key, short); err == nil {
		t.Fatal("expected error for short blob, got nil")
	}
}
