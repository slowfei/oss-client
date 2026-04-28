package volcengine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/capability"
	"github.com/maqian/oss-client/pkg/uos/s3common"
)

// driverImpl implements pkg/uos.Client by translating to/from
// github.com/volcengine/ve-tos-golang-sdk/v2/tos. It holds the SDK
// *tos.ClientV2; per-call work just feeds typed *Input structs into the
// vendor methods and translates *Output structs into unified responses.
//
// driverImpl is safe for concurrent use; *tos.ClientV2 is itself
// goroutine-safe.
type driverImpl struct {
	cfg    uos.Config
	client *tos.ClientV2
}

// Provider returns "volcengine". Required by uos.Client.
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

// As exposes the underlying ve-tos-golang-sdk handle for callers that
// need vendor-specific features. Supported targets:
//
//   - **tos.ClientV2: filled with the high-level TOS V2 client.
//
// Returns false (without mutating target) for any other type.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **tos.ClientV2:
		*t = d.client
		return true
	default:
		return false
	}
}

// Close releases any driver-held resources. The TOS ClientV2 owns no
// goroutines that require explicit shutdown beyond its underlying
// http.Client transport, so this is a no-op kept here to satisfy the
// uos.Client interface. (The SDK's Close() on V1 *Client is a separate
// type; ClientV2 callers don't need it.)
func (d *driverImpl) Close() error { return nil }

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService implements uos.BucketService.
type bucketService struct{ d *driverImpl }

// List enumerates buckets visible to the configured credential. TOS's
// ListBuckets does not paginate (it returns the full set in one call);
// the unified MaxResults / ContinuationToken fields are accepted but
// have no effect at the wire level — we simply return everything.
func (b bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"
	_ = req // ListBuckets has no pagination in TOS; req fields are accepted for unified-API parity.
	res, err := b.d.client.ListBuckets(ctx, &tos.ListBucketsInput{})
	if err != nil {
		return nil, mapError(b.d.Provider(), op, "", "", err)
	}
	out := make([]uos.BucketInfo, 0, len(res.Buckets))
	for _, bp := range res.Buckets {
		bi := uos.BucketInfo{
			Name:   bp.Name,
			Region: bp.Location,
		}
		if t, perr := time.Parse(time.RFC3339, bp.CreationDate); perr == nil {
			bi.CreatedAt = t
		}
		out = append(out, bi)
	}
	return out, nil
}

// Create makes a new bucket. TOS rejects already-existing buckets with
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
	in := &tos.CreateBucketV2Input{Bucket: req.Name}
	if req.ACL != "" {
		in.ACL = enum.ACLType(req.ACL)
	}
	if _, err := b.d.client.CreateBucketV2(ctx, in); err != nil {
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

// Stat returns BucketInfo for an existing bucket. TOS HeadBucket
// returns 200 + Region/StorageClass on success and 404
// "NoSuchBucket" on miss; the error mapper translates the latter to
// ErrNotFound.
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
	res, err := b.d.client.HeadBucket(ctx, &tos.HeadBucketInput{Bucket: req.Name})
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return &uos.BucketInfo{Name: req.Name, Region: res.Region}, nil
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
	if _, err := b.d.client.DeleteBucket(ctx, &tos.DeleteBucketInput{Bucket: req.Name}); err != nil {
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

// Put writes a single object via TOS PutObjectV2. Bodies pass through
// to the SDK untouched; the driver requires a known size (Size>=0)
// because TOS PutObjectV2 signs Content-Length into the request and the
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
			Message: "Size is required (-1 unsupported by TOS PutObjectV2; use multipart for streaming)",
		}
	}
	in := &tos.PutObjectV2Input{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket:        bucket,
			Key:           req.Key,
			ContentLength: req.Size,
			Meta:          s3common.LowerMetadataKeys(req.Metadata),
		},
		Content: req.Body,
	}
	applyContentHeaders(&in.PutObjectBasicInput, req.Content)
	if req.StorageClass != "" {
		in.StorageClass = enum.StorageClassType(req.StorageClass)
	}
	if req.ACL != "" {
		in.ACL = enum.ACLType(req.ACL)
	}
	if req.IfMatch != "" {
		in.IfMatch = req.IfMatch
	}
	res, err := o.d.client.PutObjectV2(ctx, in)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(res.ETag, `"`),
		VersionID: res.VersionID,
	}, nil
}

