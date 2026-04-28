package minio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	miniogo "github.com/minio/minio-go/v7"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/capability"
	"github.com/maqian/object-storage-client/pkg/uos/s3common"
)

// driverImpl implements pkg/uos.Client by translating to/from
// minio-go/v7. It holds the high-level *miniogo.Client (used for
// data-plane convenience methods like PutObject / ListObjects /
// PresignedGetObject) and the *miniogo.Core wrapper (used for the raw
// multipart primitives the unified MultipartService exposes).
type driverImpl struct {
	cfg    uos.Config
	client *miniogo.Client
	core   *miniogo.Core
}

// Provider returns "minio". Required by uos.Client.
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

// As exposes the underlying minio-go handles for callers that need
// vendor-specific features. Supported targets:
//
//   - **miniogo.Client:  filled with the high-level client.
//   - **miniogo.Core:    filled with the raw multipart wrapper.
//
// Returns false (without mutating target) for any other type.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **miniogo.Client:
		*t = d.client
		return true
	case **miniogo.Core:
		*t = d.core
		return true
	default:
		return false
	}
}

// Close releases any driver-held resources. minio-go's Client owns no
// goroutines that require shutdown beyond the underlying http.Client
// transport (which httpx.NewClient configures with idle timeouts), so
// this is a no-op kept here to satisfy the uos.Client interface.
func (d *driverImpl) Close() error { return nil }

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService implements uos.BucketService.
type bucketService struct{ d *driverImpl }

// List enumerates buckets visible to the configured credential.
// minio-go's ListBuckets returns the full set in a single call; the
// pagination fields on uos.ListBucketsRequest are honored client-side
// so callers see a consistent surface across providers.
func (b bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"
	infos, err := b.d.client.ListBuckets(ctx)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, "", "", err)
	}
	out := make([]uos.BucketInfo, 0, len(infos))
	for _, bi := range infos {
		out = append(out, uos.BucketInfo{
			Name:      bi.Name,
			Region:    bi.BucketRegion,
			CreatedAt: bi.CreationDate,
		})
	}
	if req.MaxResults > 0 && len(out) > req.MaxResults {
		out = out[:req.MaxResults]
	}
	return out, nil
}

// Create makes a new bucket. minio-go's MakeBucket is idempotent across
// the (region, owner) tuple; when the bucket already exists in a
// different namespace the SDK surfaces BucketAlreadyExists, which the
// error mapper translates to ErrAlreadyExists.
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
	region := req.Region
	if region == "" {
		region = b.d.cfg.Region
	}
	err := b.d.client.MakeBucket(ctx, req.Name, miniogo.MakeBucketOptions{Region: region})
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return &uos.BucketInfo{
		Name:      req.Name,
		Region:    region,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Stat returns BucketInfo for an existing bucket. minio-go's
// BucketExists swallows NoSuchBucket and returns (false, nil); we
// surface that as ErrNotFound so callers can errors.Is on the contract.
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
	exists, err := b.d.client.BucketExists(ctx, req.Name)
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
	region, _ := b.d.client.GetBucketLocation(ctx, req.Name)
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
	err := b.d.client.RemoveBucket(ctx, req.Name)
	if err != nil {
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

// Put writes a single object. Bodies pass through to minio-go's
// PutObject untouched; size==-1 enables minio-go's streaming-multipart
// path (the contract suite does not exercise that case in M2 because
// transfer.Manager owns the unknown-size policy in the unified API).
func (o objectService) Put(ctx context.Context, req uos.PutObjectRequest) (*uos.PutObjectResult, error) {
	const op = "PutObject"
	bucket := o.pickBucket(req.Bucket)
	if req.Body == nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Body is required",
		}
	}
	opts := miniogo.PutObjectOptions{
		ContentType:        req.Content.ContentType,
		ContentEncoding:    req.Content.ContentEncoding,
		ContentDisposition: req.Content.ContentDisposition,
		ContentLanguage:    req.Content.ContentLanguage,
		CacheControl:       req.Content.CacheControl,
		Expires:            req.Content.Expires,
		StorageClass:       req.StorageClass,
		UserMetadata:       toLowerMap(req.Metadata),
	}
	if req.IfMatch != "" {
		opts.SetMatchETag(req.IfMatch)
	}
	if req.IfNoneMatch != "" {
		opts.SetMatchETagExcept(req.IfNoneMatch)
	}
	info, err := o.d.client.PutObject(ctx, bucket, req.Key, req.Body, req.Size, opts)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      info.ETag,
		VersionID: info.VersionID,
	}, nil
}

