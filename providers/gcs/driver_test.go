//go:build docker

// driver_test.go wires the providers/gcs driver into pkg/testkit/contract's
// RunSuite using an in-process fake-gcs-server as the GCS JSON API emulator.
// No Docker is required for this test — the fake-gcs-server listens on an
// httptest.Server in the same process.
//
// To exercise: from providers/gcs, run
//
//	go test -tags=docker -count=1 -timeout=180s -cover ./...
//
// # Emulator approach
//
// fake-gcs-server (github.com/fsouza/fake-gcs-server) implements the GCS JSON
// API (storage/v1/) in-process, so the cloud.google.com/go/storage SDK can
// talk to it without modification. The driver is opened with:
//   - DriverConfig.EmulatorEndpoint set to the local server URL
//   - DriverConfig.ProjectID set to "test-project" (arbitrary; emulator accepts any)
//   - No CredentialProvider — the factory's emulator path injects
//     option.WithoutAuthentication() automatically when EmulatorEndpoint is
//     set and CredentialProvider is nil.
//
// # Cloud-nightly
//
// The cloud-nightly workflow (.github/workflows/cloud-nightly.yml) continues to
// run the suite against real GCS using OMC_GCS_NIGHTLY_KEY / _BUCKET / _PROJECT
// environment variables. The loadCloudConfig helper below supports that path.
package gcs_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/maqian/oss-client/pkg/testkit/contract"
	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
	gcsdrv "github.com/maqian/oss-client/providers/gcs"
)

// TestRunSuite runs the conformance suite against the in-process
// fake-gcs-server emulator. When the OMC_GCS_NIGHTLY_* env vars are present
// it runs against real GCS instead (cloud-nightly path).
func TestRunSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("requires emulator startup; -short skips")
	}

	// Cloud-nightly: prefer real GCS when credentials are present.
	if cfg, ok := loadCloudConfig(t); ok {
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
			SkipCases: gcsCloudSkipCases(),
		}
		contract.RunSuite(t, fut)
		return
	}

	// PR-gate path: use in-process fake-gcs-server.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	endpoint, cleanup, err := contract.SpawnFakeGCS(ctx)
	if err != nil {
		t.Fatalf("SpawnFakeGCS: %v", err)
	}
	t.Cleanup(cleanup)

	fut := contract.FactoryUnderTest{
		Provider: "gcs",
		Bucket:   "uos-contract-test",
		Endpoint: endpoint,
		Setup: func(ctx context.Context, t *testing.T) (uos.Client, func(), error) {
			t.Helper()
			c, err := gcsdrv.Factory().Open(ctx, uos.Config{
				Provider: "gcs",
				DriverConfig: &gcsdrv.DriverConfig{
					ProjectID:        "test-project",
					EmulatorEndpoint: endpoint,
				},
				// No CredentialProvider: factory injects WithoutAuthentication()
				// automatically when EmulatorEndpoint is set and no cred is given.
			})
			if err != nil {
				return nil, nil, err
			}
			return c, func() { _ = c.Close() }, nil
		},
		SkipCases: gcsEmulatorSkipCases(),
	}

	contract.RunSuite(t, fut)
}

// TestSpawnMinIOSmoke validates that the testcontainers wiring still resolves
// correctly from the gcs module's transitive dependency set. This exercises
// the MinIO smoke path that previously served as the only PR-gate test.
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

// gcsEmulatorSkipCases returns the SkipCases map for fake-gcs-server runs.
// Only cases with documented emulator limitations are skipped.
func gcsEmulatorSkipCases() map[string]string {
	return map[string]string{
		// fake-gcs-server does not validate V4 signatures on presigned URL
		// round-trips. The URL is structurally generated but HTTP requests
		// against it fail because the emulator does not verify signing keys.
		"TestRunSuite/signer/sign_url_get_round_trip": "fake-gcs-server: HTTP GET against presigned URL fails (emulator does not validate V4 signatures)",
		"TestRunSuite/signer/sign_url_put_round_trip": "fake-gcs-server: HTTP PUT against presigned URL fails (emulator does not validate V4 signatures)",

		// IssueDirectGrant is Unsupported for GCS (uses presigned PUT instead).
		// The capability suite asserts ErrUnsupported; the signer/shape case
		// expects a non-error grant body.
		"TestRunSuite/signer/issue_direct_grant_shape": "GCS uses presigned URL; CapDirectGrant=Unsupported per matrix footnote 5",

		// GCS resumable-upload registry is in-process only; List always returns
		// empty. See multipartService doc comment in driver.go.
		"TestRunSuite/multipart/list_uploads":         "GCS in-process resumable-upload registry has no server-side listing; List always returns empty",
		"TestRunSuite/multipart/resume_after_failure": "M1 stub; transfer.Manager StateStore wiring lands in v0.2",
	}
}

// gcsCloudSkipCases returns the SkipCases map for real-GCS (cloud-nightly) runs.
func gcsCloudSkipCases() map[string]string {
	return map[string]string{
		"TestRunSuite/signer/issue_direct_grant_shape": "GCS uses presigned URL; CapDirectGrant=Unsupported per matrix footnote 5",
		"TestRunSuite/multipart/resume_after_failure":  "GCS resumable-upload registry is in-process only; cross-process resume needs Client.As(target) — see M4 lessons",
		"TestRunSuite/multipart/list_uploads":          "GCS does not expose a multi-process queryable upload list; List always returns empty — see M4 lessons",
	}
}

// loadCloudConfig assembles a uos.Config from the OMC_GCS_NIGHTLY_*
// environment variables. Returns ok=false (without erroring) when the minimum
// required vars are unset so the caller can fall back to the emulator path.
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
