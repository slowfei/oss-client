// direct_grant_upyun demonstrates the Upyun FORM upload authorization shape
// via Signer.IssueDirectGrant — the M5 validation moment for
// DirectGrantModeForm, the LAST of the 4 frozen DirectGrantMode values
// exercised in production.
//
// Unlike REST-PUT, Upyun upload authorisation is FORM-based: the caller
// POSTs a multipart/form-data payload carrying a base64-encoded JSON policy
// and a signed "authorization" field to the Upyun upload endpoint.
// IssueDirectGrant returns a *uos.DirectGrant{Mode: DirectGrantModeForm}
// that describes every field the caller needs to construct that POST — no
// vendor SDK required on the client side.
//
// This example shows the CALLER-SIDE shape; it does not issue a real upload
// (which requires real Upyun service credentials). Placeholder credentials
// are accepted and will produce a structurally valid grant whose signature
// is based on the placeholder password — useful for integration wiring
// without a live Upyun account.
//
// Run with real credentials:
//
//	export OMC_UPYUN_DEMO_BUCKET=my-service-name
//	export OMC_UPYUN_DEMO_OPERATOR=my-operator
//	export OMC_UPYUN_DEMO_PASSWORD=my-operator-password
//	go run .
//
// Run with placeholders (structural demo only):
//
//	go run .
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"

	// Side-effect import registers the Upyun Factory on uos.DefaultRegistry.
	_ "github.com/maqian/oss-client/providers/upyun"
	upyunprovider "github.com/maqian/oss-client/providers/upyun"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bucket := envOr("OMC_UPYUN_DEMO_BUCKET", "my-upyun-service")
	operator := envOr("OMC_UPYUN_DEMO_OPERATOR", "demo-operator")
	password := envOr("OMC_UPYUN_DEMO_PASSWORD", "demo-password")

	usingPlaceholders := os.Getenv("OMC_UPYUN_DEMO_BUCKET") == ""
	if usingPlaceholders {
		fmt.Println("NOTE: using placeholder credentials — grant signature is structurally valid but not accepted by real Upyun.")
		fmt.Println("      Set OMC_UPYUN_DEMO_BUCKET, OMC_UPYUN_DEMO_OPERATOR, OMC_UPYUN_DEMO_PASSWORD for live validation.")
		fmt.Println()
	}

	// Open the Upyun client.
	//
	// Key design points:
	//  - DriverConfig.Bucket IS the Upyun "service name" (1:1 mapping).
	//    Services are provisioned via the Upyun console; there is no
	//    programmatic CreateBucket — the driver returns ErrUnsupported for that.
	//  - AuthCustom (recommended) uses Upyun Unified-Authorization (HMAC-SHA1).
	//    AuthSharedKey is the basic-auth fallback (deprecated, not recommended).
	//  - The SDK MD5s the password before signing — callers MUST NOT pre-hash.
	creds := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthCustom,
		Opaque: &upyunprovider.OperatorCredential{
			Operator: operator,
			Password: password,
		},
	})
	cfg := uos.Config{
		Provider: "upyun",
		DriverConfig: &upyunprovider.DriverConfig{
			Bucket: bucket,
		},
		CredentialProvider: creds,
	}

	cli, err := uos.DefaultRegistry().Open(ctx, cfg)
	if err != nil {
		log.Fatalf("Open: %v", err)
	}
	defer cli.Close()

	fmt.Printf("opened upyun client → service=%s operator=%s\n\n", bucket, operator)

	// -------------------------------------------------------------------------
	// FORM upload grant — DirectGrantModeForm (M5 validation moment)
	// -------------------------------------------------------------------------
	//
	// IssueDirectGrant(Operation=Upload) returns Mode=DirectGrantModeForm.
	// The grant carries:
	//   FormFields["policy"]        — base64-encoded JSON upload policy
	//   FormFields["authorization"] — UpYun <operator>:<HMAC-SHA1-sig>
	//   Headers["Authorization"]    — same authorization value (redundant but
	//                                 correct per Upyun's unified-auth spec)
	//   URL                         — https://v0.api.upyun.com/<service>
	//   Method                      — POST
	//
	// The 6 vendor-specific Extra keys Upyun recognises:
	//   "notify-url"          → policy.notify-url (async callback)
	//   "apps"                → policy.apps (pre-treatment JSON array)
	//   "expiration-override" → policy.expiration (Unix seconds; overrides ExpiresIn)
	//   "save-key"            → policy.save-key (overrides Key)
	//   "content-md5"         → form-field content-md5 (whole-object integrity)
	//   "allow-file-type"     → policy.allow-file-type (overrides ContentType)
	grant, err := cli.Signer(bucket).IssueDirectGrant(ctx, uos.DirectGrantRequest{
		Key:         "uploads/2026/photo.jpg",
		Operation:   uos.DirectGrantUpload,
		ExpiresIn:   30 * time.Minute,
		MaxBytes:    10 * 1024 * 1024, // 10 MiB → policy.content-length-range
		ContentType: "image/jpeg",     // → policy.allow-file-type
		Extra: map[string]string{
			"notify-url": "https://my.app/upyun-notify",
		},
	})
	if err != nil {
		log.Fatalf("IssueDirectGrant(upload): %v", err)
	}

	fmt.Println("=== DirectGrant (Mode=Form, Operation=Upload) ===")
	fmt.Printf("  Mode:      %s\n", grant.Mode)
	fmt.Printf("  URL:       %s\n", grant.URL)
	fmt.Printf("  Method:    %s\n", grant.Method)
	fmt.Printf("  ExpiresAt: %s\n", grant.ExpiresAt.Format(time.RFC3339))
	fmt.Println()

	fmt.Println("  Headers:")
	printHeader(grant.Headers)

	fmt.Println("  FormFields:")
	printMap(grant.FormFields)
	fmt.Println()

	// -------------------------------------------------------------------------
	// Equivalent curl command
	// -------------------------------------------------------------------------
	fmt.Println("=== Equivalent curl command ===")
	fmt.Printf("  curl -F \"policy=%s\" \\\n", truncate(grant.FormFields["policy"], 40))
	fmt.Printf("       -F \"authorization=%s\" \\\n", truncate(grant.FormFields["authorization"], 40))
	fmt.Printf("       -F \"file=@local.jpg\" \\\n")
	fmt.Printf("       %s\n", grant.URL)
	fmt.Println()

	// -------------------------------------------------------------------------
	// Mixed Signer dispatch: Download → SignURL (not IssueDirectGrant)
	// -------------------------------------------------------------------------
	//
	// Upyun's download authorization is URL-shaped (a `_upt` query parameter).
	// IssueDirectGrant(Operation=Download) returns ErrUnsupported{CapDirectGrant}
	// pointing at SignURL by design — this is the mixed-Signer-dispatch model.
	fmt.Println("=== Mixed Signer dispatch: Download via SignURL ===")
	_, dlErr := cli.Signer(bucket).IssueDirectGrant(ctx, uos.DirectGrantRequest{
		Key:       "uploads/2026/photo.jpg",
		Operation: uos.DirectGrantDownload,
		ExpiresIn: 30 * time.Minute,
	})
	if dlErr != nil {
		var ue *uos.Error
		if errors.As(dlErr, &ue) && ue.Code == uos.ErrUnsupported {
			fmt.Printf("  IssueDirectGrant(download) → ErrUnsupported (capability=%s)\n", ue.Capability)
			fmt.Printf("  Message: %s\n", ue.Message)
		} else {
			fmt.Printf("  IssueDirectGrant(download) → unexpected error: %v\n", dlErr)
		}
	} else {
		fmt.Println("  IssueDirectGrant(download) → unexpectedly succeeded (should be ErrUnsupported)")
	}
	fmt.Println()

	// Show the correct download path: SignURL(GET).
	signed, signErr := cli.Signer(bucket).SignURL(ctx, uos.SignURLRequest{
		Key:       "uploads/2026/photo.jpg",
		Method:    http.MethodGet,
		ExpiresIn: 30 * time.Minute,
	})
	if signErr != nil {
		fmt.Printf("  SignURL(GET) → error: %v\n", signErr)
	} else {
		fmt.Printf("  SignURL(GET) → %s\n", truncate(signed.URL, 80))
		fmt.Printf("  ExpiresAt:    %s\n", signed.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Println()

	fmt.Println("direct_grant_upyun OK — see README.md for the full educational story")
}

// printHeader prints an http.Header sorted by key.
func printHeader(h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Printf("    %s: %s\n", k, truncate(v, 60))
		}
	}
}

// printMap prints a string map sorted by key.
func printMap(m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("    %s: %s\n", k, truncate(m[k], 60))
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
