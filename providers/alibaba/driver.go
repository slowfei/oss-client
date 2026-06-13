package alibaba

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/capability"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// driverImpl 通过向 v2 SDK（github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss）
// 翻译调用来实现 pkg/uos.Client。它持有 SDK 的 *oss.Client，每个方法调用时
// 直接构造对应的 Request 结构体。
//
// driverImpl 支持并发使用；*oss.Client 本身是 goroutine 安全的。
type driverImpl struct {
	cfg    uos.Config
	client *oss.Client
}

// Provider 返回 "alibaba"。实现 uos.Client 接口。
func (d *driverImpl) Provider() uos.Provider { return providerID }

// Capabilities 返回此 driver 的 v1 冻结 capability.Report。
// 每次调用生成新副本，调用方可安全修改。
func (d *driverImpl) Capabilities(_ context.Context) (capability.Report, error) {
	return capabilities(), nil
}

// Buckets 返回绑定到此 Client 的 BucketService 视图。
func (d *driverImpl) Buckets() uos.BucketService { return bucketService{d: d} }

// Objects 返回绑定到指定 bucket 的 ObjectService 视图。
// bucket 名在此处捕获，因此省略 Bucket 字段的请求仍能定位到正确的命名空间。
func (d *driverImpl) Objects(bucket string) uos.ObjectService {
	return objectService{d: d, defaultBucket: bucket}
}

// Multipart 返回绑定到指定 bucket 的 MultipartService 视图。
func (d *driverImpl) Multipart(bucket string) uos.MultipartService {
	return multipartService{d: d, defaultBucket: bucket}
}

// Signer 返回绑定到指定 bucket 的 Signer 视图。
func (d *driverImpl) Signer(bucket string) uos.Signer {
	return signerService{d: d, defaultBucket: bucket}
}

// As 暴露底层的 alibabacloud-oss-go-sdk-v2 handle 供需要厂商专属功能的调用方使用。
// 支持的目标类型：
//
//   - **oss.Client：填充高层 OSS 客户端。
//
// 对于任何其他类型返回 false（不修改 target）。
func (d *driverImpl) As(target any) bool {
	switch t := target.(type) {
	case **oss.Client:
		*t = d.client
		return true
	default:
		return false
	}
}

// Close 释放 driver 持有的任何资源。v2 SDK 的 *oss.Client 无需
// 在底层 http.Client transport 之外关闭 goroutine，因此此处为 no-op，
// 仅为满足 uos.Client 接口而保留。
func (d *driverImpl) Close() error { return nil }

// ----------------------------------------------------------------------
// BucketService
// ----------------------------------------------------------------------

// bucketService 实现 uos.BucketService。
type bucketService struct{ d *driverImpl }

// List 枚举配置的凭证可见的 bucket。OSS ListBuckets 支持 MaxKeys + Marker
// 分页；通过统一的 MaxResults / ContinuationToken 字段映射。
func (b bucketService) List(ctx context.Context, req uos.ListBucketsRequest) ([]uos.BucketInfo, error) {
	const op = "ListBuckets"

	v2req := &oss.ListBucketsRequest{}
	if req.MaxResults > 0 {
		v2req.MaxKeys = int32(req.MaxResults)
	}
	if req.ContinuationToken != "" {
		v2req.Marker = oss.Ptr(req.ContinuationToken)
	}

	res, err := b.d.client.ListBuckets(ctx, v2req)
	if err != nil {
		return nil, mapError(b.d.Provider(), op, "", "", err)
	}
	out := make([]uos.BucketInfo, 0, len(res.Buckets))
	for _, bp := range res.Buckets {
		out = append(out, uos.BucketInfo{
			Name:      oss.ToString(bp.Name),
			Region:    oss.ToString(bp.Location),
			CreatedAt: oss.ToTime(bp.CreationDate),
		})
	}
	return out, nil
}

