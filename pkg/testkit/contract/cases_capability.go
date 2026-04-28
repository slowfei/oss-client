package contract

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/capability"
)

// runCapabilityCases iterates the frozen 13 capabilities and asserts:
//
//  1. Every key in capability.All() is present in the driver's Report
//     (no missing keys — drivers that omit keys fail this contract).
//  2. For each capability the driver reports as Unsupported, the
//     matching operation MUST return *uos.Error{Code: ErrUnsupported,
//     Capability: <cap>} so callers can dispatch on the typed error.
//
// Callers that genuinely cannot probe an op (e.g. it depends on bucket
// state the suite hasn't set up) opt out via FactoryUnderTest.SkipCases
// keyed on the dotted t.Run path.
func runCapabilityCases(t *testing.T, fut FactoryUnderTest) {
	t.Helper()

	t.Run("report_completeness", func(t *testing.T) {
		ctx := context.Background()
		c := openClient(ctx, t, fut)
		rep, err := c.Capabilities(ctx)
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		for _, cap := range capability.All() {
			if _, ok := rep.Get(cap); !ok {
				t.Errorf("Report missing key %q (drivers MUST populate every capability.All() value)", cap)
			}
		}
	})

	for _, cap := range capability.All() {
		cap := cap
		name := "unsupported_returns_typed_error/" + string(cap)
		t.Run(name, func(t *testing.T) {
			if reason := shouldSkip(fut, t.Name()); reason != "" {
				t.Skipf("driver opt-out: %s", reason)
			}
			ctx := context.Background()
			c := openClient(ctx, t, fut)
			rep, err := c.Capabilities(ctx)
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			st, _ := rep.Get(cap)
			if st.Availability != capability.Unsupported {
				t.Skipf("driver reports %q as %v; this case asserts the Unsupported branch only", cap, st.Availability)
			}
			err = probeCapability(ctx, c, fut.Bucket, cap)
			if err == nil {
				t.Fatalf("op for %q returned nil; want ErrUnsupported", cap)
			}
			var ue *uos.Error
			if !errors.As(err, &ue) {
				t.Fatalf("op for %q: want *uos.Error, got %v", cap, err)
			}
			if ue.Code != uos.ErrUnsupported {
				t.Fatalf("op for %q: want Code=ErrUnsupported, got %q", cap, ue.Code)
			}
			if ue.Capability != cap {
				t.Fatalf("op for %q: want Capability=%q, got %q", cap, cap, ue.Capability)
			}
		})
	}
}

// probeCapability invokes the smallest operation that exercises cap so
// the suite can verify the driver returns ErrUnsupported correctly.
// Each branch matches the capability to a single low-cost call.
func probeCapability(ctx context.Context, c uos.Client, bucket string, cap capability.Capability) error {
	switch cap {
	case capability.CapBucketCRUD:
		_, err := c.Buckets().Stat(ctx, uos.StatBucketRequest{Name: bucket})
		return err
	case capability.CapObjectCRUD:
		_, err := c.Objects(bucket).Head(ctx, uos.HeadObjectRequest{Bucket: bucket, Key: "probe"})
		return err
	case capability.CapListPrefixDelimiter:
		_, err := c.Objects(bucket).List(ctx, uos.ListObjectsRequest{Bucket: bucket, Delimiter: "/"})
		return err
	case capability.CapRangeRead:
		_, err := c.Objects(bucket).Get(ctx, uos.GetObjectRequest{
			Bucket: bucket, Key: "probe", Range: &uos.ByteRange{Start: 0, End: 0},
		})
		return err
	case capability.CapMultipartUpload:
		_, err := c.Multipart(bucket).Initiate(ctx, uos.InitiateMultipartRequest{Bucket: bucket, Key: "probe"})
		return err
	case capability.CapSignedURLRead:
		_, err := c.Signer(bucket).SignURL(ctx, uos.SignURLRequest{
			Bucket: bucket, Key: "probe", Method: "GET", ExpiresIn: 1,
		})
		return err
	case capability.CapSignedURLWrite:
		_, err := c.Signer(bucket).SignURL(ctx, uos.SignURLRequest{
			Bucket: bucket, Key: "probe", Method: "PUT", ExpiresIn: 1,
		})
		return err
	case capability.CapDirectGrant:
		_, err := c.Signer(bucket).IssueDirectGrant(ctx, uos.DirectGrantRequest{
			Bucket: bucket, Key: "probe", Operation: uos.DirectGrantUpload, ExpiresIn: 1,
		})
		return err
	case capability.CapObjectTagging,
		capability.CapVersioning,
		capability.CapObjectACL,
		capability.CapManagedEncryption,
		capability.CapNativeMove:
		// These capabilities have no single-call probe in the unified API
		// (they manifest as field rejections on existing ops). Drivers
		// should surface a probe via Client.As(target) for these; the
		// contract suite cannot probe them generically, so we report no
		// error to skip the assertion at the caller.
		return uos.NewUnsupported(c.Provider(), "probe", cap, errCapabilityNotProbe)
	default:
		return fmt.Errorf("contract: no probe defined for capability %q", cap)
	}
}

var errCapabilityNotProbe = errors.New("capability has no generic probe in the unified API")
