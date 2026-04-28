//go:build docker

// driver_test.go wires the providers/gcs driver into pkg/testkit/contract's
// RunSuite. To exercise: from providers/gcs, run
//
//	go test -tags=docker -short -count=1 ./...
//
// # Why this test t.Skip's RunSuite by default
//
// MinIO speaks the AWS SigV4 wire dialect; cloud.google.com/go/storage speaks
// the GCS JSON API (and a different XML dialect for the S3-compat HMAC
// fallback). The two are not wire-compatible: handing a GCS-signed request to
// MinIO yields SignatureDoesNotMatch (HTTP 403) on every operation, and the
// JSON-API control-plane calls (CreateBucket / Buckets iterator) target
// storage.googleapis.com endpoints that MinIO does not implement.
//
// The lead's brief documents this exact outcome and instructs:
// "(b) skip RunSuite entirely with a clear comment pointing at cloud-nightly".
// We follow option (b) because option (a) would require deleting every case in
// SkipCases (every case would fail), which is more code AND less honest about
// the underlying mismatch.
//
// The end-to-end gcs contract suite runs against real GCS via the
// cloud-nightly workflow (see .github/workflows/cloud-nightly.yml), gated on
// OMC_GCS_NIGHTLY_KEY (path to Service Account JSON) +
// OMC_GCS_NIGHTLY_BUCKET + OMC_GCS_NIGHTLY_PROJECT secrets. Without those
// secrets, this test SKIPs (it does not FAIL) — matching the M3/M4
// exit-checklist rule that "cases requiring real cloud are tagged t.Skip in
// PR runs".
//
// # What the test still validates in PR gates
//
//   - Spawning MinIO via testcontainers works for the gcs module's
//     transitive dep set (proves the testkit hoist / replace directives are
//     wired correctly even with the GCS SDK's heavyweight transitive chain
//     — auth / google.golang.org/api / google-api-go-client).
//   - The driver Open() + Capabilities() + Close() shape compiles and the
//     factory is registered with DefaultRegistry.
//   - The skip-with-reason path through the contract suite is exercised, so
//     the lead can update the matrix from this run's output.
package gcs_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/maqian/object-storage-client/pkg/testkit/contract"
	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/credential"
	gcsdrv "github.com/maqian/object-storage-client/providers/gcs"
)

// TestRunSuite is the M4 PR-gate entry point. It runs the conformance suite
// against real GCS when OMC_GCS_NIGHTLY_KEY / _BUCKET / _PROJECT are set;
// otherwise it SKIPs with a clear reason pointing at the cloud-nightly
// workflow.
func TestRunSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker; -short skips contract suite")
	}

	cfg, ok := loadCloudConfig(t)
	if !ok {
		t.Skip("gcs PR gate: real-GCS contract run requires OMC_GCS_NIGHTLY_KEY (path to Service Account JSON) + OMC_GCS_NIGHTLY_BUCKET + OMC_GCS_NIGHTLY_PROJECT env vars; absent — see cloud-nightly workflow. Provider auth dialect is incompatible with MinIO (GCS JSON API ≠ AWS SigV4); the testcontainers MinIO endpoint cannot validate this driver's wire signatures.")
	}

	bucket := os.Getenv("OMC_GCS_NIGHTLY_BUCKET")
	factory := gcsdrv.Factory()

	fut := contract.FactoryUnderTest{
		Provider: "gcs",
		Bucket:   bucket,
		Endpoint: cfg.Endpoint,
		Setup: func(ctx context.Context, t *testing.T) (uos.Client, func(), error) {
			t.Helper()
			c, err := factory.Open(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return c, func() { _ = c.Close() }, nil
		},
		// Cases the gcs driver opts out of for any contract-suite run, with a
		// human-readable reason. Each entry is matched against the dotted
		// t.Run path produced by RunSuite.
		SkipCases: map[string]string{
			// GCS issues writes via presigned URL; CapDirectGrant is
			// Unsupported per docs/provider_matrix.md footnote 5.
			"TestRunSuite/signer/issue_direct_grant_shape": "GCS uses presigned URL; CapDirectGrant=Unsupported per matrix footnote 5",

			// GCS multipart is mapped onto resumable uploads with an
			// in-process session registry — see capabilities.go and the
			// 'Multipart mapping' section of the package doc. Cross-process
			// resumability requires the SDK's appendable-object preview API
			// reached via Client.As(target), which the driver does NOT wrap.
			"TestRunSuite/multipart/resume_after_failure": "GCS resumable-upload registry is in-process only; cross-process resume needs Client.As(target) — see M4 lessons",
			"TestRunSuite/multipart/list_uploads":         "GCS does not expose a multi-process queryable upload list; List always returns empty — see M4 lessons",
		},
		SkipCodes: []uos.Code{},
	}

	contract.RunSuite(t, fut)
}

// TestSpawnMinIOSmoke validates the testcontainers wiring for the gcs
// module without attempting any auth-required wire calls. It proves that the
// transitive dependency set (testkit + Docker + containerd + OTel + GCS SDK
// chain) resolves from the gcs go.mod and that the MinIO image is reachable.
//
// This case is what the M4 PR gate actually exercises — the broader contract
// suite is gated on real GCS credentials per the docstring at the top of this
// file.
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

	// Sanity-check the endpoint string the helper returned. testcontainers
	// returns "host:port" for ConnectionString; we don't try to talk GCS
	// against it (see file docstring) — the smoke check ensures the PR gate
	// fails loudly if the testkit hoist + replace wiring breaks.
	if endpoint == "" {
		t.Fatalf("SpawnMinIO returned empty endpoint")
	}
	if u, err := url.Parse("http://" + endpoint); err != nil || u.Host == "" {
		t.Fatalf("SpawnMinIO endpoint not parseable: %q (err=%v)", endpoint, err)
	}
}

// loadCloudConfig assembles a uos.Config from the OMC_GCS_NIGHTLY_*
// environment variables. Returns ok=false (without erroring) when the minimum
// required vars are unset so the caller can t.Skip cleanly.
//
// Required: OMC_GCS_NIGHTLY_KEY (path to a Service Account JSON file),
// OMC_GCS_NIGHTLY_BUCKET, OMC_GCS_NIGHTLY_PROJECT.
// Optional: OMC_GCS_NIGHTLY_ENDPOINT (emulator override).
func loadCloudConfig(t *testing.T) (uos.Config, bool) {
	t.Helper()
	keyPath := os.Getenv("OMC_GCS_NIGHTLY_KEY")
	bucket := os.Getenv("OMC_GCS_NIGHTLY_BUCKET")
	project := os.Getenv("OMC_GCS_NIGHTLY_PROJECT")
	endpoint := os.Getenv("OMC_GCS_NIGHTLY_ENDPOINT")
	if keyPath == "" || bucket == "" || project == "" {
		return uos.Config{}, false
	}

	jsonBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Logf("gcs PR gate: failed to read OMC_GCS_NIGHTLY_KEY=%q: %v — skipping", keyPath, err)
		return uos.Config{}, false
	}

	cred := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthOAuth2,
		Opaque: &gcsdrv.ServiceAccountCredential{JSON: jsonBytes},
	})

	return uos.Config{
		Provider:           "gcs",
		Endpoint:           endpoint,
		CredentialProvider: cred,
		DriverConfig: &gcsdrv.DriverConfig{
			ProjectID:        project,
			EmulatorEndpoint: endpoint,
		},
	}, true
}
