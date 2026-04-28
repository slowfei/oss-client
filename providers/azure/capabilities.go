package azure

import "github.com/maqian/object-storage-client/pkg/uos/capability"

// capabilities returns the v1-frozen capability.Report for the Azure Blob
// Storage driver. Cell values mirror docs/provider_matrix.md (the "azure"
// column); see the footnote references below for the rationale behind
// Conditional and ExtensionOnly cells.
//
// Total: 13 cells exactly (matching capability.All()).
//
//   - 6 ✅ Supported : BucketCRUD, ObjectCRUD, ListPrefixDelimiter,
//     RangeRead, MultipartUpload, ObjectTagging
//   - 1 ✅ Supported : DirectGrant⁶ — Azure SAS expressible as both URL
//     (via SignURL) and token+headers (via IssueDirectGrant). Mode used:
//     DirectGrantModeToken (the SAS string is an opaque bearer token the
//     caller carries; no form-fields, no URL encoding required beyond appending
//     it as a query string).
//   - 1 ✅ Supported : ManagedEncryption — Azure SSE is on by default for
//     all blobs at rest; no per-request opt-in needed.
//   - 2 🟡 Conditional: SignedURLRead, SignedURLWrite⁶ — SAS generation
//     requires either an account key (AuthSharedKey) or an Entra user-
//     delegation key (AuthCustom). A pre-formed SAS (AuthSAS) lacks the
//     key material to issue new SAS tokens; the driver returns
//     ErrUnsupported{CapSignedURLRead/Write} in that case.
//   - 1 🟡 Conditional: Versioning⁸ — requires versioning to be enabled
//     at the storage account level; if disabled, version-dependent ops
//     return ErrUnsupported{CapVersioning}.
//   - 1 🟡 Conditional: ObjectACL¹¹ — Azure has no S3-style per-object ACL;
//     per-blob access is controlled via SAS or RBAC. Operations targeting
//     this capability return ErrUnsupported{CapObjectACL} per footnote 11.
//   - 1 🧩 ExtensionOnly: NativeMove¹² — no native rename; default is
//     Copy+Delete. Reach via Client.As(target **azblob.Client).
func capabilities() capability.Report {
	return capability.Report{
		Items: map[capability.Capability]capability.CapabilityStatus{
			capability.CapBucketCRUD: {
				Availability: capability.Supported,
				Reason:       "Azure Container Create / GetProperties / List / Delete (Container maps 1:1 to Bucket)",
			},
			capability.CapObjectCRUD: {
				Availability: capability.Supported,
				Reason:       "Azure Blob Upload / Download / GetProperties / Delete / BlobExists / DeleteBlobs / Copy",
			},
			capability.CapListPrefixDelimiter: {
				Availability: capability.Supported,
				Reason:       "Azure ListBlobsHierarchy with Prefix + Delimiter (\"/\") — identical S3-style hierarchical listing",
			},
			capability.CapRangeRead: {
				Availability: capability.Supported,
				Reason:       "HTTP Range header via azblob.DownloadStreamOptions.Range (azblob.HTTPRange{Offset, Count})",
			},
			capability.CapMultipartUpload: {
				Availability: capability.Supported,
				Reason:       "Block Blob staging: PutBlock (UploadPart) + PutBlockList (Complete); Initiate assigns upload ID; Abort deletes uncommitted blocks. Min block 4 MiB, max 50,000 blocks — see Lessons (M4).",
			},
			capability.CapSignedURLRead: {
				Availability: capability.Conditional,
				Reason:       "SAS URL generation requires AuthSharedKey (account-key SAS) or AuthCustom (user-delegation SAS); AuthSAS callers lack key material. Returns ErrUnsupported{CapSignedURLRead} when credential cannot sign.",
			},
			capability.CapSignedURLWrite: {
				Availability: capability.Conditional,
				Reason:       "Same key-material gating as CapSignedURLRead; PUT SAS returned when credential allows signing. Returns ErrUnsupported{CapSignedURLWrite} when credential cannot sign.",
			},
			capability.CapDirectGrant: {
				Availability: capability.Supported,
				Reason:       "Azure SAS expressed as DirectGrant with Mode=DirectGrantModeToken; Token holds the SAS query string; Headers carries x-ms-version. Both upload and download operations supported.",
			},
			capability.CapObjectTagging: {
				Availability: capability.Supported,
				Reason:       "Azure Blob Index Tags (SetTags / GetTags); exposed via As(target **azblob.Client) for full tag management beyond the unified surface.",
			},
			capability.CapVersioning: {
				Availability: capability.Conditional,
				Reason:       "Requires storage account-level versioning enabled (Azure Blob versioning); if disabled, version-dependent ops return ErrUnsupported{CapVersioning}. Detected at first call, not at construction.",
			},
			capability.CapObjectACL: {
				Availability: capability.Conditional,
				Reason:       "Azure has no S3-style per-object ACL; per-blob access is controlled via SAS permissions or RBAC. Calls targeting CapObjectACL return ErrUnsupported — use Signer.IssueDirectGrant for scoped access. See provider_matrix.md footnote 11.",
			},
			capability.CapManagedEncryption: {
				Availability: capability.Supported,
				Reason:       "Azure Storage Service Encryption (SSE) is enabled by default for all blobs; Microsoft-managed keys used unless customer-managed keys are configured at the account level.",
			},
			capability.CapNativeMove: {
				Availability: capability.ExtensionOnly,
				Reason:       "No native rename/move primitive; default path is Copy+Delete (helpers.Move). Blob rename via the preview Rename API is reachable via Client.As(target **azblob.Client). See provider_matrix.md footnote 12.",
			},
		},
	}
}
