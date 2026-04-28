package upyun

import (
	"context"
	"errors"
	"net/http"

	upyunsdk "github.com/upyun/go-sdk/v3/upyun"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/s3common"
)

// mapError translates a github.com/upyun/go-sdk/v3 error into a *uos.Error
// per architecture_plan §7.1 (the 14 frozen Code values). Decision tree:
//
//  1. Pass-through any *uos.Error already produced upstream.
//  2. *upyunsdk.Error — vendor's typed error carrying StatusCode + Code +
//     Message + RequestID. Consult the local Upyun error-code table
//     first, then s3common.MapHTTPStatus.
//  3. context.Canceled / DeadlineExceeded → ErrTimeout via s3common.MapContextErr.
//  4. Catch-all → ErrInternal with Cause preserved.
//
// Upyun error codes are vendor-specific 8-digit numerics (e.g. 40400001
// "file or directory not found", 40300003 "username password error").
// They are NOT S3-compat code strings and MUST NOT be added to
// s3common.MapCodeString — the local mapUpyunErrorCode switch below is
// the sole translation point for Upyun wire codes (mirrors the M4
// gcs/azure pattern).
func mapError(provider uos.Provider, op, bucket, key string, err error) error {
	if err == nil {
		return nil
	}
	// Pass through any *uos.Error already produced upstream (argument
	// validation, capability gating, etc.).
	var alreadyMapped *uos.Error
	if errors.As(err, &alreadyMapped) {
		// v0.1.1 patch: augment with caller's context if the inner
		// *uos.Error lacks it. Identity-pass loses Operation/Bucket/Key
		// when an inner layer (capability gating, arg validation) built
		// the error with only Code+Message.
		augmented := *alreadyMapped
		if augmented.Provider == "" {
			augmented.Provider = provider
		}
		if augmented.Operation == "" {
			augmented.Operation = op
		}
		if augmented.Bucket == "" {
			augmented.Bucket = bucket
		}
		if augmented.Key == "" {
			augmented.Key = key
		}
		return &augmented
	}

	out := &uos.Error{
		Provider:  provider,
		Operation: op,
		Bucket:    bucket,
		Key:       key,
		Code:      uos.ErrInternal,
		Message:   err.Error(),
		Cause:     err,
	}

	// *upyunsdk.Error is the typed error returned by the Upyun SDK for
	// every non-2xx response (parsed from the JSON body when present).
	// It carries StatusCode (HTTP), Code (vendor 8-digit numeric),
	// Message, and RequestID.
	var upErr *upyunsdk.Error
	if errors.As(err, &upErr) {
		out.HTTPStatus = upErr.StatusCode
		out.RequestID = upErr.RequestID
		if upErr.Message != "" {
			out.Message = upErr.Message
		}

		// Consult the local Upyun-specific code table first.
		if code, ok := mapUpyunErrorCode(upErr.Code); ok {
			out.Code = code
			out.Retryable = s3common.IsRetryable(code) || isUpyunRetryableStatus(upErr.StatusCode)
			return out
		}
		// Fall back to the HTTP-status table shared by all drivers.
		if code, ok := s3common.MapHTTPStatus(upErr.StatusCode); ok {
			out.Code = code
			out.Retryable = s3common.IsRetryable(code) || isUpyunRetryableStatus(upErr.StatusCode)
			return out
		}
		// Unknown status — stay at ErrInternal but set Retryable on 5xx.
		out.Retryable = isUpyunRetryableStatus(upErr.StatusCode)
		return out
	}

	// Context cancellation / deadline.
	if code, ok := s3common.MapContextErr(err); ok {
		out.Code = code
		out.Retryable = errors.Is(err, context.DeadlineExceeded)
		return out
	}

	// Unmapped: ErrInternal with Cause preserved.
	return out
}

// mapUpyunErrorCode maps Upyun wire error codes to the 14 frozen
// pkg/uos.Code values. Upyun documents its error codes as 8-digit
// integers prefixed by the HTTP status code (e.g. 404xxxxx for
// not-found, 403xxxxx for permission-denied). The most common codes
// observed in production are listed below; any new code discovered
// during contract testing belongs here.
//
// These codes are vendor-specific and MUST NOT be added to
// s3common.MapCodeString (which is S3-compat XML only).
func mapUpyunErrorCode(code int) (uos.Code, bool) {
	switch code {
	// Not-found family (404xxxxxx)
	case 40400001, // file or directory not found
		40400002, // file not found
		40400006, // directory not found
		40400010, // service / bucket not found
		40400029, // upload-part not found
		40400031: // multipart upload not found
		return uos.ErrNotFound, true

	// Already-exists family (rare in Upyun; mostly 4060xxxx not-acceptable)
	case 40600001: // resource already exists
		return uos.ErrAlreadyExists, true

	// Auth / permission family (401/403)
	case 40100001, // unauthorised
		40100007: // missing token / signature
		return uos.ErrUnauthenticated, true

	case 40300001, // operation not permitted
		40300003, // username / password error
		40300006, // bucket access denied
		40300010, // operator disabled
		40300012: // signature mismatch
		return uos.ErrPermissionDenied, true

	// Precondition family (412 / 304)
	case 41200001, // condition not met
		30400001: // not modified
		return uos.ErrPreconditionFailed, true

	// Conflict family (409)
	case 40900001, // resource conflict
		40900002, // file in use
		40900010: // multipart already completed
		return uos.ErrConflict, true

	// Rate-limit family (429)
	case 42900001, // too many requests
		42900002: // bucket throughput exceeded
		return uos.ErrRateLimited, true

	// Timeout family (408)
	case 40800001: // request timeout
		return uos.ErrTimeout, true

	// Checksum / integrity family
	case 40000018: // content-md5 mismatch
		return uos.ErrChecksumMismatch, true

	// Length-required family (411)
	case 41100001, // missing content-length
		41100002: // missing content-length on multipart
		return uos.ErrLengthRequired, true

	// Invalid argument family (400)
	case 40000001, // bad request
		40000002, // bucket name invalid
		40000003, // path invalid
		40000010, // header invalid
		40000011, // signature format invalid
		40000017, // file size exceeds limit
		40000031, // multipart part-id invalid
		40000033, // multipart part-size invalid
		40000034: // multipart stage invalid
		return uos.ErrInvalidArgument, true

	// Transient infrastructure (5xx)
	case 50000001, // internal server error
		50300001: // service unavailable
		return uos.ErrTemporary, true
	}
	return "", false
}

// isUpyunRetryableStatus returns true for HTTP status codes Upyun
// documents as transient and retry-worthy, beyond the three frozen
// pkg/uos Codes (rate_limited / timeout / temporary) already covered by
// s3common.IsRetryable.
func isUpyunRetryableStatus(status int) bool {
	switch status {
	case http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout,      // 504
		http.StatusInternalServerError, // 500
		http.StatusBadGateway:          // 502
		return true
	}
	return false
}
