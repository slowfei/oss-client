# Provider Roadmap

> **Status**: Binding for v1. Authoritative source for milestone order.
> Companion to `docs/architecture_plan.md` (§4 milestones, §5 rollout summary).

## Reading guide

This file expands `docs/architecture_plan.md` §4 with **per-provider** scope, validation focus, risk callouts, and a per-milestone exit checklist. Capability cell-by-cell support is in `docs/provider_matrix.md`; this file is about *order*, *grouping*, and *what each provider's milestone has to teach the abstraction*.

## Locked milestone sequence

| Milestone | Providers (added) | Cumulative SDKs landed | Theme |
| --- | --- | --- | --- |
| M1 | (none — core only) | 0 | Define the abstraction. |
| M2 | aws, minio | 2 | Validate S3-family abstraction; bring up `testcontainers` + contract suite. |
| M3 | alibaba, tencent, huawei, volcengine | 6 | Prove abstraction absorbs HMAC-family国云 variants without core change. |
| M4 | gcs, azure | 8 | Prove abstraction absorbs heterogeneous auth (OAuth2, SharedKey, SAS) without core change. |
| M5 | qiniu, upyun | 10 | Prove `DirectGrant` covers non-URL token / FORM authorization. |
| M6 | (none — stabilize) | 10 | Benchmark, polish, GA prep, v1.0.0. |

A provider may NOT ship out of milestone order without an explicit addendum to `architecture_plan.md`.

## M2 — AWS + MinIO

### Validation focus

- The S3 semantics are the de-facto reference; if the abstraction can't comfortably express AWS S3 + MinIO together, the abstraction is wrong and must be fixed in `pkg/uos` BEFORE M3 starts.
- MinIO doubles as the `testcontainers` backend for every later provider's PR gate.

### `providers/aws`

| Field | Value |
| --- | --- |
| SDK choice | `github.com/aws/aws-sdk-go-v2` + `service/s3` |
| Driver type | Native |
| AuthScheme | `AuthHMAC` (AK/SK + STS session token) |
| Endpoint | virtual-host (default), path-style (opt-in via `DriverConfig`); custom endpoint for S3-compat targets |
| Region resolution | from `Config.Region`; no auto-probe in v1 |
| Multipart | delegate to S3 native multipart |
| Signed URL | S3 v4 presign |
| Risk | None novel; this is the reference. Watch for double-retry: AWS SDK has its own retryer; driver MUST translate `RetryPolicy` once and disable the duplicate layer. |

### `providers/minio`

| Field | Value |
| --- | --- |
| SDK choice | `github.com/minio/minio-go/v7` |
| Driver type | Native (preferred over routing through AWS SDK) |
| AuthScheme | `AuthHMAC` |
| Endpoint | always custom; path-style is the default |
| TLS | private CA support is first-class (`HTTPConfig.RootCAs`) |
| Multipart | delegate to minio-go |
| Signed URL | minio-go presign |
| Risk | minio-go's error surface differs subtly from AWS SDK; `error_map.go` MUST be tested against actual MinIO errors, not assumed. |

### M2 exit checklist (each item must be ✅ before tagging)

- [ ] `pkg/testkit/contract/minio.go` spins up MinIO via `testcontainers-go` and tears down cleanly across all OS targets in CI.
- [ ] `providers/aws` and `providers/minio` both pass `pkg/testkit/contract.RunSuite` against the `testcontainers` MinIO endpoint.
- [ ] Capability matrix populated for both in `docs/provider_matrix.md`.
- [ ] Cloud nightly workflow file present (may exit SKIP without secrets).
- [ ] At least one cloud-nightly run with real AWS credentials documented as green by the maintainer.
- [ ] `providers/aws/v0.1.0` and `providers/minio/v0.1.0` tags pushed.
- [ ] No new `Code` or `Capability` was added to `pkg/uos`.

## M3 — Alibaba + Tencent + Huawei + Volcengine

### Validation focus