// Get streams an object body via TOS GetObjectV2. Range requests use
// the SDK's input.Range string field, which expects the full HTTP
// "bytes=start-end" header value; we format the unified ByteRange
// accordingly. Returned ObjectReader.Body is the raw io.ReadCloser;
// callers MUST Close it.
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)
	in := &tos.GetObjectV2Input{
		Bucket:    bucket,
		Key:       req.Key,
		VersionID: req.VersionID,
	}
	if req.Range != nil {
		in.Range = formatRangeHeader(*req.Range)
	}
	if req.IfMatch != "" {
		in.IfMatch = req.IfMatch
	}
	if req.IfNoneMatch != "" {
		in.IfNoneMatch = req.IfNoneMatch
	}
	if !req.IfModifiedSince.IsZero() {
		in.IfModifiedSince = req.IfModifiedSince
	}
	if !req.IfUnmodifiedSince.IsZero() {
		in.IfUnmodifiedSince = req.IfUnmodifiedSince
	}
	res, err := o.d.client.GetObjectV2(ctx, in)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectMetaV2(bucket, req.Key, &res.GetObjectBasicOutput.ObjectMetaV2)
	return &uos.ObjectReader{
		Body:          res.Content,
		ContentLength: res.GetObjectBasicOutput.ObjectMetaV2.ContentLength,
		Info:          info,
	}, nil
}

// Head returns ObjectInfo without the body. TOS HeadObjectV2 returns
// the full ObjectMetaV2 header set including x-tos-meta-* user metadata.
func (o objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)
	in := &tos.HeadObjectV2Input{
		Bucket:    bucket,
		Key:       req.Key,
		VersionID: req.VersionID,
	}
	res, err := o.d.client.HeadObjectV2(ctx, in)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectMetaV2(bucket, req.Key, &res.ObjectMetaV2)
	return &info, nil
}

// Delete removes a single object. TOS DeleteObjectV2 is idempotent:
// removing a missing key returns 204 No Content, not 404.
func (o objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)
	in := &tos.DeleteObjectV2Input{
		Bucket:    bucket,
		Key:       req.Key,
		VersionID: req.VersionID,
	}
	if _, err := o.d.client.DeleteObjectV2(ctx, in); err != nil {
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

// DeleteMany removes a batch of keys via TOS DeleteMultiObjects. The
// SDK returns Deleted (success entries) and Error (per-key failures);
// we surface both via the unified DeleteManyResult.
func (o objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	objs := make([]tos.ObjectTobeDeleted, 0, len(req.Keys))
	for _, k := range req.Keys {
		objs = append(objs, tos.ObjectTobeDeleted{Key: k})
	}
	in := &tos.DeleteMultiObjectsInput{
		Bucket:  bucket,
		Objects: objs,
		Quiet:   req.Quiet,
	}
	res, err := o.d.client.DeleteMultiObjects(ctx, in)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}
	out := &uos.DeleteManyResult{}
	if !req.Quiet {
		out.Deleted = make([]string, 0, len(res.Deleted))
		for _, d := range res.Deleted {
			out.Deleted = append(out.Deleted, d.Key)
		}
	}
	for _, e := range res.Error {
		out.Failed = append(out.Failed, uos.DeleteFailure{
			Key:     e.Key,
			Code:    mapServiceCode(e.Code, 0),
			Message: e.Message,
		})
	}
	return out, nil
}

