package uos

// Request and response shapes consumed by BucketService.

// ListBucketsRequest is the request shape for BucketService.List.
type ListBucketsRequest struct {
	// MaxResults caps the number of buckets returned in a single call.
	// 0 means "use vendor default". Drivers may further cap this.
	MaxResults int
	// ContinuationToken is the opaque cursor returned by a prior call.
	// Empty means "start from the beginning".
	ContinuationToken string
}

// CreateBucketRequest is the request shape for BucketService.Create.
type CreateBucketRequest struct {
	// Name is the bucket name to create. Required.
	Name string
	// Region is the home region for the new bucket. Empty means "use
	// the Client's configured region".
	Region string
	// ACL is the canned ACL to apply at creation, when supported.
	// Vendor-defined string; empty means "use vendor default".
	ACL string
}

// StatBucketRequest is the request shape for BucketService.Stat.
type StatBucketRequest struct {
	// Name is the bucket name to stat. Required.
	Name string
}

// DeleteBucketRequest is the request shape for BucketService.Delete.
type DeleteBucketRequest struct {
	// Name is the bucket name to delete. Required. The bucket MUST be
	// empty per most vendors' semantics.
	Name string
}
