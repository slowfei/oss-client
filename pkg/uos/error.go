// Package uos defines the universal object storage public API. This file
// declares the v1-frozen 14-code error surface plus the single concrete
// *Error type that all driver operations must return.
package uos

import (
	"errors"
	"fmt"

	"github.com/slowfei/oss-client/pkg/uos/capability"
)

// Code is the v1-frozen error code enum. See architecture_plan §7.1 for
// the authoritative semantic definition of each value. The set is frozen
// for v1.x; adding a new code requires at least two providers needing
// the same semantic and a minor version bump on pkg/uos.
type Code string

// The 14 frozen v1 error codes. Declaration order matches
// architecture_plan §7.1 and is observed by AllCodes().
const (
	// ErrUnsupported is returned when the operation requires a capability
	// the driver does not expose. The Error.Capability field MUST be
	// populated with the missing capability.
	ErrUnsupported Code = "unsupported"
	// ErrInvalidArgument is for malformed requests caught client-side or
	// rejected server-side as syntactically invalid (bad bucket name,
	// negative size, conflicting headers, etc.).
	ErrInvalidArgument Code = "invalid_argument"
	// ErrNotFound covers bucket-not-found, key-not-found, and version-not-found.
	ErrNotFound Code = "not_found"
	// ErrAlreadyExists is returned when creating a resource (typically a
	// bucket) that already exists, when uniqueness was required.
	ErrAlreadyExists Code = "already_exists"
	// ErrPermissionDenied is for ACL/IAM denials on otherwise valid requests.
	ErrPermissionDenied Code = "permission_denied"
	// ErrUnauthenticated is for missing/expired/malformed credentials.
	ErrUnauthenticated Code = "unauthenticated"
	// ErrPreconditionFailed covers If-Match / If-None-Match / If-Modified-Since failures.
	ErrPreconditionFailed Code = "precondition_failed"
	// ErrConflict is for concurrent-modification and similar state conflicts.
	ErrConflict Code = "conflict"
	// ErrRateLimited is for vendor throttling (HTTP 429 / SlowDown / etc.). Retryable.
	ErrRateLimited Code = "rate_limited"
	// ErrTimeout is for client-observed deadline exceeded or server-side timeout. Retryable.
	ErrTimeout Code = "timeout"
	// ErrTemporary is for transient infrastructure errors (HTTP 5xx without a more specific code). Retryable.
	ErrTemporary Code = "temporary"
	// ErrChecksumMismatch is for end-to-end integrity check failures (MD5/CRC/SHA mismatch on upload/download).
	ErrChecksumMismatch Code = "checksum_mismatch"
	// ErrLengthRequired is for streaming uploads where the provider needs Content-Length and the caller did not supply one.
	ErrLengthRequired Code = "length_required"
	// ErrInternal is the catch-all for unmapped vendor errors. The original error MUST be preserved in Cause.
	ErrInternal Code = "internal"
)

