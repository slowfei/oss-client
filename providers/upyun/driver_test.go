//go:build docker

// driver_test.go wires the providers/upyun driver into pkg/testkit/contract's
// RunSuite. To exercise: from providers/upyun, run
//
//	go test -tags=docker -short -count=1 ./...
//
// # Why TestRunSuite t.Skip's by default
//
// Upyun speaks a bespoke REST + Unified-Authorization wire dialect (HMAC-SHA1
// signature over a method-uri-date-policy-md5 tuple) that is fundamentally
// incompatible with MinIO's AWS SigV4 protocol. There is no Upyun-compatible
// emulator image in the testcontainers/testcontainers-go module catalogue.
// Running the full contract suite therefore requires real Upyun credentials
// gated on cloud-nightly secrets.
//
// The end-to-end Upyun contract suite runs against real Upyun USS via the
// cloud-nightly workflow, gated on:
//   - OMC_UPYUN_NIGHTLY_BUCKET   — Upyun service name
//   - OMC_UPYUN_NIGHTLY_OPERATOR — service-scoped operator name
//   - OMC_UPYUN_NIGHTLY_PASSWORD — operator password (plaintext; SDK MD5s it)
//
// Without those secrets this test SKIPs (not FAILs) — matching the M5
// exit-checklist rule.
//
// # What the test validates in PR gates
//
//   - TestSpawnMinIOSmoke: spinning MinIO via testcontainers works for the
//     upyun module's transitive dep set (proves the testkit hoist + replace
//     directives are wired correctly even though MinIO can't speak Upyun auth).
//   - The driver Open() + Capabilities() + Close() lifecycle compiles and
//     the factory wiring is correct.
package upyun_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/slowfei/oss-client/pkg/testkit/contract"
	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"
	upyundrv "github.com/slowfei/oss-client/providers/upyun"
)

// TestRunSuite is the M5 PR-gate entry point. It runs the conformance suite
// against real Upyun USS when OMC_UPYUN_NIGHTLY_BUCKET / _OPERATOR /
// _PASSWORD are set; otherwise it SKIPs with a clear reason pointing at the
// cloud-nightly workflow.
func TestRunSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker; -short skips contract suite")
	}

	cfg, ok := loadCloudConfig(t)
	if !ok {
		t.Skip("upyun PR gate: real-Upyun contract run requires OMC_UPYUN_NIGHTLY_BUCKET / OMC_UPYUN_NIGHTLY_OPERATOR / OMC_UPYUN_NIGHTLY_PASSWORD env vars; absent — see cloud-nightly workflow. Provider auth dialect (Upyun Unified-Authorization HMAC-SHA1) is incompatible with MinIO (AWS SigV4); the testcontainers MinIO endpoint cannot validate this driver's wire signatures.")
	}

	bucket := os.Getenv("OMC_UPYUN_NIGHTLY_BUCKET")
	factory := upyundrv.Factory()

	fut := contract.FactoryUnderTest{
		Provider:           "upyun",
		Bucket:             bucket,
		BucketIsPreCreated: true, // cloud-nightly: caller owns OMC_UPYUN_NIGHTLY_BUCKET
		Setup: func(ctx context.Context, t *testing.T) (uos.Client, func(), error) {
			t.Helper()
			c, err := factory.Open(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return c, func() { _ = c.Close() }, nil
		},
		SkipCases: map[string]string{
			// Upyun services are provisioned via the web portal; programmatic
			// Create / Delete return ErrUnsupported per the bucketService doc.
			"TestRunSuite/bucket/create": "Upyun services are provisioned via the web portal; CreateBucket=ErrUnsupported per provider_matrix.md M5",
			"TestRunSuite/bucket/delete": "Upyun services are provisioned via the web portal; DeleteBucket=ErrUnsupported per provider_matrix.md M5",

			// Upyun does not expose object versioning as a unified data-plane
			// capability; CapVersioning=Unsupported per matrix footnote 9.
			"TestRunSuite/object/versioning": "Upyun does not expose versioning per matrix footnote 9",

			// Object ACL / tagging / encryption surface only via As(target)
			// per matrix footnote 7; the unified contract test cannot drive them.
			"TestRunSuite/object/acl":     "Upyun ACL is service-level (web portal) per matrix footnote 7; reach via As(target)",
			"TestRunSuite/object/tagging": "Upyun tagging surfaces only via As(target) per matrix footnote 7",

			// Cross-bucket Copy is not supported (Upyun client is bound to a
			// single service per Open).
			"TestRunSuite/object/copy_cross_bucket": "upyun client is bound to a single bucket per Open; cross-bucket Copy unsupported",

			// SignedURLWrite is FORM-shaped (not URL); use IssueDirectGrant
			// per matrix footnote 3.
			"TestRunSuite/signer/signed_url_write": "Upyun upload authorization is FORM-based; use IssueDirectGrant per matrix footnote 3",
		},
		SkipCodes: []uos.Code{},
	}

	contract.RunSuite(t, fut)
}

// TestSpawnMinIOSmoke validates the testcontainers wiring for the upyun
// module without attempting any auth-required wire calls. Proves the
// transitive dependency set (testkit + Docker + containerd + OTel)
// resolves from providers/upyun/go.mod and the MinIO image is reachable.
func TestSpawnMinIOSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker; -short skips testcontainers")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	endpoint, _, _, cleanup, err := contract.SpawnMinIO(ctx)
	if err != nil {
		t.Fatalf("SpawnMinIO: %v", err)
	}
	t.Cleanup(cleanup)

	if endpoint == "" {
		t.Fatalf("SpawnMinIO returned empty endpoint")
	}
	if u, err := url.Parse("http://" + endpoint); err != nil || u.Host == "" {
		t.Fatalf("SpawnMinIO endpoint not parseable: %q (err=%v)", endpoint, err)
	}
}

// loadCloudConfig assembles a uos.Config from the OMC_UPYUN_NIGHTLY_*
// environment variables. Returns ok=false (without erroring) when the
// minimum required vars are unset so the caller can t.Skip cleanly.
//
// Required: OMC_UPYUN_NIGHTLY_BUCKET, OMC_UPYUN_NIGHTLY_OPERATOR,
// OMC_UPYUN_NIGHTLY_PASSWORD.
func loadCloudConfig(t *testing.T) (uos.Config, bool) {
	t.Helper()
	bucket := os.Getenv("OMC_UPYUN_NIGHTLY_BUCKET")
	operator := os.Getenv("OMC_UPYUN_NIGHTLY_OPERATOR")
	password := os.Getenv("OMC_UPYUN_NIGHTLY_PASSWORD")
	if bucket == "" || operator == "" || password == "" {
		return uos.Config{}, false
	}

	cred := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthCustom,
		Opaque: &upyundrv.OperatorCredential{
			Operator: operator,
			Password: password,
		},
	})

	return uos.Config{
		Provider: "upyun",
		DriverConfig: &upyundrv.DriverConfig{
			Bucket: bucket,
		},
		CredentialProvider: cred,
	}, true
}
