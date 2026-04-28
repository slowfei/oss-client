package aws

import "github.com/maqian/oss-client/pkg/uos/capability"

// capabilities returns the AWS S3 driver's capability.Report. It maps
// every entry in capability.All() (no missing keys, per architecture_plan
// §7.2 and the contract suite's report_completeness case).
//
// The cells must match docs/provider_matrix.md's "aws" column:
//
//   - CapBucketCRUD .................. Supported
//   - CapObjectCRUD .................. Supported
//   - CapListPrefixDelimiter ......... Supported
//   - CapRangeRead ................... Supported
//   - CapMultipartUpload ............. Supported
//   - CapSignedURLRead ............... Supported
//   - CapSignedURLWrite .............. Supported
//   - CapDirectGrant ................. Unsupported (footnote 5: S3 family uses presigned URLs)
//   - CapObjectTagging ............... Supported
//   - CapVersioning .................. Conditional (requires bucket-level versioning enabled)
//   - CapObjectACL ................... Conditional (S3 Object Ownership / BucketOwnerEnforced disables ACLs)
//   - CapManagedEncryption ........... Supported (SSE-S3 default; SSE-KMS / SSE-C via DriverConfig)
//   - CapNativeMove .................. ExtensionOnly (footnote 12: no provider exposes a native rename)
func capabilities() capability.Report {
	return capability.Report{
		Items: map[capability.Capability]capability.CapabilityStatus{
			capability.CapBucketCRUD: {
				Availability: capability.Supported,
				Reason:       "S3 CreateBucket / HeadBucket / ListBuckets / DeleteBucket",
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
				Reason:       "S3 PutObject / GetObject / HeadObject / DeleteObject / DeleteObjects / CopyObject",
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
				Reason:       "ListObjectsV2 with Prefix + Delimiter",
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
				Reason:       "HTTP Range header on GetObject",
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
				Reason:       "S3 CreateMultipartUpload / UploadPart / CompleteMultipartUpload / AbortMultipartUpload / ListMultipartUploads",
			},
			capability.CapSignedURLRead: {
				Availability: capability.Supported,
				Reason:       "S3 SigV4 PresignGetObject",
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Supported,
				Reason:       "S3 SigV4 PresignPutObject",
			},
			capability.CapDirectGrant: {
				Availability: capability.Unsupported,
				Reason:       "S3 family uses presigned URL (CapSignedURLRead/Write) — see provider_matrix.md footnote 5",
			},
			capability.CapObjectTagging: {
				Availability: capability.Supported,
				Reason:       "S3 PutObjectTagging / GetObjectTagging (driver exposes via As(target *s3.Client))",
			},
			capability.CapVersioning: {
				Availability: capability.Conditional,
				Reason:       "requires bucket-level versioning enabled; reachable via VersionID round-trip",
			},
			capability.CapObjectACL: {
				Availability: capability.Conditional,
				Reason:       "subject to bucket Object Ownership setting; AWS deprecates ACLs in favour of bucket policies",
			},
			capability.CapManagedEncryption: {
				Availability: capability.Supported,
				Reason:       "SSE-S3 default; SSE-KMS / SSE-C selectable via PutObjectInput",
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				Reason:       "no native rename; default is Copy+Delete — see provider_matrix.md footnote 12",
			},
		},
	}
}
