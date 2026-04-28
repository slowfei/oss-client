package uos

import (
	"context"
	"time"

	"github.com/maqian/object-storage-client/pkg/uos/credential"
	"github.com/maqian/object-storage-client/pkg/uos/httpx"
	"github.com/maqian/object-storage-client/pkg/uos/middleware"
)

// Config is the top-level instantiation parameter passed to
// Registry.Open / Factory.Open. Field semantics:
//
//   - Provider names which driver to dispatch to. Required.
//   - Region / Endpoint are vendor-defined; either or both may be needed.
//   - CredentialProvider resolves credentials lazily; nil means anonymous.
//   - HTTP, Retry, Transfer, Middleware are optional knobs with safe defaults.
//   - DriverConfig is the escape hatch for vendor-specific options;
//     Factory.Validate type-asserts it.
//
// Config is value-typed; copies are cheap and goroutine-safe.
type Config struct {
	// Provider names the target driver, e.g. "aws", "alibaba". Required.
	Provider Provider
	// Region is the vendor-defined region identifier (e.g. "us-east-1",
	// "cn-hangzhou"). Required by most drivers; empty when the vendor
	// auto-resolves region from Endpoint.
	Region string
	// Endpoint is an optional override for the vendor-default endpoint.
	// Used for self-hosted S3-compatibles (MinIO) and for region-pinning
	// in vendors that need it.
	Endpoint string
	// CredentialProvider resolves credentials. nil means anonymous —
	// drivers that require credentials reject this at Validate time.
	CredentialProvider credential.Provider
	// HTTP tunes the HTTP transport. Zero value uses stdlib defaults
	// (see httpx.NewClient).
	HTTP httpx.HTTPConfig
	// Retry configures retry behavior for both pkg/uos-orchestrated
	// retries and driver translation into vendor-SDK retryers.
	Retry RetryPolicy
	// Transfer configures the transfer.Manager used by the unified
	// upload entry point. The any-typed indirection avoids importing
	// pkg/uos/transfer here (Stream A reserves that file).
	Transfer any
	// Middleware bundles cross-cutting hooks (logging, metrics, tracing).
	// Zero value is a no-op chain (see middleware.NoopChain).
	Middleware middleware.Chain
	// DriverConfig is the escape hatch for vendor-specific options that
	// don't fit the unified surface. Factory.Validate type-asserts it
	// to the driver's expected concrete type and rejects mismatches.
	DriverConfig any
}

// RetryPolicy is the v1 retry contract for both pkg/uos-orchestrated
// retries and driver translation into vendor-SDK retryers.
//
// Drivers MUST translate this once at construction time and disable
// any duplicate vendor-internal retry layer (per docs/provider_roadmap
// cross-cutting risk: "double retry storm").
//
// The IsIdempotent hook lets callers extend the retry-eligibility
// logic without recompiling pkg/uos. A nil IsIdempotent falls back to
// DefaultIsIdempotent, which retries the safe HTTP verbs.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts (including the first).
	// 0 means "use driver default" (typically 3).
	MaxAttempts int
	// BaseBackoff is the initial backoff after the first failure.
	// 0 means "use driver default" (typically 100ms).
	BaseBackoff time.Duration
	// MaxBackoff is the upper bound on per-attempt backoff. 0 means
	// "use driver default" (typically 20s).
	MaxBackoff time.Duration
	// Jitter is the proportional jitter applied to each backoff
	// (0.0..1.0). 0 means "use driver default" (typically 0.2).
	Jitter float64
	// IsIdempotent decides whether a given high-level operation is
	// safe to retry. nil → DefaultIsIdempotent.
	IsIdempotent func(op string) bool
	// RetryableCodes restricts retry to these pkg/uos.Code values.
	// Empty → driver default (typically: rate_limited, timeout, temporary).
	RetryableCodes []Code
}

// DefaultIsIdempotent is the fallback IsIdempotent predicate. It
// retries the conventional safe operations and refuses to retry
// non-idempotent ones (Put, Initiate, UploadPart, Complete, Abort)
// unless the caller opts in via a custom IsIdempotent.
//
// This list is intentionally conservative: PutObject is NOT idempotent
// in general because the same key may be overwritten by another writer
// between attempts. Callers who know their writer is the sole producer
// (or who carry If-None-Match preconditions) can override.
func DefaultIsIdempotent(op string) bool {
	switch op {
	case "ListBuckets", "StatBucket",
		"GetObject", "HeadObject", "ListObjects", "ExistsObject",
		"ListMultipartUploads",
		"SignURL":
		return true
	default:
		return false
	}
}

// Factory constructs a Client for one provider. Implementations live
// in providers/<name>/factory.go and register themselves with a
// Registry at process startup (typically via package init() that calls
// uos.DefaultRegistry().Register(...)).
type Factory interface {
	// Provider returns the canonical provider id this Factory handles.
	Provider() Provider
	// Validate inspects cfg and reports any structural problem (missing
	// region, type-mismatched DriverConfig, unsupported credential
	// scheme, etc.). It MUST NOT perform network I/O.
	Validate(cfg Config) error
	// Open performs network I/O if needed (credential probe, region
	// auto-discovery) and returns a usable Client. The returned Client
	// owns the supplied Config; callers MUST NOT mutate it afterwards.
	Open(ctx context.Context, cfg Config) (Client, error)
}

// Registry resolves a Config to a Client by looking up the named
// Provider's Factory. Implementations are typically a process-global
// singleton (see DefaultRegistry / NewRegistry).
type Registry interface {
	// Register adds a Factory to the Registry. Returns an error on
	// duplicate Provider id (register-once-per-provider rule).
	Register(f Factory) error
	// Open looks up cfg.Provider's Factory, invokes Validate, then Open.
	// Returns *Error{Code: ErrInvalidArgument} if no Factory is registered
	// for cfg.Provider.
	Open(ctx context.Context, cfg Config) (Client, error)
}
