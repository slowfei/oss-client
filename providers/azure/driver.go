package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/capability"
	"github.com/maqian/object-storage-client/pkg/uos/credential"
	"github.com/maqian/object-storage-client/pkg/uos/s3common"
)

// readSeekCloser wraps any io.Reader + size into an io.ReadSeekCloser so it
// can be passed to Azure SDK methods that require seekable bodies.
// The Azure SDK uses Seek(0, io.SeekEnd) to determine Content-Length when the
// caller does not supply it explicitly; we pre-buffer the body so the size is
// always known.
type readSeekCloser struct {
	*bytes.Reader
}

func (readSeekCloser) Close() error { return nil }

// toReadSeekCloser buffers r into memory (up to size bytes) and returns an
// io.ReadSeekCloser. For bodies where Size is already known the caller can
// pass size as a hint (used only to pre-allocate the buffer).
func toReadSeekCloser(r io.Reader, size int64) (io.ReadSeekCloser, error) {
	var buf []byte
	var err error
	if size > 0 {
		buf = make([]byte, 0, size)
		buf, err = io.ReadAll(io.LimitReader(r, size))
	} else {
		buf, err = io.ReadAll(r)
	}
	if err != nil {
		return nil, fmt.Errorf("azure: buffer body: %w", err)
	}
	return readSeekCloser{bytes.NewReader(buf)}, nil
}

// driverImpl implements pkg/uos.Client backed by the Azure Blob Storage SDK.
//
// Bucket → Container mapping: the Azure object model has Storage Account >
// Container > Blob. The driver maps the unified Bucket concept 1:1 onto
// Azure Containers; the Storage Account is captured once in DriverConfig.
//
// driverImpl is safe for concurrent use; *azblob.Client is goroutine-safe
// and Container/BlockBlob client handles are cheap to derive per request.
type driverImpl struct {
	cfg        uos.Config
	dc         *DriverConfig
	client     *azblob.Client
	authScheme credential.AuthScheme

	// sharedKey is non-nil when authScheme == AuthSharedKey; used by the
	// signer to generate account-key SAS tokens without a network round-trip.
	sharedKey *azblob.SharedKeyCredential

	// tokenCred is non-nil when authScheme == AuthCustom; used by the
	// signer to request user-delegation keys for user-delegation SAS.
	tokenCred azcore.TokenCredential

	// uploadSessions tracks in-flight block-blob multipart uploads so that
	// List and Abort can reference them. Azure Block Blob staging has no
	// server-side "upload ID" concept analogous to S3; we synthesise one.
	uploadMu       sync.Mutex
	uploadSessions map[string]*uploadSession
}

// uploadSession holds the in-flight state for one block-blob multipart upload.
type uploadSession struct {
	bucket    string
	key       string
	uploadID  string
	initiated time.Time
	blockIDs  []string // base64-encoded block IDs in insertion order
}

// Provider returns "azure".
func (d *driverImpl) Provider() uos.Provider { return providerID }

// Capabilities returns the v1-frozen capability.Report for this driver.
func (d *driverImpl) Capabilities(_ context.Context) (capability.Report, error) {
	return capabilities(), nil
}

// Buckets returns the BucketService (Container) view bound to this Client.
func (d *driverImpl) Buckets() uos.BucketService { return bucketService{d: d} }

// Objects returns the ObjectService (Blob) view bound to the named bucket (Container).
func (d *driverImpl) Objects(bucket string) uos.ObjectService {
	return objectService{d: d, defaultBucket: bucket}
}

// Multipart returns the MultipartService (Block Blob staging) view bound to
// the named bucket.
func (d *driverImpl) Multipart(bucket string) uos.MultipartService {
	return multipartService{d: d, defaultBucket: bucket}
}

// Signer returns the Signer view bound to the named bucket.
func (d *driverImpl) Signer(bucket string) uos.Signer {
	return signerService{d: d, defaultBucket: bucket}
}

// As exposes the underlying azblob.Client for vendor-specific operations.
// Supported targets:
//
//   - **azblob.Client: filled with the high-level Azure Blob client.
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **azblob.Client:
		*t = d.client
		return true
	default:
		return false
	}
}

// Close is a no-op: the Azure SDK holds no background goroutines that
// require explicit shutdown. Kept to satisfy uos.Client.
func (d *driverImpl) Close() error { return nil }

// containerClient returns an azblob container client for the named container.
func (d *driverImpl) containerClient(name string) *container.Client {
	return d.client.ServiceClient().NewContainerClient(name)
}

// blockBlobClient returns an azblob block blob client for the named object.
func (d *driverImpl) blockBlobClient(containerName, blobName string) *blockblob.Client {
	return d.client.ServiceClient().NewContainerClient(containerName).NewBlockBlobClient(blobName)
}

// blobClient returns an azblob blob client for the named object.
func (d *driverImpl) blobClient(containerName, blobName string) *blob.Client {
	return d.client.ServiceClient().NewContainerClient(containerName).NewBlobClient(blobName)
}

