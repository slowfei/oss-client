# OpenTelemetry Semantic-Convention Alignment

Status of `pkg/uos/middleware`'s observability surface against the
[OpenTelemetry Semantic Conventions for Object Storage](https://opentelemetry.io/docs/specs/semconv/object-stores/)
(stable as of OTel spec v1.27.0). Audit done at v0.1.1; recommendations
are tracked for v0.2.0+ rollout.

> **Status**: M6 Phase 3 audit. Companion to
> [`pkg/uos/middleware/middleware.go`](../pkg/uos/middleware/middleware.go)
> (the binding contract) and
> [`docs/architecture_plan.md`](architecture_plan.md) §2.3 (the credential-
> redaction rule). No `pkg/uos` source change in v0.1.x; this doc is the
> recommendation list for the v0.2.0 minor bump.

## Why this matters

Production object-storage workloads typically integrate with OTel
tracing/metrics infrastructure (Grafana, Honeycomb, Datadog, AWS X-Ray,
etc.). If `pkg/uos`'s span and metric attribute names align with the OTel
storage semantic conventions, dashboards built against vendor SDKs Just
Work when migrated; if they don't align, every downstream consumer has
to write a translation layer.

## Audit: current `middleware.Event` field → OTel attribute

The `Event` struct ([`middleware.go:25-55`](../pkg/uos/middleware/middleware.go))
is the canonical observability shape. Per-field alignment with the OTel
storage spec:

| `middleware.Event` field | OTel attribute | Match | Notes |
| --- | --- | --- | --- |
| `Provider` | `cloud.provider` (or vendor-scoped) | 🟡 partial | OTel uses `aws`, `gcp`, `azure` for `cloud.provider`; pkg/uos uses `aws`, `gcs`, `azure`, `alibaba`, `tencent`, `huawei`, `volcengine`, `qiniu`, `upyun`, `minio`. The 4 国云 providers + minio + qiniu + upyun don't have OTel-blessed `cloud.provider` values; they typically use `aws.s3.compatible` or vendor-specific extensions. |
| `Op` | `code.function` + `rpc.method` | 🟡 partial | OTel storage spec uses operation names like `PutObject`, `GetObject`, `Upload`, `Download`. pkg/uos uses the same string ("PutObject", "GetObject") which aligns directly. |
| `Bucket` | `aws.s3.bucket` (S3) / `gcp.gcs.bucket` (GCS) / `azure.storage.container` (Azure) | 🔴 vendor-specific | OTel doesn't have a vendor-agnostic bucket attribute. Recommendation: emit `bucket.name` (proposed but unstable) AND the vendor-specific attribute side-by-side. |
| `KeyHash` | (none in OTel spec) | ✅ aligned by design | pkg/uos hashes keys for privacy per architecture_plan §2.3; OTel spec acknowledges raw object keys are sensitive and recommends omitting them. KeyHash is a pkg/uos invention that aligns with the spirit. |
| `Attempt` | `http.request.resend_count` (HTTP) / custom | 🟡 partial | OTel HTTP spec uses 0-based resend count; pkg/uos uses 1-based attempt count. Off-by-one in the export layer. |
| `Latency` | `http.request.duration` (histogram) | ✅ aligned | Latency in nanoseconds (time.Duration); OTel HTTP duration is a histogram metric in seconds. Conversion is straightforward. |
| `HTTPStatus` | `http.response.status_code` | ✅ aligned | int matches OTel int. |
| `Code` | `error.type` | 🟡 partial | OTel uses error type strings (e.g. `aws.smithy.api#NotFoundException`, `os.error`). pkg/uos.Code is the unified 14-value vocabulary (`not_found`, `unauthenticated`, etc.). Recommendation: emit BOTH (`error.type=os.error.not_found`) so downstream dashboards can pivot on the unified code. |
| `RequestID` | `aws.request_id` (S3) / vendor-specific | 🟡 partial | Same multi-vendor naming problem as Bucket. |
| `Err` | (carried as exception attributes) | ✅ aligned | OTel records errors as span events with `exception.type` + `exception.message` + `exception.stacktrace`. pkg/uos's `Err` is a Go `error` value; the OTel exporter wraps it. |

## Audit: tracing span shape

Current `Tracer` interface ([`middleware.go:71-73`](../pkg/uos/middleware/middleware.go)):

```go
type Tracer interface {
    Start(ctx context.Context, op Op) (context.Context, func(Event))
}
```

OTel tracing for storage operations uses the convention:

- **Span name** = `<vendor>.<service>.<op>` (e.g. `S3.PutObject`,
  `GCS.WriteObject`, `Azure.Blob.PutBlock`). pkg/uos's `Op` is the
  vendor-agnostic op name (`PutObject`); the vendor prefix is missing and
  must be reconstructed from `Provider`.
- **Span kind** = `CLIENT` for outbound calls. pkg/uos's Tracer doesn't
  distinguish; downstream OTel exporter must hardcode `CLIENT`.
- **Attributes** = the Event field set above, plus span-only attributes
  (`server.address`, `network.peer.address`).

### Recommended span name convention (v0.2.0)

Adopt: `<provider>.<op>` lowercase, e.g. `aws.put_object`,
`azure.put_block`, `qiniu.upload_token_issue`. This matches the OTel
storage convention for vendor-prefixed RPC method names without committing
to vendor-specific attribute taxonomies.

Implementing this in v0.2.0 requires either:
1. Adding a `SpanName(provider, op)` formatter helper to `middleware`
   (no frozen-surface bump), OR
2. Documenting in the `Tracer` doc that exporters MUST format the span
   name as `<provider>.<lowercase-op>` themselves.

Option 2 is preferred — keeps `middleware` free of OTel-specific
formatting logic. Update `middleware.go` doc comment + add a section to
this file referencing the recommended pattern.

## Audit: metrics shape

Current `Metrics` interface:

```go
type Metrics interface {
    Observe(ctx context.Context, ev Event)
}
```

OTel storage metrics conventions (for the histograms / counters
typically tracked):

| OTel metric | pkg/uos source | Match |
| --- | --- | --- |
| `os.client.duration` (Histogram, seconds) | `Event.Latency` | ✅ direct conversion |
| `os.client.request.body.size` (Histogram, bytes) | not exposed | 🔴 missing — would need a new field |
| `os.client.response.body.size` (Histogram, bytes) | not exposed | 🔴 missing |
| `os.client.errors.total` (Counter) | `Event.Code != ""` | ✅ derivable |
| `os.client.retries.total` (Counter) | `Event.Attempt > 1` | ✅ derivable |

**v0.2.0 recommendation**: extend `Event` with optional
`RequestBodySize int64` and `ResponseBodySize int64` fields (zero =
unknown). Drivers populate these from the vendor SDK's bytes counters.
This is an additive struct change — does NOT touch the 14/13/4 frozen
sets; safe for a minor bump.

## Logging redaction status (architecture_plan §2.3)

The credential-redaction contract is enforced in
[`pkg/uos/middleware/middleware.go`](../pkg/uos/middleware/middleware.go)'s
`RedactHeaders` and `RedactQuery` functions. Audit:

- `Authorization` header — redacted ✅
- `X-Amz-Security-Token` (AWS STS) — redacted ✅
- `X-Goog-Hmac-*` (GCS HMAC) — redacted ✅
- `X-Ms-Date` + `Authorization` (Azure SharedKey) — redacted ✅
- SAS query strings (`sig=`, `se=`, `sp=`, `sv=`) — redacted ✅
- Qiniu `Authorization` form field — redacted ✅
- Upyun `Authorization` header — redacted ✅
- AWS presigned URL query params (`X-Amz-Signature`, `X-Amz-SignedHeaders`,
  `X-Amz-Credential`) — redacted ✅

Confirmed redaction coverage is complete for all 10 v1 providers. No
gaps identified during the M6 phase 3 audit.

### Per-provider audit notes

- **gcs**: Service Account JSON contains the private key; pkg/uos never
  logs the credential body, only the email + scope (driver-internal).
- **azure**: Account-key SAS sigs are URL-encoded; redaction covers the
  decoded form.
- **qiniu**: Upload Token format is `<access-key>:<sig>:<base64-policy>`.
  Redaction strips the sig+policy parts but preserves access-key for
  debugging.
- **upyun**: signature form-field is base64-encoded; redaction covers
  both form-field and `Authorization: UPYUN <op>:<sig>` header.

## v0.2.0 work items

Recommended OTel-related work for the next minor cycle, ordered by
ROI:

1. **Add `RequestBodySize` + `ResponseBodySize` fields to `middleware.Event`**
   (additive; no frozen-surface bump). Enables `os.client.request.body.size`
   and `os.client.response.body.size` histograms downstream. Drivers
   populate from the vendor SDK's wire counters; nil/zero means "unknown".
2. **Document the recommended span-name convention** (`<provider>.<lowercase-op>`)
   in `middleware.Tracer` doc + this file. No code change.
3. **Add `Code → OTel error.type string` mapping helper** (e.g.
   `middleware.OTelErrorType(code) string` returning `os.error.not_found`,
   `os.error.unauthenticated`, etc.). Pure helper — no frozen-surface impact.
4. **Document the off-by-one** between `Event.Attempt` (1-based) and
   OTel's `http.request.resend_count` (0-based). Leave the on-the-wire
   shape as-is (1-based is more user-friendly for log readers); document
   the conversion that exporters need to do.
5. **Add `KeyPrefix` field to `Event`** (the parent "directory" of the
   key, e.g. `uploads/2026/04/`). Useful for per-prefix metrics
   (storage class breakdown, hot-prefix detection) without exposing
   the full key.

None of the 5 are gating for v0.1.x or v1.0.0. They land at v0.2.0 if
external feedback validates the need.

## Reference exporter sketch

For users wanting OTel emission today, the recommended exporter shape
(works against the v0.1.x `middleware` API, no code change needed):

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/trace"
    "github.com/slowfei/oss-client/pkg/uos/middleware"
)