// Create 创建一个新的 bucket。OSS 对已存在的 bucket 返回 BucketAlreadyExists，
// 错误映射层将其翻译为 ErrAlreadyExists。
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

	v2req := &oss.PutBucketRequest{
		Bucket: oss.Ptr(req.Name),
	}
	if req.ACL != "" {
		v2req.Acl = oss.BucketACLType(req.ACL)
	}

	if _, err := b.d.client.PutBucket(ctx, v2req); err != nil {
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

// Stat 返回已存在 bucket 的 BucketInfo。OSS 没有单独的 IsBucketExist；
// 通过 GetBucketInfo 调用，将 NoSuchBucket 错误映射为 ErrNotFound。
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

	infoRes, err := b.d.client.GetBucketInfo(ctx, &oss.GetBucketInfoRequest{
		Bucket: oss.Ptr(req.Name),
	})
	if err != nil {
		return nil, mapError(b.d.Provider(), op, req.Name, "", err)
	}

	region := oss.ToString(infoRes.BucketInfo.Location)

	return &uos.BucketInfo{
		Name:      req.Name,
		Region:    region,
		CreatedAt: oss.ToTime(infoRes.BucketInfo.CreationDate),
	}, nil
}

// Delete 移除一个空 bucket。非空 bucket 返回 BucketNotEmpty，
// 错误层映射为 ErrConflict。
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

	if _, err := b.d.client.DeleteBucket(ctx, &oss.DeleteBucketRequest{
		Bucket: oss.Ptr(req.Name),
	}); err != nil {
		return mapError(b.d.Provider(), op, req.Name, "", err)
	}
	return nil
}

// ----------------------------------------------------------------------
// ObjectService
// ----------------------------------------------------------------------

// objectService 为固定 bucket 实现 uos.ObjectService。
// 当设置了请求中的 Bucket 字段时优先使用；未设置时回退到 defaultBucket。
type objectService struct {
	d             *driverImpl
	defaultBucket string
}

// pickBucket 返回 reqBucket（如果非空），否则返回服务的 defaultBucket。
// 对于未绑定服务的无 bucket 请求返回空字符串，让 wire 层自然返回 InvalidArgument。
func (o objectService) pickBucket(reqBucket string) string {
	if reqBucket != "" {
		return reqBucket
	}
	return o.defaultBucket
}

// Put 通过 OSS PutObject 写入单个对象。Body 原样传递到 SDK；
// driver 要求已知 size（Size>=0），因为 OSS PutObject 会将 Content-Length
// 签入请求，未知 size 的流式路径走 transfer.Manager（v0.1 中为 BYPASS；
// v0.2 中按 ADR Follow-up #1 提升至 Uploader）。
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

	v2req := &oss.PutObjectRequest{
		Bucket:        oss.Ptr(bucket),
		Key:           oss.Ptr(req.Key),
		Body:          req.Body,
		ContentLength: oss.Ptr(req.Size),
	}
	applyContentHeaders(v2req, req.Content)
	applyMetadata(v2req, req.Metadata)
	if req.StorageClass != "" {
		v2req.StorageClass = oss.StorageClassType(req.StorageClass)
	}
	if req.ACL != "" {
		v2req.Acl = oss.ObjectACLType(req.ACL)
	}
	// v2 PutObjectRequest 没有 IfMatch/IfNoneMatch 专属字段，
	// 通过 RequestCommon.Headers 传递。
	if req.IfMatch != "" {
		if v2req.RequestCommon.Headers == nil {
			v2req.RequestCommon.Headers = map[string]string{}
		}
		v2req.RequestCommon.Headers["If-Match"] = req.IfMatch
	}
	if req.IfNoneMatch != "" {
		if v2req.RequestCommon.Headers == nil {
			v2req.RequestCommon.Headers = map[string]string{}
		}
		v2req.RequestCommon.Headers["If-None-Match"] = req.IfNoneMatch
	}

	res, err := o.d.client.PutObject(ctx, v2req)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(oss.ToString(res.ETag), `"`),
		VersionID: oss.ToString(res.VersionId),
	}, nil
}

