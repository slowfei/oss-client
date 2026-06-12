package qiniu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	qauth "github.com/qiniu/go-sdk/v7/auth"
	"github.com/qiniu/go-sdk/v7/storage"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/capability"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// driverImpl implements pkg/uos.Client backed by the qiniu/go-sdk/v7
// storage + auth packages.
//
// Bucket → Bucket: the unified Bucket maps 1:1 onto a Qiniu Kodo bucket.
// Region (Qiniu zone) is captured once in DriverConfig.Region (or
// uos.Config.Region) and used to build the SDK *storage.Config.
//
// driverImpl is safe for concurrent use; the underlying SDK objects
// (BucketManager, FormUploader, ResumeUploaderV2) are goroutine-safe; the
// in-process upload-session map is guarded by sync.Mutex.
type driverImpl struct {
	cfg    uos.Config
	dc     *DriverConfig
	region string

	mac    *qauth.Credentials
	sdkCfg *storage.Config

	bucketManager  *storage.BucketManager
	formUploader   *storage.FormUploader
	resumeUploader *storage.ResumeUploaderV2

	// uploadSessions tracks in-flight RUv2 uploads. Qiniu's RUv2 returns an
	// opaque uploadId from InitParts; the driver passes it back as the
	// unified UploadID (no synthesis needed). The map records the per-part
	// etag list so Complete can build the parts manifest, plus the bucket /
	// key / token / upToken host for subsequent UploadParts/Complete calls.
	uploadMu       sync.Mutex
	uploadSessions map[string]*uploadSession
}

// uploadSession holds the in-flight state for one RUv2 upload.
type uploadSession struct {
	bucket    string
	key       string
	uploadID  string
	upHost    string
	upToken   string
	initiated time.Time
	parts     []storage.UploadPartInfo // accumulated per-part metadata
	metadata  map[string]string        // x-qn-meta-* + x:* user vars
	mime      string
}

// Provider returns "qiniu".
func (d *driverImpl) Provider() uos.Provider { return providerID }

// Capabilities returns the v1-frozen capability.Report for this driver.
func (d *driverImpl) Capabilities(_ context.Context) (capability.Report, error) {
	return capabilities(), nil
}

// Buckets returns the BucketService view bound to this Client.
func (d *driverImpl) Buckets() uos.BucketService { return bucketService{d: d} }

// Objects returns the ObjectService view bound to the named bucket.
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

// As exposes the underlying qiniu SDK objects for vendor-specific
// operations. Supported targets:
//
//   - **storage.BucketManager: filled with the high-level bucket admin client.
//   - **storage.FormUploader: filled with the small-object upload client.
//   - **storage.ResumeUploaderV2: filled with the multipart upload client.
//   - **auth.Credentials: filled with the underlying AK/SK pair.
//
// Returns false (without mutating target) for any other type.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **storage.BucketManager:
		*t = d.bucketManager
		return true
	case **storage.FormUploader:
		*t = d.formUploader
		return true
	case **storage.ResumeUploaderV2:
		*t = d.resumeUploader
		return true
	case **qauth.Credentials:
		*t = d.mac
		return true
	default:
		return false
	}
}

// Close is a no-op: the qiniu SDK holds no background goroutines that
// require explicit shutdown.
func (d *driverImpl) Close() error { return nil }

// storeSession stores an upload session under the SDK uploadId.
func (d *driverImpl) storeSession(s *uploadSession) {
	d.uploadMu.Lock()
	defer d.uploadMu.Unlock()
	d.uploadSessions[s.uploadID] = s
}

// loadSession looks up an upload session by uploadId.
func (d *driverImpl) loadSession(uploadID string) (*uploadSession, bool) {
	d.uploadMu.Lock()
	defer d.uploadMu.Unlock()
	s, ok := d.uploadSessions[uploadID]
	return s, ok
}

// deleteSession removes an upload session by uploadId.
func (d *driverImpl) deleteSession(uploadID string) {
	d.uploadMu.Lock()
	defer d.uploadMu.Unlock()
	delete(d.uploadSessions, uploadID)
}

