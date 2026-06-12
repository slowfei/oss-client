package alibaba

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"
)

// signing_test.go provides PR-gate signing-shape coverage for the alibaba
// driver. The vendor TestRunSuite SKIPs against testcontainers MinIO
// (OSS HMAC dialect ≠ AWS SigV4); these tests instead validate the
// driver's wire-output shape (URL host pattern, query parameters,
// Authorization-header prefix) WITHOUT reaching the wire. They run in
// the default PR gate (no //go:build docker).

// newTestClient constructs a driverImpl wired with placeholder credentials
// suitable for offline signing-shape assertions.
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

// TestSignURL_Read_Shape validates that SignURL(GET) returns a URL
// pointing at the OSS virtual-host endpoint and carries the OSS HMAC v1
// query parameters that the wire layer will consume.
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

// TestSignURL_Write_Shape verifies the alibaba driver supports SignURL
// for PUT (S3-family pattern) and the resulting URL still carries the
// OSS-format query parameters.
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

// TestSign_OSS_v1_AuthHeader exercises the OSS SDK's signer directly via
// the driver's SignURL ExpiresIn field — for OSS v1 the resulting URL is
// query-signed (Authorization is encoded into query params, not a
// header). This test asserts the canonical query-param signature shape
// rather than an Authorization header (which only appears on
// header-signed in-flight requests).
func TestSign_OSS_v1_AuthHeader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("test-bucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "GET",
		Key:       "auth-shape.bin",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("SignURL: %v", err)
	}
	u, _ := url.Parse(signed.URL)
	q := u.Query()
	// The OSSAccessKeyId field is the canonical OSS v1 signal; AWS SigV4
	// would instead use X-Amz-Algorithm. Mismatching prefixes here
	// indicates a wire-dialect bug (e.g. accidentally building SigV4 for
	// an OSS endpoint).
	if q.Get("OSSAccessKeyId") != "LTAITestAccessKeyID1234567890ABCD" {
		t.Errorf("OSSAccessKeyId: got %q, want test AK", q.Get("OSSAccessKeyId"))
	}
	if got := q.Get("X-Amz-Algorithm"); got != "" {
		t.Errorf("unexpected X-Amz-Algorithm=%q on OSS-signed URL — wire dialect leak", got)
	}
}

// TestErrorMap_VendorErrorCode feeds a synthetic oss.ServiceError into
// mapError and verifies the right uos.Code is returned. Catches drift in
// the s3common code table or the alibaba driver's mapServiceCode dispatch.
func TestErrorMap_VendorErrorCode(t *testing.T) {
	t.Parallel()
	svcErr := oss.ServiceError{
		Code:       "NoSuchBucket",
		StatusCode: 404,
		Message:    "the specified bucket does not exist",
		RequestID:  "test-request-id",
	}
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
}
