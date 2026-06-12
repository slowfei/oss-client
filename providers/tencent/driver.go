package tencent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/capability"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// driverImpl implements pkg/uos.Client by translating to/from
// github.com/tencentyun/cos-go-sdk-v5. It holds a single base
// *cos.Client (its Service handle is bucket-agnostic and used for
// ListBuckets); per-bucket BucketURLs are built lazily in bucketClient.
//
// driverImpl is safe for concurrent use; *cos.Client is goroutine-safe
// and per-call bucket clients share the same authenticated http.Client.
type driverImpl struct {
	cfg        uos.Config
	client     *cos.Client
	httpClient *http.Client
	region     string
	appID      string
	scheme     string
	endpoint   string
}

// Provider returns "tencent". Required by uos.Client.
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

// As exposes the underlying cos-go-sdk-v5 handle for callers that need
// vendor-specific features. Supported targets:
//
//   - **cos.Client: filled with the high-level COS client (BaseURL.BucketURL is nil; callers set it per-call).
//
// Returns false (without mutating target) for any other type.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **cos.Client:
		*t = d.client
		return true
	default:
		return false
	}
}

// Close releases any driver-held resources. The cos-go-sdk-v5 client
// owns no goroutines that require shutdown beyond the underlying
// http.Client transport, so this is a no-op kept here to satisfy the
// uos.Client interface.
func (d *driverImpl) Close() error { return nil }

// bucketURL builds the per-bucket BucketURL according to the COS
// convention. If d.endpoint is set we honor it as the host (replacing
// only the bucket placeholder); otherwise the canonical
// "<bucket>-<appid>.cos.<region>.myqcloud.com" host is used.
//
// Returns *uos.Error{Code: ErrInvalidArgument} when the bucket name is
// empty or the endpoint cannot be parsed. The bucket name MAY already
// include the "-<appid>" suffix; if it does, AppID is ignored to avoid
// double-suffixing.
func (d *driverImpl) bucketURL(op, bucket string) (*url.URL, error) {
	if bucket == "" {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: op,
			Message:   "bucket name is required",
		}
	}
	full := bucket
	if d.appID != "" && !strings.Contains(bucket, "-") {
		full = bucket + "-" + d.appID
	}
	if d.endpoint != "" {
		// Caller supplied an explicit endpoint; substitute the bucket
		// placeholder (or use the endpoint host as-is when it already
		// names a specific bucket). For COS, the convention is to host
		// the bucket as a virtual-host subdomain.
		u, err := url.Parse(d.endpoint)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: op,
				Bucket:    bucket,
				Message:   fmt.Sprintf("invalid endpoint %q: %v", d.endpoint, err),
				Cause:     err,
			}
		}
		// If the endpoint already includes a host (it should for valid
		// usage), prepend the bucket as a subdomain when not already there.
		if u.Host != "" && !strings.HasPrefix(u.Host, full+".") {
			u.Host = full + "." + u.Host
		}
		return u, nil
	}
	raw := fmt.Sprintf("%s://%s.cos.%s.myqcloud.com", d.scheme, full, d.region)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: op,
			Bucket:    bucket,
			Message:   fmt.Sprintf("constructed bucket URL is invalid: %v", err),
			Cause:     err,
		}
	}
	return u, nil
}

// bucketClient returns a *cos.Client bound to the named bucket. The
// returned client shares the same http.Client / transport as the base
// driver client (so credentials and connection pool are reused) but
// has BaseURL.BucketURL set to the per-bucket URL.
func (d *driverImpl) bucketClient(op, bucket string) (*cos.Client, error) {
	bu, err := d.bucketURL(op, bucket)
	if err != nil {
		return nil, err
	}
	c := cos.NewClient(&cos.BaseURL{BucketURL: bu}, d.httpClient)
	// Mirror the base client's RetryOpt + CRC settings so per-call
	// clients don't reintroduce the SDK-internal retryer that the
	// factory disabled (RetryOpt.Count default is 3).
	c.Conf.RetryOpt.Count = 1
	return c, nil
}

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService implements uos.BucketService.
type bucketService struct{ d *driverImpl }

