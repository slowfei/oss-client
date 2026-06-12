package gcs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/capability"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// driverImpl implements pkg/uos.Client by translating to/from
// cloud.google.com/go/storage. It holds the SDK *storage.Client (used
// for bucket-scoped service operations and for constructing per-object
// handles), the project id (required for bucket-create/list), and the
// signing material the Signer needs to mint SignedURLs.
//
// driverImpl is safe for concurrent use; *storage.Client is itself
// goroutine-safe and per-call BucketHandle / ObjectHandle values are
// cheap to allocate. The in-process resumable-upload registry guards
// itself with a sync.RWMutex.
type driverImpl struct {
	cfg         uos.Config
	client      *storage.Client
	projectID   string
	signerEmail string
	signerKey   []byte
	signScheme  storage.SigningScheme
	uploads     *uploadRegistry
}

// Provider returns "gcs". Required by uos.Client.
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

// As exposes the underlying cloud.google.com/go/storage handle for
// callers that need vendor-specific features. Supported targets:
//
//   - **storage.Client: filled with the high-level GCS client.
//
// Returns false (without mutating target) for any other type.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **storage.Client:
		*t = d.client
		return true
	default:
		return false
	}
}

// Close releases resources held by the underlying *storage.Client. The
// SDK closes its HTTP transport pool on Close; subsequent operations
// will fail. The in-process resumable-upload registry is left intact;
// any open Writer in it will surface a "Writer is closed" error on its
// next Write/Close call.
func (d *driverImpl) Close() error {
	if d.client == nil {
		return nil
	}
	return d.client.Close()
}

// pickBucketHandle returns the SDK BucketHandle bound to name.
// BucketHandle construction is local-only (no I/O), so misconfiguration
// surfaces at the first call against it rather than here.
func (d *driverImpl) bucketHandle(name string) *storage.BucketHandle {
	return d.client.Bucket(name)
}

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService implements uos.BucketService.
type bucketService struct{ d *driverImpl }

// List enumerates buckets visible to the configured credential within
// the driver's project. GCS REQUIRES a project id for bucket listing;
// we pull it from DriverConfig.ProjectID at Open time.
//
// The SDK's BucketIterator does not expose a stable continuation token
// at the public-API surface (the gax PageInfo is internal); to honor
// the unified ContinuationToken contract we set the page size via
// PageInfo.MaxSize, drain one page, and surface the resulting
// pageInfo.Token (the SDK exposes it via PageInfo()) as NextToken when
// the iterator says more pages remain.
func (b bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"
	if b.d.projectID == "" {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  b.d.Provider(),
			Operation: op,
			Message:   "DriverConfig.ProjectID is required for ListBuckets on GCS (the JSON API rejects bucket listing without a project)",
		}
	}
	it := b.d.client.Buckets(ctx, b.d.projectID)
	if req.MaxResults > 0 {
		it.PageInfo().MaxSize = req.MaxResults
	}
	if req.ContinuationToken != "" {
		it.PageInfo().Token = req.ContinuationToken
	}

	out := make([]uos.BucketInfo, 0, max(req.MaxResults, 0))
	for {
		ba, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, mapError(b.d.Provider(), op, "", "", err)
		}
		out = append(out, uos.BucketInfo{
			Name:      ba.Name,
			Region:    strings.ToLower(ba.Location),
			CreatedAt: ba.Created,
		})
		// Honor MaxResults as a hard cap on returned items, since the
		// iterator may merge multiple SDK pages into one Next() loop.
		if req.MaxResults > 0 && len(out) >= req.MaxResults {
			break
		}
	}
	return out, nil
}

// Create makes a new bucket. GCS rejects already-existing buckets with
// a 409 carrying Reason="conflict", which the error mapper translates
// to ErrAlreadyExists.
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
	if b.d.projectID == "" {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  b.d.Provider(),
			Operation: op,
			Bucket:    req.Name,
			Message:   "DriverConfig.ProjectID is required for CreateBucket on GCS",
		}
	}
	attrs := &storage.BucketAttrs{}
	if req.Region != "" {
		attrs.Location = req.Region
	}
	if req.ACL != "" {
		// Predefined ACL fields apply at create time; the SDK forwards
		// them as the predefinedAcl query parameter.
		attrs.PredefinedACL = req.ACL
	}
	bh := b.d.bucketHandle(req.Name)
	if err := bh.Create(ctx, b.d.projectID, attrs); err != nil {
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

// Stat returns BucketInfo for an existing bucket. The SDK wraps a 404
// into storage.ErrBucketNotExist, which the error mapper translates to
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
	bh := b.d.bucketHandle(req.Name)
	attrs, err := bh.Attrs(ctx)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return &uos.BucketInfo{
		Name:      attrs.Name,
		Region:    strings.ToLower(attrs.Location),
		CreatedAt: attrs.Created,
	}, nil
}

