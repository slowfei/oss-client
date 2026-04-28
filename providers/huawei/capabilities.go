// Package huawei is the native uos.Client driver for Huawei Cloud OBS.
// It targets the v0.1 frozen pkg/uos surface (architecture_plan §1) and
// implements every method on uos.Client by translating to/from
// github.com/huaweicloud/huaweicloud-sdk-go-obs.
//
// All exported types and methods carry doc comments so the package is
// directly usable from godoc; vendor SDK types stay confined to this
// directory per AGENTS.md ("no provider-specific public types in
// pkg/uos") and are reachable from caller code only via Client.As(target).
//
// Per the M3 milestone validation focus, this driver consumes the shared
// pkg/uos/s3common helpers (extracted pre-tag from M2's AWS+MinIO
// drivers) instead of duplicating wire-level mapping logic — see
// docs/provider_roadmap.md §M3 and architecture_plan §3.3.
//
// # Endpoint pairing strictness
//
// docs/provider_roadmap.md M3 cross-cutting risk: Huawei OBS is the most
// region/endpoint-pairing-sensitive vendor in the M3 quartet. An
// incorrect Region+Endpoint combination produces silent HTTP 301 / 307
// redirects rather than a clean ErrInvalidArgument; the SDK follows the
// redirect and the caller observes a downstream signature failure. To
// fail fast, this driver makes Endpoint mandatory (does NOT auto-derive
// from Region) — callers MUST set the Endpoint matching their region's
// public OBS host (e.g. "https://obs.cn-north-4.myhuaweicloud.com") so
// region pinning is observable at construction time.
package huawei

import "github.com/maqian/object-storage-client/pkg/uos/capability"

// capabilities returns the v1-frozen capability.Report for the Huawei
// OBS driver. Cell values mirror docs/provider_matrix.md (the "huawei"
// column); see footnotes 5 (CapDirectGrant on S3-family) and 12
// (CapNativeMove everywhere) for the rationale.
//
// Drivers MUST populate every key returned by capability.All() per
// architecture_plan §7.2; missing keys would fail the contract suite's
// report_completeness case. Returning a fresh map per call keeps the
// caller free to mutate the returned report without affecting later
// calls into this driver.
func capabilities() capability.Report {
	return capability.Report{
		Items: map[capability.Capability]capability.CapabilityStatus{
			capability.CapBucketCRUD: {
				Availability: capability.Supported,
				Reason:       "OBS CreateBucket / GetBucketMetadata / ListBuckets / DeleteBucket",
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
				Reason:       "OBS PutObject / GetObject / GetObjectMetadata / DeleteObject / DeleteObjects / CopyObject",
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
				Reason:       "OBS ListObjects with Prefix + Delimiter",
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
				Reason:       "HTTP Range header on GetObject (GetObjectInput.RangeStart / RangeEnd)",
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
				Reason:       "OBS InitiateMultipartUpload / UploadPart / CompleteMultipartUpload / AbortMultipartUpload / ListMultipartUploads",
			},
			capability.CapSignedURLRead: {
				Availability: capability.Supported,
				Reason:       "OBS CreateSignedUrl with HttpMethodGet (HMAC v2 / v4 selectable via DriverConfig.Signature)",
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Supported,
				Reason:       "OBS CreateSignedUrl with HttpMethodPut",
			},
			capability.CapDirectGrant: {
				Availability: capability.Unsupported,
				Reason:       "S3-family uses presigned URL (CapSignedURLRead/Write) — see provider_matrix.md footnote 5",
			},
			capability.CapObjectTagging: {
				Availability: capability.Supported,
				Reason:       "OBS SetObjectTagging / GetObjectTagging (driver exposes via As(target **obs.ObsClient))",
			},
			capability.CapVersioning: {
				Availability: capability.Conditional,
				Reason:       "requires bucket-level versioning enabled; reachable via VersionID round-trip",
			},
			capability.CapObjectACL: {
				Availability: capability.Conditional,
				Reason:       "OBS canned ACLs (private / public-read / public-read-write) take effect only when the bucket policy permits per-object ACL writes",
			},
			capability.CapManagedEncryption: {
				Availability: capability.Supported,
				Reason:       "OBS server-side encryption (SSE-OBS default; SSE-KMS via SseHeader on the typed APIs)",
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				Reason:       "no native rename in the unified surface; default is Copy+Delete — see provider_matrix.md footnote 12",
			},
		},
	}
}
