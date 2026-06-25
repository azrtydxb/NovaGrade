package auth

import (
	"errors"
)

// ErrUnauthorized is returned when authentication fails.
var ErrUnauthorized = errors.New("auth: unauthorized")

// APIKeyResolver resolves API keys to principals using a config-backed map.
// Phase 1: keys stored in-memory from config. No DB table required.
// To add a key, call Register before serving requests.
type APIKeyResolver struct {
	keys map[string]Principal // key → principal
}

// NewAPIKeyResolver creates an empty resolver.
func NewAPIKeyResolver() *APIKeyResolver {
	return &APIKeyResolver{keys: make(map[string]Principal)}
}

// Register adds an API key → principal mapping.
// In Phase 1 keys are stored plaintext in-memory from startup config.
func (r *APIKeyResolver) Register(key string, p Principal) {
	r.keys[key] = p
}

// Resolve looks up a raw API key and returns the associated Principal.
// Returns ErrUnauthorized if the key is unknown.
func (r *APIKeyResolver) Resolve(key string) (Principal, error) {
	p, ok := r.keys[key]
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	return p, nil
}
