package minio

import (
	"errors"

	miniogo "github.com/minio/minio-go/v7"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/s3common"
)

// mapError translates a minio-go error into a *uos.Error tagged with
// the caller-supplied operation / bucket / key context. Returning nil
// on a nil err keeps call sites uncluttered. The mapping below covers
// every frozen pkg/uos.Code reachable through the S3 wire protocol;
// any vendor code we do not recognise falls through to ErrInternal
// with the original error preserved in Cause for errors.Unwrap
// traversal.
//
// architecture_plan §7.1 forbids returning anything outside the 14
// frozen Codes; this function (plus pkg/uos/s3common.MapCodeString /
// MapHTTPStatus / MapContextErr) is the single chokepoint that
// enforces that rule for the MinIO driver.
func mapError(provider uos.Provider, op, bucket, key string, err error) error {
	if err == nil {
		return nil
	}
	// Pass through any *uos.Error already produced upstream
	// (capability gating, argument validation, etc.).
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

	resp := miniogo.ToErrorResponse(err)
	out := &uos.Error{
		Provider:   provider,
		Operation:  op,
		Bucket:     bucket,
		Key:        key,
		HTTPStatus: resp.StatusCode,
		RequestID:  resp.RequestID,
		// HostID is the AWS extended request id; carry it as the
		// secondary identifier so callers don't lose it during triage.
		SecondaryID: resp.HostID,
		Message:     errorMessage(resp, err),
		Cause:       err,
	}

	out.Code = mapErrorCode(resp, err)
	out.Retryable = isRetryable(out.Code, resp)
	return out
}

// mapErrorCode picks the frozen pkg/uos.Code that best fits resp/err.
// The decision tree consults the vendor Code string (S3-compat wire
// signal) via the shared s3common.MapCodeString table, then falls
// back to HTTP status, then to context errors, and finally lands on
// ErrInternal as the documented catch-all.
//
// miniogo's package-level constants (NoSuchKey, NoSuchBucket, ...)
// are typed as plain strings, so feeding string(resp.Code) to
// s3common.MapCodeString resolves them transparently.
func mapErrorCode(resp miniogo.ErrorResponse, err error) uos.Code {
	if code, ok := s3common.MapCodeString(resp.Code); ok {
		return code
	}
	if code, ok := s3common.MapHTTPStatus(resp.StatusCode); ok {
		return code
	}
	if code, ok := s3common.MapContextErr(err); ok {
		return code
	}
	return uos.ErrInternal
}

// errorMessage returns the most descriptive message available,
// preferring the vendor-supplied Message and falling back to err.Error().
func errorMessage(resp miniogo.ErrorResponse, err error) string {
	if resp.Message != "" {
		return resp.Message
	}
	return err.Error()
}

// isRetryable hints whether a retry is reasonable. The driver itself
// disables minio-go's internal retryer (per docs/provider_roadmap.md
// cross-cutting risk: "double retry storm"); the field exists so
// callers + pkg/uos.RetryPolicy can decide.
//
// Delegates the per-Code decision to pkg/uos/s3common.IsRetryable;
// the HTTP-5xx-without-vendor-code rescue stays inline because it
// depends on resp.StatusCode (not just the resolved Code).
func isRetryable(code uos.Code, resp miniogo.ErrorResponse) bool {
	if s3common.IsRetryable(code) {
		return true
	}
	// Some 5xx without a vendor Code still warrant a retry.
	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		return true
	}
	return false
}