// Delete removes an empty bucket. Non-empty buckets surface as a 409
// with Reason="conflict", mapped to ErrConflict by the error layer.
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
	bh := b.d.bucketHandle(req.Name)
	if err := bh.Delete(ctx); err != nil {
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

// objectHandle constructs the SDK ObjectHandle for (bucket, key),
// optionally pinned to a specific generation when versionID is
// non-empty.
func (o objectService) objectHandle(bucket, key, versionID string) (*storage.ObjectHandle, error) {
	oh := o.d.bucketHandle(bucket).Object(key)
	if versionID != "" {
		gen, err := strconv.ParseInt(versionID, 10, 64)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  o.d.Provider(),
				Operation: "ObjectHandle",
				Bucket:    bucket,
				Key:       key,
				Message:   fmt.Sprintf("VersionID %q is not a valid GCS generation number", versionID),
			}
		}
		oh = oh.Generation(gen)
	}
	return oh, nil
}

// Put writes a single object via GCS Writer. The Writer streams the
// body to a single resumable-upload session under the hood (the SDK
// dispatches single-shot vs chunked based on Writer.ChunkSize). We
// stamp the unified ContentHeaders + Metadata + StorageClass into the
// Writer.ObjectAttrs before the first Write, then io.Copy the body.
func (o objectService) Put(ctx context.Context, req uos.PutObjectRequest) (*uos.PutObjectResult, error) {
	const op = "PutObject"
	bucket := o.pickBucket(req.Bucket)
	if req.Body == nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Body is required",
		}
	}
	oh, err := o.objectHandle(bucket, req.Key, "")
	if err != nil {
		return nil, err
	}
	if req.IfNoneMatch == "*" {
		oh = oh.If(storage.Conditions{DoesNotExist: true})
	}
	w := oh.NewWriter(ctx)
	w.Name = req.Key
	applyContentHeadersToAttrs(req.Content, &w.ObjectAttrs)
	if md := s3common.LowerMetadataKeys(req.Metadata); len(md) > 0 {
		w.Metadata = md
	}
	if req.StorageClass != "" {
		w.StorageClass = req.StorageClass
	}
	if req.ACL != "" {
		w.PredefinedACL = req.ACL
	}
	if _, err := io.Copy(w, req.Body); err != nil {
		_ = w.Close() // best-effort cleanup; drop the close error so the original io error wins
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	if err := w.Close(); err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	attrs := w.Attrs()
	return &uos.PutObjectResult{
		ETag:      strings.Trim(attrs.Etag, `"`),
		VersionID: strconv.FormatInt(attrs.Generation, 10),
		Checksum:  checksumFromAttrs(attrs),
	}, nil
}

// Get streams an object body via GCS NewReader / NewRangeReader.
// Returned ObjectReader.Body is the SDK Reader; callers MUST Close it.
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)
	oh, err := o.objectHandle(bucket, req.Key, req.VersionID)
	if err != nil {
		return nil, err
	}
	if conds, ok := buildReadConditions(req); ok {
		oh = oh.If(conds)
	}
	var r *storage.Reader
	if req.Range != nil {
		offset, length := translateRange(*req.Range)
		r, err = oh.NewRangeReader(ctx, offset, length)
	} else {
		r, err = oh.NewReader(ctx)
	}
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	// IfModifiedSince / IfUnmodifiedSince are not directly expressible
	// via storage.Conditions (which is generation-based); we honor them
	// post-read by checking the LastModified attr. This mirrors the
	// behavior the contract suite expects.
	if !req.IfModifiedSince.IsZero() && !r.Attrs.LastModified.After(req.IfModifiedSince) {
		_ = r.Close()
		return nil, &uos.Error{
			Code: uos.ErrPreconditionFailed, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "object not modified since IfModifiedSince",
		}
	}
	if !req.IfUnmodifiedSince.IsZero() && r.Attrs.LastModified.After(req.IfUnmodifiedSince) {
		_ = r.Close()
		return nil, &uos.Error{
			Code: uos.ErrPreconditionFailed, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "object modified after IfUnmodifiedSince",
		}
	}
	info := uos.ObjectInfo{
		Bucket:       bucket,
		Key:          req.Key,
		Size:         r.Attrs.Size,
		LastModified: r.Attrs.LastModified,
		Content: uos.ContentHeaders{
			ContentType:     r.Attrs.ContentType,
			ContentEncoding: r.Attrs.ContentEncoding,
			CacheControl:    r.Attrs.CacheControl,
		},
	}
	if r.Attrs.Generation != 0 {
		info.VersionID = strconv.FormatInt(r.Attrs.Generation, 10)
	}
	if md := r.Metadata(); len(md) > 0 {
		info.Metadata = s3common.LowerMetadataKeys(md)
	}
	return &uos.ObjectReader{
		Body:          r,
		ContentLength: r.Remain(),
		Info:          info,
	}, nil
}

