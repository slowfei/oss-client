# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and each module in this multi-module workspace adheres to [Semantic
Versioning](https://semver.org/spec/v2.0.0.html) independently. See
[RELEASING.md](./RELEASING.md) for the per-module tag protocol.

## [Unreleased]

## [v0.2.1]

### Added

- **`pkg/testkit/contract`** — optional dotenv loading for real-cloud
  provider tests. Test binaries now read environment from
  `$OMC_DOTENV_PATH`, `$XDG_CONFIG_HOME/oss-client/oss-client-cloud.env`,
  or `$HOME/.config/oss-client/oss-client-cloud.env` before provider
  config is loaded; already-exported environment variables take
  precedence.
- **Provider PR-gate coverage** — emulator-backed and signing-shape unit
  tests for non-S3 providers, including Alibaba OSS, Huawei OBS, Qiniu
  Kodo, Tencent COS, Upyun, Volcengine TOS, GCS, and Azure.

### Fixed

- **`providers/aws`** — custom endpoints now honor virtual-hosted style
  when `ForcePathStyle` / `PathStyle` is false, instead of forcing
  path-style solely because an endpoint override is configured. This
  keeps AWS S3-compatible endpoints usable without tying them to any
  vendor-specific provider module.
- **`pkg/testkit/contract`** — real-cloud contract suites now isolate
  generated objects under per-run prefixes and clean up test artifacts
  after each run, including multipart uploads. BYOB mode also preserves
  caller-owned bucket contents by only deleting objects created by the
  suite.
- **Release tooling** — synchronized release bumps now keep examples and
  benchmarks aligned with the 12 tagged modules.
- **Provider contract tests** — updated Qiniu and Tencent real-cloud
  skip cases to match the current contract suite names and provider
  behavior.

## [v0.2.0]

### Added

- **`pkg/uos/streamio`** — new sub-package providing
  `streamio.Writer`, an `io.WriteCloser` that wraps the unified
  `MultipartService` lifecycle. Callers stream bytes; the helper
  buffers to ≥5 MiB part boundaries, auto-initiates multipart on
  first overflow, uploads parts as they fill, and at `Close` either
  commits via `Complete` (multipart path) or issues a single `Put`
  (small-object fast path when total ≤ `SmallObjectThreshold`).
  `Abort` releases vendor-side multipart state without committing
  (idempotent; safe before/after `Close`). Sticky write errors
  surface on subsequent `Write` calls; `Close` attempts an Abort to
  release vendor state and returns the original error. Works on
  **all 10 v1 providers** because every shipped driver supports
  `MultipartService` natively — no provider-specific code in the
  helper. Compile-time `var _ io.WriteCloser = (*Writer)(nil)`
  asserts the interface contract. Unit tests use an in-memory fake
  `uos.Client` to verify the lifecycle without Docker; runnable
  end-to-end demo at [`examples/streaming_write/`](examples/streaming_write/)
  streams 12 MiB through the helper against MinIO and verifies
  sha256 round-trip.
- **`examples/streaming_write/`** — runnable demo of `streamio.Writer`.
  Generates 12 MiB of synthetic log lines (~144k lines), streams
  them into a new object (auto-promoted to 3-part multipart at the
  5 MiB / 5 MiB / 2 MiB boundaries), reads the object back, and
  verifies byte-for-byte sha256 integrity. Defaults to local MinIO
  via env vars; pivots to AWS or any S3-compatible endpoint via
  `OMC_STREAM_*` env vars.

- **`providers/qiniu/v0.1.0`** (M5, native non-HMAC driver against
  Qiniu Kodo via `github.com/qiniu/go-sdk/v7 v7.26.10`): full
  `pkg/uos.Client` surface (Bucket / Object / Multipart / Signer).
  `AuthCustom` only — Qiniu's Upload Token / Download Token / Manage
  Token are derived from the same AK/SK pair via different signing
  scopes, so a single `qiniu.Credentials` payload absorbs all three
  via the existing `credential.Credential.Opaque any` escape hatch
  (no `pkg/uos/credential` change). Object plane uses
  `storage.FormUploader.Put` for small objects;
  `storage.BucketManager.Stat/Delete/Copy/ListFiles` for object ops;
  HTTP GET against `storage.MakePrivateURL(domain, key, deadline)`
  for downloads (requires `DriverConfig.Domain` — fail-fast at
  config or first-call when missing). Multipart maps onto
  `storage.ResumeUploaderV2.InitParts/UploadParts/CompleteParts`
  with in-process session tracking (Qiniu RUv2 returns server-side
  uploadId; sessions kept in-process for `Abort` lifecycle and
  block-list tracking). Bypasses `pkg/uos/transfer.Manager`.
  `Signer.SignURL` (read) via `MakePrivateURL`; `SignURL` (write)
  returns `*uos.Error{Code: ErrUnsupported, Capability:
  CapSignedURLWrite, Reason: "qiniu write authorization is
  non-URL; use IssueDirectGrant"}` per matrix footnote 4.
  **`Signer.IssueDirectGrant` validates DirectGrantModeToken in a
  NEW context** — for `req.Operation==DirectGrantUpload`, builds
  the Upload Token via `storage.PutPolicy{Scope, Expires, FsizeLimit,
  MimeLimit, ...}.UploadToken(creds)` and returns
  `*uos.DirectGrant{Mode: DirectGrantModeToken, Token: <upload-token>,
  URL: <upload-host>, Method: "POST"}`. 8 vendor-specific PutPolicy
  override keys recognized via `req.Extra` (`callbackUrl`,
  `callbackBody`, `callbackHost`, `callbackBodyType`, `returnBody`,
  `returnUrl`, `saveKey`, `persistentOps`). For
  `req.Operation==DirectGrantDownload`, returns Download Token (signed
  URL embedded as Token field) — also `Mode: DirectGrantModeToken`,
  pragmatic encoding choice that keeps both upload + download under
  one dispatch shape. The frozen 4-mode set was sufficient — no new
  mode needed; this is the SECOND validation of `DirectGrantModeToken`
  after Azure SAS (M4), confirming the mode is the right dispatch
  axis for opaque-bearer auth shapes. `error_map.go` houses a LOCAL
  `mapQiniuReason` switch (~20 reason-string prefixes/contains
  matched against `*qclient.ErrorInfo` HTTP-style errors and SDK
  sentinels `ErrBucketNotExist`/`ErrNoSuchFile`); `s3common.MapCodeString`
  deliberately NOT extended (Qiniu is non-S3-family). Test:
  `TestRunSuite` SKIPs by default (Qiniu wire dialect ≠ MinIO S3
  SigV4); cloud-nightly env vars `OMC_QINIU_NIGHTLY_KEY`/`_SECRET`/
  `_BUCKET`/`_ZONE` (+ optional `_DOMAIN`) gate the real-Qiniu
  contract; 6 SkipCases registered (versioning, acl,
  sign_url_write, issue_direct_grant_shape, multipart_resume,
  multipart_list).

- **`providers/upyun/v0.1.0`** (M5, native non-HMAC driver against
  Upyun USS via `github.com/upyun/go-sdk/v3 v3.0.4`): full
  `pkg/uos.Client` surface. `AuthCustom` default (signature auth
  preferred); `AuthSharedKey` accepted as basic-auth fallback
  (documented as discouraged for security and rate-limit reasons).
  `DriverConfig.Bucket` is Upyun's "service name" (1:1 mapping to
  unified Bucket — Upyun has no concept of "create bucket" since
  services are portal-provisioned: `BucketService.Create/Delete`
  return `ErrUnsupported{CapBucketCRUD}` cleanly). Object plane uses
  upyun SDK's `Get`/`Put`/`Delete`/`Mkdir`/`List`. Multipart maps
  onto upyun's chunked-upload via `X-Upyun-Multi-*` headers
  (resumable upload with 0-based `Part-Id` translated to/from
  `pkg/uos.PartNumber` 1-based at the boundary). Bypasses
  `pkg/uos/transfer.Manager`. Metadata: `x-upyun-meta-*` on the wire
  via `s3common.LowerMetadataKeys` for case-folding. `Signer.SignURL`
  (download) issues URL-shaped signed download via Upyun signature
  mechanism (`_upt=<expiration>/<full-path>/<signature>` query
  params per matrix footnote 3); `SignURL` (write) returns
  `*uos.Error{Code: ErrUnsupported, Capability: CapSignedURLWrite,
  Reason: "upyun upload authorization is FORM-based; use
  IssueDirectGrant"}`. **`Signer.IssueDirectGrant` validates the
  LAST frozen DirectGrantMode value: `DirectGrantModeForm`** — for
  `req.Operation==DirectGrantUpload`, builds the Upyun FORM
  authorization (policy JSON with bucket / save-key / expiration /
  optional content-length-range / optional notify-url from
  `req.Extra["notify-url"]`, base64-encoded; HMAC signature over
  policy + operator) and returns:
    - `Mode: DirectGrantModeForm`
    - `URL: https://v0.api.upyun.com/<bucket>`
    - `Method: "POST"`
    - `Headers: {Authorization: "UPYUN <op>:<sig>"}` (carried for
      callers preferring header-form auth)
    - `FormFields: {policy, authorization, optional content-md5,
      optional x-upyun-meta-*}`
    - `Token: ""` (not used for Form mode)
  6 vendor-specific policy keys recognized via `req.Extra`
  (`notify-url`, `apps`, `expiration-override`, `save-key`,
  `content-md5`, `allow-file-type`). For
  `req.Operation==DirectGrantDownload`, returns
  `*uos.Error{Code: ErrUnsupported, Capability: CapDirectGrant,
  Reason: "upyun download authorization is URL-based; use SignURL"}`
  — Upyun supports mixed Signer dispatch (download URL via SignURL,
  upload Form via IssueDirectGrant). **The existing DirectGrant
  struct fields (Mode/URL/Method/Headers/FormFields/Token) cleanly
  absorbed Upyun FORM upload with zero widening**; this completes
  the 4-mode frozen-set validation: URL (S3-family read presign) /
  Form (Upyun upload) / Token (Azure SAS, Qiniu upload+download) /
  Headers (still unused but available — future vendor candidate).
  `error_map.go` houses a LOCAL `mapUpyunErrorCode` switch
  (~30 entries on Upyun's 8-digit numeric codes, e.g.
  `40400000+ "file or directory not found"` → `ErrNotFound`,
  `40300000+ "username password error"` → `ErrUnauthenticated`);
  `s3common.MapCodeString` deliberately NOT extended. SDK
  context-cancellation is best-effort (upyun SDK doesn't accept
  `context.Context`; driver wraps every call in goroutine + select);
  v0.2.0 candidate to wire SDK to context-aware http.Client. Test:
  `TestRunSuite` SKIPs by default; cloud-nightly env vars
  `OMC_UPYUN_NIGHTLY_BUCKET`/`_OPERATOR`/`_PASSWORD` gate
  real-Upyun contract.

