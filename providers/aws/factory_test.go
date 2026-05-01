package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestStaticEndpointResolverVirtualHostStyle(t *testing.T) {
	resolver := &staticEndpointResolver{endpoint: "https://s3.compat.example.com"}

	ep, err := resolver.ResolveEndpoint(context.Background(), s3.EndpointParameters{
		Bucket:         stringPtr("example-bucket"),
		ForcePathStyle: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("ResolveEndpoint: %v", err)
	}

	if got, want := ep.URI.String(), "https://example-bucket.s3.compat.example.com"; got != want {
		t.Fatalf("endpoint: want %q got %q", want, got)
	}
}

func TestStaticEndpointResolverPathStyle(t *testing.T) {
	resolver := &staticEndpointResolver{endpoint: "http://localhost:9000/base"}

	ep, err := resolver.ResolveEndpoint(context.Background(), s3.EndpointParameters{
		Bucket:         stringPtr("example-bucket"),
		ForcePathStyle: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("ResolveEndpoint: %v", err)
	}

	if got, want := ep.URI.String(), "http://localhost:9000/base/example-bucket"; got != want {
		t.Fatalf("endpoint: want %q got %q", want, got)
	}
}

func TestStaticEndpointResolverBareHostUsesScheme(t *testing.T) {
	resolver := &staticEndpointResolver{
		endpoint:     "localhost:9000",
		disableHTTPS: true,
	}

	ep, err := resolver.ResolveEndpoint(context.Background(), s3.EndpointParameters{
		Bucket:         stringPtr("example-bucket"),
		ForcePathStyle: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("ResolveEndpoint: %v", err)
	}

	if got, want := ep.URI.String(), "http://localhost:9000/example-bucket"; got != want {
		t.Fatalf("endpoint: want %q got %q", want, got)
	}
}

func stringPtr(v string) *string { return &v }

func boolPtr(v bool) *bool { return &v }