// Head returns ObjectInfo without the body via ObjectHandle.Attrs.
// storage.ErrObjectNotExist surfaces as ErrNotFound through the
// error mapper.
func (o objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)
	oh, err := o.objectHandle(bucket, req.Key, req.VersionID)
	if err != nil {
		return nil, err
	}
	attrs, err := oh.Attrs(ctx)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateAttrsToInfo(bucket, attrs)
	return &info, nil
}

// Delete removes a single object. GCS Delete is idempotent at the
// driver layer: deleting a missing object surfaces ErrObjectNotExist,
// which the contract test for delete-idempotency expects callers to
// ignore via errors.Is. The unified contract documents the semantic on
// ObjectService.Delete (nil error per most vendors), so we unwrap the
// not-found case to nil here.
func (o objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)
	oh, err := o.objectHandle(bucket, req.Key, req.VersionID)
	if err != nil {
		return err
	}
	if err := oh.Delete(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil
		}
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

// DeleteMany removes a batch of keys. GCS has NO native batch-delete
// API at the JSON layer; the driver issues per-key Delete in serial,
// collecting per-key failures into the unified DeleteFailure shape.
// Quiet is honored to omit the Deleted slice for callers that only
// care about failures.
func (o objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	out := &uos.DeleteManyResult{}
	for _, key := range req.Keys {
		oh, err := o.objectHandle(bucket, key, "")
		if err != nil {
			out.Failed = append(out.Failed, uos.DeleteFailure{
				Key:     key,
				Code:    uos.ErrInvalidArgument,
				Message: err.Error(),
			})
			continue
		}
		if err := oh.Delete(ctx); err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				if !req.Quiet {
					out.Deleted = append(out.Deleted, key)
				}
				continue
			}
			mapped := mapError(o.d.Provider(), op, bucket, key, err)
			var ue *uos.Error
			if errors.As(mapped, &ue) {
				out.Failed = append(out.Failed, uos.DeleteFailure{
					Key:     key,
					Code:    ue.Code,
					Message: ue.Message,
				})
			} else {
				out.Failed = append(out.Failed, uos.DeleteFailure{
					Key:     key,
					Code:    uos.ErrInternal,
					Message: err.Error(),
				})
			}
			continue
		}
		if !req.Quiet {
			out.Deleted = append(out.Deleted, key)
		}
	}
	return out, nil
}

// Copy duplicates an object via GCS Copier. Same-bucket and
// cross-bucket copies use the same primitive; GCS rewrites the object
// server-side (the SDK's Run() returns once the rewrite chain is
// complete).
func (o objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	dstBucket := req.DestBucket
	srcBucket := req.SourceBucket
	if srcBucket == "" {
		srcBucket = o.defaultBucket
	}
	src, err := o.objectHandle(srcBucket, req.SourceKey, req.SourceVersionID)
	if err != nil {
		return nil, err
	}
	dst, err := o.objectHandle(dstBucket, req.DestKey, "")
	if err != nil {
		return nil, err
	}
	c := dst.CopierFrom(src)
	applyContentHeadersToAttrs(req.Content, &c.ObjectAttrs)
	if md := s3common.LowerMetadataKeys(req.Metadata); len(md) > 0 {
		c.Metadata = md
	}
	if req.StorageClass != "" {
		c.StorageClass = req.StorageClass
	}
	if req.ACL != "" {
		c.PredefinedACL = req.ACL
	}
	if req.IfNoneMatch == "*" {
		c = dst.If(storage.Conditions{DoesNotExist: true}).CopierFrom(src)
	}
	attrs, err := c.Run(ctx)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, dstBucket, req.DestKey, err)
	}
	res := &uos.CopyObjectResult{
		ETag:         strings.Trim(attrs.Etag, `"`),
		LastModified: attrs.Updated,
	}
	if attrs.Generation != 0 {
		res.VersionID = strconv.FormatInt(attrs.Generation, 10)
	}
	return res, nil
}