// listSessions returns all in-flight sessions for a bucket optionally
// filtered by prefix.
func (d *driverImpl) listSessions(bucket, prefix string) []*uploadSession {
	d.uploadMu.Lock()
	defer d.uploadMu.Unlock()
	var out []*uploadSession
	for _, s := range d.uploadSessions {
		if s.bucket != bucket {
			continue
		}
		if prefix != "" && !strings.HasPrefix(s.key, prefix) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// uploadHost picks the upload host to advertise in DirectGrant.URL and to
// route ResumeUploaderV2 calls to. Order: DriverConfig.UploadEndpoint
// override → SDK's per-region resolution. Empty result lets the SDK pick at
// call time (the SDK has its own region resolver).
func (d *driverImpl) uploadHost(bucket string) string {
	if d.dc.UploadEndpoint != "" {
		return d.dc.UploadEndpoint
	}
	if h, err := d.resumeUploader.UpHost(d.mac.AccessKey, bucket); err == nil {
		return h
	}
	return ""
}

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService implements uos.BucketService over storage.BucketManager.
type bucketService struct{ d *driverImpl }

// List enumerates buckets visible to the configured credential. Qiniu's
// Buckets API does not paginate at the SDK level; we return all in a
// single call and ignore ContinuationToken / MaxResults.
func (b bucketService) List(_ context.Context, _ uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"
	names, err := b.d.bucketManager.Buckets(false)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, "", "", err)
	}
	out := make([]uos.BucketInfo, 0, len(names))
	for _, name := range names {
		out = append(out, uos.BucketInfo{
			Name:   name,
			Region: b.d.region,
		})
	}
	return out, nil
}

// Create provisions a new bucket. Region is taken from req.Region if set,
// otherwise the driver's configured region.
func (b bucketService) Create(_ context.Context, req uos.CreateBucketRequest) (*uos.BucketInfo, error) {
	const op = "CreateBucket"
	if req.Name == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: b.d.Provider(), Operation: op,
			Message: "bucket name is required",
		}
	}
	region := req.Region
	if region == "" {
		region = b.d.region
	}
	if region == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: b.d.Provider(), Operation: op,
			Bucket: req.Name, Message: "region is required to create a qiniu bucket (Qiniu zone, e.g. \"z0\")",
		}
	}
	if err := b.d.bucketManager.CreateBucket(req.Name, storage.RegionID(region)); err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return &uos.BucketInfo{
		Name:      req.Name,
		Region:    region,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Stat checks bucket existence. Qiniu has no dedicated Stat API; the
// idiomatic check is a 1-key list-files probe.
func (b bucketService) Stat(_ context.Context, req uos.StatBucketRequest) (*uos.BucketInfo, error) {
	const op = "StatBucket"
	if req.Name == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: b.d.Provider(), Operation: op,
			Message: "bucket name is required",
		}
	}
	// Use a 1-key list-files probe: if the bucket exists (even when empty)
	// the call succeeds; if it doesn't, we get storage.ErrBucketNotExist.
	if _, _, _, _, err := b.d.bucketManager.ListFiles(req.Name, "", "", "", 1); err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return &uos.BucketInfo{
		Name:   req.Name,
		Region: b.d.region,
	}, nil
}

// Delete removes an empty bucket. Qiniu rejects non-empty buckets.
func (b bucketService) Delete(_ context.Context, req uos.DeleteBucketRequest) error {
	const op = "DeleteBucket"
	if req.Name == "" {
		return &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: b.d.Provider(), Operation: op,
			Message: "bucket name is required",
		}
	}
	if err := b.d.bucketManager.DropBucket(req.Name); err != nil {
		return mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return nil
}

// ----------------------------------------------------------------------
// ObjectService
// ----------------------------------------------------------------------

// objectService implements uos.ObjectService.
type objectService struct {
	d             *driverImpl
	defaultBucket string
}

func (o objectService) pickBucket(reqBucket string) string {
	if reqBucket != "" {
		return reqBucket
	}
	return o.defaultBucket
}

// Put uploads a single object via storage.FormUploader. The caller-supplied
// metadata (x-qn-meta-*) and content type are baked into the PutPolicy.
// Qiniu requires Size for FormUploader; -1 is rejected with ErrLengthRequired.
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
			Message: "Size is required for qiniu PutObject; use MultipartService for unknown-size uploads",
		}
	}

	// Build a scoped Upload Token bound to (bucket, key) so the upload can
	// only land at the target key. Expires=3600s default.
	policy := storage.PutPolicy{
		Scope:     bucket + ":" + req.Key,
		Expires:   3600,
		MimeLimit: req.Content.ContentType,
	}
	upToken := policy.UploadToken(o.d.mac)

	extra := &storage.PutExtra{
		MimeType: req.Content.ContentType,
		Params:   buildPutParams(req.Metadata),
	}
	var ret storage.PutRet
	if err := o.d.formUploader.Put(ctx, &ret, upToken, req.Key, req.Body, req.Size, extra); err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag: ret.Hash,
	}, nil
}