// serviceClient returns the underlying azblob service client.
func (d *driverImpl) serviceClient() *service.Client {
	return d.client.ServiceClient()
}

// sessionKey builds the map key for upload sessions.
func sessionKey(bucket, key, uploadID string) string {
	return bucket + "\x00" + key + "\x00" + uploadID
}

// storeSession stores an upload session; init the map lazily.
func (d *driverImpl) storeSession(sess *uploadSession) {
	d.uploadMu.Lock()
	defer d.uploadMu.Unlock()
	if d.uploadSessions == nil {
		d.uploadSessions = make(map[string]*uploadSession)
	}
	d.uploadSessions[sessionKey(sess.bucket, sess.key, sess.uploadID)] = sess
}

// loadSession looks up an upload session.
func (d *driverImpl) loadSession(bucket, key, uploadID string) (*uploadSession, bool) {
	d.uploadMu.Lock()
	defer d.uploadMu.Unlock()
	if d.uploadSessions == nil {
		return nil, false
	}
	s, ok := d.uploadSessions[sessionKey(bucket, key, uploadID)]
	return s, ok
}

// deleteSession removes an upload session.
func (d *driverImpl) deleteSession(bucket, key, uploadID string) {
	d.uploadMu.Lock()
	defer d.uploadMu.Unlock()
	if d.uploadSessions != nil {
		delete(d.uploadSessions, sessionKey(bucket, key, uploadID))
	}
}

// listSessions returns all in-flight sessions for a bucket with an optional prefix.
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

// ----------------------------------------------------------------------
// BucketService (Container operations)
// ----------------------------------------------------------------------

// bucketService implements uos.BucketService mapping Bucket → Azure Container.
type bucketService struct{ d *driverImpl }

// List enumerates containers visible to the configured credential.
func (b bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"
	opts := &service.ListContainersOptions{}
	if req.ContinuationToken != "" {
		opts.Marker = &req.ContinuationToken
	}
	if req.MaxResults > 0 {
		n := int32(req.MaxResults) //nolint:gosec
		opts.MaxResults = &n
	}

	pager := b.d.serviceClient().NewListContainersPager(opts)
	var out []uos.BucketInfo
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError(b.d.Provider(), op, "", "", err)
		}
		for _, item := range page.ContainerItems {
			info := uos.BucketInfo{}
			if item.Name != nil {
				info.Name = *item.Name
			}
			if item.Properties != nil && item.Properties.LastModified != nil {
				info.CreatedAt = *item.Properties.LastModified
			}
			out = append(out, info)
		}
	}
	return out, nil
}

// Create provisions a new container. Returns ErrAlreadyExists if it exists.
func (b bucketService) Create(ctx context.Context, req uos.CreateBucketRequest) (*uos.BucketInfo, error) {
	const op = "CreateBucket"
	if req.Name == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: b.d.Provider(), Operation: op,
			Message: "bucket name is required",
		}
	}
	cc := b.d.containerClient(req.Name)
	_, err := cc.Create(ctx, &container.CreateOptions{})
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return &uos.BucketInfo{
		Name:      req.Name,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Stat returns ContainerInfo for an existing container.
func (b bucketService) Stat(ctx context.Context, req uos.StatBucketRequest) (*uos.BucketInfo, error) {
	const op = "StatBucket"
	if req.Name == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: b.d.Provider(), Operation: op,
			Message: "bucket name is required",
		}
	}
	cc := b.d.containerClient(req.Name)
	props, err := cc.GetProperties(ctx, &container.GetPropertiesOptions{})
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}
	info := uos.BucketInfo{Name: req.Name}
	if props.LastModified != nil {
		info.CreatedAt = *props.LastModified
	}
	return &info, nil
}

// Delete removes an empty container.
func (b bucketService) Delete(ctx context.Context, req uos.DeleteBucketRequest) error {
	const op = "DeleteBucket"
	if req.Name == "" {
		return &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: b.d.Provider(), Operation: op,
			Message: "bucket name is required",
		}
	}
	cc := b.d.containerClient(req.Name)
	_, err := cc.Delete(ctx, &container.DeleteOptions{})
	if err != nil {
		return mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return nil
}

// ----------------------------------------------------------------------
// ObjectService (Blob operations)
// ----------------------------------------------------------------------

// objectService implements uos.ObjectService for a fixed container.
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

// Put uploads a blob. Azure requires Content-Length for block blobs; Size
// must be >= 0. Unknown-size uploads (-1) are rejected with ErrLengthRequired.
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
			Message: "Size is required for Azure PutObject; use MultipartService for unknown-size uploads",
		}
	}

	bbc := o.d.blockBlobClient(bucket, req.Key)
	opts := &blockblob.UploadOptions{
		HTTPHeaders: buildBlobHTTPHeaders(req.Content),
		Metadata:    buildMetadataMap(req.Metadata),
	}
	if req.StorageClass != "" {
		tier := blob.AccessTier(req.StorageClass)
		opts.Tier = &tier
	}

	rsc, bufErr := toReadSeekCloser(req.Body, req.Size)
	if bufErr != nil {
		return nil, &uos.Error{
			Code: uos.ErrInternal, Provider: o.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: bufErr.Error(), Cause: bufErr,
		}
	}
	resp, err := bbc.Upload(ctx, rsc, opts)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	result := &uos.PutObjectResult{}
	if resp.ETag != nil {
		result.ETag = strings.Trim(string(*resp.ETag), `"`)
	}
	return result, nil
}