// List enumerates buckets visible to the configured credential. COS's
// Service.Get supports MaxKeys + Marker pagination; we honor both via
// the unified MaxResults / ContinuationToken fields.
func (b bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"
	opt := &cos.ServiceGetOptions{}
	if req.MaxResults > 0 {
		opt.MaxKeys = int64(req.MaxResults)
	}
	if req.ContinuationToken != "" {
		opt.Marker = req.ContinuationToken
	}
	res, _, err := b.d.client.Service.Get(ctx, opt)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, "", "", err)
	}
	out := make([]uos.BucketInfo, 0, len(res.Buckets))
	for _, bp := range res.Buckets {
		var created time.Time
		if bp.CreationDate != "" {
			if t, perr := time.Parse(time.RFC3339, bp.CreationDate); perr == nil {
				created = t
			}
		}
		out = append(out, uos.BucketInfo{
			Name:      bp.Name,
			Region:    bp.Region,
			CreatedAt: created,
		})
	}
	return out, nil
}

// Create makes a new bucket. COS rejects already-existing buckets with
// BucketAlreadyExists / BucketAlreadyOwnedByYou, which the error mapper
// translates to ErrAlreadyExists.
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
	bc, err := b.d.bucketClient(op, req.Name)
	if err != nil {
		return nil, err
	}
	opt := &cos.BucketPutOptions{}
	if req.ACL != "" {
		opt.XCosACL = req.ACL
	}
	if _, err := bc.Bucket.Put(ctx, opt); err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	region := req.Region
	if region == "" {
		region = b.d.region
	}
	return &uos.BucketInfo{
		Name:      req.Name,
		Region:    region,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Stat returns BucketInfo for an existing bucket. COS's Bucket.Head
// returns 404 NotFound when the bucket doesn't exist; the error mapper
// translates that to ErrNotFound.
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
	bc, err := b.d.bucketClient(op, req.Name)
	if err != nil {
		return nil, err
	}
	resp, err := bc.Bucket.Head(ctx)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	region := b.d.region
	if resp != nil && resp.Header != nil {
		if h := resp.Header.Get("X-Cos-Bucket-Region"); h != "" {
			region = h
		}
	}
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
	bc, err := b.d.bucketClient(op, req.Name)
	if err != nil {
		return err
	}
	if _, err := bc.Bucket.Delete(ctx); err != nil {
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
// produce the empty string and let bucketURL return InvalidArgument
// naturally.
func (o objectService) pickBucket(reqBucket string) string {
	if reqBucket != "" {
		return reqBucket
	}
	return o.defaultBucket
}

// Put writes a single object via COS Object.Put. Bodies pass through to
// the SDK untouched; the driver requires a known size (Size>=0) because
// COS's Put expects Content-Length on the request.
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
			Message: "Size is required (-1 unsupported by COS Put; use multipart for streaming)",
		}
	}
	bc, err := o.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	opt := &cos.ObjectPutOptions{
		ACLHeaderOptions:       buildACLHeaderOptions(req.ACL),
		ObjectPutHeaderOptions: buildPutHeaderOptions(req.Content, req.Metadata, req.StorageClass, req.Size, req.IfMatch, req.IfNoneMatch),
	}
	resp, err := bc.Object.Put(ctx, req.Key, req.Body, opt)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(headerGet(resp, "ETag"), `"`),
		VersionID: headerGet(resp, "x-cos-version-id"),
	}, nil
}

// Get streams an object body via COS Object.Get. Range requests use the
// SDK's Range header (formats "bytes=start-end"). Returned
// ObjectReader.Body is the raw io.ReadCloser; callers MUST Close it.
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)
	bc, err := o.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	opt := &cos.ObjectGetOptions{}
	if req.Range != nil {
		opt.Range = "bytes=" + formatRange(*req.Range)
	}
	if !req.IfModifiedSince.IsZero() {
		opt.IfModifiedSince = req.IfModifiedSince.UTC().Format(http.TimeFormat)
	}
	if hdr := buildPreconditionHeader(req.IfMatch, req.IfNoneMatch, time.Time{}, req.IfUnmodifiedSince); hdr != nil {
		opt.XOptionHeader = hdr
	}
	var resp *cos.Response
	if req.VersionID != "" {
		resp, err = bc.Object.Get(ctx, req.Key, opt, req.VersionID)
	} else {
		resp, err = bc.Object.Get(ctx, req.Key, opt)
	}
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectInfo(bucket, req.Key, resp.Header)
	return &uos.ObjectReader{
		Body:          resp.Body,
		ContentLength: info.Size,
		Info:          info,
	}, nil
}

