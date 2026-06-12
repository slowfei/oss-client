package volcengine

import (
	"context"
	"errors"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// mapError translates a ve-tos-golang-sdk/v2/tos error into a *uos.Error
// per architecture_plan §7.1 (the 14 frozen Code values). It handles:
//
//   - Pass-through of any *uos.Error already wrapped upstream (capability
//     gating, argument validation, etc.).
//   - *tos.TosServerError, the SDK's wire-error shape carrying Code,
//     Message, RequestID, HostID, EC, and StatusCode.
//   - *tos.UnexpectedStatusCodeError, returned for non-2xx responses
//     without a parseable JSON error body.
//   - *tos.TosClientError, the SDK's pre-wire validation/serialisation
//     wrapper (no HTTP roundtrip — typically client-side argument bugs).
//   - Generic fall-through to pkg/uos/s3common helpers (MapCodeString
//     for the wire code string, MapHTTPStatus for the status fallback,
//     MapContextErr for context cancellation), then ErrInternal as the
//     documented catch-all.
//
// architecture_plan §7.1 forbids returning anything outside the 14
// frozen Codes; this function is the single chokepoint that enforces
// that rule for the volcengine driver.
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

	// Most TOS data-plane errors arrive as *tos.TosServerError (returned
	// by pointer). The SDK populates StatusCode + Code + Message +
	// RequestID + HostID + EC from the wire response.
	var svcErr *tos.TosServerError
	if errors.As(err, &svcErr) && svcErr != nil {
		out.HTTPStatus = svcErr.StatusCode
		out.RequestID = svcErr.RequestID
		// HostID is the TOS server cluster id; carry it as the secondary
		// identifier so callers don't lose it during triage.
		out.SecondaryID = svcErr.HostID
		if svcErr.Message != "" {
			out.Message = svcErr.Message
		}
		out.Code = mapServiceCode(svcErr.Code, svcErr.StatusCode)
		out.Retryable = isRetryable(out.Code, svcErr.StatusCode)
		return out
	}

	// *tos.UnexpectedStatusCodeError covers responses with a recognised
	// HTTP status but no parseable JSON error body. We use the status
	// fallback table in s3common to classify it.
	var statusErr *tos.UnexpectedStatusCodeError
	if errors.As(err, &statusErr) && statusErr != nil {
		out.HTTPStatus = statusErr.StatusCode
		out.RequestID = statusErr.RequestID
		if mapped, ok := s3common.MapHTTPStatus(statusErr.StatusCode); ok {
			out.Code = mapped
		}
		out.Retryable = isRetryable(out.Code, statusErr.StatusCode)
		return out
	}

	// *tos.TosClientError covers SDK-side validation / serialisation
	// failures that never made a wire call (bad bucket name, missing
	// required field, JSON encode failure). These are caller-side bugs;
	// classify as ErrInvalidArgument and unwrap the cause for diagnostics.
	var clientErr *tos.TosClientError
	if errors.As(err, &clientErr) && clientErr != nil {
		out.Code = uos.ErrInvalidArgument
		if clientErr.Message != "" {
			out.Message = clientErr.Message
		}
		if clientErr.Cause != nil {
			out.Cause = clientErr.Cause
		}
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

// mapServiceCode picks the frozen pkg/uos.Code that best fits a TOS
// TosServerError. The decision tree consults the shared s3common code
// table first; TOS-specific codes that are not yet in the shared table
// fall back to the HTTP status. Unknown codes land on ErrInternal as
// the documented catch-all.
//
// Listing TOS codes that surface during contract testing but are NOT
// yet in s3common.MapCodeString is part of the M3 volcengine validation
// goal — they are reported at execution time so the lead can extend
// s3common in a follow-up commit.
func mapServiceCode(code string, status int) uos.Code {
	if mapped, ok := s3common.MapCodeString(code); ok {
		return mapped
	}
	if mapped, ok := s3common.MapHTTPStatus(status); ok {
		return mapped
	}
	return uos.ErrInternal
}

// isRetryable hints whether a retry is reasonable. The driver disables
// the SDK's internal retryer at construction time (see factory.Open),
// so this field is the authoritative signal pkg/uos.RetryPolicy uses
// to decide. Delegates the per-Code decision to s3common.IsRetryable;
// the HTTP-5xx-without-vendor-code rescue stays inline because it
// depends on the wire status (not just the resolved Code).
func isRetryable(code uos.Code, status int) bool {
	if s3common.IsRetryable(code) {
		return true
	}
	// Some 5xx without a vendor Code still warrant a retry.
	if status >= 500 && status < 600 {
		return true
	}
	return false
}
