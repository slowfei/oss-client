// Package middleware defines the v1 contracts for cross-cutting concerns
// that every driver should honor: logging, metrics, and tracing.
//
// Drivers MUST NOT log full Signed URLs, Authorization headers, SAS
// tokens, Upload Tokens, or any other credential material per
// architecture_plan §2.3. Use RedactHeaders / RedactQuery before passing
// any URL or header map to a Logger.
//
// This package is a leaf in the dependency graph: it MUST NOT import
// pkg/uos (avoids an import cycle with Config).
package middleware

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Op is the high-level operation tag used in logging / metrics / tracing.
// Drivers should use stable string values such as "PutObject", "ListBuckets"
// so dashboards and alerts can pivot on them. Op is plain string so callers
// don't need to import a constants enum.
type Op string

// Event is the unified shape passed to Logger / Metrics / Tracer hooks.
// Fields not relevant to a particular event MAY be left zero.
type Event struct {
	// Provider is the provider id (e.g. "aws", "azure").
	Provider string
	// Op is the high-level operation name.
	Op Op
	// Bucket is the bucket the operation targeted, when applicable.
	Bucket string
	// KeyHash is a non-reversible digest of the object key, NEVER the raw
	// key. Drivers compute this so logging volumes can pivot per-key
	// without retaining PII / sensitive paths.
	KeyHash string
	// Attempt is the 1-based retry attempt counter.
	Attempt int
	// Latency is the end-to-end latency for this attempt.
	Latency time.Duration
	// HTTPStatus is the upstream HTTP status, when one exists.
	HTTPStatus int
	// Code is the resolved pkg/uos.Code (empty on success). Stored as
	// string so this package stays free of pkg/uos imports.
	Code string
	// RequestID is the upstream request id, when one exists.
	RequestID string
	// Err is the operation error, when one exists.
	Err error
}

// Logger receives one Event per operation attempt. Implementations MUST
// be safe for concurrent use and MUST NOT block — drivers will call Log
// on every retry. The default is a no-op (see NoopLogger).
type Logger interface {
	Log(ctx context.Context, ev Event)
}

// Metrics receives the same Event shape as Logger and is intended for
// counters / histograms (e.g. per-Op latency, per-Code error count).
// Implementations MUST be safe for concurrent use and MUST NOT block.
type Metrics interface {
	Observe(ctx context.Context, ev Event)
}

// Tracer starts a span around an Op and returns a finish callback.
// Drivers wrap a single attempt in Start/finish; the returned context
// carries the span so nested calls can extend it.
type Tracer interface {
	Start(ctx context.Context, op Op) (context.Context, func(Event))
}

// Chain composes Logger / Metrics / Tracer. Nil entries are no-ops.
// Drivers always call the Chain (never the individual fields directly)
// so that adding a new hook later does not require touching every driver.
type Chain struct {
	// Logger receives Log events; nil → no-op.
	Logger Logger
	// Metrics receives Observe events; nil → no-op.
	Metrics Metrics
	// Tracer wraps spans; nil → no-op identity span.
	Tracer Tracer
}

// NoopChain returns a Chain whose hooks are all nil — every method on
// the chain is then a no-op. This is the default Config.Middleware
// when callers don't supply one.
func NoopChain() Chain { return Chain{} }

// Log dispatches to Chain.Logger when set.
func (c Chain) Log(ctx context.Context, ev Event) {
	if c.Logger != nil {
		c.Logger.Log(ctx, ev)
	}
}

// Observe dispatches to Chain.Metrics when set.
func (c Chain) Observe(ctx context.Context, ev Event) {
	if c.Metrics != nil {
		c.Metrics.Observe(ctx, ev)
	}
}

// Start dispatches to Chain.Tracer when set; otherwise returns the
// supplied context and a no-op finish func so callers don't need a
// nil-check.
func (c Chain) Start(ctx context.Context, op Op) (context.Context, func(Event)) {
	if c.Tracer != nil {
		return c.Tracer.Start(ctx, op)
	}
	return ctx, func(Event) {}
}

