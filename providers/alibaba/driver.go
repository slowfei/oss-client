package alibaba

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/capability"
	"github.com/maqian/oss-client/pkg/uos/s3common"
)

// driverImpl implements pkg/uos.Client by translating to/from
// github.com/aliyun/aliyun-oss-go-sdk/oss. It holds the SDK *oss.Client
// (used for bucket-scoped service operations) and constructs an
// *oss.Bucket per request when one is needed.
//
// driverImpl is safe for concurrent use; *oss.Client is itself
// goroutine-safe and per-call *oss.Bucket handles are cheap to allocate.
type driverImpl struct {
	cfg    uos.Config
	client *oss.Client
}

// Provider returns "alibaba". Required by uos.Client.
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

// As exposes the underlying aliyun-oss-go-sdk handle for callers that
// need vendor-specific features. Supported targets:
//
//   - **oss.Client: filled with the high-level OSS client.
//
// Returns false (without mutating target) for any other type.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **oss.Client:
		*t = d.client
		return true
	default:
		return false
	}
}

// Close releases any driver-held resources. The aliyun-oss-go-sdk
// *oss.Client owns no goroutines that require shutdown beyond the
// underlying http.Client transport, so this is a no-op kept here to
// satisfy the uos.Client interface.
func (d *driverImpl) Close() error { return nil }

// bucketHandle returns an *oss.Bucket bound to name. The SDK's Bucket
// constructor performs only a name-validation check; no network I/O.
// On invalid bucket name we surface ErrInvalidArgument so the unified
// error contract holds without an extra wire round-trip.
func (d *driverImpl) bucketHandle(op, name string) (*oss.Bucket, error) {
	b, err := d.client.Bucket(name)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: op,
			Bucket:    name,
			Message:   err.Error(),
			Cause:     err,
		}
	}
	return b, nil
}

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService implements uos.BucketService.
type bucketService struct{ d *driverImpl }

// List enumerates buckets visible to the configured credential. OSS's
// ListBuckets supports MaxKeys + Marker pagination via Option helpers;
// we honor both via the unified MaxResults / ContinuationToken fields.
func (b bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.MaxResults > 0 {
		opts = append(opts, oss.MaxKeys(req.MaxResults))
	}
	if req.ContinuationToken != "" {
		opts = append(opts, oss.Marker(req.ContinuationToken))
	}
	res, err := b.d.client.ListBuckets(opts...)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, "", "", err)
	}
	out := make([]uos.BucketInfo, 0, len(res.Buckets))
	for _, bp := range res.Buckets {
		out = append(out, uos.BucketInfo{
			Name:      bp.Name,
			Region:    bp.Region,
			CreatedAt: bp.CreationDate,
		})
	}
	return out, nil
}

