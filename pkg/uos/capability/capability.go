// Package capability defines the v1-frozen capability vocabulary that drivers
// use to declare which subset of the unified API they implement.
//
// String value convention: every capability uses dotted lower-case segments
// (group.feature) so the values are stable, JSON-friendly, and trivially
// extensible into namespaced subsections (e.g. "object.tagging") without
// colliding with future additions. The set of 13 values is frozen for v1
// per architecture_plan §7.2; adding a 14th capability requires a minor
// version bump and at least two providers exposing the same semantic.
//
// This package is a leaf in the dependency graph: it MUST NOT import
// pkg/uos. The rich-error wrapper that turns a missing capability into
// *uos.Error{Code: ErrUnsupported, Capability: c} lives in pkg/uos
// (see uos.NewUnsupported). Report.Require here returns a plain sentinel
// error so callers in pkg/uos can wrap it without an import cycle.
package capability

// Capability is the v1-frozen 13-value capability enum. See
// architecture_plan §7.2 for the full semantic definition of each value.
type Capability string

// The 13 frozen v1 capabilities. The order of declaration matches the
// authoritative order in architecture_plan §7.2 and is observed by All().
const (
	// CapBucketCRUD covers Create / Stat / List / Delete on buckets.
	CapBucketCRUD Capability = "bucket.crud"
	// CapObjectCRUD covers Put / Get / Head / Delete / Exists / DeleteMany / Copy on objects.
	CapObjectCRUD Capability = "object.crud"
	// CapListPrefixDelimiter is List with prefix + delimiter (S3-style hierarchical listing).
	CapListPrefixDelimiter Capability = "object.list.prefix_delimiter"
	// CapRangeRead is HTTP-Range-style partial reads on Get.
	CapRangeRead Capability = "object.range_read"
	// CapMultipartUpload is the Initiate/UploadPart/Complete/Abort family.
	CapMultipartUpload Capability = "object.multipart_upload"
	// CapSignedURLRead is time-bounded signed URLs for object download.
	CapSignedURLRead Capability = "signer.url_read"
	// CapSignedURLWrite is time-bounded signed URLs for object upload (PUT/POST).
	CapSignedURLWrite Capability = "signer.url_write"
	// CapDirectGrant is non-URL grants (Azure SAS, Qiniu Token, Upyun FORM, signed headers).
	CapDirectGrant Capability = "signer.direct_grant"
	// CapObjectTagging is per-object tagging (key/value labels).
	CapObjectTagging Capability = "object.tagging"
	// CapVersioning is bucket-level object versioning.
	CapVersioning Capability = "bucket.versioning"
	// CapObjectACL is per-object access-control lists / canned ACLs.
	CapObjectACL Capability = "object.acl"
	// CapManagedEncryption is server-side encryption with provider-managed keys (SSE-S3 / SSE-KMS / SSE-C analog).
	CapManagedEncryption Capability = "object.encryption.managed"
	// CapNativeMove is a single-call rename/move primitive (e.g. Alibaba OSS PostObject Restore, GCS rewrite-as-move). Default helper is Copy+Delete; this capability declares native availability.
	CapNativeMove Capability = "object.native_move"
)

// All returns the 13 frozen capabilities in declaration order. Used by
// surface_test and the contract test suite to verify drivers populate
// every key in their Report.
func All() []Capability {
	return []Capability{
		CapBucketCRUD,
		CapObjectCRUD,
		CapListPrefixDelimiter,
		CapRangeRead,
		CapMultipartUpload,
		CapSignedURLRead,
		CapSignedURLWrite,
		CapDirectGrant,
		CapObjectTagging,
		CapVersioning,
		CapObjectACL,
		CapManagedEncryption,
		CapNativeMove,
	}
}
