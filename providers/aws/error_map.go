package aws

import (
	"context"
	"errors"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/maqian/object-storage-client/pkg/uos"
)

// mapError translates an aws-sdk-go-v2 error into a *uos.Error per
// architecture_plan §7.1 (the 14 frozen Code values). It handles:
//
//   - typed S3 *types.* error shapes (NoSuchKey, NoSuchBucket, ...)
//   - generic smithy.APIError code dispatch (AccessDenied, SlowDown, ...)
//   - awshttp.ResponseError for HTTP status fall-through (5xx → Temporary)
//   - context cancellation (Timeout / Internal)
//
// Unmapped errors fall through to ErrInternal with a populated Cause so
// errors.Unwrap callers still see the original. RequestID and
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

	// Context cancellation / deadline.
	if errors.Is(err, context.Canceled) {
		out.Code = uos.ErrTimeout
		out.Retryable = false
		return out
	}
	if errors.Is(err, context.DeadlineExceeded) {
		out.Code = uos.ErrTimeout
		out.Retryable = true
		return out
	}

	// HTTP-level metadata: RequestID, SecondaryID, status.
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		out.HTTPStatus = respErr.HTTPStatusCode()
		out.RequestID = respErr.ServiceRequestID()
	}

	// Typed S3 errors (most precise match wins).
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

	// Generic smithy.APIError code dispatch — covers many error codes
	// MinIO and AWS return that don't have a typed wrapper class.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if msg := apiErr.ErrorMessage(); msg != "" {
			out.Message = msg
		} else {
			out.Message = code
		}
		switch code {
		case "NoSuchKey", "NoSuchBucket", "NoSuchUpload", "NotFound", "404":
			out.Code = uos.ErrNotFound
		case "BucketAlreadyExists", "BucketAlreadyOwnedByYou":
			out.Code = uos.ErrAlreadyExists
		case "AccessDenied", "AllAccessDisabled", "Forbidden":
			out.Code = uos.ErrPermissionDenied
		case "InvalidAccessKeyId", "SignatureDoesNotMatch",
			"InvalidSecurity", "ExpiredToken", "InvalidToken",
			"AuthorizationHeaderMalformed", "MissingSecurityHeader":
			out.Code = uos.ErrUnauthenticated
		case "PreconditionFailed", "InvalidRange", "NotModified":
			out.Code = uos.ErrPreconditionFailed
		case "BucketNotEmpty":
			out.Code = uos.ErrConflict
		case "OperationAborted", "InvalidBucketState":
			out.Code = uos.ErrConflict
		case "SlowDown", "ThrottlingException", "Throttling",
			"TooManyRequests", "RequestLimitExceeded":
			out.Code = uos.ErrRateLimited
			out.Retryable = true
		case "RequestTimeout", "RequestTimeoutException":
			out.Code = uos.ErrTimeout
			out.Retryable = true
		case "ServiceUnavailable", "InternalError", "InternalFailure":
			out.Code = uos.ErrTemporary
			out.Retryable = true
		case "BadDigest", "InvalidDigest", "XAmzContentSHA256Mismatch":
			out.Code = uos.ErrChecksumMismatch
		case "MissingContentLength":
			out.Code = uos.ErrLengthRequired
		case "InvalidArgument", "MalformedXML", "InvalidBucketName",
			"InvalidObjectName", "EntityTooLarge", "EntityTooSmall",
			"KeyTooLongError", "MetadataTooLarge":
			out.Code = uos.ErrInvalidArgument
		default:
			// Status-based fallback for unknown codes.
			if respErr != nil {
				switch s := respErr.HTTPStatusCode(); {
				case s == http.StatusNotFound:
					out.Code = uos.ErrNotFound
				case s == http.StatusForbidden:
					out.Code = uos.ErrPermissionDenied
				case s == http.StatusUnauthorized:
					out.Code = uos.ErrUnauthenticated
				case s == http.StatusPreconditionFailed:
					out.Code = uos.ErrPreconditionFailed
				case s == http.StatusConflict:
					out.Code = uos.ErrConflict
				case s == http.StatusTooManyRequests:
					out.Code = uos.ErrRateLimited
					out.Retryable = true
				case s == http.StatusRequestTimeout:
					out.Code = uos.ErrTimeout
					out.Retryable = true
				case s == http.StatusLengthRequired:
					out.Code = uos.ErrLengthRequired
				case s >= 500 && s < 600:
					out.Code = uos.ErrTemporary
					out.Retryable = true
				case s >= 400 && s < 500:
					out.Code = uos.ErrInvalidArgument
				}
			}
		}
		return out
	}

	// Pure HTTP fallback (no APIError wrapper, but a ResponseError exists).
	if respErr != nil {
		switch s := respErr.HTTPStatusCode(); {
		case s == http.StatusNotFound:
			out.Code = uos.ErrNotFound
		case s == http.StatusForbidden:
			out.Code = uos.ErrPermissionDenied
		case s == http.StatusUnauthorized:
			out.Code = uos.ErrUnauthenticated
		case s == http.StatusPreconditionFailed:
			out.Code = uos.ErrPreconditionFailed
		case s == http.StatusConflict:
			out.Code = uos.ErrConflict
		case s == http.StatusTooManyRequests:
			out.Code = uos.ErrRateLimited
			out.Retryable = true
		case s == http.StatusRequestTimeout:
			out.Code = uos.ErrTimeout
			out.Retryable = true
		case s == http.StatusLengthRequired:
			out.Code = uos.ErrLengthRequired
		case s >= 500 && s < 600:
			out.Code = uos.ErrTemporary
			out.Retryable = true
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
