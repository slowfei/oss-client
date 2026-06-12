package upyun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	upyunsdk "github.com/upyun/go-sdk/v3/upyun"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/capability"
	"github.com/slowfei/oss-client/pkg/uos/credential"
)

// signing_test.go provides PR-gate signing-shape coverage for the upyun
// driver. Upyun does not speak any S3-compat dialect; the vendor
// TestRunSuite SKIPs against testcontainers MinIO. These tests validate
// the driver's wire-output shape — the FORM upload payload
// (DirectGrantModeForm) and the signed download URL (`_upt` token
// scheme) — WITHOUT reaching the wire.

// newTestClient constructs a driverImpl wired with placeholder
// credentials suitable for offline signing-shape assertions.
func newTestClient(t *testing.T) uos.Client {
	t.Helper()
	cli, err := factoryImpl{}.Open(context.Background(), uos.Config{
		Provider: providerID,
		CredentialProvider: credential.NewStatic(credential.Credential{
			Scheme: credential.AuthCustom,
			Opaque: &OperatorCredential{
				Operator: "test-operator",
				Password: "test-operator-password",
			},
		}),
		DriverConfig: &DriverConfig{Bucket: "test-bucket"},
	})
	if err != nil {
		t.Fatalf("factoryImpl.Open: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// TestSignURL_Read_Shape validates SignURL(GET) returns an Upyun
// `_upt`-bearing URL pointing at the per-bucket CDN host.
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
		t.Errorf("Method: got %q, want GET", signed.Method)
	}
	if !strings.Contains(signed.URL, "test-bucket.b0.upaiyun.com") {
		t.Errorf("URL: got %q, expected to contain Upyun CDN host", signed.URL)
	}
	if !strings.Contains(signed.URL, "obj.txt") {
		t.Errorf("URL: got %q, expected to contain key", signed.URL)
	}
	if !strings.Contains(signed.URL, "_upt=") {
		t.Errorf("URL: got %q, expected `_upt=` query param (Upyun UPT-V1 dialect)", signed.URL)
	}
}

// TestSignURL_Write_Unsupported asserts that SignURL with PUT/POST
// returns ErrUnsupported{CapSignedURLWrite}, directing the caller to
// IssueDirectGrant (Upyun upload authorization is FORM-based, not URL).
func TestSignURL_Write_Unsupported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	for _, method := range []string{"PUT", "POST"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			_, err := cli.Signer("test-bucket").SignURL(ctx, uos.SignURLRequest{
				Method:    method,
				Key:       "obj.txt",
				ExpiresIn: time.Hour,
			})
			if err == nil {
				t.Fatalf("SignURL(%s): expected error, got nil", method)
			}
			var uosErr *uos.Error
			if !errors.As(err, &uosErr) {
				t.Fatalf("SignURL(%s): expected *uos.Error, got %T (%v)", method, err, err)
			}
			if uosErr.Code != uos.ErrUnsupported {
				t.Errorf("Code: got %q, want %q", uosErr.Code, uos.ErrUnsupported)
			}
			if uosErr.Capability != capability.CapSignedURLWrite {
				t.Errorf("Capability: got %q, want %q", uosErr.Capability, capability.CapSignedURLWrite)
			}
			if !strings.Contains(uosErr.Message, "DirectGrantModeForm") {
				t.Errorf("Message: %q does not mention DirectGrantModeForm", uosErr.Message)
			}
		})
	}
}