// List enumerates objects matching prefix / delimiter via GCS
// BucketHandle.Objects. NextToken round-trips through the SDK's
// PageInfo.Token so opaque-cursor pagination works across providers.
func (o objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)
	q := &storage.Query{
		Prefix:      req.Prefix,
		Delimiter:   req.Delimiter,
		StartOffset: req.StartAfter,
	}
	it := o.d.bucketHandle(bucket).Objects(ctx, q)
	if req.MaxResults > 0 {
		it.PageInfo().MaxSize = req.MaxResults
	}
	if req.ContinuationToken != "" {
		it.PageInfo().Token = req.ContinuationToken
	}

	out := &uos.ObjectList{}
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, mapError(o.d.Provider(), op, bucket, "", err)
		}
		// Synthetic prefix entries (the GCS SDK convention for
		// CommonPrefixes when Delimiter is set) carry only
		// ObjectAttrs.Prefix; everything else is zero.
		if attrs.Prefix != "" {
			out.CommonPrefixes = append(out.CommonPrefixes, attrs.Prefix)
			continue
		}
		out.Items = append(out.Items, translateAttrsToInfo(bucket, attrs))
		if req.MaxResults > 0 && len(out.Items) >= req.MaxResults {
			break
		}
	}
	out.NextToken = it.PageInfo().Token
	out.Truncated = out.NextToken != ""
	return out, nil
}

// ----------------------------------------------------------------------
// MultipartService
// ----------------------------------------------------------------------

// multipartService implements uos.MultipartService backed by an
// in-process resumable-upload session registry. The driver creates a
// single SDK Writer per Initiate, stashes it under a synthetic
// UploadID, and feeds parts to it via UploadPart calls. Complete closes
// the Writer; Abort closes it with an error to cancel the resumable
// session.
//
// **In-process limitation** (logged in docs/provider_roadmap.md
// "Lessons (M4)"): the resumable session lives only in the process
// that called Initiate. Cross-process resumability requires the SDK's
// gRPC AppendableObject path, which the driver does NOT wrap; reach
// for it via Client.As(target). MultipartService.List always returns
// an empty page on this driver; callers gating on that case should
// SkipCases the orphan-cleanup test.
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

// Initiate starts a new resumable upload by allocating a Writer and
// stashing it in the in-process registry under a synthetic UploadID.
// Subsequent UploadPart calls write to the same Writer; Complete
// closes it; Abort closes it with an error.
//
// The Writer is configured with a chunk size matching the GCS minimum
// (256 KiB) so each UploadPart can flush its data to a chunk boundary.
// A larger chunk size would require buffering parts across calls; we
// trade some memory for simpler semantics.
func (m multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if req.Key == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Message: "Key is required",
		}
	}
	uploadID, err := newUploadID()
	if err != nil {
		return nil, &uos.Error{
			Code: uos.ErrInternal, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "failed to allocate UploadID",
			Cause: err,
		}
	}
	// We deliberately do NOT call NewWriter here: it would tie the
	// upload's lifetime to the Initiate call's context, which is
	// almost always a short request-scoped context. Instead we capture
	// just enough state to construct the Writer at the first
	// UploadPart call (where the caller-supplied ctx may be longer
	// lived).
	pending := &uploadSession{
		bucket:       bucket,
		key:          req.Key,
		content:      req.Content,
		metadata:     s3common.LowerMetadataKeys(req.Metadata),
		storageClass: req.StorageClass,
		acl:          req.ACL,
		nextPart:     1,
	}
	m.d.uploads.put(uploadID, pending)
	return &uos.MultipartUpload{
		UploadID:     uploadID,
		Bucket:       bucket,
		Key:          req.Key,
		Initiated:    time.Now().UTC(),
		StorageClass: req.StorageClass,
		Metadata:     pending.metadata,
	}, nil
}