- **`providers/gcs/v0.1.0`** (M4, native non-HMAC driver against
  Google Cloud Storage via `cloud.google.com/go/storage v1.62.1`):
  full `pkg/uos.Client` surface (Bucket / Object / Multipart /
  Signer). `AuthOAuth2` default with Service Account JSON, ADC, or
  Workload Identity Federation; `AuthHMAC` available for HMAC keys.
  GCS resumable upload mapped onto `MultipartService` with two
  documented scope concessions: (a) sequential-only — out-of-order
  `UploadPart` returns `ErrInvalidArgument` because the SDK's
  `*storage.Writer` enforces contiguous byte ranges; (b)
  per-process upload session registry — `MultipartService.List`
  returns an empty page because the SDK does not expose the
  resumable session URL from the high-level Writer. Both are
  documented in `provider_roadmap.md` Lessons (M4); neither
  requires a `pkg/uos` change. Bypasses `pkg/uos/transfer.Manager`.
  `Signer.SignURL` uses V4 signing; if the resolved credential
  lacks a private key (ADC w/o key, Compute Engine, GKE Workload
  Identity), `SignURL` returns `*uos.Error{Code: ErrUnsupported,
  Capability: CapSignedURLRead/Write, Reason: "credential lacks
  signing key"}` per matrix footnote 1. `Signer.IssueDirectGrant`
  returns `ErrUnsupported{CapDirectGrant}` per matrix footnote 5.
  Versioning uses GCS generation-int64 numbers, formatted as
  decimal strings round-tripped through `ObjectInfo.VersionID` and
  `req.VersionID`. `error_map.go` houses a LOCAL
  `mapGoogleAPIReason` switch (~25 GCS-specific reason strings →
  14 frozen `Code`s); `s3common.MapCodeString` was deliberately
  NOT extended (GCS is non-S3-family). `s3common.MapHTTPStatus` +
  `MapContextErr` + `IsRetryable` + `LowerMetadataKeys` reused as
  wire-protocol-agnostic helpers. SDK-internal retry disabled via
  `*storage.Client.SetRetry(storage.WithMaxAttempts(1),
  storage.WithPolicy(storage.RetryNever))`. Test: `TestRunSuite`
  SKIPs by default (GCS dialect ≠ MinIO S3 SigV4); cloud-nightly
  env vars `OMC_GCS_NIGHTLY_KEY`/`_BUCKET`/`_PROJECT` gate the
  real-GCS contract suite. `TestSpawnMinIOSmoke` runs in PR gate.