// TestIssueDirectGrant_FormUpload_Shape asserts the FORM upload grant
// returned for Operation=upload uses Mode=Form, the Upyun upload
// endpoint, POST method, and carries the policy + authorization fields
// in both Headers (Authorization) and FormFields. The Authorization
// header MUST start with "UpYun <op>:" (Upyun unified-auth signature
// shape); a wrong prefix here indicates the SDK's signer was bypassed.
func TestIssueDirectGrant_FormUpload_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	grant, err := cli.Signer("test-bucket").IssueDirectGrant(ctx, uos.DirectGrantRequest{
		Operation: uos.DirectGrantUpload,
		Key:       "obj.txt",
		ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueDirectGrant(upload): %v", err)
	}
	if grant.Mode != uos.DirectGrantModeForm {
		t.Errorf("Mode: got %q, want %q", grant.Mode, uos.DirectGrantModeForm)
	}
	if grant.Method != http.MethodPost {
		t.Errorf("Method: got %q, want %q (Upyun FORM upload uses POST)", grant.Method, http.MethodPost)
	}
	if grant.URL != "https://v0.api.upyun.com/test-bucket" {
		t.Errorf("URL: got %q, want %q (Upyun upload endpoint)", grant.URL, "https://v0.api.upyun.com/test-bucket")
	}

	auth := grant.Headers.Get("Authorization")
	if auth == "" {
		t.Fatalf("Headers: missing Authorization; have=%v", grant.Headers)
	}
	if !strings.HasPrefix(auth, "UpYun test-operator:") {
		t.Errorf("Authorization: got %q, want %q prefix (Upyun unified-auth dialect)", auth, "UpYun test-operator:")
	}

	if grant.FormFields["policy"] == "" {
		t.Errorf("FormFields.policy: missing; want base64 policy; have=%v", grant.FormFields)
	}
	if grant.FormFields["authorization"] != auth {
		t.Errorf("FormFields.authorization: got %q, want %q (must mirror header)", grant.FormFields["authorization"], auth)
	}
	// The policy must be valid base64 of a JSON document containing
	// `bucket` and `save-key`. Decode and sanity-check those keys.
	policyJSON, err := base64.StdEncoding.DecodeString(grant.FormFields["policy"])
	if err != nil {
		t.Fatalf("policy is not valid base64: %v (raw=%q)", err, grant.FormFields["policy"])
	}
	var policy map[string]any
	if err := json.Unmarshal(policyJSON, &policy); err != nil {
		t.Fatalf("policy JSON unmarshal: %v (raw=%s)", err, policyJSON)
	}
	if got := policy["bucket"]; got != "test-bucket" {
		t.Errorf("policy.bucket: got %v, want %q", got, "test-bucket")
	}
	if got := policy["save-key"]; got != "obj.txt" {
		t.Errorf("policy.save-key: got %v, want %q", got, "obj.txt")
	}
}

// TestIssueDirectGrant_Download_Unsupported asserts that
// IssueDirectGrant with Operation=download returns
// ErrUnsupported{CapDirectGrant} (Upyun download authorization is
// URL-shaped via SignURL, not a DirectGrant).
func TestIssueDirectGrant_Download_Unsupported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	_, err := cli.Signer("test-bucket").IssueDirectGrant(ctx, uos.DirectGrantRequest{
		Operation: uos.DirectGrantDownload,
		Key:       "obj.txt",
		ExpiresIn: time.Hour,
	})
	if err == nil {
		t.Fatalf("IssueDirectGrant(download): expected error, got nil")
	}
	var uosErr *uos.Error
	if !errors.As(err, &uosErr) {
		t.Fatalf("IssueDirectGrant(download): expected *uos.Error, got %T (%v)", err, err)
	}
	if uosErr.Code != uos.ErrUnsupported {
		t.Errorf("Code: got %q, want %q", uosErr.Code, uos.ErrUnsupported)
	}
	if uosErr.Capability != capability.CapDirectGrant {
		t.Errorf("Capability: got %q, want %q", uosErr.Capability, capability.CapDirectGrant)
	}
	if !strings.Contains(uosErr.Message, "SignURL") {
		t.Errorf("Message: %q does not direct caller to SignURL", uosErr.Message)
	}
}

// TestErrorMap_VendorErrorCode feeds a synthetic *upyunsdk.Error into
// mapError and verifies the right uos.Code is returned. Upyun error
// codes are 8-digit numeric (40400001 = "file or directory not found").
func TestErrorMap_VendorErrorCode(t *testing.T) {
	t.Parallel()
	upErr := &upyunsdk.Error{
		StatusCode: http.StatusNotFound,
		Code:       40400001,
		Message:    "file or directory not found",
		Header:     http.Header{},
	}
	mapped := mapError(providerID, "HeadObject", "test-bucket", "missing-key", upErr)
	var uosErr *uos.Error
	if !errors.As(mapped, &uosErr) {
		t.Fatalf("mapError: expected *uos.Error, got %T (%v)", mapped, mapped)
	}
	if uosErr.Code != uos.ErrNotFound {
		t.Errorf("Code: got %q, want %q (Upyun 40400001 → ErrNotFound)", uosErr.Code, uos.ErrNotFound)
	}
	if uosErr.HTTPStatus != http.StatusNotFound {
		t.Errorf("HTTPStatus: got %d, want %d", uosErr.HTTPStatus, http.StatusNotFound)
	}
}