// Create makes a new bucket. OSS rejects already-existing buckets with
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
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.ACL != "" {
		opts = append(opts, oss.ACL(oss.ACLType(req.ACL)))
	}
	if err := b.d.client.CreateBucket(req.Name, opts...); err != nil {
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

// Stat returns BucketInfo for an existing bucket. OSS's IsBucketExist
// swallows NoSuchBucket and returns (false, nil); we surface that as
// ErrNotFound so callers can errors.Is on the contract.
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
	exists, err := b.d.client.IsBucketExist(req.Name)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	if !exists {
		return nil, &uos.Error{
			Code:      uos.ErrNotFound,
			Provider:  b.d.Provider(),
			Operation: op,
			Bucket:    req.Name,
			Message:   "bucket does not exist",
		}
	}
	region, _ := b.d.client.GetBucketLocation(req.Name, oss.WithContext(ctx))
	return &uos.BucketInfo{Name: req.Name, Region: region}, nil
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
	if err := b.d.client.DeleteBucket(req.Name, oss.WithContext(ctx)); err != nil {
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

// Put writes a single object via OSS PutObject. Bodies pass through to
// the SDK untouched; the driver requires a known size (Size>=0) because
// OSS's PutObject signs Content-Length into the request and the
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
			Message: "Size is required (-1 unsupported by OSS PutObject; use multipart for streaming)",
		}
	}
	bh, err := o.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx), oss.ContentLength(req.Size)}
	opts = append(opts, contentHeaderOptions(req.Content)...)
	opts = append(opts, metadataOptions(req.Metadata)...)
	if req.StorageClass != "" {
		opts = append(opts, oss.ObjectStorageClass(oss.StorageClassType(req.StorageClass)))
	}
	if req.ACL != "" {
		opts = append(opts, oss.ObjectACL(oss.ACLType(req.ACL)))
	}
	if req.IfMatch != "" {
		opts = append(opts, oss.IfMatch(req.IfMatch))
	}
	if req.IfNoneMatch != "" {
		opts = append(opts, oss.IfNoneMatch(req.IfNoneMatch))
	}
	var respHeader http.Header
	opts = append(opts, oss.GetResponseHeader(&respHeader))
	if err := bh.PutObject(req.Key, req.Body, opts...); err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(respHeader.Get(oss.HTTPHeaderEtag), `"`),
		VersionID: respHeader.Get("X-Oss-Version-Id"),
	}, nil
}

// Get streams an object body via OSS GetObject. Range requests use the
// SDK's NormalizedRange option (formats "bytes=start-end" identically
// to the unified ByteRange contract). Returned ObjectReader.Body is the
// raw io.ReadCloser; callers MUST Close it.
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)
	bh, err := o.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.VersionID != "" {
		opts = append(opts, oss.VersionId(req.VersionID))
	}
	if req.Range != nil {
		opts = append(opts, oss.NormalizedRange(formatRange(*req.Range)))
		// RangeBehavior=standard makes OSS return 416 InvalidRange when
		// the requested range is unsatisfiable, mirroring S3 / MinIO
		// semantics that the contract suite expects.
		opts = append(opts, oss.RangeBehavior("standard"))
	}
	if req.IfMatch != "" {
		opts = append(opts, oss.IfMatch(req.IfMatch))
	}
	if req.IfNoneMatch != "" {
		opts = append(opts, oss.IfNoneMatch(req.IfNoneMatch))
	}
	if !req.IfModifiedSince.IsZero() {
		opts = append(opts, oss.IfModifiedSince(req.IfModifiedSince))
	}
	if !req.IfUnmodifiedSince.IsZero() {
		opts = append(opts, oss.IfUnmodifiedSince(req.IfUnmodifiedSince))
	}
	var respHeader http.Header
	opts = append(opts, oss.GetResponseHeader(&respHeader))
	body, err := bh.GetObject(req.Key, opts...)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectInfo(bucket, req.Key, respHeader)
	contentLen := info.Size
	return &uos.ObjectReader{
		Body:          body,
		ContentLength: contentLen,
		Info:          info,
	}, nil
}

// Head returns ObjectInfo without the body. OSS's GetObjectMeta is the
// lightweight HEAD-flavoured call (returns Last-Modified / Content-Length /
// ETag plus user metadata headers).
func (o objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)
	bh, err := o.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.VersionID != "" {
		opts = append(opts, oss.VersionId(req.VersionID))
	}
	// GetObjectDetailedMeta returns the full header set including
	// x-oss-meta-* user metadata; GetObjectMeta returns only the basic
	// trio (ETag / Last-Modified / Content-Length).
	header, err := bh.GetObjectDetailedMeta(req.Key, opts...)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectInfo(bucket, req.Key, header)
	return &info, nil
}

