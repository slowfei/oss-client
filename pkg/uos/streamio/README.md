# pkg/uos/streamio

`github.com/slowfei/oss-client/pkg/uos/streamio` — streaming-write helpers
on top of the unified `uos.Client` interface. Wraps the multipart-upload
lifecycle into an `io.WriteCloser` so callers stream bytes without
managing PartNumber sequences, minimum-part-size buffering, or
Initiate / Complete / Abort plumbing.

## Why

The unified `MultipartService` is faithful to the underlying vendor
multipart contract: caller decides when to Initiate, picks PartNumbers,
buffers ≥5 MiB chunks, and calls Complete. That's the right shape for
parallel uploads of a known-size body, but tedious for the common case
of "open a new object and stream into it". `streamio.Writer` handles
the bookkeeping.

## Provider support

**All 10 v1 providers** (aws, minio, alibaba, tencent, huawei,
volcengine, gcs, azure, qiniu, upyun). The helper only calls the
unified `MultipartService` + `ObjectService.Put`, both of which are
universally supported.

## Quickstart

```go
import (
    "github.com/slowfei/oss-client/pkg/uos"
    "github.com/slowfei/oss-client/pkg/uos/streamio"
)

w, err := streamio.NewWriter(ctx, cli, "my-bucket", "log.txt", streamio.WriterOptions{
    ContentType: "text/plain",
    Metadata:    uos.Metadata{"source": "my-app"},
})
if err != nil { return err }
defer w.Close()

for line := range incoming {
    if _, err := w.Write([]byte(line + "\n")); err != nil {
        w.Abort() // release vendor multipart state
        return err
    }
}
return w.Close() // commits via Complete (or single Put if total < 5 MiB)
```

## Lifecycle

| Phase | Behavior |
|---|---|
| `NewWriter` | Validates args; no I/O. Returns error on nil client / empty bucket / empty key / PartSize < 5 MiB. |
| `Write` (buffer fills below PartSize) | Buffers in memory. No I/O. |
| `Write` (buffer ≥ PartSize) | First overflow: calls `Multipart.Initiate`. Subsequent: `Multipart.UploadPart` per full part. |
| `Close` (no multipart, buffer ≤ SmallObjectThreshold) | Single `Object.Put` (small-object fast path). |
| `Close` (multipart in progress, OR buffer > SmallObjectThreshold) | Initiates multipart if needed, uploads remaining buffer as final part (no min-size constraint), calls `Multipart.Complete`. |
| `Abort` | If multipart was initiated, calls `Multipart.Abort` to release vendor state. Idempotent; safe to call before/after Close. |
| Sticky write error | Subsequent Write calls return the same error. Close attempts to release vendor state via Abort, then returns the original error. |

## Options

```go
type WriterOptions struct {
    ContentType          string       // MIME type
    Metadata             uos.Metadata // user metadata
    StorageClass         string       // vendor-specific storage class
    ACL                  string       // vendor-specific canned ACL
    PartSize             int          // multipart part size; default 5 MiB; min 5 MiB
    SmallObjectThreshold int          // total ≤ this → single Put; default = PartSize
}
```

## Examples

A runnable demo lives at
[`examples/streaming_write/`](../../../examples/streaming_write/) —
spins up MinIO via testcontainers (or any S3-compatible endpoint via
env vars), writes 12 MiB of synthetic log data through a Writer, and
verifies the round-trip read.

## Concurrency

`Writer` is **NOT** safe for concurrent use. Wrap with a mutex if
multiple goroutines write to the same Writer.

## Testing

```bash
go test -race -count=1 ./pkg/uos/streamio/
```

The unit tests use an in-memory fake `uos.Client` to verify the
multipart lifecycle without Docker. The
[`pkg/testkit/contract`](../testkit/contract/) suite separately
exercises the full multipart contract against a real MinIO via
`testcontainers-go` for every shipped driver.

## See also

- [`pkg/uos`](../) — unified `Client` API
- [`docs/migration_guide.md`](../../../docs/migration_guide.md) — vendor SDK → pkg/uos walkthrough
- [`examples/streaming_write/`](../../../examples/streaming_write/) — runnable demo
- [`examples/multipart/`](../../../examples/multipart/) — raw `MultipartService` demo (when you need explicit PartNumber control)