- Four HMAC-family providers in one milestone is a **stress test** of the abstraction. If the four can ship without `pkg/uos` change, the design is solid for HMAC-family additions in v1.x.
- Endpoint / region quirks differ per vendor; each vendor has its own dialect. Endpoint resolution stays in the driver (`EndpointResolver`), not in `pkg/uos`.

### Per-provider summary

| Provider | SDK | Notable risk |
| --- | --- | --- |
| `providers/alibaba` (OSS) | `github.com/aliyun/aliyun-oss-go-sdk` | Storage class strings differ from AWS; signed URL host vs CNAME mode; metadata key prefix `x-oss-meta-`. |
| `providers/tencent` (COS) | `github.com/tencentyun/cos-go-sdk-v5` | Endpoint format includes appid; signed URL TTL caps; bucket name normalization. |
| `providers/huawei` (OBS) | `github.com/huaweicloud/huaweicloud-sdk-go-obs` | Region/endpoint pairing matters more than other vendors; clock-skew sensitivity on signing. |
| `providers/volcengine` (TOS) | `github.com/volcengine/ve-tos-golang-sdk/v2/tos` | Directory semantics emulated via `prefix/delimiter`; storage class enum is vendor-specific. |

### M3 exit checklist

- [ ] All 4 providers compile and pass `RunSuite` against `testcontainers` MinIO for runnable cases.
- [ ] Cases requiring real cloud (real Signed URL round-trip with vendor host, vendor-specific storage class round-trip) tagged `t.Skip("cloud-only")` and listed in nightly.
- [ ] Matrix updated for all 4.
- [ ] No new `Code` / `Capability` added; if any was tempting, the rationale is logged here under "Lessons" before tagging.
- [ ] 4 provider tags pushed.

## M4 — GCS + Azure

### Validation focus

- This is the **non-HMAC milestone**. If `DriverConfig` + `AuthScheme` + `Capability` can absorb OAuth2 / Service Account / SharedKey / SAS / User Delegation **without** changing `pkg/uos`, the abstraction proves it can host vendors with fundamentally different auth shapes.

### `providers/gcs`

| Field | Value |
| --- | --- |
| SDK choice | `cloud.google.com/go/storage` |
| AuthScheme | `AuthOAuth2` (default), with `AuthHMAC` available when HMAC keys are configured |
| Signed URL | Requires a private-key-bearing credential; if absent → `Signer.SignURL` returns `Code: ErrUnsupported, Capability: CapSignedURLRead` with `Reason: "credential lacks signing key"` |
| Risk | ADC (Application Default Credentials) discovery has subtle precedence; document explicitly which `credential.Provider` resolves it. |

### `providers/azure`

| Field | Value |
| --- | --- |
| SDK choice | `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob` |
| AuthScheme | `AuthSharedKey` / `AuthSAS` / `AuthCustom` (Entra delegation goes here in v1) |
| Bucket mapping | Azure Container → unified `Bucket`; Storage Account is in `DriverConfig` |
| Signed URL | SAS token issued via `Signer`; account-key SAS vs user-delegation SAS distinguished by `AuthScheme` |
| Risk | SAS expiry-time semantics differ from S3 presign (SAS includes start time, not just expiry); test both. |

### M4 exit checklist

- [ ] `gcs` and `azure` compile and pass adapted `RunSuite`.
- [ ] SAS / GCS Signed URL behavior reflected in matrix as `Conditional` with reason or `ExtensionOnly` where unavoidable.
- [ ] No new `Code` / `Capability` added to `pkg/uos`. Any temptation logged here.
- [ ] 2 provider tags pushed.

## M5 — Qiniu + Upyun

### Validation focus

- This is the **DirectGrant milestone**. Qiniu Upload Token and Upyun FORM are not URL-shaped — they are token / form authorizations. If `Signer.IssueDirectGrant` can return both shapes via the unified `DirectGrant` struct, the design holds for non-URL authorization without a separate abstraction.

### `providers/qiniu`