// NoopLogger is the default Logger that drops every event. Useful as a
// stand-in when a Logger is required by signature but the caller has
// nothing to attach.
type NoopLogger struct{}

// Log discards the event.
func (NoopLogger) Log(context.Context, Event) {}

// NoopMetrics is the default Metrics that drops every observation.
type NoopMetrics struct{}

// Observe discards the event.
func (NoopMetrics) Observe(context.Context, Event) {}

// NoopTracer is the default Tracer that returns the supplied context
// unchanged and a no-op finish func.
type NoopTracer struct{}

// Start returns ctx unchanged and a finish callback that drops its event.
func (NoopTracer) Start(ctx context.Context, _ Op) (context.Context, func(Event)) {
	return ctx, func(Event) {}
}

// Compile-time assertions: the no-op types satisfy the interfaces.
var (
	_ Logger  = NoopLogger{}
	_ Metrics = NoopMetrics{}
	_ Tracer  = NoopTracer{}
)

// sensitiveHeaders is the redaction list applied by RedactHeaders. It is
// a closed set; extending it is a minor version bump on this package
// because changes are visible in user-facing log output.
var sensitiveHeaders = map[string]struct{}{
	"authorization":        {},
	"proxy-authorization":  {},
	"x-amz-security-token": {},
	"x-amz-signature":      {},
	"x-goog-signature":     {},
	"x-ms-signature":       {},
	"x-ms-copy-source":     {},
	"x-upload-token":       {},
	"upload-token":         {},
	"cookie":               {},
	"set-cookie":           {},
}

// sensitiveQueryParams is the redaction list applied by RedactURL /
// RedactQuery. Same closed-set rule as sensitiveHeaders.
var sensitiveQueryParams = map[string]struct{}{
	"x-amz-signature":      {},
	"x-amz-credential":     {},
	"x-amz-security-token": {},
	"x-goog-signature":     {},
	"signature":            {},
	"sig":                  {},
	"sv":                   {},
	"se":                   {},
	"sp":                   {},
	"sas_token":            {},
	"upload-token":         {},
	"token":                {},
}

// redactedValue is the placeholder substituted for every sensitive
// header / query value. Plain ASCII so it greps unambiguously.
const redactedValue = "REDACTED"

// RedactHeaders returns a copy of h with sensitive header values
// replaced by the literal string "REDACTED". The input map is not
// modified. The match is case-insensitive (HTTP headers are
// case-insensitive on the wire).
func RedactHeaders(h map[string][]string) map[string][]string {
	if h == nil {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		if _, sensitive := sensitiveHeaders[strings.ToLower(k)]; sensitive {
			redacted := make([]string, len(vs))
			for i := range vs {
				redacted[i] = redactedValue
			}
			out[k] = redacted
			continue
		}
		// Defensive copy so callers can't mutate the input slice via the output.
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

// RedactURL returns a copy of u with sensitive query parameters
// replaced by "REDACTED". The user-info portion is also stripped (it
// commonly carries credentials in cloud-vendor-emitted URLs). The input
// URL is not modified.
func RedactURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	cp := *u
	if cp.User != nil {
		cp.User = url.User(redactedValue)
	}
	cp.RawQuery = RedactQuery(cp.Query()).Encode()
	return &cp
}

// RedactQuery returns a copy of v with sensitive query parameters
// replaced by "REDACTED". The match is case-insensitive (query
// parameters in cloud-vendor signed URLs are commonly mixed-case).
func RedactQuery(v url.Values) url.Values {
	if v == nil {
		return nil
	}
	out := make(url.Values, len(v))
	for k, vs := range v {
		if _, sensitive := sensitiveQueryParams[strings.ToLower(k)]; sensitive {
			redacted := make([]string, len(vs))
			for i := range vs {
				redacted[i] = redactedValue
			}
			out[k] = redacted
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

// Compile-time guarantee that http.Header (which is map[string][]string)
// stays compatible with RedactHeaders' input type. If the stdlib ever
// changes this type, the build fails here rather than silently at the
// caller.
var _ map[string][]string = http.Header{}