// Delete removes a single object. OSS DeleteObject is idempotent:
// removing a missing key returns 204 No Content, not 404.
func (o objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)
	bh, err := o.d.bucketHandle(op, bucket)
	if err != nil {
		return err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.VersionID != "" {
		opts = append(opts, oss.VersionId(req.VersionID))
	}
	if err := bh.DeleteObject(req.Key, opts...); err != nil {
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

// DeleteMany removes a batch of keys via OSS DeleteObjects. The SDK
// returns the per-key Deleted slice (or an empty slice when Quiet is
// set); per-key failures arrive as the wire-level XML <Error> entries
// folded into a single ServiceError when the whole call fails.
func (o objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	bh, err := o.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.Quiet {
		opts = append(opts, oss.DeleteObjectsQuiet(true))
	}
	res, err := bh.DeleteObjects(req.Keys, opts...)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}
	out := &uos.DeleteManyResult{}
	if !req.Quiet {
		out.Deleted = append(out.Deleted, res.DeletedObjects...)
	}
	return out, nil
}

// Copy duplicates an object. OSS supports same-account cross-bucket
// copy via x-oss-copy-source; we use CopyObjectFrom when the source
// bucket differs from the destination so the SDK assembles the header
// against the source bucket.
func (o objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	dstBucket := req.DestBucket
	bh, err := o.d.bucketHandle(op, dstBucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	opts = append(opts, contentHeaderOptions(req.Content)...)
	opts = append(opts, metadataOptions(req.Metadata)...)
	if req.SourceVersionID != "" {
		opts = append(opts, oss.VersionId(req.SourceVersionID))
	}
	switch strings.ToUpper(req.MetadataDirective) {
	case "COPY":
		opts = append(opts, oss.MetadataDirective(oss.MetaCopy))
	case "REPLACE":
		opts = append(opts, oss.MetadataDirective(oss.MetaReplace))
	default:
		// OSS default mirrors AWS: COPY when no metadata is supplied,
		// REPLACE when the caller supplies metadata. The unified
		// CopyObjectRequest documents this implicit behaviour.
		if req.Metadata != nil {
			opts = append(opts, oss.MetadataDirective(oss.MetaReplace))
		}
	}
	if req.StorageClass != "" {
		opts = append(opts, oss.ObjectStorageClass(oss.StorageClassType(req.StorageClass)))
	}
	if req.ACL != "" {
		opts = append(opts, oss.ObjectACL(oss.ACLType(req.ACL)))
	}
	if req.IfMatch != "" {
		opts = append(opts, oss.CopySourceIfMatch(req.IfMatch))
	}
	if req.IfNoneMatch != "" {
		opts = append(opts, oss.CopySourceIfNoneMatch(req.IfNoneMatch))
	}
	var respHeader http.Header
	opts = append(opts, oss.GetResponseHeader(&respHeader))

	var copyRes oss.CopyObjectResult
	if req.SourceBucket == "" || req.SourceBucket == dstBucket {
		copyRes, err = bh.CopyObject(req.SourceKey, req.DestKey, opts...)
	} else {
		copyRes, err = bh.CopyObjectFrom(req.SourceBucket, req.SourceKey, req.DestKey, opts...)
	}
	if err != nil {
		return nil, mapError(o.d.Provider(), op, dstBucket, req.DestKey, err)
	}
	return &uos.CopyObjectResult{
		ETag:         strings.Trim(copyRes.ETag, `"`),
		LastModified: copyRes.LastModified,
		VersionID:    respHeader.Get("X-Oss-Version-Id"),
	}, nil
}

// List enumerates objects matching prefix / delimiter via OSS
// ListObjectsV2. NextToken round-trips through the V2
// NextContinuationToken so opaque-cursor pagination works across
// providers.
func (o objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)
	bh, err := o.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.Prefix != "" {
		opts = append(opts, oss.Prefix(req.Prefix))
	}
	if req.Delimiter != "" {
		opts = append(opts, oss.Delimiter(req.Delimiter))
	}
	if req.MaxResults > 0 {
		opts = append(opts, oss.MaxKeys(req.MaxResults))
	}
	if req.ContinuationToken != "" {
		opts = append(opts, oss.ContinuationToken(req.ContinuationToken))
	}
	if req.StartAfter != "" {
		opts = append(opts, oss.StartAfter(req.StartAfter))
	}
	res, err := bh.ListObjectsV2(opts...)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}
	out := &uos.ObjectList{
		Items:          make([]uos.ObjectInfo, 0, len(res.Objects)),
		CommonPrefixes: append([]string(nil), res.CommonPrefixes...),
		NextToken:      res.NextContinuationToken,
		Truncated:      res.IsTruncated,
	}
	for _, op := range res.Objects {
		out.Items = append(out.Items, uos.ObjectInfo{
			Bucket:       bucket,
			Key:          op.Key,
			Size:         op.Size,
			ETag:         strings.Trim(op.ETag, `"`),
			LastModified: op.LastModified,
			StorageClass: op.StorageClass,
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// MultipartService
// ----------------------------------------------------------------------

// multipartService implements uos.MultipartService backed by the OSS
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
// carries the OSS-issued UploadID that all subsequent UploadPart /
// Complete / Abort calls must reference.
func (m multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	bh, err := m.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	opts = append(opts, contentHeaderOptions(req.Content)...)
	opts = append(opts, metadataOptions(req.Metadata)...)
	if req.StorageClass != "" {
		opts = append(opts, oss.ObjectStorageClass(oss.StorageClassType(req.StorageClass)))
	}
	if req.ACL != "" {
		opts = append(opts, oss.ObjectACL(oss.ACLType(req.ACL)))
	}
	imur, err := bh.InitiateMultipartUpload(req.Key, opts...)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.MultipartUpload{
		UploadID:     imur.UploadID,
		Bucket:       bucket,
		Key:          req.Key,
		Initiated:    time.Now().UTC(),
		StorageClass: req.StorageClass,
		Metadata:     req.Metadata,
	}, nil
}

// UploadPart uploads a single part. OSS's UploadPart expects the
// caller-supplied size as a hint to the SDK's io.LimitedReader wrapper;
// we forward req.Size verbatim and let the wire layer surface
// InvalidArgument when the body length doesn't match.
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
	bh, err := m.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	imur := oss.InitiateMultipartUploadResult{
		Bucket:   bucket,
		Key:      req.Key,
		UploadID: req.UploadID,
	}
	part, err := bh.UploadPart(imur, req.Body, req.Size, req.PartNumber, oss.WithContext(ctx))
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.UploadedPart{
		PartNumber: part.PartNumber,
		ETag:       strings.Trim(part.ETag, `"`),
		Size:       req.Size,
	}, nil
}

// Complete finalises the multipart upload by stitching the supplied
// parts in PartNumber order. Parts MUST be presented sorted; the OSS
// SDK re-sorts internally (defence in depth) but the contract suite
// requires the caller to supply the sorted order regardless.
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if len(req.Parts) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Parts is required and must be non-empty",
		}
	}
	bh, err := m.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	imur := oss.InitiateMultipartUploadResult{
		Bucket:   bucket,
		Key:      req.Key,
		UploadID: req.UploadID,
	}
	parts := make([]oss.UploadPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, oss.UploadPart{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		})
	}
	var respHeader http.Header
	opts := []oss.Option{oss.WithContext(ctx), oss.GetResponseHeader(&respHeader)}
	res, err := bh.CompleteMultipartUpload(imur, parts, opts...)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(res.ETag, `"`),
		VersionID: respHeader.Get("X-Oss-Version-Id"),
	}, nil
}