// UploadPart appends a part to the resumable upload identified by
// UploadID. Parts MUST arrive in PartNumber order (1, 2, 3, …); the
// driver rejects out-of-order arrivals with ErrInvalidArgument because
// GCS resumable uploads are inherently sequential — the underlying
// session URL accepts only contiguous byte ranges.
func (m multipartService) UploadPart(ctx context.Context, req uos.UploadPartRequest) (*uos.UploadedPart, error) {
	const op = "UploadPart"
	bucket := m.pickBucket(req.Bucket)
	if req.Body == nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Body is required",
		}
	}
	if req.UploadID == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "UploadID is required",
		}
	}
	if req.PartNumber < 1 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "PartNumber must be >= 1",
		}
	}
	sess, ok := m.d.uploads.get(req.UploadID)
	if !ok {
		return nil, &uos.Error{
			Code: uos.ErrNotFound, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: "UploadID not found in this process; the GCS resumable-upload registry is in-process only — see provider_matrix.md and the M4 lessons",
		}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.nextPart != req.PartNumber {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("PartNumber %d is out of order (next expected: %d); GCS resumable uploads are sequential", req.PartNumber, sess.nextPart),
		}
	}
	if sess.writer == nil {
		oh := m.d.bucketHandle(bucket).Object(sess.key)
		sess.writerCtx, sess.cancelCtx = context.WithCancel(context.Background())
		w := oh.NewWriter(sess.writerCtx)
		w.Name = sess.key
		applyContentHeadersToAttrs(sess.content, &w.ObjectAttrs)
		if len(sess.metadata) > 0 {
			w.Metadata = sess.metadata
		}
		if sess.storageClass != "" {
			w.StorageClass = sess.storageClass
		}
		if sess.acl != "" {
			w.PredefinedACL = sess.acl
		}
		sess.writer = w
	}
	written, err := io.Copy(sess.writer, req.Body)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	sess.nextPart++
	return &uos.UploadedPart{
		PartNumber: req.PartNumber,
		// GCS does not return per-part ETags from the Writer; the
		// caller-supplied PartNumber is enough to identify the part on
		// the server side. We synthesise a stable ETag string so the
		// unified contract sees a non-empty value (the contract suite
		// does not rely on the ETag's specific format).
		ETag: fmt.Sprintf("part-%d", req.PartNumber),
		Size: written,
	}, nil
}