- **`providers/azure/v0.1.0`** (M4, native non-HMAC driver against
  Azure Blob Storage via
  `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob v1.6.4`):
  full `pkg/uos.Client` surface. `AuthSharedKey` default
  (AccountName + AccountKey); `AuthSAS` (token string) and
  `AuthCustom` (Entra ID / user-delegation key) supported via
  per-scheme `azblob` constructor dispatch. `DriverConfig.StorageAccount`
  required (Azure has no S3-style "region" — Storage Account
  encodes location). Container ↔ `Bucket` 1:1. Azure Block Blob
  mapped onto `MultipartService`: `PartNumber` synthesises
  base64-encoded block IDs; `Complete` issues `PutBlockList`.
  Block Blob's 4 MiB minimum staging-block size differs from S3's
  5 MiB — sub-4-MiB parts get `ErrInvalidArgument` from Azure at
  `StageBlock` time; documented in `provider_roadmap.md`
  Lessons (M4) as a deferred `Capabilities().MinPartSize` v0.2.0
  candidate (needs ≥2 providers per ADR rule). Bypasses
  `pkg/uos/transfer.Manager`. **DirectGrant via `DirectGrantModeToken`
  validates the frozen 4-mode set** — `Signer.IssueDirectGrant`
  returns the SAS query-string in `DirectGrant.Token`; caller
  constructs the final URL as `DirectGrant.URL + "?" +
  DirectGrant.Token`; `DirectGrant.Headers` carries `x-ms-version`
  for protocol-version pinning. `Signer.SignURL` uses SAS with
  start-time set to `now − 5min` for clock-skew tolerance and
  expiry per `request.ExpiresIn`; the start-time fits inside the
  existing `SignURLRequest` shape — no surface change. Account-key
  SAS works with `AuthSharedKey`; user-delegation SAS requires
  `AuthCustom` and a `GetUserDelegationCredential` round-trip
  (validity = `ExpiresIn + 5min`). `error_map.go` houses a LOCAL
  `mapAzureErrorCode` switch (~50 Azure ErrorCodes → 14 frozen
  `Code`s) keyed on `*azcore.ResponseError.ErrorCode`;
  `s3common.MapCodeString` deliberately NOT extended.
  `CapObjectACL` returns `ErrUnsupported{CapObjectACL}` at call
  time per matrix footnote 11 — Azure has no per-object ACL
  surface; access controlled via SAS / RBAC / per-blob SAS with
  restricted permissions. `CapVersioning` returns
  `ErrUnsupported{CapVersioning, Reason: "blob versioning is not
  enabled at the storage account level"}` if the account lacks
  it (footnote 8). `VersionID` is applied via `blob.Client.WithVersionID()`
  scoping (not an option field). SDK-internal retry disabled via
  `azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: 0}}`.
  Test: `TestRunSuite` SKIPs by default; cloud-nightly env vars
  `OMC_AZURE_NIGHTLY_ACCOUNT`/`_KEY`/`_CONTAINER` gate real-Azure
  contract. `TestSpawnMinIOSmoke` runs in PR gate.

