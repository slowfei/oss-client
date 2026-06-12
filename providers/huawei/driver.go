package huawei

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/capability"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// driverImpl implements pkg/uos.Client by translating to/from
// github.com/huaweicloud/huaweicloud-sdk-go-obs. It holds the SDK
// *obs.ObsClient (used for every wire call) and the original Config
// (consulted by Capabilities and a few defaults).
//
// driverImpl is safe for concurrent use; *obs.ObsClient is itself
// goroutine-safe (the SDK's basic security provider serialises token
// refreshes internally).
type driverImpl struct {
	cfg    uos.Config
	client *obs.ObsClient
}

// Provider returns "huawei". Required by uos.Client.
func (d *driverImpl) Provider() uos.Provider { return providerID }

// Capabilities returns the v1-frozen capability.Report for this driver.
// The map is computed once per call (capabilities() returns a fresh
// copy) so callers may mutate it freely.
func (d *driverImpl) Capabilities(_ context.Context) (capability.Report, error) {
	return capabilities(), nil
}

// Buckets returns the BucketService view bound to this Client.
func (d *driverImpl) Buckets() uos.BucketService { return bucketService{d: d} }

// Objects returns the ObjectService view bound to the named bucket.
// The bucket name is captured here so request structs that omit Bucket
// can still target the right namespace.
func (d *driverImpl) Objects(bucket string) uos.ObjectService {
	return objectService{d: d, defaultBucket: bucket}
}

// Multipart returns the MultipartService view bound to the named bucket.
func (d *driverImpl) Multipart(bucket string) uos.MultipartService {
	return multipartService{d: d, defaultBucket: bucket}
}

// Signer returns the Signer view bound to the named bucket.
func (d *driverImpl) Signer(bucket string) uos.Signer {
	return signerService{d: d, defaultBucket: bucket}
}

// As exposes the underlying huaweicloud-sdk-go-obs handle for callers
// that need vendor-specific features. Supported targets:
//
//   - **obs.ObsClient: filled with the high-level OBS client.
//
// Returns false (without mutating target) for any other type.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **obs.ObsClient:
		*t = d.client
		return true
	default:
		return false
	}
}

// Close releases driver-held resources. The huaweicloud-sdk-go-obs
// *obs.ObsClient owns an http.Transport whose idle connections must be
// drained on shutdown; SDK Close() handles that and nil-clears the
// client / config so leaks are detectable. Idempotent: subsequent calls
// to obs.ObsClient methods after Close panic, so the driver does NOT
// guarantee post-Close usability per the unified Client contract.
func (d *driverImpl) Close() error {
	if d.client != nil {
		d.client.Close()
		d.client = nil
	}
	return nil
}

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService implements uos.BucketService.
type bucketService struct{ d *driverImpl }

// List enumerates buckets visible to the configured credential. OBS's
// ListBuckets supports MaxKeys + Marker pagination via the input struct;
// we honor both via the unified MaxResults / ContinuationToken fields.
func (b bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"
	input := &obs.ListBucketsInput{QueryLocation: true}
	if req.MaxResults > 0 {
		input.MaxKeys = req.MaxResults
	}
	if req.ContinuationToken != "" {
		input.Marker = req.ContinuationToken
	}
	res, err := b.d.client.ListBuckets(input, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(b.d.Provider(), op, "", "", err)
	}
	out := make([]uos.BucketInfo, 0, len(res.Buckets))
	for _, bp := range res.Buckets {
		out = append(out, uos.BucketInfo{
			Name:      bp.Name,
			Region:    bp.Location,
			CreatedAt: bp.CreationDate,
		})
	}
	return out, nil
}