// Get 通过 OSS GetObject 流式读取对象体。Range 请求使用标准 HTTP Range 头
// （格式 "bytes=start-end"）。返回的 ObjectReader.Body 是原始 io.ReadCloser；
// 调用方必须 Close。
func (o objectService) Get(ctx context.Context, req uos.GetObjectRequest) (*uos.ObjectReader, error) {
	const op = "GetObject"
	bucket := o.pickBucket(req.Bucket)

	v2req := &oss.GetObjectRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(req.Key),
	}
	if req.VersionID != "" {
		v2req.VersionId = oss.Ptr(req.VersionID)
	}
	if req.Range != nil {
		v2req.Range = oss.Ptr(formatRange(*req.Range))
		v2req.RangeBehavior = oss.Ptr("standard")
	}
	if req.IfMatch != "" {
		v2req.IfMatch = oss.Ptr(req.IfMatch)
	}
	if req.IfNoneMatch != "" {
		v2req.IfNoneMatch = oss.Ptr(req.IfNoneMatch)
	}
	if !req.IfModifiedSince.IsZero() {
		v2req.IfModifiedSince = oss.Ptr(req.IfModifiedSince.Format(http.TimeFormat))
	}
	if !req.IfUnmodifiedSince.IsZero() {
		v2req.IfUnmodifiedSince = oss.Ptr(req.IfUnmodifiedSince.Format(http.TimeFormat))
	}

	res, err := o.d.client.GetObject(ctx, v2req)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}

	info := translateObjectInfo(bucket, req.Key, res)
	contentLen := res.ContentLength
	return &uos.ObjectReader{
		Body:          res.Body,
		ContentLength: contentLen,
		Info:          info,
	}, nil
}

// Head 返回没有 body 的 ObjectInfo。v2 SDK 的 HeadObject 等效于 v1 的
// GetObjectDetailedMeta（返回 Last-Modified / Content-Length / ETag 外加用户 metadata 头）。
func (o objectService) Head(ctx context.Context, req uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	const op = "HeadObject"
	bucket := o.pickBucket(req.Bucket)

	v2req := &oss.HeadObjectRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(req.Key),
	}
	if req.VersionID != "" {
		v2req.VersionId = oss.Ptr(req.VersionID)
	}

	res, err := o.d.client.HeadObject(ctx, v2req)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	info := translateHeadObjectInfo(bucket, req.Key, res)
	return &info, nil
}

// Delete 移除单个对象。OSS DeleteObject 是幂等的：
// 删除不存在的 key 返回 204 No Content，而非 404。
func (o objectService) Delete(ctx context.Context, req uos.DeleteObjectRequest) error {
	const op = "DeleteObject"
	bucket := o.pickBucket(req.Bucket)

	v2req := &oss.DeleteObjectRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(req.Key),
	}
	if req.VersionID != "" {
		v2req.VersionId = oss.Ptr(req.VersionID)
	}

	if _, err := o.d.client.DeleteObject(ctx, v2req); err != nil {
		return mapError(o.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// Exists 报告对象是否存在。按合约套件约定，not-found 返回 (false, nil)；
// 其他错误传播。
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

// DeleteMany 通过 OSS DeleteMultipleObjects 删除一批 key。
// SDK 在成功删除时返回每个 key 的 Deleted 条目。
func (o objectService) DeleteMany(ctx context.Context, req uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	const op = "DeleteManyObjects"
	bucket := o.pickBucket(req.Bucket)
	if len(req.Keys) == 0 {
		return &uos.DeleteManyResult{}, nil
	}

	objects := make([]oss.ObjectIdentifier, 0, len(req.Keys))
	for _, k := range req.Keys {
		objects = append(objects, oss.ObjectIdentifier{Key: oss.Ptr(k)})
	}

	v2req := &oss.DeleteMultipleObjectsRequest{
		Bucket: oss.Ptr(bucket),
		Delete: &oss.Delete{
			Objects: objects,
			Quiet:   req.Quiet,
		},
	}

	res, err := o.d.client.DeleteMultipleObjects(ctx, v2req)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}

	out := &uos.DeleteManyResult{}
	if !req.Quiet {
		for _, d := range res.DeletedObjects {
			out.Deleted = append(out.Deleted, oss.ToString(d.Key))
		}
	}
	return out, nil
}

// Copy 复制对象。OSS 支持同账号跨 bucket 复制，通过 x-oss-copy-source 实现；
// 当源 bucket 与目标不同时通过 SourceBucket 字段区分。
func (o objectService) Copy(ctx context.Context, req uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	const op = "CopyObject"
	dstBucket := req.DestBucket

	v2req := &oss.CopyObjectRequest{
		Bucket:    oss.Ptr(dstBucket),
		Key:       oss.Ptr(req.DestKey),
		SourceKey: oss.Ptr(req.SourceKey),
	}
	if req.SourceBucket != "" && req.SourceBucket != dstBucket {
		v2req.SourceBucket = oss.Ptr(req.SourceBucket)
	}
	if req.SourceVersionID != "" {
		v2req.SourceVersionId = oss.Ptr(req.SourceVersionID)
	}
	if req.StorageClass != "" {
		v2req.StorageClass = oss.StorageClassType(req.StorageClass)
	}
	if req.ACL != "" {
		v2req.Acl = oss.ObjectACLType(req.ACL)
	}
	if req.IfMatch != "" {
		v2req.IfMatch = oss.Ptr(req.IfMatch)
	}
	if req.IfNoneMatch != "" {
		v2req.IfNoneMatch = oss.Ptr(req.IfNoneMatch)
	}
	applyContentHeaders(v2req, req.Content)
	applyMetadata(v2req, req.Metadata)

	switch strings.ToUpper(req.MetadataDirective) {
	case "COPY":
		v2req.MetadataDirective = oss.Ptr("COPY")
	case "REPLACE":
		v2req.MetadataDirective = oss.Ptr("REPLACE")
	default:
		if req.Metadata != nil {
			v2req.MetadataDirective = oss.Ptr("REPLACE")
		}
	}

	res, err := o.d.client.CopyObject(ctx, v2req)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, dstBucket, req.DestKey, err)
	}
	return &uos.CopyObjectResult{
		ETag:         strings.Trim(oss.ToString(res.ETag), `"`),
		VersionID:    oss.ToString(res.VersionId),
		LastModified: oss.ToTime(res.LastModified),
	}, nil
}