- **`providers/tencent/v0.1.0`** (M3 phase 2, native HMAC against
  Tencent COS via `cos-go-sdk-v5`): full `pkg/uos.Client` surface
  (Bucket / Object / Multipart / Signer); presign via COS HMAC v1
  (`q-sign-algorithm=sha1`); auto-batches `DeleteMany` at the COS
  1000-key cap. Tencent-specific quirk: bucket names MUST contain
  the `-<appid>` suffix (e.g. `examplebucket-1250000000`); driver
  auto-suffixes via `DriverConfig.AppID` when unsuffixed name is
  passed. Per-bucket `*cos.Client` design (cos-go-sdk-v5 binds
  BucketURL to client) handled via shared http.Client + per-call
  bucketClient() that reuses signing creds + connection pool.
  CRC64 integrity verification ON by default. Bypasses
  `pkg/uos/transfer.Manager` (consistent with M2 + Uploader). Test:
  `TestRunSuite` SKIPs by default (COS HMAC v1 ≠ AWS SigV4); cloud-
  nightly env vars `OMC_TENCENT_NIGHTLY_KEY/_SECRET/_BUCKET/_REGION/
  _APPID` gate real-COS contract.

- **`providers/huawei/v0.1.0`** (M3 phase 2, native HMAC against
  Huawei OBS via `huaweicloud-sdk-go-obs`): full `pkg/uos.Client`
  surface; presign via `obs.CreateSignedUrl` (HMAC v2 / v4
  selectable through `DriverConfig.Signature`); `Close()` actually
  releases the SDK's internal http.Transport pool (alibaba/aws/
  minio drivers have no Close to call). Huawei-specific quirk:
  region/endpoint pairing is **strict** — wrong pairing produces
  silent HTTP 301/307 redirects rather than a clean
  `ErrInvalidArgument`. Driver makes `Endpoint` mandatory in
  `Validate` (no auto-derivation from Region) so misconfiguration
  surfaces at construction time; documented in three places (Validate
  doc, Open doc, package doc). Bypasses `transfer.Manager`. Test:
  `TestRunSuite` SKIPs by default (OBS HMAC ≠ AWS SigV4); cloud-
  nightly env vars `OMC_HUAWEI_NIGHTLY_KEY/_SECRET/_BUCKET/_ENDPOINT`
  (+ optional `_REGION`) gate real-OBS contract.

- **`providers/volcengine/v0.1.0`** (M3 phase 2, native HMAC
  against Volcengine TOS via `ve-tos-golang-sdk/v2`): full
  `pkg/uos.Client` surface; presign via `tos.PreSignedURL`. TOS
  uses SigV4-style signing but with `defaultServiceName = "tos"`
  instead of `"s3"`, making the wire dialect incompatible with
  MinIO's S3 SigV4 verifier. Endpoint convention auto-derived as
  `https://tos-<region>.volces.com` when absent. Storage class
  pass-through: TOS supports `STANDARD`/`IA`/`INTELLIGENT_TIERING`/
  `ARCHIVE_FR`/`ARCHIVE`/`COLD_ARCHIVE`/`DEEP_COLD_ARCHIVE`
  (note `ARCHIVE_FR` = "Archive Frequent Restore", TOS-specific
  with no AWS direct equivalent — closest analog is `GLACIER_IR`).
  Volcengine-specific quirk: `ListObjectsV2` uses Marker/NextMarker
  pagination (NOT S3 V2 `ContinuationToken` — that lives in TOS's
  separate `ListObjectsType2` API); driver maps unified opaque
  cursor to Marker. `MaxRetryCount` exposed via `DriverConfig` for
  callers needing vendor-level retry escape hatch (default 0 =
  disabled). Bypasses `transfer.Manager`. Test: `TestRunSuite`
  SKIPs by default; cloud-nightly env vars
  `OMC_VOLCENGINE_NIGHTLY_KEY/_SECRET/_BUCKET/_REGION/_ENDPOINT`
  gate real-TOS contract.

- **`providers/alibaba/v0.1.0`** (M3 first driver, native HMAC against
  Alibaba OSS via `aliyun-oss-go-sdk` v3): full `pkg/uos.Client`
  surface (Bucket / Object / Multipart / Signer); presign via OSS
  HMAC v1/v4 (selectable through `DriverConfig.AuthVersion`); CNAME
  mode opt-in via `DriverConfig.UseCNAME`; auto path-style for
  non-`aliyuncs.com` endpoints. `error_map.go` is **132 LoC**
  (vs AWS's 169 LoC vs minio's 103 LoC) — the s3common extraction
  validated cleanly against a non-AWS S3-family vendor. The driver
  bypasses `pkg/uos/transfer.Manager` (same as AWS+MinIO; vendor
  SDK encodes its own multipart orchestration); the Uploader
  interface contract pre-tag amendment makes this first-class.
  Capabilities matrix matches the alibaba column of
  `docs/provider_matrix.md` (9 ✅ / 1 ❌ / 2 🟡 / 1 🧩 — same shape
  as AWS+MinIO). `driver_test.go` SKIPs `TestRunSuite` by default
  (OSS HMAC ≠ AWS SigV4 → cannot authenticate against testcontainers
  MinIO); cloud-nightly env vars (`OMC_ALIBABA_NIGHTLY_*`) gate the
  real-OSS contract suite. The `TestSpawnMinIOSmoke` PR-gate test
  validates testkit wiring.