// Get downloads an object body. Qiniu Kodo serves downloads via the
// io/source domain bound to the bucket. For private buckets the URL must
// be signed via storage.MakePrivateURL; for public buckets a public URL
// works. The driver always signs (private-safe default) when DriverConfig.Domain
// is set; without Domain we return ErrUnsupported because Kodo does not
// expose a "download via management API" path.
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)
	if o.d.dc.Domain == "" {
		return nil, &uos.Error{
			Code: uos.ErrUnsupported, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Capability: capability.CapObjectCRUD,
			Message:    "qiniu GetObject requires DriverConfig.Domain (the bucket's bound CDN/source domain) — Kodo has no management-API download path",
		}
	}
	deadline := time.Now().Add(5 * time.Minute).Unix()
	url := storage.MakePrivateURL(o.d.mac, o.d.dc.Domain, req.Key, deadline)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &uos.Error{
			Code: uos.ErrInternal, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: err.Error(), Cause: err,
		}
	}
	if req.Range != nil {
		httpReq.Header.Set("Range", buildRangeHeader(*req.Range))
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	if resp.StatusCode >= 400 {
		// Build an ErrorInfo so the mapper translates the wire status.
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, &qiniuStatusError{
			status: resp.StatusCode,
			msg:    strings.TrimSpace(string(body)),
		})
	}
	info := uos.ObjectInfo{
		Bucket:       bucket,
		Key:          req.Key,
		Size:         resp.ContentLength,
		LastModified: parseLastModified(resp.Header.Get("Last-Modified")),
		ETag:         strings.Trim(resp.Header.Get("ETag"), `"`),
		Content: uos.ContentHeaders{
			ContentType:        resp.Header.Get("Content-Type"),
			ContentEncoding:    resp.Header.Get("Content-Encoding"),
			ContentDisposition: resp.Header.Get("Content-Disposition"),
			CacheControl:       resp.Header.Get("Cache-Control"),
		},
		Metadata: extractQiniuMetadata(resp.Header),
	}
	return &uos.ObjectReader{
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		Info:          info,
	}, nil
}

// Head returns object metadata via BucketManager.Stat (the management API
// equivalent of S3 HEAD). Versioning is not exposed.
func (o objectService) Head(_ context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)
	fi, err := o.d.bucketManager.Stat(bucket, req.Key)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateFileInfo(bucket, req.Key, fi)
	return &info, nil
}

// Delete removes a single object. Idempotent at the unified contract layer:
// deleting a missing key returns nil.
func (o objectService) Delete(_ context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)
	if err := o.d.bucketManager.Delete(bucket, req.Key); err != nil {
		mapped := mapError(o.d.Provider(), op, bucket, req.Key, err)
		var ue *uos.Error
		if errors.As(mapped, &ue) && ue.Code == uos.ErrNotFound {
			return nil // idempotent
		}
		return mapped
	}
	return nil
}

// Exists reports whether an object exists.
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

// DeleteMany batches deletes via the SDK's Batch API. Quiet semantics are
// honored.
func (o objectService) DeleteMany(_ context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	ops := make([]string, 0, len(req.Keys))
	for _, k := range req.Keys {
		ops = append(ops, storage.URIDelete(bucket, k))
	}
	results, err := o.d.bucketManager.Batch(ops)
	out := &uos.DeleteManyResult{}
	if err != nil {
		// A batch-level error (network, auth, etc.) — surface for all keys.
		mapped := mapError(o.d.Provider(), op, bucket, "", err)
		var ue *uos.Error
		if !errors.As(mapped, &ue) {
			return nil, mapped
		}
		for _, k := range req.Keys {
			out.Failed = append(out.Failed, uos.DeleteFailure{
				Key:     k,
				Code:    ue.Code,
				Message: ue.Message,
			})
		}
		return out, nil
	}
	for i, r := range results {
		if i >= len(req.Keys) {
			break
		}
		key := req.Keys[i]
		if r.Code == 200 || r.Code == 612 /* not found — treat as deleted */ {
			if !req.Quiet {
				out.Deleted = append(out.Deleted, key)
			}
			continue
		}
		// Map the per-key code via mapQiniuReason + HTTP status fallback.
		code := uos.ErrInternal
		if c, ok := mapQiniuReason(r.Data.Error); ok {
			code = c
		} else if c, ok := s3common.MapHTTPStatus(r.Code); ok {
			code = c
		}
		out.Failed = append(out.Failed, uos.DeleteFailure{
			Key:     key,
			Code:    code,
			Message: r.Data.Error,
		})
	}
	return out, nil
}