// AllCodes returns the 14 frozen codes in declaration order. Used by
// surface_test and the contract test suite to verify completeness.
func AllCodes() []Code {
	return []Code{
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
}

// Provider is the canonical string identifier for an object-storage
// provider (e.g. "aws", "alibaba", "azure"). It is declared here, in
// the same file as Code, so that the error type can reference it
// without forcing a separate import.
type Provider string

// Error is the single concrete error type returned by all pkg/uos
// operations. Drivers MUST translate vendor errors into *Error;
// provider-specific richness goes into Message, SecondaryID, Cause,
// and (when Code == ErrUnsupported) Capability.
//
// Stable matching contract for v1: errors.Is(err, &Error{Code: c})
// returns true iff err is an *Error with the same Code. All other
// fields are diagnostic and do not participate in matching.
type Error struct {
	// Provider is the provider id that produced the error.
	Provider Provider
	// Operation is the high-level op name (e.g. "PutObject", "ListBuckets").
	Operation string
	// Bucket is the bucket the operation targeted, when applicable.
	Bucket string
	// Key is the object key the operation targeted, when applicable.
	Key string
	// Code is one of the 14 frozen Code values.
	Code Code
	// Message is human-readable detail, vendor-flavored allowed.
	Message string
	// HTTPStatus is the upstream HTTP status, when one exists.
	HTTPStatus int
	// RequestID is the upstream request id, when one exists.
	RequestID string
	// SecondaryID is a vendor secondary identifier (e.g. AWS extended request id).
	SecondaryID string
	// Retryable hints whether a retry is reasonable. Drivers SHOULD set this
	// based on Code semantics (rate_limited, timeout, temporary → true) and
	// vendor-specific signals.
	Retryable bool
	// Capability is populated only when Code == ErrUnsupported, naming the
	// missing capability so callers can dispatch on it.
	Capability capability.Capability
	// Cause is the underlying error, preserved for errors.Unwrap traversal.
	Cause error
}

// Error formats the error for logging and human inspection. The format is
// intentionally compact and key=value oriented so it greps well; do not
// rely on the exact layout in tests.
func (e *Error) Error() string {
	if e == nil {
		return "<nil *uos.Error>"
	}
	parts := fmt.Sprintf("uos: code=%s", string(e.Code))
	if e.Provider != "" {
		parts += fmt.Sprintf(" provider=%s", string(e.Provider))
	}
	if e.Operation != "" {
		parts += fmt.Sprintf(" op=%s", e.Operation)
	}
	if e.Bucket != "" {
		parts += fmt.Sprintf(" bucket=%s", e.Bucket)
	}
	if e.Key != "" {
		parts += fmt.Sprintf(" key=%s", e.Key)
	}
	if e.HTTPStatus != 0 {
		parts += fmt.Sprintf(" status=%d", e.HTTPStatus)
	}
	if e.RequestID != "" {
		parts += fmt.Sprintf(" rid=%s", e.RequestID)
	}
	if e.Capability != "" {
		parts += fmt.Sprintf(" cap=%s", string(e.Capability))
	}
	if e.Message != "" {
		parts += fmt.Sprintf(" msg=%q", e.Message)
	}
	if e.Cause != nil {
		parts += fmt.Sprintf(" cause=%v", e.Cause)
	}
	return parts
}

// Unwrap returns the underlying cause for use with errors.Unwrap and
// errors.Is/As traversal. Returns nil when no cause was attached.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is implements the matching contract documented on Error: two *Error
// values match iff their Code values match. When the target also names
// a non-empty Capability, the receiver's Capability must equal it too;
// otherwise Capability is ignored. All other fields are diagnostic and
// never participate in matching.
//
// This guarantees the v1 stability promise:
//
//	errors.Is(err, &uos.Error{Code: uos.ErrNotFound})
//
// remains valid for the lifetime of v1.x.
func (e *Error) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	if t.Code != "" && t.Code != e.Code {
		return false
	}
	if t.Capability != "" && t.Capability != e.Capability {
		return false
	}
	return true
}

// NewUnsupported is the rich-error wrapper for capability gaps. It
// returns *Error{Code: ErrUnsupported, Capability: cap} populated from
// the given context. This wrapper lives in pkg/uos (rather than the
// capability package) so that capability remains a leaf import: see
// capability.Report.Require for the sentinel error this wraps.
func NewUnsupported(provider Provider, operation string, cap capability.Capability, cause error) *Error {
	return &Error{
		Provider:   provider,
		Operation:  operation,
		Code:       ErrUnsupported,
		Capability: cap,
		Message:    fmt.Sprintf("capability %q is not supported by provider %q", string(cap), string(provider)),
		Cause:      cause,
	}
}

// WrapMissingCapability inspects err for a capability.Report.Require
// sentinel; if found, it returns a populated *Error{Code: ErrUnsupported,
// Capability: <missing>}. If err is not such a sentinel, returns nil.
// Drivers should use this to convert Report.Require failures into the
// rich error type without re-implementing the wrapping logic.
func WrapMissingCapability(provider Provider, operation string, err error) *Error {
	if err == nil {
		return nil
	}
	cap, ok := capability.MissingCapability(err)
	if !ok {
		return nil
	}
	return NewUnsupported(provider, operation, cap, err)
}

// Compile-time guarantees that *Error implements the standard error
// interfaces (error, Unwrap, Is). Keeping these here makes accidental
// signature changes a build-time failure.
var (
	_ error = (*Error)(nil)
	_       = errors.Unwrap
)
