package credential

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// fakeProvider is a Provider stub that returns a canned (cred, err) pair.
type fakeProvider struct {
	cred  Credential
	err   error
	calls atomic.Int64
}

func (f *fakeProvider) Resolve(_ context.Context, _ string) (Credential, error) {
	f.calls.Add(1)
	return f.cred, f.err
}

func TestChain_Empty(t *testing.T) {
	t.Parallel()
	c := NewChain()
	_, err := c.Resolve(context.Background(), "any")
	if !errors.Is(err, ErrChainEmpty) {
		t.Fatalf("expected ErrChainEmpty, got %v", err)
	}
}

func TestChain_NilEntriesDropped(t *testing.T) {
	t.Parallel()
	c := NewChain(nil, nil, nil)
	_, err := c.Resolve(context.Background(), "any")
	if !errors.Is(err, ErrChainEmpty) {
		t.Fatalf("expected ErrChainEmpty after nil-drop, got %v", err)
	}
	if len(c.Providers) != 0 {
		t.Fatalf("expected nil entries to be filtered, got %d entries", len(c.Providers))
	}
}

func TestChain_FirstSuccessWins(t *testing.T) {
	t.Parallel()
	want := Credential{Scheme: AuthHMAC, Opaque: "first"}
	first := &fakeProvider{cred: want}
	second := &fakeProvider{cred: Credential{Scheme: AuthAnonymous}}
	c := NewChain(first, second)
	got, err := c.Resolve(context.Background(), "x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Opaque != "first" {
		t.Errorf("expected first provider's credential, got %v", got.Opaque)
	}
	if first.calls.Load() != 1 {
		t.Errorf("first.calls = %d, want 1", first.calls.Load())
	}
	if second.calls.Load() != 0 {
		t.Errorf("second.calls = %d, want 0 (short-circuit on success)", second.calls.Load())
	}
}

func TestChain_FallsThroughOnError(t *testing.T) {
	t.Parallel()
	first := &fakeProvider{err: errors.New("first failed")}
	second := &fakeProvider{cred: Credential{Scheme: AuthHMAC, Opaque: "second"}}
	c := NewChain(first, second)
	got, err := c.Resolve(context.Background(), "x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Opaque != "second" {
		t.Errorf("expected second provider's credential, got %v", got.Opaque)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Errorf("calls first=%d second=%d, want 1/1",
			first.calls.Load(), second.calls.Load())
	}
}

func TestChain_AllFail_JoinsErrors(t *testing.T) {
	t.Parallel()
	errA := errors.New("A bad")
	errB := errors.New("B bad")
	c := NewChain(
		&fakeProvider{err: errA},
		&fakeProvider{err: errB},
	)
	_, err := c.Resolve(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error when every provider fails")
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Errorf("expected joined error to wrap both child errors, got %v", err)
	}
}

func TestChain_ConcurrentResolve(t *testing.T) {
	t.Parallel()
	p := &fakeProvider{cred: Credential{Scheme: AuthAnonymous}}
	c := NewChain(p)
	const N = 50
	done := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, err := c.Resolve(context.Background(), "x")
			done <- err
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent Resolve: %v", err)
		}
	}
	if got := p.calls.Load(); got != int64(N) {
		t.Errorf("calls = %d, want %d", got, N)
	}
}
