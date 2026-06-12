package qiniu

import (
	"context"
	"errors"
	"net/http"
	"strings"

	qclient "github.com/qiniu/go-sdk/v7/client"
	"github.com/qiniu/go-sdk/v7/storage"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// mapError translates a qiniu/go-sdk error into a *uos.Error per
// architecture_plan §7.1 (the 14 frozen Code values). Decision tree:
//
//  1. Pass-through any *uos.Error already produced upstream.
//  2. SDK sentinels (storage.ErrBucketNotExist / storage.ErrNoSuchFile) — direct map to ErrNotFound.
//  3. *qclient.ErrorInfo — vendor's typed HTTP error; consult the local
//     qiniu reason-string table first, then s3common.MapHTTPStatus on the Code field.
//  4. context.Canceled / DeadlineExceeded → ErrTimeout via s3common.MapContextErr.
//  5. Catch-all → ErrInternal with Cause preserved.
//
// Qiniu reason strings (e.g. "file exists", "no such file or directory")
// are vendor-specific and MUST NOT be added to s3common.MapCodeString —
// the local mapQiniuReason switch below is the sole translation point for
// Qiniu wire reasons. This boundary mirrors the gcs and azure drivers
// (see Lessons (M4) in provider_roadmap.md).
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

	// SDK sentinels short-circuit the wire-level inspection.
	if errors.Is(err, storage.ErrBucketNotExist) || errors.Is(err, storage.ErrNoSuchFile) {
		out.Code = uos.ErrNotFound
		out.HTTPStatus = http.StatusNotFound
		return out
	}

	// *qclient.ErrorInfo is the typed error returned by the qiniu SDK for
	// all HTTP-level failures. Code is the HTTP status (200..599); Err is
	// the vendor reason string ("no such file or directory", "file exists",
	// etc.); Reqid is the X-Reqid header.
	var ei *qclient.ErrorInfo
	if errors.As(err, &ei) {
		out.HTTPStatus = ei.Code
		out.RequestID = ei.Reqid
		if ei.Err != "" {
			out.Message = ei.Err
		}
		// Consult the local Qiniu-specific reason table first.
		if code, ok := mapQiniuReason(ei.Err); ok {
			out.Code = code
			out.Retryable = s3common.IsRetryable(code) || isQiniuRetryableStatus(ei.Code)
			return out
		}
		// Fall back to the HTTP-status table shared by all drivers.
		if code, ok := s3common.MapHTTPStatus(ei.Code); ok {
			out.Code = code
			out.Retryable = s3common.IsRetryable(code) || isQiniuRetryableStatus(ei.Code)
			return out
		}
		// Unknown status — stay at ErrInternal but flag retryable on 5xx.
		out.Retryable = isQiniuRetryableStatus(ei.Code)
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

// mapQiniuReason maps Qiniu wire reason strings (the .Err field of
// *qclient.ErrorInfo) to the 14 frozen pkg/uos.Code values. The vocabulary
// comes from the Kodo HTTP API documentation and the SDK's own error
// constants (storage.ErrBucketNotExist / ErrNoSuchFile).
//
// Match is case-insensitive prefix match because Qiniu sometimes appends
// a trailing identifier to the canonical reason (e.g. "file exists, hash
// xxxxx"). Returns ("", false) for unrecognised reasons; callers fall
// through to the HTTP-status table.
//
// This LOCAL switch is the qiniu-only counterpart to s3common.MapCodeString;
// per the M4 brief, non-S3-family drivers do NOT extend MapCodeString —
// they keep their vendor vocabulary in their own error_map.go.
func mapQiniuReason(reason string) (uos.Code, bool) {
	r := strings.ToLower(strings.TrimSpace(reason))
	switch {
	// Not-found family
	case strings.HasPrefix(r, "no such file") ||
		strings.HasPrefix(r, "no such bucket") ||
		strings.HasPrefix(r, "bucket not exist") ||
		strings.HasPrefix(r, "no such entry") ||
		strings.HasPrefix(r, "key not exists") ||
		strings.Contains(r, "not found"):
		return uos.ErrNotFound, true

	// Already-exists family
	case strings.HasPrefix(r, "file exists") ||
		strings.HasPrefix(r, "bucket exists") ||
		strings.Contains(r, "already exist"):
		return uos.ErrAlreadyExists, true

	// Auth / permission family
	case strings.HasPrefix(r, "bad token") ||
		strings.HasPrefix(r, "invalid token") ||
		strings.HasPrefix(r, "expired token") ||
		strings.HasPrefix(r, "invalid uptoken") ||
		strings.Contains(r, "bad authorization") ||
		strings.Contains(r, "invalid signature") ||
		strings.Contains(r, "invalid accesskey"):
		return uos.ErrUnauthenticated, true

	case strings.Contains(r, "permission denied") ||
		strings.Contains(r, "access denied") ||
		strings.HasPrefix(r, "forbidden"):
		return uos.ErrPermissionDenied, true

	// Conflict / state family (Qiniu uses 612/614 with these reasons)
	case strings.Contains(r, "callback url conflict") ||
		strings.Contains(r, "bucket is locked") ||
		strings.Contains(r, "in progress"):
		return uos.ErrConflict, true

	// Rate-limit / throttle family
	case strings.Contains(r, "too many requests") ||
		strings.Contains(r, "rate limit") ||
		strings.Contains(r, "throttle"):
		return uos.ErrRateLimited, true

	// Checksum / integrity family (Qiniu signals via 406 / 619)
	case strings.Contains(r, "etag mismatch") ||
		strings.Contains(r, "md5 mismatch") ||
		strings.Contains(r, "crc32 mismatch") ||
		strings.Contains(r, "data corrupted"):
		return uos.ErrChecksumMismatch, true

	// Length-required (Qiniu signals via 400 with this reason for chunked uploads)
	case strings.Contains(r, "content-length required") ||
		strings.Contains(r, "missing content-length"):
		return uos.ErrLengthRequired, true

	// Invalid-argument family
	case strings.Contains(r, "invalid bucket") ||
		strings.Contains(r, "invalid key") ||
		strings.Contains(r, "invalid argument") ||
		strings.Contains(r, "invalid parameter") ||
		strings.Contains(r, "invalid range") ||
		strings.Contains(r, "invalid mimetype") ||
		strings.Contains(r, "invalid request") ||
		strings.Contains(r, "key too long") ||
		strings.Contains(r, "bad request"):
		return uos.ErrInvalidArgument, true
	}
	return "", false
}

// isQiniuRetryableStatus returns true for HTTP status codes that Qiniu
// documents as transient and retry-worthy beyond the three frozen
// pkg/uos Codes (rate_limited / timeout / temporary) already covered by
// s3common.IsRetryable. Qiniu uses some non-standard codes (5xx + 6xx)
// for vendor-specific transient errors.
func isQiniuRetryableStatus(status int) bool {
	switch {
	case status == http.StatusServiceUnavailable, // 503
		status == http.StatusGatewayTimeout,      // 504
		status == http.StatusInternalServerError, // 500
		status == http.StatusBadGateway:          // 502
		return true
	case status >= 570 && status < 580:
		// Qiniu vendor-specific 57x range is documented as upstream-busy
		// (e.g. 573 single-bucket rate limit, 579 callback failed).
		return true
	}
	return false
}
