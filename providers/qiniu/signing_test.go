package qiniu

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	qclient "github.com/qiniu/go-sdk/v7/client"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/capability"
	"github.com/slowfei/oss-client/pkg/uos/credential"
)

// signing_test.go provides PR-gate signing-shape coverage for the qiniu
// driver. Qiniu does not speak any S3-compat dialect, so the vendor
// TestRunSuite SKIPs against testcontainers MinIO; these tests instead
// validate the driver's wire-output shape — the Upload Token format
// (DirectGrantModeToken) and the signed download URL (DirectGrantModeURL)
// — WITHOUT reaching the wire.

// newTestClient constructs a driverImpl wired with placeholder
// credentials suitable for offline signing-shape assertions.
// DriverConfig.Domain is set so SignURL(GET) and DirectGrant(download)
// can build a private URL (Qiniu private-URL signing requires the
// bucket-bound CDN domain).
//
// DriverConfig.UploadEndpoint is set to a fixed value so DirectGrant
// (upload) returns a deterministic URL without invoking the SDK's
// per-region UpHost resolver (which makes a network call to qiniu's
// API host on first use).
func newTestClient(t *testing.T) uos.Client {
	t.Helper()
	cli, err := factoryImpl{}.Open(context.Background(), uos.Config{
		Provider: providerID,
		Region:   "z0",
		CredentialProvider: credential.NewStatic(credential.Credential{
			Scheme: credential.AuthCustom,
			Opaque: &Credentials{
				AccessKey: "QiniuTestAccessKey1234567890ABCDEFGHIJKL",
				SecretKey: "QiniuTestSecretKey1234567890ABCDEFGHIJKL",
			},
		}),
		DriverConfig: &DriverConfig{
			Region: "z0",
			// MakePrivateURL accepts the Domain verbatim and prepends it to
			// the resulting URL — supplying a bare host produces a
			// scheme-less URL that url.Parse cannot disambiguate, so we
			// pass the full https:// origin (the SDK preserves it).
			Domain:         "https://test-bucket.example.com",
			UploadEndpoint: "https://upload.qiniup.com",
		},
	})
	if err != nil {
		t.Fatalf("factoryImpl.Open: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// TestSignURL_Read_Shape validates SignURL(GET) returns a Qiniu private
// URL bound to DriverConfig.Domain and carrying the `e` (expires) and
// `token` query params (the canonical Qiniu private-URL shape).
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
	if u.Host != "test-bucket.example.com" {
		t.Errorf("Host: got %q, want %q (DriverConfig.Domain)", u.Host, "test-bucket.example.com")
	}
	if !strings.Contains(u.Path, "obj.txt") {
		t.Errorf("Path: got %q, expected to contain key", u.Path)
	}
	q := u.Query()
	if q.Get("e") == "" {
		t.Errorf("missing `e` (expires) query param (Qiniu private-URL dialect); query=%q", u.RawQuery)
	}
	if q.Get("token") == "" {
		t.Errorf("missing `token` query param (Qiniu private-URL dialect); query=%q", u.RawQuery)
	}
}

// TestSignURL_Write_Unsupported asserts that SignURL with PUT/POST
// returns ErrUnsupported{CapSignedURLWrite} per the Qiniu driver's
// documented behaviour (write authorization is non-URL — the caller is
// directed to IssueDirectGrant instead).
func TestSignURL_Write_Unsupported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	for _, method := range []string{"PUT", "POST", "DELETE"} {
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
			if !strings.Contains(uosErr.Message, "IssueDirectGrant") {
				t.Errorf("Message: %q does not direct caller to IssueDirectGrant", uosErr.Message)
			}
		})
	}
}

// TestIssueDirectGrant_UploadToken_Shape asserts the Upload Token grant
// returned for Operation=upload uses Mode=Token with a 3-part Qiniu
// upload token (`<ak>:<sig>:<base64-policy>`), POST method, and an
// upload host URL.
func TestIssueDirectGrant_UploadToken_Shape(t *testing.T) {
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
	if grant.Mode != uos.DirectGrantModeToken {
		t.Errorf("Mode: got %q, want %q", grant.Mode, uos.DirectGrantModeToken)
	}
	if grant.Method != http.MethodPost {
		t.Errorf("Method: got %q, want %q (Qiniu form-upload uses POST)", grant.Method, http.MethodPost)
	}
	if grant.URL != "https://upload.qiniup.com" {
		t.Errorf("URL: got %q, want %q (DriverConfig.UploadEndpoint)", grant.URL, "https://upload.qiniup.com")
	}
	// Qiniu Upload Token format: <access-key>:<base64-signature>:<base64-policy>
	parts := strings.Split(grant.Token, ":")
	if len(parts) != 3 {
		t.Fatalf("Token: got %q (%d parts), want 3-part \"<ak>:<sig>:<base64-policy>\"", grant.Token, len(parts))
	}
	if parts[0] != "QiniuTestAccessKey1234567890ABCDEFGHIJKL" {
		t.Errorf("Token AK part: got %q, want test AK", parts[0])
	}
	if parts[1] == "" {
		t.Errorf("Token signature part is empty: %q", grant.Token)
	}
	if parts[2] == "" {
		t.Errorf("Token policy part is empty: %q", grant.Token)
	}
	if grant.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt: got zero, want non-zero")
	}
}

// TestIssueDirectGrant_DownloadURL_Shape asserts the Download grant
// returned for Operation=download uses Mode=URL (Qiniu's signed
// download is a URL-shaped bearer) with `e` and `token` query params,
// pointing at DriverConfig.Domain.
func TestIssueDirectGrant_DownloadURL_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	grant, err := cli.Signer("test-bucket").IssueDirectGrant(ctx, uos.DirectGrantRequest{
		Operation: uos.DirectGrantDownload,
		Key:       "obj.txt",
		ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueDirectGrant(download): %v", err)
	}
	if grant.Mode != uos.DirectGrantModeURL {
		t.Errorf("Mode: got %q, want %q", grant.Mode, uos.DirectGrantModeURL)
	}
	if grant.Method != http.MethodGet {
		t.Errorf("Method: got %q, want GET", grant.Method)
	}
	u, err := url.Parse(grant.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", grant.URL, err)
	}
	if u.Host != "test-bucket.example.com" {
		t.Errorf("Host: got %q, want %q (DriverConfig.Domain)", u.Host, "test-bucket.example.com")
	}
	q := u.Query()
	if q.Get("e") == "" {
		t.Errorf("missing `e` (expires) query param; query=%q", u.RawQuery)
	}
	if q.Get("token") == "" {
		t.Errorf("missing `token` query param; query=%q", u.RawQuery)
	}
}

// TestErrorMap_VendorErrorCode feeds a synthetic *qclient.ErrorInfo
// into mapError and verifies the right uos.Code is returned.
func TestErrorMap_VendorErrorCode(t *testing.T) {
	t.Parallel()
	ei := &qclient.ErrorInfo{
		Code:  http.StatusNotFound,
		Err:   "no such file or directory",
		Reqid: "qiniu-test-rid",
	}
	mapped := mapError(providerID, "HeadObject", "test-bucket", "missing-key", ei)
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
	if uosErr.RequestID != "qiniu-test-rid" {
		t.Errorf("RequestID: got %q, want qiniu-test-rid", uosErr.RequestID)
	}
}
