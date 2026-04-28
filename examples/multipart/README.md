# multipart

A runnable end-to-end demo of the unified `pkg/uos` `MultipartService` API.
Opens a Client, initiates a 3-part multipart upload (3 × 5 MiB = 15 MiB),
completes the upload, verifies the result via `Head`, demonstrates the `Abort`
path, then tears everything down on exit.

## Run

### Option A — local MinIO (no cloud account needed; requires Docker)

```bash
docker run -d --rm --name omc-minio -p 9000:9000 \
    -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
    minio/minio server /data

go run ./examples/multipart

docker stop omc-minio
```

Defaults target `http://localhost:9000` with the canonical
`minioadmin/minioadmin` credentials, an auto-generated bucket name
(`uos-multipart-<unix-timestamp>`), and the object key `multipart-demo.bin`.

### Option B — real AWS S3 (requires a real account + an existing bucket)

```bash
export OMC_MULTIPART_PROVIDER=aws
export OMC_MULTIPART_REGION=us-east-1
export OMC_MULTIPART_ENDPOINT=
export OMC_MULTIPART_BUCKET=my-bucket-name-here
export OMC_MULTIPART_KEY=AKIA...
export OMC_MULTIPART_SECRET=...

go run ./examples/multipart
```

`OMC_MULTIPART_ENDPOINT=` (empty) makes the AWS driver use the default
regional endpoint. Set it to a non-empty URL for S3-compatible alternatives.

### Option C — any other provider

Swap the `OMC_MULTIPART_PROVIDER` env var to one of the 10 supported provider
ids (`aws`, `minio`, `alibaba`, `tencent`, `huawei`, `volcengine`, `gcs`,
`azure`, `qiniu`, `upyun`) and add the matching side-effect import to
[`main.go`](main.go) so its `init()` registers the Factory.

## Env vars

| Variable | Default | Description |
| --- | --- | --- |
| `OMC_MULTIPART_PROVIDER` | `minio` | Provider id (factory key). |
| `OMC_MULTIPART_REGION` | `us-east-1` | Region string passed to the driver. |
| `OMC_MULTIPART_ENDPOINT` | `http://localhost:9000` | Override endpoint URL. |
| `OMC_MULTIPART_BUCKET` | `uos-multipart-<ts>` | Target bucket (auto-created). |
| `OMC_MULTIPART_OBJECT_KEY` | `multipart-demo.bin` | Target object key. |
| `OMC_MULTIPART_KEY` | `minioadmin` | Access key ID. |
| `OMC_MULTIPART_SECRET` | `minioadmin` | Secret access key. |

## Expected output

```
opened minio client → endpoint=http://localhost:9000 bucket=uos-multipart-1714322103 key=multipart-demo.bin
bucket created
initiated: uploadID="..."
  part 1 uploaded: etag="..." size=5242880
  part 2 uploaded: etag="..." size=5242880
  part 3 uploaded: etag="..." size=5242880
complete: etag="..." versionID=""
head: size=15728640 content-type="application/octet-stream" metadata=map[parts:3 source:multipart-example]
list after complete: 0 in-flight uploads (expect 0 on S3-compatible)
abort demo: initiated uploadID="..."
abort demo: part 1 etag="..."
abort demo: upload aborted
abort demo: 0 in-flight uploads after abort (expect 0)
abort demo: confirmed — aborted key absent (ErrNotFound)
multipart OK — cleanup runs on defer
```

Cleanup is wired via `defer` — the bucket and object are removed even on
failure (best-effort).

## What this demonstrates

| Step | Method | What it shows |
| --- | --- | --- |
| Open | `uos.DefaultRegistry().Open(ctx, cfg)` | Provider dispatch via the `Factory` registry; one `Config` shape across all 10 drivers. |
| Bucket Create | `cli.Buckets().Create(...)` | Idempotent against `ErrAlreadyExists`. |
| Initiate | `cli.Multipart(bucket).Initiate(...)` | Returns a `MultipartUpload` with `UploadID`; binds `ContentType` and user `Metadata` at upload-start time. |
| UploadPart × 3 | `multipart.UploadPart(...)` | Streams 5 MiB parts (S3 minimum for non-final parts); collects `UploadedPart.ETag` per part. |
| Complete | `multipart.Complete(...)` | Stitches parts in `PartNumber` order; returns `PutObjectResult` with the final composite `ETag`. |
| Head | `cli.Objects(bucket).Head(...)` | Verifies `Size` (15 MiB), `ContentType`, and user `Metadata` round-tripped correctly. |
| List | `multipart.List(...)` | Enumerates in-flight uploads; zero after Complete on S3-compatible drivers. |
| Abort | `multipart.Abort(...)` | Cancels an in-flight upload; vendor releases partial-part storage. |
| Absent confirm | `cli.Objects(bucket).Head(...)` | Confirms aborted key returns `ErrNotFound` (never committed). |
| Cleanup | `cli.Buckets().Delete(...)` / `cli.Objects(bucket).Delete(...)` | Runs on `defer` — fires even on `log.Fatalf`. |

## Driver notes: `MultipartService.List` behaviour

`List` is fully functional on **S3-compatible** drivers (`aws`, `minio`,
`alibaba`, `tencent`, `huawei`, `volcengine`): the vendor tracks in-flight
uploads server-side and returns them here.

On **non-S3** drivers (`gcs`, `azure`, `qiniu`, `upyun`), `List` returns only
uploads initiated in the **current process session** (in-memory state). This
is a documented M4/M5 driver-side limitation: those vendors either have no
server-side multipart listing API or expose it through a different call shape.
Use `List` for orphan cleanup only when you know the sessions were started in
the same process.

## What this does NOT cover

- `ObjectService.Put` (see `examples/quickstart` for the simple put/get path)
- Presigned multipart (`SignURL` on part URLs — vendor-specific)
- Resumable uploads with a custom state store (persist `UploadID` + `ETag`
  list across process restarts, then call `Complete` in a later run)
- Cross-provider migration
- Parallel part upload (goroutine fan-out over `UploadPart`)
