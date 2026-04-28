package uos

import (
	"io"
	"time"
)

// Request and response shapes consumed by MultipartService.

// InitiateMultipartRequest is the request shape for
// MultipartService.Initiate. The returned MultipartUpload.UploadID is
// the handle UploadPart / Complete / Abort all reference.
type InitiateMultipartRequest struct {
	// Bucket is the target bucket. Required.
	Bucket string
	// Key is the target object key. Required.
	Key string
	// Content carries optional content-negotiation headers.
	Content ContentHeaders
	// Metadata is optional user-defined metadata (lower-cased keys).
	Metadata Metadata
	// StorageClass is the vendor-defined storage class. Empty means default.
	StorageClass string
	// ACL is the vendor-defined canned ACL. Empty means default.
	ACL string
}

// UploadPartRequest is the request shape for MultipartService.UploadPart.
type UploadPartRequest struct {
	// Bucket is the target bucket. Required.
	Bucket string
	// Key is the target object key. Required.
	Key string
	// UploadID is the handle returned by Initiate. Required.
	UploadID string
	// PartNumber is the 1-based part index. Required, > 0.
	PartNumber int
	// Body is the part payload stream. Required, non-nil.
	Body io.Reader
	// Size is the part length in bytes. Required (vendors typically
	// require Content-Length on each part).
	Size int64
	// Checksum is an optional caller-supplied checksum for this part.
	Checksum Checksum
}

// UploadedPart describes a part the vendor has acknowledged. The
// returned ETag (and Checksum, when present) MUST be presented back to
// MultipartService.Complete.
type UploadedPart struct {
	// PartNumber is the 1-based part index this entry describes.
	PartNumber int
	// ETag is the vendor-reported entity tag of the uploaded part.
	ETag string
	// Size is the part length in bytes.
	Size int64
	// Checksum is the vendor-reported integrity value, when present.
	Checksum Checksum
}

// CompleteMultipartRequest is the request shape for MultipartService.Complete.
type CompleteMultipartRequest struct {
	// Bucket is the target bucket. Required.
	Bucket string
	// Key is the target object key. Required.
	Key string
	// UploadID is the handle returned by Initiate. Required.
	UploadID string
	// Parts is the ordered list of uploaded parts to assemble. Required, non-empty.
	Parts []UploadedPart
}

// AbortMultipartRequest is the request shape for MultipartService.Abort.
type AbortMultipartRequest struct {
	// Bucket is the target bucket. Required.
	Bucket string
	// Key is the target object key. Required.
	Key string
	// UploadID is the handle returned by Initiate. Required.
	UploadID string
}

// ListMultipartUploadsRequest is the request shape for MultipartService.List.
// Drivers expose this so callers (and the testkit) can clean up orphan
// uploads — see the multipart-orphan cross-cutting risk in
// docs/provider_roadmap.md.
type ListMultipartUploadsRequest struct {
	// Bucket is the target bucket. Required.
	Bucket string
	// Prefix narrows results to uploads whose Key starts with Prefix.
	Prefix string
	// MaxResults caps the page size. 0 means "use vendor default".
	MaxResults int
	// ContinuationToken is the opaque cursor returned by a prior call.
	ContinuationToken string
}

// MultipartUpload describes a multipart upload in flight. It is
// returned by MultipartService.Initiate and listed by
// MultipartService.List. This type is part of the v0.1 frozen public
// surface (Critic R1 sign-off).
type MultipartUpload struct {
	// UploadID is the vendor handle that identifies this upload.
	UploadID string
	// Bucket is the bucket the upload targets.
	Bucket string
	// Key is the object key the upload targets.
	Key string
	// Initiated is the absolute time the upload was initiated.
	Initiated time.Time
	// StorageClass is the vendor-defined storage class associated with the upload.
	StorageClass string
	// Metadata is the user-defined metadata bound at Initiate time
	// (lower-cased keys).
	Metadata Metadata
	// Extra preserves raw vendor headers/fields not mapped above.
	Extra map[string]string
}

// MultipartUploadList is the page returned by MultipartService.List. It
// is part of the v0.1 frozen public surface (Critic R1 sign-off).
type MultipartUploadList struct {
	// Uploads is the page of in-flight multipart uploads matching the request.
	Uploads []MultipartUpload
	// NextToken is an opaque cursor; pass back as
	// ListMultipartUploadsRequest.ContinuationToken.
	NextToken string
	// Truncated is true iff more pages remain.
	Truncated bool
}