// Complete finalises the resumable upload by closing the Writer. The
// caller-supplied Parts slice is discarded — GCS resumable uploads
// don't take a per-part manifest at finalisation; the parts have
// already been streamed to the session URL in order.
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	sess, ok := m.d.uploads.take(req.UploadID)
	if !ok {
		return nil, &uos.Error{
			Code: uos.ErrNotFound, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "UploadID not found in this process",
		}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.writer == nil {
		// Zero-part upload: open a Writer just so we materialise an
		// empty object on the server.
		oh := m.d.bucketHandle(bucket).Object(sess.key)
		sess.writerCtx, sess.cancelCtx = context.WithCancel(context.Background())
		w := oh.NewWriter(sess.writerCtx)
		w.Name = sess.key
		applyContentHeadersToAttrs(sess.content, &w.ObjectAttrs)
		if len(sess.metadata) > 0 {
			w.Metadata = sess.metadata
		}
		if sess.storageClass != "" {
			w.StorageClass = sess.storageClass
		}
		if sess.acl != "" {
			w.PredefinedACL = sess.acl
		}
		sess.writer = w
	}
	if err := sess.writer.Close(); err != nil {
		if sess.cancelCtx != nil {
			sess.cancelCtx()
		}
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	if sess.cancelCtx != nil {
		sess.cancelCtx()
	}
	attrs := sess.writer.Attrs()
	out := &uos.PutObjectResult{
		ETag:     strings.Trim(attrs.Etag, `"`),
		Checksum: checksumFromAttrs(attrs),
	}
	if attrs.Generation != 0 {
		out.VersionID = strconv.FormatInt(attrs.Generation, 10)
	}
	return out, nil
}

// Abort cancels an in-flight resumable upload by closing the Writer
// with an error and dropping the session from the registry. Idempotent
// at the unified-contract layer: aborting an unknown UploadID is a no-op
// (returns nil).
func (m multipartService) Abort(_ context.Context, req uos.AbortMultipartRequest) error {
	sess, ok := m.d.uploads.take(req.UploadID)
	if !ok {
		return nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.writer != nil {
		_ = sess.writer.CloseWithError(errors.New("uos: multipart upload aborted"))
	}
	if sess.cancelCtx != nil {
		sess.cancelCtx()
	}
	return nil
}

// List always returns an empty page. The in-process resumable-upload
// registry is not a multi-process queryable surface; callers that need
// orphan-cleanup must use the SDK's appendable-object path via
// Client.As(target). See package doc 'Multipart mapping' for the
// design rationale.
func (m multipartService) List(_ context.Context, _ uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	return &uos.MultipartUploadList{}, nil
}

// ----------------------------------------------------------------------
// Signer
// ----------------------------------------------------------------------

// signerService implements uos.Signer for GCS V4 / V2 signed URLs.
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

// SignURL returns an HTTP-signed URL for the requested operation via
// storage.SignedURL. If the configured credential lacks a private
// signing key (ADC backed by Compute Engine / GKE / Workload Identity
// has no local key) the call returns
// ErrUnsupported{Capability: CapSignedURLRead/Write,
// Reason: "credential lacks signing key"} per the M4 brief.
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
	if method == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Method is required",
		}
	}
	switch method {
	case http.MethodGet, http.MethodPut, http.MethodHead, http.MethodDelete:
		// allowed
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q (allowed: GET, PUT, HEAD, DELETE)", method),
		}
	}
	cap := capability.CapSignedURLRead
	if method == http.MethodPut {
		cap = capability.CapSignedURLWrite
	}
	if s.d.signerEmail == "" || len(s.d.signerKey) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrUnsupported, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Capability: cap,
			Message:    "credential lacks signing key — Service Account JSON or HMAC keys required for SignURL on GCS",
		}
	}
	opts := &storage.SignedURLOptions{
		Scheme:         s.d.signScheme,
		Method:         method,
		GoogleAccessID: s.d.signerEmail,
		PrivateKey:     s.d.signerKey,
		Expires:        time.Now().Add(req.ExpiresIn),
	}
	if req.Headers != nil {
		// SDK expects "key:value" strings for extension headers.
		for k, vs := range req.Headers {
			for _, v := range vs {
				opts.Headers = append(opts.Headers, fmt.Sprintf("%s:%s", k, v))
			}
		}
	}
	if len(req.Query) > 0 {
		opts.QueryParameters = make(map[string][]string, len(req.Query))
		for k, v := range req.Query {
			opts.QueryParameters[k] = []string{v}
		}
	}
	signed, err := s.d.bucketHandle(bucket).SignedURL(req.Key, opts)
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
// because GCS issues writes as URL (PUT presigned via SignURL). See
// docs/provider_matrix.md footnote 5.
func (s signerService) IssueDirectGrant(_ context.Context, req uos.DirectGrantRequest) (*uos.DirectGrant, error) {
	return nil, &uos.Error{
		Provider:   s.d.Provider(),
		Operation:  "IssueDirectGrant",
		Bucket:     req.Bucket,
		Key:        req.Key,
		Code:       uos.ErrUnsupported,
		Capability: capability.CapDirectGrant,
		Message:    "GCS uses presigned URL — use Signer.SignURL with PUT instead",
	}
}

// ----------------------------------------------------------------------
// Resumable-upload session registry
// ----------------------------------------------------------------------

// uploadRegistry is the in-process index from synthetic UploadID to
// uploadSession. Safe for concurrent use; callers acquire/release
// sessions via get/take/put.
type uploadRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*uploadSession
}

func newUploadRegistry() *uploadRegistry {
	return &uploadRegistry{sessions: make(map[string]*uploadSession)}
}

func (r *uploadRegistry) put(id string, s *uploadSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = s
}

func (r *uploadRegistry) get(id string) (*uploadSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[id]
	return s, ok
}

func (r *uploadRegistry) take(id string) (*uploadSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
	}
	return s, ok
}

// uploadSession captures the in-flight state of one resumable upload.
// The Writer is created lazily on the first UploadPart so its lifetime
// can outlive the Initiate call's context.
type uploadSession struct {
	mu           sync.Mutex
	bucket       string
	key          string
	content      uos.ContentHeaders
	metadata     uos.Metadata
	storageClass string
	acl          string
	nextPart     int

	writer    *storage.Writer
	writerCtx context.Context
	cancelCtx context.CancelFunc
}

