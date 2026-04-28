package aws

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/capability"
)

// driverImpl is the concrete uos.Client for AWS S3. It holds a
// pre-configured *s3.Client (with retry disabled, optional endpoint
// override, optional path-style addressing) plus a presigner. The
// uos.Config is captured for Provider() and Capabilities() reporting.
//
// driverImpl is safe for concurrent use; *s3.Client and *s3.PresignClient
// are themselves goroutine-safe.
type driverImpl struct {
	cfg       uos.Config
	s3        *s3.Client
	presigner *s3.PresignClient
}

// Provider returns the canonical provider id ("aws").
func (d *driverImpl) Provider() uos.Provider { return providerID }

// Capabilities returns the static capability.Report for this driver.
// The report is computed at request time but never depends on the
// runtime state — drivers that need bucket-level probing should
// compose this with a per-bucket runtime probe at the call site.
func (d *driverImpl) Capabilities(_ context.Context) (capability.Report, error) {
	return capabilities(), nil
}

// Buckets returns the BucketService view bound to this driver.
func (d *driverImpl) Buckets() uos.BucketService { return &bucketService{d: d} }

// Objects returns the ObjectService view bound to bucket. The bucket
// name is captured for diagnostic threading; per-request bucket fields
// in the request structs are still authoritative.
func (d *driverImpl) Objects(bucket string) uos.ObjectService {
	return &objectService{d: d, bucket: bucket}
}

// Multipart returns the MultipartService view bound to bucket.
func (d *driverImpl) Multipart(bucket string) uos.MultipartService {
	return &multipartService{d: d, bucket: bucket}
}

// Signer returns the Signer view bound to bucket.
func (d *driverImpl) Signer(bucket string) uos.Signer {
	return &signer{d: d, bucket: bucket}
}

// As populates target when target is a non-nil pointer to one of the
// vendor-specific concrete types this driver exposes:
//
//   - **s3.Client → the underlying SDK client
//   - **s3.PresignClient → the underlying presign client
//
// Returns true iff target was populated.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **s3.Client:
		*t = d.s3
		return true
	case **s3.PresignClient:
		*t = d.presigner
		return true
	}
	return false
}

// Close releases any resources held by the driver. The aws-sdk-go-v2
// *s3.Client has no Close hook; the underlying *http.Client is shared
// by reference and is the caller's responsibility. Close is therefore
// a no-op but remains idempotent for interface compliance.
func (d *driverImpl) Close() error { return nil }

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService is the AWS-flavoured BucketService view.
type bucketService struct {
	d *driverImpl
}

// List enumerates buckets visible to the configured credential. AWS
// ListBuckets does not paginate the v0.1 contract surface aggressively
// (1000 buckets per page is the SDK default); the response NextToken is
// the SDK's NextContinuationToken.
func (b *bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	out, err := b.d.s3.ListBuckets(ctx, &s3.ListBucketsInput{
		MaxBuckets:        ptr32(int32(req.MaxResults)),
		ContinuationToken: ptrIfNotEmpty(req.ContinuationToken),
	})
	if err != nil {
		return nil, mapError("ListBuckets", "", "", err)
	}
	items := make([]uos.BucketInfo, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		bi := uos.BucketInfo{
			Name: deref(b.Name),
		}
		if b.CreationDate != nil {
			bi.CreatedAt = *b.CreationDate
		}
		if b.BucketRegion != nil {
			bi.Region = *b.BucketRegion
		}
		items = append(items, bi)
	}
	return items, nil
}

// Create provisions a new bucket. AWS S3 buckets in us-east-1 must
// omit the LocationConstraint field; other regions require it. The
// driver derives the constraint from CreateBucketRequest.Region (when
// set) or uos.Config.Region.
func (b *bucketService) Create(ctx context.Context, req uos.CreateBucketRequest) (*uos.BucketInfo, error) {
	in := &s3.CreateBucketInput{
		Bucket: ptrIfNotEmpty(req.Name),
	}
	region := req.Region
	if region == "" {
		region = b.d.cfg.Region
	}
	if region != "" && region != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(region),
		}
	}
	if req.ACL != "" {
		in.ACL = types.BucketCannedACL(req.ACL)
	}
	_, err := b.d.s3.CreateBucket(ctx, in)
	if err != nil {
		return nil, mapError("CreateBucket", req.Name, "", err)
	}
	return &uos.BucketInfo{Name: req.Name, Region: region}, nil
}