// Get downloads a blob body. Range requests use azblob.HTTPRange.
// VersionID is set via blob.Client.WithVersionID (the options struct does not
// expose it directly in the v1.6.x SDK).
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)
	bc := o.d.blobClient(bucket, req.Key)

	// VersionID is threaded via WithVersionID which returns a new client.
	if req.VersionID != "" {
		var err error
		bc, err = bc.WithVersionID(req.VersionID)
		if err != nil {
			return nil, &uos.Error{
				Code: uos.ErrInvalidArgument, Provider: o.d.Provider(), Operation: op,
				Bucket: bucket, Key: req.Key,
				Message: fmt.Sprintf("invalid VersionID: %v", err), Cause: err,
			}
		}
	}

	opts := &blob.DownloadStreamOptions{}
	if req.Range != nil {
		opts.Range = buildHTTPRange(*req.Range)
	}

	resp, err := bc.DownloadStream(ctx, opts)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}

	info := translateBlobDownloadResponse(bucket, req.Key, resp)
	return &uos.ObjectReader{
		Body:          resp.Body,
		ContentLength: info.Size,
		Info:          info,
	}, nil
}

// Head returns blob metadata without the body.
// VersionID is set via blob.Client.WithVersionID.
func (o objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)
	bc := o.d.blobClient(bucket, req.Key)

	if req.VersionID != "" {
		var err error
		bc, err = bc.WithVersionID(req.VersionID)
		if err != nil {
			return nil, &uos.Error{
				Code: uos.ErrInvalidArgument, Provider: o.d.Provider(), Operation: op,
				Bucket: bucket, Key: req.Key,
				Message: fmt.Sprintf("invalid VersionID: %v", err), Cause: err,
			}
		}
	}

	props, err := bc.GetProperties(ctx, &blob.GetPropertiesOptions{})
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateBlobProperties(bucket, req.Key, props)
	return &info, nil
}

// Delete removes a single blob. Azure DeleteBlob is not strictly idempotent;
// deleting a missing blob returns BlobNotFound which we suppress to nil per
// the contract (idempotent delete).
// VersionID is set via blob.Client.WithVersionID.
func (o objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)
	bc := o.d.blobClient(bucket, req.Key)

	if req.VersionID != "" {
		var err error
		bc, err = bc.WithVersionID(req.VersionID)
		if err != nil {
			return &uos.Error{
				Code: uos.ErrInvalidArgument, Provider: o.d.Provider(), Operation: op,
				Bucket: bucket, Key: req.Key,
				Message: fmt.Sprintf("invalid VersionID: %v", err), Cause: err,
			}
		}
	}

	_, err := bc.Delete(ctx, &blob.DeleteOptions{})
	if err != nil {
		mapped := mapError(o.d.Provider(), op, bucket, req.Key, err)
		var ue *uos.Error
		if errors.As(mapped, &ue) && ue.Code == uos.ErrNotFound {
			return nil // idempotent
		}
		return mapped
	}
	return nil
}

// Exists reports whether a blob exists. Returns (false, nil) for missing blobs.
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

// DeleteMany removes a batch of blobs. Azure has no native batch-delete for
// block blobs in the standard tier; we issue individual deletes and collect
// per-key outcomes.
func (o objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}
	result := &uos.DeleteManyResult{}
	for _, key := range req.Keys {
		bc := o.d.blobClient(bucket, key)
		_, err := bc.Delete(ctx, &blob.DeleteOptions{})
		if err != nil {
			mapped := mapError(o.d.Provider(), op, bucket, key, err)
			var ue *uos.Error
			if errors.As(mapped, &ue) && ue.Code == uos.ErrNotFound {
				// Idempotent: treat as deleted.
				if !req.Quiet {
					result.Deleted = append(result.Deleted, key)
				}
				continue
			}
			var code uos.Code
			if ue != nil {
				code = ue.Code
			} else {
				code = uos.ErrInternal
			}
			result.Failed = append(result.Failed, uos.DeleteFailure{
				Key:     key,
				Code:    code,
				Message: err.Error(),
			})
			continue
		}
		if !req.Quiet {
			result.Deleted = append(result.Deleted, key)
		}
	}
	return result, nil
}

