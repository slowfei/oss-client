package credential

import (
	"context"
	"errors"
	"fmt"
)

// Chain composes multiple Providers and returns the first successful
// Credential. Providers are tried in registration order; a non-nil
// error from one Provider is recorded and the next is tried. If every
// Provider fails, Chain.Resolve returns a single error wrapping the
// final failure (with errors.Join collecting all underlying errors so
// callers can inspect via errors.Unwrap traversal).
//
// Chain is safe for concurrent use as long as its component Providers are.
type Chain struct {
	// Providers are tried in slice order. Nil entries are skipped.
	Providers []Provider
}

// NewChain constructs a Chain from the supplied Providers. Order is
// preserved; nil entries are dropped at construction time so the
// hot-path in Resolve does not need a nil-check per element.
func NewChain(providers ...Provider) *Chain {
	filtered := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		filtered = append(filtered, p)
	}
	return &Chain{Providers: filtered}
}

// ErrChainEmpty is returned by Resolve when the Chain has no Providers
// (or every supplied Provider was nil at construction time).
var ErrChainEmpty = errors.New("credential/chain: no providers configured")

// Resolve walks Providers in order and returns the first non-error
// result. If every Provider errors, the returned error is the joined
// set of all errors (in walk order). The target argument is forwarded
// verbatim to each child Provider.
func (c *Chain) Resolve(ctx context.Context, target string) (Credential, error) {
	if len(c.Providers) == 0 {
		return Credential{}, ErrChainEmpty
	}
	errs := make([]error, 0, len(c.Providers))
	for i, p := range c.Providers {
		cred, err := p.Resolve(ctx, target)
		if err == nil {
			return cred, nil
		}
		errs = append(errs, fmt.Errorf("provider[%d]: %w", i, err))
	}
	return Credential{}, errors.Join(errs...)
}

// Compile-time check that *Chain implements Provider.
var _ Provider = (*Chain)(nil)
