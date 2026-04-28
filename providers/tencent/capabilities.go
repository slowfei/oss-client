// Package tencent is the native uos.Client driver for Tencent Cloud COS.
// It targets the v0.1 frozen pkg/uos surface (architecture_plan §1) and
// implements every method on uos.Client by translating to/from
// github.com/tencentyun/cos-go-sdk-v5.
//
// All exported types and methods carry doc comments so the package is
// directly usable from godoc; vendor SDK types stay confined to this
// directory per AGENTS.md ("no provider-specific public types in
// pkg/uos") and are reachable from caller code only via Client.As(target).
//
// Per the M3 milestone validation focus, this driver consumes the shared
// pkg/uos/s3common helpers (extracted pre-tag from M2's AWS+MinIO
// drivers and extended during M3 alibaba landing) instead of duplicating
// wire-level mapping logic — see docs/provider_roadmap.md §M3 and
// architecture_plan §3.3.
package tencent

import "github.com/maqian/oss-client/pkg/uos/capability"

// capabilities returns the v1-frozen capability.Report for the Tencent
// COS driver. Cell values mirror docs/provider_matrix.md (the "tencent"
// column); see footnotes 5 (CapDirectGrant on S3-family), 12
// (CapNativeMove everywhere), 13 (CapVersioning bucket-level), and
// 14 (CapObjectACL bucket-policy-permitting) for the rationale.
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
				Reason:       "COS Service.Get / Bucket.Put / Bucket.Head / Bucket.Delete",
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
				Reason:       "COS Object.Put / Get / Head / Delete / DeleteMulti / Copy",
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
				Reason:       "COS Bucket.Get with Prefix + Delimiter",
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
				Reason:       "HTTP Range header on Object.Get (ObjectGetOptions.Range)",
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
				Reason:       "COS Object.InitiateMultipartUpload / UploadPart / CompleteMultipartUpload / AbortMultipartUpload / ListUploads",
			},
			capability.CapSignedURLRead: {
				Availability: capability.Supported,
				Reason:       "COS Object.GetPresignedURL with HTTP GET (HMAC v1)",
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Supported,
				Reason:       "COS Object.GetPresignedURL with HTTP PUT (HMAC v1)",
			},
			capability.CapDirectGrant: {
				Availability: capability.Unsupported,
				Reason:       "S3-family uses presigned URL (CapSignedURLRead/Write) — see provider_matrix.md footnote 5",
			},
			capability.CapObjectTagging: {
				Availability: capability.Supported,
				Reason:       "COS Object.PutTagging / GetTagging / DeleteTagging (driver exposes via As(target **cos.Client))",
			},
			capability.CapVersioning: {
				Availability: capability.Conditional,
				Reason:       "requires bucket-level versioning enabled; reachable via VersionID round-trip",
			},
			capability.CapObjectACL: {
				Availability: capability.Conditional,
				Reason:       "COS canned ACLs (private / public-read / public-read-write) take effect only when the bucket policy permits per-object ACL writes",
			},
			capability.CapManagedEncryption: {
				Availability: capability.Supported,
				Reason:       "COS server-side encryption (SSE-COS via x-cos-server-side-encryption header; SSE-KMS via SSE-COS-KMS variant)",
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				Reason:       "no native rename; default is Copy+Delete — see provider_matrix.md footnote 12",
			},
		},
	}
}
