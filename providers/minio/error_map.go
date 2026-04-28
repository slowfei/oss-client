package minio

import (
	"context"
	"errors"
	"net/http"

	miniogo "github.com/minio/minio-go/v7"

	"github.com/maqian/object-storage-client/pkg/uos"
)

// mapError translates a minio-go error into a *uos.Error tagged with the
// caller-supplied operation / bucket / key context. Returning nil on a
// nil err keeps call sites uncluttered. The mapping below covers every
// frozen pkg/uos.Code reachable through the S3 wire protocol; any
// vendor code we do not recognise falls through to ErrInternal with the
// original error preserved in Cause for errors.Unwrap traversal.
//
// architecture_plan §7.1 forbids returning anything outside the 14
// frozen Codes; this function is the single chokepoint that enforces
// that rule for the MinIO driver.
func mapError(provider uos.Provider, op, bucket, key string, err error) error {
	if err == nil {
		return nil
	}
	// If the call site already produced a *uos.Error (e.g. capability
	// gating or argument validation), pass it straight through.
	var alreadyMapped *uos.Error
	if errors.As(err, &alreadyMapped) {
		return alreadyMapped
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
// The decision tree consults the vendor Code string first (the wire
// signal), then falls back to HTTP status, then to context errors, and
// finally lands on ErrInternal as the documented catch-all.
func mapErrorCode(resp miniogo.ErrorResponse, err error) uos.Code {
	switch resp.Code {
	case miniogo.NoSuchBucket,
		miniogo.NoSuchKey,
		miniogo.NoSuchVersion,
		miniogo.NoSuchUpload,
		miniogo.NoSuchBucketPolicy,
		miniogo.NoSuchCORSConfiguration,
		miniogo.NoSuchTagSet:
		return uos.ErrNotFound
	case miniogo.BucketAlreadyExists,
		miniogo.BucketAlreadyOwnedByYou:
		return uos.ErrAlreadyExists
	case miniogo.AccessDenied,
		miniogo.AllAccessDisabled,
		miniogo.MethodNotAllowed,
		"AuthorizationHeaderMalformed":
		return uos.ErrPermissionDenied
	case miniogo.InvalidAccessKeyID,
		miniogo.SignatureDoesNotMatch,
		"InvalidSecurity",
		"InvalidToken",
		"ExpiredToken":
		return uos.ErrUnauthenticated
	case miniogo.PreconditionFailed,
		"NotModified":
		return uos.ErrPreconditionFailed
	case miniogo.Conflict,
		miniogo.BucketNotEmpty,
		"OperationAborted":
		return uos.ErrConflict
	case "SlowDown", "TooManyRequests", "ThrottlingException":
		return uos.ErrRateLimited
	case "RequestTimeout", "RequestTimeoutException":
		return uos.ErrTimeout
	case miniogo.RequestTimeTooSkewed,
		miniogo.MalformedDate,
		miniogo.MalformedXML,
		miniogo.MalformedPOSTRequest,
		miniogo.MalformedPolicy,
		miniogo.InvalidArgument,
		miniogo.InvalidBucketName,
		miniogo.InvalidRegion,
		miniogo.InvalidPart,
		miniogo.InvalidPartOrder,
		miniogo.InvalidObjectState,
		miniogo.InvalidRange,
		miniogo.InvalidDigest,
		miniogo.InvalidDuration,
		miniogo.AuthorizationQueryParametersError,
		miniogo.MissingFields,
		miniogo.MissingRequestBodyError,
		miniogo.XMinioInvalidObjectName,
		miniogo.APINotSupported,
		miniogo.NotImplemented,
		miniogo.EntityTooLarge,
		miniogo.EntityTooSmall:
		return uos.ErrInvalidArgument
	case miniogo.MissingContentLength:
		return uos.ErrLengthRequired
	case miniogo.BadDigest, miniogo.IncompleteBody, miniogo.UnexpectedEOF:
		return uos.ErrChecksumMismatch
	case miniogo.InternalError:
		return uos.ErrTemporary
	}

	// Status-only fallbacks for cases where the vendor didn't supply a
	// Code (or supplied one we don't recognise). Order matters: more
	// specific codes win over generic 4xx/5xx.
	switch resp.StatusCode {
	case http.StatusNotFound:
		return uos.ErrNotFound
	case http.StatusUnauthorized:
		return uos.ErrUnauthenticated
	case http.StatusForbidden:
		return uos.ErrPermissionDenied
	case http.StatusConflict:
		return uos.ErrConflict
	case http.StatusPreconditionFailed:
		return uos.ErrPreconditionFailed
	case http.StatusRequestTimeout:
		return uos.ErrTimeout
	case http.StatusTooManyRequests:
		return uos.ErrRateLimited
	case http.StatusLengthRequired:
		return uos.ErrLengthRequired
	}
	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		return uos.ErrTemporary
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return uos.ErrInvalidArgument
	}

	// Context cancellation / deadline manifests as a non-S3 error.
	if errors.Is(err, context.DeadlineExceeded) {
		return uos.ErrTimeout
	}
	if errors.Is(err, context.Canceled) {
		return uos.ErrTimeout
	}
	return uos.ErrInternal
}

// errorMessage returns the most descriptive message available, preferring
// the vendor-supplied Message and falling back to err.Error().
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
func isRetryable(code uos.Code, resp miniogo.ErrorResponse) bool {
	switch code {
	case uos.ErrRateLimited, uos.ErrTimeout, uos.ErrTemporary:
		return true
	}
	// Some 5xx without a vendor Code still warrant a retry.
	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		return true
	}
	return false
}
