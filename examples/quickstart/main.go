// quickstart demonstrates the unified pkg/uos surface end-to-end against
// any S3-compatible endpoint. It opens a Client, creates a bucket, puts
// + gets + signs + deletes a small object, then tears the bucket down.
//
// Run against a local MinIO container (no cloud account needed):
//
//	docker run -d --rm --name omc-minio -p 9000:9000 \
//	    -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
//	    minio/minio server /data
//	go run ./examples/quickstart
//	docker stop omc-minio
//
// Run against a real cloud (e.g. real AWS S3) by setting env vars:
//
//	export OMC_QUICKSTART_PROVIDER=aws
//	export OMC_QUICKSTART_REGION=us-east-1
//	export OMC_QUICKSTART_ENDPOINT=
//	export OMC_QUICKSTART_BUCKET=my-bucket-name-here
//	export OMC_QUICKSTART_KEY=AKIA...
//	export OMC_QUICKSTART_SECRET=...
//	go run ./examples/quickstart
//
// Defaults (unset env vars) target a local MinIO at http://localhost:9000
// with the canonical minioadmin/minioadmin credentials.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/credential"

	// Side-effect imports register provider Factories on uos.DefaultRegistry.
	// Pull only the providers your application uses.
	_ "github.com/maqian/object-storage-client/providers/aws"
	_ "github.com/maqian/object-storage-client/providers/minio"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	provider := envOr("OMC_QUICKSTART_PROVIDER", "minio")
	region := envOr("OMC_QUICKSTART_REGION", "us-east-1")
	endpoint := envOr("OMC_QUICKSTART_ENDPOINT", "http://localhost:9000")
	bucket := envOr("OMC_QUICKSTART_BUCKET", fmt.Sprintf("uos-quickstart-%d", time.Now().Unix()))
	key := envOr("OMC_QUICKSTART_OBJECT_KEY", "hello.txt")
	access := envOr("OMC_QUICKSTART_KEY", "minioadmin")
	secret := envOr("OMC_QUICKSTART_SECRET", "minioadmin")

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

	// Create bucket (ignore "already exists" — quickstart-friendly).
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

	// PutObject.
	body := "hello from pkg/uos quickstart — the unified Go SDK across 10 providers"
	put, err := cli.Objects(bucket).Put(ctx, uos.PutObjectRequest{
		Key:      key,
		Body:     strings.NewReader(body),
		Size:     int64(len(body)),
		Content:  uos.ContentHeaders{ContentType: "text/plain"},
		Metadata: uos.Metadata{"source": "quickstart"},
	})
	must(err, "Put")
	fmt.Printf("put: etag=%q size=%d\n", put.ETag, len(body))

	defer func() {
		if err := cli.Objects(bucket).Delete(context.Background(), uos.DeleteObjectRequest{Key: key}); err != nil {
			fmt.Printf("(cleanup) object delete: %v\n", err)
		}
	}()

	// GetObject.
	got, err := cli.Objects(bucket).Get(ctx, uos.GetObjectRequest{Key: key})
	must(err, "Get")
	defer got.Body.Close()
	gotBody, err := io.ReadAll(got.Body)
	must(err, "ReadAll")
	if !bytes.Equal(gotBody, []byte(body)) {
		log.Fatalf("Get body mismatch: want %q got %q", body, string(gotBody))
	}
	fmt.Printf("get: %d bytes round-tripped, content-type=%q, metadata=%v\n",
		len(gotBody), got.Info.Content.ContentType, got.Info.Metadata)

	// SignURL — presigned GET URL valid for 5 minutes.
	signed, err := cli.Signer(bucket).SignURL(ctx, uos.SignURLRequest{
		Method:    "GET",
		Key:       key,
		ExpiresIn: 5 * time.Minute,
	})
	if err != nil {
		// Some providers (qiniu Write, upyun Write) return ErrUnsupported
		// for SignURL — that's a documented driver-side fence, not a bug.
		fmt.Printf("sign: skipped (%v)\n", err)
	} else {
		fmt.Printf("sign: %s (expires %s)\n", truncate(signed.URL, 80), signed.ExpiresAt.Format(time.RFC3339))
	}

	// Capabilities — what does this driver promise?
	report, err := cli.Capabilities(ctx)
	must(err, "Capabilities")
	fmt.Printf("capabilities: %d cells reported (use docs/provider_matrix.md for the visual breakdown)\n",
		len(report.Items))

	fmt.Println("quickstart OK — cleanup runs on defer")
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