// Stat returns the BucketInfo for an existing bucket via HeadBucket.
// HeadBucket carries no body, so Region/CreatedAt are populated from
// response headers + the configured region as fallback.
func (b *bucketService) Stat(ctx context.Context, req uos.StatBucketRequest) (*uos.BucketInfo, error) {
	out, err := b.d.s3.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: ptrIfNotEmpty(req.Name),
	})
	if err != nil {
		return nil, mapError("HeadBucket", req.Name, "", err)
	}
	info := &uos.BucketInfo{Name: req.Name}
	if out.BucketRegion != nil {
		info.Region = *out.BucketRegion
	} else {
		info.Region = b.d.cfg.Region
	}
	return info, nil
}

// Delete removes an empty bucket. Non-empty buckets surface as
// ErrConflict per the BucketNotEmpty mapping in error_map.go.
func (b *bucketService) Delete(ctx context.Context, req uos.DeleteBucketRequest) error {
	_, err := b.d.s3.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: ptrIfNotEmpty(req.Name),
	})
	if err != nil {
		return mapError("DeleteBucket", req.Name, "", err)
	}
	return nil
}

// ----------------------------------------------------------------------
// ObjectService
// ----------------------------------------------------------------------

// objectService is the AWS-flavoured ObjectService view bound to a bucket.
type objectService struct {
	d      *driverImpl
	bucket string
}

// Put writes an object via S3 PutObject. The body is streamed; AWS SDK
// requires a known Content-Length when the body is not a *bytes.Reader,
// so callers passing -1 should rely on transfer.Manager (currently
// bypassed in v0.1; the driver will return an error for streaming
// unknown-size bodies).
func (o *objectService) Put(ctx context.Context, req uos.PutObjectRequest) (*uos.PutObjectResult, error) {
	if req.Body == nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "PutObject",
			Bucket:    req.Bucket,
			Key:       req.Key,
			Message:   "Body is required",
		}
	}
	if req.Size < 0 {
		return nil, &uos.Error{
			Code:      uos.ErrLengthRequired,
			Provider:  providerID,
			Operation: "PutObject",
			Bucket:    req.Bucket,
			Key:       req.Key,
			Message:   "Size is required (-1 unsupported by AWS PutObject; use multipart for streaming)",
		}
	}
	in := &s3.PutObjectInput{
		Bucket:        ptrIfNotEmpty(req.Bucket),
		Key:           ptrIfNotEmpty(req.Key),
		Body:          req.Body,
		ContentLength: ptr64(req.Size),
		Metadata:      metadataToAWS(req.Metadata),
	}
	applyContent(&in.ContentType, &in.ContentEncoding, &in.ContentLanguage,
		&in.ContentDisposition, &in.CacheControl, &in.Expires, req.Content)
	if req.StorageClass != "" {
		in.StorageClass = types.StorageClass(req.StorageClass)
	}
	if req.ACL != "" {
		in.ACL = types.ObjectCannedACL(req.ACL)
	}
	if req.IfMatch != "" {
		in.IfMatch = ptrIfNotEmpty(req.IfMatch)
	}
	if req.IfNoneMatch != "" {
		in.IfNoneMatch = ptrIfNotEmpty(req.IfNoneMatch)
	}
	out, err := o.d.s3.PutObject(ctx, in)
	if err != nil {
		return nil, mapError("PutObject", req.Bucket, req.Key, err)
	}
	res := &uos.PutObjectResult{
		ETag:      deref(out.ETag),
		VersionID: deref(out.VersionId),
	}
	if out.ChecksumCRC32C != nil {
		res.Checksum = uos.Checksum{Type: "crc32c", Value: []byte(*out.ChecksumCRC32C)}
	}
	return res, nil
}

