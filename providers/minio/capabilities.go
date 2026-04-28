// Package minio is the native uos.Client driver for MinIO and any other
// S3-compatible service that speaks the minio-go/v7 dialect cleanly. It
// targets the v0.1 frozen pkg/uos surface (architecture_plan §1) and
// implements every method on uos.Client by translating to/from minio-go.
//
// All exported types and methods carry doc comments so the package is
// directly usable from godoc; vendor SDK types stay confined to this
// directory per AGENTS.md ("no provider-specific public types in
// pkg/uos") and are reachable from caller code only via Client.As(target).
package minio

import "github.com/maqian/object-storage-client/pkg/uos/capability"

// capabilities returns the v1-frozen capability.Report for the MinIO
// native driver. Cell values mirror docs/provider_matrix.md (the "minio"
// column); see footnotes 5 and 12 there for the reasons CapDirectGrant
// is Unsupported and CapNativeMove is ExtensionOnly.
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
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
			},
			capability.CapSignedURLRead: {
				Availability: capability.Supported,
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Supported,
			},
			capability.CapDirectGrant: {
				Availability: capability.Unsupported,
				// Footnote 5 in docs/provider_matrix.md.
				Reason: "S3-family uses presigned URL, not direct grant",
			},
			capability.CapObjectTagging: {
				Availability: capability.Supported,
			},
			capability.CapVersioning: {
				Availability: capability.Conditional,
				Reason:       "requires bucket versioning enabled",
			},
			capability.CapObjectACL: {
				Availability: capability.Conditional,
				Reason:       "depends on bucket policy / canned ACL configuration",
			},
			capability.CapManagedEncryption: {
				Availability: capability.Supported,
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				// Footnote 12 in docs/provider_matrix.md.
				Reason: "no native rename; helpers.Move performs Copy+Delete",
			},
		},
	}
}
