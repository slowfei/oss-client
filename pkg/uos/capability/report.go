package capability

import (
	"errors"
	"fmt"
)

// Availability expresses how a capability is exposed by a (driver, config,
// credential) triple. See architecture_plan §7.2 for semantics.
type Availability uint8

const (
	// Unsupported means the provider does not expose this capability;
	// operations that require it MUST return *uos.Error{Code: ErrUnsupported}.
	Unsupported Availability = iota
	// Conditional means support depends on runtime config / credential; a
	// probe is required before relying on the capability.
	Conditional
	// Supported means the capability works as documented for this configuration.
	Supported
	// ExtensionOnly means the underlying provider can do it, but pkg/uos
	// does not abstract it; callers must reach for Client.As(target).
	ExtensionOnly
)

// CapabilityStatus pairs an Availability with a human-readable Reason for
// diagnostics. Reason MAY be empty when the Availability is self-explanatory.
type CapabilityStatus struct {
	// Availability is the level of support exposed by the driver.
	Availability Availability
	// Reason is a human-readable explanation, e.g. "requires bucket-level versioning enabled".
	Reason string
}

// Report is the per-(driver, config, credential) capability declaration.
// Drivers MUST populate Items with every capability returned by All();
// missing keys are treated as Unsupported with Reason="not implemented".
type Report struct {
	// Items maps every frozen capability to its current status.
	Items map[Capability]CapabilityStatus
}

// Get returns the status for c. If the entry is missing, the returned
// boolean is false and the status defaults to Unsupported. Callers that
// only need a yes/no answer should prefer Has.
func (r Report) Get(c Capability) (CapabilityStatus, bool) {
	if r.Items == nil {
		return CapabilityStatus{Availability: Unsupported, Reason: "no report"}, false
	}
	st, ok := r.Items[c]
	return st, ok
}

// Has returns true iff the capability is Supported. Conditional and
// ExtensionOnly both return false because the unified API cannot promise
// the call will succeed without a runtime probe (Conditional) or at all
// (ExtensionOnly — caller must use As(target)).
func (r Report) Has(c Capability) bool {
	st, ok := r.Get(c)
	if !ok {
		return false
	}
	return st.Availability == Supported
}

// ErrCapabilityUnsupported is the sentinel returned by Require when a
// capability is missing. Callers in pkg/uos wrap it into a rich
// *uos.Error{Code: ErrUnsupported, Capability: c}; callers outside
// pkg/uos can match it via errors.Is.
//
// This indirection avoids the import cycle that would arise if
// capability imported pkg/uos to construct *uos.Error directly. See
// pkg/uos.NewUnsupported for the rich wrapper.
var ErrCapabilityUnsupported = errors.New("capability unsupported")

// missingCapabilityError tags an ErrCapabilityUnsupported with the
// specific capability that was missing, so callers (and the wrapper in
// pkg/uos) can recover it without parsing strings.
type missingCapabilityError struct {
	cap Capability
}

func (e *missingCapabilityError) Error() string {
	return fmt.Sprintf("capability %q unsupported", string(e.cap))
}

func (e *missingCapabilityError) Unwrap() error { return ErrCapabilityUnsupported }

// Capability returns the missing capability tagged on the error.
func (e *missingCapabilityError) Capability() Capability { return e.cap }

// Require returns nil if c is Supported and a non-nil error otherwise.
// The returned error wraps ErrCapabilityUnsupported and exposes the
// missing capability via a Capability() method, so the rich-error
// wrapper in pkg/uos can recover it without parsing strings.
func (r Report) Require(c Capability) error {
	if r.Has(c) {
		return nil
	}
	return &missingCapabilityError{cap: c}
}

// MissingCapability extracts the capability tagged on an error returned
// by Report.Require. Returns ("", false) if err is not such an error.
// Used by the rich-error wrapper in pkg/uos to populate Error.Capability.
func MissingCapability(err error) (Capability, bool) {
	var mc *missingCapabilityError
	if errors.As(err, &mc) {
		return mc.cap, true
	}
	return "", false
}
