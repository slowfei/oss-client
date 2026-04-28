# v1.0.0 Polish Audit

M6 Phase 3 stabilization review. Audits 3 areas the architecture_plan
flagged for v1.0.0 cut-time review:

1. Idempotency markers on retryable operations
2. Retry-budget guards (per-call + per-batch)
3. Log redaction completeness (cross-references `docs/otel_alignment.md`)

> **Status**: M6 Phase 3 audit, run at v0.1.1. Findings are the
> "ready for v1.0.0?" answer per provider_roadmap.md §M6 exit criterion.
> No `pkg/uos` source change required for v1.0.0 cut; recommendations
> targeting v0.2.0+ are listed at the end.

## 1. Idempotency markers

### Current state

Per [`pkg/uos/config.go:96-106`](../pkg/uos/config.go),
`DefaultIsIdempotent(op string) bool` returns:

```go
case "ListBuckets", "StatBucket",
     "GetObject", "HeadObject", "ListObjects", "ExistsObject",
     "ListMultipartUploads",
     "SignURL":
    return true
default:
    return false
```

Read operations are idempotent by default; write operations (including
`PutObject`, `Initiate`, `UploadPart`, `Complete`, `Abort`,
`DeleteObject`, etc.) return `false` and are NOT auto-retried by
`RetryPolicy`.

This conservative default is the architecturally-correct choice per
the `DefaultIsIdempotent` doc comment:

> PutObject is NOT idempotent in general because the same key may be
> overwritten by another writer between attempts. Callers who know
> their writer is the sole producer (or who carry If-None-Match
> preconditions) can override.

### Audit verdict: **READY**

The conservative default is the right call. The opt-in path
(`RetryPolicy.IsIdempotent func(op string) bool`) lets callers who
KNOW their write is safe (single-producer key, content-addressed key,
If-None-Match precondition guarantee) extend retry-eligibility without
recompiling pkg/uos.

### Documented opt-in patterns

Callers who want to retry writes safely have 3 patterns, each documented:

#### Pattern A: Content-addressed key (single-producer guarantee)

```go
// Key is the SHA-256 of the body. Identical content produces an
// identical key. Concurrent writers writing the same content land
// on the same key with identical bytes; retry is safe.
cfg.Retry = uos.RetryPolicy{
    MaxAttempts: 3,
    IsIdempotent: func(op string) bool {
        if op == "PutObject" {
            return true // app-level invariant: keys are content-addressed
        }
        return uos.DefaultIsIdempotent(op)
    },
}
```

#### Pattern B: If-None-Match precondition

```go
// Set IfNoneMatch="*" to atomically reject if a key already exists.
// Retry is safe because the second attempt will get ErrAlreadyExists,
// which the caller can treat as success (idempotent semantics).
_, err := cli.Objects(bucket).Put(ctx, uos.PutObjectRequest{
    Key:         "uploads/2026/04/abc.bin",
    Body:        body,
    IfNoneMatch: "*", // first-write-wins
})
if err != nil {
    var uerr *uos.Error
    if errors.As(err, &uerr) && uerr.Code == uos.ErrAlreadyExists {
        // treat as success — first attempt landed before retry
    }
}
```

#### Pattern C: Multipart UploadPart with explicit PartNumber

```go
// UploadPart with PartNumber=N is naturally idempotent: re-uploading
// the same part overwrites with identical bytes. Multipart Complete
// then references the PartNumber set, not the upload sequence.
cfg.Retry = uos.RetryPolicy{
    IsIdempotent: func(op string) bool {
        return op == "UploadPart" || uos.DefaultIsIdempotent(op)
    },
}
```

### v0.2.0 documentation work item

**Add a "Retry semantics" section to** [`docs/migration_guide.md`](migration_guide.md)
**covering Patterns A/B/C above.** This is the most-likely caller question
post-v1.0.0 ("how do I make Put retryable?"). 1-paragraph mention exists
in DefaultIsIdempotent's doc comment; users will want a longer treatment
in migration_guide.

## 2. Retry-budget guards

### Current state

[`pkg/uos/config.go:66-85`](../pkg/uos/config.go) defines `RetryPolicy`:

```go
type RetryPolicy struct {
    MaxAttempts    int           // 0 → driver default (typically 3)
    BaseBackoff    time.Duration // 0 → driver default (typically 100ms)
    MaxBackoff     time.Duration // 0 → driver default (typically 20s)
    Jitter         float64       // 0 → driver default (typically 0.2)
    IsIdempotent   func(op string) bool
    RetryableCodes []Code
}
```

Drivers MUST translate this once at construction time and disable any
duplicate vendor-internal retry layer (the "double retry storm" risk
documented in [`docs/provider_roadmap.md`](provider_roadmap.md)
cross-cutting risks). Verified via:

