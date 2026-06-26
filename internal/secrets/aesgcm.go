package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

// Encrypt encrypts plaintext using AES-256-GCM.
// key must be exactly 32 bytes. Returns nonce||ciphertext.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secrets: rand nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	return append(nonce, ciphertext...), nil
}

// Decrypt decrypts a blob produced by Encrypt (nonce||ciphertext).
// Returns an error on wrong key, tampered ciphertext, or short blob.
func Decrypt(key, blob []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new GCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(blob) < nonceSize+1 {
		return nil, errors.New("secrets: blob too short")
	}
	nonce, ciphertext := blob[:nonceSize], blob[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt failed: %w", err)
	}
	return plaintext, nil
}

// KeyFromEnv reads a base64-standard-encoded 32-byte key from the named environment variable.
func KeyFromEnv(name string) ([]byte, error) {
	val := os.Getenv(name)
	if val == "" {
		return nil, fmt.Errorf("secrets: env var %q not set", name)
	}
	key, err := base64.StdEncoding.DecodeString(val)
	if err != nil {
		return nil, fmt.Errorf("secrets: decode %q: %w", name, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key from %q must be 32 bytes, got %d", name, len(key))
	}
	return key, nil
}