// List 通过 OSS ListObjectsV2 枚举匹配 prefix / delimiter 的对象。
// NextToken 通过 V2 NextContinuationToken 往返传输，使跨 provider
// 的不透明游标分页能正常工作。
func (o objectService) List(ctx context.Context, req uos.ListObjectsRequest) (*uos.ObjectList, error) {
	const op = "ListObjects"
	bucket := o.pickBucket(req.Bucket)

	v2req := &oss.ListObjectsV2Request{
		Bucket: oss.Ptr(bucket),
	}
	if req.Prefix != "" {
		v2req.Prefix = oss.Ptr(req.Prefix)
	}
	if req.Delimiter != "" {
		v2req.Delimiter = oss.Ptr(req.Delimiter)
	}
	if req.MaxResults > 0 {
		v2req.MaxKeys = int32(req.MaxResults)
	}
	if req.ContinuationToken != "" {
		v2req.ContinuationToken = oss.Ptr(req.ContinuationToken)
	}
	if req.StartAfter != "" {
		v2req.StartAfter = oss.Ptr(req.StartAfter)
	}

	res, err := o.d.client.ListObjectsV2(ctx, v2req)
	if err != nil {
		return nil, mapError(o.d.Provider(), op, bucket, "", err)
	}

	out := &uos.ObjectList{
		CommonPrefixes: make([]string, 0, len(res.CommonPrefixes)),
		NextToken:      oss.ToString(res.NextContinuationToken),
		Truncated:      res.IsTruncated,
	}
	for _, cp := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, oss.ToString(cp.Prefix))
	}
	out.Items = make([]uos.ObjectInfo, 0, len(res.Contents))
	for _, op := range res.Contents {
		out.Items = append(out.Items, uos.ObjectInfo{
			Bucket:       bucket,
			Key:          oss.ToString(op.Key),
			Size:         op.Size,
			ETag:         strings.Trim(oss.ToString(op.ETag), `"`),
			LastModified: oss.ToTime(op.LastModified),
			StorageClass: oss.ToString(op.StorageClass),
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// MultipartService
// ----------------------------------------------------------------------

// multipartService 实现 uos.MultipartService，底层使用 OSS 原生分片原语
// （InitiateMultipartUpload / UploadPart / CompleteMultipartUpload /
// AbortMultipartUpload / ListMultipartUploads）。
// 分片服务在 v0.1 中采用 bypass-vendor-native 模式 —— pkg/uos/transfer.Manager
// 在此处被 BYPASS；提升工作于 v0.2 按 ADR Follow-up #1 进行。
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

// Initiate 启动新的分片上传。返回的 MultipartUpload 携带 OSS 分配的 UploadID，
// 后续所有 UploadPart / Complete / Abort 调用都必须引用。
func (m multipartService) Initiate(ctx context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	const op = "InitiateMultipartUpload"
	bucket := m.pickBucket(req.Bucket)

	v2req := &oss.InitiateMultipartUploadRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(req.Key),
	}
	applyContentHeaders(v2req, req.Content)
	applyMetadata(v2req, req.Metadata)
	if req.StorageClass != "" {
		v2req.StorageClass = oss.StorageClassType(req.StorageClass)
	}
	// v2 InitiateMultipartUploadRequest 没有 Acl 专属字段，
	// 通过 RequestCommon.Headers 传递 x-oss-object-acl。
	if req.ACL != "" {
		if v2req.RequestCommon.Headers == nil {
			v2req.RequestCommon.Headers = map[string]string{}
		}
		v2req.RequestCommon.Headers["x-oss-object-acl"] = req.ACL
	}

	res, err := m.d.client.InitiateMultipartUpload(ctx, v2req)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.MultipartUpload{
		UploadID:     oss.ToString(res.UploadId),
		Bucket:       bucket,
		Key:          req.Key,
		Initiated:    time.Now().UTC(),
		StorageClass: req.StorageClass,
		Metadata:     req.Metadata,
	}, nil
}

// UploadPart 上传单个分片。OSS UploadPart 将调用方提供的 size 作为提示
// 用于 SDK 的 io.LimitedReader 包装；我们原样转发 req.Size
// 并在 body 长度不匹配时让 wire 层暴露 InvalidArgument。
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

	v2req := &oss.UploadPartRequest{
		Bucket:     oss.Ptr(bucket),
		Key:        oss.Ptr(req.Key),
		UploadId:   oss.Ptr(req.UploadID),
		PartNumber: int32(req.PartNumber),
		Body:       req.Body,
	}

	res, err := m.d.client.UploadPart(ctx, v2req)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.UploadedPart{
		PartNumber: req.PartNumber,
		ETag:       strings.Trim(oss.ToString(res.ETag), `"`),
		Size:       req.Size,
	}, nil
}

// Complete 通过将提供的分片按 PartNumber 顺序拼接来最终完成分片上传。
// Parts 必须有序呈现；合约套件要求调用方无论如何都提供有序列表。
func (m multipartService) Complete(ctx context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	const op = "CompleteMultipartUpload"
	bucket := m.pickBucket(req.Bucket)
	if len(req.Parts) == 0 {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: m.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key, Message: "Parts is required and must be non-empty",
		}
	}

	parts := make([]oss.UploadPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, oss.UploadPart{
			PartNumber: int32(p.PartNumber),
			ETag:       oss.Ptr(p.ETag),
		})
	}

	v2req := &oss.CompleteMultipartUploadRequest{
		Bucket:   oss.Ptr(bucket),
		Key:      oss.Ptr(req.Key),
		UploadId: oss.Ptr(req.UploadID),
		CompleteMultipartUpload: &oss.CompleteMultipartUpload{
			Parts: parts,
		},
	}

	res, err := m.d.client.CompleteMultipartUpload(ctx, v2req)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return &uos.PutObjectResult{
		ETag:      strings.Trim(oss.ToString(res.ETag), `"`),
		VersionID: oss.ToString(res.VersionId),
	}, nil
}