// Get streams an object via miniogo.Core.GetObject (the low-level
// API), which honors Range/conditional headers verbatim. The high-level
// miniogo.Client.GetObject returns a stateful *Object that re-derives
// Range internally per Read call — that's the wrong shape for the
// unified ObjectReader contract, which hands the body straight to the
// caller and expects the wire-level Range to apply.
//
// An open-ended End=-1 produces a "from Start to EOF" range request via
// minio-go's SetRange(start, 0) form. The body returned here is a raw
// io.ReadCloser; callers MUST Close it.
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)
	opts := miniogo.GetObjectOptions{VersionID: req.VersionID}
	if req.Range != nil {
		// minio-go's SetRange expects a closed [start, end] interval;
		// translate End=-1 ("to EOF") into a from-offset open range.
		var rangeErr error
		switch {
		case req.Range.End < 0:
			rangeErr = opts.SetRange(req.Range.Start, 0)
		default:
			rangeErr = opts.SetRange(req.Range.Start, req.Range.End)
		}
		if rangeErr != nil {
			return nil, &uos.Error{
				Code: uos.ErrInvalidArgument, Provider: o.d.Provider(), Operation: op,
				Bucket: bucket, Key: req.Key, Message: rangeErr.Error(), Cause: rangeErr,
			}
		}
	}
	if !req.IfModifiedSince.IsZero() {
		_ = opts.SetModified(req.IfModifiedSince)
	}
	if !req.IfUnmodifiedSince.IsZero() {
		_ = opts.SetUnmodified(req.IfUnmodifiedSince)
	}
	if req.IfMatch != "" {
		_ = opts.SetMatchETag(req.IfMatch)
	}
	if req.IfNoneMatch != "" {
		_ = opts.SetMatchETagExcept(req.IfNoneMatch)
	}
	body, stat, _, err := o.d.core.GetObject(ctx, bucket, req.Key, opts)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectInfo(bucket, req.Key, stat)
	// For a ranged read Stat.Size is the slice length, which is what the
	// unified ContentLength field documents.
	return &uos.ObjectReader{
		Body:          body,
		ContentLength: stat.Size,
		Info:          info,
	}, nil
}

// Head returns ObjectInfo without the body. minio-go's StatObject
// surfaces NoSuchKey on missing objects, which the error mapper
// translates to ErrNotFound.
func (o objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)
	stat, err := o.d.client.StatObject(ctx, bucket, req.Key, miniogo.StatObjectOptions{VersionID: req.VersionID})
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectInfo(bucket, req.Key, stat)
	return &info, nil
}

// Delete removes a single object. S3 semantics make this idempotent:
// removing a missing key returns 204 No Content, not 404.
func (o objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)
	err := o.d.client.RemoveObject(ctx, bucket, req.Key, miniogo.RemoveObjectOptions{VersionID: req.VersionID})
	if err != nil {
		return mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// Exists reports whether an object exists. Per the contract suite, the
// not-found case returns (false, nil); other errors propagate.
func (o objectService) Exists(ctx context.Context, req uos.HeadObjectRequest) (bool, error) {
	const op = "ExistsObject"
	bucket := o.pickBucket(req.Bucket)
	_, err := o.d.client.StatObject(ctx, bucket, req.Key, miniogo.StatObjectOptions{VersionID: req.VersionID})
	if err == nil {
		return true, nil
	}
	mapped := mapError(o.d.Provider(), op, bucket, req.Key, err)
	var ue *uos.Error
	if errors.As(mapped, &ue) && ue.Code == uos.ErrNotFound {
		return false, nil
	}
	return false, mapped
}

// DeleteMany removes a batch of keys. minio-go drives RemoveObjects via
// channels; we feed the channel from the request slice and accumulate
// per-key results into the unified DeleteManyResult shape.
func (o objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	in := make(chan miniogo.ObjectInfo, len(req.Keys))
	for _, k := range req.Keys {
		in <- miniogo.ObjectInfo{Key: k}
	}
	close(in)
	out := o.d.client.RemoveObjectsWithResult(ctx, bucket, in, miniogo.RemoveObjectsOptions{})
	res := &uos.DeleteManyResult{
		Deleted: make([]string, 0, len(req.Keys)),
	}
	for r := range out {
		if r.Err == nil {
			if !req.Quiet {
				res.Deleted = append(res.Deleted, r.ObjectName)
			}
			continue
		}
		mapped := mapError(o.d.Provider(), op, bucket, r.ObjectName, r.Err)
		var ue *uos.Error
		var code uos.Code
		var msg string
		if errors.As(mapped, &ue) {
			code = ue.Code
			msg = ue.Message
		} else {
			code = uos.ErrInternal
			msg = r.Err.Error()
		}
		res.Failed = append(res.Failed, uos.DeleteFailure{
			Key:     r.ObjectName,
			Code:    code,
			Message: msg,
		})
	}
	return res, nil
}

// Copy duplicates an object. S3-family providers support same-account
// cross-bucket copy via x-amz-copy-source, exposed by minio-go through
// the CopyObject(dst, src) signature.
func (o objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	src := miniogo.CopySrcOptions{
		Bucket:      req.SourceBucket,
		Object:      req.SourceKey,
		VersionID:   req.SourceVersionID,
		MatchETag:   req.IfMatch,
		NoMatchETag: req.IfNoneMatch,
	}
	dst := miniogo.CopyDestOptions{
		Bucket:             req.DestBucket,
		Object:             req.DestKey,
		ContentType:        req.Content.ContentType,
		ContentEncoding:    req.Content.ContentEncoding,
		ContentDisposition: req.Content.ContentDisposition,
		ContentLanguage:    req.Content.ContentLanguage,
		CacheControl:       req.Content.CacheControl,
		Expires:            req.Content.Expires,
	}
	if strings.EqualFold(req.MetadataDirective, "REPLACE") || req.Metadata != nil {
		dst.ReplaceMetadata = true
		dst.UserMetadata = toLowerMap(req.Metadata)
	}
	info, err := o.d.client.CopyObject(ctx, dst, src)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, req.DestBucket, req.DestKey, err)
	}
	return &uos.CopyObjectResult{
		ETag:         info.ETag,
		VersionID:    info.VersionID,
		LastModified: info.LastModified,
	}, nil
}

