package credential

import (
	"context"
	"testing"
	"time"
)

func TestStaticProvider_ReturnsSameCredential(t *testing.T) {
	t.Parallel()
	exp := time.Now().Add(time.Hour)
	want := Credential{
		Scheme:    AuthHMAC,
		ExpiresAt: &exp,
		Opaque:    "opaque-payload",
	}
	p := NewStatic(want)
	got, err := p.Resolve(context.Background(), "any-target")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Scheme != want.Scheme {
		t.Errorf("Scheme = %q, want %q", got.Scheme, want.Scheme)
	}
	if got.Opaque != want.Opaque {
		t.Errorf("Opaque = %v, want %v", got.Opaque, want.Opaque)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, exp)
	}
}

func TestStaticProvider_TargetIgnored(t *testing.T) {
	t.Parallel()
	p := NewStatic(Credential{Scheme: AuthAnonymous})
	for _, target := range []string{"", "aws", "azure"} {
		got, err := p.Resolve(context.Background(), target)
		if err != nil {
			t.Fatalf("target=%q: %v", target, err)
		}
		if got.Scheme != AuthAnonymous {
			t.Errorf("target=%q: Scheme = %q, want anonymous", target, got.Scheme)
		}
	}
}

func TestStaticProvider_NilCredential(t *testing.T) {
	t.Parallel()
	// Zero value should resolve cleanly; the Provider does not validate the payload.
	p := &StaticProvider{}
	got, err := p.Resolve(context.Background(), "x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Scheme != "" {
		t.Errorf("expected zero Scheme, got %q", got.Scheme)
	}
}