// Copy duplicates an object server-side via BucketManager.Copy with
// force=true (mirrors S3 overwrite semantics).
func (o objectService) Copy(_ context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	srcBucket := req.SourceBucket
	if srcBucket == "" {
		srcBucket = o.defaultBucket
	}
	dstBucket := req.DestBucket
	if err := o.d.bucketManager.Copy(srcBucket, req.SourceKey, dstBucket, req.DestKey, true); err != nil {
		return nil, mapError(o.d.Provider(), op, dstBucket, req.DestKey, err)
	}
	// Re-stat the destination to pick up the resulting hash + LastModified.
	fi, err := o.d.bucketManager.Stat(dstBucket, req.DestKey)
	if err != nil {
		// Copy succeeded but stat failed — still return success with empty metadata.
		return &uos.CopyObjectResult{}, nil //nolint:nilerr
	}
	return &uos.CopyObjectResult{
		ETag:         fi.Hash,
		LastModified: putTimeToTime(fi.PutTime),
	}, nil
}

// List enumerates objects matching prefix/delimiter via ListFiles.
func (o objectService) List(_ context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)
	limit := req.MaxResults
	if limit <= 0 {
		limit = 1000
	}
	items, prefixes, nextMarker, hasNext, err := o.d.bucketManager.ListFiles(
		bucket, req.Prefix, req.Delimiter, req.ContinuationToken, limit,
	)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}
	out := &uos.ObjectList{
		CommonPrefixes: prefixes,
		NextToken:      nextMarker,
		Truncated:      hasNext,
	}
	for _, it := range items {
		out.Items = append(out.Items, translateListItem(bucket, it))
	}
	return out, nil
}

// ----------------------------------------------------------------------
// MultipartService — Qiniu Resumable Upload v2 (RUv2)
// ----------------------------------------------------------------------
//
// Qiniu's RUv2 maps cleanly onto MultipartService:
//
//	Initiate   → ResumeUploaderV2.InitParts; the SDK returns an opaque
//	             uploadId we surface as the unified UploadID.
//	UploadPart → ResumeUploaderV2.UploadParts (one block per call); the
//	             returned UploadPartsRet.Etag is recorded for Complete.
//	Complete   → ResumeUploaderV2.CompleteParts with the recorded parts list.
//	Abort      → no server-side abort API; we drop the in-process session
//	             entry. Uncommitted RUv2 sessions expire automatically per
//	             the InitPartsRet.ExpireAt timestamp (default 7 days).
//	List       → returns in-process sessions only (mirrors the gcs / azure
//	             pattern documented in Lessons (M4)).
//
// Per the RUv2 contract, parts are sequential per block — but the upload
// API itself accepts parts in any order; ordering is enforced at Complete
// time via the parts manifest. The unified contract requires Parts sorted
// by PartNumber ascending at Complete; we honor that.

// multipartService implements uos.MultipartService over RUv2.
type multipartService struct {
	d             *driverImpl
	defaultBucket string
}

func (m multipartService) pickBucket(reqBucket string) string {
	if reqBucket != "" {
		return reqBucket
	}
	return m.defaultBucket
}

// Initiate starts a new RUv2 upload by minting an Upload Token, calling
// InitParts, and stashing the resulting uploadId + per-call state in the
// in-process session map.
func (m multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if req.Key == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Message: "Key is required",
		}
	}

	// Build a scoped Upload Token bound to (bucket, key) for the upload session.
	policy := storage.PutPolicy{
		Scope:   bucket + ":" + req.Key,
		Expires: 3600 * 24, // 24h lifetime for multipart sessions
	}
	upToken := policy.UploadToken(m.d.mac)
	upHost, err := m.d.resumeUploader.UpHost(m.d.mac.AccessKey, bucket)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}

	var initRet storage.InitPartsRet
	if err := m.d.resumeUploader.InitParts(ctx, upToken, upHost, bucket, req.Key, true, &initRet); err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}

	sess := &uploadSession{
		bucket:    bucket,
		key:       req.Key,
		uploadID:  initRet.UploadID,
		upHost:    upHost,
		upToken:   upToken,
		initiated: time.Now().UTC(),
		metadata:  buildPutParams(req.Metadata),
		mime:      req.Content.ContentType,
	}
	m.d.storeSession(sess)

	return &uos.MultipartUpload{
		UploadID:     initRet.UploadID,
		Bucket:       bucket,
		Key:          req.Key,
		Initiated:    sess.initiated,
		StorageClass: req.StorageClass,
		Metadata:     req.Metadata,
	}, nil
}