- **`pkg/uos/s3common.MapCodeString` extension** — 10 OSS-specific
  error codes added (gap-fill driven by the alibaba driver's wire
  surface review):
  - → `ErrNotFound`: `NoSuchObjectVersion`, `KmsKeyNotFound`
  - → `ErrConflict`: `BucketVersioningSuspended`,
    `InvalidEncryptionAlgorithmError`, `RestoreAlreadyInProgress`,
    `BucketReplicationException`
  - → `ErrInvalidArgument`: `InvalidLocationConstraint`,
    `MalformedAclError`, `RequestIsNotMultiPartContent`,
    `EntityTooSmallError` (the OSS "Error"-suffixed alias of
    `EntityTooSmall` already in the table)
  All 10 carry test cases in `s3common_test.go`. Additive — does
  not affect AWS or MinIO drivers; M3 tencent/huawei/volcengine
  drivers consume them transparently from day one.

- **`pkg/uos/s3common`** (new public subpackage of `pkg/uos`):
  shared S3-family wire-protocol mappings used by `providers/aws`,
  `providers/minio`, and the future M3+ 国云 drivers
  (alibaba/tencent/huawei/volcengine). Five exported helpers, all
  stdlib-only:
  - `MapCodeString(code) (uos.Code, bool)` — S3-compat error code
    string → `uos.Code` table covering every wire code that
    resolves to one of the 14 frozen `pkg/uos.Code` values.
  - `MapHTTPStatus(status) (uos.Code, bool)` — HTTP status fallback
    when the vendor didn't supply a recognised code string.
  - `MapContextErr(err) (uos.Code, bool)` — context cancellation /
    deadline → `uos.ErrTimeout`.
  - `IsRetryable(code) bool` — marks the three retryable Codes
    (`RateLimited` / `Timeout` / `Temporary`).
  - `LowerMetadataKeys(m) map[string]string` — case-folds metadata
    map keys; collapses nil and empty to nil so vendor SDKs see
    "no metadata" rather than "explicit empty".
  All five carry table-driven tests (`s3common_test.go`).

### Changed

- **`providers/aws/error_map.go`** (240 → 169 LoC, ~30 % shrink):
  generic `smithy.APIError` code dispatch + HTTP-status fallback +
  context cancellation handling now delegate to `s3common`. The
  vendor-typed-error switch (e.g. `*types.NoSuchKey`) stays inline
  because `aws-sdk-go-v2` typed shapes carry richer message text
  than the wire-level code string.
- **`providers/minio/error_map.go`** (185 → 103 LoC, ~45 % shrink):
  the entire `mapErrorCode` body collapses into a 4-line decision
  tree over `s3common.MapCodeString` → `MapHTTPStatus` →
  `MapContextErr` → `uos.ErrInternal` catch-all. `miniogo`'s
  package-level constants (`miniogo.NoSuchKey`, etc.) are
  string-typed so `s3common.MapCodeString(string(resp.Code))`
  resolves them transparently.
- **`providers/{aws,minio}/driver.go`** metadata case-folding:
  `metadataToAWS` / `metadataFromAWS` / `toLowerMap` are now thin
  one-line adapters over `s3common.LowerMetadataKeys`. Behaviour
  unchanged (nil and empty input still collapse to nil; mixed-case
  keys still fold to lower-case).

### Resolved

- **ADR Follow-up #4 — s3common extraction**: originally planned
  for "M3+ once two S3-family drivers have shipped." Architect's
  M2 review recommended extracting at the FIRST M3 driver landing
  (i.e. ahead of Alibaba/Tencent) rather than waiting for the
  second. Extraction is now landed pre-tag with the existing M2
  duplication as the proof; M3 drivers consume the shared helpers
  from day one rather than duplicating the wire-level mappings.

### Added (M6 phase 2+3 — examples, benchmarks, polish docs)

- **`examples/multipart/`**: runnable end-to-end demo of
  `MultipartService.Initiate` → `UploadPart` × 3 (5 MiB each) → `Complete`,
  plus an `Abort` lifecycle demo. Defaults to local MinIO via env-var
  config; pivots to AWS or any other provider via `OMC_MULTIPART_PROVIDER`.
- **`examples/direct_grant_qiniu/`**: Qiniu Upload Token + Download URL
  DirectGrant demo. Educational (placeholder credentials produce
  structurally-correct output). Demonstrates the M5 `DirectGrantModeToken`
  semantic + the v0.1.1 Download `DirectGrantModeURL` correction. Lists the
  8 PutPolicy override `req.Extra` keys recognized by the qiniu driver.
- **`examples/direct_grant_upyun/`**: Upyun FORM upload DirectGrant demo.
  Validates the M5 final-frozen `DirectGrantModeForm` shape (the LAST of
  4 frozen DirectGrantMode values exercised in production). Shows the
  Qiniu-vs-Upyun side-by-side dispatch comparison.
- **`benchmarks/`**: per-provider Put / Get / Multipart / SignURL throughput
  benchmark scaffold. M6 phase 2 baseline ships AWS + MinIO benchmarks
  (S3-family); per-vendor sweeps for the 8 non-S3 drivers land at
  v1.0.0 cut. Uses `testcontainers-go` MinIO (Docker required); standalone
  module (`GOWORK=off go test -tags=docker -bench=.`) so transitive
  testcontainers deps stay out of the root module's chain.
