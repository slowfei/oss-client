package tencent

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"
)

// signing_test.go provides PR-gate signing-shape coverage for the tencent
// driver. The vendor TestRunSuite SKIPs against testcontainers MinIO
// (COS HMAC v1 dialect ≠ AWS SigV4); these tests instead validate the
// driver's wire-output shape (URL host pattern, q-sign-* query params,
// Authorization-header prefix) WITHOUT reaching the wire.

// newTestClient constructs a driverImpl wired with placeholder credentials
// suitable for offline signing-shape assertions. AppID is set so the
// per-bucket BucketURL composes correctly.
func newTestClient(t *testing.T) uos.Client {
	t.Helper()
	cli, err := factoryImpl{}.Open(context.Background(), uos.Config{
		Provider: providerID,
		Region:   "ap-guangzhou",
		CredentialProvider: credential.NewStatic(credential.Credential{
			Scheme: credential.AuthHMAC,
			Opaque: &credential.EnvHMACCredential{
				AccessKeyID:     "AKIDTestSecretIDxxxxxxxxxxxxxxxxxxxxxxxx",
				SecretAccessKey: "TestSecretKeyxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			},
		}),
		DriverConfig: &DriverConfig{AppID: "1250000000"},
	})
	if err != nil {
		t.Fatalf("factoryImpl.Open: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// TestSignURL_Read_Shape validates that SignURL(GET) returns a URL
// pointing at the COS virtual-host endpoint and carries the COS HMAC v1
// q-sign-* query parameters that the wire layer will consume.
func TestSignURL_Read_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("examplebucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "GET",
		Key:       "obj.txt",
		ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatalf("SignURL: %v", err)
	}
	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", signed.URL, err)
	}
	wantHostSuffix := "cos.ap-guangzhou.myqcloud.com"
	if !strings.HasSuffix(u.Host, wantHostSuffix) {
		t.Errorf("Host: got %q, expected suffix %q", u.Host, wantHostSuffix)
	}
	if !strings.HasPrefix(u.Host, "examplebucket-1250000000.") {
		t.Errorf("Host: got %q, expected bucket-appid prefix %q", u.Host, "examplebucket-1250000000")
	}
	if !strings.Contains(u.Path, "obj.txt") {
		t.Errorf("Path: got %q, expected to contain key", u.Path)
	}
}

// TestSignURL_Write_Shape verifies the tencent driver supports SignURL
// for PUT (S3-family pattern).
func TestSignURL_Write_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("examplebucket").SignURL(ctx, uos.SignURLRequest{
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
	if !strings.Contains(signed.URL, "obj.txt") {
		t.Errorf("URL: got %q, expected to contain key", signed.URL)
	}
}

// TestSign_COS_v1_QSign asserts the COS HMAC v1 query-string signature
// dialect. The wire identifier is `q-sign-algorithm=sha1`; the AK lives
// under `q-ak`; the time window lives under `q-sign-time` and `q-key-time`.
// AWS SigV4 would instead use `X-Amz-Algorithm` — a mismatch here
// indicates a dangerous wire-dialect bug.
func TestSign_COS_v1_QSign(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("examplebucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "GET",
		Key:       "qsign.bin",
		ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatalf("SignURL: %v", err)
	}
	u, _ := url.Parse(signed.URL)
	q := u.Query()
	if got := q.Get("q-sign-algorithm"); got != "sha1" {
		t.Errorf("q-sign-algorithm: got %q, want %q (COS v1 dialect)", got, "sha1")
	}
	if got := q.Get("q-ak"); got == "" {
		t.Errorf("missing q-ak query param (COS v1 carries AK in q-ak); query=%q", u.RawQuery)
	}
	if got := q.Get("q-sign-time"); got == "" {
		t.Errorf("missing q-sign-time query param (COS v1); query=%q", u.RawQuery)
	}
	if got := q.Get("q-key-time"); got == "" {
		t.Errorf("missing q-key-time query param (COS v1); query=%q", u.RawQuery)
	}
	if got := q.Get("q-signature"); got == "" {
		t.Errorf("missing q-signature query param (COS v1); query=%q", u.RawQuery)
	}
	if got := q.Get("X-Amz-Algorithm"); got != "" {
		t.Errorf("unexpected X-Amz-Algorithm=%q on COS-signed URL — wire dialect leak", got)
	}
}

// TestErrorMap_VendorErrorCode feeds a synthetic *cos.ErrorResponse into
// mapError and verifies the right uos.Code is returned. The COS SDK's
// ErrorResponse.Error() method dereferences Response.Request.URL, so we
// stage a minimal request/response pair to keep the panic-free contract.
func TestErrorMap_VendorErrorCode(t *testing.T) {
	t.Parallel()
	reqURL, _ := url.Parse("https://examplebucket-1250000000.cos.ap-guangzhou.myqcloud.com/missing")
	svcErr := &cos.ErrorResponse{
		Code:    "NoSuchBucket",
		Message: "the specified bucket does not exist",
		Response: &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{},
			Request:    &http.Request{Method: http.MethodHead, URL: reqURL},
		},
		RequestID: "tencent-test-rid",
	}
	mapped := mapError(providerID, "StatBucket", "missing-bucket", "", svcErr)
	var uosErr *uos.Error
	if !errors.As(mapped, &uosErr) {
		t.Fatalf("mapError: expected *uos.Error, got %T (%v)", mapped, mapped)
	}
	if uosErr.Code != uos.ErrNotFound {
		t.Errorf("Code: got %q, want %q", uosErr.Code, uos.ErrNotFound)
	}
	if uosErr.HTTPStatus != http.StatusNotFound {
		t.Errorf("HTTPStatus: got %d, want %d", uosErr.HTTPStatus, http.StatusNotFound)
	}
	if uosErr.RequestID != "tencent-test-rid" {
		t.Errorf("RequestID: got %q, want tencent-test-rid", uosErr.RequestID)
	}
}