// Copy duplicates a blob within the same storage account using server-side copy.
func (o objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	srcBucket := req.SourceBucket
	if srcBucket == "" {
		srcBucket = o.defaultBucket
	}
	dstBucket := req.DestBucket

	// Build the source URL from the service client.
	srcURL := o.d.serviceClient().URL()
	if !strings.HasSuffix(srcURL, "/") {
		srcURL += "/"
	}
	srcURL = srcURL + srcBucket + "/" + req.SourceKey

	dstBC := o.d.blobClient(dstBucket, req.DestKey)
	opts := &blob.CopyFromURLOptions{
		Metadata: buildMetadataMap(req.Metadata),
	}
	if req.StorageClass != "" {
		tier := blob.AccessTier(req.StorageClass)
		opts.Tier = &tier
	}

	resp, err := dstBC.CopyFromURL(ctx, srcURL, opts)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, dstBucket, req.DestKey, err)
	}
	result := &uos.CopyObjectResult{}
	if resp.ETag != nil {
		result.ETag = strings.Trim(string(*resp.ETag), `"`)
	}
	if resp.LastModified != nil {
		result.LastModified = *resp.LastModified
	}
	return result, nil
}

// List enumerates blobs matching prefix/delimiter using Azure's hierarchical
// list API (ListBlobsHierarchy) when Delimiter is set, or flat list otherwise.
func (o objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)
	cc := o.d.containerClient(bucket)

	out := &uos.ObjectList{}

	if req.Delimiter != "" {
		// Hierarchical listing.
		opts := &container.ListBlobsHierarchyOptions{
			Prefix: strPtr(req.Prefix),
		}
		if req.MaxResults > 0 {
			n := int32(req.MaxResults) //nolint:gosec
			opts.MaxResults = &n
		}
		if req.ContinuationToken != "" {
			opts.Marker = &req.ContinuationToken
		}
		pager := cc.NewListBlobsHierarchyPager(req.Delimiter, opts)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, mapError(o.d.Provider(), op, bucket, "", err)
			}
			for _, item := range page.Segment.BlobItems {
				out.Items = append(out.Items, translateBlobItem(bucket, item))
			}
			for _, prefix := range page.Segment.BlobPrefixes {
				if prefix.Name != nil {
					out.CommonPrefixes = append(out.CommonPrefixes, *prefix.Name)
				}
			}
			if page.NextMarker != nil && *page.NextMarker != "" {
				out.NextToken = *page.NextMarker
				out.Truncated = true
				break // one page at a time
			}
			break
		}
	} else {
		// Flat listing.
		opts := &container.ListBlobsFlatOptions{
			Prefix: strPtr(req.Prefix),
			Include: container.ListBlobsInclude{
				Metadata: true,
			},
		}
		if req.MaxResults > 0 {
			n := int32(req.MaxResults) //nolint:gosec
			opts.MaxResults = &n
		}
		if req.ContinuationToken != "" {
			opts.Marker = &req.ContinuationToken
		}
		pager := cc.NewListBlobsFlatPager(opts)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, mapError(o.d.Provider(), op, bucket, "", err)
			}
			for _, item := range page.Segment.BlobItems {
				out.Items = append(out.Items, translateBlobItem(bucket, item))
			}
			if page.NextMarker != nil && *page.NextMarker != "" {
				out.NextToken = *page.NextMarker
				out.Truncated = true
				break
			}
			break
		}
	}

	return out, nil
}

// ----------------------------------------------------------------------
// MultipartService — Block Blob staging
// ----------------------------------------------------------------------
//
// Azure Block Blob upload maps onto MultipartService as follows:
//
//	Initiate   → allocate an in-process uploadSession with a synthetic uploadID;
//	             no server-side call needed (block staging is stateless until
//	             PutBlockList).
//	UploadPart → PutBlock with a base64-encoded block ID derived from PartNumber.
//	             Minimum block size: 4 MiB (Azure) vs 5 MiB (S3) — see Lessons (M4).
//	Complete   → PutBlockList assembling the block IDs in PartNumber order.
//	Abort      → no server-side abort for uncommitted blocks; they expire after
//	             7 days automatically. We delete the in-process session entry.
//	List       → returns in-process sessions only (no cross-process resume;
//	             see Lessons M4 for rationale).

// multipartService implements uos.MultipartService via Block Blob staging.
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

// Initiate creates a new upload session. No network call is made; Azure
// block staging is stateless until PutBlockList.
func (m multipartService) Initiate(_ context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if req.Key == "" {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Message: "Key is required",
		}
	}
	// Synthesise an upload ID from timestamp + bucket + key (sufficient
	// uniqueness for in-process tracking; not cryptographically random).
	uploadID := fmt.Sprintf("%d-%s-%s", time.Now().UnixNano(), bucket, req.Key)
	sess := &uploadSession{
		bucket:    bucket,
		key:       req.Key,
		uploadID:  uploadID,
		initiated: time.Now().UTC(),
	}
	m.d.storeSession(sess)
	return &uos.MultipartUpload{
		UploadID:     uploadID,
		Bucket:       bucket,
		Key:          req.Key,
		Initiated:    sess.initiated,
		StorageClass: req.StorageClass,
		Metadata:     req.Metadata,
	}, nil
}

