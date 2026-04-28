package credential

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// EnvProvider resolves a Credential from process environment variables.
// It is the starter Provider documented in v0.1; vendor-specific drivers
// (e.g. providers/azure for AZURE_*, providers/gcs for
// GOOGLE_APPLICATION_CREDENTIALS) are expected to ship their own
// EnvProvider variants.
//
// Lookup order on each Resolve call:
//
//  1. OSC_ACCESS_KEY_ID + OSC_SECRET_ACCESS_KEY (+ optional OSC_SESSION_TOKEN)
//  2. AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (+ optional AWS_SESSION_TOKEN)
//
// The AWS_* fallback is documented as ergonomic only; it is not a
// statement that this SDK implements AWS SigV4 — drivers do.
//
// EnvProvider is safe for concurrent use; os.Getenv reads are atomic.
type EnvProvider struct{}

// NewEnv constructs an EnvProvider. Provided for symmetry with
// NewStatic and NewChain so call sites read uniformly.
func NewEnv() *EnvProvider { return &EnvProvider{} }

// EnvHMACCredential is the concrete payload returned by EnvProvider's
// Resolve when an HMAC-style credential is found. Drivers expecting an
// HMAC scheme type-assert Credential.Opaque to *EnvHMACCredential.
//
// Field names are kept generic (AccessKeyID / SecretAccessKey) so the
// shape can be reused by any HMAC-family driver (AWS, Alibaba, Tencent,
// Huawei, Volcengine, Qiniu, MinIO).
type EnvHMACCredential struct {
	// AccessKeyID is the public identifier of the key pair.
	AccessKeyID string
	// SecretAccessKey is the secret half of the key pair. Drivers MUST
	// NOT log this value (see middleware.RedactHeaders).
	SecretAccessKey string
	// SessionToken is the optional temporary-credential token (set when
	// the credential came from an STS-style flow). Empty for long-lived keys.
	SessionToken string
}

// envCredentialNotFound is returned when no recognised env var pair is set.
// Wrapped in a typed sentinel so Chain.Resolve can distinguish "this
// Provider has no opinion" from "this Provider failed".
var envCredentialNotFound = errors.New("credential/env: no OSC_* or AWS_* credentials set")

// Resolve scans the environment in OSC_*-then-AWS_* order and returns
// the first complete pair as an AuthHMAC Credential. The target argument
// is ignored; EnvProvider does not vary by provider id (drivers that
// need vendor-specific env names ship their own provider).
func (e *EnvProvider) Resolve(_ context.Context, target string) (Credential, error) {
	for _, prefix := range []string{"OSC", "AWS"} {
		ak := os.Getenv(prefix + "_ACCESS_KEY_ID")
		sk := os.Getenv(prefix + "_SECRET_ACCESS_KEY")
		if ak == "" || sk == "" {
			continue
		}
		token := os.Getenv(prefix + "_SESSION_TOKEN")
		return Credential{
			Scheme: AuthHMAC,
			Opaque: &EnvHMACCredential{
				AccessKeyID:     ak,
				SecretAccessKey: sk,
				SessionToken:    token,
			},
		}, nil
	}
	return Credential{}, fmt.Errorf("%w (target=%q)", envCredentialNotFound, target)
}

// IsEnvCredentialNotFound reports whether err is the not-found sentinel
// returned by EnvProvider.Resolve when no recognised env vars are set.
// Chain uses this to fall through to the next Provider rather than
// short-circuiting on what is logically a "not configured" signal.
func IsEnvCredentialNotFound(err error) bool {
	return errors.Is(err, envCredentialNotFound)
}

// Compile-time check that EnvProvider implements Provider.
var _ Provider = (*EnvProvider)(nil)
