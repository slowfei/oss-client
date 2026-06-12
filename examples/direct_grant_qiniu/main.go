// direct_grant_qiniu demonstrates the Qiniu Upload Token flow via the unified
// Signer.IssueDirectGrant API. The key insight this example illustrates is
// that Qiniu's write authorization is a bearer-token grant (Mode=Token), not
// a URL-shaped grant (Mode=URL) — which means callers POST the token as a
// multipart form field rather than embedding it in a URL.
//
// This is the M5 DirectGrantModeToken validation moment: the same Mode that
// Azure SAS uses (also Mode=Token) but with a fundamentally different wire
// shape (opaque bearer string POSTed to an upload endpoint vs. a URL query
// string handed directly to the storage endpoint).
//
// The example does NOT need real Qiniu credentials. With placeholder values
// the generated Upload Token is cryptographically valid but will not
// authenticate against Qiniu Kodo. Setting the OMC_QINIU_DEMO_* env vars
// yields a fully-authenticated token that a real Qiniu bucket would accept.
//
// Run:
//
//	go run .
//
// See README.md for the full walkthrough and expected output.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"

	// Side-effect import registers the Qiniu Factory on uos.DefaultRegistry.
	_ "github.com/slowfei/oss-client/providers/qiniu"

	qiniu "github.com/slowfei/oss-client/providers/qiniu"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// ------------------------------------------------------------------ //
	// Configuration — env vars or structural placeholders.                 //
	// ------------------------------------------------------------------ //
	ak := envOr("OMC_QINIU_DEMO_KEY", "DEMO_AK")
	sk := envOr("OMC_QINIU_DEMO_SECRET", "DEMO_SK")
	bucket := envOr("OMC_QINIU_DEMO_BUCKET", "demo-bucket")
	domain := envOr("OMC_QINIU_DEMO_DOMAIN", "https://demo.example.com")
	region := envOr("OMC_QINIU_DEMO_REGION", "z0")

	usingPlaceholders := ak == "DEMO_AK"
	if usingPlaceholders {
		fmt.Println("# [placeholder mode] No OMC_QINIU_DEMO_* env vars set.")
		fmt.Println("# The Upload Token below is structurally correct but won't")
		fmt.Println("# authenticate against a real Qiniu bucket.")
		fmt.Println("#")
	}

	// ------------------------------------------------------------------ //
	// Open a Qiniu Client.                                                 //
	//                                                                      //
	// The qiniu driver uses AuthCustom with a *qiniu.Credentials payload.  //
	// All three token families (Upload, Download, Manage) are derived from //
	// the same AK/SK pair; no distinct credential type per operation.      //
	// ------------------------------------------------------------------ //
	creds := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthCustom,
		Opaque: &qiniu.Credentials{
			AccessKey: ak,
			SecretKey: sk,
		},
	})
	cfg := uos.Config{
		Provider:           "qiniu",
		Region:             region,
		CredentialProvider: creds,
		DriverConfig: &qiniu.DriverConfig{
			// Domain is required for Download grants and GetObject.
			// For Upload-only usage it can be empty.
			Domain:   domain,
			UseHTTPS: true,
		},
	}

	cli, err := uos.DefaultRegistry().Open(ctx, cfg)
	must(err, "Open")
	defer cli.Close()

	fmt.Printf("opened qiniu client  bucket=%s  region=%s  domain=%s\n\n",
		bucket, region, domain)

	// ------------------------------------------------------------------ //
	// IssueDirectGrant — Upload Token (Operation=upload)                  //
	//                                                                      //
	// The returned DirectGrant has:                                        //
	//   Mode   = DirectGrantModeToken  (opaque bearer string)             //
	//   URL    = upload host for the target region                        //
	//   Method = POST (multipart/form-data)                               //
	//   Token  = the Qiniu Upload Token (PutPolicy.UploadToken)           //
	// ------------------------------------------------------------------ //
	uploadGrant, err := cli.Signer(bucket).IssueDirectGrant(ctx, uos.DirectGrantRequest{
		Key:         "uploads/demo-image.jpg",
		Operation:   uos.DirectGrantUpload,
		ExpiresIn:   30 * time.Minute,
		MaxBytes:    10 * 1024 * 1024, // 10 MiB cap
		ContentType: "image/jpeg",
		Extra: map[string]string{
			// PutPolicy override knobs Qiniu recognises via req.Extra:
			"saveKey":      "uploads/$(uuid)", // rename-on-server
			"returnBody":   `{"key":"$(key)","hash":"$(etag)","size":$(fsize)}`,
			"callbackUrl":  "https://example.com/hooks/qiniu-upload",
			"callbackBody": `{"key":"$(key)","hash":"$(etag)"}`,
		},
	})
	must(err, "IssueDirectGrant(upload)")

	fmt.Println("=== Upload Token (DirectGrantModeToken) ===")
	fmt.Printf("  Mode      : %s\n", uploadGrant.Mode)
	fmt.Printf("  URL       : %s\n", uploadGrant.URL)
	fmt.Printf("  Method    : %s\n", uploadGrant.Method)
	fmt.Printf("  Token     : %s\n", truncate(uploadGrant.Token, 80))
	fmt.Printf("  ExpiresAt : %s\n", uploadGrant.ExpiresAt.Format(time.RFC3339))

	// Show the caller exactly how to use the grant: POST multipart/form-data
	// with the token as a form field named "token". This is the Qiniu wire
	// contract described at:
	// https://developer.qiniu.com/kodo/manual/1272/form-upload
	fmt.Println()
	fmt.Println("=== How a caller uses the Upload Token (curl) ===")
	fmt.Printf("  curl -X POST '%s' \\\n", uploadGrant.URL)
	fmt.Printf("       -F token='%s' \\\n", truncate(uploadGrant.Token, 40))
	fmt.Printf("       -F key='uploads/demo-image.jpg' \\\n")
	fmt.Printf("       -F file=@/path/to/local/image.jpg\n")
	fmt.Println()
	fmt.Println("  # The token field is the ONLY authorization credential.")
	fmt.Println("  # No Authorization header. No query-string signature.")
	fmt.Println("  # This is the Mode=Token dispatch shape: opaque bearer")
	fmt.Println("  # string carried in vendor-defined form field.")

	// ------------------------------------------------------------------ //
	// IssueDirectGrant — Download Token (Operation=download)              //
	//                                                                      //
	// Per the v0.1.1 patch: Qiniu's download authorization is a signed    //
	// URL (query-string signature on DriverConfig.Domain). The driver      //
	// returns Mode=DirectGrantModeURL so callers GET DirectGrant.URL        //
	// directly — no bearer token semantics apply.                          //
	// ------------------------------------------------------------------ //
	downloadGrant, err := cli.Signer(bucket).IssueDirectGrant(ctx, uos.DirectGrantRequest{
		Key:       "uploads/demo-image.jpg",
		Operation: uos.DirectGrantDownload,
		ExpiresIn: 30 * time.Minute,
	})
	must(err, "IssueDirectGrant(download)")

	fmt.Println()
	fmt.Println("=== Download URL (DirectGrantModeURL) ===")
	fmt.Printf("  Mode      : %s\n", downloadGrant.Mode)
	fmt.Printf("  URL       : %s\n", truncate(downloadGrant.URL, 100))
	fmt.Printf("  Method    : %s\n", downloadGrant.Method)
	fmt.Printf("  ExpiresAt : %s\n", downloadGrant.ExpiresAt.Format(time.RFC3339))

	fmt.Println()
	fmt.Println("=== How a caller uses the Download URL (curl) ===")
	fmt.Printf("  curl -X GET '%s'\n", truncate(downloadGrant.URL, 80))
	fmt.Println()
	fmt.Println("  # Mode=URL: the signed URL IS the grant.")
	fmt.Println("  # No additional headers or form fields needed.")

	// ------------------------------------------------------------------ //
	// Dispatch pattern — how business code handles both modes              //
	// ------------------------------------------------------------------ //
	fmt.Println()
	fmt.Println("=== Caller-side dispatch on grant.Mode ===")
	for _, g := range []*uos.DirectGrant{uploadGrant, downloadGrant} {
		switch g.Mode {
		case uos.DirectGrantModeToken:
			fmt.Printf("  Mode=token  → POST to %s with form field token=<Token>\n", g.URL)
		case uos.DirectGrantModeURL:
			fmt.Printf("  Mode=url    → %s %s directly (URL IS the grant)\n", g.Method, truncate(g.URL, 60))
		case uos.DirectGrantModeForm:
			fmt.Printf("  Mode=form   → POST multipart form fields to %s\n", g.URL)
		case uos.DirectGrantModeHeaders:
			fmt.Printf("  Mode=headers→ %s %s with custom headers\n", g.Method, g.URL)
		}
	}

	fmt.Println()
	fmt.Println("direct_grant_qiniu OK")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func must(err error, op string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", op, err)
		os.Exit(1)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