// Head returns ObjectInfo without the body. COS's Object.Head returns
// the full header set including x-cos-meta-* user metadata.
func (o objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)
	bc, err := o.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	var resp *cos.Response
	if req.VersionID != "" {
		resp, err = bc.Object.Head(ctx, req.Key, nil, req.VersionID)
	} else {
		resp, err = bc.Object.Head(ctx, req.Key, nil)
	}
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateObjectInfo(bucket, req.Key, resp.Header)
	return &info, nil
}

// Delete removes a single object. COS Object.Delete is idempotent:
// removing a missing key returns 204 No Content, not 404.
func (o objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)
	bc, err := o.d.bucketClient(op, bucket)
	if err != nil {
		return err
	}
	opt := &cos.ObjectDeleteOptions{}
	if req.VersionID != "" {
		opt.VersionId = req.VersionID
	}
	if _, err := bc.Object.Delete(ctx, req.Key, opt); err != nil {
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

// deleteManyMaxBatch is the COS DeleteMulti batch cap (server-enforced
// per https://cloud.tencent.com/document/product/436/8289).
const deleteManyMaxBatch = 1000

// DeleteMany removes a batch of keys via COS Object.DeleteMulti. The
// SDK caps a single call at 1000 keys; we auto-batch above that. The
// returned DeleteManyResult merges the per-batch results into a single
// success/failure tally.
func (o objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	bc, err := o.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	out := &uos.DeleteManyResult{}
	for start := 0; start < len(req.Keys); start += deleteManyMaxBatch {
		end := start + deleteManyMaxBatch
		if end > len(req.Keys) {
			end = len(req.Keys)
		}
		objs := make([]cos.Object, 0, end-start)
		for _, k := range req.Keys[start:end] {
			objs = append(objs, cos.Object{Key: k})
		}
		batchOpt := &cos.ObjectDeleteMultiOptions{
			Quiet:   req.Quiet,
			Objects: objs,
		}
		res, _, berr := bc.Object.DeleteMulti(ctx, batchOpt)
		if berr != nil {
			return out, mapError(o.d.Provider(), op, bucket, "", berr)
		}
		if !req.Quiet {
			for _, d := range res.DeletedObjects {
				out.Deleted = append(out.Deleted, d.Key)
			}
		}
		for _, e := range res.Errors {
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
	}
	return out, nil
}

// Copy duplicates an object. COS supports same-account cross-bucket
// copy via x-cos-copy-source; we build the source URL from the source
// bucket's BucketURL host (the cos-go-sdk-v5 Copy helper expects
// "<host>/<key>" without scheme).
func (o objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	dstBucket := req.DestBucket
	bc, err := o.d.bucketClient(op, dstBucket)
	if err != nil {
		return nil, err
	}
	srcBucket := req.SourceBucket
	if srcBucket == "" {
		srcBucket = dstBucket
	}
	srcURL, err := o.d.bucketURL(op, srcBucket)
	if err != nil {
		return nil, err
	}
	source := srcURL.Host + "/" + req.SourceKey
	if req.SourceVersionID != "" {
		source = source + "?versionId=" + req.SourceVersionID
	}
	opt := &cos.ObjectCopyOptions{
		ObjectCopyHeaderOptions: &cos.ObjectCopyHeaderOptions{},
		ACLHeaderOptions:        buildACLHeaderOptions(req.ACL),
	}
	if req.Content.ContentType != "" {
		opt.ObjectCopyHeaderOptions.ContentType = req.Content.ContentType
	}
	if req.Content.ContentEncoding != "" {
		opt.ObjectCopyHeaderOptions.ContentEncoding = req.Content.ContentEncoding
	}
	if req.Content.ContentLanguage != "" {
		opt.ObjectCopyHeaderOptions.ContentLanguage = req.Content.ContentLanguage
	}
	if req.Content.ContentDisposition != "" {
		opt.ObjectCopyHeaderOptions.ContentDisposition = req.Content.ContentDisposition
	}
	if req.Content.CacheControl != "" {
		opt.ObjectCopyHeaderOptions.CacheControl = req.Content.CacheControl
	}
	if !req.Content.Expires.IsZero() {
		opt.ObjectCopyHeaderOptions.Expires = req.Content.Expires.UTC().Format(http.TimeFormat)
	}
	if req.StorageClass != "" {
		opt.ObjectCopyHeaderOptions.XCosStorageClass = req.StorageClass
	}
	if req.IfMatch != "" {
		opt.ObjectCopyHeaderOptions.XCosCopySourceIfMatch = req.IfMatch
	}
	if req.IfNoneMatch != "" {
		opt.ObjectCopyHeaderOptions.XCosCopySourceIfNoneMatch = req.IfNoneMatch
	}
	switch strings.ToUpper(req.MetadataDirective) {
	case "COPY":
		opt.ObjectCopyHeaderOptions.XCosMetadataDirective = "Copy"
	case "REPLACE":
		opt.ObjectCopyHeaderOptions.XCosMetadataDirective = "Replaced"
	default:
		// COS default mirrors AWS: COPY when no metadata is supplied,
		// REPLACE when the caller supplies metadata. The unified
		// CopyObjectRequest documents this implicit behaviour.
		if req.Metadata != nil {
			opt.ObjectCopyHeaderOptions.XCosMetadataDirective = "Replaced"
		}
	}
	if req.Metadata != nil {
		opt.ObjectCopyHeaderOptions.XCosMetaXXX = metadataToHeader(req.Metadata)
	}

	copyRes, resp, err := bc.Object.Copy(ctx, req.DestKey, source, opt)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, dstBucket, req.DestKey, err)
	}
	var lastModified time.Time
	if copyRes != nil && copyRes.LastModified != "" {
		if t, perr := time.Parse(time.RFC3339, copyRes.LastModified); perr == nil {
			lastModified = t
		}
	}
	versionID := ""
	if copyRes != nil && copyRes.VersionId != "" {
		versionID = copyRes.VersionId
	} else if resp != nil {
		versionID = headerGet(resp, "x-cos-version-id")
	}
	return &uos.CopyObjectResult{
		ETag:         strings.Trim(copyRes.ETag, `"`),
		LastModified: lastModified,
		VersionID:    versionID,
	}, nil
}

