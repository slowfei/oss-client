// Package s3common holds the wire-protocol-level mappings shared by
// every S3-family driver (providers/aws, providers/minio, and the M3+
// 国云 quartet alibaba/tencent/huawei/volcengine). It is NOT a driver
// — it has zero vendor-SDK imports and runs on stdlib only.
//
// # What it covers
//
//   - MapCodeString: the S3-compat error code string → uos.Code table
//     (e.g. "NoSuchKey" → uos.ErrNotFound, "SlowDown" → uos.ErrRateLimited).
//   - MapHTTPStatus: HTTP status fallback when the vendor didn't
//     supply a recognised code string.
//   - MapContextErr: context cancellation / deadline → uos.ErrTimeout.
//   - IsRetryable: marks the three retryable Codes (RateLimited /
//     Timeout / Temporary).
//   - LowerMetadataKeys: case-folds metadata map keys (S3-family stores
//     custom metadata under x-amz-meta-* and Go's HTTP layer is
//     case-insensitive; the unified API normalises at the driver
//     boundary).
//
// # What it does NOT cover
//
// Vendor-specific typed-error wrappers (e.g. aws-sdk-go-v2's
// *types.NoSuchKey or minio-go's miniogo.ErrorResponse) stay inside
// each driver's error_map.go on top of these helpers. Pointer-flatten
// helpers, range header formatting, and DeleteMany batching are NOT
// duplicated across providers/aws + providers/minio (only AWS uses
// pointer-heavy types; MinIO uses native typed APIs); they are
// therefore NOT in scope here. Re-evaluate at M3 if a 国云 driver
// proves the same pattern.
//
// # Why a public subpackage of pkg/uos
//
// Providers live in their own modules (`providers/<name>/go.mod`),
// so an `internal/` package would be unreachable. Hosting s3common
// under pkg/uos/ matches the pattern already used by capability,
// credential, transfer, middleware, and httpx — providers' existing
// `require github.com/maqian/object-storage-client` covers it without
// a go.mod change.
package s3common

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/maqian/object-storage-client/pkg/uos"
)

// MapCodeString returns the uos.Code matching an S3-compat error code
// string (e.g. "NoSuchKey", "AccessDenied"). The boolean is false when
// the code string is not recognised; callers should fall through to
// MapHTTPStatus or their vendor-specific typed-error switch before
// landing on uos.ErrInternal as the documented catch-all.
//
// The case strings are grouped by target uos.Code. Aliases (e.g.
// "RequestTimeout" + "RequestTimeoutException") are listed under the
// same arm so vendor variants resolve identically.
func MapCodeString(code string) (uos.Code, bool) {
	switch code {
	case "NoSuchKey", "NoSuchBucket", "NoSuchUpload", "NoSuchVersion",
		"NoSuchBucketPolicy", "NoSuchCORSConfiguration",
		"NoSuchTagSet", "NotFound", "404",
		// OSS-specific aliases (added during M3 alibaba driver landing).
		"NoSuchObjectVersion", "KmsKeyNotFound":
		return uos.ErrNotFound, true
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou":
		return uos.ErrAlreadyExists, true
	case "AccessDenied", "AllAccessDisabled", "Forbidden",
		"MethodNotAllowed", "AuthorizationHeaderMalformed":
		return uos.ErrPermissionDenied, true
	case "InvalidAccessKeyId", "SignatureDoesNotMatch",
		"InvalidSecurity", "ExpiredToken", "InvalidToken",
		"MissingSecurityHeader":
		return uos.ErrUnauthenticated, true
	case "PreconditionFailed", "InvalidRange", "NotModified":
		return uos.ErrPreconditionFailed, true
	case "BucketNotEmpty", "OperationAborted", "InvalidBucketState",
		"Conflict",
		// OSS-specific aliases (added during M3 alibaba driver landing).
		"BucketVersioningSuspended", "InvalidEncryptionAlgorithmError",
		"RestoreAlreadyInProgress", "BucketReplicationException":
		return uos.ErrConflict, true
	case "SlowDown", "ThrottlingException", "Throttling",
		"TooManyRequests", "RequestLimitExceeded":
		return uos.ErrRateLimited, true
	case "RequestTimeout", "RequestTimeoutException":
		return uos.ErrTimeout, true
	case "ServiceUnavailable", "InternalError", "InternalFailure":
		return uos.ErrTemporary, true
	case "BadDigest", "InvalidDigest", "XAmzContentSHA256Mismatch",
		"IncompleteBody", "UnexpectedEOF":
		return uos.ErrChecksumMismatch, true
	case "MissingContentLength":
		return uos.ErrLengthRequired, true
	case "InvalidArgument", "MalformedXML", "MalformedDate",
		"MalformedPOSTRequest", "MalformedPolicy",
		"InvalidBucketName", "InvalidObjectName", "InvalidRegion",
		"InvalidPart", "InvalidPartOrder", "InvalidObjectState",
		"InvalidDuration", "AuthorizationQueryParametersError",
		"MissingFields", "MissingRequestBodyError",
		"XMinioInvalidObjectName", "APINotSupported",
		"NotImplemented", "EntityTooLarge", "EntityTooSmall",
		"KeyTooLongError", "MetadataTooLarge",
		"RequestTimeTooSkewed",
		// OSS-specific aliases (added during M3 alibaba driver landing).
		// OSS occasionally appends "Error" suffix to the canonical
		// codes (EntityTooSmallError) — list both forms so the wire
		// variant resolves identically.
		"InvalidLocationConstraint", "MalformedAclError",
		"RequestIsNotMultiPartContent", "EntityTooSmallError":
		return uos.ErrInvalidArgument, true
	}
	return "", false
}

