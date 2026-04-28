package credential

import "context"

// StaticProvider is a Provider that always returns the same Credential.
// It is the simplest possible Provider implementation; useful for tests,
// for callers that already have a long-lived key pair, and for embedding
// inside a Chain that ends with a static fallback.
//
// StaticProvider is safe for concurrent use; the embedded Credential is
// returned by value so callers cannot mutate the Provider's state.
type StaticProvider struct {
	// Credential is returned verbatim by every call to Resolve.
	Credential Credential
}

// NewStatic constructs a StaticProvider holding the supplied Credential.
// Provided as a constructor so callers can compose Chains with literal
// Credential values without naming the struct field.
func NewStatic(c Credential) *StaticProvider {
	return &StaticProvider{Credential: c}
}

// Resolve returns the fixed Credential. The target argument is ignored;
// a StaticProvider does not vary by provider id.
func (s *StaticProvider) Resolve(_ context.Context, _ string) (Credential, error) {
	return s.Credential, nil
}

// Compile-time check that StaticProvider implements Provider.
var _ Provider = (*StaticProvider)(nil)
