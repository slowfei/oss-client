package contract

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/maqian/oss-client/pkg/uos"
)

// runMultipartCases enumerates the MultipartService contract cases.
// The cases use the raw multipart primitives (not transfer.Manager) so
// they pin the driver-side Initiate/UploadPart/Complete/Abort contract
// directly.
func runMultipartCases(t *testing.T, fut FactoryUnderTest) {
	t.Helper()

	runCase(t, fut, "initiate_upload_complete_get", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		key := "mp-end-to-end"
		mp := c.Multipart(fut.Bucket)
		up, err := mp.Initiate(ctx, uos.InitiateMultipartRequest{Bucket: fut.Bucket, Key: key})
		if err != nil {
			t.Fatalf("Initiate: %v", err)
		}
		// 3 parts of random bytes; 5 MiB minimum part size for S3-family
		// providers, but the suite pins the contract not the threshold.
		// Use 5 MiB to match the typical vendor minimum.
		const partSize = 5 * 1024 * 1024
		var allBytes bytes.Buffer
		var parts []uos.UploadedPart
		for i := 1; i <= 3; i++ {
			body := make([]byte, partSize)
			if _, err := rand.Read(body); err != nil {
				t.Fatalf("rand: %v", err)
			}
			allBytes.Write(body)
			res, err := mp.UploadPart(ctx, uos.UploadPartRequest{
				Bucket: fut.Bucket, Key: key, UploadID: up.UploadID,
				PartNumber: i, Body: bytes.NewReader(body), Size: partSize,
			})
			if err != nil {
				t.Fatalf("UploadPart %d: %v", i, err)
			}
			parts = append(parts, *res)
		}
		if _, err := mp.Complete(ctx, uos.CompleteMultipartRequest{
			Bucket: fut.Bucket, Key: key, UploadID: up.UploadID, Parts: parts,
		}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		r, err := c.Objects(fut.Bucket).Get(ctx, uos.GetObjectRequest{Bucket: fut.Bucket, Key: key})
		if err != nil {
			t.Fatalf("Get assembled: %v", err)
		}
		defer r.Body.Close()
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, allBytes.Bytes()) {
			t.Fatal("assembled object body mismatch")
		}
	})

	runCase(t, fut, "resume_after_failure", func(t *testing.T, c uos.Client) {
		// In M1 this case body is necessarily a t.Skip until the resume
		// scenario can be exercised against a real driver — the suite
		// pins the case name and shape here so M2 can drop a body in.
		t.Skip("requires StateStore + driver wiring; exercised in M2")
	})

	runCase(t, fut, "abort_cleans_up_orphan", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		key := "mp-aborted"
		mp := c.Multipart(fut.Bucket)
		up, err := mp.Initiate(ctx, uos.InitiateMultipartRequest{Bucket: fut.Bucket, Key: key})
		if err != nil {
			t.Fatalf("Initiate: %v", err)
		}
		if err := mp.Abort(ctx, uos.AbortMultipartRequest{Bucket: fut.Bucket, Key: key, UploadID: up.UploadID}); err != nil {
			t.Fatalf("Abort: %v", err)
		}
		out, err := mp.List(ctx, uos.ListMultipartUploadsRequest{Bucket: fut.Bucket, Prefix: key})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, u := range out.Uploads {
			if u.UploadID == up.UploadID {
				t.Fatalf("aborted upload %q still listed in bucket", up.UploadID)
			}
		}
	})

	runCase(t, fut, "complete_with_unknown_upload_returns_invalid_argument", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		_, err := c.Multipart(fut.Bucket).Complete(ctx, uos.CompleteMultipartRequest{
			Bucket: fut.Bucket, Key: "no-such-key", UploadID: "no-such-upload",
			Parts: []uos.UploadedPart{{PartNumber: 1, ETag: "x"}},
		})
		if err == nil {
			t.Fatal("Complete with bogus upload: want error, got nil")
		}
		// Vendors disagree on the exact code (NotFound vs InvalidArgument vs Conflict).
		// The contract is "drivers MUST surface a *uos.Error", not a
		// specific code; tolerate any of the three.
		var ue *uos.Error
		if !errors.As(err, &ue) {
			t.Fatalf("Complete bogus upload: want *uos.Error, got %v", err)
		}
		switch ue.Code {
		case uos.ErrNotFound, uos.ErrInvalidArgument, uos.ErrConflict:
			// expected
		default:
			t.Fatalf("Complete bogus upload: unexpected code %q", ue.Code)
		}
	})
}
