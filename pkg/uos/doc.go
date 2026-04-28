// Package uos is the public API of the universal object storage client SDK.
// It defines the unified Client interface and four sub-services
// (BucketService, ObjectService, MultipartService, Signer) that every
// provider driver implements, plus the value types, error model, and
// capability vocabulary that the unified surface depends on.
//
// uos itself ships zero provider code: drivers live in sibling modules
// under providers/<name>/. Callers obtain a Client by registering a
// provider Factory (typically via the driver's package init) and then
// calling registry.Open(ctx, cfg).
//
// # Stability
//
// The v1 surface is intentionally narrow and frozen at three points:
//
//   - 14 Code constants (see error.go and AllCodes()) — the error
//     vocabulary callers may match on with errors.Is.
//   - 13 Capability constants (see capability.All()) — the feature
//     vocabulary drivers populate in Client.Capabilities().
//   - 4 DirectGrantMode constants (see request.go) — the dispatch
//     shapes a Signer's DirectGrant return may take.
//
// Adding a value to any of these three sets requires (a) at least two
// providers needing the same semantic and (b) a minor version bump on
// pkg/uos. The freezing rule is enforced by surface_test.go's
// TestFrozenSurface, which fails on any literal-value drift. See the
// per-module release protocol in RELEASING.md and the binding
// rationale in docs/architecture_plan.md §7 for the full rules.
//
// # Layout
//
// pkg/uos is the root of a small package family, each piece self-contained:
//
//   - capability — frozen Capability vocabulary + Report (Availability enum + helpers).
//   - credential — Provider interface plus StaticProvider, EnvProvider, Chain.
//   - transfer   — Manager skeleton (planner / worker pool / abort-on-failure / resume).
//   - middleware — Logger / Metrics / Tracer contracts and the redaction list.
//   - httpx      — HTTPConfig + NewClient honoring TLS / proxy / idle-conn settings.
//   - testkit/contract — RunSuite(t, FactoryUnderTest) and the v0.1 contract case files
//     (build-tagged docker MinIO helper for live verification).
//
// # Quickstart
//
// A runnable example ships in M6; the v0.1 outline is:
//
//	import (
//	    "context"
//	    "github.com/maqian/object-storage-client/pkg/uos"
//	    _ "github.com/maqian/object-storage-client/providers/aws" // registers Factory
//	)
//
//	cfg := uos.Config{Provider: "aws", /* region, endpoint, credentials, ... */}
//	cli, err := uos.DefaultRegistry().Open(context.Background(), cfg)
//	if err != nil { /* handle */ }
//	defer cli.Close()
//
// No provider drivers exist yet at v0.1; the import above is the
// expected M2+ shape and is documented here for forward reference only.
package uos