// Abort cancels an in-flight multipart upload. OSS makes Abort
// idempotent at the wire level (NoSuchUpload returns 204 in some
// regions, ServiceError in others); the error mapper translates either
// to ErrNotFound when applicable.
func (m multipartService) Abort(ctx context.Context, req uos.AbortMultipartRequest) error {
	const op = "AbortMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	bh, err := m.d.bucketHandle(op, bucket)
	if err != nil {
		return err
	}
	imur := oss.InitiateMultipartUploadResult{
		Bucket:   bucket,
		Key:      req.Key,
		UploadID: req.UploadID,
	}
	if err := bh.AbortMultipartUpload(imur, oss.WithContext(ctx)); err != nil {
		return mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// List enumerates in-flight multipart uploads in the bucket. Pagination
// is handled by OSS via KeyMarker; we expose a single page (the vendor
// default cap is 1000 uploads) and surface NextToken when more pages
// remain so callers can iterate.
func (m multipartService) List(ctx context.Context, req uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	const op = "ListMultipartUploads"
	bucket := m.pickBucket(req.Bucket)
	bh, err := m.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.Prefix != "" {
		opts = append(opts, oss.Prefix(req.Prefix))
	}
	if req.MaxResults > 0 {
		opts = append(opts, oss.MaxUploads(req.MaxResults))
	}
	if req.ContinuationToken != "" {
		opts = append(opts, oss.KeyMarker(req.ContinuationToken))
	}
	res, err := bh.ListMultipartUploads(opts...)
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
			UploadID:  u.UploadID,
			Bucket:    bucket,
			Key:       u.Key,
			Initiated: u.Initiated,
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// Signer
// ----------------------------------------------------------------------

// signerService implements uos.Signer for OSS HMAC presigned URLs.
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

// SignURL returns an HTTP-signed URL for the requested operation. OSS's
// Bucket.SignURL builds the URL synchronously (no I/O) and returns it as
// a string; we wrap it in the unified SignedURL shape.
func (s signerService) SignURL(ctx context.Context, req uos.SignURLRequest) (*uos.SignedURL, error) {
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
	var ossMethod oss.HTTPMethod
	switch method {
	case http.MethodGet:
		ossMethod = oss.HTTPGet
	case http.MethodPut:
		ossMethod = oss.HTTPPut
	case http.MethodHead:
		ossMethod = oss.HTTPHead
	case http.MethodDelete:
		ossMethod = oss.HTTPDelete
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q (allowed: GET, PUT, HEAD, DELETE)", method),
		}
	}
	bh, err := s.d.bucketHandle(op, bucket)
	if err != nil {
		return nil, err
	}
	opts := []oss.Option{oss.WithContext(ctx)}
	if req.VersionID != "" {
		opts = append(opts, oss.VersionId(req.VersionID))
	}
	for k, v := range req.Query {
		opts = append(opts, oss.AddParam(k, v))
	}
	for k, vs := range req.Headers {
		if len(vs) == 0 {
			continue
		}
		opts = append(opts, oss.SetHeader(k, vs[0]))
	}
	expiresIn := int64(req.ExpiresIn / time.Second)
	if expiresIn <= 0 {
		expiresIn = 1
	}
	signed, err := bh.SignURL(req.Key, ossMethod, expiresIn, opts...)
	if err != nil {
		return nil, mapError(s.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.SignedURL{
		URL:       signed,
		Method:    method,
		ExpiresAt: time.Now().Add(req.ExpiresIn).UTC(),
		Headers:   req.Headers,
	}, nil
}

// IssueDirectGrant always returns ErrUnsupported / CapDirectGrant
// because S3-family providers (including OSS) issue write
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
		Message:    "OSS uses presigned URL — use Signer.SignURL instead",
	}
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// metadataOptions converts a uos.Metadata map into the per-key
// oss.Meta(...) options the OSS SDK expects. Keys are lower-cased via
// s3common.LowerMetadataKeys; the OSS wire layer stamps the
// "x-oss-meta-" prefix automatically (the Meta option only takes the
// suffix).
func metadataOptions(m uos.Metadata) []oss.Option {
	lower := s3common.LowerMetadataKeys(m)
	if len(lower) == 0 {
		return nil
	}
	opts := make([]oss.Option, 0, len(lower))
	for k, v := range lower {
		opts = append(opts, oss.Meta(k, v))
	}
	return opts
}

// contentHeaderOptions stamps a uos.ContentHeaders onto the matching
// oss.* per-header options. Empty fields leave the header off so the
// vendor default is preserved.
func contentHeaderOptions(c uos.ContentHeaders) []oss.Option {
	var opts []oss.Option
	if c.ContentType != "" {
		opts = append(opts, oss.ContentType(c.ContentType))
	}
	if c.ContentEncoding != "" {
		opts = append(opts, oss.ContentEncoding(c.ContentEncoding))
	}
	if c.ContentLanguage != "" {
		opts = append(opts, oss.ContentLanguage(c.ContentLanguage))
	}
	if c.ContentDisposition != "" {
		opts = append(opts, oss.ContentDisposition(c.ContentDisposition))
	}
	if c.CacheControl != "" {
		opts = append(opts, oss.CacheControl(c.CacheControl))
	}
	if !c.Expires.IsZero() {
		opts = append(opts, oss.Expires(c.Expires))
	}
	return opts
}

// translateObjectInfo rebuilds a uos.ObjectInfo from an OSS response
// header set. The OSS user-metadata convention prefixes each key with
// "X-Oss-Meta-"; we strip the prefix and lower-case the remainder via
// s3common.LowerMetadataKeys for round-trip equality with the unified
// Metadata contract.
func translateObjectInfo(bucket, key string, header http.Header) uos.ObjectInfo {
	info := uos.ObjectInfo{
		Bucket: bucket,
		Key:    key,
		ETag:   strings.Trim(header.Get(oss.HTTPHeaderEtag), `"`),
	}
	if v := header.Get(oss.HTTPHeaderContentLength); v != "" {
		var size int64
		if _, err := fmt.Sscanf(v, "%d", &size); err == nil {
			info.Size = size
		} else {
			info.Size = -1
		}
	} else {
		info.Size = -1
	}
	if v := header.Get(oss.HTTPHeaderLastModified); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			info.LastModified = t
		}
	}
	if v := header.Get("X-Oss-Storage-Class"); v != "" {
		info.StorageClass = v
	}
	if v := header.Get("X-Oss-Version-Id"); v != "" {
		info.VersionID = v
	}
	info.Content = uos.ContentHeaders{
		ContentType:        header.Get(oss.HTTPHeaderContentType),
		ContentEncoding:    header.Get(oss.HTTPHeaderContentEncoding),
		ContentLanguage:    header.Get(oss.HTTPHeaderContentLanguage),
		ContentDisposition: header.Get(oss.HTTPHeaderContentDisposition),
		CacheControl:       header.Get(oss.HTTPHeaderCacheControl),
	}
	if v := header.Get(oss.HTTPHeaderExpires); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			info.Content.Expires = t
		}
	}
	info.Metadata = extractUserMetadata(header)
	return info
}

// extractUserMetadata pulls the lower-cased user-defined metadata out of
// an OSS response header set. OSS prefixes each key with "X-Oss-Meta-";
// we strip the prefix and lower-case the remainder. Returns nil for an
// empty result so the unified ObjectInfo.Metadata contract (nil ==
// "no metadata") is preserved.
func extractUserMetadata(h http.Header) uos.Metadata {
	const prefix = "X-Oss-Meta-"
	out := uos.Metadata{}
	for k, vs := range h {
		if !strings.HasPrefix(k, prefix) || len(vs) == 0 {
			continue
		}
		out[strings.ToLower(strings.TrimPrefix(k, prefix))] = vs[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// formatRange renders a uos.ByteRange as the HTTP Range header value
// the OSS SDK's NormalizedRange option expects ("start-end" or
// "start-"). The "bytes=" prefix is added by the SDK.
func formatRange(r uos.ByteRange) string {
	if r.End < 0 {
		return fmt.Sprintf("%d-", r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End)
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