// Create makes a new bucket. OBS rejects already-existing buckets with
// BucketAlreadyExists, which the error mapper translates to
// ErrAlreadyExists.
func (b bucketService) Create(ctx context.Context, req uos.CreateBucketRequest) (*uos.BucketInfo, error) {
	const op = "CreateBucket"
	if req.Name == "" {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  b.d.Provider(),
			Operation: op,
			Message:   "bucket name is required",
		}
	}
	input := &obs.CreateBucketInput{Bucket: req.Name}
	if req.Region != "" {
		input.Location = req.Region
	}
	if req.ACL != "" {
		input.ACL = obs.AclType(req.ACL)
	}
	if _, err := b.d.client.CreateBucket(input, obs.WithRequestContext(ctx)); err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	region := req.Region
	if region == "" {
		region = b.d.cfg.Region
	}
	return &uos.BucketInfo{
		Name:      req.Name,
		Region:    region,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Stat returns BucketInfo for an existing bucket. OBS exposes
// GetBucketMetadata as the lightweight HEAD-flavoured call (returns
// Location / Storage class / Version etc. as response headers). Missing
// buckets surface as ObsError with status 404, mapped to ErrNotFound.
func (b bucketService) Stat(ctx context.Context, req uos.StatBucketRequest) (*uos.BucketInfo, error) {
	const op = "StatBucket"
	if req.Name == "" {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  b.d.Provider(),
			Operation: op,
			Message:   "bucket name is required",
		}
	}
	res, err := b.d.client.GetBucketMetadata(
		&obs.GetBucketMetadataInput{Bucket: req.Name},
		obs.WithRequestContext(ctx),
	)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return &uos.BucketInfo{
		Name:   req.Name,
		Region: res.Location,
	}, nil
}

// Delete removes an empty bucket. Non-empty buckets surface as
// BucketNotEmpty, mapped to ErrConflict by the error layer.
func (b bucketService) Delete(ctx context.Context, req uos.DeleteBucketRequest) error {
	const op = "DeleteBucket"
	if req.Name == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  b.d.Provider(),
			Operation: op,
			Message:   "bucket name is required",
		}
	}
	if _, err := b.d.client.DeleteBucket(req.Name, obs.WithRequestContext(ctx)); err != nil {
		return mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return nil
}

// ----------------------------------------------------------------------
// ObjectService
// ----------------------------------------------------------------------

// objectService implements uos.ObjectService for a fixed bucket. The
// per-request Bucket field is honored when set; defaultBucket fills in
// when callers omit it.
type objectService struct {
	d             *driverImpl
	defaultBucket string
}

// pickBucket returns the explicit bucket from req, falling back to the
// service's default. Bucket-less requests against an unbound service
// produce the empty string and let the wire-level call return
// InvalidArgument naturally.
func (o objectService) pickBucket(reqBucket string) string {
	if reqBucket != "" {
		return reqBucket
	}
	return o.defaultBucket
}

// Put writes a single object via OBS PutObject. Bodies pass through to
// the SDK untouched; the driver requires a known size (Size>=0) because
// OBS's PutObject signs Content-Length into the request and the
// unknown-size streaming path lives in transfer.Manager (bypassed in
// v0.1; promoted to Uploader in v0.2 per ADR Follow-up #1).
func (o objectService) Put(ctx context.Context, req uos.PutObjectRequest) (*uos.PutObjectResult, error) {
	const op = "PutObject"
	bucket := o.pickBucket(req.Bucket)
	if req.Body == nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Body is required",
		}
	}
	if req.Size < 0 {
		return nil, &uos.Error{
			Code: uos.ErrLengthRequired, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: "Size is required (-1 unsupported by OBS PutObject; use multipart for streaming)",
		}
	}
	input := &obs.PutObjectInput{
		PutObjectBasicInput: obs.PutObjectBasicInput{
			ObjectOperationInput: obs.ObjectOperationInput{
				Bucket:   bucket,
				Key:      req.Key,
				Metadata: s3common.LowerMetadataKeys(req.Metadata),
			},
			HttpHeader:    contentHeaderToHTTP(req.Content),
			ContentLength: req.Size,
		},
		Body: req.Body,
	}
	if req.StorageClass != "" {
		input.StorageClass = obs.StorageClassType(req.StorageClass)
	}
	if req.ACL != "" {
		input.ACL = obs.AclType(req.ACL)
	}
	res, err := o.d.client.PutObject(input, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(res.ETag, `"`),
		VersionID: res.VersionId,
	}, nil
}

