package gcs

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// mapError translates a cloud.google.com/go/storage error into a
// *uos.Error per architecture_plan §7.1 (the 14 frozen Code values).
// It handles:
//
//   - Pass-through of any *uos.Error already wrapped upstream
//     (capability gating, argument validation, etc.).
//   - storage.ErrObjectNotExist / storage.ErrBucketNotExist sentinels —
//     the SDK wraps the wire-level 404 in these so errors.Is matching
//     works without parsing message strings.
//   - *googleapi.Error, the JSON API wire-error shape carrying HTTP
//     status, message, and a list of vendor-specific Reason / Message
//     ErrorItems.
//   - Generic fall-through to pkg/uos/s3common helpers (MapHTTPStatus
//     for the HTTP status fallback, MapContextErr for context
//     cancellation), then ErrInternal as the documented catch-all.
//
// architecture_plan §7.1 forbids returning anything outside the 14
// frozen Codes; this function is the single chokepoint that enforces
// that rule for the GCS driver.
//
// # Why GCS codes are NOT in s3common.MapCodeString
//
// pkg/uos/s3common.MapCodeString covers the S3-compat XML error codes
// (NoSuchKey, BucketAlreadyExists, SlowDown, etc.) shared by AWS,
// MinIO, Alibaba OSS, Tencent COS, Huawei OBS, and Volcengine TOS. GCS
// surfaces errors via the JSON API with a different vocabulary
// (Reason="notFound", Reason="forbidden", Reason="rateLimitExceeded",
// etc.) that does NOT belong in the S3-compat table. They are mapped
// in the LOCAL switch (mapGoogleAPIReason) instead, per the M4 brief.
func mapError(provider uos.Provider, op, bucket, key string, err error) error {
	if err == nil {
		return nil
	}
	// Pass through any *uos.Error already produced upstream.
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

	// SDK sentinels first — they short-circuit the wire-level error
	// inspection because the SDK has already promised "this is a 404".
	if errors.Is(err, storage.ErrObjectNotExist) || errors.Is(err, storage.ErrBucketNotExist) {
		out.Code = uos.ErrNotFound
		out.HTTPStatus = http.StatusNotFound
		return out
	}

	// *googleapi.Error is the JSON API wire-error shape. The SDK
	// returns it directly (or wrapped via fmt.Errorf with %w) for any
	// non-2xx response. We stamp HTTPStatus + Message before deciding
	// the Code so the fallback paths (MapHTTPStatus) have the wire
	// status to consult.
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		out.HTTPStatus = apiErr.Code
		if apiErr.Message != "" {
			out.Message = apiErr.Message
		}
		// First try the per-Reason switch (vendor-specific GCS code
		// strings). If no Reason matches, fall back to HTTP status.
		var matched bool
		for _, item := range apiErr.Errors {
			if code, ok := mapGoogleAPIReason(item.Reason); ok {
				out.Code = code
				if out.Message == "" && item.Message != "" {
					out.Message = item.Message
				}
				matched = true
				break
			}
		}
		if !matched {
			// fake-gcs-server (and some GCS paths) return 409 with an empty
			// Errors slice but a human-readable message containing "already
			// exists". Promote to ErrAlreadyExists before the generic
			// MapHTTPStatus fallback so the contract suite's
			// create_idempotency_already_exists case passes against the emulator.
			if apiErr.Code == http.StatusConflict &&
				containsFold(apiErr.Message, "already exists") {
				out.Code = uos.ErrAlreadyExists
			} else if mapped, ok := s3common.MapHTTPStatus(apiErr.Code); ok {
				out.Code = mapped
			}
		}
		out.Retryable = isRetryable(out.Code, apiErr.Code)
		return out
	}

	// Context cancellation / deadline (the cheapest residual check;
	// distinguish Canceled — caller intent, do NOT retry — from
	// DeadlineExceeded — transient, DO retry).
	if code, ok := s3common.MapContextErr(err); ok {
		out.Code = code
		out.Retryable = errors.Is(err, context.DeadlineExceeded)
		return out
	}

	// Unmapped: stay at ErrInternal; preserve the original via Cause so
	// errors.Unwrap / errors.As callers still see it.
	return out
}

// mapGoogleAPIReason picks the frozen pkg/uos.Code that best fits a
// GCS *googleapi.Error.Errors[i].Reason string. The vocabulary comes
// from the JSON API documentation:
//
//	https://cloud.google.com/storage/docs/json_api/v1/status-codes
//
// The boolean is false for unrecognised reasons; callers fall through
// to MapHTTPStatus and then ErrInternal.
//
// This LOCAL switch is the GCS-only counterpart to
// s3common.MapCodeString — kept here per the M4 brief because GCS uses
// a JSON-flavored vocabulary distinct from the S3-compat XML codes.
func mapGoogleAPIReason(reason string) (uos.Code, bool) {
	switch reason {
	case "notFound":
		return uos.ErrNotFound, true
	case "conflict", "alreadyExists":
		return uos.ErrAlreadyExists, true
	case "forbidden", "accountDisabled", "insufficientPermissions",
		"objectViewerRequired", "stoppedByOrgPolicy",
		"countryBlocked", "userProjectAccessDenied":
		return uos.ErrPermissionDenied, true
	case "required", "unauthorized", "authError", "lockedDomainExpired",
		"invalidCredentials":
		return uos.ErrUnauthenticated, true
	case "conditionNotMet":
		return uos.ErrPreconditionFailed, true
	case "resourceIsNotReady", "resourceInUseByAnotherResource",
		"objectUnderActiveHold":
		return uos.ErrConflict, true
	case "rateLimitExceeded", "userRateLimitExceeded",
		"quotaExceeded", "concurrentLimitExceeded":
		return uos.ErrRateLimited, true
	case "backendError", "internalError":
		return uos.ErrTemporary, true
	case "invalidAltValue", "invalidParameter", "invalidQuery",
		"badRequest", "invalid", "parseError",
		"invalidArgument", "wrongUrlForUpload",
		"unsupportedProtocol":
		return uos.ErrInvalidArgument, true
	case "uploadBrokenConnection", "checksumMismatch":
		return uos.ErrChecksumMismatch, true
	}
	return "", false
}

// containsFold reports whether s contains substr under Unicode case-folding.
// Used to detect "already exists" in emulator error messages that lack a
// structured Reason field.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// isRetryable hints whether a retry is reasonable. The driver itself
// owns no internal retryer (the SDK ships one but we disable it at
// construction by setting Bucket.Retryer to a no-op policy); the field
// exists so callers + pkg/uos.RetryPolicy can decide. Delegates the
// per-Code decision to s3common.IsRetryable; the
// HTTP-5xx-without-vendor-code rescue stays inline because it depends
// on the wire status (not just the resolved Code).
func isRetryable(code uos.Code, status int) bool {
	if s3common.IsRetryable(code) {
		return true
	}
	// Some 5xx without a vendor reason still warrant a retry.
	if status >= 500 && status < 600 {
		return true
	}
	return false
}
