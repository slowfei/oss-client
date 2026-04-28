# quickstart

A runnable end-to-end demo of the unified `pkg/uos` API. Opens a Client,
creates a bucket, puts + gets + signs + deletes a small object, then tears
the bucket down on exit.

## Run

### Option A — local MinIO (no cloud account needed; requires Docker)

```bash
docker run -d --rm --name omc-minio -p 9000:9000 \
    -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
    minio/minio server /data

go run ./examples/quickstart

docker stop omc-minio
```

Defaults target `http://localhost:9000` with the canonical
`minioadmin/minioadmin` credentials, an auto-generated bucket name
(`uos-quickstart-<unix-timestamp>`), and the object key `hello.txt`.

### Option B — real AWS S3 (requires a real account + an existing bucket)

```bash
export OMC_QUICKSTART_PROVIDER=aws
export OMC_QUICKSTART_REGION=us-east-1
export OMC_QUICKSTART_ENDPOINT=
export OMC_QUICKSTART_BUCKET=my-bucket-name-here
export OMC_QUICKSTART_KEY=AKIA...
export OMC_QUICKSTART_SECRET=...

go run ./examples/quickstart
```

`OMC_QUICKSTART_ENDPOINT=` (empty) makes the AWS driver use the default
regional endpoint. Set it to a non-empty URL for S3-compatible alternatives.

### Option C — any other provider

Swap the `OMC_QUICKSTART_PROVIDER` env var to one of the 10 supported
provider ids (`aws`, `minio`, `alibaba`, `tencent`, `huawei`, `volcengine`,
`gcs`, `azure`, `qiniu`, `upyun`) and add the matching side-effect import
to [`main.go`](main.go) so its `init()` registers the Factory. Provider-
specific config knobs (e.g. `qiniu` needs `DriverConfig.Domain`,
`azure` needs `DriverConfig.StorageAccount`) are documented in the
per-provider README under `providers/<name>/README.md`.

## Expected output

```
opened minio client → endpoint=http://localhost:9000 bucket=uos-quickstart-1714322103 key=hello.txt
bucket created
put: etag="0c4d9b1a8f7e3c…" size=72
get: 72 bytes round-tripped, content-type="text/plain", metadata=map[source:quickstart]
sign: http://localhost:9000/uos-quickstart-…?X-Amz-Algorithm=AWS4-HMAC-SHA256&… (expires 2026-04-28T22:11:30Z)
capabilities: 13 cells reported (use docs/provider_matrix.md for the visual breakdown)
quickstart OK — cleanup runs on defer
```

Cleanup is wired via `defer` — the bucket and object are removed even on
failure (best-effort).

## What this demonstrates

| Step | Method | What it shows |
| --- | --- | --- |
| Open | `uos.DefaultRegistry().Open(ctx, cfg)` | Provider dispatch via the `Factory` registry; one `Config` shape across all 10 drivers. |
| Bucket Create | `cli.Buckets().Create(...)` | The `BucketService` view; idempotent against `ErrAlreadyExists`. |
| Put | `cli.Objects(bucket).Put(...)` | The `ObjectService` view bound to a default bucket; round-trips body + metadata + content type. |
| Get | `cli.Objects(bucket).Get(...)` | Streaming response (`io.ReadCloser`); range-read support via `req.Range` (not exercised here). |
| Sign | `cli.Signer(bucket).SignURL(...)` | Presigned-URL issue; gracefully degrades to `ErrUnsupported` for vendors that don't support URL-shaped signing. |
| Capabilities | `cli.Capabilities(ctx)` | The 13-cell capability `Report`; cross-reference `docs/provider_matrix.md`. |
| Bucket Delete | `cli.Buckets().Delete(...)` | Cleanup on defer. |

## What this does NOT cover

- Multipart upload (`MultipartService.Initiate / UploadPart / Complete / Abort`)
- `IssueDirectGrant` (the non-URL grant path — see `providers/qiniu` and
  `providers/upyun` READMEs for the canonical Token / Form examples)
- Bucket / object versioning
- Object tagging, ACL, server-side encryption knobs
- Cross-provider migration

These are M6 phase 2/3 deliverables in the examples directory.