// UploadPart stages one block via ResumeUploaderV2.UploadParts. The
// returned per-part etag is recorded in the session for Complete.
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
			Bucket: bucket, Key: req.Key, Message: "Size is required for qiniu UploadPart",
		}
	}
	if req.PartNumber < 1 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "PartNumber must be >= 1",
		}
	}
	sess, ok := m.d.loadSession(req.UploadID)
	if !ok {
		return nil, &uos.Error{
			Code: uos.ErrNotFound, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("upload session %q not found in this process", req.UploadID),
		}
	}

	var ret storage.UploadPartsRet
	// partMD5 is empty string ("" means caller did not supply MD5; SDK skips MD5 verification).
	if err := m.d.resumeUploader.UploadParts(
		ctx, sess.upToken, sess.upHost, sess.bucket, sess.key, true,
		sess.uploadID, int64(req.PartNumber), "", &ret, req.Body, int(req.Size),
	); err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}

	// Record the part for Complete.
	m.d.uploadMu.Lock()
	sess.parts = append(sess.parts, storage.UploadPartInfo{
		Etag:       ret.Etag,
		PartNumber: int64(req.PartNumber),
	})
	m.d.uploadMu.Unlock()

	return &uos.UploadedPart{
		PartNumber: req.PartNumber,
		ETag:       ret.Etag,
		Size:       req.Size,
	}, nil
}

// Complete finalises the RUv2 upload by calling CompleteParts with the
// caller-supplied parts list. Parts MUST be sorted by PartNumber
// ascending per the unified contract.
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if len(req.Parts) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Parts must be non-empty",
		}
	}
	sess, ok := m.d.loadSession(req.UploadID)
	if !ok {
		return nil, &uos.Error{
			Code: uos.ErrNotFound, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("upload session %q not found in this process", req.UploadID),
		}
	}

	// Build the parts manifest from the caller's UploadedPart list.
	progresses := make([]storage.UploadPartInfo, 0, len(req.Parts))
	for _, p := range req.Parts {
		progresses = append(progresses, storage.UploadPartInfo{
			Etag:       p.ETag,
			PartNumber: int64(p.PartNumber),
		})
	}

	extra := &storage.RputV2Extra{
		Progresses: progresses,
		MimeType:   sess.mime,
		Metadata:   filterMetaPrefix(sess.metadata),
		CustomVars: filterCustomVarsPrefix(sess.metadata),
	}
	var ret storage.PutRet
	if err := m.d.resumeUploader.CompleteParts(
		ctx, sess.upToken, sess.upHost, &ret,
		sess.bucket, sess.key, true, sess.uploadID, extra,
	); err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	m.d.deleteSession(req.UploadID)
	return &uos.PutObjectResult{ETag: ret.Hash}, nil
}

// Abort drops the in-process session. Qiniu has no server-side abort API
// for uncommitted RUv2 uploads — they expire per InitPartsRet.ExpireAt.
func (m multipartService) Abort(_ context.Context, req uos.AbortMultipartRequest) error {
	m.d.deleteSession(req.UploadID)
	return nil
}

