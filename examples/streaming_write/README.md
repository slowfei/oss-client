# streaming_write

A runnable end-to-end demo of [`pkg/uos/streamio.Writer`](../../pkg/uos/streamio/).
Generates 12 MiB of synthetic log lines, streams them into a new object,
then reads the object back and verifies byte-for-byte integrity (sha256
round-trip).

## What it shows

| Step | Behavior |
|---|---|
| `streamio.NewWriter` | Validates args; no I/O. |
| Per `Write` call (small) | Buffers in memory. |
| First `Write` that pushes buffer ≥ 5 MiB | Auto-initiates `MultipartService`; uploads first 5 MiB part. |
| Subsequent `Write` calls hitting 5 MiB boundary | Uploads next part. |
| `Close` (with leftover < 5 MiB) | Uploads remaining as final part; calls `MultipartService.Complete`. |
| Read-back via `Objects.Get` | Streams body via `io.ReadCloser`; sha256 verify. |

For a 12 MiB body the helper produces 3 parts: 5 MiB + 5 MiB + 2 MiB.
Smaller objects (< 5 MiB total) skip multipart entirely and use a
single `Object.Put` (small-object fast path).

## Run

### Option A — local MinIO (no cloud account; requires Docker)

```bash
docker run -d --rm --name omc-minio -p 9000:9000 \
    -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
    minio/minio server /data

go run ./examples/streaming_write

docker stop omc-minio
```

### Option B — real AWS S3

```bash
export OMC_STREAM_PROVIDER=aws
export OMC_STREAM_REGION=us-east-1
export OMC_STREAM_ENDPOINT=
export OMC_STREAM_BUCKET=my-bucket
export OMC_STREAM_KEY=AKIA...
export OMC_STREAM_SECRET=...

go run ./examples/streaming_write
```

### Option C — any other provider

Swap `OMC_STREAM_PROVIDER` to one of the 10 supported provider ids and
add the matching driver import to [`main.go`](main.go) so its `init()`
registers the Factory. The demo uses MinIO + AWS imports by default.

## Expected output

```
opened minio client → endpoint=http://localhost:9000 bucket=uos-stream-1714322103 key=stream-demo.log
bucket created
streamed: 12582912 bytes across 144304 log lines (sha256=4f8b1a2c…)
close: multipart complete
verify: 12582912 bytes round-tripped, sha256 matches (4f8b1a2c…)
streaming_write OK — cleanup runs on defer
```

The `12582912` byte count = exactly 12 MiB; the line count varies
slightly per run because each line carries a nanosecond timestamp.

## Cleanup

Defer-driven: object deleted before bucket deleted before the program
exits. Best-effort even on failure.

## See also

- [`pkg/uos/streamio/`](../../pkg/uos/streamio/) — the Writer implementation
- [`examples/multipart/`](../multipart/) — raw `MultipartService` demo (when you need explicit PartNumber control + List/Abort lifecycle visibility)
- [`examples/quickstart/`](../quickstart/) — minimal Open + Put + Get + Sign
