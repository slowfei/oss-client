//go:build docker

// driver_test.go wires the providers/aws driver into pkg/testkit/contract's
// RunSuite. The S3-compat path uses a testcontainers-spawned MinIO endpoint
// rather than real AWS, so the suite runs in PR gates without leaking credentials.
//
// To exercise: from providers/aws, run
//
//	go test -tags=docker -short -count=1 ./...
//
// Real-AWS-only cases (e.g. UploadPart against a 5 MiB minimum part-size that
// MinIO sometimes accepts but AWS rejects) live behind the cloud nightly
// workflow; this PR-gate file SkipCases them with reason.
package aws_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/maqian/object-storage-client/pkg/testkit/contract"
	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/credential"
	awsdrv "github.com/maqian/object-storage-client/providers/aws"
)

// TestRunSuite runs the conformance suite against a MinIO testcontainer
// using the AWS SDK v2 driver in S3-compat mode (custom endpoint +
// path-style + SigV4 region placeholder).
func TestRunSuite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	endpoint, accessKey, secretKey, cleanupContainer, err := contract.SpawnMinIO(ctx)
	if err != nil {
		t.Fatalf("SpawnMinIO: %v", err)
	}
	t.Cleanup(cleanupContainer)

	endpointURL := normaliseEndpoint(endpoint)

	cred := credential.NewStatic(credential.Credential{
		Scheme: credential.AuthHMAC,
		Opaque: &credential.EnvHMACCredential{
			AccessKeyID:     accessKey,
			SecretAccessKey: secretKey,
		},
	})

	cfg := uos.Config{
		Provider:           "aws",
		Region:             "us-east-1", // placeholder; SigV4 needs one
		Endpoint:           endpointURL,
		CredentialProvider: cred,
		DriverConfig: &awsdrv.DriverConfig{
			PathStyle:    true,
			DisableHTTPS: true,
		},
	}

	factory := awsdrv.Factory()

	fut := contract.FactoryUnderTest{
		Provider: "aws",
		Bucket:   "uos-contract-test",
		Endpoint: endpointURL,
		Setup: func(ctx context.Context, t *testing.T) (uos.Client, func(), error) {
			c, err := factory.Open(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return c, func() { _ = c.Close() }, nil
		},
		// Cases the AWS driver opts out of for this PR-gate run, with
		// reason. Each entry is matched against the t.Run hierarchy
		// path produced by RunSuite.
		SkipCases: map[string]string{
			// AWS S3 has no non-URL grant model (provider_matrix.md
			// footnote 5 / capabilities.go); IssueDirectGrant returns
			// ErrUnsupported with Capability=CapDirectGrant, which the
			// capability suite already verifies. The signer.shape case
			// expects a grant body, which AWS will never emit.
			"TestRunSuite/signer/issue_direct_grant_shape": "AWS S3 uses presigned URLs; CapDirectGrant=Unsupported per matrix footnote 5",

			// Special-char key with '?' and '%FF' triggers a SigV4 vs
			// MinIO canonicalisation mismatch on the wire (verified to
			// fail with the raw aws-sdk-go-v2 against this MinIO image:
			// the SDK signs the un-encoded form while MinIO canonicalises
			// the encoded form). The driver itself passes the key
			// opaquely; the bug lives between aws-sdk-go-v2 SigV4 and
			// older MinIO releases. Cloud-nightly against real AWS
			// validates the key shape end-to-end.
			"TestRunSuite/object/put_get_special_char_key": "aws-sdk-go-v2 + MinIO 2024-08 disagree on '?'/%FF canonicalisation; verified against real AWS in cloud nightly",

			// Multipart resume requires a persisted StateStore + driver
			// wiring; the M1 contract suite already t.Skips this case,
			// listed here for documentation parity.
			"TestRunSuite/multipart/resume_after_failure": "M1 stub; transfer.Manager StateStore wiring lands in v0.2",
		},
		// SkipCodes lists pkg/uos.Code values the AWS driver provably cannot
		// reach against MinIO in S3-compat mode within the contract suite's
		// generic per-code scenario harness. The contract suite's per-code
		// cases are themselves t.Skip'd in M1; the list is forward-looking.
		SkipCodes: []uos.Code{},
	}

	contract.RunSuite(t, fut)
}

// normaliseEndpoint accepts either a full URL ("http://host:port") or a
// bare host:port (testcontainers' ConnectionString) and returns a URL
// the AWS SDK v2 EndpointResolverV2 can consume. http:// is forced so
// the SDK doesn't attempt TLS against MinIO's HTTP listener.
//
// Note: url.Parse is permissive — it parses "localhost:34567" as
// Scheme="localhost" Opaque="34567". We therefore only treat the input
// as scheme-bearing when the scheme is the literal "http" or "https".
func normaliseEndpoint(raw string) string {
	if raw == "" {
		return raw
	}
	if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return raw
	}
	return "http://" + raw
}
