package huawei

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
)

// signing_test.go provides PR-gate signing-shape coverage for the huawei
// driver. The vendor TestRunSuite SKIPs against testcontainers MinIO
// (OBS HMAC dialect ≠ AWS SigV4); these tests instead validate the
// driver's wire-output shape (URL host pattern, query parameters,
// Authorization-header prefix) WITHOUT reaching the wire.

// newTestClient constructs a driverImpl wired with placeholder credentials
// suitable for offline signing-shape assertions. Endpoint is mandatory
// for the huawei driver per its strict region/endpoint pairing rule.
func newTestClient(t *testing.T) uos.Client {
	t.Helper()
	cli, err := factoryImpl{}.Open(context.Background(), uos.Config{
		Provider: providerID,
		Region:   "cn-north-4",
		Endpoint: "https://obs.cn-north-4.myhuaweicloud.com",
		CredentialProvider: credential.NewStatic(credential.Credential{
			Scheme: credential.AuthHMAC,
			Opaque: &credential.EnvHMACCredential{
				AccessKeyID:     "HWTestAccessKeyIdxxxxxxxxxxxxxxxx",
				SecretAccessKey: "HWTestSecretAccessKeyxxxxxxxxxxxxxxxxxxxxx",
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
// pointing at the OBS virtual-host endpoint and carries the OBS query
// parameters that the wire layer will consume.
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
	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", signed.URL, err)
	}
	wantHostSuffix := "obs.cn-north-4.myhuaweicloud.com"
	if !strings.Contains(u.Host, wantHostSuffix) {
		t.Errorf("Host: got %q, expected to contain %q", u.Host, wantHostSuffix)
	}
	if !strings.Contains(u.Host, "test-bucket") && !strings.Contains(u.Path, "test-bucket") {
		t.Errorf("URL: got host=%q path=%q, expected bucket name in host or path", u.Host, u.Path)
	}
	if !strings.Contains(u.Path, "obj.txt") {
		t.Errorf("Path: got %q, expected to contain key", u.Path)
	}
	q := u.Query()
	// OBS v2 (default) presigned URLs carry AWSAccessKeyId + Signature + Expires
	// query params — note the SDK keeps the legacy "AWSAccessKeyId" field
	// name for v2 SigV2 compatibility, NOT "AccessKeyId" or "OBSAccessKeyId".
	if q.Get("AWSAccessKeyId") == "" {
		t.Errorf("expected AWSAccessKeyId query param (OBS v2 dialect); query=%q", u.RawQuery)
	}
	if q.Get("Signature") == "" {
		t.Errorf("expected Signature query param (OBS v2 dialect); query=%q", u.RawQuery)
	}
	if q.Get("Expires") == "" {
		t.Errorf("expected Expires query param (OBS v2 dialect); query=%q", u.RawQuery)
	}
}

// TestSignURL_Write_Shape verifies the huawei driver supports SignURL
// for PUT (S3-family pattern).
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
	if !strings.Contains(signed.URL, "obj.txt") {
		t.Errorf("URL: got %q, expected to contain key", signed.URL)
	}
}

// TestSign_OBS_v2_AuthHeader exercises the OBS SDK's signer via SignURL.
// OBS v2 is query-signed (signature in the URL); the canonical signal is
// the AccessKeyId / Signature / Expires query trio. AWS SigV4 would
// instead use X-Amz-Algorithm — a mismatch here indicates a wire-dialect
// bug.
func TestSign_OBS_v2_AuthHeader(t *testing.T) {
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
	if q.Get("AWSAccessKeyId") != "HWTestAccessKeyIdxxxxxxxxxxxxxxxx" {
		t.Errorf("AWSAccessKeyId: got %q, want test AK", q.Get("AWSAccessKeyId"))
	}
	if got := q.Get("X-Amz-Algorithm"); got != "" {
		t.Errorf("unexpected X-Amz-Algorithm=%q on OBS-signed URL — wire dialect leak", got)
	}
}

// TestErrorMap_VendorErrorCode feeds a synthetic obs.ObsError into
// mapError and verifies the right uos.Code is returned.
func TestErrorMap_VendorErrorCode(t *testing.T) {
	t.Parallel()
	obsErr := obs.ObsError{
		BaseModel: obs.BaseModel{
			StatusCode: 404,
			RequestId:  "obs-test-rid",
		},
		Code:    "NoSuchBucket",
		Message: "the specified bucket does not exist",
	}
	mapped := mapError(providerID, "StatBucket", "missing-bucket", "", obsErr)
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
	if uosErr.RequestID != "obs-test-rid" {
		t.Errorf("RequestID: got %q, want obs-test-rid", uosErr.RequestID)
	}
}