| Field | Value |
| --- | --- |
| SDK choice | `github.com/qiniu/go-sdk/v7` |
| AuthScheme | `AuthCustom` (Qiniu's Upload / Download / Manage tokens are distinct credentials) |
| DirectGrant | Upload Token surfaced via `IssueDirectGrant(operation=upload)`; Download Token via `IssueDirectGrant(operation=download)` |
| Signed URL | URL-shaped private-bucket access surfaces via `SignURL`; Upload Token surfaces only via `IssueDirectGrant` |
| Risk | `Capabilities` must clearly mark which path serves which use case; business code shouldn't have to know Qiniu specifics to choose. |

### `providers/upyun`

| Field | Value |
| --- | --- |
| SDK choice | `github.com/upyun/go-sdk/v3` (preferred) or REST direct |
| AuthScheme | `AuthCustom` for FORM signing; `AuthSharedKey`-equivalent for basic-auth fallback |
| DirectGrant | FORM upload params surfaced via `IssueDirectGrant` with `Mode=form` |
| Risk | Upyun's media-processing / persistent-pipeline features are explicitly out-of-scope and surface only as `As(target)`. |

### M5 exit checklist

- [ ] `qiniu` and `upyun` compile and pass adapted `RunSuite`.
- [ ] `CapDirectGrant` marked `Supported` for both in matrix.
- [ ] No new `Code` / `Capability` added to `pkg/uos`. (If `DirectGrant` shape needs an extra field, that's a `request.go` minor bump, not a new top-level type.)
- [ ] 2 provider tags pushed.

## M6 — Stabilization

### Scope

- Benchmarks under `benchmarks/`: per-provider Put/Get/Multipart throughput, Signed-URL generation rate.
- Examples under `examples/`: minimal Put+Get+Sign per provider; one cross-provider migration example.
- Migration guide: how to switch from raw vendor SDK to `pkg/uos`.
- OpenTelemetry semantic-conventions alignment: every span / metric attribute name aligned with current OTel storage conventions.
- Polish: idempotency markers on retryable ops, retry-budget guards, log redaction audit.

### Exit criterion

- All 10 providers tagged `v1.0.0`; `pkg/uos/v1.0.0` cut.
- CHANGELOG present and accurate.
- README has a 30-line minimum quickstart per provider.
- `architecture_plan.md` Appendix A is empty (all deferred items resolved or explicitly punted to v1.x with a tracking issue).

## Cross-cutting risks (apply to every milestone)

| Risk | Mitigation |
| --- | --- |
| **Double-retry**: vendor SDK + driver both retry, multiplying latency on transient errors. | Each driver's `factory.go` MUST disable the vendor SDK's internal retryer and route all retries through `RetryPolicy`. Document in driver README. |
| **Double-encode**: keys passed to vendor SDK already URL-encoded once. | `pkg/uos` treats keys as opaque; driver MUST NOT re-encode. Contract test includes a key-with-special-chars case (`#?&%/`). |
| **Credential leak**: Authorization headers / SAS tokens / Upload Tokens in logs. | `middleware/middleware.go` defines a redaction contract; each driver wires it before any log call. |
| **Endpoint misconfiguration**: bucket-region mismatch silently returns 301/307 or wrong-host errors. | `EndpointResolver` per driver responsible for failing fast on obviously-wrong region/endpoint pairs (e.g., AWS bucket in us-east-1 with us-west-2 in Config). |
| **Multipart orphan**: failed multipart leaves vendor-side upload session open, billing the user. | `transfer.Manager` calls `Abort` on every non-resumable failure path; `StateStore` records every initiated upload. |

## Lessons (filled per-milestone post-mortem)

> Each milestone's tag PR MUST append a 1-paragraph "Lessons" entry below before merging. If no abstraction-level lesson surfaced, write "no abstraction defect detected; X providers shipped with zero `pkg/uos` change". This rolling log is the input for the M6 stabilization review.

- **M2 lessons**: _(to be filled)_
- **M3 lessons**: _(to be filled)_
- **M4 lessons**: _(to be filled)_
- **M5 lessons**: _(to be filled)_
- **M6 lessons**: _(to be filled)_