// Abort 取消一个正在进行的分片上传。OSS 使 Abort 在 wire 层幂等
// （NoSuchUpload 在某些 region 返回 204，其他返回 ServiceError）；
// 错误映射器在适用时将两者翻译为 ErrNotFound。
func (m multipartService) Abort(ctx context.Context, req uos.AbortMultipartRequest) error {
	const op = "AbortMultipartUpload"
	bucket := m.pickBucket(req.Bucket)

	v2req := &oss.AbortMultipartUploadRequest{
		Bucket:   oss.Ptr(bucket),
		Key:      oss.Ptr(req.Key),
		UploadId: oss.Ptr(req.UploadID),
	}

	if _, err := m.d.client.AbortMultipartUpload(ctx, v2req); err != nil {
		return mapError(m.d.Provider(), op, bucket, req.Key, err)
	}
	return nil
}

// List 枚举 bucket 中正在进行的分片上传。分页由 OSS 通过 KeyMarker 处理；
// 当剩余更多页时暴露 NextToken 使调用方可以迭代。
func (m multipartService) List(ctx context.Context, req uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	const op = "ListMultipartUploads"
	bucket := m.pickBucket(req.Bucket)

	v2req := &oss.ListMultipartUploadsRequest{
		Bucket: oss.Ptr(bucket),
	}
	if req.Prefix != "" {
		v2req.Prefix = oss.Ptr(req.Prefix)
	}
	if req.MaxResults > 0 {
		v2req.MaxUploads = int32(req.MaxResults)
	}
	if req.ContinuationToken != "" {
		v2req.KeyMarker = oss.Ptr(req.ContinuationToken)
	}

	res, err := m.d.client.ListMultipartUploads(ctx, v2req)
	if err != nil {
		return nil, mapError(m.d.Provider(), op, bucket, "", err)
	}

	out := &uos.MultipartUploadList{
		Uploads:   make([]uos.MultipartUpload, 0, len(res.Uploads)),
		Truncated: res.IsTruncated,
		NextToken: oss.ToString(res.NextKeyMarker),
	}
	for _, u := range res.Uploads {
		out.Uploads = append(out.Uploads, uos.MultipartUpload{
			UploadID:  oss.ToString(u.UploadId),
			Bucket:    bucket,
			Key:       oss.ToString(u.Key),
			Initiated: oss.ToTime(u.Initiated),
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------
// Signer
// ----------------------------------------------------------------------

// signerService 为 OSS HMAC 预签名 URL 实现 uos.Signer。
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

// SignURL 返回所请求操作的 HTTP 签名 URL。v2 SDK 的 Client.Presign 方法
// 构建 URL 同步（无 I/O）并返回 PresignResult；我们将其包装为统一的 SignedURL 形态。
//
// v2 SDK 的 Presign 不支持 DELETE 操作：内部 marshalPresignInput 仅识别
// GetObject / PutObject / HeadObject / InitiateMultipartUpload / UploadPart /
// CompleteMultipartUpload / AbortMultipartUpload 七种请求类型。
// DELETE 作为预签名操作极为罕见（临时删除授权几乎都走服务端 API），
// 当 Method=="DELETE" 时返回 ErrInvalidArgument。
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

	var presignReq any
	var supported bool
	switch method {
	case http.MethodGet:
		presignReq = &oss.GetObjectRequest{
			Bucket:    oss.Ptr(bucket),
			Key:       oss.Ptr(req.Key),
			VersionId: strPtrIfNonEmpty(req.VersionID),
		}
		supported = true
	case http.MethodPut:
		presignReq = &oss.PutObjectRequest{
			Bucket: oss.Ptr(bucket),
			Key:    oss.Ptr(req.Key),
		}
		supported = true
	case http.MethodHead:
		presignReq = &oss.HeadObjectRequest{
			Bucket:    oss.Ptr(bucket),
			Key:       oss.Ptr(req.Key),
			VersionId: strPtrIfNonEmpty(req.VersionID),
		}
		supported = true
	case http.MethodDelete:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: "SignURL DELETE is not supported by the v2 OSS SDK; use the ObjectService.Delete API instead",
		}
	default:
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q (allowed: GET, PUT, HEAD)", method),
		}
	}

	if !supported {
		return nil, &uos.Error{
			Code: uos.ErrInvalidArgument, Provider: s.d.Provider(), Operation: op,
			Bucket: bucket, Key: req.Key,
			Message: fmt.Sprintf("unsupported SignURL method %q", method),
		}
	}

	// 构造 Presign 选项：设置过期时间 + 附加请求头/查询参数
	var presignOpts []func(*oss.PresignOptions)
	presignOpts = append(presignOpts, oss.PresignExpires(req.ExpiresIn))
	applyPresignCommon(presignReq, req.Headers, req.Query)

	result, err := s.d.client.Presign(ctx, presignReq, presignOpts...)
	if err != nil {
		return nil, mapError(s.d.Provider(), op, bucket, req.Key, err)
	}

	return &uos.SignedURL{
		URL:       result.URL,
		Method:    result.Method,
		ExpiresAt: result.Expiration,
		Headers:   signedHeadersToHTTP(result.SignedHeaders),
	}, nil
}

// IssueDirectGrant 始终返回 ErrUnsupported / CapDirectGrant，
// 因为 S3-family provider（包括 OSS）以 URL 形式（use SignURL with PUT）
// 发放写入授权。参见 docs/provider_matrix.md footnote 5。
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

// strPtrIfNonEmpty 在 s 非空时返回指向 s 的指针，否则返回 nil。
func strPtrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func applyPresignCommon(req any, headers http.Header, query map[string]string) {
	commonHeaders := httpHeaderToMap(headers)
	commonQuery := stringMapCopy(query)
	if len(commonHeaders) == 0 && len(commonQuery) == 0 {
		return
	}
	switch r := req.(type) {
	case *oss.GetObjectRequest:
		r.RequestCommon.Headers = commonHeaders
		r.RequestCommon.Parameters = commonQuery
	case *oss.PutObjectRequest:
		r.RequestCommon.Headers = commonHeaders
		r.RequestCommon.Parameters = commonQuery
	case *oss.HeadObjectRequest:
		r.RequestCommon.Headers = commonHeaders
		r.RequestCommon.Parameters = commonQuery
	}
}

func httpHeaderToMap(headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		out[key] = strings.Join(values, ",")
	}
	return out
}

