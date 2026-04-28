# Provider Matrix

> **Status**: Binding for v1. Authoritative source for "which provider supports which capability today."
> Companion to `docs/architecture_plan.md` (§7.2 capability model) and `docs/provider_roadmap.md` (per-milestone exit checklists).

## How to read this file

| Symbol | Meaning |
| --- | --- |
| ✅ `Supported` | Provider implements this capability and the contract test for it passes (in PR gate or cloud nightly). |
| 🟡 `Conditional` | Implemented but only under specific config / credential / bucket state. Cell footnote explains. |
| 🧩 `ExtensionOnly` | Underlying provider can do it; `pkg/uos` does not abstract it; reach via `As(target)`. |
| ❌ `Unsupported` | Underlying provider doesn't expose this. `pkg/uos` returns `*Error{Code: ErrUnsupported, Capability: <cap>}`. |
| ⏳ `Planned (M_n_)` | Driver not yet shipped; cell will be filled at the milestone above. |
| — | Not applicable to this provider's auth/data model. |

The matrix below is the **target** v1 state. Cells marked `Planned` get filled during the milestone listed.

## Driver implementation status

| Provider | Driver path | SDK | Driver status | Milestone |
| --- | --- | --- | --- | --- |
| AWS S3 | `providers/aws` | `aws-sdk-go-v2` | **Shipped (v0.1.0)** | M2 |
| MinIO | `providers/minio` | `minio-go/v7` | **Shipped (v0.1.0)** | M2 |
| Alibaba OSS | `providers/alibaba` | `aliyun-oss-go-sdk` | **Shipped (v0.1.0)** | M3 |
| Tencent COS | `providers/tencent` | `cos-go-sdk-v5` | **Shipped (v0.1.0)** | M3 |
| Huawei OBS | `providers/huawei` | `huaweicloud-sdk-go-obs` | **Shipped (v0.1.0)** | M3 |
| Volcengine TOS | `providers/volcengine` | `ve-tos-golang-sdk/v2/tos` | **Shipped (v0.1.0)** | M3 |
| Google Cloud Storage | `providers/gcs` | `cloud.google.com/go/storage` | **Shipped (v0.1.0)** | M4 |
| Azure Blob Storage | `providers/azure` | `azure-sdk-for-go/sdk/storage/azblob` | **Shipped (v0.1.0)** | M4 |
| Qiniu Kodo | `providers/qiniu` | `qiniu/go-sdk/v7` | **Shipped (v0.1.0)** | M5 |
| Upyun USS | `providers/upyun` | `upyun/go-sdk/v3` | **Shipped (v0.1.0)** | M5 |

When a driver ships, change `Planned` → `Shipped (vX.Y.Z)` and replace `⏳ Planned (M_n_)` cells in the capability matrix below with the actual outcome.

## Capability matrix (v1 target)

The 13 v1 capabilities (frozen — see `architecture_plan.md` §7.2). Cells reflect the **target** state at end of v1.0.0.

| Capability \ Provider | aws | minio | alibaba | tencent | huawei | volcengine | gcs | azure | qiniu | upyun |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `CapBucketCRUD` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `CapObjectCRUD` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `CapListPrefixDelimiter` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `CapRangeRead` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `CapMultipartUpload` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `CapSignedURLRead` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | 🟡¹ | 🟡² | ✅ | 🟡³ |
| `CapSignedURLWrite` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | 🟡¹ | 🟡² | 🟡⁴ | 🟡³ |
| `CapDirectGrant` | ❌⁵ | ❌⁵ | ❌⁵ | ❌⁵ | ❌⁵ | ❌⁵ | ❌⁵ | ✅⁶ | ✅ | ✅ |
| `CapObjectTagging` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | 🧩⁷ | 🧩⁷ |
| `CapVersioning` | 🟡¹³ | 🟡¹³ | 🟡¹³ | 🟡¹³ | 🟡¹³ | 🟡¹³ | ✅ | 🟡⁸ | ❌⁹ | ❌⁹ |
| `CapObjectACL` | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁰ | 🟡¹¹ | 🧩⁷ | 🧩⁷ |
| `CapManagedEncryption` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | 🧩⁷ | 🧩⁷ |
| `CapNativeMove` | 🧩¹² | 🧩¹² | 🧩¹² | 🧩¹² | 🧩¹² | 🧩¹² | 🧩¹² | 🧩¹² | 🧩¹² | 🧩¹² |

### Footnotes

