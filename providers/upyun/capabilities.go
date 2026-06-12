package upyun

import "github.com/slowfei/oss-client/pkg/uos/capability"

// capabilities returns the v1-frozen capability.Report for the Upyun USS
// driver. Cell values mirror docs/provider_matrix.md (the "upyun" column);
// see footnotes 3 (FORM upload), 7 (bespoke admin APIs surface only via
// As(target)), 9 (versioning not exposed), and 12 (NativeMove default
// path) for the rationale.
//
// Total: 13 cells exactly (matching capability.All()).
//
//   - 5 ✅ Supported : BucketCRUD (with portal-provisioned caveat),
//     ObjectCRUD, ListPrefixDelimiter, RangeRead, MultipartUpload
//   - 1 ✅ Supported : SignedURLRead — Upyun signed download URL via the
//     `_upt` query parameter (URL-shaped, GET only).
//   - 1 🟡 Conditional: SignedURLWrite — upload authorization is
//     FORM-shaped, NOT URL. Returns ErrUnsupported{CapSignedURLWrite}
//     with a reason pointing at IssueDirectGrant per matrix footnote 3.
//   - 1 ✅ Supported : DirectGrant — THE M5 validation moment. Mode used
//     for upload: DirectGrantModeForm (the LAST frozen DirectGrantMode
//     not yet exercised by any shipped driver). Download authorization
//     is URL-shaped via SignURL.
//   - 3 🧩 ExtensionOnly: ObjectTagging, ObjectACL, ManagedEncryption —
//     bespoke admin APIs that don't map cleanly to the unified surface.
//     Reach via Client.As(target **upyun.UpYun) per footnote 7.
//   - 1 ❌ Unsupported: Versioning — Upyun does not expose object
//     versioning as a unified data-plane capability per footnote 9.
//   - 1 🧩 ExtensionOnly: NativeMove — Upyun has Move/Copy primitives
//     (X-Upyun-Move-Source / X-Upyun-Copy-Source headers) but the
//     unified default is Copy+Delete; the native primitive is reachable
//     via As(target) per footnote 12.
func capabilities() capability.Report {
	return capability.Report{
		Items: map[capability.Capability]capability.CapabilityStatus{
			capability.CapBucketCRUD: {
				Availability: capability.Supported,
				Reason:       "Upyun service is provisioned via the web portal; List/Stat are exposed via Usage(); Create/Delete return ErrUnsupported with portal-provisioning reason — see provider_roadmap.md M5 + driver.go bucketService doc",
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
				Reason:       "Upyun REST Get / Put / Delete / GetInfo / Mkdir; Copy mapped via X-Upyun-Copy-Source header (server-side intra-bucket copy)",
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
				Reason:       "Upyun List with X-List-Limit + X-List-Iter pagination; Prefix is mapped onto the request Path; Delimiter (\"/\") is implicit (Upyun returns folders as IsDir=true)",
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
				Reason:       "HTTP Range header passed through GetObjectConfig.Headers (Upyun honors RFC 7233 ranged GETs)",
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
				Reason:       "Upyun resumable upload via X-Upyun-Multi-Stage headers (initiate / upload / complete); part size 1 MiB minimum, multiple of 1 MiB, ≤ 10000 parts; List supports cross-process orphan enumeration via ListMultipartUploads",
			},
			capability.CapSignedURLRead: {
				Availability: capability.Supported,
				Reason:       "Upyun signed download URL via the `_upt` query parameter; valid for GET only — see provider_matrix.md footnote 3",
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Conditional,
				Reason:       "Upyun upload authorization is FORM-shaped, NOT URL; SignURL(method=PUT/POST) returns ErrUnsupported{CapSignedURLWrite} with reason pointing at IssueDirectGrant — see provider_matrix.md footnote 3",
			},
			capability.CapDirectGrant: {
				Availability: capability.Supported,
				Reason:       "Upyun FORM upload expressed as DirectGrant{Mode: DirectGrantModeForm} (THE M5 milestone validation moment). FormFields carries policy + authorization; download grants returned as DirectGrantModeURL (signed URL) since Upyun download authorization is URL-shaped",
			},
			capability.CapObjectTagging: {
				Availability: capability.ExtensionOnly,
				Reason:       "Upyun has no S3-style per-object tagging surface; bespoke metadata-modification API (PATCH ?metadata=) is reachable via Client.As(target **upyun.UpYun) — see provider_matrix.md footnote 7",
			},
			capability.CapVersioning: {
				Availability: capability.Unsupported,
				Reason:       "Upyun does not expose object versioning as a unified data-plane capability — see provider_matrix.md footnote 9",
			},
			capability.CapObjectACL: {
				Availability: capability.ExtensionOnly,
				Reason:       "Upyun has no S3-style per-object ACL; access control is configured at the service level via the web portal. Reach via Client.As(target **upyun.UpYun) — see provider_matrix.md footnote 7",
			},
			capability.CapManagedEncryption: {
				Availability: capability.ExtensionOnly,
				Reason:       "Upyun does not expose a unified server-side-encryption surface; SSL-in-transit is configured per-service via the web portal — see provider_matrix.md footnote 7",
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				Reason:       "Upyun supports server-side Move via the X-Upyun-Move-Source header but the unified default is Copy+Delete (helpers.Move); the native primitive is reachable via Client.As(target **upyun.UpYun) — see provider_matrix.md footnote 12",
			},
		},
	}
}
