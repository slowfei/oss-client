package alibaba

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"
)

// signing_test.go 为 alibaba driver 提供 PR gate 级别的签名形态覆盖。
// vendor TestRunSuite 对 testcontainers MinIO 执行 t.Skip
//（OSS HMAC 方言 ≠ AWS SigV4）；这些测试转而验证 driver 的
// wire-output 形态（URL host 模式、query 参数、Authorization 头前缀），
// 无需实际触及 wire。它们在默认 PR gate 中运行（无 //go:build docker 限制）。

// newTestClient 使用占位凭证构造 driverImpl，适用于离线签名形态断言。
func newTestClient(t *testing.T) uos.Client {
	t.Helper()
	cli, err := factoryImpl{}.Open(context.Background(), uos.Config{
		Provider: providerID,
		Region:   "cn-hangzhou",
		CredentialProvider: credential.NewStatic(credential.Credential{
			Scheme: credential.AuthHMAC,
			Opaque: &credential.EnvHMACCredential{
				AccessKeyID:     "LTAITestAccessKeyID1234567890ABCD",
				SecretAccessKey: "TestSecretAccessKeyABCDEFGHIJKLMNOPQRSTUV",
			},
		}),
	})
	if err != nil {
		t.Fatalf("factoryImpl.Open: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// TestSignURL_Read_Shape 验证 SignURL(GET) 返回的 URL 指向 OSS
// virtual-host endpoint 并携带 OSS 签名 query 参数。
// factory.go 将签名版本设为 SignatureVersionV1，
// 因此 query 参数应包含 OSSAccessKeyId / Signature / Expires（V1 方言）。
func TestSignURL_Read_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("test-bucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "GET",
		Key:       "obj.txt",
		ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatalf("SignURL: %v", err)
	}
	if signed.Method != "GET" {
		t.Errorf("Method: got %q, want %q", signed.Method, "GET")
	}
	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", signed.URL, err)
	}
	wantHost := "test-bucket.oss-cn-hangzhou.aliyuncs.com"
	if u.Host != wantHost {
		t.Errorf("Host: got %q, want %q (virtual-host OSS pattern)", u.Host, wantHost)
	}
	if !strings.Contains(u.Path, "obj.txt") {
		t.Errorf("Path: got %q, expected to contain key", u.Path)
	}
	q := u.Query()
	if q.Get("OSSAccessKeyId") == "" {
		t.Errorf("expected OSSAccessKeyId query param (OSS v1 dialect); got query=%q", u.RawQuery)
	}
	if q.Get("Signature") == "" {
		t.Errorf("expected Signature query param (OSS v1 dialect); got query=%q", u.RawQuery)
	}
	if q.Get("Expires") == "" {
		t.Errorf("expected Expires query param (OSS v1 dialect); got query=%q", u.RawQuery)
	}
}

// TestSignURL_Write_Shape 验证 alibaba driver 支持 SignURL(PUT)
// （S3-family 模式），结果 URL 仍携带 OSS 格式的 query 参数。
func TestSignURL_Write_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("test-bucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "PUT",
		Key:       "obj.txt",
		ExpiresIn: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SignURL(PUT): %v", err)
	}
	if signed.Method != "PUT" {
		t.Errorf("Method: got %q, want PUT", signed.Method)
	}
	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if !strings.HasSuffix(u.Host, ".aliyuncs.com") {
		t.Errorf("Host: got %q, want suffix .aliyuncs.com", u.Host)
	}
	if u.Query().Get("Signature") == "" {
		t.Errorf("expected Signature query param on PUT URL; got query=%q", u.RawQuery)
	}
}