// List returns in-process upload sessions for the bucket. Cross-process
// orphan listing is not supported (mirrors the gcs / azure pattern).
func (m multipartService) List(_ context.Context, req uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	bucket := m.pickBucket(req.Bucket)
	sessions := m.d.listSessions(bucket, req.Prefix)
	limit := req.MaxResults
	if limit <= 0 {
		limit = len(sessions)
	}
	out := &uos.MultipartUploadList{}
	for i, s := range sessions {
		if i >= limit {
			out.Truncated = true
			break
		}
		out.Uploads = append(out.Uploads, uos.MultipartUpload{
			UploadID:  s.uploadID,
			Bucket:    s.bucket,
			Key:       s.key,
			Initiated: s.initiated,
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// Signer — SignURL (download) + IssueDirectGrant (Upload Token)
// ----------------------------------------------------------------------
//
// Qiniu signing surfaces:
//
//   - SignURL(GET/HEAD) → storage.MakePrivateURL on DriverConfig.Domain.
//   - SignURL(PUT/POST) → ErrUnsupported{CapSignedURLWrite, Reason} per
//     provider_matrix.md footnote 4 (Qiniu write authorization is non-URL).
//   - IssueDirectGrant(upload) → Upload Token via PutPolicy.UploadToken,
//     wrapped in DirectGrant{Mode: DirectGrantModeToken}. **The M5 milestone
//     validation: DirectGrantModeToken used in a NEW context (distinct from
//     M4 azure SAS, which is also Token but encoded as a URL query string).**
//   - IssueDirectGrant(download) → signed URL via MakePrivateURL embedded
//     as DirectGrant.Token (URL string is the bearer payload), Mode=Token.
//     Caller GETs the URL directly.

// signerService implements uos.Signer for Qiniu.
type signerService struct {
	d             *driverImpl
	defaultBucket string
}

func (s signerService) pickBucket(reqBucket string) string {
	if reqBucket != "" {
		return reqBucket
	}
	return s.defaultBucket
}

// SignURL returns a SignedURL for the requested operation. Only GET and
// HEAD are supported; PUT/POST/DELETE return ErrUnsupported{CapSignedURLWrite}
// per provider_matrix.md footnote 4 (write authorization is non-URL via
// IssueDirectGrant).
func (s signerService) SignURL(_ context.Context, req uos.SignURLRequest) (*uos.SignedURL, error) {
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
	case http.MethodGet, http.MethodHead:
		// Read path: handled below via MakePrivateURL.
	case http.MethodPut, http.MethodPost, http.MethodDelete:
		return nil, &uos.Error{
			Code: uos.ErrUnsupported, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Capability: capability.CapSignedURLWrite,
			Message:    "qiniu write authorization is non-URL; use IssueDirectGrant (Mode=DirectGrantModeToken)",
		}
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q (allowed: GET, HEAD; PUT/POST via IssueDirectGrant)", method),
		}
	}
	if s.d.dc.Domain == "" {
		return nil, &uos.Error{
			Code: uos.ErrUnsupported, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Capability: capability.CapSignedURLRead,
			Message:    "qiniu SignURL requires DriverConfig.Domain (the bucket's bound CDN/source domain)",
		}
	}
	deadline := time.Now().Add(req.ExpiresIn).Unix()
	url := storage.MakePrivateURL(s.d.mac, s.d.dc.Domain, req.Key, deadline)
	return &uos.SignedURL{
		URL:       url,
		Method:    method,
		ExpiresAt: time.Unix(deadline, 0).UTC(),
	}, nil
}

// IssueDirectGrant returns a DirectGrant carrying either a Qiniu Upload
// Token (Operation=upload) or a Qiniu Download Token / signed download URL
// (Operation=download). Both shapes use Mode=DirectGrantModeToken.
//
// **M5 validation**: this is the milestone moment for DirectGrantModeToken
// in a NEW context. The Upload Token is an opaque bearer string the caller
// POSTs to the upload host as the "token" field of a multipart form — it
// is not a URL, not a form-fields collection by itself, and not a set of
// custom headers on a separate URL. DirectGrantModeToken fits semantically
// because the caller carries Token and POSTs it to URL with Method=POST.
//
// For Download (Operation=download), the Token is the signed download URL
// itself (a URL-shaped bearer the caller GETs directly), with URL set to
// the same value for callers that prefer to read DirectGrant.URL. This is
// a pragmatic encoding decision: Qiniu's "Download Token" is technically
// a private URL signature, and surfacing it as Mode=Token (rather than
// Mode=URL) keeps both upload and download grants under a single dispatch
// shape on the qiniu driver — see Lessons (M5).
//
// req.Extra recognised keys (forwarded into the PutPolicy for upload):
//
//   - "callbackUrl"     → PutPolicy.CallbackURL
//   - "callbackBody"    → PutPolicy.CallbackBody
//   - "callbackHost"    → PutPolicy.CallbackHost
//   - "callbackBodyType"→ PutPolicy.CallbackBodyType
//   - "returnBody"      → PutPolicy.ReturnBody
//   - "returnUrl"       → PutPolicy.ReturnURL
//   - "saveKey"         → PutPolicy.SaveKey
//   - "persistentOps"   → PutPolicy.PersistentOps
func (s signerService) IssueDirectGrant(_ context.Context, req uos.DirectGrantRequest) (*uos.DirectGrant, error) {
	const op = "IssueDirectGrant"
	bucket := s.pickBucket(req.Bucket)
	if req.ExpiresIn <= 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "ExpiresIn must be > 0",
		}
	}
	expires := time.Now().Add(req.ExpiresIn).UTC()

	switch req.Operation {
	case uos.DirectGrantUpload, "":
		// Build the PutPolicy. Scope is "<bucket>" if Key is empty, else
		// "<bucket>:<key>" so the token is bound to the target object.
		scope := bucket
		if req.Key != "" {
			scope = bucket + ":" + req.Key
		}
		policy := storage.PutPolicy{
			Scope:      scope,
			Expires:    uint64(req.ExpiresIn.Seconds()),
			FsizeLimit: req.MaxBytes,
			MimeLimit:  req.ContentType,
		}
		// Honor caller-supplied vendor extension fields via req.Extra.
		if v, ok := req.Extra["callbackUrl"]; ok {
			policy.CallbackURL = v
		}
		if v, ok := req.Extra["callbackBody"]; ok {
			policy.CallbackBody = v
		}
		if v, ok := req.Extra["callbackHost"]; ok {
			policy.CallbackHost = v
		}
		if v, ok := req.Extra["callbackBodyType"]; ok {
			policy.CallbackBodyType = v
		}
		if v, ok := req.Extra["returnBody"]; ok {
			policy.ReturnBody = v
		}
		if v, ok := req.Extra["returnUrl"]; ok {
			policy.ReturnURL = v
		}
		if v, ok := req.Extra["saveKey"]; ok {
			policy.SaveKey = v
		}
		if v, ok := req.Extra["persistentOps"]; ok {
			policy.PersistentOps = v
		}

		token := policy.UploadToken(s.d.mac)
		uploadHost := s.d.uploadHost(bucket)
		if uploadHost == "" {
			uploadHost = "https://upload.qiniup.com" // generic fallback; SDK resolves per-region in practice
		}
		// Qiniu form-style upload uses POST (Content-Type: multipart/form-data)
		// with the token in the form field "token" — see
		// https://developer.qiniu.com/kodo/manual/1272/form-upload.
		return &uos.DirectGrant{
			Mode:      uos.DirectGrantModeToken,
			URL:       uploadHost,
			Method:    http.MethodPost,
			Token:     token,
			ExpiresAt: expires,
		}, nil

	case uos.DirectGrantDownload:
		if s.d.dc.Domain == "" {
			return nil, &uos.Error{
				Code: uos.ErrUnsupported, Provider: s.d.Provider(), Operation: op,
				Bucket: bucket, Key: req.Key,
				Capability: capability.CapDirectGrant,
				Message:    "qiniu DirectGrant download requires DriverConfig.Domain (the bucket's bound CDN/source domain)",
			}
		}
		deadline := expires.Unix()
		signedURL := storage.MakePrivateURL(s.d.mac, s.d.dc.Domain, req.Key, deadline)
		// v0.1.1: Mode=URL is the architecturally honest encoding for
		// Qiniu's URL-shaped Download Token; the M5 ship used Mode=Token
		// for cross-operation dispatch symmetry, but Mode=URL tells the
		// truth (callers GET DirectGrant.URL directly; no opaque bearer
		// token semantics apply).
		return &uos.DirectGrant{
			Mode:      uos.DirectGrantModeURL,
			URL:       signedURL,
			Method:    http.MethodGet,
			ExpiresAt: expires,
		}, nil

	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unknown DirectGrantOperation %q (recognised: \"upload\", \"download\")", string(req.Operation)),
		}
	}
}