// Get streams the object body via S3 GetObject. Range requests use the
// HTTP Range header (bytes=start-end). The returned ObjectReader.Body
// must be closed by the caller.
func (o *objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	in := &s3.GetObjectInput{
		Bucket:    ptrIfNotEmpty(req.Bucket),
		Key:       ptrIfNotEmpty(req.Key),
		VersionId: ptrIfNotEmpty(req.VersionID),
	}
	if req.Range != nil {
		in.Range = ptrIfNotEmpty(formatRange(*req.Range))
	}
	if req.IfMatch != "" {
		in.IfMatch = ptrIfNotEmpty(req.IfMatch)
	}
	if req.IfNoneMatch != "" {
		in.IfNoneMatch = ptrIfNotEmpty(req.IfNoneMatch)
	}
	if !req.IfModifiedSince.IsZero() {
		t := req.IfModifiedSince
		in.IfModifiedSince = &t
	}
	if !req.IfUnmodifiedSince.IsZero() {
		t := req.IfUnmodifiedSince
		in.IfUnmodifiedSince = &t
	}
	out, err := o.d.s3.GetObject(ctx, in)
	if err != nil {
		return nil, mapError("GetObject", req.Bucket, req.Key, err)
	}
	info := uos.ObjectInfo{
		Bucket:   req.Bucket,
		Key:      req.Key,
		ETag:     deref(out.ETag),
		Metadata: metadataFromAWS(out.Metadata),
	}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	} else {
		info.Size = -1
	}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	if out.VersionId != nil {
		info.VersionID = *out.VersionId
	}
	info.StorageClass = string(out.StorageClass)
	info.Content = readContentHeaders(out.ContentType, out.ContentEncoding,
		out.ContentLanguage, out.ContentDisposition, out.CacheControl, out.Expires)
	length := int64(-1)
	if out.ContentLength != nil {
		length = *out.ContentLength
	}
	return &uos.ObjectReader{
		Body:          out.Body,
		ContentLength: length,
		Info:          info,
	}, nil
}

// Head fetches object metadata without the body via S3 HeadObject.
// Missing objects surface as ErrNotFound per error_map.go.
func (o *objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	in := &s3.HeadObjectInput{
		Bucket:    ptrIfNotEmpty(req.Bucket),
		Key:       ptrIfNotEmpty(req.Key),
		VersionId: ptrIfNotEmpty(req.VersionID),
	}
	out, err := o.d.s3.HeadObject(ctx, in)
	if err != nil {
		return nil, mapError("HeadObject", req.Bucket, req.Key, err)
	}
	info := &uos.ObjectInfo{
		Bucket:   req.Bucket,
		Key:      req.Key,
		ETag:     deref(out.ETag),
		Metadata: metadataFromAWS(out.Metadata),
	}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	} else {
		info.Size = -1
	}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	if out.VersionId != nil {
		info.VersionID = *out.VersionId
	}
	info.StorageClass = string(out.StorageClass)
	info.Content = readContentHeaders(out.ContentType, out.ContentEncoding,
		out.ContentLanguage, out.ContentDisposition, out.CacheControl, out.Expires)
	return info, nil
}

// Delete removes a single object. AWS DeleteObject is idempotent: a
// missing key returns 204 No Content and is reported as nil error per
// the request.go contract.
func (o *objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	_, err := o.d.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:    ptrIfNotEmpty(req.Bucket),
		Key:       ptrIfNotEmpty(req.Key),
		VersionId: ptrIfNotEmpty(req.VersionID),
	})
	if err != nil {
		return mapError("DeleteObject", req.Bucket, req.Key, err)
	}
	return nil
}