// Get streams an object body via OBS GetObject. Range requests use the
// SDK's RangeStart / RangeEnd fields (canonical "bytes=start-end" is
// assembled by the SDK). Returned ObjectReader.Body is the raw
// io.ReadCloser; callers MUST Close it.
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)
	input := &obs.GetObjectInput{
		GetObjectMetadataInput: obs.GetObjectMetadataInput{
			Bucket:    bucket,
			Key:       req.Key,
			VersionId: req.VersionID,
		},
		IfMatch:           req.IfMatch,
		IfNoneMatch:       req.IfNoneMatch,
		IfModifiedSince:   req.IfModifiedSince,
		IfUnmodifiedSince: req.IfUnmodifiedSince,
	}
	if req.Range != nil {
		// OBS accepts either explicit RangeStart/RangeEnd OR a "bytes=..."
		// Range string. Use the explicit fields so open-ended ranges
		// (End=-1) are encoded by the SDK as "RangeStart-" without
		// us building the header by hand.
		input.RangeStart = req.Range.Start
		if req.Range.End >= 0 {
			input.RangeEnd = req.Range.End
		} else {
			// SDK treats RangeEnd=0 as "unset" only when RangeStart=0;
			// for open-ended ranges with non-zero start we set the
			// Range string explicitly to avoid ambiguity.
			input.Range = fmt.Sprintf("bytes=%d-", req.Range.Start)
			input.RangeStart = 0
		}
	}
	res, err := o.d.client.GetObject(input, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectInfo(bucket, req.Key, &res.GetObjectMetadataOutput)
	return &uos.ObjectReader{
		Body:          res.Body,
		ContentLength: res.ContentLength,
		Info:          info,
	}, nil
}

// Head returns ObjectInfo without the body. OBS's GetObjectMetadata is
// the HEAD-flavoured call (returns Last-Modified / Content-Length /
// ETag plus user metadata in the parsed map).
func (o objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)
	res, err := o.d.client.GetObjectMetadata(
		&obs.GetObjectMetadataInput{
			Bucket:    bucket,
			Key:       req.Key,
			VersionId: req.VersionID,
		},
		obs.WithRequestContext(ctx),
	)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectInfo(bucket, req.Key, res)
	return &info, nil
}