- **`docs/migration_guide.md`**: vendor-SDK → pkg/uos migration walkthrough.
  Covers Open / Put / Get / Multipart / Sign translation patterns; per-
  vendor SDK mapping table; capability discovery; error handling
  translation; multi-vendor support pattern; explicit "when NOT to migrate"
  guidance.
- **`docs/otel_alignment.md`**: M6 phase 3 audit of `pkg/uos/middleware`'s
  observability surface against the OpenTelemetry Semantic Conventions for
  Object Stores (OTel spec v1.27.0). Per-field alignment matrix +
  recommended span name convention + 5 v0.2.0 work items + redaction
  status (verified complete across all 10 providers) + reference exporter
  sketch.
- **`docs/v1_polish_audit.md`**: M6 phase 3 v1.0.0 readiness review.
  Covers the 3 architecture_plan §M6 audit areas: idempotency markers
  (verdict: READY — DefaultIsIdempotent's conservative default + opt-in
  Patterns A/B/C); retry-budget guards (READY — 10 drivers verified to
  honor single-source-of-truth); log redaction (READY — no gaps across
  10 providers). Final v1.0.0 readiness verdict: READY pending
  Appendix A consolidation + maintainer tag pass.
- **`.gitignore`**: explicit ignore for compiled `examples/<name>/<name>`
  binaries (M6 Phase 2 executors built per-example binaries during
  verification, polluting `git status`).

### Fixed (post-M5 v0.1.1 patch — architect-flagged correctness items)