// Exists reports whether an object exists. Missing objects return
// (false, nil) per the request.go contract; other errors propagate.
func (o *objectService) Exists(ctx context.Context, req uos.HeadObjectRequest) (bool, error) {
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

// awsDeleteBatchLimit is the maximum keys per S3 DeleteObjects call.
const awsDeleteBatchLimit = 1000

// DeleteMany removes a batch of objects. S3 DeleteObjects caps each
// call at 1000 keys; the driver chunks larger batches automatically and
// aggregates per-key Deleted/Failed lists into a single
// DeleteManyResult. A non-nil error indicates a transport-level failure
// affecting the entire chunk; partial-success per-key failures are
// reported in DeleteManyResult.Failed.
func (o *objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	result := &uos.DeleteManyResult{}
	for start := 0; start < len(req.Keys); start += awsDeleteBatchLimit {
		end := start + awsDeleteBatchLimit
		if end > len(req.Keys) {
			end = len(req.Keys)
		}
		chunk := req.Keys[start:end]
		objs := make([]types.ObjectIdentifier, 0, len(chunk))
		for _, k := range chunk {
			k := k
			objs = append(objs, types.ObjectIdentifier{Key: &k})
		}
		out, err := o.d.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: ptrIfNotEmpty(req.Bucket),
			Delete: &types.Delete{
				Objects: objs,
				Quiet:   ptrBool(req.Quiet),
			},
			// CRC32 satisfies S3-compat targets (MinIO etc.) that still
			// require a Content-MD5 / x-amz-checksum-algorithm header on
			// bulk DeleteObjects. AWS itself accepts CRC32 too.
			ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
		}, withContentMD5())
		if err != nil {
			return result, mapError("DeleteObjects", req.Bucket, "", err)
		}
		for _, d := range out.Deleted {
			result.Deleted = append(result.Deleted, deref(d.Key))
		}
		for _, f := range out.Errors {
			result.Failed = append(result.Failed, uos.DeleteFailure{
				Key:     deref(f.Key),
				Code:    deleteFailureCode(deref(f.Code)),
				Message: deref(f.Message),
			})
		}
	}
	return result, nil
}

// Copy duplicates an object via S3 CopyObject. AWS requires the
// CopySource header in the form "bucket/url-encoded-key" — the SDK
// handles encoding when given the SourceBucket / SourceKey via the
// CopySource string assembled here.
func (o *objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	src := req.SourceBucket + "/" + req.SourceKey
	if req.SourceVersionID != "" {
		src += "?versionId=" + req.SourceVersionID
	}
	in := &s3.CopyObjectInput{
		Bucket:     ptrIfNotEmpty(req.DestBucket),
		Key:        ptrIfNotEmpty(req.DestKey),
		CopySource: ptrIfNotEmpty(src),
		Metadata:   metadataToAWS(req.Metadata),
	}
	if req.MetadataDirective != "" {
		in.MetadataDirective = types.MetadataDirective(req.MetadataDirective)
	}
	if req.StorageClass != "" {
		in.StorageClass = types.StorageClass(req.StorageClass)
	}
	if req.ACL != "" {
		in.ACL = types.ObjectCannedACL(req.ACL)
	}
	if req.IfMatch != "" {
		in.CopySourceIfMatch = ptrIfNotEmpty(req.IfMatch)
	}
	if req.IfNoneMatch != "" {
		in.CopySourceIfNoneMatch = ptrIfNotEmpty(req.IfNoneMatch)
	}
	applyContent(&in.ContentType, &in.ContentEncoding, &in.ContentLanguage,
		&in.ContentDisposition, &in.CacheControl, &in.Expires, req.Content)
	out, err := o.d.s3.CopyObject(ctx, in)
	if err != nil {
		return nil, mapError("CopyObject", req.DestBucket, req.DestKey, err)
	}
	res := &uos.CopyObjectResult{}
	if out.CopyObjectResult != nil {
		res.ETag = deref(out.CopyObjectResult.ETag)
		if out.CopyObjectResult.LastModified != nil {
			res.LastModified = *out.CopyObjectResult.LastModified
		}
	}
	if out.VersionId != nil {
		res.VersionID = *out.VersionId
	}
	return res, nil
}

// List enumerates objects via ListObjectsV2 with the requested prefix /
// delimiter / pagination. NextToken is the opaque
// NextContinuationToken; Truncated mirrors IsTruncated.
func (o *objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	in := &s3.ListObjectsV2Input{
		Bucket:            ptrIfNotEmpty(req.Bucket),
		Prefix:            ptrIfNotEmpty(req.Prefix),
		Delimiter:         ptrIfNotEmpty(req.Delimiter),
		MaxKeys:           ptr32(int32(req.MaxResults)),
		ContinuationToken: ptrIfNotEmpty(req.ContinuationToken),
		StartAfter:        ptrIfNotEmpty(req.StartAfter),
	}
	out, err := o.d.s3.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, mapError("ListObjectsV2", req.Bucket, "", err)
	}
	res := &uos.ObjectList{}
	for _, item := range out.Contents {
		oi := uos.ObjectInfo{
			Bucket:       req.Bucket,
			Key:          deref(item.Key),
			ETag:         deref(item.ETag),
			StorageClass: string(item.StorageClass),
		}
		if item.Size != nil {
			oi.Size = *item.Size
		} else {
			oi.Size = -1
		}
		if item.LastModified != nil {
			oi.LastModified = *item.LastModified
		}
		res.Items = append(res.Items, oi)
	}
	for _, cp := range out.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, deref(cp.Prefix))
	}
	if out.NextContinuationToken != nil {
		res.NextToken = *out.NextContinuationToken
	}
	if out.IsTruncated != nil {
		res.Truncated = *out.IsTruncated
	}
	return res, nil
}