// Delete removes a single object. OBS DeleteObject is idempotent:
// removing a missing key returns 204 No Content, not 404.
func (o objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)
	_, err := o.d.client.DeleteObject(&obs.DeleteObjectInput{
		Bucket:    bucket,
		Key:       req.Key,
		VersionId: req.VersionID,
	}, obs.WithRequestContext(ctx))
	if err != nil {
		return mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// Exists reports whether an object exists. Per the contract suite, the
// not-found case returns (false, nil); other errors propagate.
func (o objectService) Exists(ctx context.Context, req uos.HeadObjectRequest) (bool, error) {
	_, err := o.Head(ctx, req)
	if err == nil {
		return true, nil
	}
	var ue *uos.Error
	if errors.As(err, &ue) && ue.Code == uos.ErrNotFound {
		return false, nil
	}
	return false, err
}

// DeleteMany removes a batch of keys via OBS DeleteObjects. The SDK
// returns the per-key Deleteds slice (empty when Quiet is set);
// per-key failures arrive as the Errors slice on the same response.
// Whole-call failures (auth, malformed request) surface via the
// outer err.
func (o objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	objs := make([]obs.ObjectToDelete, 0, len(req.Keys))
	for _, k := range req.Keys {
		objs = append(objs, obs.ObjectToDelete{Key: k})
	}
	res, err := o.d.client.DeleteObjects(&obs.DeleteObjectsInput{
		Bucket:  bucket,
		Quiet:   req.Quiet,
		Objects: objs,
	}, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}
	out := &uos.DeleteManyResult{}
	if !req.Quiet {
		for _, d := range res.Deleteds {
			out.Deleted = append(out.Deleted, d.Key)
		}
	}
	for _, e := range res.Errors {
		// Map per-key vendor error codes through the s3common table so
		// the unified Code values are consistent across providers.
		code, ok := s3common.MapCodeString(e.Code)
		if !ok {
			code = uos.ErrInternal
		}
		out.Failed = append(out.Failed, uos.DeleteFailure{
			Key:     e.Key,
			Code:    code,
			Message: e.Message,
		})
	}
	return out, nil
}

// Copy duplicates an object. OBS supports same-account cross-bucket
// copy via x-obs-copy-source; the SDK assembles the header from the
// CopySourceBucket / CopySourceKey fields on the input struct. Cross-
// region copies require both buckets to be in the same OBS instance.
func (o objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	dstBucket := req.DestBucket
	srcBucket := req.SourceBucket
	if srcBucket == "" {
		srcBucket = dstBucket
	}
	input := &obs.CopyObjectInput{
		ObjectOperationInput: obs.ObjectOperationInput{
			Bucket:   dstBucket,
			Key:      req.DestKey,
			Metadata: s3common.LowerMetadataKeys(req.Metadata),
		},
		HttpHeader:            contentHeaderToHTTP(req.Content),
		CopySourceBucket:      srcBucket,
		CopySourceKey:         req.SourceKey,
		CopySourceVersionId:   req.SourceVersionID,
		CopySourceIfMatch:     req.IfMatch,
		CopySourceIfNoneMatch: req.IfNoneMatch,
	}
	switch strings.ToUpper(req.MetadataDirective) {
	case "COPY":
		input.MetadataDirective = obs.CopyMetadata
	case "REPLACE":
		input.MetadataDirective = obs.ReplaceMetadata
	default:
		// OBS default mirrors AWS: COPY when no metadata is supplied,
		// REPLACE when the caller supplies metadata. The unified
		// CopyObjectRequest documents this implicit behaviour.
		if req.Metadata != nil {
			input.MetadataDirective = obs.ReplaceMetadata
		}
	}
	if req.StorageClass != "" {
		input.StorageClass = obs.StorageClassType(req.StorageClass)
	}
	if req.ACL != "" {
		input.ACL = obs.AclType(req.ACL)
	}
	res, err := o.d.client.CopyObject(input, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(o.d.Provider(), op, dstBucket, req.DestKey, err)
	}
	return &uos.CopyObjectResult{
		ETag:         strings.Trim(res.ETag, `"`),
		LastModified: res.LastModified,
		VersionID:    res.VersionId,
	}, nil
}

// List enumerates objects matching prefix / delimiter via OBS
// ListObjects. NextToken round-trips through NextMarker so opaque-
// cursor pagination works across providers.
//
// Note: OBS does not expose a V2-style ContinuationToken; the SDK uses
// Marker (the last key returned in the previous page). The unified
// ContinuationToken field is forwarded as Marker.
func (o objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)
	input := &obs.ListObjectsInput{
		Bucket: bucket,
		ListObjsInput: obs.ListObjsInput{
			Prefix:    req.Prefix,
			Delimiter: req.Delimiter,
			MaxKeys:   req.MaxResults,
		},
	}
	// OBS uses Marker for pagination. The unified ContinuationToken
	// field carries it round-trip; StartAfter has no direct equivalent
	// on OBS so it is mapped to the same Marker field (semantics align
	// for the contract suite's "list past key X" case).
	if req.ContinuationToken != "" {
		input.Marker = req.ContinuationToken
	} else if req.StartAfter != "" {
		input.Marker = req.StartAfter
	}
	res, err := o.d.client.ListObjects(input, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}
	out := &uos.ObjectList{
		Items:          make([]uos.ObjectInfo, 0, len(res.Contents)),
		CommonPrefixes: append([]string(nil), res.CommonPrefixes...),
		NextToken:      res.NextMarker,
		Truncated:      res.IsTruncated,
	}
	for _, c := range res.Contents {
		out.Items = append(out.Items, uos.ObjectInfo{
			Bucket:       bucket,
			Key:          c.Key,
			Size:         c.Size,
			ETag:         strings.Trim(c.ETag, `"`),
			LastModified: c.LastModified,
			StorageClass: string(c.StorageClass),
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// MultipartService
// ----------------------------------------------------------------------

// multipartService implements uos.MultipartService backed by the OBS
// raw multipart primitives (InitiateMultipartUpload / UploadPart /
// CompleteMultipartUpload / AbortMultipartUpload / ListMultipartUploads).
// Multipart is bypass-vendor-native in v0.1 — pkg/uos/transfer.Manager
// is BYPASSED here; promotion lands in v0.2 per ADR Follow-up #1.
type multipartService struct {
	d             *driverImpl
	defaultBucket string
}

// pickBucket mirrors objectService.pickBucket for multipart paths.
func (m multipartService) pickBucket(reqBucket string) string {
	if reqBucket != "" {
		return reqBucket
	}
	return m.defaultBucket
}

// Initiate starts a new multipart upload. The returned MultipartUpload
// carries the OBS-issued UploadID that all subsequent UploadPart /
// Complete / Abort calls must reference.
func (m multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	input := &obs.InitiateMultipartUploadInput{
		ObjectOperationInput: obs.ObjectOperationInput{
			Bucket:   bucket,
			Key:      req.Key,
			Metadata: s3common.LowerMetadataKeys(req.Metadata),
		},
		HttpHeader: contentHeaderToHTTP(req.Content),
	}
	if req.StorageClass != "" {
		input.StorageClass = obs.StorageClassType(req.StorageClass)
	}
	if req.ACL != "" {
		input.ACL = obs.AclType(req.ACL)
	}
	res, err := m.d.client.InitiateMultipartUpload(input, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.MultipartUpload{
		UploadID:     res.UploadId,
		Bucket:       bucket,
		Key:          req.Key,
		Initiated:    time.Now().UTC(),
		StorageClass: req.StorageClass,
		Metadata:     req.Metadata,
	}, nil
}

// UploadPart uploads a single part. OBS's UploadPart expects the
// caller-supplied size as PartSize (used as a hint to the SDK's
// io.LimitedReader wrapper); we forward req.Size verbatim and let the
// wire layer surface InvalidArgument when the body length doesn't
// match.
func (m multipartService) UploadPart(ctx context.Context, req uos.UploadPartRequest) (*uos.UploadedPart, error) {
	const op = "UploadPart"
	bucket := m.pickBucket(req.Bucket)
	if req.Body == nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Body is required",
		}
	}
	if req.Size < 0 {
		return nil, &uos.Error{
			Code: uos.ErrLengthRequired, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Size is required for UploadPart",
		}
	}
	input := &obs.UploadPartInput{
		Bucket:     bucket,
		Key:        req.Key,
		UploadId:   req.UploadID,
		PartNumber: req.PartNumber,
		Body:       req.Body,
		PartSize:   req.Size,
	}
	res, err := m.d.client.UploadPart(input, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.UploadedPart{
		PartNumber: res.PartNumber,
		ETag:       strings.Trim(res.ETag, `"`),
		Size:       req.Size,
	}, nil
}

// Complete finalises the multipart upload by stitching the supplied
// parts in PartNumber order. Parts MUST be presented sorted; the OBS
// SDK forwards the order as-given to the wire so the contract suite
// requirement (caller supplies sorted parts) is enforced server-side.
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if len(req.Parts) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Parts is required and must be non-empty",
		}
	}
	parts := make([]obs.Part, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, obs.Part{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		})
	}
	res, err := m.d.client.CompleteMultipartUpload(&obs.CompleteMultipartUploadInput{
		Bucket:   bucket,
		Key:      req.Key,
		UploadId: req.UploadID,
		Parts:    parts,
	}, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(res.ETag, `"`),
		VersionID: res.VersionId,
	}, nil
}

