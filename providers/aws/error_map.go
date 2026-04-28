package aws

import (
	"context"
	"errors"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/s3common"
)

// mapError translates an aws-sdk-go-v2 error into a *uos.Error per
// architecture_plan §7.1 (the 14 frozen Code values). It handles:
//
//   - typed S3 *types.* error shapes (NoSuchKey, NoSuchBucket, ...)
//   - generic smithy.APIError code dispatch (delegated to
//     pkg/uos/s3common.MapCodeString — shared across all S3-family
//     drivers).
//   - awshttp.ResponseError for HTTP status fall-through (delegated
//     to pkg/uos/s3common.MapHTTPStatus).
//   - context cancellation / deadline (delegated to
//     pkg/uos/s3common.MapContextErr).
//
// Unmapped errors fall through to ErrInternal with a populated Cause
// so errors.Unwrap callers still see the original. RequestID and
// SecondaryID (extended request id) are populated whenever the AWS
// response carries them.
//
// nil err returns nil.
func mapError(op, bucket, key string, err error) error {
	if err == nil {
		return nil
	}
	out := &uos.Error{
		Provider:  providerID,
		Operation: op,
		Bucket:    bucket,
		Key:       key,
		Code:      uos.ErrInternal,
		Message:   err.Error(),
		Cause:     err,
	}

	// Context cancellation / deadline (cheapest check first; ambient
	// ctx errors typically fire before any vendor-side classification
	// matters). Distinguish Canceled (caller intent — do NOT retry)
	// from DeadlineExceeded (transient — DO retry).
	if code, ok := s3common.MapContextErr(err); ok {
		out.Code = code
		out.Retryable = errors.Is(err, context.DeadlineExceeded)
		return out
	}

	// HTTP-level metadata: RequestID, SecondaryID, status. Capture
	// before classification so that fall-through paths can still use
	// them.
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		out.HTTPStatus = respErr.HTTPStatusCode()
		out.RequestID = respErr.ServiceRequestID()
	}

	// Typed S3 errors (most precise match wins). These vendor-typed
	// shapes carry richer context than the wire-level code string;
	// keep them inline so message text stays vendor-flavoured.
	switch typed := err.(type) {
	case *types.NoSuchKey:
		out.Code = uos.ErrNotFound
		out.Message = msgOr(typed.Message, "object not found")
		return out
	case *types.NoSuchBucket:
		out.Code = uos.ErrNotFound
		out.Message = msgOr(typed.Message, "bucket not found")
		return out
	case *types.NoSuchUpload:
		out.Code = uos.ErrNotFound
		out.Message = msgOr(typed.Message, "multipart upload not found")
		return out
	case *types.NotFound:
		out.Code = uos.ErrNotFound
		out.Message = msgOr(typed.Message, "not found")
		return out
	case *types.BucketAlreadyExists:
		out.Code = uos.ErrAlreadyExists
		out.Message = msgOr(typed.Message, "bucket already exists in another account")
		return out
	case *types.BucketAlreadyOwnedByYou:
		out.Code = uos.ErrAlreadyExists
		out.Message = msgOr(typed.Message, "bucket already exists and is owned by you")
		return out
	case *types.IdempotencyParameterMismatch:
		out.Code = uos.ErrConflict
		out.Message = msgOr(typed.Message, "idempotency parameter mismatch")
		return out
	case *types.InvalidObjectState:
		out.Code = uos.ErrConflict
		out.Message = msgOr(typed.Message, "invalid object state")
		return out
	case *types.InvalidRequest:
		out.Code = uos.ErrInvalidArgument
		out.Message = msgOr(typed.Message, "invalid request")
		return out
	case *types.InvalidWriteOffset:
		out.Code = uos.ErrInvalidArgument
		out.Message = msgOr(typed.Message, "invalid write offset")
		return out
	case *types.TooManyParts:
		out.Code = uos.ErrInvalidArgument
		out.Message = msgOr(typed.Message, "too many parts")
		return out
	case *types.AccessDenied:
		out.Code = uos.ErrPermissionDenied
		out.Message = msgOr(typed.Message, "access denied")
		return out
	case *types.EncryptionTypeMismatch:
		out.Code = uos.ErrPreconditionFailed
		out.Message = msgOr(typed.Message, "encryption type mismatch")
		return out
	case *types.ObjectAlreadyInActiveTierError:
		out.Code = uos.ErrConflict
		out.Message = msgOr(typed.Message, "object already in active tier")
		return out
	case *types.ObjectNotInActiveTierError:
		out.Code = uos.ErrConflict
		out.Message = msgOr(typed.Message, "object not in active tier")
		return out
	}

	// Generic smithy.APIError code dispatch via the shared S3-compat
	// table. Covers the codes MinIO and AWS both emit for error
	// shapes that don't have a typed wrapper class.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if msg := apiErr.ErrorMessage(); msg != "" {
			out.Message = msg
		} else {
			out.Message = code
		}
		if mapped, ok := s3common.MapCodeString(code); ok {
			out.Code = mapped
			out.Retryable = s3common.IsRetryable(mapped)
			return out
		}
		// Unknown code string — fall through to status-based mapping.
	}

	// HTTP-status fallback (catches both APIError-but-unknown-code
	// and pure-HTTP errors with no APIError wrapper).
	if respErr != nil {
		if mapped, ok := s3common.MapHTTPStatus(respErr.HTTPStatusCode()); ok {
			out.Code = mapped
			out.Retryable = s3common.IsRetryable(mapped)
		}
	}
	return out
}

// msgOr returns *p when non-nil/non-empty, otherwise fallback. Used to
// prefer the vendor's human-readable Message field where present.
func msgOr(p *string, fallback string) string {
	if p != nil && *p != "" {
		return *p
	}
	return fallback
}
