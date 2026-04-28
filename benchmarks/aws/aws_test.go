//go:build docker

// Package awsbench measures per-operation throughput for the AWS S3 driver
// (providers/aws) running against a testcontainers-spawned MinIO endpoint.
// AWS and MinIO are the M6 phase 2 baseline (S3-family); per-vendor sweeps
// for the other 8 providers land in M6 phase 3 / v1.0.0.
//
// Run with:
//
//	go test -tags=docker -bench=. -benchmem ./benchmarks/aws
package awsbench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/maqian/oss-client/pkg/testkit/contract"
	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
	awsdrv "github.com/maqian/oss-client/providers/aws"
)

// awsBenchEnv holds the shared state created once per benchmark binary
// invocation. It is populated by setupAWS and torn down via b.Cleanup.
type awsBenchEnv struct {
	client uos.Client
	bucket string
}

// setupAWS spins up a MinIO testcontainer and opens an AWS-driver client
// pointed at it. The container and client are cleaned up via b.Cleanup.
// b.ResetTimer is NOT called here; callers do that after any per-bench setup.
func setupAWS(b *testing.B) *awsBenchEnv {
	b.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	b.Cleanup(cancel)

	endpoint, accessKey, secretKey, cleanup, err := contract.SpawnMinIO(ctx)
	if err != nil {
		b.Fatalf("setupAWS: SpawnMinIO: %v", err)
	}
	b.Cleanup(cleanup)

	endpointURL := normaliseEndpoint(endpoint)

	cred := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthHMAC,
		Opaque: &credential.EnvHMACCredential{
			AccessKeyID:     accessKey,
			SecretAccessKey: secretKey,
		},
	})

	cfg := uos.Config{
		Provider:           "aws",
		Region:             "us-east-1",
		Endpoint:           endpointURL,
		CredentialProvider: cred,
		DriverConfig: &awsdrv.DriverConfig{
			PathStyle:    true,
			DisableHTTPS: true,
		},
	}

	cli, err := awsdrv.Factory().Open(ctx, cfg)
	if err != nil {
		b.Fatalf("setupAWS: Open: %v", err)
	}
	b.Cleanup(func() { _ = cli.Close() })

	const bucket = "bench-aws"
	if _, err := cli.Buckets().Create(ctx, uos.CreateBucketRequest{Name: bucket}); err != nil {
		b.Fatalf("setupAWS: create bucket: %v", err)
	}
	b.Cleanup(func() {
		_ = cli.Buckets().Delete(context.Background(), uos.DeleteBucketRequest{Name: bucket})
	})

	return &awsBenchEnv{client: cli, bucket: bucket}
}

// BenchmarkPut_Small measures repeated Put of a 4 KiB body to a unique key
// per iteration. b.SetBytes lets the framework report MB/s alongside ns/op.
func BenchmarkPut_Small(b *testing.B) {
	env := setupAWS(b)
	body := bytes.Repeat([]byte("x"), 4*1024) // 4 KiB

	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-put-small-%d", i)
		_, err := env.client.Objects(env.bucket).Put(context.Background(), uos.PutObjectRequest{
			Key:  key,
			Body: bytes.NewReader(body),
			Size: int64(len(body)),
		})
		if err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
}

// BenchmarkGet_Small uploads a single 4 KiB object once at setup, then
// measures repeated Get of that same key. This isolates the read path
// (HTTP round-trip + body drain) from write overhead.
func BenchmarkGet_Small(b *testing.B) {
	env := setupAWS(b)
	body := bytes.Repeat([]byte("x"), 4*1024) // 4 KiB
	const key = "bench-get-small"

	ctx := context.Background()
	if _, err := env.client.Objects(env.bucket).Put(ctx, uos.PutObjectRequest{
		Key:  key,
		Body: bytes.NewReader(body),
		Size: int64(len(body)),
	}); err != nil {
		b.Fatalf("BenchmarkGet_Small setup Put: %v", err)
	}

	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		r, err := env.client.Objects(env.bucket).Get(ctx, uos.GetObjectRequest{Key: key})
		if err != nil {
			b.Fatalf("Get: %v", err)
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			r.Body.Close()
			b.Fatalf("Get drain: %v", err)
		}
		r.Body.Close()
	}
}

