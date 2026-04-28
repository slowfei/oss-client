// multipart demonstrates the unified pkg/uos MultipartService end-to-end
// against any S3-compatible endpoint. It opens a Client, creates a bucket,
// initiates a multipart upload, streams 3 × 5 MiB parts, completes the
// upload, verifies via Head, exercises the Abort path, and tears everything
// down on exit.
//
// Run against a local MinIO container (no cloud account needed):
//
//	docker run -d --rm --name omc-minio -p 9000:9000 \
//	    -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
//	    minio/minio server /data
//	go run ./examples/multipart
//	docker stop omc-minio
//
// Run against a real cloud (e.g. real AWS S3) by setting env vars:
//
//	export OMC_MULTIPART_PROVIDER=aws
//	export OMC_MULTIPART_REGION=us-east-1
//	export OMC_MULTIPART_ENDPOINT=
//	export OMC_MULTIPART_BUCKET=my-bucket-name-here
//	export OMC_MULTIPART_KEY=AKIA...
//	export OMC_MULTIPART_SECRET=...
//	go run ./examples/multipart
//
// Defaults (unset env vars) target a local MinIO at http://localhost:9000
// with the canonical minioadmin/minioadmin credentials, an auto-generated
// bucket name (uos-multipart-<unix-timestamp>), and the object key
// multipart-demo.bin.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/credential"

	// Side-effect imports register provider Factories on uos.DefaultRegistry.
	// Pull only the providers your application uses.
	_ "github.com/maqian/object-storage-client/providers/aws"
	_ "github.com/maqian/object-storage-client/providers/minio"
)