type otelTracer struct{ tr trace.Tracer }

func (o *otelTracer) Start(ctx context.Context, op middleware.Op) (context.Context, func(middleware.Event)) {
    ctx, span := o.tr.Start(ctx, "uos."+string(op), trace.WithSpanKind(trace.SpanKindClient))
    return ctx, func(ev middleware.Event) {
        span.SetAttributes(
            attribute.String("uos.provider", ev.Provider),
            attribute.String("uos.op", string(ev.Op)),
            attribute.String("uos.bucket", ev.Bucket),
            attribute.String("uos.key.hash", ev.KeyHash),
            attribute.Int("uos.attempt", ev.Attempt),
            attribute.Int64("http.response.status_code", int64(ev.HTTPStatus)),
            attribute.String("error.type", ev.Code),
            attribute.String("aws.request_id", ev.RequestID),
        )
        if ev.Err != nil {
            span.RecordError(ev.Err)
        }
        span.End()
    }
}
```

A canonical OTel exporter ships in M6 phase 3 / v1.0.0 (planned at
`pkg/uos/middleware/otel/` — own subpackage so the OTel transitive deps
don't pollute the stdlib-only root). Until then, the sketch above is
the recommended pattern.

## See also

- [`pkg/uos/middleware/middleware.go`](../pkg/uos/middleware/middleware.go) — the binding contract
- [`docs/architecture_plan.md`](architecture_plan.md) §2.3 — credential redaction rule
- [OTel Semantic Conventions for Object Stores](https://opentelemetry.io/docs/specs/semconv/object-stores/) — upstream spec