func stringMapCopy(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// applyContentHeaders 将 uos.ContentHeaders 映射到 *PutObjectRequest 的对应字段上。
// 空字段不设置，保留 vendor 默认值。
// 同时支持 *InitiateMultipartUploadRequest 和 *CopyObjectRequest（通过接口）。
type contentRequest interface {
	SetCacheControl(*string)
	SetContentDisposition(*string)
	SetContentEncoding(*string)
	SetContentType(*string)
	SetExpires(*string)
}

// 由于 v2 SDK 的 Request 结构体不共享统一接口，针对每种 Request 类型
// 提供具体的 applyContentHeaders 实现。

func applyContentHeaders(req any, c uos.ContentHeaders) {
	// v2 SDK 中 Content-Language 并非所有 Request 结构体的专属字段，
	// 需要通过 RequestCommon.Headers 传递。
	var headers map[string]string

	switch r := req.(type) {
	case *oss.PutObjectRequest:
		if c.ContentType != "" {
			r.ContentType = oss.Ptr(c.ContentType)
		}
		if c.ContentEncoding != "" {
			r.ContentEncoding = oss.Ptr(c.ContentEncoding)
		}
		if c.ContentDisposition != "" {
			r.ContentDisposition = oss.Ptr(c.ContentDisposition)
		}
		if c.CacheControl != "" {
			r.CacheControl = oss.Ptr(c.CacheControl)
		}
		if !c.Expires.IsZero() {
			r.Expires = oss.Ptr(c.Expires.Format(http.TimeFormat))
		}
		headers = r.RequestCommon.Headers
	case *oss.CopyObjectRequest:
		if c.ContentType != "" {
			r.ContentType = oss.Ptr(c.ContentType)
		}
		if c.ContentEncoding != "" {
			r.ContentEncoding = oss.Ptr(c.ContentEncoding)
		}
		if c.ContentDisposition != "" {
			r.ContentDisposition = oss.Ptr(c.ContentDisposition)
		}
		if c.CacheControl != "" {
			r.CacheControl = oss.Ptr(c.CacheControl)
		}
		if !c.Expires.IsZero() {
			r.Expires = oss.Ptr(c.Expires.Format(http.TimeFormat))
		}
		headers = r.RequestCommon.Headers
	case *oss.InitiateMultipartUploadRequest:
		if c.ContentType != "" {
			r.ContentType = oss.Ptr(c.ContentType)
		}
		if c.ContentEncoding != "" {
			r.ContentEncoding = oss.Ptr(c.ContentEncoding)
		}
		if c.ContentDisposition != "" {
			r.ContentDisposition = oss.Ptr(c.ContentDisposition)
		}
		if c.CacheControl != "" {
			r.CacheControl = oss.Ptr(c.CacheControl)
		}
		if !c.Expires.IsZero() {
			r.Expires = oss.Ptr(c.Expires.Format(http.TimeFormat))
		}
		headers = r.RequestCommon.Headers
	}

	if c.ContentLanguage != "" && headers != nil {
		headers["Content-Language"] = c.ContentLanguage
	}
}

// applyMetadata 将 uos.Metadata 映射为 v2 SDK 的 Metadata map。
// Key 通过 s3common.LowerMetadataKeys 转为小写；
// OSS wire 层自动添加 "x-oss-meta-" 前缀（Metadata 字段 tag 为 header,x-oss-meta-,usermeta）。
func applyMetadata(req any, m uos.Metadata) {
	lower := s3common.LowerMetadataKeys(m)
	if len(lower) == 0 {
		return
	}
	switch r := req.(type) {
	case *oss.PutObjectRequest:
		r.Metadata = lower
	case *oss.CopyObjectRequest:
		r.Metadata = lower
	case *oss.InitiateMultipartUploadRequest:
		r.Metadata = lower
	}
}

// translateObjectInfo 从 v2 GetObjectResult 重建 uos.ObjectInfo。
// 注意：v2 GetObjectResult 没有 ContentEncoding/ContentDisposition/
// CacheControl/ContentLanguage/Expires 等专属输出字段；
// 这些头信息需从 res.Headers（ResultCommon.Headers）中提取。
func translateObjectInfo(bucket, key string, res *oss.GetObjectResult) uos.ObjectInfo {
	info := uos.ObjectInfo{
		Bucket: bucket,
		Key:    key,
		ETag:   strings.Trim(oss.ToString(res.ETag), `"`),
		Size:   res.ContentLength,
		Content: uos.ContentHeaders{
			ContentType:        oss.ToString(res.ContentType),
			ContentEncoding:    res.Headers.Get("Content-Encoding"),
			ContentLanguage:    res.Headers.Get("Content-Language"),
			ContentDisposition: res.Headers.Get("Content-Disposition"),
			CacheControl:       res.Headers.Get("Cache-Control"),
		},
		StorageClass: oss.ToString(res.StorageClass),
		VersionID:    oss.ToString(res.VersionId),
		Metadata:     res.Metadata,
	}
	if res.LastModified != nil {
		info.LastModified = *res.LastModified
	}
	if v := res.Headers.Get("Expires"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			info.Content.Expires = t
		}
	}
	return info
}