// UploadPart stages one block. The block ID is a base64-encoded string
// derived from PartNumber (padded to 6 digits for lexicographic ordering).
// Azure imposes a minimum of 1 byte (no minimum for uncommitted blocks in
// the API, but practical minimum is 1 byte; 4 MiB is the minimum for
// the final committed block in a multi-part upload according to Azure docs).
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

	sess, ok := m.d.loadSession(bucket, req.Key, req.UploadID)
	if !ok {
		return nil, &uos.Error{
			Code: uos.ErrNotFound, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("upload session %q not found; was Initiate called?", req.UploadID),
		}
	}

	// Base64-encode a zero-padded part number as the block ID.
	// Azure requires block IDs within a blob to be the same length.
	blockID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%06d", req.PartNumber)))

	rsc, bufErr := toReadSeekCloser(req.Body, req.Size)
	if bufErr != nil {
		return nil, &uos.Error{
			Code: uos.ErrInternal, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: bufErr.Error(), Cause: bufErr,
		}
	}
	bbc := m.d.blockBlobClient(bucket, req.Key)
	_, err := bbc.StageBlock(ctx, blockID, rsc, &blockblob.StageBlockOptions{})
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}

	// Record the block ID in session order.
	m.d.uploadMu.Lock()
	sess.blockIDs = append(sess.blockIDs, blockID)
	m.d.uploadMu.Unlock()

	return &uos.UploadedPart{
		PartNumber: req.PartNumber,
		ETag:       blockID, // use block ID as ETag proxy for Complete
		Size:       req.Size,
	}, nil
}

// Complete finalises the block blob by calling PutBlockList with the block
// IDs extracted from the Parts list. Parts are expected sorted by PartNumber;
// the block ID is the ETag field set by UploadPart (base64-encoded part
// number). If the caller passes UploadedPart.ETag values from a prior
// UploadPart call those are used directly; otherwise we re-derive them.
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if len(req.Parts) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Parts must be non-empty",
		}
	}

	sess, ok := m.d.loadSession(bucket, req.Key, req.UploadID)
	if !ok {
		return nil, &uos.Error{
			Code: uos.ErrNotFound, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("upload session %q not found", req.UploadID),
		}
	}

	// Build the block list from Parts. If the caller sets ETag to the
	// base64-encoded block ID (as UploadPart does) use it directly;
	// otherwise re-derive from PartNumber.
	blockIDs := make([]string, 0, len(req.Parts))
	for _, p := range req.Parts {
		if p.ETag != "" {
			blockIDs = append(blockIDs, p.ETag)
		} else {
			blockIDs = append(blockIDs, base64.StdEncoding.EncodeToString(
				[]byte(fmt.Sprintf("%06d", p.PartNumber)),
			))
		}
	}

	bbc := m.d.blockBlobClient(bucket, req.Key)
	opts := &blockblob.CommitBlockListOptions{
		HTTPHeaders: buildBlobHTTPHeadersFromMetadata(sess),
		Metadata:    buildMetadataMap(sess.metadata()),
	}
	resp, err := bbc.CommitBlockList(ctx, blockIDs, opts)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}

	m.d.deleteSession(bucket, req.Key, req.UploadID)

	result := &uos.PutObjectResult{}
	if resp.ETag != nil {
		result.ETag = strings.Trim(string(*resp.ETag), `"`)
	}
	if resp.VersionID != nil {
		result.VersionID = *resp.VersionID
	}
	return result, nil
}

// Abort removes the in-process session. Uncommitted Azure blocks expire
// automatically after 7 days — there is no explicit "abort" API for
// uncommitted blocks analogous to S3 AbortMultipartUpload.
func (m multipartService) Abort(_ context.Context, req uos.AbortMultipartRequest) error {
	const op = "AbortMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	m.d.deleteSession(bucket, req.Key, req.UploadID)
	return nil
}

// List returns in-process upload sessions for the bucket. Azure has no
// server-side listing of uncommitted blocks across blobs; this is
// in-process only. Cross-process orphan cleanup is not supported in v1.
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

// metadata returns the metadata stored in the session at Initiate time.
func (s *uploadSession) metadata() uos.Metadata {
	return nil // sessions do not persist metadata in this implementation
}

// ----------------------------------------------------------------------
// Signer — SAS URL and DirectGrant
// ----------------------------------------------------------------------

// signerService implements uos.Signer for Azure SAS.
//
// SAS start-time: Azure SAS tokens carry an optional start time
// (signedstart=). The unified SignURLRequest.ExpiresIn does not expose a
// start-time offset. The driver sets start = now−5 min for clock-skew
// tolerance and expiry = now+ExpiresIn. This is a deliberate compromise
// documented in Lessons (M4) in provider_roadmap.md.
//
// Auth gating: SAS generation requires key material. AuthSharedKey uses
// the account key directly; AuthCustom derives a user-delegation key via
// the Azure REST API (requires UserDelegationKey permission). AuthSAS
// callers lack key material and receive ErrUnsupported{CapSignedURLRead/Write}.
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

