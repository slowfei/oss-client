package tencent

import (
	"context"
	"errors"

	"github.com/tencentyun/cos-go-sdk-v5"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/s3common"
)

// mapError translates a cos-go-sdk-v5 error into a *uos.Error per
// architecture_plan §7.1 (the 14 frozen Code values). It handles:
//
//   - Pass-through of any *uos.Error already wrapped upstream
//     (capability gating, argument validation, etc.).
//   - *cos.ErrorResponse, the SDK's wire-error shape carrying
//     RequestID, TraceID, Code, Message, and the underlying http.Response.
//   - *cos.RetryError, the wrapper the SDK uses when its retryer gave
//     up (we set RetryOpt.Count=1 so this should be rare; we still
//     unwrap it for defence in depth).
//   - Generic fall-through to pkg/uos/s3common helpers (MapCodeString
//     for the wire code string, MapHTTPStatus for the status fallback,
//     MapContextErr for context cancellation), then ErrInternal as the
//     documented catch-all.
//
// architecture_plan §7.1 forbids returning anything outside the 14
// frozen Codes; this function is the single chokepoint that enforces
// that rule for the tencent driver.
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

	// Unwrap *cos.RetryError so we can inspect the underlying typed
	// error. The SDK only wraps when its retryer (which we disabled by
	// setting RetryOpt.Count=1) made multiple attempts; the last attempt
	// is at the end of the slice.
	if rerr := (*cos.RetryError)(nil); errors.As(err, &rerr) && len(rerr.Errs) > 0 {
		err = rerr.Errs[len(rerr.Errs)-1]
	}

	// Most COS data-plane errors arrive as *cos.ErrorResponse. The SDK
	// populates Code + Message + RequestID from the wire response and
	// keeps the original *http.Response (carrying StatusCode + headers).
	var svcErr *cos.ErrorResponse
	if errors.As(err, &svcErr) {
		if svcErr.Response != nil {
			out.HTTPStatus = svcErr.Response.StatusCode
		}
		out.RequestID = svcErr.RequestID
		// TraceID is the COS server-side trace identifier; carry it as
		// the secondary identifier so callers don't lose it during triage.
		out.SecondaryID = svcErr.TraceID
		if svcErr.Message != "" {
			out.Message = svcErr.Message
		}
		out.Code = mapServiceCode(svcErr.Code, out.HTTPStatus)
		out.Retryable = isRetryable(out.Code, out.HTTPStatus)
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

// mapServiceCode picks the frozen pkg/uos.Code that best fits a COS
// ErrorResponse. The decision tree consults the shared s3common code
// table first; codes that are not yet in the shared table fall back to
// the HTTP status. Unknown codes land on ErrInternal as the documented
// catch-all.
//
// Listing COS codes that surface during contract testing but are NOT
// yet in s3common.MapCodeString is part of the M3 tencent validation
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

// isRetryable hints whether a retry is reasonable. The driver itself
// owns no internal retryer (we explicitly disabled cos-go-sdk-v5's by
// setting RetryOpt.Count=1 in factory.Open); the field exists so
// callers + pkg/uos.RetryPolicy can decide. Delegates the per-Code
// decision to s3common.IsRetryable; the HTTP-5xx-without-vendor-code
// rescue stays inline because it depends on the wire status (not just
// the resolved Code).
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
