package azure

import (
	"context"
	"errors"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/s3common"
)

// mapError translates an azblob/azcore error into a *uos.Error per
// architecture_plan §7.1 (the 14 frozen Code values). Decision tree:
//
//  1. Pass-through any *uos.Error already produced upstream.
//  2. *azcore.ResponseError — vendor's typed HTTP error; consult the
//     local Azure error-code table first, then s3common.MapHTTPStatus.
//  3. context.Canceled / DeadlineExceeded → ErrTimeout via s3common.MapContextErr.
//  4. Catch-all → ErrInternal with Cause preserved.
//
// Azure ErrorCodes are vendor-specific strings (e.g. "BlobNotFound",
// "ContainerNotFound"). They are NOT S3-compat codes and MUST NOT be
// added to s3common.MapCodeString — the local mapAzureErrorCode switch
// below is the sole translation point for Azure wire codes.
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

	// *azcore.ResponseError is the typed error returned by the Azure SDK
	// for all HTTP-level failures. It carries StatusCode, ErrorCode
	// (the Azure x-ms-error-code string), and a RawResponse.
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		out.HTTPStatus = respErr.StatusCode
		if respErr.RawResponse != nil {
			out.RequestID = respErr.RawResponse.Header.Get("x-ms-request-id")
			out.SecondaryID = respErr.RawResponse.Header.Get("x-ms-client-request-id")
		}
		if respErr.ErrorCode != "" {
			out.Message = respErr.ErrorCode
		}

		// Consult the local Azure-specific code table first.
		if code, ok := mapAzureErrorCode(respErr.ErrorCode); ok {
			out.Code = code
			out.Retryable = s3common.IsRetryable(code) || isAzureRetryableStatus(respErr.StatusCode)
			return out
		}
		// Fall back to the HTTP-status table shared by all drivers.
		if code, ok := s3common.MapHTTPStatus(respErr.StatusCode); ok {
			out.Code = code
			out.Retryable = s3common.IsRetryable(code) || isAzureRetryableStatus(respErr.StatusCode)
			return out
		}
		// Unknown status — stay at ErrInternal but set Retryable on 5xx.
		out.Retryable = isAzureRetryableStatus(respErr.StatusCode)
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

// mapAzureErrorCode maps Azure Blob Storage wire error-code strings to the
// 14 frozen pkg/uos.Code values. Azure error codes are documented at:
// https://learn.microsoft.com/en-us/rest/api/storageservices/blob-service-error-codes
//
// These codes are vendor-specific and must NOT be added to
// s3common.MapCodeString (which is S3-compat only). Any new Azure code
// discovered during contract testing belongs here.
func mapAzureErrorCode(code string) (uos.Code, bool) {
	switch code {
	// Not-found family
	case "BlobNotFound", "ContainerNotFound", "BlobAccessTierNotSupported",
		"ResourceNotFound", "ShareNotFound", "QueueNotFound",
		"TableNotFound", "EntityNotFound":
		return uos.ErrNotFound, true

	// Already-exists family
	case "ContainerAlreadyExists", "BlobAlreadyExists",
		"ResourceAlreadyExists":
		return uos.ErrAlreadyExists, true

	// Auth / permission family
	case "AuthenticationFailed", "AuthorizationFailure",
		"AccountIsDisabled", "AuthorizationPermissionMismatch",
		"AuthorizationProtocolMismatch", "AuthorizationResourceTypeMismatch",
		"AuthorizationServiceMismatch", "AuthorizationSourceIPMismatch",
		"KeyVaultEncryptionKeyNotFound", "KeyVaultErrorDuringAuthentication":
		return uos.ErrUnauthenticated, true

	case "InsufficientAccountPermissions", "ContainerDisabled",
		"BlobArchived", "PublicAccessNotPermitted",
		"SasTokenLacksWritePermission", "SasTokenExpired",
		"AccessTierNotSupported":
		return uos.ErrPermissionDenied, true

	// Precondition family
	case "ConditionNotMet", "BlobOverwritten",
		"SourceConditionNotMet", "TargetConditionNotMet":
		return uos.ErrPreconditionFailed, true

	// Conflict / state family
	case "ContainerBeingDeleted", "ContainerNotFound_Lease",
		"LeaseAlreadyPresent", "LeaseAlreadyBroken",
		"LeaseIdMismatchWithBlobOperation", "LeaseIdMismatchWithContainerOperation",
		"LeaseIdMismatchWithLeaseOperation", "LeaseIdMissing",
		"LeaseIsBreakingAndCannotBeAcquired", "LeaseIsBreakingAndCannotBeChanged",
		"LeaseIsBrokenAndCannotBeRenewed", "LeaseLost",
		"LeaseNotPresentWithBlobOperation", "LeaseNotPresentWithContainerOperation",
		"LeaseNotPresentWithLeaseOperation", "NoPendingCopyOperation",
		"CopyIdMismatch", "SnapshotsPresent", "InvalidBlobOrBlock",
		"BlockCountExceedsLimit", "InvalidBlockList",
		"BlobImmutableDueToPolicy", "BlobBeingRehydrated",
		"OperationNotAllowedOnIncrementalCopyBlob":
		return uos.ErrConflict, true

	// Rate-limit / throttle family
	case "ServerBusy", "OperationTimedOut",
		"IngressOverAccountLimit", "EgressOverAccountLimit",
		"OperationsRateOverAccountLimit", "TooManyRequests":
		return uos.ErrRateLimited, true

	// Timeout family (distinct from rate limit — transient server timeout)
	case "RequestTimeout", "RequestTimeTooSkewed":
		return uos.ErrTimeout, true

	// Checksum / integrity family
	case "Md5Mismatch", "CrcMismatch":
		return uos.ErrChecksumMismatch, true

	// Missing content-length
	case "MissingContentLengthHeader", "MissingRequiredHeader":
		return uos.ErrLengthRequired, true

	// Invalid argument family
	case "InvalidHeaderValue", "InvalidInput", "InvalidMd5",
		"InvalidMetadata", "InvalidQueryParameterValue",
		"InvalidRange", "InvalidResourceName", "InvalidUri",
		"InvalidXmlDocument", "InvalidXmlNodeValue",
		"MissingRequiredQueryParameter", "MissingRequiredXmlNode",
		"MultipleConditionHeadersNotSupported", "OutOfRangeInput",
		"OutOfRangeQueryParameterValue", "UnsupportedHeader",
		"UnsupportedHttpVerb", "UnsupportedQueryParameter",
		"UnsupportedXmlNode", "EmptyMetadataKey",
		"InvalidBlockId", "InvalidVersionForPageBlobOperation",
		"XMsVersionNotSupportedForBlobType",
		"BlobTypeNotSupported", "AppendPositionConditionNotMet",
		"MaxBlobSizeConditionNotMet":
		return uos.ErrInvalidArgument, true

	// Transient infrastructure errors
	case "InternalError", "InternalErrorV2":
		return uos.ErrTemporary, true
	}
	return "", false
}

// isAzureRetryableStatus returns true for HTTP status codes that Azure
// documents as transient and retry-worthy, beyond the three frozen
// pkg/uos Codes (rate_limited / timeout / temporary) already covered by
// s3common.IsRetryable.
func isAzureRetryableStatus(status int) bool {
	switch status {
	case http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout,      // 504
		http.StatusInternalServerError, // 500
		http.StatusBadGateway:          // 502
		return true
	}
	return false
}