// List enumerates objects matching prefix / delimiter via COS
// Bucket.Get. NextToken round-trips through NextMarker so opaque-cursor
// pagination works across providers.
func (o objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)
	bc, err := o.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	opt := &cos.BucketGetOptions{
		Prefix:    req.Prefix,
		Delimiter: req.Delimiter,
		MaxKeys:   req.MaxResults,
	}
	// COS Bucket.Get does not have a native StartAfter equivalent; the
	// closest analogue is Marker, which we use for both opaque
	// continuation and StartAfter (the contract suite documents that
	// NextToken is opaque, so callers cannot rely on StartAfter
	// post-pagination semantics).
	if req.ContinuationToken != "" {
		opt.Marker = req.ContinuationToken
	} else if req.StartAfter != "" {
		opt.Marker = req.StartAfter
	}
	res, _, err := bc.Bucket.Get(ctx, opt)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}
	out := &uos.ObjectList{
		Items:          make([]uos.ObjectInfo, 0, len(res.Contents)),
		CommonPrefixes: append([]string(nil), res.CommonPrefixes...),
		NextToken:      res.NextMarker,
		Truncated:      res.IsTruncated,
	}
	for _, ob := range res.Contents {
		var lastModified time.Time
		if ob.LastModified != "" {
			if t, perr := time.Parse(time.RFC3339, ob.LastModified); perr == nil {
				lastModified = t
			}
		}
		out.Items = append(out.Items, uos.ObjectInfo{
			Bucket:       bucket,
			Key:          ob.Key,
			Size:         ob.Size,
			ETag:         strings.Trim(ob.ETag, `"`),
			LastModified: lastModified,
			StorageClass: ob.StorageClass,
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// MultipartService
// ----------------------------------------------------------------------

// multipartService implements uos.MultipartService backed by the COS
// raw multipart primitives (InitiateMultipartUpload / UploadPart /
// CompleteMultipartUpload / AbortMultipartUpload / ListUploads).
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
// carries the COS-issued UploadID that all subsequent UploadPart /
// Complete / Abort calls must reference.
func (m multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	bc, err := m.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	opt := &cos.InitiateMultipartUploadOptions{
		ACLHeaderOptions:       buildACLHeaderOptions(req.ACL),
		ObjectPutHeaderOptions: buildPutHeaderOptions(req.Content, req.Metadata, req.StorageClass, 0, "", ""),
	}
	res, _, err := bc.Object.InitiateMultipartUpload(ctx, req.Key, opt)
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

// UploadPart uploads a single part. COS UploadPart requires the
// caller-supplied size; we forward req.Size verbatim and let the wire
// layer surface InvalidArgument when the body length doesn't match.
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
	bc, err := m.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	opt := &cos.ObjectUploadPartOptions{
		ContentLength: req.Size,
	}
	resp, err := bc.Object.UploadPart(ctx, req.Key, req.UploadID, req.PartNumber, req.Body, opt)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.UploadedPart{
		PartNumber: req.PartNumber,
		ETag:       strings.Trim(headerGet(resp, "ETag"), `"`),
		Size:       req.Size,
	}, nil
}

// Complete finalises the multipart upload by stitching the supplied
// parts in PartNumber order. Parts MUST be presented sorted; we sort
// defensively (the contract suite still requires the caller to supply
// sorted input).
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if len(req.Parts) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Parts is required and must be non-empty",
		}
	}
	bc, err := m.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	parts := make([]cos.Object, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, cos.Object{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	opt := &cos.CompleteMultipartUploadOptions{Parts: parts}
	res, resp, err := bc.Object.CompleteMultipartUpload(ctx, req.Key, req.UploadID, opt)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(res.ETag, `"`),
		VersionID: headerGet(resp, "x-cos-version-id"),
	}, nil
}

// Abort cancels an in-flight multipart upload. COS makes Abort
// idempotent at the wire level (NoSuchUpload returns 204 in some
// regions, ErrorResponse in others); the error mapper translates either
// to ErrNotFound when applicable.
func (m multipartService) Abort(ctx context.Context, req uos.AbortMultipartRequest) error {
	const op = "AbortMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	bc, err := m.d.bucketClient(op, bucket)
	if err != nil {
		return err
	}
	if _, err := bc.Object.AbortMultipartUpload(ctx, req.Key, req.UploadID); err != nil {
		return mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// List enumerates in-flight multipart uploads in the bucket. Pagination
// is handled by COS via KeyMarker; we expose a single page (the vendor
// default cap is 1000 uploads) and surface NextToken when more pages
// remain so callers can iterate.
func (m multipartService) List(ctx context.Context, req uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	const op = "ListMultipartUploads"
	bucket := m.pickBucket(req.Bucket)
	bc, err := m.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	opt := &cos.ObjectListUploadsOptions{
		Prefix:     req.Prefix,
		MaxUploads: req.MaxResults,
		KeyMarker:  req.ContinuationToken,
	}
	res, _, err := bc.Object.ListUploads(ctx, opt)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, "", err)
	}
	out := &uos.MultipartUploadList{
		Uploads:   make([]uos.MultipartUpload, 0, len(res.Upload)),
		Truncated: res.IsTruncated,
		NextToken: res.NextKeyMarker,
	}
	for _, u := range res.Upload {
		var initiated time.Time
		if u.Initiated != "" {
			if t, perr := time.Parse(time.RFC3339, u.Initiated); perr == nil {
				initiated = t
			}
		}
		out.Uploads = append(out.Uploads, uos.MultipartUpload{
			UploadID:     u.UploadID,
			Bucket:       bucket,
			Key:          u.Key,
			Initiated:    initiated,
			StorageClass: u.StorageClass,
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// Signer
// ----------------------------------------------------------------------

// signerService implements uos.Signer for COS HMAC v1 presigned URLs.
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

// SignURL returns an HTTP-signed URL for the requested operation. COS's
// Object.GetPresignedURL builds the URL synchronously (no I/O) and
// returns *url.URL; we wrap it in the unified SignedURL shape.
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
	switch method {
	case http.MethodGet, http.MethodPut, http.MethodHead, http.MethodDelete:
		// supported
	case "":
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Method is required",
		}
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q (allowed: GET, PUT, HEAD, DELETE)", method),
		}
	}
	bc, err := s.d.bucketClient(op, bucket)
	if err != nil {
		return nil, err
	}
	cred := s.d.client.GetCredential()
	if cred == nil {
		return nil, &uos.Error{
			Code: uos.ErrUnauthenticated, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "credential transport not initialised",
		}
	}
	presOpt := &cos.PresignedURLOptions{}
	if len(req.Query) > 0 {
		q := url.Values{}
		for k, v := range req.Query {
			q.Set(k, v)
		}
		if req.VersionID != "" {
			q.Set("versionId", req.VersionID)
		}
		presOpt.Query = &q
	} else if req.VersionID != "" {
		q := url.Values{}
		q.Set("versionId", req.VersionID)
		presOpt.Query = &q
	}
	if len(req.Headers) > 0 {
		hdr := req.Headers.Clone()
		presOpt.Header = &hdr
	}
	signed, err := bc.Object.GetPresignedURL(ctx, method, req.Key, cred.SecretID, cred.SecretKey, req.ExpiresIn, presOpt)
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
// because S3-family providers (including COS) issue write
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
		Message:    "COS uses presigned URL — use Signer.SignURL instead",
	}
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// buildPutHeaderOptions assembles the per-Put header bag for COS,
// stamping content-negotiation headers, user metadata, storage class,
// content-length, and conditional-write preconditions.
func buildPutHeaderOptions(content uos.ContentHeaders, meta uos.Metadata, storageClass string, size int64, ifMatch, ifNoneMatch string) *cos.ObjectPutHeaderOptions {
	out := &cos.ObjectPutHeaderOptions{}
	if content.ContentType != "" {
		out.ContentType = content.ContentType
	}
	if content.ContentEncoding != "" {
		out.ContentEncoding = content.ContentEncoding
	}
	if content.ContentLanguage != "" {
		out.ContentLanguage = content.ContentLanguage
	}
	if content.ContentDisposition != "" {
		out.ContentDisposition = content.ContentDisposition
	}
	if content.CacheControl != "" {
		out.CacheControl = content.CacheControl
	}
	if !content.Expires.IsZero() {
		out.Expires = content.Expires.UTC().Format(http.TimeFormat)
	}
	if storageClass != "" {
		out.XCosStorageClass = storageClass
	}
	if size > 0 {
		out.ContentLength = size
	}
	if h := metadataToHeader(meta); h != nil {
		out.XCosMetaXXX = h
	}
	if hdr := buildPreconditionHeader(ifMatch, ifNoneMatch, time.Time{}, time.Time{}); hdr != nil {
		out.XOptionHeader = hdr
	}
	return out
}

// buildACLHeaderOptions returns the per-object ACL bag for COS, or nil
// when no ACL was requested. Empty ACL leaves the vendor default in
// place.
func buildACLHeaderOptions(acl string) *cos.ACLHeaderOptions {
	if acl == "" {
		return nil
	}
	return &cos.ACLHeaderOptions{XCosACL: acl}
}

// buildPreconditionHeader synthesises the conditional-write headers
// (If-Match / If-None-Match / If-Unmodified-Since) into an
// http.Header bag for use as ObjectPutHeaderOptions.XOptionHeader or
// ObjectGetOptions.XOptionHeader. Returns nil when no precondition is
// requested.
func buildPreconditionHeader(ifMatch, ifNoneMatch string, _ time.Time, ifUnmodifiedSince time.Time) *http.Header {
	var hdr http.Header
	add := func(k, v string) {
		if hdr == nil {
			hdr = http.Header{}
		}
		hdr.Set(k, v)
	}
	if ifMatch != "" {
		add("If-Match", ifMatch)
	}
	if ifNoneMatch != "" {
		add("If-None-Match", ifNoneMatch)
	}
	if !ifUnmodifiedSince.IsZero() {
		add("If-Unmodified-Since", ifUnmodifiedSince.UTC().Format(http.TimeFormat))
	}
	if hdr == nil {
		return nil
	}
	return &hdr
}

// metadataToHeader converts a uos.Metadata map into an http.Header
// suitable for COS's XCosMetaXXX field. Keys are lower-cased via
// s3common.LowerMetadataKeys; the COS wire layer stamps the
// "x-cos-meta-" prefix automatically when XCosMetaXXX is set, so we
// store the suffix only.
func metadataToHeader(m uos.Metadata) *http.Header {
	lower := s3common.LowerMetadataKeys(m)
	if len(lower) == 0 {
		return nil
	}
	hdr := http.Header{}
	for k, v := range lower {
		hdr.Set(k, v)
	}
	return &hdr
}

// translateObjectInfo rebuilds a uos.ObjectInfo from a COS response
// header set. The COS user-metadata convention prefixes each key with
// "X-Cos-Meta-"; we strip the prefix and lower-case the remainder via
// s3common.LowerMetadataKeys for round-trip equality with the unified
// Metadata contract.
func translateObjectInfo(bucket, key string, header http.Header) uos.ObjectInfo {
	info := uos.ObjectInfo{
		Bucket: bucket,
		Key:    key,
		ETag:   strings.Trim(header.Get("ETag"), `"`),
	}
	if v := header.Get("Content-Length"); v != "" {
		if size, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			info.Size = size
		} else {
			info.Size = -1
		}
	} else {
		info.Size = -1
	}
	if v := header.Get("Last-Modified"); v != "" {
		if t, perr := http.ParseTime(v); perr == nil {
			info.LastModified = t
		}
	}
	if v := header.Get("X-Cos-Storage-Class"); v != "" {
		info.StorageClass = v
	}
	if v := header.Get("X-Cos-Version-Id"); v != "" {
		info.VersionID = v
	}
	info.Content = uos.ContentHeaders{
		ContentType:        header.Get("Content-Type"),
		ContentEncoding:    header.Get("Content-Encoding"),
		ContentLanguage:    header.Get("Content-Language"),
		ContentDisposition: header.Get("Content-Disposition"),
		CacheControl:       header.Get("Cache-Control"),
	}
	if v := header.Get("Expires"); v != "" {
		if t, perr := http.ParseTime(v); perr == nil {
			info.Content.Expires = t
		}
	}
	info.Metadata = extractUserMetadata(header)
	return info
}

// extractUserMetadata pulls the lower-cased user-defined metadata out of
// a COS response header set. COS prefixes each key with "X-Cos-Meta-";
// we strip the prefix and lower-case the remainder. Returns nil for an
// empty result so the unified ObjectInfo.Metadata contract (nil ==
// "no metadata") is preserved.
func extractUserMetadata(h http.Header) uos.Metadata {
	const prefix = "X-Cos-Meta-"
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

// formatRange renders a uos.ByteRange as the suffix the COS Range
// header expects ("start-end" or "start-"). The "bytes=" prefix is
// added by the caller in objectService.Get.
func formatRange(r uos.ByteRange) string {
	if r.End < 0 {
		return fmt.Sprintf("%d-", r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End)
}

// headerGet is a nil-safe accessor for cos.Response.Header.
func headerGet(resp *cos.Response, key string) string {
	if resp == nil || resp.Response == nil {
		return ""
	}
	return resp.Header.Get(key)
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