// ----------------------------------------------------------------------
// MultipartService
// ----------------------------------------------------------------------

// multipartService is the AWS-flavoured MultipartService view bound to
// a bucket. The driver uses raw S3 multipart primitives directly —
// pkg/uos/transfer.Manager is BYPASSED in v0.1 (see ADR Follow-up #1).
type multipartService struct {
	d      *driverImpl
	bucket string
}

// Initiate starts a multipart upload via CreateMultipartUpload.
func (m *multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	in := &s3.CreateMultipartUploadInput{
		Bucket:   ptrIfNotEmpty(req.Bucket),
		Key:      ptrIfNotEmpty(req.Key),
		Metadata: metadataToAWS(req.Metadata),
	}
	applyContent(&in.ContentType, &in.ContentEncoding, &in.ContentLanguage,
		&in.ContentDisposition, &in.CacheControl, &in.Expires, req.Content)
	if req.StorageClass != "" {
		in.StorageClass = types.StorageClass(req.StorageClass)
	}
	if req.ACL != "" {
		in.ACL = types.ObjectCannedACL(req.ACL)
	}
	out, err := m.d.s3.CreateMultipartUpload(ctx, in)
	if err != nil {
		return nil, mapError("CreateMultipartUpload", req.Bucket, req.Key, err)
	}
	return &uos.MultipartUpload{
		UploadID:  deref(out.UploadId),
		Bucket:    req.Bucket,
		Key:       req.Key,
		Initiated: time.Now().UTC(),
		Metadata:  req.Metadata,
	}, nil
}

// UploadPart uploads a single part via S3 UploadPart.
func (m *multipartService) UploadPart(ctx context.Context, req uos.UploadPartRequest) (*uos.UploadedPart, error) {
	if req.Body == nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: providerID,
			Operation: "UploadPart", Bucket: req.Bucket, Key: req.Key,
			Message: "Body is required",
		}
	}
	if req.Size < 0 {
		return nil, &uos.Error{
			Code: uos.ErrLengthRequired, Provider: providerID,
			Operation: "UploadPart", Bucket: req.Bucket, Key: req.Key,
			Message: "Size is required for UploadPart",
		}
	}
	out, err := m.d.s3.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        ptrIfNotEmpty(req.Bucket),
		Key:           ptrIfNotEmpty(req.Key),
		UploadId:      ptrIfNotEmpty(req.UploadID),
		PartNumber:    ptr32(int32(req.PartNumber)),
		Body:          req.Body,
		ContentLength: ptr64(req.Size),
	})
	if err != nil {
		return nil, mapError("UploadPart", req.Bucket, req.Key, err)
	}
	return &uos.UploadedPart{
		PartNumber: req.PartNumber,
		ETag:       deref(out.ETag),
		Size:       req.Size,
	}, nil
}

// Complete finalises the upload via CompleteMultipartUpload.
func (m *multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	parts := make([]types.CompletedPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		p := p
		parts = append(parts, types.CompletedPart{
			ETag:       ptrIfNotEmpty(p.ETag),
			PartNumber: ptr32(int32(p.PartNumber)),
		})
	}
	out, err := m.d.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          ptrIfNotEmpty(req.Bucket),
		Key:             ptrIfNotEmpty(req.Key),
		UploadId:        ptrIfNotEmpty(req.UploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return nil, mapError("CompleteMultipartUpload", req.Bucket, req.Key, err)
	}
	res := &uos.PutObjectResult{
		ETag:      deref(out.ETag),
		VersionID: deref(out.VersionId),
	}
	return res, nil
}