// Copy duplicates an object. TOS CopyObject supports same-account
// cross-bucket copy via SrcBucket / SrcKey; the destination is set on
// the top-level Bucket / Key fields.
func (o objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	dstBucket := req.DestBucket
	srcBucket := req.SourceBucket
	if srcBucket == "" {
		srcBucket = dstBucket
	}
	in := &tos.CopyObjectInput{
		Bucket:       dstBucket,
		Key:          req.DestKey,
		SrcBucket:    srcBucket,
		SrcKey:       req.SourceKey,
		SrcVersionID: req.SourceVersionID,
		Meta:         s3common.LowerMetadataKeys(req.Metadata),
	}
	if c := req.Content; c.ContentType != "" {
		in.ContentType = c.ContentType
	}
	if c := req.Content; c.ContentEncoding != "" {
		in.ContentEncoding = c.ContentEncoding
	}
	if c := req.Content; c.ContentLanguage != "" {
		in.ContentLanguage = c.ContentLanguage
	}
	if c := req.Content; c.ContentDisposition != "" {
		in.ContentDisposition = c.ContentDisposition
	}
	if c := req.Content; c.CacheControl != "" {
		in.CacheControl = c.CacheControl
	}
	if c := req.Content; !c.Expires.IsZero() {
		in.Expires = c.Expires
	}
	if req.StorageClass != "" {
		in.StorageClass = enum.StorageClassType(req.StorageClass)
	}
	if req.ACL != "" {
		in.ACL = enum.ACLType(req.ACL)
	}
	if req.IfMatch != "" {
		in.CopySourceIfMatch = req.IfMatch
	}
	if req.IfNoneMatch != "" {
		in.CopySourceIfNoneMatch = req.IfNoneMatch
	}
	switch strings.ToUpper(req.MetadataDirective) {
	case "COPY":
		in.MetadataDirective = enum.MetadataDirectiveCopy
	case "REPLACE":
		in.MetadataDirective = enum.MetadataDirectiveReplace
	default:
		// TOS default mirrors AWS: COPY when no metadata is supplied,
		// REPLACE when the caller supplies metadata. The unified
		// CopyObjectRequest documents this implicit behaviour.
		if req.Metadata != nil {
			in.MetadataDirective = enum.MetadataDirectiveReplace
		}
	}
	res, err := o.d.client.CopyObject(ctx, in)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, dstBucket, req.DestKey, err)
	}
	out := &uos.CopyObjectResult{
		ETag:      strings.Trim(res.ETag, `"`),
		VersionID: res.VersionID,
	}
	if t, perr := time.Parse(time.RFC3339, res.LastModified); perr == nil {
		out.LastModified = t
	}
	return out, nil
}

// List enumerates objects matching prefix / delimiter via TOS
// ListObjectsV2. NOTE: TOS ListObjectsV2 uses Marker-based pagination
// (NOT S3-style ContinuationToken — that is TOS ListObjectsType2). We
// round-trip the unified ContinuationToken through Marker / NextMarker
// so opaque-cursor pagination works across providers.
func (o objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)
	in := &tos.ListObjectsV2Input{
		Bucket: bucket,
		ListObjectsInput: tos.ListObjectsInput{
			Prefix:    req.Prefix,
			Delimiter: req.Delimiter,
			Marker:    req.ContinuationToken,
			MaxKeys:   req.MaxResults,
		},
	}
	// TOS ListObjectsV2 has no StartAfter; if the caller supplied one
	// AND no explicit ContinuationToken, treat StartAfter as the marker
	// (lexicographically equivalent for the "list keys after" use case).
	if req.StartAfter != "" && in.Marker == "" {
		in.Marker = req.StartAfter
	}
	res, err := o.d.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}
	out := &uos.ObjectList{
		Items:     make([]uos.ObjectInfo, 0, len(res.Contents)),
		NextToken: res.NextMarker,
		Truncated: res.IsTruncated,
	}
	for _, cp := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, cp.Prefix)
	}
	for _, obj := range res.Contents {
		out.Items = append(out.Items, uos.ObjectInfo{
			Bucket:       bucket,
			Key:          obj.Key,
			Size:         obj.Size,
			ETag:         strings.Trim(obj.ETag, `"`),
			LastModified: obj.LastModified,
			StorageClass: string(obj.StorageClass),
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// MultipartService
// ----------------------------------------------------------------------

// multipartService implements uos.MultipartService backed by the TOS
// raw multipart primitives (CreateMultipartUploadV2 / UploadPartV2 /
// CompleteMultipartUploadV2 / AbortMultipartUpload / ListMultipartUploadsV2).
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
// carries the TOS-issued UploadID that all subsequent UploadPart /
// Complete / Abort calls must reference.
func (m multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	in := &tos.CreateMultipartUploadV2Input{
		Bucket: bucket,
		Key:    req.Key,
		Meta:   s3common.LowerMetadataKeys(req.Metadata),
	}
	if c := req.Content; c.ContentType != "" {
		in.ContentType = c.ContentType
	}
	if c := req.Content; c.ContentEncoding != "" {
		in.ContentEncoding = c.ContentEncoding
	}
	if c := req.Content; c.ContentLanguage != "" {
		in.ContentLanguage = c.ContentLanguage
	}
	if c := req.Content; c.ContentDisposition != "" {
		in.ContentDisposition = c.ContentDisposition
	}
	if c := req.Content; c.CacheControl != "" {
		in.CacheControl = c.CacheControl
	}
	if c := req.Content; !c.Expires.IsZero() {
		in.Expires = c.Expires
	}
	if req.StorageClass != "" {
		in.StorageClass = enum.StorageClassType(req.StorageClass)
	}
	if req.ACL != "" {
		in.ACL = enum.ACLType(req.ACL)
	}
	res, err := m.d.client.CreateMultipartUploadV2(ctx, in)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.MultipartUpload{
		UploadID:     res.UploadID,
		Bucket:       bucket,
		Key:          req.Key,
		Initiated:    time.Now().UTC(),
		StorageClass: req.StorageClass,
		Metadata:     req.Metadata,
	}, nil
}

// UploadPart uploads a single part. TOS UploadPartV2 expects the
// caller-supplied size as Content-Length; we forward req.Size verbatim
// and let the wire layer surface InvalidArgument when the body length
// doesn't match.
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
	in := &tos.UploadPartV2Input{
		UploadPartBasicInput: tos.UploadPartBasicInput{
			Bucket:     bucket,
			Key:        req.Key,
			UploadID:   req.UploadID,
			PartNumber: req.PartNumber,
		},
		Content:       req.Body,
		ContentLength: req.Size,
	}
	res, err := m.d.client.UploadPartV2(ctx, in)
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
// parts in PartNumber order. Parts MUST be presented sorted; TOS's
// CompleteMultipartUploadV2 validates the order and rejects mismatches
// with InvalidPartOrder.
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if len(req.Parts) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Parts is required and must be non-empty",
		}
	}
	parts := make([]tos.UploadedPartV2, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, tos.UploadedPartV2{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		})
	}
	in := &tos.CompleteMultipartUploadV2Input{
		Bucket:   bucket,
		Key:      req.Key,
		UploadID: req.UploadID,
		Parts:    parts,
	}
	res, err := m.d.client.CompleteMultipartUploadV2(ctx, in)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(res.ETag, `"`),
		VersionID: res.VersionID,
	}, nil
}

