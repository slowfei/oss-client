package transfer

import (
	"context"
	"errors"
	"sync"
)

// ErrStateNotFound is the sentinel returned by StateStore.Load when the
// requested key has no payload. Use errors.Is to test for it.
var ErrStateNotFound = errors.New("transfer: resume state not found")

// StateStore persists opaque resume payloads keyed by an arbitrary
// caller-provided string. The payload is []byte and the StateStore
// knows nothing about its structure: each driver owns the schema of
// what it serialises into the payload. This is the load-bearing
// decoupling that lets pkg/uos/transfer remain stable while drivers
// evolve their resume formats independently (architecture_plan §4.11).
//
// Implementations MUST be safe for concurrent use across goroutines.
// Save MUST be atomic with respect to concurrent Load on the same key
// (no torn reads). Delete MUST be a no-op when the key is absent.
// Load MUST return ErrStateNotFound when the key is absent so callers
// can distinguish "no prior state" from a real error.
type StateStore interface {
	// Save persists data for key, overwriting any prior payload. data
	// is opaque to the StateStore; callers own the schema.
	Save(ctx context.Context, key string, data []byte) error
	// Load returns the payload previously stored under key, or
	// ErrStateNotFound if the key has no payload.
	Load(ctx context.Context, key string) ([]byte, error)
	// Delete removes the payload stored under key. It is a no-op when
	// the key is absent.
	Delete(ctx context.Context, key string) error
}

// MemoryStateStore is a safe-for-concurrent-use in-memory StateStore.
// It is intended for tests, ephemeral processes, and callers who do
// not need resume across process restarts.
type MemoryStateStore struct {
	m sync.Map // map[string][]byte
}

// NewMemoryStateStore returns an empty MemoryStateStore ready for use.
// The zero value is also usable; this constructor exists for symmetry
// with future variants.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{}
}

// Save stores a defensive copy of data under key. The copy means
// callers may mutate their buffer after Save returns without affecting
// stored state.
func (s *MemoryStateStore) Save(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	s.m.Store(key, cp)
	return nil
}

// Load returns a defensive copy of the payload previously stored under
// key, or ErrStateNotFound when key is absent.
func (s *MemoryStateStore) Load(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, ok := s.m.Load(key)
	if !ok {
		return nil, ErrStateNotFound
	}
	src := v.([]byte)
	cp := make([]byte, len(src))
	copy(cp, src)
	return cp, nil
}

// Delete removes the payload stored under key. It is a no-op when the
// key is absent.
func (s *MemoryStateStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.m.Delete(key)
	return nil
}

// Compile-time guarantee that MemoryStateStore satisfies StateStore.
var _ StateStore = (*MemoryStateStore)(nil)