// SignURL issues a SAS URL for the requested blob.
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

	perms, err := methodToSASPermissions(method)
	if err != nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: err.Error(),
		}
	}

	// SAS start = now - 5 min (clock-skew tolerance).
	// SAS expiry = now + ExpiresIn.
	// See package doc and Lessons (M4) for rationale.
	now := time.Now().UTC()
	startTime := now.Add(-5 * time.Minute)
	expiryTime := now.Add(req.ExpiresIn)

	sasURL, err := s.buildSASURL(ctx, bucket, req.Key, perms, startTime, expiryTime)
	if err != nil {
		return nil, err
	}
	return &uos.SignedURL{
		URL:       sasURL,
		Method:    method,
		ExpiresAt: expiryTime,
	}, nil
}

// IssueDirectGrant returns a DirectGrant carrying the SAS token as a
// bearer-token string (Mode = DirectGrantModeToken).
//
// The frozen DirectGrantMode values are:
//
//	DirectGrantModeURL     "url"
//	DirectGrantModeForm    "form"
//	DirectGrantModeToken   "token"    ← Azure SAS uses this
//	DirectGrantModeHeaders "headers"
//
// Azure SAS is an opaque query-string token the caller appends to a blob
// URL or passes as the Authorization-alternative via the ?sv=…&sig=…
// query string. DirectGrantModeToken fits semantically because the SAS
// string is an opaque bearer token the caller carries and appends to the
// blob endpoint URL — it is not a URL itself, not form fields, and not
// a set of headers attached to a separate URL. The caller carries Token
// and constructs the final URL as <blob-endpoint>?<Token>.
func (s signerService) IssueDirectGrant(ctx context.Context, req uos.DirectGrantRequest) (*uos.DirectGrant, error) {
	const op = "IssueDirectGrant"
	bucket := s.pickBucket(req.Bucket)
	if req.ExpiresIn <= 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "ExpiresIn must be > 0",
		}
	}

	method := "GET"
	if req.Operation == uos.DirectGrantUpload {
		method = "PUT"
	}
	perms, err := methodToSASPermissions(method)
	if err != nil {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: err.Error(),
		}
	}

	now := time.Now().UTC()
	startTime := now.Add(-5 * time.Minute)
	expiryTime := now.Add(req.ExpiresIn)

	sasURL, err := s.buildSASURL(ctx, bucket, req.Key, perms, startTime, expiryTime)
	if err != nil {
		return nil, err
	}

	// Extract the query string (SAS token) from the full URL.
	sasToken := ""
	if idx := strings.Index(sasURL, "?"); idx >= 0 {
		sasToken = sasURL[idx+1:]
	} else {
		sasToken = sasURL
	}

	// Build the blob base URL (without SAS) for the DirectGrant.URL field.
	blobBaseURL := s.d.serviceClient().URL()
	if !strings.HasSuffix(blobBaseURL, "/") {
		blobBaseURL += "/"
	}
	blobBaseURL += bucket + "/" + req.Key

	headers := http.Header{}
	// x-ms-version is informational; callers may set it on their requests.
	headers.Set("x-ms-version", "2024-11-04")

	return &uos.DirectGrant{
		Mode:      uos.DirectGrantModeToken,
		URL:       blobBaseURL,
		Method:    method,
		Headers:   headers,
		Token:     sasToken,
		ExpiresAt: expiryTime,
	}, nil
}

