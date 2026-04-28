package s3common

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/maqian/oss-client/pkg/uos"
)

func TestMapCodeString(t *testing.T) {
	cases := []struct {
		in   string
		want uos.Code
		ok   bool
	}{
		// One representative per arm in MapCodeString — keep the
		// table small but exercise every uos.Code that the wire
		// layer can produce.
		{"NoSuchKey", uos.ErrNotFound, true},
		{"NoSuchBucket", uos.ErrNotFound, true},
		{"404", uos.ErrNotFound, true},
		{"BucketAlreadyExists", uos.ErrAlreadyExists, true},
		{"AccessDenied", uos.ErrPermissionDenied, true},
		{"MethodNotAllowed", uos.ErrPermissionDenied, true},
		{"SignatureDoesNotMatch", uos.ErrUnauthenticated, true},
		{"ExpiredToken", uos.ErrUnauthenticated, true},
		{"PreconditionFailed", uos.ErrPreconditionFailed, true},
		{"InvalidRange", uos.ErrPreconditionFailed, true},
		{"BucketNotEmpty", uos.ErrConflict, true},
		{"SlowDown", uos.ErrRateLimited, true},
		{"ThrottlingException", uos.ErrRateLimited, true},
		{"RequestTimeout", uos.ErrTimeout, true},
		{"ServiceUnavailable", uos.ErrTemporary, true},
		{"BadDigest", uos.ErrChecksumMismatch, true},
		{"MissingContentLength", uos.ErrLengthRequired, true},
		{"InvalidArgument", uos.ErrInvalidArgument, true},
		{"MalformedXML", uos.ErrInvalidArgument, true},
		// OSS-specific aliases added during M3 alibaba driver landing.
		{"NoSuchObjectVersion", uos.ErrNotFound, true},
		{"KmsKeyNotFound", uos.ErrNotFound, true},
		{"BucketVersioningSuspended", uos.ErrConflict, true},
		{"BucketReplicationException", uos.ErrConflict, true},
		{"RestoreAlreadyInProgress", uos.ErrConflict, true},
		{"InvalidEncryptionAlgorithmError", uos.ErrConflict, true},
		{"InvalidLocationConstraint", uos.ErrInvalidArgument, true},
		{"MalformedAclError", uos.ErrInvalidArgument, true},
		{"RequestIsNotMultiPartContent", uos.ErrInvalidArgument, true},
		{"EntityTooSmallError", uos.ErrInvalidArgument, true},
		// Unknown / empty codes return ok=false and let the caller
		// fall through to MapHTTPStatus.
		{"", "", false},
		{"SomeProprietaryVendorCode", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := MapCodeString(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("MapCodeString(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMapHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   uos.Code
		ok     bool
	}{
		{404, uos.ErrNotFound, true},
		{403, uos.ErrPermissionDenied, true},
		{401, uos.ErrUnauthenticated, true},
		{412, uos.ErrPreconditionFailed, true},
		{409, uos.ErrConflict, true},
		{429, uos.ErrRateLimited, true},
		{408, uos.ErrTimeout, true},
		{411, uos.ErrLengthRequired, true},
		// Range fallbacks.
		{500, uos.ErrTemporary, true},
		{503, uos.ErrTemporary, true},
		{599, uos.ErrTemporary, true},
		{400, uos.ErrInvalidArgument, true},
		{422, uos.ErrInvalidArgument, true},
		// Unmapped ranges return ok=false.
		{200, "", false},
		{204, "", false},
		{301, "", false},
		{0, "", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status=%d", tc.status), func(t *testing.T) {
			got, ok := MapHTTPStatus(tc.status)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("MapHTTPStatus(%d) = (%q, %v), want (%q, %v)",
					tc.status, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMapContextErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want uos.Code
		ok   bool
	}{
		{"canceled", context.Canceled, uos.ErrTimeout, true},
		{"deadline_exceeded", context.DeadlineExceeded, uos.ErrTimeout, true},
		{"wrapped_canceled", fmt.Errorf("wrap: %w", context.Canceled), uos.ErrTimeout, true},
		{"wrapped_deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), uos.ErrTimeout, true},
		{"plain_other", errors.New("not a ctx error"), "", false},
		{"nil", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MapContextErr(tc.err)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("MapContextErr(%v) = (%q, %v), want (%q, %v)",
					tc.err, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := []uos.Code{uos.ErrRateLimited, uos.ErrTimeout, uos.ErrTemporary}
	for _, c := range retryable {
		t.Run(string(c)+"_yes", func(t *testing.T) {
			if !IsRetryable(c) {
				t.Fatalf("IsRetryable(%q) = false, want true", c)
			}
		})
	}
	// Spot-check non-retryable codes (NOT exhaustive — the contract
	// is "exactly the three above are true; everything else false").
	nonRetryable := []uos.Code{
		uos.ErrNotFound, uos.ErrAlreadyExists, uos.ErrPermissionDenied,
		uos.ErrUnauthenticated, uos.ErrPreconditionFailed,
		uos.ErrConflict, uos.ErrChecksumMismatch, uos.ErrLengthRequired,
		uos.ErrInvalidArgument, uos.ErrInternal, uos.ErrUnsupported,
	}
	for _, c := range nonRetryable {
		t.Run(string(c)+"_no", func(t *testing.T) {
			if IsRetryable(c) {
				t.Fatalf("IsRetryable(%q) = true, want false", c)
			}
		})
	}
}

func TestLowerMetadataKeys(t *testing.T) {
	// nil → nil (vendor SDKs distinguish nil from empty; we collapse).
	if got := LowerMetadataKeys(nil); got != nil {
		t.Fatalf("LowerMetadataKeys(nil) = %v, want nil", got)
	}
	// Empty → nil (the unified contract treats nil and empty as
	// "no metadata"; collapse at this boundary so vendor SDKs see
	// nil instead of an explicit empty map).
	if got := LowerMetadataKeys(map[string]string{}); got != nil {
		t.Fatalf("LowerMetadataKeys({}) = %v, want nil", got)
	}
	// Mixed-case keys → lower-cased; values untouched.
	got := LowerMetadataKeys(map[string]string{
		"X-App-Name":   "portal",
		"X-AMZ-META":   "ignored", // x- prefix is preserved verbatim except case
		"AlreadyLower": "v",
	})
	want := map[string]string{
		"x-app-name":   "portal",
		"x-amz-meta":   "ignored",
		"alreadylower": "v",
	}
	if len(got) != len(want) {
		t.Fatalf("LowerMetadataKeys length mismatch: got %d, want %d", len(got), len(want))
	}
	for k, wantV := range want {
		if gotV, ok := got[k]; !ok || gotV != wantV {
			t.Errorf("LowerMetadataKeys missing or wrong value for %q: got %q, want %q", k, gotV, wantV)
		}
	}
	// Source not mutated (defensive copy).
	src := map[string]string{"X-Foo": "bar"}
	_ = LowerMetadataKeys(src)
	if _, ok := src["X-Foo"]; !ok {
		t.Fatalf("LowerMetadataKeys mutated the source map")
	}
}