// Abort cancels an in-flight upload via AbortMultipartUpload. AWS
// returns 204 even for unknown upload ids, so the operation is
// idempotent at the wire level.
func (m *multipartService) Abort(ctx context.Context, req uos.AbortMultipartRequest) error {
	_, err := m.d.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   ptrIfNotEmpty(req.Bucket),
		Key:      ptrIfNotEmpty(req.Key),
		UploadId: ptrIfNotEmpty(req.UploadID),
	})
	if err != nil {
		return mapError("AbortMultipartUpload", req.Bucket, req.Key, err)
	}
	return nil
}

// List enumerates in-flight multipart uploads via ListMultipartUploads.
// The opaque NextToken is the SDK's NextKeyMarker (AWS uses two
// markers for pagination but the v0.1 surface exposes only the key
// marker; full upload-id-marker support is left for v0.2).
func (m *multipartService) List(ctx context.Context, req uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	in := &s3.ListMultipartUploadsInput{
		Bucket:     ptrIfNotEmpty(req.Bucket),
		Prefix:     ptrIfNotEmpty(req.Prefix),
		MaxUploads: ptr32(int32(req.MaxResults)),
		KeyMarker:  ptrIfNotEmpty(req.ContinuationToken),
	}
	out, err := m.d.s3.ListMultipartUploads(ctx, in)
	if err != nil {
		return nil, mapError("ListMultipartUploads", req.Bucket, "", err)
	}
	res := &uos.MultipartUploadList{}
	for _, u := range out.Uploads {
		mp := uos.MultipartUpload{
			UploadID:     deref(u.UploadId),
			Bucket:       req.Bucket,
			Key:          deref(u.Key),
			StorageClass: string(u.StorageClass),
		}
		if u.Initiated != nil {
			mp.Initiated = *u.Initiated
		}
		res.Uploads = append(res.Uploads, mp)
	}
	if out.NextKeyMarker != nil {
		res.NextToken = *out.NextKeyMarker
	}
	if out.IsTruncated != nil {
		res.Truncated = *out.IsTruncated
	}
	return res, nil
}

// ----------------------------------------------------------------------
// Signer
// ----------------------------------------------------------------------

// signer is the AWS-flavoured Signer view bound to a bucket. It uses
// *s3.PresignClient for v4 presigning.
type signer struct {
	d      *driverImpl
	bucket string
}

// SignURL issues a presigned URL bound to req.Method (GET / PUT / HEAD /
// DELETE). Other methods return ErrInvalidArgument.
func (s *signer) SignURL(ctx context.Context, req uos.SignURLRequest) (*uos.SignedURL, error) {
	if req.ExpiresIn <= 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: providerID,
			Operation: "SignURL", Bucket: req.Bucket, Key: req.Key,
			Message: "ExpiresIn must be > 0",
		}
	}
	method := strings.ToUpper(req.Method)
	expiresAt := time.Now().Add(req.ExpiresIn)
	withExpiry := func(o *s3.PresignOptions) { o.Expires = req.ExpiresIn }

	switch method {
	case "GET":
		out, err := s.d.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket:    ptrIfNotEmpty(req.Bucket),
			Key:       ptrIfNotEmpty(req.Key),
			VersionId: ptrIfNotEmpty(req.VersionID),
		}, withExpiry)
		if err != nil {
			return nil, mapError("PresignGetObject", req.Bucket, req.Key, err)
		}
		return &uos.SignedURL{
			URL:       out.URL,
			Method:    "GET",
			ExpiresAt: expiresAt,
			Headers:   out.SignedHeader,
		}, nil
	case "PUT":
		out, err := s.d.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket: ptrIfNotEmpty(req.Bucket),
			Key:    ptrIfNotEmpty(req.Key),
		}, withExpiry)
		if err != nil {
			return nil, mapError("PresignPutObject", req.Bucket, req.Key, err)
		}
		return &uos.SignedURL{
			URL:       out.URL,
			Method:    "PUT",
			ExpiresAt: expiresAt,
			Headers:   out.SignedHeader,
		}, nil
	case "HEAD":
		out, err := s.d.presigner.PresignHeadObject(ctx, &s3.HeadObjectInput{
			Bucket: ptrIfNotEmpty(req.Bucket),
			Key:    ptrIfNotEmpty(req.Key),
		}, withExpiry)
		if err != nil {
			return nil, mapError("PresignHeadObject", req.Bucket, req.Key, err)
		}
		return &uos.SignedURL{
			URL:       out.URL,
			Method:    "HEAD",
			ExpiresAt: expiresAt,
			Headers:   out.SignedHeader,
		}, nil
	case "DELETE":
		out, err := s.d.presigner.PresignDeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: ptrIfNotEmpty(req.Bucket),
			Key:    ptrIfNotEmpty(req.Key),
		}, withExpiry)
		if err != nil {
			return nil, mapError("PresignDeleteObject", req.Bucket, req.Key, err)
		}
		return &uos.SignedURL{
			URL:       out.URL,
			Method:    "DELETE",
			ExpiresAt: expiresAt,
			Headers:   out.SignedHeader,
		}, nil
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: providerID,
			Operation: "SignURL", Bucket: req.Bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported method %q (allowed: GET, PUT, HEAD, DELETE)", req.Method),
		}
	}
}