// MapHTTPStatus returns the uos.Code matching an HTTP status code per
// the v0.1 error model fallback rules. The boolean is false for
// statuses that don't have a meaningful default mapping (1xx / 2xx /
// 3xx, and other unmapped ranges).
//
// Drivers should consult MapCodeString first; MapHTTPStatus is the
// fallback when the vendor didn't supply a recognised code.
func MapHTTPStatus(status int) (uos.Code, bool) {
	switch status {
	case http.StatusNotFound:
		return uos.ErrNotFound, true
	case http.StatusForbidden:
		return uos.ErrPermissionDenied, true
	case http.StatusUnauthorized:
		return uos.ErrUnauthenticated, true
	case http.StatusPreconditionFailed:
		return uos.ErrPreconditionFailed, true
	case http.StatusConflict:
		return uos.ErrConflict, true
	case http.StatusTooManyRequests:
		return uos.ErrRateLimited, true
	case http.StatusRequestTimeout:
		return uos.ErrTimeout, true
	case http.StatusLengthRequired:
		return uos.ErrLengthRequired, true
	}
	if status >= 500 && status < 600 {
		return uos.ErrTemporary, true
	}
	if status >= 400 && status < 500 {
		return uos.ErrInvalidArgument, true
	}
	return "", false
}

// MapContextErr returns uos.ErrTimeout when err is a context
// cancellation or deadline-exceeded sentinel; the second return is
// false otherwise. Drivers typically check this last so a real
// vendor-side error wins over an ambient ctx cancellation.
func MapContextErr(err error) (uos.Code, bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return uos.ErrTimeout, true
	}
	return "", false
}

// IsRetryable reports whether code is one of the three frozen Codes
// (RateLimited / Timeout / Temporary) that callers — and the
// pkg/uos.RetryPolicy default — should retry by default. All other
// Codes return false; the driver may still mark Retryable explicitly
// via a HTTP-status-based check (e.g. some 5xx without a vendor code).
func IsRetryable(code uos.Code) bool {
	switch code {
	case uos.ErrRateLimited, uos.ErrTimeout, uos.ErrTemporary:
		return true
	}
	return false
}

// LowerMetadataKeys returns a copy of m with all keys lower-cased.
// Returns nil for both nil and empty input — S3-family vendor SDKs
// typically distinguish nil from empty (treating nil as "leave
// default" and an empty map as "explicitly set empty metadata"); the
// unified pkg/uos.Metadata contract treats nil and empty identically
// as "no metadata", so we collapse them at this boundary.
//
// The unified pkg/uos.Metadata contract requires lower-case keys at
// the driver boundary in BOTH directions (in: when handing to the
// vendor SDK; out: when parsing vendor-returned headers like
// x-amz-meta-* into the unified Metadata).
func LowerMetadataKeys(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}