// List enumerates objects matching prefix / delimiter. minio-go returns
// the page as a streaming channel; we drain a single page (bounded by
// MaxResults) and emit a NextToken so callers can resume.
//
// Pagination model: minio-go's ListObjects with Recursive=true (no
// delimiter) returns the full namespace in one channel. To honor
// MaxResults we stop draining after that many items and synthesise a
// NextToken from the last key's name (used as StartAfter on the next
// call). This is identical to how AWS recommends client-side pagination
// when the wire-level continuation token isn't surfaced by the SDK.
func (o objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)
	opts := miniogo.ListObjectsOptions{
		Prefix:     req.Prefix,
		Recursive:  req.Delimiter == "",
		MaxKeys:    req.MaxResults,
		StartAfter: pickListMarker(req),
	}
	out := &uos.ObjectList{}
	limit := req.MaxResults
	last := ""
	prefixes := make(map[string]struct{})
	for obj := range o.d.client.ListObjects(ctx, bucket, opts) {
		if obj.Err != nil {
			return nil, mapError(o.d.Provider(), op, bucket, "", obj.Err)
		}
		// minio-go folds CommonPrefixes into the iterator as ObjectInfo
		// entries whose Key ends with the delimiter and whose Size==0.
		// Extract them when the caller asked for hierarchical listing.
		if req.Delimiter != "" && strings.HasSuffix(obj.Key, req.Delimiter) && obj.ETag == "" {
			if _, dup := prefixes[obj.Key]; !dup {
				prefixes[obj.Key] = struct{}{}
				out.CommonPrefixes = append(out.CommonPrefixes, obj.Key)
			}
			continue
		}
		out.Items = append(out.Items, translateObjectInfo(bucket, obj.Key, obj))
		last = obj.Key
		if limit > 0 && len(out.Items) >= limit {
			out.Truncated = true
			out.NextToken = last
			return out, nil
		}
	}
	return out, nil
}

// pickListMarker returns the cursor minio-go's ListObjects should
// resume from. Both ContinuationToken (cross-provider unified field)
// and StartAfter (S3 native) are accepted; ContinuationToken wins when
// both are set because that is the field the unified surface
// documents as the round-trip cursor.
func pickListMarker(req uos.ListObjectsRequest) string {
	if req.ContinuationToken != "" {
		return req.ContinuationToken
	}
	return req.StartAfter
}

// ----------------------------------------------------------------------
// MultipartService
// ----------------------------------------------------------------------

