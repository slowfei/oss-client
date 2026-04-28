package qiniu

import "github.com/maqian/object-storage-client/pkg/uos/capability"

// capabilities returns the v1-frozen capability.Report for the Qiniu Cloud
// Storage (Kodo) driver. Cell values mirror docs/provider_matrix.md (the
// "qiniu" column); see the footnote references below for the rationale
// behind Conditional and ExtensionOnly cells.
//
// Total: 13 cells exactly (matching capability.All()).
//
//   - 6 ✅ Supported : BucketCRUD, ObjectCRUD, ListPrefixDelimiter,
//     RangeRead, MultipartUpload, SignedURLRead — the read-side signed-URL
//     path is via storage.MakePrivateURL (download).
//   - 1 ✅ Supported : DirectGrant — Qiniu Upload Token + Download Token
//     both expressed via DirectGrantModeToken (the M5 validation moment).
//     See provider_matrix.md footnote 4 for why Qiniu's write-side
//     authorization is non-URL.
//   - 1 🟡 Conditional: SignedURLWrite — Qiniu does NOT issue URL-shaped
//     write authorization; the Upload Token is the write path and surfaces
//     via IssueDirectGrant. SignURL with PUT/POST returns ErrUnsupported
//     with Reason directing callers to IssueDirectGrant. See footnote 4.
//   - 3 🧩 ExtensionOnly: ObjectTagging, ObjectACL, ManagedEncryption —
//     Qiniu has bespoke admin APIs (file lifecycle, bucket ACL toggles,
//     KMS) that don't fit the unified surface. Reach via Client.As(target).
//     See footnote 7.
//   - 1 ❌ Unsupported: Versioning — Qiniu does not expose object
//     versioning as a data-plane capability; bucket-level immutability is
//     a separate vendor feature. See footnote 9.
//   - 1 🧩 ExtensionOnly: NativeMove — Qiniu's BucketManager.Move exists
//     but the unified default is Copy+Delete; reach via As(target) for the
//     native primitive. See footnote 12.
func capabilities() capability.Report {
	return capability.Report{
		Items: map[capability.Capability]capability.CapabilityStatus{
			capability.CapBucketCRUD: {
				Availability: capability.Supported,
				Reason:       "Qiniu storage.BucketManager Buckets / CreateBucket / Stat / DropBucket — bucket admin via the management API",
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
				Reason:       "Qiniu storage.FormUploader.PutFile / BucketManager.Get / Stat / Delete / Copy — object plane via the management + upload APIs",
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
				Reason:       "Qiniu BucketManager.ListFiles with prefix + delimiter — S3-style hierarchical listing supported",
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
				Reason:       "HTTP Range header against the (signed) download URL; honored by Qiniu's CDN/source endpoints natively",
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
				Reason:       "Qiniu storage.ResumeUploaderV2 (Resumable Upload v2) — synthetic UploadID indexes an in-process session; Initiate creates session, UploadPart stages a block, Complete finalises via mkfile. Per-block sequential per Qiniu RUv2 contract — see Lessons (M5).",
			},
			capability.CapSignedURLRead: {
				Availability: capability.Supported,
				Reason:       "URL-shaped private-bucket download via storage.MakePrivateURL(creds, domain, key, deadline) — read access works; write authorization is non-URL (see CapSignedURLWrite + CapDirectGrant)",
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Conditional,
				Reason:       "Qiniu write authorization is non-URL: callers POST a multipart form to the upload host using an Upload Token in the 'token' field. SignURL with PUT/POST returns ErrUnsupported{CapSignedURLWrite, Reason='qiniu write authorization is non-URL; use IssueDirectGrant'} per provider_matrix.md footnote 4.",
			},
			capability.CapDirectGrant: {
				Availability: capability.Supported,
				Reason:       "Qiniu Upload Token + Download Token expressed as DirectGrant with Mode=DirectGrantModeToken; Token holds the bearer string, URL holds the upload/download endpoint, Method names POST (upload) or GET (download). Validates DirectGrantModeToken in a NEW context (Upload Token, distinct from Azure SAS).",
			},
			capability.CapObjectTagging: {
				Availability: capability.ExtensionOnly,
				Reason:       "Qiniu has no S3-style per-object tagging API; metadata-style tagging is via x-qn-meta-* headers on upload. Reach via Client.As(target *storage.BucketManager) for vendor-specific lifecycle/tagging admin. See provider_matrix.md footnote 7.",
			},
			capability.CapVersioning: {
				Availability: capability.Unsupported,
				Reason:       "Qiniu does not expose object versioning as a data-plane capability; bucket-level immutability is a separate vendor feature reachable only via the management portal. See provider_matrix.md footnote 9.",
			},
			capability.CapObjectACL: {
				Availability: capability.ExtensionOnly,
				Reason:       "Qiniu has no S3-style per-object ACL; per-object access is controlled via Upload Token / Download Token scope (Mode=DirectGrantModeToken) or bucket-level public/private toggle. Reach via Client.As(target) for the bucket-level ACL admin. See provider_matrix.md footnote 7.",
			},
			capability.CapManagedEncryption: {
				Availability: capability.ExtensionOnly,
				Reason:       "Qiniu KMS server-side encryption is configured at bucket level (not per-object) via the management portal; the unified per-request opt-in is not exposed. Reach via Client.As(target) for the bespoke admin path. See provider_matrix.md footnote 7.",
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				Reason:       "Qiniu storage.BucketManager.Move exists as a native rename primitive but the unified default is Copy+Delete (helpers.Move). Reach via Client.As(target *storage.BucketManager) for the single-call rename. See provider_matrix.md footnote 12.",
			},
		},
	}
}
