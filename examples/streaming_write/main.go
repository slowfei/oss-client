// streaming_write demonstrates pkg/uos/streamio.Writer streaming a new
// object end-to-end against any S3-compatible endpoint. The example
// generates 12 MiB of synthetic log data, streams it through a Writer
// (which auto-promotes to multipart upload at the 5 MiB part boundary),
// then reads the object back and verifies byte-for-byte integrity.
//
// Run against a local MinIO container (no cloud account needed):
//
//	docker run -d --rm --name omc-minio -p 9000:9000 \
//	    -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
//	    minio/minio server /data
//	go run ./examples/streaming_write
//	docker stop omc-minio
//
// Run against any S3-compatible endpoint via env vars:
//
//	export OMC_STREAM_PROVIDER=aws
//	export OMC_STREAM_REGION=us-east-1
//	export OMC_STREAM_ENDPOINT=
//	export OMC_STREAM_BUCKET=my-bucket
//	export OMC_STREAM_KEY=AKIA...
//	export OMC_STREAM_SECRET=...
//	go run ./examples/streaming_write
//
// Defaults target a local MinIO at http://localhost:9000.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"
	"github.com/slowfei/oss-client/pkg/uos/streamio"

	_ "github.com/slowfei/oss-client/providers/aws"
	_ "github.com/slowfei/oss-client/providers/minio"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	provider := envOr("OMC_STREAM_PROVIDER", "minio")
	region := envOr("OMC_STREAM_REGION", "us-east-1")
	endpoint := envOr("OMC_STREAM_ENDPOINT", "http://localhost:9000")
	bucket := envOr("OMC_STREAM_BUCKET", fmt.Sprintf("uos-stream-%d", time.Now().Unix()))
	key := envOr("OMC_STREAM_KEY_NAME", "stream-demo.log")
	access := envOr("OMC_STREAM_KEY", "minioadmin")
	secret := envOr("OMC_STREAM_SECRET", "minioadmin")

	cli, err := uos.DefaultRegistry().Open(ctx, uos.Config{
		Provider: uos.Provider(provider),
		Region:   region,
		Endpoint: endpoint,
		CredentialProvider: credential.NewStatic(credential.Credential{
			Scheme: credential.AuthHMAC,
			Opaque: &credential.EnvHMACCredential{
				AccessKeyID:     access,
				SecretAccessKey: secret,
			},
		}),
	})
	must(err, "Open")
	defer cli.Close()

	fmt.Printf("opened %s client → endpoint=%s bucket=%s key=%s\n",
		provider, endpoint, bucket, key)

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

	// Streaming write: 12 MiB of synthetic log lines through a Writer.
	// streamio.Writer auto-promotes to multipart at the 5 MiB boundary,
	// producing 2 full parts (5 MiB each) + 1 final part (2 MiB).
	w, err := streamio.NewWriter(ctx, cli, bucket, key, streamio.WriterOptions{
		ContentType: "text/plain",
		Metadata:    uos.Metadata{"source": "streaming_write_demo"},
	})
	must(err, "NewWriter")
	defer func() {
		if err := cli.Objects(bucket).Delete(context.Background(), uos.DeleteObjectRequest{Key: key}); err != nil {
			fmt.Printf("(cleanup) object delete: %v\n", err)
		}
	}()

	const targetBytes = 12 * 1024 * 1024 // 12 MiB → 3 parts (5 + 5 + 2)
	hash := sha256.New()
	written := 0
	lineNum := 0
	for written < targetBytes {
		line := fmt.Sprintf("line=%010d ts=%d payload=%s\n",
			lineNum, time.Now().UnixNano(), bytes.Repeat([]byte("x"), 64))
		if _, err := w.Write([]byte(line)); err != nil {
			_ = w.Abort()
			log.Fatalf("Write line %d: %v", lineNum, err)
		}
		hash.Write([]byte(line))
		written += len(line)
		lineNum++
	}
	wantSum := hash.Sum(nil)
	fmt.Printf("streamed: %d bytes across %d log lines (sha256=%x)\n",
		written, lineNum, wantSum[:8])

	if err := w.Close(); err != nil {
		log.Fatalf("Close: %v", err)
	}
	fmt.Println("close: multipart complete")

	// Read back and verify byte-for-byte.
	got, err := cli.Objects(bucket).Get(ctx, uos.GetObjectRequest{Key: key})
	must(err, "Get")
	defer got.Body.Close()

	gotHash := sha256.New()
	gotBytes, err := io.Copy(gotHash, got.Body)
	must(err, "ReadBack")
	gotSum := gotHash.Sum(nil)

	if gotBytes != int64(written) {
		log.Fatalf("size mismatch: wrote %d, read %d", written, gotBytes)
	}
	if !bytes.Equal(gotSum, wantSum) {
		log.Fatalf("checksum mismatch: wrote sha256=%x, read sha256=%x", wantSum, gotSum)
	}
	fmt.Printf("verify: %d bytes round-tripped, sha256 matches (%x)\n",
		gotBytes, gotSum[:8])

	fmt.Println("streaming_write OK — cleanup runs on defer")
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