1. **GCS Signed URL**: requires a credential that bears a private signing key (Service Account JSON or HMAC keys). Application Default Credentials without a key cannot sign; in that case `Signer.SignURL` returns `Code: ErrUnsupported, Capability: CapSignedURLRead/Write, Reason: "credential lacks signing key"`. Cell becomes ✅ once credential is sufficient.
2. **Azure SAS**: account-key SAS works with `AuthSharedKey`; user-delegation SAS works with `AuthCustom` carrying an Entra-issued user-delegation key. SAS includes a start-time semantic that `SignURL` ignores; use `IssueDirectGrant` for full control.
3. **Upyun FORM/signed URL**: signed URL works for download; upload signed URLs are issued as FORM authorization via `DirectGrant`, not URL.
4. **Qiniu Signed URL Write**: write authorization in Qiniu is Upload Token (non-URL); URL-shaped writes are not idiomatic. `CapSignedURLWrite` is `Conditional` and the recommended path is `CapDirectGrant`.
5. **`CapDirectGrant` on S3-family**: S3-family providers issue write authorization as URL (presigned PUT). They do not have a non-URL grant model, so `CapDirectGrant` is `Unsupported` and callers should use `CapSignedURLWrite` instead.
6. **Azure DirectGrant**: SAS can be expressed as URL (via `SignURL`) or as token + headers (via `IssueDirectGrant`). Both modes are supported.
7. **Qiniu / Upyun ACL / Tagging / Encryption**: these vendors expose ACL / tagging / encryption through bespoke admin APIs that do not map cleanly to the unified semantics. Reach via `As(target any)` and the vendor SDK directly. Cell is `ExtensionOnly`.
8. **Azure Versioning**: requires the storage account to have versioning enabled at the account level; otherwise `CapVersioning` returns `Unsupported` with that reason.
9. **Qiniu / Upyun Versioning**: not exposed as a unified data-plane capability in v1. Cell stays `Unsupported`.
10. **GCS ACL**: GCS Uniform vs Fine-grained access control changes which ACL ops are valid; `Conditional` with reason returned at runtime.
11. **Azure ACL**: Azure does not have S3-style per-object ACLs; the closest analog (per-blob SAS with restricted permissions) is exposed via `Signer`. Object-level ACL surface returns `Unsupported`.
12. **`CapNativeMove` everywhere**: no provider exposes a server-side rename / move that meets the cross-provider semantic bar. Default behavior is `Copy + Delete` via `helpers.Move`. Where a vendor has a true native rename (e.g., HDFS-flavored or via legacy admin APIs), it is reached via `As(target)`. Cell is `ExtensionOnly` everywhere.
13. **S3-family Versioning**: requires bucket-level versioning to be enabled; if disabled, `CapVersioning`-gated calls return `Code: ErrUnsupported, Capability: CapVersioning, Reason: "bucket versioning is not enabled"`. Detected at first call, not at construction.
14. **S3-family ObjectACL**: subject to the bucket's Object Ownership setting. Buckets created with default settings since 2023 have ACLs disabled (`BucketOwnerEnforced`); per-object ACL operations return `Code: ErrUnsupported, Capability: CapObjectACL` in that mode. Buckets with `ObjectWriter`/`BucketOwnerPreferred` ownership accept ACL writes. The cell is `Conditional` for both AWS and MinIO.

## Auth scheme matrix

| Provider | Default `AuthScheme` | Alternate `AuthScheme`s | Notes |
| --- | --- | --- | --- |
| aws | `AuthHMAC` | (STS via temporary AK/SK + session token) | Standard AWS SigV4. |
| minio | `AuthHMAC` | — | Identical SigV4 signing. |
| alibaba | `AuthHMAC` | (STS) | OSS signature v1 / v4 selectable via `DriverConfig.SignatureVersion`. |
| tencent | `AuthHMAC` | (STS) | Includes appid in resource path. |
| huawei | `AuthHMAC` | — | Region/endpoint pairing is strict. |
| volcengine | `AuthHMAC` | (STS) | TOS signature parity with AWS-style. |
| gcs | `AuthOAuth2` | `AuthHMAC` (HMAC keys) | OAuth2 / Service Account / ADC chain. |
| azure | `AuthSharedKey` | `AuthSAS`, `AuthCustom` (Entra / user-delegation) | Storage Account in `DriverConfig`. |
| qiniu | `AuthCustom` | — | Upload / Download / Manage tokens are distinct credentials, all wrapped behind `AuthCustom`. |
| upyun | `AuthCustom` | `AuthSharedKey` (basic auth, fallback only) | Signature auth preferred. |

## What this matrix is NOT

- Not a feature wishlist. Cells marked `🧩 ExtensionOnly` will NOT become `✅ Supported` in v1; v1 explicitly defers them to `As(target)`. Promoting any of them is a v1.x process that requires a `pkg/uos/capability` bump and ≥ 2-provider justification (per `architecture_plan.md` §7.2).
- Not a SLA. `✅ Supported` means the contract test for that capability passes; it does not promise vendor-side availability or performance.
- Not auto-generated. Until M6, this file is hand-edited as each driver ships. M6 stabilization may add a `go test`-driven regenerator.

## Update protocol

When a driver ships:

1. Change "Driver implementation status" cell from `Planned` → `Shipped (vX.Y.Z)`.
2. For each of the 13 capabilities, replace the `⏳M_n_` cell with `✅` / `🟡` / `🧩` / `❌` plus footnote if needed.
3. Open a PR titled `matrix: <provider> milestone M_n_ shipped`. The PR description includes the contract-test job URL.
4. Cross-check `architecture_plan.md` Appendix A and remove any item this driver's milestone resolved.