// buildSASURL generates a SAS URL using either the shared-key or the
// user-delegation path, depending on the driver's auth scheme. AuthSAS
// callers return ErrUnsupported because they already have a SAS and lack
// the key material to issue a new one.
func (s signerService) buildSASURL(
	ctx context.Context,
	bucket, key string,
	perms sas.BlobPermissions,
	startTime, expiryTime time.Time,
) (string, error) {
	const op = "buildSASURL"

	switch s.d.authScheme {
	case credential.AuthSharedKey:
		// Account-key SAS: sign with the shared key credential directly.
		if s.d.sharedKey == nil {
			return "", &uos.Error{
				Code: uos.ErrUnauthenticated, Provider: s.d.Provider(), Operation: op,
				Bucket: bucket, Key: key,
				Message: "shared key credential is nil; cannot generate SAS",
			}
		}
		sasQueryParams, err := sas.BlobSignatureValues{
			Protocol:      sas.ProtocolHTTPS,
			StartTime:     startTime,
			ExpiryTime:    expiryTime,
			ContainerName: bucket,
			BlobName:      key,
			Permissions:   perms.String(),
		}.SignWithSharedKey(s.d.sharedKey)
		if err != nil {
			return "", &uos.Error{
				Code: uos.ErrInternal, Provider: s.d.Provider(), Operation: op,
				Bucket: bucket, Key: key,
				Message: "SAS signing with shared key failed",
				Cause:   err,
			}
		}
		blobURL := s.d.serviceClient().URL()
		if !strings.HasSuffix(blobURL, "/") {
			blobURL += "/"
		}
		blobURL += bucket + "/" + key + "?" + sasQueryParams.Encode()
		return blobURL, nil

	case credential.AuthCustom:
		// User-delegation SAS: obtain a user-delegation key from Azure, then sign.
		if s.d.tokenCred == nil {
			return "", &uos.Error{
				Code: uos.ErrUnsupported, Provider: s.d.Provider(), Operation: op,
				Bucket: bucket, Key: key,
				Capability: capability.CapSignedURLRead,
				Message:    "AuthCustom credential is nil; cannot generate user-delegation SAS",
			}
		}
		svcClient := s.d.serviceClient()
		udkStart := startTime
		udkExpiry := expiryTime.Add(5 * time.Minute) // user-delegation key validity window
		udkResp, err := svcClient.GetUserDelegationCredential(ctx, service.KeyInfo{
			Start:  to(udkStart.UTC().Format(sas.TimeFormat)),
			Expiry: to(udkExpiry.UTC().Format(sas.TimeFormat)),
		}, nil)
		if err != nil {
			return "", mapError(s.d.Provider(), op, bucket, key, err)
		}
		sasQueryParams, err := sas.BlobSignatureValues{
			Protocol:      sas.ProtocolHTTPS,
			StartTime:     startTime,
			ExpiryTime:    expiryTime,
			ContainerName: bucket,
			BlobName:      key,
			Permissions:   perms.String(),
		}.SignWithUserDelegation(udkResp)
		if err != nil {
			return "", &uos.Error{
				Code: uos.ErrInternal, Provider: s.d.Provider(), Operation: op,
				Bucket: bucket, Key: key,
				Message: "SAS signing with user-delegation key failed",
				Cause:   err,
			}
		}
		blobURL := svcClient.URL()
		if !strings.HasSuffix(blobURL, "/") {
			blobURL += "/"
		}
		blobURL += bucket + "/" + key + "?" + sasQueryParams.Encode()
		return blobURL, nil

	case credential.AuthSAS:
		// Pre-formed SAS — no key material available to issue new tokens.
		return "", &uos.Error{
			Code:       uos.ErrUnsupported,
			Provider:   s.d.Provider(),
			Operation:  op,
			Bucket:     bucket,
			Key:        key,
			Capability: capability.CapSignedURLRead,
			Message:    "AuthSAS credential does not carry key material; use AuthSharedKey or AuthCustom to issue new SAS tokens",
		}

	default:
		return "", &uos.Error{
			Code:      uos.ErrUnauthenticated,
			Provider:  s.d.Provider(),
			Operation: op,
			Bucket:    bucket,
			Key:       key,
			Message:   fmt.Sprintf("unknown auth scheme %q for SAS generation", s.d.authScheme),
		}
	}
}

// methodToSASPermissions converts an HTTP method string to the Azure SAS
// BlobPermissions bitmap. Only the four methods supported by the unified
// SignURL surface are mapped.
func methodToSASPermissions(method string) (sas.BlobPermissions, error) {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead:
		return sas.BlobPermissions{Read: true}, nil
	case http.MethodPut:
		return sas.BlobPermissions{Write: true, Create: true}, nil
	case http.MethodDelete:
		return sas.BlobPermissions{Delete: true}, nil
	default:
		return sas.BlobPermissions{}, fmt.Errorf(
			"unsupported SignURL method %q (allowed: GET, HEAD, PUT, DELETE)", method,
		)
	}
}

// ----------------------------------------------------------------------
// Translation helpers
// ----------------------------------------------------------------------

// buildBlobHTTPHeaders converts uos.ContentHeaders to the Azure SDK type.
func buildBlobHTTPHeaders(c uos.ContentHeaders) *blob.HTTPHeaders {
	h := &blob.HTTPHeaders{}
	if c.ContentType != "" {
		h.BlobContentType = &c.ContentType
	}
	if c.ContentEncoding != "" {
		h.BlobContentEncoding = &c.ContentEncoding
	}
	if c.ContentLanguage != "" {
		h.BlobContentLanguage = &c.ContentLanguage
	}
	if c.ContentDisposition != "" {
		h.BlobContentDisposition = &c.ContentDisposition
	}
	if c.CacheControl != "" {
		h.BlobCacheControl = &c.CacheControl
	}
	return h
}

// buildBlobHTTPHeadersFromMetadata builds HTTP headers for CommitBlockList.
func buildBlobHTTPHeadersFromMetadata(_ *uploadSession) *blob.HTTPHeaders {
	return &blob.HTTPHeaders{}
}

// buildMetadataMap converts uos.Metadata to Azure SDK metadata (map[string]*string).
// Keys are lower-cased via s3common.LowerMetadataKeys for round-trip equality.
// Azure metadata keys are case-insensitive; the SDK preserves user case, so
// we normalise to lower-case at the driver boundary.
func buildMetadataMap(m uos.Metadata) map[string]*string {
	lower := s3common.LowerMetadataKeys(m)
	if len(lower) == 0 {
		return nil
	}
	out := make(map[string]*string, len(lower))
	for k, v := range lower {
		v := v // capture
		out[k] = &v
	}
	return out
}