- **`providers/azure` multipart `Initiate.Metadata` round-trip
  to `Complete`**: `uploadSession.metadata()` returned `nil`
  unconditionally, dropping caller-supplied metadata at
  `CommitBlockList` time. Now `uploadSession` carries a `meta
  uos.Metadata` field captured at `Initiate` and replayed at
  `Complete`. Only Azure was affected (other multipart drivers
  pass metadata directly through their SDKs' Init APIs); the
  PR-gate contract suite did not exercise multipart-with-metadata
  so the gap silently slipped through M4. Architect-flagged in
  M4 review; fixed in v0.1.1.
- **All 9 non-AWS drivers' `mapError()` `errors.As(&alreadyMapped)`
  early-return now augments the inner `*uos.Error` with caller
  context** (Provider / Operation / Bucket / Key) when those
  fields are empty, instead of identity-passing through and
  losing the outer mapError's context. Affects `providers/{alibaba,
  azure, gcs, huawei, minio, qiniu, tencent, upyun, volcengine}`.
  AWS uses a different `mapError` shape with no
  `errors.As(&alreadyMapped)` guard, so it was not affected.
  Cross-driver consistent fix; architect-flagged in M5 review.
- **`providers/qiniu` `Signer.IssueDirectGrant(Download)` Mode
  changed from `DirectGrantModeToken` to `DirectGrantModeURL`**.
  Qiniu's Download "Token" is technically a `MakePrivateURL`
  signature embedded in a URL — `Mode=URL` tells the truth
  (callers GET `DirectGrant.URL` directly; no opaque bearer
  token semantics apply). The M5 ship used `Mode=Token` for
  cross-operation dispatch symmetry; architect-flagged as
  doc-clarity fix. Upload path unchanged (still
  `Mode=DirectGrantModeToken` — Qiniu Upload Token IS a true
  bearer-string-in-form-field). `DirectGrant.Token` field is no
  longer set on Download (was redundant under `Mode=URL`).

## [providers/aws/v0.1.0] — 2026-04-28 — M2 (AWS native driver)

First tagged release of the AWS S3 native driver. Implements every
`pkg/uos.Client` method against `aws-sdk-go-v2 + service/s3`. Passes
the cross-provider contract test kit (`pkg/testkit/contract.RunSuite`)
against a `testcontainers-go` MinIO endpoint in S3-compat mode.

### Added

- `providers/aws/factory.go`: `factoryImpl` registers itself on
  `pkg/uos.DefaultRegistry` via `init()`. `Provider() = "aws"`,
  `Validate(cfg)` requires Region, `Open(ctx, cfg)` constructs an
  `aws.Config` with a custom `EndpointResolverV2` (when
  `cfg.Endpoint` is set, for S3-compat targets), `aws.NopRetryer{}`
  (deliberate — pkg/uos owns retry per `RetryPolicy`), and a
  credentials adapter that pulls AK/SK from
  `cfg.CredentialProvider`. Path-style addressing on opt-in via
  `DriverConfig.PathStyle` (forced when a custom endpoint is set).
- `providers/aws/driver.go`: `driverImpl` implements `Client` plus
  the four sub-services (`BucketService`, `ObjectService`,
  `MultipartService`, `Signer`). Notable choices:
  - Multipart uses raw `s3.CreateMultipartUpload` /
    `UploadPart` / `CompleteMultipartUpload` (does not route
    through `pkg/uos/transfer.Manager` — see Notes below).
  - `DeleteMany` auto-batches keys into S3's 1000-per-request cap.
  - `Signer.SignURL` uses `s3.PresignClient`. `IssueDirectGrant`
    returns `ErrUnsupported{Capability: CapDirectGrant}` per
    matrix footnote 5 (S3-family uses presigned URL).
  - `As(target)` exposes `**s3.Client` and `**s3.PresignClient`.
- `providers/aws/error_map.go`: translates `*types.NoSuchKey`,
  `*types.NoSuchBucket`, `*types.BucketAlreadyExists`,
  `*types.BucketAlreadyOwnedByYou`, `*types.NotFound`, and generic
  `smithy.APIError` codes into the 14 frozen `pkg/uos.Code`
  values; HTTP status fallback for unmapped errors. `RequestID`
  and `SecondaryID` populated from awsmiddleware metadata.
- `providers/aws/capabilities.go`: returns the 13-cell
  `capability.Report` matching the aws column of
  `docs/provider_matrix.md` (9 ✅, 2 🟡 [Versioning, ObjectACL —
  see footnotes 13/14], 1 ❌ [DirectGrant], 1 🧩 [NativeMove]).
- `providers/aws/driver_test.go` (build tag `docker`): spawns
  MinIO via `pkg/testkit/contract.SpawnMinIO`, configures the
  AWS SDK to point at the S3-compat endpoint, runs the contract
  suite end-to-end. 28 PASS, 17 SKIP (3 driver-level SkipCases
  for MinIO/SDK canonicalisation drift on `?` and `%FF` keys —
  cloud-nightly will validate against real AWS).

### Notes

- Bypassed `pkg/uos/transfer.Manager` in favor of raw
  `s3.UploadPart` orchestration. See `RELEASING.md` §5
  (ADR Follow-up #1) — this answer, plus the parallel MinIO
  driver's identical bypass, motivates the v0.2.0 `Uploader`
  interface refactor that the Architect originally proposed.
- `go.mod` floor is `go 1.25.0` because `aws-sdk-go-v2 v1.41.6+`
  transitively requires it. Root `pkg/uos` remains at `go 1.22`.
- Real AWS smoke tests are gated by the cloud-nightly workflow
  (`.github/workflows/cloud-nightly.yml`) which exits SKIP when
  `OMC_AWS_NIGHTLY_KEY` / `OMC_AWS_NIGHTLY_SECRET` are absent.

## [providers/minio/v0.1.0] — 2026-04-28 — M2 (MinIO native driver)

First tagged release of the MinIO native driver. Implements every
`pkg/uos.Client` method against `minio-go/v7`. Passes the
cross-provider contract test kit against a `testcontainers-go`
MinIO endpoint.

### Added

- `providers/minio/factory.go`: `factoryImpl` registers on
  `pkg/uos.DefaultRegistry` via `init()`. `Provider() = "minio"`,
  `Validate(cfg)` requires Endpoint + CredentialProvider,
  `Open(ctx, cfg)` constructs a `minio.Client` with
  `BucketLookup: BucketLookupPath` (path-style is the MinIO
  default) and `MaxRetries: 1` (pkg/uos owns retry per
  `RetryPolicy`).
- `providers/minio/driver.go`: `driverImpl` implements `Client`
  plus the four sub-services. Notable choices:
  - `Get` uses `minio.Core.GetObject` (raw API) instead of the
    high-level streaming reader, because the latter ignores
    explicit `Range` options. Required for the contract suite's
    `range_returns_slice` case.
  - Multipart delegated to `minio.Client.PutObject` (vendor
    handles size-based dispatch + parallel parts + abort) plus
    `Core` for raw `Initiate/UploadPart/Complete/Abort`.
  - `Signer.SignURL` uses `minio.PresignedGet/Put`.
    `IssueDirectGrant` returns the same typed-Unsupported error
    as AWS.
  - `As(target)` exposes `**minio.Client` and `**minio.Core`.
- `providers/minio/error_map.go`: translates `minio.ErrorResponse`
  codes (`NoSuchKey`, `NoSuchBucket`, `BucketAlreadyOwnedByYou`,
  `AccessDenied`, `SignatureDoesNotMatch`, `SlowDown`,
  `RequestTimeout`, etc.) into the 14 frozen `pkg/uos.Code`
  values. Catch-all is `ErrInternal` with `Cause` populated.
- `providers/minio/capabilities.go`: same shape as the AWS
  driver (S3-family).
- `providers/minio/driver_test.go` (build tag `docker`): spawns
  MinIO via the testkit helper, runs the contract suite. All
  cases pass; 1 driver-level `SkipCases` entry for
  `signer/issue_direct_grant_shape` (the capability-gating case
  for the same path passes — proving the typed-Unsupported
  contract).

### Notes

- Bypassed `pkg/uos/transfer.Manager` for the same reason as the
  AWS driver. See ADR Follow-up #1 in `RELEASING.md` §5.
- `go.mod` floor is `go 1.22` (matches root). `minio-go/v7` does
  not require Go 1.25.

## [pkg/uos/v0.1.0] — 2026-04-28 — M1 (Core Skeleton)

First tagged release of the universal object storage client SDK. Ships
the public API surface and shared internals; zero provider drivers.
M2 (AWS + MinIO) is unblocked from day one.

### Added

- **Public API in `pkg/uos`**:
  - `Client` interface and four sub-services: `BucketService`,
    `ObjectService`, `MultipartService`, `Signer`.
  - `Provider`, `Config`, `Factory`, `Registry`, plus default
    in-process `Registry` (`NewRegistry` / `DefaultRegistry`).
  - 14 frozen `Code` constants (`ErrUnsupported`, `ErrInvalidArgument`,
    `ErrNotFound`, `ErrAlreadyExists`, `ErrPermissionDenied`,
    `ErrUnauthenticated`, `ErrPreconditionFailed`, `ErrConflict`,
    `ErrRateLimited`, `ErrTimeout`, `ErrTemporary`,
    `ErrChecksumMismatch`, `ErrLengthRequired`, `ErrInternal`) and
    the concrete `*Error` type with `Is` / `Unwrap` matching contract.
  - `NewUnsupported` and `WrapMissingCapability` helpers for the
    capability-gap rich error.
  - Request / response value types: `BucketInfo`, `ObjectInfo`,
    `ObjectReader`, `ObjectList`, `Metadata`, `ContentHeaders`,
    `Checksum`, `DirectGrant`, `SignedURL`, plus per-service request
    families in `request_bucket.go`, `request_object.go`,
    `request_multipart.go`, `request_signer.go`.
  - `MultipartUpload` and `MultipartUploadList` shapes (Critic R1
    sign-off addition: explicit field set, not inferred).
- **`pkg/uos/capability`**: 13 frozen `Capability` constants
  (`bucket.crud`, `object.crud`, `object.list.prefix_delimiter`,
  `object.range_read`, `object.multipart_upload`, `signer.url_read`,
  `signer.url_write`, `signer.direct_grant`, `object.tagging`,
  `bucket.versioning`, `object.acl`, `object.encryption.managed`,
  `object.native_move`); `Availability` enum; `Report` with `Get` /
  `Has` / `Require` helpers and `MissingCapability` sentinel.
- **`pkg/uos/credential`**: `Provider` interface, `Credential` struct
  with `AuthScheme` enum, `StaticProvider`, `EnvProvider` (reads
  `OSC_*` and AWS-compatible `AWS_*` vars; AWS coupling tracked as
  v0.2.0 cleanup), `Chain` (first-success traversal).
- **`pkg/uos/transfer`**: `Manager` skeleton with planner, bounded
  worker pool, abort-on-failure semantics, and resume hook backed by
  a `StateStore` (memory implementation included). Local adapter
  types `UploadRequest` and `MultipartServiceLike` documented as
  the cycle-avoidance pattern (Critic R1 / Architect R3.ii).
- **`pkg/uos/middleware`**: `Logger`, `Metrics`, `Tracer` contracts;
  composer `Chain`; redaction list of 11 sensitive headers and 12
  sensitive query params.
- **`pkg/uos/httpx`**: `HTTPConfig` (Timeout, Proxy, RootCAs,
  InsecureSkipVerify, MaxIdleConns) and `NewClient` constructor;
  emits a runtime warning when `InsecureSkipVerify` is set.
- **`pkg/testkit/contract` (separate Go module)**: `RunSuite(t, FactoryUnderTest)`
  plus case files for bucket, object, multipart, signer, capability,
  and error coverage. `minio.go` (build tag `docker`) wraps a
  `testcontainers-go` MinIO container for live cross-provider checks.
  Lives at `github.com/maqian/oss-client/pkg/testkit/contract`
  with its own `go.mod`, isolating the testcontainers / Docker / OTel
  transitive chain from `pkg/uos` consumers.
- **Frozen surface fence**: `pkg/uos/surface_test.go` /
  `TestFrozenSurface` — three subtests literal-pin the 14 Codes,
  13 Capabilities, and 4 DirectGrantModes (Critic R1 binding).
- **`Uploader` and `Downloader` interfaces** (pre-tag fold-in
  resolving ADR Follow-up #1): structural one-method interfaces
  in `pkg/uos/uploader.go`. `ObjectService` satisfies both
  implicitly via `Put` / `Get`; the providers/aws and providers/minio
  drivers needed zero code change. Compile-time `var _` assertions
  in `uploader.go` itself catch any future drift in `ObjectService`'s
  `Put`/`Get` signatures. Lets callers depend on upload-only or
  download-only semantics, and lets future drivers (M4 GCS, M5 Upyun)
  satisfy `Uploader` via a `transfer.Manager`-backed wrapper without
  forcing the full `ObjectService` surface.
- **CI**: `.github/workflows/ci.yml` declares five jobs:
  - `unit-root` — matrix `ubuntu-latest`/`macos-latest` × Go `1.22`/`1.23`
    against the root module.
  - `unit-testkit` — same matrix at Go `1.25` against the testkit module.
  - `unit-docker` — `-tags=docker` contract suite from the testkit module.
  - `vet-fmt` — `go vet` and `gofmt -l .` enforced on both modules.
  - `surface` — the `TestFrozenSurface` tripwire on Go `1.22`.
- **Operational**: `Makefile` (test / vet / fmt / add-provider),
  `LICENSE` (Apache-2.0 placeholder), `go.work` enumerating root +
  testkit + future provider modules, `scripts/add-provider.sh` for
  multi-module provider scaffolding (writes `go 1.22` into
  scaffolded provider go.mod, matching the root floor).

### Notes

- **Go directive**: root `go.mod` targets **Go 1.22** (the originally
  planned floor). `pkg/testkit/contract` declares **Go 1.25** because
  its `testcontainers-go` chain transitively requires it; that cost
  is contained inside the testkit module and never reaches `pkg/uos`
  consumers.
- **ADR Follow-up #3 — `pkg/testkit/contract` module hoist** —
  resolved **before tagging v0.1.0**. Originally planned as M6
  (conditional), then provisionally promoted to v0.2.0 mandatory
  during M1, the hoist landed inside the v0.1.0 release. Result:
  root `go.sum` is empty (no third-party transitive entries from the
  contract testkit), satisfying NFR-008 at the module level for the
  first time. See `RELEASING.md` §5 for the post-resolution status.
- No provider drivers ship in v0.1; `providers/<name>/` directories
  arrive in M2+.