// IssueDirectGrant returns ErrUnsupported with Capability=CapDirectGrant.
// AWS S3 issues write authorisation as presigned URL (CapSignedURLWrite);
// it has no non-URL grant model. See provider_matrix.md footnote 5.
func (s *signer) IssueDirectGrant(_ context.Context, req uos.DirectGrantRequest) (*uos.DirectGrant, error) {
	return nil, &uos.Error{
		Provider:   providerID,
		Operation:  "IssueDirectGrant",
		Bucket:     req.Bucket,
		Key:        req.Key,
		Code:       uos.ErrUnsupported,
		Capability: capability.CapDirectGrant,
		Message:    "S3 uses presigned URL — use Signer.SignURL instead",
	}
}

// ----------------------------------------------------------------------
// Helpers (pointer wrappers, conversions)
// ----------------------------------------------------------------------

// ptrIfNotEmpty returns *string for a non-empty s, nil otherwise. The
// AWS SDK uses pointer fields to distinguish "unset" from "set to
// empty"; this helper keeps the call sites compact.
func ptrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ptr32 returns *int32 for a non-zero v, nil otherwise.
func ptr32(v int32) *int32 {
	if v == 0 {
		return nil
	}
	return &v
}

// ptr64 returns *int64 for a non-zero v, nil otherwise.
func ptr64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// ptrBool returns &v unconditionally (AWS SDK uses *bool to indicate "explicitly set").
func ptrBool(v bool) *bool { return &v }

// deref returns the pointee of p, or "" when p is nil. Used to flatten
// AWS SDK *string response fields into uos value-typed equivalents.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// metadataToAWS converts uos.Metadata to the SDK's map[string]string.
// Drivers MUST lower-case keys per request.go contract; AWS S3 lower-
// cases on the wire too, so the round-trip is well-defined.
func metadataToAWS(m uos.Metadata) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// metadataFromAWS converts the SDK's map[string]string back to
// uos.Metadata, lower-casing keys defensively.
func metadataFromAWS(m map[string]string) uos.Metadata {
	if len(m) == 0 {
		return nil
	}
	out := make(uos.Metadata, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// applyContent stamps ContentHeaders fields onto the matching SDK
// pointer fields. Empty values leave the SDK pointer nil so the
// vendor's default is preserved.
func applyContent(ct, enc, lang, disp, cache **string, expires **time.Time, ch uos.ContentHeaders) {
	if ch.ContentType != "" {
		v := ch.ContentType
		*ct = &v
	}
	if ch.ContentEncoding != "" {
		v := ch.ContentEncoding
		*enc = &v
	}
	if ch.ContentLanguage != "" {
		v := ch.ContentLanguage
		*lang = &v
	}
	if ch.ContentDisposition != "" {
		v := ch.ContentDisposition
		*disp = &v
	}
	if ch.CacheControl != "" {
		v := ch.CacheControl
		*cache = &v
	}
	if !ch.Expires.IsZero() {
		t := ch.Expires
		*expires = &t
	}
}

// readContentHeaders rebuilds a uos.ContentHeaders from the AWS
// response pointers. Nil pointers leave the corresponding field zero.
func readContentHeaders(ct, enc, lang, disp, cache *string, expires *time.Time) uos.ContentHeaders {
	out := uos.ContentHeaders{
		ContentType:        deref(ct),
		ContentEncoding:    deref(enc),
		ContentLanguage:    deref(lang),
		ContentDisposition: deref(disp),
		CacheControl:       deref(cache),
	}
	if expires != nil {
		out.Expires = *expires
	}
	return out
}

// formatRange renders a uos.ByteRange as the HTTP Range header value
// required by S3 GetObject ("bytes=START-END" or "bytes=START-").
func formatRange(r uos.ByteRange) string {
	if r.End < 0 {
		return fmt.Sprintf("bytes=%d-", r.Start)
	}
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
}

// withContentMD5 returns a per-operation s3.Options mutator that injects
// a Build-step middleware computing the legacy Content-MD5 header on the
// request body. MinIO and several S3-compat targets still require
// Content-MD5 on bulk DeleteObjects (they return MissingContentMD5
// otherwise) even when the modern x-amz-checksum-* header is present.
// Real AWS accepts the redundant header without complaint.
func withContentMD5() func(*s3.Options) {
	return func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Build.Add(contentMD5Middleware{}, middleware.After)
		})
	}
}