// translateBlobDownloadResponse builds an ObjectInfo from a blob download response.
func translateBlobDownloadResponse(bucket, key string, resp blob.DownloadStreamResponse) uos.ObjectInfo {
	info := uos.ObjectInfo{Bucket: bucket, Key: key}
	if resp.ContentLength != nil {
		info.Size = *resp.ContentLength
	} else {
		info.Size = -1
	}
	if resp.ETag != nil {
		info.ETag = strings.Trim(string(*resp.ETag), `"`)
	}
	if resp.LastModified != nil {
		info.LastModified = *resp.LastModified
	}
	if resp.VersionID != nil {
		info.VersionID = *resp.VersionID
	}
	info.Content = extractContentHeaders(resp.ContentType, resp.ContentEncoding, resp.ContentLanguage, resp.ContentDisposition, resp.CacheControl)
	info.Metadata = extractAzureMetadata(resp.Metadata)
	return info
}

// translateBlobProperties builds an ObjectInfo from GetProperties response.
func translateBlobProperties(bucket, key string, props blob.GetPropertiesResponse) uos.ObjectInfo {
	info := uos.ObjectInfo{Bucket: bucket, Key: key}
	if props.ContentLength != nil {
		info.Size = *props.ContentLength
	} else {
		info.Size = -1
	}
	if props.ETag != nil {
		info.ETag = strings.Trim(string(*props.ETag), `"`)
	}
	if props.LastModified != nil {
		info.LastModified = *props.LastModified
	}
	if props.VersionID != nil {
		info.VersionID = *props.VersionID
	}
	info.Content = extractContentHeaders(props.ContentType, props.ContentEncoding, props.ContentLanguage, props.ContentDisposition, props.CacheControl)
	info.Metadata = extractAzureMetadata(props.Metadata)
	return info
}

// translateBlobItem converts an Azure ListBlobsFlatSegment item to ObjectInfo.
func translateBlobItem(bucket string, item *container.BlobItem) uos.ObjectInfo {
	info := uos.ObjectInfo{Bucket: bucket}
	if item.Name != nil {
		info.Key = *item.Name
	}
	if item.Properties != nil {
		if item.Properties.ContentLength != nil {
			info.Size = *item.Properties.ContentLength
		} else {
			info.Size = -1
		}
		if item.Properties.ETag != nil {
			info.ETag = strings.Trim(string(*item.Properties.ETag), `"`)
		}
		if item.Properties.LastModified != nil {
			info.LastModified = *item.Properties.LastModified
		}
		if item.Properties.AccessTier != nil {
			info.StorageClass = string(*item.Properties.AccessTier)
		}
	}
	if item.VersionID != nil {
		info.VersionID = *item.VersionID
	}
	info.Metadata = extractAzureMetadata(item.Metadata)
	return info
}

// extractContentHeaders builds uos.ContentHeaders from pointer-typed Azure fields.
func extractContentHeaders(ct, ce, cl, cd, cc *string) uos.ContentHeaders {
	h := uos.ContentHeaders{}
	if ct != nil {
		h.ContentType = *ct
	}
	if ce != nil {
		h.ContentEncoding = *ce
	}
	if cl != nil {
		h.ContentLanguage = *cl
	}
	if cd != nil {
		h.ContentDisposition = *cd
	}
	if cc != nil {
		h.CacheControl = *cc
	}
	return h
}

// extractAzureMetadata converts Azure SDK metadata (map[string]*string) to
// uos.Metadata with lower-cased keys. The Azure SDK strips the x-ms-meta-
// wire prefix before exposing keys; we normalise to lower-case for
// cross-provider round-trip equality. Returns nil for empty maps.
func extractAzureMetadata(m map[string]*string) uos.Metadata {
	if len(m) == 0 {
		return nil
	}
	raw := make(map[string]string, len(m))
	for k, v := range m {
		if v != nil {
			raw[k] = *v
		}
	}
	return s3common.LowerMetadataKeys(raw)
}

// buildHTTPRange converts a uos.ByteRange to the Azure SDK's blob.HTTPRange.
func buildHTTPRange(r uos.ByteRange) blob.HTTPRange {
	hr := blob.HTTPRange{Offset: r.Start}
	if r.End >= 0 {
		hr.Count = r.End - r.Start + 1
	}
	// Count == 0 means "to end of blob" in the Azure SDK.
	return hr
}

// strPtr returns a pointer to the string. Used to pass optional string
// fields to Azure SDK options that require *string.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// to returns a pointer to a string literal (for small inline uses).
func to(s string) *string { return &s }

// Ensure bytes.Buffer is imported (used indirectly via io.NopCloser wrapping).
var _ = bytes.NewBuffer

// Compile-time assertions that all service types satisfy the uos interfaces.
var (
	_ uos.Client           = (*driverImpl)(nil)
	_ uos.BucketService    = bucketService{}
	_ uos.ObjectService    = objectService{}
	_ uos.MultipartService = multipartService{}
	_ uos.Signer           = signerService{}
)
