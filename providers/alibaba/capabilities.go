// Package alibaba is the native uos.Client driver for Alibaba Cloud OSS.
// It targets the v0.1 frozen pkg/uos surface (architecture_plan §1) and
// implements every method on uos.Client by translating to/from
// github.com/aliyun/aliyun-oss-go-sdk/oss.
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
package alibaba

import "github.com/slowfei/oss-client/pkg/uos/capability"

// capabilities returns the v1-frozen capability.Report for the Alibaba
// OSS driver. Cell values mirror docs/provider_matrix.md (the "alibaba"
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
				Reason:       "OSS CreateBucket / GetBucketInfo / ListBuckets / DeleteBucket",
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
				Reason:       "OSS PutObject / GetObject / GetObjectMeta / DeleteObject / DeleteObjects / CopyObject",
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
				Reason:       "OSS ListObjectsV2 with Prefix + Delimiter",
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
				Reason:       "HTTP Range header on GetObject (oss.Range / oss.NormalizedRange)",
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
				Reason:       "OSS InitiateMultipartUpload / UploadPart / CompleteMultipartUpload / AbortMultipartUpload / ListMultipartUploads",
			},
			capability.CapSignedURLRead: {
				Availability: capability.Supported,
				Reason:       "OSS Bucket.SignURL (HMAC v1 / v4 selectable via DriverConfig)",
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Supported,
				Reason:       "OSS Bucket.SignURL with HTTPPut",
			},
			capability.CapDirectGrant: {
				Availability: capability.Unsupported,
				Reason:       "S3-family uses presigned URL (CapSignedURLRead/Write) — see provider_matrix.md footnote 5",
			},
			capability.CapObjectTagging: {
				Availability: capability.Supported,
				Reason:       "OSS PutObjectTagging / GetObjectTagging (driver exposes via As(target **oss.Client))",
			},
			capability.CapVersioning: {
				Availability: capability.Conditional,
				Reason:       "requires bucket-level versioning enabled; reachable via VersionID round-trip",
			},
			capability.CapObjectACL: {
				Availability: capability.Conditional,
				Reason:       "OSS canned ACLs (private / public-read / public-read-write) take effect only when the bucket policy permits per-object ACL writes",
			},
			capability.CapManagedEncryption: {
				Availability: capability.Supported,
				Reason:       "OSS server-side encryption (SSE-OSS default; SSE-KMS via ServerSideEncryption / ServerSideEncryptionKeyID options)",
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				Reason:       "no native rename; default is Copy+Delete — see provider_matrix.md footnote 12",
			},
		},
	}
}
