package volcengine

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
)

// signing_test.go provides PR-gate signing-shape coverage for the
// volcengine driver. The vendor TestRunSuite SKIPs against testcontainers
// MinIO (TOS uses SigV4 service=tos, ≠ AWS SigV4 service=s3); these tests
// instead validate the driver's wire-output shape (URL host pattern,
// canonical-string service component, X-Amz-* query parameters) WITHOUT
// reaching the wire.

// newTestClient constructs a driverImpl wired with placeholder credentials
// suitable for offline signing-shape assertions.
func newTestClient(t *testing.T) uos.Client {
	t.Helper()
	cli, err := factoryImpl{}.Open(context.Background(), uos.Config{
		Provider: providerID,
		Region:   "cn-beijing",
		CredentialProvider: credential.NewStatic(credential.Credential{
			Scheme: credential.AuthHMAC,
			Opaque: &credential.EnvHMACCredential{
				AccessKeyID:     "AKLTTestAccessKeyIDxxxxxxxxxxxxxx",
				SecretAccessKey: "TestSecretAccessKeyVolcxxxxxxxxxxxxxxxxxxxx",
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
// pointing at the TOS virtual-host endpoint and carries the SigV4 query
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
	// TOS endpoint convention: tos-<region>.volces.com (note the literal
	// "tos-" prefix, distinct from AWS s3-<region> or alibaba oss-<region>;
	// concatenating the wrong prefix produces SignatureDoesNotMatch on
	// every request).
	wantHostSuffix := "tos-cn-beijing.volces.com"
	if !strings.Contains(u.Host, wantHostSuffix) {
		t.Errorf("Host: got %q, expected to contain %q (TOS endpoint convention)", u.Host, wantHostSuffix)
	}
	if !strings.Contains(u.Host, "test-bucket") && !strings.Contains(u.Path, "test-bucket") {
		t.Errorf("URL: got host=%q path=%q, expected bucket name in host or path", u.Host, u.Path)
	}
	if !strings.Contains(u.Path, "obj.txt") {
		t.Errorf("Path: got %q, expected to contain key", u.Path)
	}
}

// TestSignURL_Write_Shape verifies the volcengine driver supports
// SignURL for PUT (S3-family pattern).
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

// TestSign_TOS_SigV4_Service_TOS asserts the TOS SigV4 dialect carries
// `service=tos` in the X-Amz-Credential scope (NOT `service=s3`). This
// is the load-bearing wire-format distinction that makes the TOS
// signature mutually incompatible with AWS S3 endpoints — a regression
// here would route traffic to the wrong validator service.
func TestSign_TOS_SigV4_Service_TOS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cli := newTestClient(t)

	signed, err := cli.Signer("test-bucket").SignURL(ctx, uos.SignURLRequest{
		Method:    "GET",
		Key:       "service-shape.bin",
		ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatalf("SignURL: %v", err)
	}
	u, _ := url.Parse(signed.URL)
	q := u.Query()
	if got := q.Get("X-Tos-Algorithm"); got != "TOS4-HMAC-SHA256" {
		t.Errorf("X-Tos-Algorithm: got %q, want %q (TOS SigV4 dialect)", got, "TOS4-HMAC-SHA256")
	}
	cred := q.Get("X-Tos-Credential")
	if cred == "" {
		t.Fatalf("missing X-Tos-Credential query param (TOS SigV4 dialect); query=%q", u.RawQuery)
	}
	// Credential scope format: <ak>/<date>/<region>/<service>/request
	if !strings.Contains(cred, "/cn-beijing/tos/") {
		t.Errorf("X-Tos-Credential scope: got %q, expected /cn-beijing/tos/ segment (NOT /s3/)", cred)
	}
	if strings.Contains(cred, "/s3/") {
		t.Errorf("X-Tos-Credential scope contains /s3/: %q — wire dialect leak (TOS uses service=tos)", cred)
	}
}

// TestErrorMap_VendorErrorCode feeds a synthetic *tos.TosServerError
// into mapError and verifies the right uos.Code is returned.
// TosServerError embeds tos.TosError + tos.RequestInfo; the StatusCode
// + RequestID fields live on the embedded RequestInfo, not on the
// outer struct (which has the same field names exposed via promotion).
func TestErrorMap_VendorErrorCode(t *testing.T) {
	t.Parallel()
	svcErr := &tos.TosServerError{
		TosError: tos.TosError{
			Message: "the specified bucket does not exist",
		},
		RequestInfo: tos.RequestInfo{
			StatusCode: 404,
			RequestID:  "volc-test-rid",
		},
		Code: "NoSuchBucket",
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
	if uosErr.RequestID != "volc-test-rid" {
		t.Errorf("RequestID: got %q, want volc-test-rid", uosErr.RequestID)
	}
}