// BenchmarkPut_Medium_1MiB measures repeated Put of a 1 MiB body to a
// unique key per iteration. The larger payload surfaces serialisation and
// TCP throughput costs that are invisible at 4 KiB.
func BenchmarkPut_Medium_1MiB(b *testing.B) {
	env := setupAWS(b)
	body := bytes.Repeat([]byte("m"), 1*1024*1024) // 1 MiB

	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-put-1mib-%d", i)
		_, err := env.client.Objects(env.bucket).Put(context.Background(), uos.PutObjectRequest{
			Key:  key,
			Body: bytes.NewReader(body),
			Size: int64(len(body)),
		})
		if err != nil {
			b.Fatalf("Put 1MiB: %v", err)
		}
	}
}

// BenchmarkMultipart_15MiB measures one full multipart cycle per iteration:
// Initiate + 3 × UploadPart(5 MiB) + Complete. Each iteration uses a unique
// key so concurrent runs don't collide. b.SetBytes reports aggregate bytes
// transferred (15 MiB per cycle).
func BenchmarkMultipart_15MiB(b *testing.B) {
	env := setupAWS(b)
	const partSize = 5 * 1024 * 1024 // 5 MiB — S3 minimum part size
	const totalSize = 3 * partSize   // 15 MiB
	part := bytes.Repeat([]byte("p"), partSize)

	b.SetBytes(totalSize)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-multipart-%d", i)
		ctx := context.Background()

		mu, err := env.client.Multipart(env.bucket).Initiate(ctx, uos.InitiateMultipartRequest{
			Bucket: env.bucket,
			Key:    key,
		})
		if err != nil {
			b.Fatalf("Initiate: %v", err)
		}

		parts := make([]uos.UploadedPart, 3)
		for pn := 1; pn <= 3; pn++ {
			up, err := env.client.Multipart(env.bucket).UploadPart(ctx, uos.UploadPartRequest{
				Bucket:     env.bucket,
				Key:        key,
				UploadID:   mu.UploadID,
				PartNumber: pn,
				Body:       bytes.NewReader(part),
				Size:       partSize,
			})
			if err != nil {
				_ = env.client.Multipart(env.bucket).Abort(ctx, uos.AbortMultipartRequest{
					Bucket:   env.bucket,
					Key:      key,
					UploadID: mu.UploadID,
				})
				b.Fatalf("UploadPart %d: %v", pn, err)
			}
			parts[pn-1] = *up
		}

		if _, err := env.client.Multipart(env.bucket).Complete(ctx, uos.CompleteMultipartRequest{
			Bucket:   env.bucket,
			Key:      key,
			UploadID: mu.UploadID,
			Parts:    parts,
		}); err != nil {
			b.Fatalf("Complete: %v", err)
		}
	}
}

// BenchmarkSignURL_Read measures the pure CPU cost of SigV4 presigned-URL
// generation (no network I/O). The URL is never fetched; this benchmark
// exercises only the aws-sdk-go-v2 signing path.
//
// b.RunParallel is used because SignURL is stateless and benefits from
// concurrency on multi-core machines.
func BenchmarkSignURL_Read(b *testing.B) {
	env := setupAWS(b)
	const key = "bench-sign-url-read"

	// Put a placeholder object so the key exists; the Sign call itself
	// doesn't require this but it keeps the fixture realistic.
	_, _ = env.client.Objects(env.bucket).Put(context.Background(), uos.PutObjectRequest{
		Key:  key,
		Body: strings.NewReader("placeholder"),
		Size: int64(len("placeholder")),
	})

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := env.client.Signer(env.bucket).SignURL(context.Background(), uos.SignURLRequest{
				Key:       key,
				Method:    "GET",
				ExpiresIn: 15 * time.Minute,
			})
			if err != nil {
				b.Errorf("SignURL: %v", err)
			}
		}
	})
}

// normaliseEndpoint accepts either a full URL ("http://host:port") or a
// bare host:port returned by testcontainers' ConnectionString and ensures it
// has an "http://" scheme prefix the AWS SDK can consume.
func normaliseEndpoint(raw string) string {
	if len(raw) >= 7 && raw[:7] == "http://" {
		return raw
	}
	if len(raw) >= 8 && raw[:8] == "https://" {
		return raw
	}
	return "http://" + raw
}