// Abort cancels an in-flight multipart upload. OBS makes Abort
// idempotent at the wire level (NoSuchUpload returns 204 in some
// regions, ObsError in others); the error mapper translates either to
// ErrNotFound when applicable.
func (m multipartService) Abort(ctx context.Context, req uos.AbortMultipartRequest) error {
	const op = "AbortMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	_, err := m.d.client.AbortMultipartUpload(&obs.AbortMultipartUploadInput{
		Bucket:   bucket,
		Key:      req.Key,
		UploadId: req.UploadID,
	}, obs.WithRequestContext(ctx))
	if err != nil {
		return mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// List enumerates in-flight multipart uploads in the bucket. Pagination
// is handled by OBS via KeyMarker; we expose a single page and surface
// NextToken when more pages remain so callers can iterate.
func (m multipartService) List(ctx context.Context, req uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	const op = "ListMultipartUploads"
	bucket := m.pickBucket(req.Bucket)
	input := &obs.ListMultipartUploadsInput{
		Bucket: bucket,
		Prefix: req.Prefix,
	}
	if req.MaxResults > 0 {
		input.MaxUploads = req.MaxResults
	}
	if req.ContinuationToken != "" {
		input.KeyMarker = req.ContinuationToken
	}
	res, err := m.d.client.ListMultipartUploads(input, obs.WithRequestContext(ctx))
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, "", err)
	}
	out := &uos.MultipartUploadList{
		Uploads:   make([]uos.MultipartUpload, 0, len(res.Uploads)),
		Truncated: res.IsTruncated,
		NextToken: res.NextKeyMarker,
	}
	for _, u := range res.Uploads {
		out.Uploads = append(out.Uploads, uos.MultipartUpload{
			UploadID:     u.UploadId,
			Bucket:       bucket,
			Key:          u.Key,
			Initiated:    u.Initiated,
			StorageClass: string(u.StorageClass),
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// Signer
// ----------------------------------------------------------------------

// signerService implements uos.Signer for OBS HMAC presigned URLs.
type signerService struct {
	d             *driverImpl
	defaultBucket string
}

// pickBucket mirrors the per-service convention.
func (s signerService) pickBucket(reqBucket string) string {
	if reqBucket != "" {
		return reqBucket
	}
	return s.defaultBucket
}

// SignURL returns an HTTP-signed URL for the requested operation. OBS's
// CreateSignedUrl builds the URL synchronously (no I/O) and returns it
// alongside the headers the caller MUST attach to the wire request.
func (s signerService) SignURL(ctx context.Context, req uos.SignURLRequest) (*uos.SignedURL, error) {
	_ = ctx // CreateSignedUrl performs no I/O; ctx is unused.
	const op = "SignURL"
	bucket := s.pickBucket(req.Bucket)
	if req.ExpiresIn <= 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "ExpiresIn must be > 0",
		}
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Method is required",
		}
	}
	var obsMethod obs.HttpMethodType
	switch method {
	case http.MethodGet:
		obsMethod = obs.HttpMethodGet
	case http.MethodPut:
		obsMethod = obs.HttpMethodPut
	case http.MethodHead:
		obsMethod = obs.HttpMethodHead
	case http.MethodDelete:
		obsMethod = obs.HttpMethodDelete
	case http.MethodPost:
		obsMethod = obs.HttpMethodPost
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q (allowed: GET, PUT, HEAD, DELETE, POST)", method),
		}
	}

	headers := make(map[string]string, len(req.Headers))
	for k, vs := range req.Headers {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}
	queries := make(map[string]string, len(req.Query))
	for k, v := range req.Query {
		queries[k] = v
	}
	if req.VersionID != "" {
		queries["versionId"] = req.VersionID
	}

	expiresIn := int(req.ExpiresIn / time.Second)
	if expiresIn <= 0 {
		expiresIn = 1
	}
	out, err := s.d.client.CreateSignedUrl(&obs.CreateSignedUrlInput{
		Method:      obsMethod,
		Bucket:      bucket,
		Key:         req.Key,
		Expires:     expiresIn,
		Headers:     headers,
		QueryParams: queries,
	})
	if err != nil {
		return nil, mapError(s.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.SignedURL{
		URL:       out.SignedUrl,
		Method:    method,
		ExpiresAt: time.Now().Add(req.ExpiresIn).UTC(),
		Headers:   out.ActualSignedRequestHeaders,
	}, nil
}

// IssueDirectGrant always returns ErrUnsupported / CapDirectGrant
// because S3-family providers (including OBS) issue write
// authorisations as URLs (use SignURL with PUT). See
// docs/provider_matrix.md footnote 5.
func (s signerService) IssueDirectGrant(_ context.Context, req uos.DirectGrantRequest) (*uos.DirectGrant, error) {
	return nil, &uos.Error{
		Provider:   s.d.Provider(),
		Operation:  "IssueDirectGrant",
		Bucket:     req.Bucket,
		Key:        req.Key,
		Code:       uos.ErrUnsupported,
		Capability: capability.CapDirectGrant,
		Message:    "OBS uses presigned URL — use Signer.SignURL instead",
	}
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// contentHeaderToHTTP converts the unified ContentHeaders into the
// vendor-typed obs.HttpHeader struct the OBS SDK accepts on Put / Copy
// / Initiate inputs. Empty fields stay empty so the vendor default is
// preserved.
//
// OBS's HttpHeader has no Expires time.Time — it uses a string
// HttpExpires field. We format with http.TimeFormat (RFC 1123) when the
// caller supplied a non-zero value so the wire representation matches
// the standard HTTP Expires header.
func contentHeaderToHTTP(c uos.ContentHeaders) obs.HttpHeader {
	h := obs.HttpHeader{
		ContentType:        c.ContentType,
		ContentEncoding:    c.ContentEncoding,
		ContentLanguage:    c.ContentLanguage,
		ContentDisposition: c.ContentDisposition,
		CacheControl:       c.CacheControl,
	}
	if !c.Expires.IsZero() {
		h.HttpExpires = c.Expires.UTC().Format(http.TimeFormat)
	}
	return h
}

// translateObjectInfo rebuilds a uos.ObjectInfo from an OBS metadata
// response. The OBS SDK already parses user metadata into the
// Metadata map (lower-cased keys, prefix stripped), so we forward it
// after a defensive lower-case pass via s3common.LowerMetadataKeys.
func translateObjectInfo(bucket, key string, meta *obs.GetObjectMetadataOutput) uos.ObjectInfo {
	info := uos.ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		Size:         meta.ContentLength,
		ETag:         strings.Trim(meta.ETag, `"`),
		LastModified: meta.LastModified,
		StorageClass: string(meta.StorageClass),
		VersionID:    meta.VersionId,
		Content: uos.ContentHeaders{
			ContentType:        meta.ContentType,
			ContentEncoding:    meta.ContentEncoding,
			ContentLanguage:    meta.ContentLanguage,
			ContentDisposition: meta.ContentDisposition,
			CacheControl:       meta.CacheControl,
		},
		Metadata: s3common.LowerMetadataKeys(meta.Metadata),
	}
	if meta.HttpExpires != "" {
		if t, err := http.ParseTime(meta.HttpExpires); err == nil {
			info.Content.Expires = t
		}
	}
	return info
}

// Compile-time assertion that driverImpl satisfies the full uos.Client
// surface. Drift in pkg/uos.Client surfaces as a build failure here,
// where the fix is unambiguous.
var (
	_ uos.Client           = (*driverImpl)(nil)
	_ uos.BucketService    = bucketService{}
	_ uos.ObjectService    = objectService{}
	_ uos.MultipartService = multipartService{}
	_ uos.Signer           = signerService{}
)