// contentMD5Middleware is a Build-step middleware that buffers the
// request body, computes its MD5, and stamps the Content-MD5 header
// when the header is not already present. It is intentionally safe to
// add to operations that already carry a Content-MD5 header — those
// requests pass through unchanged.
type contentMD5Middleware struct{}

// ID identifies this middleware in the chain.
func (contentMD5Middleware) ID() string { return "uos/aws/ContentMD5" }

// HandleBuild buffers the body, MD5s it, and sets the Content-MD5 header.
func (contentMD5Middleware) HandleBuild(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (middleware.BuildOutput, middleware.Metadata, error) {
	req, ok := in.Request.(*smithyhttp.Request)
	if !ok || req == nil {
		return next.HandleBuild(ctx, in)
	}
	if req.Header.Get("Content-Md5") != "" {
		return next.HandleBuild(ctx, in)
	}
	stream := req.GetStream()
	if stream == nil {
		return next.HandleBuild(ctx, in)
	}
	body, err := io.ReadAll(stream)
	if err != nil {
		return middleware.BuildOutput{}, middleware.Metadata{}, fmt.Errorf("aws: read body for Content-MD5: %w", err)
	}
	sum := md5.Sum(body)
	req.Header.Set("Content-Md5", base64.StdEncoding.EncodeToString(sum[:]))
	// Replace the stream so downstream middlewares can re-read the body.
	newReq, err := req.SetStream(strings.NewReader(string(body)))
	if err != nil {
		return middleware.BuildOutput{}, middleware.Metadata{}, fmt.Errorf("aws: reset body for Content-MD5: %w", err)
	}
	in.Request = newReq
	return next.HandleBuild(ctx, in)
}

// deleteFailureCode maps the per-key error codes S3 returns inside a
// DeleteObjects response to pkg/uos.Code values. Unrecognised codes
// fall through to ErrInternal so the caller still sees the diagnostic
// in DeleteFailure.Message.
func deleteFailureCode(s string) uos.Code {
	switch s {
	case "AccessDenied":
		return uos.ErrPermissionDenied
	case "NoSuchKey":
		return uos.ErrNotFound
	case "NoSuchBucket":
		return uos.ErrNotFound
	case "InvalidArgument":
		return uos.ErrInvalidArgument
	}
	return uos.ErrInternal
}

// Compile-time guarantees that driverImpl implements the four uos
// service interfaces.
var (
	_ uos.Client           = (*driverImpl)(nil)
	_ uos.BucketService    = (*bucketService)(nil)
	_ uos.ObjectService    = (*objectService)(nil)
	_ uos.MultipartService = (*multipartService)(nil)
	_ uos.Signer           = (*signer)(nil)
	_                      = io.EOF
	_                      = awsv2.AnonymousCredentials{}
)
