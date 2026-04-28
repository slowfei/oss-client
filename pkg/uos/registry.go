package uos

import (
	"context"
	"fmt"
	"sync"
)

// defaultRegistry is the process-global Registry returned by
// DefaultRegistry. Drivers register themselves here from package init().
var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-global Registry. Drivers register
// themselves with it from package init(); business code calls Open()
// against it to construct a Client.
//
// The default Registry is safe for concurrent use.
func DefaultRegistry() Registry {
	return defaultRegistry
}

// inProcessRegistry is the default in-process Registry implementation.
// It is unexported because callers should depend on the Registry
// interface, not on this concrete type.
type inProcessRegistry struct {
	mu        sync.RWMutex
	factories map[Provider]Factory
}

// NewRegistry constructs a fresh, empty Registry. Most callers should
// use DefaultRegistry() instead; NewRegistry is exposed primarily so
// tests can build isolated Registries that don't see process-globally
// registered Factories.
func NewRegistry() Registry {
	return &inProcessRegistry{
		factories: make(map[Provider]Factory),
	}
}

// Register adds f to the Registry, keyed by f.Provider(). Returns
// *Error{Code: ErrInvalidArgument} if f is nil, f.Provider() is empty,
// or a Factory is already registered for the same Provider id
// (register-once-per-provider rule).
func (r *inProcessRegistry) Register(f Factory) error {
	if f == nil {
		return &Error{
			Code:      ErrInvalidArgument,
			Operation: "Registry.Register",
			Message:   "nil Factory",
		}
	}
	id := f.Provider()
	if id == "" {
		return &Error{
			Code:      ErrInvalidArgument,
			Operation: "Registry.Register",
			Message:   "Factory.Provider() returned empty id",
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.factories[id]; exists {
		return &Error{
			Code:      ErrAlreadyExists,
			Operation: "Registry.Register",
			Provider:  id,
			Message:   fmt.Sprintf("Factory for provider %q is already registered", string(id)),
		}
	}
	r.factories[id] = f
	return nil
}

// Open dispatches cfg to the registered Factory, invoking Validate
// then Open. Returns *Error{Code: ErrInvalidArgument} if cfg.Provider
// is empty or no Factory is registered for it; surfaces the Factory's
// Validate / Open errors verbatim otherwise (drivers are responsible
// for returning *Error from their own paths).
func (r *inProcessRegistry) Open(ctx context.Context, cfg Config) (Client, error) {
	if cfg.Provider == "" {
		return nil, &Error{
			Code:      ErrInvalidArgument,
			Operation: "Registry.Open",
			Message:   "Config.Provider is empty",
		}
	}

	r.mu.RLock()
	f, ok := r.factories[cfg.Provider]
	r.mu.RUnlock()

	if !ok {
		return nil, &Error{
			Code:      ErrInvalidArgument,
			Operation: "Registry.Open",
			Provider:  cfg.Provider,
			Message:   fmt.Sprintf("no Factory registered for provider %q", string(cfg.Provider)),
		}
	}

	if err := f.Validate(cfg); err != nil {
		return nil, err
	}
	return f.Open(ctx, cfg)
}
