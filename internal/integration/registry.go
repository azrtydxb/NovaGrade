package integration

import "sync"

// Registry maps (Category, provider) pairs to factory functions.
// Factories return the concrete connector (cast to the needed interface at call site).
type Registry struct {
	mu        sync.RWMutex
	factories map[registryKey]func() any
}

type registryKey struct {
	category Category
	provider string
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[registryKey]func() any),
	}
}

// Register associates a factory function with the given category and provider name.
// Registering the same key twice overwrites the previous factory.
func (r *Registry) Register(category Category, provider string, factory func() any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[registryKey{category, provider}] = factory
}

// Get retrieves the factory for (category, provider).
// Returns (nil, false) if no factory is registered.
func (r *Registry) Get(category Category, provider string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[registryKey{category, provider}]
	if !ok {
		return nil, false
	}
	return f(), true
}
