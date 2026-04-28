package credential

import (
	"context"
	"errors"
	"testing"
)

// envSet sets the four pairs of env vars (OSC_/AWS_) for a test and
// returns a cleanup func. t.Setenv handles teardown automatically;
// this helper just centralises the var names so a typo only happens once.
func envSet(t *testing.T, prefix, ak, sk, token string) {
	t.Helper()
	t.Setenv(prefix+"_ACCESS_KEY_ID", ak)
	t.Setenv(prefix+"_SECRET_ACCESS_KEY", sk)
	t.Setenv(prefix+"_SESSION_TOKEN", token)
}

// envClear unsets a prefix's three env vars by setting them to empty strings.
func envClear(t *testing.T, prefix string) {
	t.Helper()
	t.Setenv(prefix+"_ACCESS_KEY_ID", "")
	t.Setenv(prefix+"_SECRET_ACCESS_KEY", "")
	t.Setenv(prefix+"_SESSION_TOKEN", "")
}

func TestEnvProvider_OSCPrefixWins(t *testing.T) {
	envSet(t, "OSC", "osc-ak", "osc-sk", "osc-token")
	envSet(t, "AWS", "aws-ak", "aws-sk", "aws-token")
	got, err := NewEnv().Resolve(context.Background(), "aws")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Scheme != AuthHMAC {
		t.Fatalf("Scheme = %q, want hmac", got.Scheme)
	}
	hc, ok := got.Opaque.(*EnvHMACCredential)
	if !ok {
		t.Fatalf("Opaque = %T, want *EnvHMACCredential", got.Opaque)
	}
	if hc.AccessKeyID != "osc-ak" || hc.SecretAccessKey != "osc-sk" || hc.SessionToken != "osc-token" {
		t.Errorf("OSC values not preferred: %+v", hc)
	}
}

func TestEnvProvider_AWSFallback(t *testing.T) {
	envClear(t, "OSC")
	envSet(t, "AWS", "aws-ak", "aws-sk", "")
	got, err := NewEnv().Resolve(context.Background(), "aws")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	hc := got.Opaque.(*EnvHMACCredential)
	if hc.AccessKeyID != "aws-ak" || hc.SecretAccessKey != "aws-sk" {
		t.Errorf("AWS fallback not honored: %+v", hc)
	}
	if hc.SessionToken != "" {
		t.Errorf("expected empty SessionToken, got %q", hc.SessionToken)
	}
}

func TestEnvProvider_NoCredentials(t *testing.T) {
	envClear(t, "OSC")
	envClear(t, "AWS")
	_, err := NewEnv().Resolve(context.Background(), "aws")
	if err == nil {
		t.Fatal("expected error when no env vars set")
	}
	if !IsEnvCredentialNotFound(err) {
		t.Fatalf("expected IsEnvCredentialNotFound to recognise the error, got %v", err)
	}
}

func TestEnvProvider_PartialOSCFallsThrough(t *testing.T) {
	// Only access key set on OSC — should fall through to AWS.
	t.Setenv("OSC_ACCESS_KEY_ID", "osc-ak")
	t.Setenv("OSC_SECRET_ACCESS_KEY", "")
	t.Setenv("OSC_SESSION_TOKEN", "")
	envSet(t, "AWS", "aws-ak", "aws-sk", "")
	got, err := NewEnv().Resolve(context.Background(), "x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	hc := got.Opaque.(*EnvHMACCredential)
	if hc.AccessKeyID != "aws-ak" {
		t.Errorf("expected AWS fallback when OSC pair is incomplete, got %q", hc.AccessKeyID)
	}
}

func TestIsEnvCredentialNotFound_OnUnrelatedError(t *testing.T) {
	t.Parallel()
	if IsEnvCredentialNotFound(errors.New("other")) {
		t.Fatal("IsEnvCredentialNotFound matched an unrelated error")
	}
	if IsEnvCredentialNotFound(nil) {
		t.Fatal("IsEnvCredentialNotFound matched nil")
	}
}