// ----------------------------------------------------------------------
// Translation helpers
// ----------------------------------------------------------------------

// buildPutParams builds the FormUploader/PutPolicy params map from the
// caller-supplied unified Metadata. Qiniu wire convention:
//
//   - Keys with prefix "x-qn-meta-" carry user metadata (case-folded
//     to lower per s3common).
//   - Keys with prefix "x:" are arbitrary user variables (forwarded
//     verbatim through PutPolicy / RputV2Extra.CustomVars).
//
// The unified Metadata map is treated as user metadata; the driver prefixes
// keys with "x-qn-meta-" if they don't already carry one of the recognised
// prefixes. Values that fail the validity check (empty value) are dropped
// at SDK time.
func buildPutParams(m uos.Metadata) map[string]string {
	lower := s3common.LowerMetadataKeys(m)
	if len(lower) == 0 {
		return nil
	}
	out := make(map[string]string, len(lower))
	for k, v := range lower {
		switch {
		case strings.HasPrefix(k, "x-qn-meta-"), strings.HasPrefix(k, "x:"):
			out[k] = v
		default:
			out["x-qn-meta-"+k] = v
		}
	}
	return out
}

// filterMetaPrefix returns only the x-qn-meta-* entries from a params map
// (used for RputV2Extra.Metadata which is a separate field from CustomVars).
// Values are kept under their fully-qualified keys.
func filterMetaPrefix(params map[string]string) map[string]string {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		if strings.HasPrefix(k, "x-qn-meta-") {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// filterCustomVarsPrefix returns only the x:* entries from a params map.
func filterCustomVarsPrefix(params map[string]string) map[string]string {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		if strings.HasPrefix(k, "x:") {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// translateFileInfo converts a storage.FileInfo (as returned by Stat) to
// the unified ObjectInfo. The PutTime field is in 100-nanosecond units
// (Windows FILETIME convention); we convert via putTimeToTime.
func translateFileInfo(bucket, key string, fi storage.FileInfo) uos.ObjectInfo {
	info := uos.ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		Size:         fi.Fsize,
		ETag:         fi.Hash,
		LastModified: putTimeToTime(fi.PutTime),
		Content: uos.ContentHeaders{
			ContentType: fi.MimeType,
		},
		Metadata: extractFileInfoMetadata(fi.MetaData),
	}
	if fi.Md5 != "" {
		info.Checksum = uos.Checksum{Type: "md5", Value: []byte(fi.Md5)}
	}
	return info
}

// translateListItem converts a storage.ListItem to the unified ObjectInfo.
func translateListItem(bucket string, it storage.ListItem) uos.ObjectInfo {
	return uos.ObjectInfo{
		Bucket:       bucket,
		Key:          it.Key,
		Size:         it.Fsize,
		ETag:         it.Hash,
		LastModified: putTimeToTime(it.PutTime),
		Content:      uos.ContentHeaders{ContentType: it.MimeType},
	}
}

// extractFileInfoMetadata folds Qiniu's MetaData map (already with
// x-qn-meta-* keys stripped by the SDK) to lower-case keys with prefix
// preserved per the unified contract.
func extractFileInfoMetadata(m map[string]string) uos.Metadata {
	if len(m) == 0 {
		return nil
	}
	out := make(uos.Metadata, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// extractQiniuMetadata folds the response's x-qn-meta-* headers into the
// unified Metadata. Lower-cased keys; "x-qn-meta-" prefix preserved so
// callers can round-trip on Put.
func extractQiniuMetadata(h http.Header) uos.Metadata {
	out := uos.Metadata{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-qn-meta-") && len(vs) > 0 {
			out[lk] = vs[0]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// putTimeToTime converts Qiniu's PutTime (100-nanosecond intervals since
// the Unix epoch) to time.Time. PutTime in the SDK has a high-bit-shifted
// encoding; the canonical conversion is "PutTime / 1e7" yields Unix seconds.
func putTimeToTime(putTime int64) time.Time {
	if putTime == 0 {
		return time.Time{}
	}
	return time.Unix(putTime/1e7, (putTime%1e7)*100).UTC()
}

// parseLastModified parses an HTTP Last-Modified header into time.Time.
// Returns the zero value on parse failure (treated as "unknown").
func parseLastModified(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := http.ParseTime(s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// buildRangeHeader formats a uos.ByteRange as the HTTP Range header value.
// End=-1 means "to EOF", encoded as "bytes=Start-".
func buildRangeHeader(r uos.ByteRange) string {
	if r.End < 0 {
		return fmt.Sprintf("bytes=%d-", r.Start)
	}
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
}

// qiniuStatusError synthesises a *qclient.ErrorInfo-shaped error so the
// HTTP-level Get path can route through mapError uniformly. We don't import
// the SDK's parseError because it requires a live response Body; for
// non-2xx GET responses we already drained the body and only have the
// status code + optional message.
type qiniuStatusError struct {
	status int
	msg    string
}

func (e *qiniuStatusError) Error() string {
	if e.msg != "" {
		return fmt.Sprintf("qiniu http %d: %s", e.status, e.msg)
	}
	return fmt.Sprintf("qiniu http %d", e.status)
}

// HTTPStatus exposes the status code so the error mapper can fall back
// to s3common.MapHTTPStatus when no reason is recognised.
func (e *qiniuStatusError) HTTPStatus() int { return e.status }

// Compile-time assertion that driverImpl satisfies the full uos.Client
// surface plus the four sub-services.
var (
	_ uos.Client           = (*driverImpl)(nil)
	_ uos.BucketService    = bucketService{}
	_ uos.ObjectService    = objectService{}
	_ uos.MultipartService = multipartService{}
	_ uos.Signer           = signerService{}
)