const (
	// partSize is 5 MiB — the S3 minimum part size for non-final parts.
	partSize = 5 * 1024 * 1024
	// numParts is the number of parts to upload (total body = 15 MiB).
	numParts = 3
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provider := envOr("OMC_MULTIPART_PROVIDER", "minio")
	region := envOr("OMC_MULTIPART_REGION", "us-east-1")
	endpoint := envOr("OMC_MULTIPART_ENDPOINT", "http://localhost:9000")
	bucket := envOr("OMC_MULTIPART_BUCKET", fmt.Sprintf("uos-multipart-%d", time.Now().Unix()))
	key := envOr("OMC_MULTIPART_OBJECT_KEY", "multipart-demo.bin")
	access := envOr("OMC_MULTIPART_KEY", "minioadmin")
	secret := envOr("OMC_MULTIPART_SECRET", "minioadmin")

	creds := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthHMAC,
		Opaque: &credential.EnvHMACCredential{
			AccessKeyID:     access,
			SecretAccessKey: secret,
		},
	})
	cfg := uos.Config{
		Provider:           uos.Provider(provider),
		Region:             region,
		Endpoint:           endpoint,
		CredentialProvider: creds,
	}

	cli, err := uos.DefaultRegistry().Open(ctx, cfg)
	must(err, "Open")
	defer cli.Close()

	fmt.Printf("opened %s client → endpoint=%s bucket=%s key=%s\n",
		provider, endpoint, bucket, key)

	// ── Create bucket (idempotent — ignore ErrAlreadyExists) ─────────────────
	if _, err := cli.Buckets().Create(ctx, uos.CreateBucketRequest{Name: bucket}); err != nil {
		if !isAlreadyExists(err) {
			must(err, "BucketCreate")
		}
		fmt.Println("bucket already exists, reusing")
	} else {
		fmt.Println("bucket created")
	}
	defer func() {
		if err := cli.Buckets().Delete(context.Background(), uos.DeleteBucketRequest{Name: bucket}); err != nil {
			fmt.Printf("(cleanup) bucket delete: %v\n", err)
		}
	}()

	// ── Happy path: Initiate → UploadPart × 3 → Complete ────────────────────
	svc := cli.Multipart(bucket)

	init, err := svc.Initiate(ctx, uos.InitiateMultipartRequest{
		Bucket:  bucket,
		Key:     key,
		Content: uos.ContentHeaders{ContentType: "application/octet-stream"},
		Metadata: uos.Metadata{
			"source": "multipart-example",
			"parts":  fmt.Sprintf("%d", numParts),
		},
	})
	must(err, "Initiate")
	fmt.Printf("initiated: uploadID=%q\n", init.UploadID)

	// Register deferred cleanup in case something goes wrong before Complete.
	// Complete or Abort will both supersede this; the double-abort is harmless
	// because Abort is idempotent on most vendors.
	abortOnce := true
	defer func() {
		if abortOnce {
			_ = svc.Abort(context.Background(), uos.AbortMultipartRequest{
				Bucket:   bucket,
				Key:      key,
				UploadID: init.UploadID,
			})
		}
	}()

	// Upload 3 parts of 5 MiB each (all-zeros payload — demo only).
	partData := make([]byte, partSize)
	var parts []uos.UploadedPart

	for i := 1; i <= numParts; i++ {
		uploaded, err := svc.UploadPart(ctx, uos.UploadPartRequest{
			Bucket:     bucket,
			Key:        key,
			UploadID:   init.UploadID,
			PartNumber: i,
			Body:       bytes.NewReader(partData),
			Size:       partSize,
		})
		must(err, fmt.Sprintf("UploadPart %d", i))
		fmt.Printf("  part %d uploaded: etag=%q size=%d\n", i, uploaded.ETag, uploaded.Size)
		parts = append(parts, uos.UploadedPart{
			PartNumber: uploaded.PartNumber,
			ETag:       uploaded.ETag,
		})
	}

	// Complete: stitch parts in order, get final ETag + size.
	result, err := svc.Complete(ctx, uos.CompleteMultipartRequest{
		Bucket:   bucket,
		Key:      key,
		UploadID: init.UploadID,
		Parts:    parts,
	})
	must(err, "Complete")
	abortOnce = false // upload is committed; suppress deferred abort
	fmt.Printf("complete: etag=%q versionID=%q\n", result.ETag, result.VersionID)

	defer func() {
		if err := cli.Objects(bucket).Delete(context.Background(), uos.DeleteObjectRequest{Key: key}); err != nil {
			fmt.Printf("(cleanup) object delete: %v\n", err)
		}
	}()

	// ── Verify via Head ──────────────────────────────────────────────────────
	info, err := cli.Objects(bucket).Head(ctx, uos.HeadObjectRequest{
		Bucket: bucket,
		Key:    key,
	})
	must(err, "Head")
	fmt.Printf("head: size=%d content-type=%q metadata=%v\n",
		info.Size, info.Content.ContentType, info.Metadata)

	// ── List in-flight uploads (should be empty after Complete) ──────────────
	// NOTE: MultipartService.List returns in-process sessions only for
	// non-S3 drivers (gcs, azure, qiniu, upyun). On S3-compatible drivers
	// (aws, minio) it reflects the vendor-side list and will show an empty
	// page here because the upload has already been completed.
	listing, err := svc.List(ctx, uos.ListMultipartUploadsRequest{
		Bucket: bucket,
		Prefix: key,
	})
	must(err, "List")
	fmt.Printf("list after complete: %d in-flight uploads (expect 0 on S3-compatible)\n",
		len(listing.Uploads))

	// ── Abort path: Initiate → UploadPart 1 → Abort ─────────────────────────
	abortKey := key + ".abort-demo"
	abortInit, err := svc.Initiate(ctx, uos.InitiateMultipartRequest{
		Bucket: bucket,
		Key:    abortKey,
	})
	must(err, "Initiate (abort demo)")
	fmt.Printf("abort demo: initiated uploadID=%q\n", abortInit.UploadID)

	abortPart, err := svc.UploadPart(ctx, uos.UploadPartRequest{
		Bucket:     bucket,
		Key:        abortKey,
		UploadID:   abortInit.UploadID,
		PartNumber: 1,
		Body:       bytes.NewReader(partData[:1024]),
		Size:       1024,
	})
	must(err, "UploadPart (abort demo)")
	fmt.Printf("abort demo: part 1 etag=%q\n", abortPart.ETag)

	err = svc.Abort(ctx, uos.AbortMultipartRequest{
		Bucket:   bucket,
		Key:      abortKey,
		UploadID: abortInit.UploadID,
	})
	must(err, "Abort")
	fmt.Println("abort demo: upload aborted")

	// Confirm the aborted upload is no longer listed.
	afterAbort, err := svc.List(ctx, uos.ListMultipartUploadsRequest{
		Bucket: bucket,
		Prefix: abortKey,
	})
	must(err, "List (after abort)")
	fmt.Printf("abort demo: %d in-flight uploads after abort (expect 0)\n",
		len(afterAbort.Uploads))

	// Confirm the object was not committed (Head should return ErrNotFound).
	_, headErr := cli.Objects(bucket).Head(ctx, uos.HeadObjectRequest{
		Bucket: bucket,
		Key:    abortKey,
	})
	if isNotFound(headErr) {
		fmt.Println("abort demo: confirmed — aborted key absent (ErrNotFound)")
	} else if headErr != nil {
		fmt.Printf("abort demo: unexpected head error: %v\n", headErr)
	} else {
		fmt.Println("abort demo: WARNING — object exists after abort (vendor may have committed it)")
	}

	fmt.Println("multipart OK — cleanup runs on defer")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func must(err error, op string) {
	if err == nil {
		return
	}
	log.Fatalf("%s: %v", op, err)
}

func isAlreadyExists(err error) bool {
	var uerr *uos.Error
	return errors.As(err, &uerr) && uerr.Code == uos.ErrAlreadyExists
}

func isNotFound(err error) bool {
	var uerr *uos.Error
	return errors.As(err, &uerr) && uerr.Code == uos.ErrNotFound
}
