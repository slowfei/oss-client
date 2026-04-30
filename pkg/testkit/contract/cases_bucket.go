package contract

import (
	"context"
	"errors"
	"testing"

	"github.com/maqian/oss-client/pkg/uos"
)

// runBucketCases enumerates the BucketService contract cases. Each
// case is a t.Run subtest named per architecture_plan §6.4. Bodies
// exercise the round-trip and assert the unified-API guarantees the
// suite is responsible for pinning.
func runBucketCases(t *testing.T, fut FactoryUnderTest) {
	t.Helper()

	// BYOB mode (cloud-nightly): the caller owns the bucket lifecycle.
	// Skipping the create / create-idempotent cases avoids destroying
	// the user's pre-existing bucket via the test's own
	// `bs.Delete(...)` cleanup. The stat_missing_returns_not_found case
	// below is non-destructive (uses a hardcoded missing-bucket name)
	// so it still runs in BYOB mode.
	if fut.BucketIsPreCreated {
		t.Run("create_stat_list_delete", func(t *testing.T) {
			t.Skip("BYOB mode (BucketIsPreCreated=true): suite does not own bucket lifecycle; would destroy caller's bucket via cleanup")
		})
		t.Run("create_idempotency_already_exists", func(t *testing.T) {
			t.Skip("BYOB mode (BucketIsPreCreated=true): suite does not own bucket lifecycle; would destroy caller's bucket via cleanup")
		})
		runCase(t, fut, "stat_missing_returns_not_found", func(t *testing.T, c uos.Client) {
			ctx := context.Background()
			_, err := c.Buckets().Stat(ctx, uos.StatBucketRequest{Name: "uos-contract-missing-bucket"})
			if err == nil {
				t.Fatal("Stat missing bucket: want ErrNotFound, got nil")
			}
			if !errors.Is(err, &uos.Error{Code: uos.ErrNotFound}) {
				t.Fatalf("Stat missing bucket: want ErrNotFound, got %v", err)
			}
		})
		return
	}

	runCase(t, fut, "create_stat_list_delete", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		bs := c.Buckets()

		_, err := bs.Create(ctx, uos.CreateBucketRequest{Name: fut.Bucket})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		t.Cleanup(func() {
			_ = bs.Delete(context.Background(), uos.DeleteBucketRequest{Name: fut.Bucket})
		})

		info, err := bs.Stat(ctx, uos.StatBucketRequest{Name: fut.Bucket})
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info == nil || info.Name != fut.Bucket {
			t.Fatalf("Stat: want bucket %q, got %+v", fut.Bucket, info)
		}

		buckets, err := bs.List(ctx, uos.ListBucketsRequest{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !containsBucket(buckets, fut.Bucket) {
			t.Fatalf("List: bucket %q not present in result", fut.Bucket)
		}

		if err := bs.Delete(ctx, uos.DeleteBucketRequest{Name: fut.Bucket}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	runCase(t, fut, "create_idempotency_already_exists", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		bs := c.Buckets()
		if _, err := bs.Create(ctx, uos.CreateBucketRequest{Name: fut.Bucket}); err != nil {
			t.Fatalf("Create #1: %v", err)
		}
		t.Cleanup(func() {
			_ = bs.Delete(context.Background(), uos.DeleteBucketRequest{Name: fut.Bucket})
		})

		_, err := bs.Create(ctx, uos.CreateBucketRequest{Name: fut.Bucket})
		if err == nil {
			t.Fatal("Create #2: want ErrAlreadyExists, got nil")
		}
		if !errors.Is(err, &uos.Error{Code: uos.ErrAlreadyExists}) {
			t.Fatalf("Create #2: want ErrAlreadyExists, got %v", err)
		}
	})

	runCase(t, fut, "stat_missing_returns_not_found", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		_, err := c.Buckets().Stat(ctx, uos.StatBucketRequest{Name: "uos-contract-missing-bucket"})
		if err == nil {
			t.Fatal("Stat missing bucket: want ErrNotFound, got nil")
		}
		if !errors.Is(err, &uos.Error{Code: uos.ErrNotFound}) {
			t.Fatalf("Stat missing bucket: want ErrNotFound, got %v", err)
		}
	})
}

// containsBucket reports whether items contains a BucketInfo with the
// given Name.
func containsBucket(items []uos.BucketInfo, name string) bool {
	for _, b := range items {
		if b.Name == name {
			return true
		}
	}
	return false
}