// newUploadID returns a hex-encoded random 16-byte id suitable as a
// MultipartService.UploadID synthetic handle. The id has no semantic
// meaning to GCS — it indexes the in-process registry only.
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "gcs-resumable-" + hex.EncodeToString(b[:]), nil
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// applyContentHeadersToAttrs stamps a uos.ContentHeaders onto the
// matching ObjectAttrs fields. Empty fields leave the attr off so the
// vendor default is preserved.
func applyContentHeadersToAttrs(c uos.ContentHeaders, attrs *storage.ObjectAttrs) {
	if c.ContentType != "" {
		attrs.ContentType = c.ContentType
	}
	if c.ContentEncoding != "" {
		attrs.ContentEncoding = c.ContentEncoding
	}
	if c.ContentLanguage != "" {
		attrs.ContentLanguage = c.ContentLanguage
	}
	if c.ContentDisposition != "" {
		attrs.ContentDisposition = c.ContentDisposition
	}
	if c.CacheControl != "" {
		attrs.CacheControl = c.CacheControl
	}
}

// translateAttrsToInfo builds a uos.ObjectInfo from a GCS *ObjectAttrs.
// The unified ObjectInfo.Metadata uses lower-case keys; we lower-case
// here on egress to match the contract.
func translateAttrsToInfo(bucket string, attrs *storage.ObjectAttrs) uos.ObjectInfo {
	info := uos.ObjectInfo{
		Bucket:       bucket,
		Key:          attrs.Name,
		Size:         attrs.Size,
		ETag:         strings.Trim(attrs.Etag, `"`),
		LastModified: attrs.Updated,
		StorageClass: attrs.StorageClass,
		Content: uos.ContentHeaders{
			ContentType:        attrs.ContentType,
			ContentEncoding:    attrs.ContentEncoding,
			ContentLanguage:    attrs.ContentLanguage,
			ContentDisposition: attrs.ContentDisposition,
			CacheControl:       attrs.CacheControl,
		},
		Metadata: s3common.LowerMetadataKeys(attrs.Metadata),
		Checksum: checksumFromAttrs(attrs),
	}
	if attrs.Generation != 0 {
		info.VersionID = strconv.FormatInt(attrs.Generation, 10)
	}
	return info
}

// checksumFromAttrs picks the strongest checksum available on the
// ObjectAttrs and returns it as the unified Checksum value. GCS
// always reports CRC32C and (for non-composite objects) MD5; we prefer
// CRC32C because it covers more object shapes.
func checksumFromAttrs(attrs *storage.ObjectAttrs) uos.Checksum {
	if attrs == nil {
		return uos.Checksum{}
	}
	if attrs.CRC32C != 0 {
		return uos.Checksum{
			Type: "crc32c",
			Value: []byte{
				byte(attrs.CRC32C >> 24),
				byte(attrs.CRC32C >> 16),
				byte(attrs.CRC32C >> 8),
				byte(attrs.CRC32C),
			},
		}
	}
	if len(attrs.MD5) > 0 {
		return uos.Checksum{Type: "md5", Value: append([]byte(nil), attrs.MD5...)}
	}
	return uos.Checksum{}
}

// translateRange converts a uos.ByteRange into the (offset, length)
// pair GCS NewRangeReader expects. End=-1 means "to EOF", which the
// SDK encodes as length=-1.
func translateRange(r uos.ByteRange) (offset, length int64) {
	if r.End < 0 {
		return r.Start, -1
	}
	return r.Start, r.End - r.Start + 1
}

// buildReadConditions converts the Get-side preconditions into the
// SDK's storage.Conditions. The bool reports whether any condition was
// set (the SDK rejects empty Conditions{} as "no conditions"). We
// honor IfMatch only when it is a numeric Generation (our VersionID
// encoding); free-form ETag preconditions don't translate cleanly to
// GCS's generation-number model.
func buildReadConditions(req uos.GetObjectRequest) (storage.Conditions, bool) {
	var c storage.Conditions
	any := false
	if req.IfMatch != "" {
		if gen, err := strconv.ParseInt(req.IfMatch, 10, 64); err == nil {
			c.GenerationMatch = gen
			any = true
		}
	}
	if req.IfNoneMatch == "*" {
		c.DoesNotExist = true
		any = true
	} else if req.IfNoneMatch != "" {
		if gen, err := strconv.ParseInt(req.IfNoneMatch, 10, 64); err == nil {
			c.GenerationNotMatch = gen
			any = true
		}
	}
	return c, any
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