- `providers/aws/factory.go`: sets `aws.Retryer` with `MaxAttempts: 1`
  (translates pkg/uos retries; vendor SDK doesn't add its own).
- `providers/minio/factory.go`: minio-go has no internal retryer to
  disable; pkg/uos is the only retry source.
- `providers/gcs/driver.go`: `*storage.Client.SetRetry(...WithMaxAttempts(1)...)`
  disables GCS SDK's internal retry.
- `providers/azure/factory.go`: `azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: 0}}`
  disables azblob SDK's internal retry.
- `providers/qiniu/factory.go`: qiniu/go-sdk/v7 has no public retry knob;
  documented as such (no double-retry risk).
- `providers/upyun/factory.go`: upyun/go-sdk/v3 has no internal retryer;
  documented.
- `providers/alibaba/factory.go`: aliyun-oss-go-sdk has only HTTP-transport
  conn-reset retries (not request-level); documented.
- `providers/tencent/factory.go`: cos-go-sdk-v5 `RetryOpt.Count = 1`.
- `providers/huawei/factory.go`: huaweicloud-sdk-go-obs disabled internal
  retryer.
- `providers/volcengine/factory.go`: ve-tos-golang-sdk `MaxRetryCount=0`
  (vendor-level escape hatch at `DriverConfig.MaxRetryCount` for callers
  who want it).

### Audit verdict: **READY**

All 10 drivers honor the single-source-of-truth principle. The
"double retry storm" risk is mitigated by construction.

### Per-batch retry budget

`RetryPolicy.MaxAttempts` caps PER-OPERATION retries. There is NO
per-batch / per-process retry budget in v1 (e.g., "no more than 1000
total retries per minute across all operations").

This is intentional — per-batch budgets are application-domain logic
(rate-limiter / circuit-breaker pattern) and don't belong in the unified
client. Callers needing this should layer their own
`golang.org/x/time/rate` or `gobreaker` middleware around `cli.Objects(...)`
calls.

**v0.2.0 documentation work item**: add this rationale to the
`RetryPolicy` doc comment so callers don't expect a built-in budget.

## 3. Log redaction completeness

Full audit lives in [`docs/otel_alignment.md`](otel_alignment.md)
"Logging redaction status" section. Summary:

- `Authorization` header — redacted ✅
- AWS STS / SigV4 query params — redacted ✅
- GCS HMAC headers — redacted ✅
- Azure SharedKey signature + SAS sigs — redacted ✅
- Qiniu Upload Token + Download Token — redacted ✅
- Upyun signature header + form-field — redacted ✅
- AWS presigned URL query params — redacted ✅

**Audit verdict: READY**. No gaps identified across the 10 v1 providers.

### v0.2.0 hardening recommendation

Add a `redaction_test.go` table-driven test in `pkg/uos/middleware/`
that covers each of the 10 vendor's signed-URL formats and
authorization-header shapes. Currently `RedactHeaders` / `RedactQuery`
are tested generically; per-vendor coverage would catch any future
vendor SDK behavior change (new query param shape introduced upstream).

## Cumulative v1.0.0 readiness verdict

**READY**. The 3 audit areas (idempotency / retry-budget / redaction) all
clear without `pkg/uos` source change. v0.1.1 is structurally complete;
the v1.0.0 cut is unblocked from a polish standpoint.

The remaining v1.0.0 gates per provider_roadmap.md §M6 exit:

- [x] All 10 providers shipped (M2-M5 done).
- [x] Frozen 14/13/4 surface intact (TestFrozenSurface 3/3 PASS).
- [x] CHANGELOG accurate per-module (covers M2 through v0.1.1 patch).
- [x] README ≥30 line quickstart per provider (M6 phase 1 done).
- [x] Cross-provider migration example (`examples/multipart` +
      `examples/direct_grant_qiniu` + `examples/direct_grant_upyun` +
      [migration_guide.md](migration_guide.md) — M6 phase 2 done).
- [x] Benchmark scaffold (M6 phase 2 done; per-vendor sweeps deferred to
      M6 phase 3 / v1.0.0).
- [x] OTel alignment audit ([otel_alignment.md](otel_alignment.md) — M6
      phase 3 done; alignment work items deferred to v0.2.0).
- [x] Polish audit (this doc — M6 phase 3 done).
- [ ] **`architecture_plan.md` Appendix A is empty (all deferred items
      resolved or explicitly punted to v1.x)** — pending; needs the v0.2.0
      candidate list (12 items in AGENTS.md item 8 + this doc's
      recommendations) consolidated into Appendix A or a v0.2.0 tracking
      issue list.
- [ ] **All 10 providers tagged v1.0.0; pkg/uos/v1.0.0 cut** — maintainer
      action; release-prep commit pattern documented in RELEASING.md §4
      (mirror of the v0.1.x release-prep done in commit 0cd775e).

## See also

- [`pkg/uos/config.go`](../pkg/uos/config.go) — RetryPolicy + DefaultIsIdempotent
- [`pkg/uos/middleware/middleware.go`](../pkg/uos/middleware/middleware.go) — RedactHeaders / RedactQuery
- [`docs/architecture_plan.md`](architecture_plan.md) §2.3 — credential-redaction rule
- [`docs/otel_alignment.md`](otel_alignment.md) — observability semantic-conv audit
- [`docs/provider_roadmap.md`](provider_roadmap.md) §M6 — milestone exit criterion
- [`AGENTS.md`](../AGENTS.md) Appendix A item 8 — cumulative v0.2.0 candidate list