// multipartService implements uos.MultipartService backed by
// miniogo.Core (the raw multipart wrapper).
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
// carries the vendor-issued UploadID that all subsequent UploadPart /
// Complete / Abort calls must reference.
func (m multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	opts := miniogo.PutObjectOptions{
		ContentType:        req.Content.ContentType,
		ContentEncoding:    req.Content.ContentEncoding,
		ContentDisposition: req.Content.ContentDisposition,
		ContentLanguage:    req.Content.ContentLanguage,
		CacheControl:       req.Content.CacheControl,
		Expires:            req.Content.Expires,
		StorageClass:       req.StorageClass,
		UserMetadata:       toLowerMap(req.Metadata),
	}
	uploadID, err := m.d.core.NewMultipartUpload(ctx, bucket, req.Key, opts)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.MultipartUpload{
		UploadID:     uploadID,
		Bucket:       bucket,
		Key:          req.Key,
		Initiated:    time.Now().UTC(),
		StorageClass: req.StorageClass,
		Metadata:     req.Metadata,
	}, nil
}

// UploadPart uploads a single part. Drivers verify the part length
// matches req.Size on the wire (S3 returns InvalidArgument otherwise);
// we don't pre-validate here so the vendor's own bounds (5 MiB minimum
// for non-final parts) stay authoritative.
func (m multipartService) UploadPart(ctx context.Context, req uos.UploadPartRequest) (*uos.UploadedPart, error) {
	const op = "UploadPart"
	bucket := m.pickBucket(req.Bucket)
	if req.Body == nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Body is required",
		}
	}
	part, err := m.d.core.PutObjectPart(ctx, bucket, req.Key, req.UploadID, req.PartNumber,
		req.Body, req.Size, miniogo.PutObjectPartOptions{})
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.UploadedPart{
		PartNumber: part.PartNumber,
		ETag:       part.ETag,
		Size:       part.Size,
	}, nil
}

// Complete finalises a multipart upload by stitching the supplied parts
// in PartNumber order. Parts MUST be presented sorted; the contract
// suite documents this and we pass them straight through to minio-go.
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if len(req.Parts) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Parts is required and must be non-empty",
		}
	}
	parts := make([]miniogo.CompletePart, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, miniogo.CompletePart{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		})
	}
	info, err := m.d.core.CompleteMultipartUpload(ctx, bucket, req.Key, req.UploadID, parts, miniogo.PutObjectOptions{})
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      info.ETag,
		VersionID: info.VersionID,
	}, nil
}

