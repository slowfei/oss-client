package contract

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/maqian/object-storage-client/pkg/uos"
)

// runObjectCases enumerates the ObjectService contract cases. Each
// case asserts a single unified-API guarantee and t.Skips when the
// driver opts out via FactoryUnderTest.SkipCases.
func runObjectCases(t *testing.T, fut FactoryUnderTest) {
	t.Helper()

	runCase(t, fut, "put_get_round_trip", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		key := "small.txt"
		body := []byte("hello, world")
		if _, err := c.Objects(fut.Bucket).Put(ctx, uos.PutObjectRequest{
			Bucket: fut.Bucket, Key: key,
			Body: bytes.NewReader(body), Size: int64(len(body)),
			Content: uos.ContentHeaders{ContentType: "text/plain"},
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		r, err := c.Objects(fut.Bucket).Get(ctx, uos.GetObjectRequest{Bucket: fut.Bucket, Key: key})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer r.Body.Close()
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("body mismatch: want %q got %q", body, got)
		}
	})

	runCase(t, fut, "put_metadata_head_round_trip", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		key := "metadata.txt"
		md := uos.Metadata{"x-trace-id": "abc-123", "owner": "team"}
		body := []byte("payload")
		if _, err := c.Objects(fut.Bucket).Put(ctx, uos.PutObjectRequest{
			Bucket: fut.Bucket, Key: key,
			Body: bytes.NewReader(body), Size: int64(len(body)),
			Content:  uos.ContentHeaders{ContentType: "application/octet-stream"},
			Metadata: md,
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		info, err := c.Objects(fut.Bucket).Head(ctx, uos.HeadObjectRequest{Bucket: fut.Bucket, Key: key})
		if err != nil {
			t.Fatalf("Head: %v", err)
		}
		// Drivers MUST lower-case keys per the request.go contract.
		for k, v := range md {
			if got := info.Metadata[strings.ToLower(k)]; got != v {
				t.Fatalf("Metadata[%q]: want %q got %q", k, v, got)
			}
		}
	})

	runCase(t, fut, "put_get_special_char_key", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		// Keys with #?&%/ catch double-encoding bugs (cross-cutting risk).
		key := "weird/key with spaces #1?a=2&b=%FF"
		body := []byte("special")
		if _, err := c.Objects(fut.Bucket).Put(ctx, uos.PutObjectRequest{
			Bucket: fut.Bucket, Key: key,
			Body: bytes.NewReader(body), Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		r, err := c.Objects(fut.Bucket).Get(ctx, uos.GetObjectRequest{Bucket: fut.Bucket, Key: key})
		if err != nil {
			t.Fatalf("Get with special key: %v", err)
		}
		defer r.Body.Close()
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) {
			t.Fatalf("body mismatch on special-char key: want %q got %q", body, got)
		}
	})

	runCase(t, fut, "get_range_returns_slice", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		key := "ranged.bin"
		full := []byte("0123456789abcdef")
		if _, err := c.Objects(fut.Bucket).Put(ctx, uos.PutObjectRequest{
			Bucket: fut.Bucket, Key: key,
			Body: bytes.NewReader(full), Size: int64(len(full)),
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		r, err := c.Objects(fut.Bucket).Get(ctx, uos.GetObjectRequest{
			Bucket: fut.Bucket, Key: key,
			Range: &uos.ByteRange{Start: 4, End: 9},
		})
		if err != nil {
			t.Fatalf("Get with range: %v", err)
		}
		defer r.Body.Close()
		got, _ := io.ReadAll(r.Body)
		want := full[4:10]
		if !bytes.Equal(got, want) {
			t.Fatalf("range bytes: want %q got %q", want, got)
		}
	})

	runCase(t, fut, "head_missing_returns_not_found", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		_, err := c.Objects(fut.Bucket).Head(ctx, uos.HeadObjectRequest{
			Bucket: fut.Bucket, Key: "this-key-does-not-exist",
		})
		if err == nil {
			t.Fatal("Head missing object: want ErrNotFound, got nil")
		}
		if !errors.Is(err, &uos.Error{Code: uos.ErrNotFound}) {
			t.Fatalf("Head missing object: want ErrNotFound, got %v", err)
		}
	})

	runCase(t, fut, "exists_missing_returns_false_no_error", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		ok, err := c.Objects(fut.Bucket).Exists(ctx, uos.HeadObjectRequest{
			Bucket: fut.Bucket, Key: "exists-missing",
		})
		if err != nil {
			t.Fatalf("Exists: want nil error, got %v", err)
		}
		if ok {
			t.Fatal("Exists: want false for missing object, got true")
		}
	})

	runCase(t, fut, "delete_many_partial_success", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		// Use a small set; the contract is the partial-success shape, not
		// the exact 1500-key stress test.
		keys := []string{"dm-a", "dm-b", "dm-c"}
		for _, k := range keys {
			if _, err := c.Objects(fut.Bucket).Put(ctx, uos.PutObjectRequest{
				Bucket: fut.Bucket, Key: k,
				Body: bytes.NewReader([]byte("x")), Size: 1,
			}); err != nil {
				t.Fatalf("seed Put %q: %v", k, err)
			}
		}
		res, err := c.Objects(fut.Bucket).DeleteMany(ctx, uos.DeleteManyRequest{
			Bucket: fut.Bucket, Keys: keys,
		})
		if err != nil {
			t.Fatalf("DeleteMany: %v", err)
		}
		if got, want := len(res.Deleted)+len(res.Failed), len(keys); got != want {
			t.Fatalf("DeleteMany: total entries %d != %d", got, want)
		}
	})

	runCase(t, fut, "copy_same_bucket_round_trip", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		src, dst := "copy-src", "copy-dst"
		body := []byte("copy me")
		if _, err := c.Objects(fut.Bucket).Put(ctx, uos.PutObjectRequest{
			Bucket: fut.Bucket, Key: src,
			Body: bytes.NewReader(body), Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("seed Put: %v", err)
		}
		if _, err := c.Objects(fut.Bucket).Copy(ctx, uos.CopyObjectRequest{
			SourceBucket: fut.Bucket, SourceKey: src,
			DestBucket: fut.Bucket, DestKey: dst,
		}); err != nil {
			t.Fatalf("Copy: %v", err)
		}
		info, err := c.Objects(fut.Bucket).Head(ctx, uos.HeadObjectRequest{Bucket: fut.Bucket, Key: dst})
		if err != nil {
			t.Fatalf("Head copy: %v", err)
		}
		if info.Size != int64(len(body)) {
			t.Fatalf("copy size: want %d got %d", len(body), info.Size)
		}
	})

	runCase(t, fut, "list_with_prefix_delimiter_pagination", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		// Seed three keys: two under "a/" and one under "b/".
		keys := []string{"a/1", "a/2", "b/1"}
		for _, k := range keys {
			if _, err := c.Objects(fut.Bucket).Put(ctx, uos.PutObjectRequest{
				Bucket: fut.Bucket, Key: k,
				Body: bytes.NewReader([]byte("x")), Size: 1,
			}); err != nil {
				t.Fatalf("seed Put %q: %v", k, err)
			}
		}
		out, err := c.Objects(fut.Bucket).List(ctx, uos.ListObjectsRequest{
			Bucket: fut.Bucket, Prefix: "a/",
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(out.Items) < 2 {
			t.Fatalf("List with prefix=a/: want >=2 items, got %d", len(out.Items))
		}
	})
}

// ensureBucket creates the bucket if missing and registers a cleanup.
// Drivers' Create may return ErrAlreadyExists for a pre-existing bucket;
// that's not a test failure here.
func ensureBucket(t *testing.T, c uos.Client, name string) {
	t.Helper()
	ctx := context.Background()
	_, err := c.Buckets().Create(ctx, uos.CreateBucketRequest{Name: name})
	if err != nil && !errors.Is(err, &uos.Error{Code: uos.ErrAlreadyExists}) {
		t.Fatalf("ensureBucket %q: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.Buckets().Delete(context.Background(), uos.DeleteBucketRequest{Name: name})
	})
}
