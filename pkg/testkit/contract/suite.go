// Package contract is the conformance suite every provider driver must
// pass to claim it implements pkg/uos.Client. It enumerates the frozen
// 14-code error surface and 13-capability matrix and asserts each
// driver maps its vendor errors and capability declarations onto the
// unified semantics correctly.
//
// Drivers wire the suite via:
//
//	func TestContract(t *testing.T) {
//	    contract.RunSuite(t, contract.FactoryUnderTest{
//	        Provider: "minio",
//	        Setup:    setupMinIO,
//	        Bucket:   "uos-contract",
//	    })
//	}
//
// In M1 the case bodies guard with `if fut.Setup == nil { t.Fatal(...) }`
// and otherwise t.Skip pending a real driver. The case taxonomy itself
// is frozen at M1 from architecture_plan §6.4.
package contract

import (
	"context"
	"testing"

	"github.com/maqian/oss-client/pkg/uos"
)

// FactoryUnderTest is the per-driver wiring the contract suite consumes.
// Drivers populate it once in their TestContract entry-point.
type FactoryUnderTest struct {
	// Provider is the canonical provider id under test (e.g. "aws", "minio").
	// Surfaced in test names and used by case files when they need a
	// provider-aware skip decision.
	Provider uos.Provider

	// Bucket is the bucket name the suite uses for object cases. The
	// driver MUST be able to create + destroy this bucket via Setup's
	// returned Client. Empty defers to a per-suite default.
	Bucket string

	// Setup constructs a uos.Client bound to the driver under test plus
	// a cleanup func the suite invokes on completion. The returned
	// cleanup MUST release every resource Setup acquired (including
	// fixture buckets / objects). Required.
	Setup func(ctx context.Context, t *testing.T) (uos.Client, func(), error)

	// Endpoint is an optional informational endpoint URL surfaced to
	// case bodies that need to make raw HTTP calls (e.g. the SignURL
	// round-trip case).
	Endpoint string

	// SkipCases lists case names the driver opts out of, with a
	// human-readable reason. Names use the dotted form
	// "<group>.<case>" matching the t.Run hierarchy.
	SkipCases map[string]string

	// SkipCodes lists pkg/uos.Code values the driver provably cannot
	// reach (e.g. a vendor that has no Length-Required error class).
	// cases_error.go honours this list.
	SkipCodes []uos.Code
}

// RunSuite is the entry point every driver test calls. It enumerates
// the frozen Code × Capability matrix via reflection-friendly helpers
// and t.Runs one subtest per group. RunSuite fails fast (not panics)
// when fut.Setup is nil — that is the single most common driver-author
// mistake and a clear error message saves debugging time.
func RunSuite(t *testing.T, fut FactoryUnderTest) {
	t.Helper()
	if fut.Setup == nil {
		t.Fatal("contract.RunSuite: FactoryUnderTest.Setup is required (nil)")
	}
	if fut.Bucket == "" {
		fut.Bucket = "uos-contract-default"
	}

	t.Run("bucket", func(t *testing.T) { runBucketCases(t, fut) })
	t.Run("object", func(t *testing.T) { runObjectCases(t, fut) })
	t.Run("multipart", func(t *testing.T) { runMultipartCases(t, fut) })
	t.Run("signer", func(t *testing.T) { runSignerCases(t, fut) })
	t.Run("capability", func(t *testing.T) { runCapabilityCases(t, fut) })
	t.Run("error", func(t *testing.T) { runErrorCases(t, fut) })
}

// shouldSkip returns the recorded reason if the driver opts out of the
// case identified by name, otherwise empty.
func shouldSkip(fut FactoryUnderTest, name string) string {
	if fut.SkipCases == nil {
		return ""
	}
	return fut.SkipCases[name]
}

// openClient runs Setup and registers the cleanup with t.Cleanup so
// callers don't have to. Test failures abort with the Setup error.
func openClient(ctx context.Context, t *testing.T, fut FactoryUnderTest) uos.Client {
	t.Helper()
	c, cleanup, err := fut.Setup(ctx, t)
	if err != nil {
		t.Fatalf("FactoryUnderTest.Setup: %v", err)
	}
	t.Cleanup(cleanup)
	return c
}

// runCase wraps a case body so the SkipCases opt-out and the openClient
// boilerplate are localized. It is a small helper, intentionally
// not a framework abstraction — case files MAY call openClient
// directly when they need richer wiring.
func runCase(t *testing.T, fut FactoryUnderTest, name string, body func(t *testing.T, c uos.Client)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if reason := shouldSkip(fut, t.Name()); reason != "" {
			t.Skipf("driver opt-out: %s", reason)
		}
		ctx := context.Background()
		c := openClient(ctx, t, fut)
		body(t, c)
	})
}
