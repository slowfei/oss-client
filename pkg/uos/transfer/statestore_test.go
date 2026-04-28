package transfer

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryStateStore_SaveLoadDelete(t *testing.T) {
	t.Parallel()

	s := NewMemoryStateStore()
	ctx := context.Background()
	if _, err := s.Load(ctx, "missing"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("Load on missing key: want ErrStateNotFound, got %v", err)
	}

	payload := []byte("hello")
	if err := s.Save(ctx, "k", payload); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, "k")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Load: bytes mismatch want=%q got=%q", payload, got)
	}

	// Mutate the original after Save: stored copy must be unaffected.
	payload[0] = 'X'
	got2, _ := s.Load(ctx, "k")
	if got2[0] == 'X' {
		t.Fatalf("Save did not defensive-copy: got %q", got2)
	}

	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx, "k"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("Load after Delete: want ErrStateNotFound, got %v", err)
	}
	// Delete on missing key is a no-op.
	if err := s.Delete(ctx, "absent"); err != nil {
		t.Fatalf("Delete on absent key: %v", err)
	}
}

func TestMemoryStateStore_ContextCancellation(t *testing.T) {
	t.Parallel()

	s := NewMemoryStateStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Save(ctx, "k", []byte("v")); err == nil {
		t.Fatal("Save with cancelled ctx: want error, got nil")
	}
	if _, err := s.Load(ctx, "k"); err == nil {
		t.Fatal("Load with cancelled ctx: want error, got nil")
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Fatal("Delete with cancelled ctx: want error, got nil")
	}
}

func TestMemoryStateStore_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	s := NewMemoryStateStore()
	ctx := context.Background()
	const writers = 8
	const ops = 200

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				key := "k"
				if err := s.Save(ctx, key, []byte{byte(id), byte(j)}); err != nil {
					t.Errorf("Save: %v", err)
					return
				}
				if _, err := s.Load(ctx, key); err != nil && !errors.Is(err, ErrStateNotFound) {
					t.Errorf("Load: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