// translateHeadObjectInfo 从 v2 HeadObjectResult 重建 uos.ObjectInfo。
// HeadObjectResult 包含 CacheControl/ContentDisposition/ContentEncoding
// 等专属输出字段。
func translateHeadObjectInfo(bucket, key string, res *oss.HeadObjectResult) uos.ObjectInfo {
	info := uos.ObjectInfo{
		Bucket: bucket,
		Key:    key,
		ETag:   strings.Trim(oss.ToString(res.ETag), `"`),
		Size:   res.ContentLength,
		Content: uos.ContentHeaders{
			ContentType:        oss.ToString(res.ContentType),
			ContentEncoding:    oss.ToString(res.ContentEncoding),
			ContentLanguage:    res.Headers.Get("Content-Language"),
			ContentDisposition: oss.ToString(res.ContentDisposition),
			CacheControl:       oss.ToString(res.CacheControl),
		},
		StorageClass: oss.ToString(res.StorageClass),
		VersionID:    oss.ToString(res.VersionId),
		Metadata:     res.Metadata,
	}
	if res.LastModified != nil {
		info.LastModified = *res.LastModified
	}
	if v := res.Headers.Get("Expires"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			info.Content.Expires = t
		}
	}
	return info
}

// formatRange 将 uos.ByteRange 渲染为 HTTP Range 头值（"bytes=start-end" 或 "bytes=start-"）。
func formatRange(r uos.ByteRange) string {
	if r.End < 0 {
		return fmt.Sprintf("bytes=%d-", r.Start)
	}
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
}

// signedHeadersToHTTP 将 PresignResult 返回的 map[string]string 签名头
// 转换为 http.Header。
func signedHeadersToHTTP(m map[string]string) http.Header {
	if len(m) == 0 {
		return nil
	}
	h := make(http.Header, len(m))
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

// 编译时断言 driverImpl 满足完整的 uos.Client 接口。
// pkg/uos.Client 的变更会在此处表现为构建失败，修复位置明确。
var (
	_ uos.Client           = (*driverImpl)(nil)
	_ uos.BucketService    = bucketService{}
	_ uos.ObjectService    = objectService{}
	_ uos.MultipartService = multipartService{}
	_ uos.Signer           = signerService{}
)