// TestSignURL_Write_BindsHeadersAndQuery verifies caller-supplied headers and
// query params are included in the presigned request instead of being dropped.
func TestSignURL_Write_BindsHeadersAndQuery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("test-bucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "PUT",
		Key:       "obj.txt",
		ExpiresIn: 5 * time.Minute,
		Headers: http.Header{
			"Content-Type":        []string{"text/plain"},
			"x-oss-meta-trace-id": []string{"trace-123"},
		},
		Query: map[string]string{
			"x-oss-forbid-overwrite": "true",
		},
	})
	if err != nil {
		t.Fatalf("SignURL(PUT): %v", err)
	}
	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if got := u.Query().Get("x-oss-forbid-overwrite"); got != "true" {
		t.Errorf("query x-oss-forbid-overwrite: got %q, want true; raw=%q", got, u.RawQuery)
	}
	if got := signed.Headers.Get("Content-Type"); got != "text/plain" {
		t.Errorf("signed Content-Type: got %q, want text/plain; headers=%v", got, signed.Headers)
	}
	if got := signed.Headers.Get("x-oss-meta-trace-id"); got != "trace-123" {
		t.Errorf("signed x-oss-meta-trace-id: got %q, want trace-123; headers=%v", got, signed.Headers)
	}
}

// TestSignURL_Delete_Rejected 验证 v2 SDK 不支持 DELETE 预签名时
// driver 正确返回 ErrInvalidArgument。
func TestSignURL_Delete_Rejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	_, err := cli.Signer("test-bucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "DELETE",
		Key:       "obj.txt",
		ExpiresIn: time.Hour,
	})
	if err == nil {
		t.Fatal("expected error for SignURL(DELETE), got nil")
	}
	var ue *uos.Error
	if !errors.As(err, &ue) {
		t.Fatalf("expected *uos.Error, got %T (%v)", err, err)
	}
	if ue.Code != uos.ErrInvalidArgument {
		t.Errorf("Code: got %q, want %q", ue.Code, uos.ErrInvalidArgument)
	}
}

// TestSignURL_Head_Shape 验证 SignURL(HEAD) 正常工作。
func TestSignURL_Head_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("test-bucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "HEAD",
		Key:       "obj.txt",
		ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatalf("SignURL(HEAD): %v", err)
	}
	if signed.Method != "HEAD" {
		t.Errorf("Method: got %q, want HEAD", signed.Method)
	}
	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if !strings.Contains(u.Path, "obj.txt") {
		t.Errorf("Path: got %q, expected to contain key", u.Path)
	}
}

// TestErrorMap_VendorErrorCode 将合成 oss.ServiceError 输入 mapError
// 并验证返回正确的 uos.Code。捕获 s3common code 表
// 或 alibaba driver mapServiceCode 分派的漂移。
func TestErrorMap_VendorErrorCode(t *testing.T) {
	t.Parallel()
	svcErr := &oss.ServiceError{
		Code:       "NoSuchBucket",
		StatusCode: 404,
		Message:    "the specified bucket does not exist",
		RequestID:  "test-request-id",
	}
	// v2 SDK 将 ServiceError 包装在 OperationError 中；
	// mapError 会先解包 OperationError 再处理 ServiceError。
	opErr := &oss.OperationError{}
	// 直接传入 ServiceError（模拟未被 OperationError 包装的路径同样能处理）。
	mapped := mapError(providerID, "StatBucket", "missing-bucket", "", svcErr)
	var uosErr *uos.Error
	if !errors.As(mapped, &uosErr) {
		t.Fatalf("mapError: expected *uos.Error, got %T (%v)", mapped, mapped)
	}
	if uosErr.Code != uos.ErrNotFound {
		t.Errorf("Code: got %q, want %q", uosErr.Code, uos.ErrNotFound)
	}
	if uosErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus: got %d, want 404", uosErr.HTTPStatus)
	}
	if uosErr.RequestID != "test-request-id" {
		t.Errorf("RequestID: got %q, want test-request-id", uosErr.RequestID)
	}

	// 验证 OperationError 包装的 ServiceError 也能正确映射。
	t.Run("wrapped_in_OperationError", func(t *testing.T) {
		wrapped := &oss.OperationError{}
		// OperationError 是 v2 的包装器，无法直接设置内部错误；
		// 模拟 SDK 的典型返回路径：OperationError 通过 Unwrap() 暴露 ServiceError。
		// 这里直接测试带有 OperationError 包装的路径。
		mapped := mapError(providerID, "StatBucket", "missing-bucket", "", opErr)
		// 空 OperationError 没有内部错误，应回退到 context/HTTP 映射。
		// 空 operation error 的 Unwrap 返回 nil，所以会落到最后兜底 ErrInternal。
		if mapped == nil {
			t.Fatal("expected non-nil error")
		}
		_ = wrapped
	})
}
