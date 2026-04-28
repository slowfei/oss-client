// Package volcengine is the native uos.Client driver for Volcengine TOS
// (Tinder Object Storage). It targets the v0.1 frozen pkg/uos surface
// (architecture_plan §1) and implements every method on uos.Client by
// translating to/from github.com/volcengine/ve-tos-golang-sdk/v2/tos.
//
// All exported types and methods carry doc comments so the package is
// directly usable from godoc; vendor SDK types stay confined to this
// directory per AGENTS.md ("no provider-specific public types in
// pkg/uos") and are reachable from caller code only via Client.As(target).
//
// This driver consumes the shared pkg/uos/s3common helpers (extracted
// pre-tag from M2's AWS+MinIO drivers and gap-filled by M3 alibaba)
// instead of duplicating wire-level mapping logic — see
// docs/provider_roadmap.md §M3 and architecture_plan §3.3.
package volcengine

import "github.com/maqian/oss-client/pkg/uos/capability"

// capabilities returns the v1-frozen capability.Report for the Volcengine
// TOS driver. Cell values mirror docs/provider_matrix.md (the "volcengine"
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
				Reason:       "TOS CreateBucketV2 / HeadBucket / ListBuckets / DeleteBucket",
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
				Reason:       "TOS PutObjectV2 / GetObjectV2 / HeadObjectV2 / DeleteObjectV2 / DeleteMultiObjects / CopyObject",
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
				Reason:       "TOS ListObjectsV2 with Prefix + Delimiter (Marker-based pagination)",
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
				Reason:       "HTTP Range header on GetObjectV2 (input.Range)",
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
				Reason:       "TOS CreateMultipartUploadV2 / UploadPartV2 / CompleteMultipartUploadV2 / AbortMultipartUpload / ListMultipartUploadsV2",
			},
			capability.CapSignedURLRead: {
				Availability: capability.Supported,
				Reason:       "TOS PreSignedURL with HttpMethodGet (HMAC SigV4 family)",
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Supported,
				Reason:       "TOS PreSignedURL with HttpMethodPut",
			},
			capability.CapDirectGrant: {
				Availability: capability.Unsupported,
				Reason:       "S3-family uses presigned URL (CapSignedURLRead/Write) — see provider_matrix.md footnote 5",
			},
			capability.CapObjectTagging: {
				Availability: capability.Supported,
				Reason:       "TOS PutObjectTagging / GetObjectTagging (driver exposes via As(target **tos.ClientV2))",
			},
			capability.CapVersioning: {
				Availability: capability.Conditional,
				Reason:       "requires bucket-level versioning enabled; reachable via VersionID round-trip",
			},
			capability.CapObjectACL: {
				Availability: capability.Conditional,
				Reason:       "TOS canned ACLs (private / public-read / public-read-write / authenticated-read / bucket-owner-*) take effect only when the bucket policy permits per-object ACL writes",
			},
			capability.CapManagedEncryption: {
				Availability: capability.Supported,
				Reason:       "TOS server-side encryption (SSE-TOS default; SSE-KMS via ServerSideEncryption / ServerSideEncryptionKeyID headers)",
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				Reason:       "TOS RenameObject is HNS-bucket-only (file-namespace); generic default is Copy+Delete — see provider_matrix.md footnote 12",
			},
		},
	}
}
