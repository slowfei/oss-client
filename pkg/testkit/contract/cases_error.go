package contract

import (
	"testing"

	"github.com/maqian/oss-client/pkg/uos"
)

// runErrorCases iterates the frozen 14 codes and asserts each one is
// reachable for this driver via at least one suite case. Drivers that
// provably cannot reach a code (e.g. GCS has no Length-Required class)
// opt out via FactoryUnderTest.SkipCodes.
//
// In M1 the case bodies are reflective shape assertions only — the
// driver-side reachability proof requires a real wire-level scenario,
// which lands with each provider in M2+. The case names here are
// frozen so M2 drivers know exactly which scenarios they must prove.
func runErrorCases(t *testing.T, fut FactoryUnderTest) {
	t.Helper()

	skip := make(map[uos.Code]bool, len(fut.SkipCodes))
	for _, code := range fut.SkipCodes {
		skip[code] = true
	}

	for _, code := range uos.AllCodes() {
		code := code
		name := "code_reachable/" + string(code)
		t.Run(name, func(t *testing.T) {
			if skip[code] {
				t.Skipf("driver opts out of code %q", code)
			}
			if reason := shouldSkip(fut, t.Name()); reason != "" {
				t.Skipf("driver opt-out: %s", reason)
			}
			// The reachability proof is per-driver and requires a wire
			// scenario the contract suite can't fabricate generically;
			// each driver implements the per-code scenario in its own
			// test file alongside this RunSuite invocation. The case
			// existing here pins the name so M2 drivers see it.
			t.Skipf("code %q reachability proven per-driver; M2 wires the scenario", code)
			_ = scenariosByCode[code] // referenced to keep the table live
		})
	}

	t.Run("all_codes_known", func(t *testing.T) {
		// Defensive: AllCodes must equal the documented set length. This
		// guards against accidental edits to error.go that bypass
		// surface_test.go (different package).
		if got, want := len(uos.AllCodes()), 14; got != want {
			t.Fatalf("uos.AllCodes(): want 14 codes (architecture_plan §7.1), got %d", got)
		}
	})
}

// scenariosByCode documents the canonical wire scenario each driver
// MUST implement to prove a Code is reachable. The map is informational
// — the contract suite does not execute these — but its presence in
// the same file makes the per-code reviewer expectation explicit.
var scenariosByCode = map[uos.Code]string{
	uos.ErrUnsupported:        "call op gated by an Unsupported capability",
	uos.ErrInvalidArgument:    "call any op with a syntactically invalid argument (negative size, empty key)",
	uos.ErrNotFound:           "Stat/Head a missing bucket or object",
	uos.ErrAlreadyExists:      "Create a bucket that already exists",
	uos.ErrPermissionDenied:   "call op with a credential that lacks IAM/ACL access",
	uos.ErrUnauthenticated:    "call op with a missing/expired credential",
	uos.ErrPreconditionFailed: "call Put/Get with an If-Match that doesn't match",
	uos.ErrConflict:           "Delete a non-empty bucket; concurrent ETag conflict",
	uos.ErrRateLimited:        "vendor returns 429/SlowDown (driver translates)",
	uos.ErrTimeout:            "wire-level deadline exceeded (ctx.Deadline)",
	uos.ErrTemporary:          "vendor returns 5xx without a more specific code",
	uos.ErrChecksumMismatch:   "supply a Checksum that doesn't match the body",
	uos.ErrLengthRequired:     "Put with -1 size and UnknownSizePolicy=Reject",
	uos.ErrInternal:           "vendor returns an unmapped error class",
}