// Abort cancels an in-flight multipart upload. TOS AbortMultipartUpload
// returns 204 on success and NoSuchUpload on a missing UploadID; the
// error mapper translates the latter to ErrNotFound.
func (m multipartService) Abort(ctx context.Context, req uos.AbortMultipartRequest) error {
	const op = "AbortMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	in := &tos.AbortMultipartUploadInput{
		Bucket:   bucket,
		Key:      req.Key,
		UploadID: req.UploadID,
	}
	if _, err := m.d.client.AbortMultipartUpload(ctx, in); err != nil {
		return mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// List enumerates in-flight multipart uploads in the bucket. TOS uses
// KeyMarker for pagination; we expose a single page (the vendor default
// cap is 1000 uploads) and surface NextToken when more pages remain so
// callers can iterate.
func (m multipartService) List(ctx context.Context, req uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	const op = "ListMultipartUploads"
	bucket := m.pickBucket(req.Bucket)
	in := &tos.ListMultipartUploadsV2Input{
		Bucket:     bucket,
		Prefix:     req.Prefix,
		KeyMarker:  req.ContinuationToken,
		MaxUploads: req.MaxResults,
	}
	res, err := m.d.client.ListMultipartUploadsV2(ctx, in)
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
			UploadID:     u.UploadID,
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

// signerService implements uos.Signer for TOS HMAC presigned URLs.
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

// SignURL returns an HTTP-signed URL for the requested operation. TOS's
// PreSignedURL builds the URL synchronously (no I/O); we wrap it in the
// unified SignedURL shape.
func (s signerService) SignURL(ctx context.Context, req uos.SignURLRequest) (*uos.SignedURL, error) {
	const op = "SignURL"
	_ = ctx // PreSignedURL is offline; ctx accepted for API symmetry.
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
	var tosMethod enum.HttpMethodType
	switch method {
	case http.MethodGet:
		tosMethod = enum.HttpMethodGet
	case http.MethodPut:
		tosMethod = enum.HttpMethodPut
	case http.MethodHead:
		tosMethod = enum.HttpMethodHead
	case http.MethodDelete:
		tosMethod = enum.HttpMethodDelete
	case http.MethodPost:
		tosMethod = enum.HttpMethodPost
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q (allowed: GET, PUT, HEAD, DELETE, POST)", method),
		}
	}
	in := &tos.PreSignedURLInput{
		HTTPMethod: tosMethod,
		Bucket:     bucket,
		Key:        req.Key,
		Expires:    int64(req.ExpiresIn / time.Second),
	}
	if in.Expires <= 0 {
		in.Expires = 1
	}
	if len(req.Query) > 0 {
		in.Query = make(map[string]string, len(req.Query))
		for k, v := range req.Query {
			in.Query[k] = v
		}
	}
	if len(req.Headers) > 0 {
		in.Header = make(map[string]string, len(req.Headers))
		for k, vs := range req.Headers {
			if len(vs) == 0 {
				continue
			}
			in.Header[k] = vs[0]
		}
	}
	res, err := s.d.client.PreSignedURL(in)
	if err != nil {
		return nil, mapError(s.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.SignedURL{
		URL:       res.SignedUrl,
		Method:    method,
		ExpiresAt: time.Now().Add(req.ExpiresIn).UTC(),
		Headers:   req.Headers,
	}, nil
}

// IssueDirectGrant always returns ErrUnsupported / CapDirectGrant
// because S3-family providers (including TOS) issue write
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
		Message:    "TOS uses presigned URL — use Signer.SignURL instead",
	}
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// applyContentHeaders stamps a uos.ContentHeaders onto the matching
// fields of a *tos.PutObjectBasicInput. Empty fields leave the wire
// header off so the vendor default is preserved.
func applyContentHeaders(in *tos.PutObjectBasicInput, c uos.ContentHeaders) {
	if c.ContentType != "" {
		in.ContentType = c.ContentType
	}
	if c.ContentEncoding != "" {
		in.ContentEncoding = c.ContentEncoding
	}
	if c.ContentLanguage != "" {
		in.ContentLanguage = c.ContentLanguage
	}
	if c.ContentDisposition != "" {
		in.ContentDisposition = c.ContentDisposition
	}
	if c.CacheControl != "" {
		in.CacheControl = c.CacheControl
	}
	if !c.Expires.IsZero() {
		in.Expires = c.Expires
	}
}

// translateObjectMetaV2 rebuilds a uos.ObjectInfo from a TOS
// ObjectMetaV2 struct. The TOS user-metadata convention is exposed via
// the Meta interface (CustomMeta backing) which the SDK already
// lower-cases at parse time; we walk it via Range and copy into the
// unified Metadata.
func translateObjectMetaV2(bucket, key string, meta *tos.ObjectMetaV2) uos.ObjectInfo {
	info := uos.ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		Size:         meta.ContentLength,
		ETag:         strings.Trim(meta.ETag, `"`),
		LastModified: meta.LastModified,
		StorageClass: string(meta.StorageClass),
		VersionID:    meta.VersionID,
	}
	if info.Size == 0 && meta.ContentLength == 0 {
		// TOS surfaces unset Content-Length as 0; the unified contract
		// uses -1 for "unknown". We can't disambiguate "real zero" from
		// "unset" reliably from headers alone, so trust the vendor's
		// reported value (zero means zero).
		info.Size = 0
	}
	info.Content = uos.ContentHeaders{
		ContentType:        meta.ContentType,
		ContentEncoding:    meta.ContentEncoding,
		ContentLanguage:    meta.ContentLanguage,
		ContentDisposition: meta.ContentDisposition,
		CacheControl:       meta.CacheControl,
		Expires:            meta.Expires,
	}
	if meta.Meta != nil {
		out := uos.Metadata{}
		meta.Meta.Range(func(k, v string) bool {
			out[strings.ToLower(k)] = v
			return true
		})
		if len(out) > 0 {
			info.Metadata = out
		}
	}
	return info
}

// formatRangeHeader renders a uos.ByteRange as the HTTP Range header
// value the TOS SDK's input.Range field expects ("bytes=start-end" or
// "bytes=start-").
func formatRangeHeader(r uos.ByteRange) string {
	if r.End < 0 {
		return fmt.Sprintf("bytes=%d-", r.Start)
	}
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
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
