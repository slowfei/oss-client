//go:build docker

// driver_test.go wires the providers/qiniu driver into pkg/testkit/contract's
// RunSuite. To exercise: from providers/qiniu, run
//
//	go test -tags=docker -short -count=1 ./...
//
// # Why TestRunSuite t.Skip's by default
//
// Qiniu Cloud Storage (Kodo) speaks a vendor-specific HTTP/REST dialect with
// its own signing scheme (Upload Token / Download Token / Manage Token —
// see package doc) that is fundamentally incompatible with MinIO's AWS SigV4
// protocol. There is no Kodo-compatible emulator image in the testcontainers
// catalogue. Running the full contract suite therefore requires real Qiniu
// credentials gated on cloud-nightly secrets.
//
// The end-to-end Qiniu contract suite runs against real Qiniu Kodo via the
// cloud-nightly workflow, gated on:
//   - OMC_QINIU_NIGHTLY_KEY    — Qiniu AccessKey (AK)
//   - OMC_QINIU_NIGHTLY_SECRET — Qiniu SecretKey (SK)
//   - OMC_QINIU_NIGHTLY_BUCKET — bucket name for the test run
//   - OMC_QINIU_NIGHTLY_ZONE   — Qiniu zone id (e.g. "z0", "z1", "z2")
//
// Optional:
//   - OMC_QINIU_NIGHTLY_DOMAIN — bound CDN/source domain for download paths
//
// Without those secrets this test SKIPs (not FAILs) — matching the M5
// exit-checklist rule.
//
// # What the test validates in PR gates
//
//   - TestSpawnMinIOSmoke: spinning MinIO via testcontainers works for the
//     qiniu module's transitive dep set (proves the testkit hoist + replace
//     directives are wired correctly even though MinIO can't speak Kodo auth).
//   - The driver Open() + Capabilities() + Close() lifecycle compiles and
//     the factory wiring is correct.
package contract_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/slowfei/oss-client/pkg/testkit/contract"
	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"
	qiniudrv "github.com/slowfei/oss-client/providers/qiniu"
)

// TestRunSuite is the M5 PR-gate entry point. It runs the conformance
// suite against real Qiniu Kodo when the OMC_QINIU_NIGHTLY_* env vars are
// set; otherwise it SKIPs with a clear reason pointing at the cloud-nightly
// workflow.
func TestRunSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker; -short skips contract suite")
	}

	cfg, ok := loadCloudConfig(t)
	if !ok {
		t.Skip("qiniu PR gate: real-Qiniu contract run requires " +
			"OMC_QINIU_NIGHTLY_KEY / OMC_QINIU_NIGHTLY_SECRET / " +
			"OMC_QINIU_NIGHTLY_BUCKET / OMC_QINIU_NIGHTLY_ZONE env vars; " +
			"absent — see cloud-nightly workflow. " +
			"Provider auth dialect (Qiniu Upload Token / Download Token / Manage Token) " +
			"is incompatible with MinIO (AWS SigV4); the testcontainers MinIO endpoint " +
			"cannot validate this driver's wire signatures.")
	}

	bucket := os.Getenv("OMC_QINIU_NIGHTLY_BUCKET")
	factory := qiniudrv.Factory()

	fut := contract.FactoryUnderTest{
		Provider:           "qiniu",
		Bucket:             bucket,
		BucketIsPreCreated: true, // cloud-nightly: caller owns OMC_QINIU_NIGHTLY_BUCKET
		Setup: func(ctx context.Context, t *testing.T) (uos.Client, func(), error) {
			t.Helper()
			c, err := factory.Open(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return c, func() { _ = c.Close() }, nil
		},
		SkipCases: map[string]string{
			// Qiniu has no S3-style versioning data plane; CapVersioning is
			// Unsupported (footnote 9 in provider_matrix.md).
			"TestRunSuite/object/versioning": "qiniu does not expose object versioning as a data-plane capability — see provider_matrix.md footnote 9",

			// Qiniu has no S3-style per-object ACL; the capability is
			// ExtensionOnly (footnote 7 in provider_matrix.md).
			"TestRunSuite/object/acl": "qiniu has no per-object ACL (CapObjectACL=ExtensionOnly); per-object access is via Upload/Download Token scope — see provider_matrix.md footnote 7",

			// SignedURLWrite is intentionally not supported (footnote 4):
			// callers must use IssueDirectGrant for write authorization.
			"TestRunSuite/signer/sign_url_put_round_trip": "qiniu write authorization is non-URL; use IssueDirectGrant — see provider_matrix.md footnote 4",

			// DirectGrant on Qiniu uses DirectGrantModeToken with the Upload
			// Token; the contract suite's URL/Form shape expectations do not
			// apply (cloud-nightly validates end-to-end via real upload).
			"TestRunSuite/signer/issue_direct_grant_shape": "qiniu IssueDirectGrant returns DirectGrantModeToken (Upload Token); contract suite shape expectations may differ — cloud-nightly validates end-to-end",

			// Multipart resume requires a persisted StateStore; deferred to v0.2.
			"TestRunSuite/multipart/resume_after_failure": "M1 stub; transfer.Manager StateStore wiring lands in v0.2",

			// Qiniu RUv2 has no server-side listing of in-flight uploads
			// across processes; List returns in-process sessions only —
			// mirrors the gcs / azure pattern documented in Lessons (M4).
			"TestRunSuite/multipart/list_uploads": "qiniu RUv2 has no cross-process upload listing; in-process only — see Lessons (M5)",
		},
		SkipCodes: []uos.Code{},
	}

	contract.RunSuite(t, fut)
}

// TestSpawnMinIOSmoke validates the testcontainers wiring for the qiniu
// module without attempting any auth-required wire calls. Proves the
// transitive dependency set (testkit + Docker + containerd + OTel) resolves
// from providers/qiniu/go.mod and the MinIO image is reachable.
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

// loadCloudConfig assembles a uos.Config from the OMC_QINIU_NIGHTLY_*
// environment variables. Returns ok=false (without erroring) when the
// minimum required vars are unset so the caller can t.Skip cleanly.
//
// Required: OMC_QINIU_NIGHTLY_KEY, OMC_QINIU_NIGHTLY_SECRET,
// OMC_QINIU_NIGHTLY_BUCKET, OMC_QINIU_NIGHTLY_ZONE.
//
// Optional: OMC_QINIU_NIGHTLY_DOMAIN — sets DriverConfig.Domain so the
// download / SignURL paths are exercised.
func loadCloudConfig(t *testing.T) (uos.Config, bool) {
	t.Helper()
	ak := os.Getenv("OMC_QINIU_NIGHTLY_KEY")
	sk := os.Getenv("OMC_QINIU_NIGHTLY_SECRET")
	bucket := os.Getenv("OMC_QINIU_NIGHTLY_BUCKET")
	zone := os.Getenv("OMC_QINIU_NIGHTLY_ZONE")
	if ak == "" || sk == "" || bucket == "" || zone == "" {
		return uos.Config{}, false
	}

	cred := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthCustom,
		Opaque: &qiniudrv.Credentials{
			AccessKey: ak,
			SecretKey: sk,
		},
	})

	return uos.Config{
		Provider: "qiniu",
		Region:   zone,
		DriverConfig: &qiniudrv.DriverConfig{
			Region:   zone,
			Domain:   os.Getenv("OMC_QINIU_NIGHTLY_DOMAIN"),
			UseHTTPS: true,
		},
		CredentialProvider: cred,
	}, true
}
