//go:build docker

package contract_test

import (
	"context"
	"testing"
	"time"

	"github.com/slowfei/oss-client/pkg/testkit/contract"
	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"

	// Side-effect import: the driver's init() registers itself on
	// uos.DefaultRegistry so we can also reach it via Registry.Open;
	// the local Open call below uses the typed Factory directly.
	driver "github.com/slowfei/oss-client/providers/minio"
)

// TestRunSuite spins up a MinIO container via testcontainers, points
// the minio-go driver at it, and runs the full pkg/testkit/contract
// conformance suite. The build tag (//go:build docker) keeps Docker
// off the default `go test ./...` path; run with `-tags=docker` to
// exercise this test.
func TestRunSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker; -short skips testcontainers")
	}

	// Spawn MinIO once for the whole suite; tear it down on test exit.
	spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer spawnCancel()
	endpoint, accessKey, secretKey, cleanup, err := contract.SpawnMinIO(spawnCtx)
	if err != nil {
		t.Fatalf("SpawnMinIO: %v", err)
	}
	t.Cleanup(cleanup)

	// Build a credential.Provider that returns the testcontainers root key.
	credProvider := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthHMAC,
		Opaque: &credential.EnvHMACCredential{
			AccessKeyID:     accessKey,
			SecretAccessKey: secretKey,
		},
	})

	cfg := uos.Config{
		Provider:           "minio",
		Endpoint:           "http://" + endpoint,
		Region:             "us-east-1",
		CredentialProvider: credProvider,
	}

	fut := contract.FactoryUnderTest{
		Provider: "minio",
		Bucket:   "uos-contract-test",
		Endpoint: "http://" + endpoint,
		Setup: func(ctx context.Context, t *testing.T) (uos.Client, func(), error) {
			t.Helper()
			c, err := driver.Factory{}.Open(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return c, func() { _ = c.Close() }, nil
		},
		// SkipCases keyed on the dotted t.Run path. Each entry has a
		// short reason so future maintainers know why the case is opted
		// out and when it should be re-enabled.
		SkipCases: map[string]string{
			// CapDirectGrant is Unsupported for the S3-family per
			// docs/provider_matrix.md footnote 5; the signer-shape case
			// expects a non-error grant and therefore can't run here.
			// The capability-gating case
			// (capability/unsupported_returns_typed_error/...) instead
			// asserts the ErrUnsupported path.
			"TestRunSuite/signer/issue_direct_grant_shape": "S3-family has no DirectGrant; presigned PUT is the equivalent (matrix footnote 5)",
		},
	}

	contract.RunSuite(t, fut)
}