// Abort cancels an in-flight multipart upload. S3 makes Abort
// idempotent; aborting an unknown upload yields NoSuchUpload, which
// the error mapper translates to ErrNotFound. Most callers don't care
// (they are aborting on a failure path), but we surface the error
// rather than swallowing it so caller observability stays accurate.
func (m multipartService) Abort(ctx context.Context, req uos.AbortMultipartRequest) error {
	const op = "AbortMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if err := m.d.core.AbortMultipartUpload(ctx, bucket, req.Key, req.UploadID); err != nil {
		return mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// List enumerates in-flight multipart uploads in the bucket. Pagination
// is handled by minio-go internally; we expose a single page (the
// vendor default cap is 1000 uploads) and surface NextToken when more
// pages remain so callers can iterate.
func (m multipartService) List(ctx context.Context, req uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	const op = "ListMultipartUploads"
	bucket := m.pickBucket(req.Bucket)
	maxUploads := req.MaxResults
	if maxUploads <= 0 {
		maxUploads = 1000
	}
	res, err := m.d.core.ListMultipartUploads(ctx, bucket, req.Prefix, req.ContinuationToken, "", "", maxUploads)
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
			StorageClass: u.StorageClass,
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// Signer
// ----------------------------------------------------------------------

// signerService implements uos.Signer for S3 v4 presigned URLs.
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

// SignURL returns an HTTP-signed URL for the requested operation.
// minio-go ships separate Presigned*Object helpers per method; we
// dispatch on req.Method and pass extra query parameters through the
// reqParams URL values so caller-supplied response-content-disposition
// (etc.) participates in the signature.
//
// Headers in the request are bound into the signature via
// PresignHeader so the caller MUST attach the same headers when issuing
// the request — the contract suite asserts this round-trip.
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

	reqParams := make(url.Values, len(req.Query))
	for k, v := range req.Query {
		reqParams.Set(k, v)
	}
	if req.VersionID != "" {
		reqParams.Set("versionId", req.VersionID)
	}

	var (
		signed *url.URL
		err    error
	)
	switch method {
	case http.MethodGet:
		if len(req.Headers) == 0 {
			signed, err = s.d.client.PresignedGetObject(ctx, bucket, req.Key, req.ExpiresIn, reqParams)
		} else {
			signed, err = s.d.client.PresignHeader(ctx, method, bucket, req.Key, req.ExpiresIn, reqParams, req.Headers)
		}
	case http.MethodHead:
		if len(req.Headers) == 0 {
			signed, err = s.d.client.PresignedHeadObject(ctx, bucket, req.Key, req.ExpiresIn, reqParams)
		} else {
			signed, err = s.d.client.PresignHeader(ctx, method, bucket, req.Key, req.ExpiresIn, reqParams, req.Headers)
		}
	case http.MethodPut:
		if len(req.Headers) == 0 {
			signed, err = s.d.client.PresignedPutObject(ctx, bucket, req.Key, req.ExpiresIn)
		} else {
			signed, err = s.d.client.PresignHeader(ctx, method, bucket, req.Key, req.ExpiresIn, reqParams, req.Headers)
		}
	case http.MethodDelete:
		signed, err = s.d.client.Presign(ctx, method, bucket, req.Key, req.ExpiresIn, reqParams)
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q", method),
		}
	}
	if err != nil {
		return nil, mapError(s.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.SignedURL{
		URL:       signed.String(),
		Method:    method,
		ExpiresAt: time.Now().Add(req.ExpiresIn).UTC(),
		Headers:   req.Headers,
	}, nil
}

// IssueDirectGrant always returns ErrUnsupported / CapDirectGrant
// because S3-family providers issue write authorisations as URLs (use
// SignURL with PUT). See docs/provider_matrix.md footnote 5.
func (s signerService) IssueDirectGrant(_ context.Context, _ uos.DirectGrantRequest) (*uos.DirectGrant, error) {
	return nil, uos.NewUnsupported(s.d.Provider(), "IssueDirectGrant", capability.CapDirectGrant, nil)
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// translateObjectInfo converts a minio-go ObjectInfo into the unified
// uos.ObjectInfo, normalising the user-metadata key case and copying
// the standard content headers across.
func translateObjectInfo(bucket, key string, in miniogo.ObjectInfo) uos.ObjectInfo {
	out := uos.ObjectInfo{
		Bucket:       bucket,
		Key:          orDefault(in.Key, key),
		Size:         in.Size,
		ETag:         in.ETag,
		LastModified: in.LastModified,
		StorageClass: in.StorageClass,
		VersionID:    in.VersionID,
		Content: uos.ContentHeaders{
			ContentType:     in.ContentType,
			ContentEncoding: in.ContentEncoding,
			Expires:         in.Expires,
		},
		Metadata: extractUserMetadata(in.Metadata, in.UserMetadata),
	}
	if in.Metadata != nil {
		if disp := in.Metadata.Get("Content-Disposition"); disp != "" {
			out.Content.ContentDisposition = disp
		}
		if lang := in.Metadata.Get("Content-Language"); lang != "" {
			out.Content.ContentLanguage = lang
		}
		if cc := in.Metadata.Get("Cache-Control"); cc != "" {
			out.Content.CacheControl = cc
		}
	}
	return out
}

// extractUserMetadata picks the lower-cased x-amz-meta-* values out of
// the response headers. UserMetadata (only set by MinIO servers) is
// preferred when present; otherwise we walk the canonical Header map.
func extractUserMetadata(h http.Header, server miniogo.StringMap) uos.Metadata {
	out := uos.Metadata{}
	for k, v := range server {
		out[strings.ToLower(k)] = v
	}
	const prefix = "X-Amz-Meta-"
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

// toLowerMap returns a copy of m with keys lower-cased. Returns nil
// for nil/empty input so minio-go's options keep their "no metadata"
// behaviour. Thin adapter over s3common.LowerMetadataKeys (kept as a
// named helper so existing call sites do not need updating).
func toLowerMap(m uos.Metadata) map[string]string {
	return s3common.LowerMetadataKeys(m)
}

// orDefault returns s when non-empty, fallback otherwise. Used to fill
// in driver-known fields when minio-go didn't echo them back.
func orDefault(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// Compile-time assertion that driverImpl satisfies the full uos.Client.
// Drift in pkg/uos.Client surfaces as a build failure here, where the
// fix is unambiguous, rather than as a confusing factory.go error.
var (
	_ uos.Client           = (*driverImpl)(nil)
	_ uos.BucketService    = (*bucketService)(nil)
	_ uos.ObjectService    = (*objectService)(nil)
	_ uos.MultipartService = (*multipartService)(nil)
	_ uos.Signer           = (*signerService)(nil)
)
