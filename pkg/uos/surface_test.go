package uos

import (
	"slices"
	"testing"

	"github.com/maqian/object-storage-client/pkg/uos/capability"
)

// TestFrozenSurface pins the v0.1 frozen surface so future PRs cannot
// silently grow it. The three subtests assert literal-strict equality
// over the three frozen sets: the 14 error Codes, the 13 Capabilities,
// and the 4 DirectGrantMode values. Adding, removing, renaming, or
// re-stringing any constant in any of these sets MUST cause this test
// to fail. See architecture_plan §7 (Frozen sets) and the Critic R1
// sign-off for the freezing-process rationale.
func TestFrozenSurface(t *testing.T) {
	t.Run("codes_frozen_14", func(t *testing.T) {
		expected := []Code{
			ErrUnsupported,
			ErrInvalidArgument,
			ErrNotFound,
			ErrAlreadyExists,
			ErrPermissionDenied,
			ErrUnauthenticated,
			ErrPreconditionFailed,
			ErrConflict,
			ErrRateLimited,
			ErrTimeout,
			ErrTemporary,
			ErrChecksumMismatch,
			ErrLengthRequired,
			ErrInternal,
		}
		got := AllCodes()
		if len(got) != 14 {
			t.Fatalf("v0.1 frozen-set rule: AllCodes() must return exactly 14 codes, got %d. Adding a code requires a minor bump and freezing-process re-run; removing one is a major bump.", len(got))
		}
		if !slices.Equal(got, expected) {
			t.Fatalf("v0.1 frozen-set rule: AllCodes() drift detected.\n  got:      %v\n  expected: %v", got, expected)
		}
		expectedStrings := []string{
			"unsupported",
			"invalid_argument",
			"not_found",
			"already_exists",
			"permission_denied",
			"unauthenticated",
			"precondition_failed",
			"conflict",
			"rate_limited",
			"timeout",
			"temporary",
			"checksum_mismatch",
			"length_required",
			"internal",
		}
		for i, c := range expected {
			if string(c) != expectedStrings[i] {
				t.Errorf("v0.1 frozen-set rule: Code[%d] string drift: got %q, want %q (renaming a Code's string value is a wire-breaking change)", i, string(c), expectedStrings[i])
			}
		}
	})

	t.Run("capabilities_frozen_13", func(t *testing.T) {
		expected := []capability.Capability{
			capability.CapBucketCRUD,
			capability.CapObjectCRUD,
			capability.CapListPrefixDelimiter,
			capability.CapRangeRead,
			capability.CapMultipartUpload,
			capability.CapSignedURLRead,
			capability.CapSignedURLWrite,
			capability.CapDirectGrant,
			capability.CapObjectTagging,
			capability.CapVersioning,
			capability.CapObjectACL,
			capability.CapManagedEncryption,
			capability.CapNativeMove,
		}
		got := capability.All()
		if len(capability.All()) != 13 {
			t.Fatalf("v0.1 frozen-set rule: capability.All() must return exactly 13 capabilities, got %d. Adding one requires a minor bump and two providers exposing the same semantic.", len(got))
		}
		if !slices.Equal(got, expected) {
			t.Fatalf("v0.1 frozen-set rule: capability.All() drift detected.\n  got:      %v\n  expected: %v", got, expected)
		}
		expectedStrings := []string{
			"bucket.crud",
			"object.crud",
			"object.list.prefix_delimiter",
			"object.range_read",
			"object.multipart_upload",
			"signer.url_read",
			"signer.url_write",
			"signer.direct_grant",
			"object.tagging",
			"bucket.versioning",
			"object.acl",
			"object.encryption.managed",
			"object.native_move",
		}
		for i, c := range expected {
			if string(c) != expectedStrings[i] {
				t.Errorf("v0.1 frozen-set rule: Capability[%d] string drift: got %q, want %q (renaming a capability's string value is wire-breaking)", i, string(c), expectedStrings[i])
			}
		}
	})

	t.Run("direct_grant_modes_frozen_4", func(t *testing.T) {
		if got := string(DirectGrantModeURL); got != "url" {
			t.Errorf("v0.1 frozen DirectGrantMode set: DirectGrantModeURL = %q, want %q", got, "url")
		}
		if got := string(DirectGrantModeForm); got != "form" {
			t.Errorf("v0.1 frozen DirectGrantMode set: DirectGrantModeForm = %q, want %q", got, "form")
		}
		if got := string(DirectGrantModeToken); got != "token" {
			t.Errorf("v0.1 frozen DirectGrantMode set: DirectGrantModeToken = %q, want %q", got, "token")
		}
		if got := string(DirectGrantModeHeaders); got != "headers" {
			t.Errorf("v0.1 frozen DirectGrantMode set: DirectGrantModeHeaders = %q, want %q", got, "headers")
		}
		// Pin the cardinality too: a 5th mode must trigger a freezing-process
		// re-run, not a silent constant addition. The set is enumerated here
		// rather than via a generated All() because the modes don't share
		// a registry-style helper.
		all := []DirectGrantMode{
			DirectGrantModeURL,
			DirectGrantModeForm,
			DirectGrantModeToken,
			DirectGrantModeHeaders,
		}
		if len(all) != 4 {
			t.Fatalf("v0.1 frozen DirectGrantMode set: expected exactly 4 modes, got %d. Adding a 5th mode requires a freezing-process re-run.", len(all))
		}
	})
}
