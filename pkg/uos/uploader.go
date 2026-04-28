package uos

import "context"

// Uploader is the structural interface for single-object upload.
//
// A type satisfies Uploader by exposing a Put method matching the
// signature below. ObjectService satisfies Uploader implicitly: its
// Put method is the canonical upload entry point. Use Uploader when a
// caller only needs upload semantics and should not be coupled to the
// full ObjectService surface (Get / Head / Delete / List / etc.).
//
// # Implementation strategy per driver
//
// Drivers implement Uploader by their ObjectService.Put method. There
// are two valid execution shapes:
//
//   - Native multipart (S3-family pattern). The vendor SDK encodes
//     size-based dispatch + parallel parts + abort-on-failure inside
//     its own client.PutObject (or equivalent). The driver's Put
//     simply translates the unified PutObjectRequest into a vendor
//     call and lets the SDK orchestrate. providers/aws and
//     providers/minio both follow this pattern in v0.1.0.
//
//   - Manager-backed orchestration (non-S3 pattern). Drivers whose
//     vendor SDK lacks a unified multipart helper — notably GCS
//     resumable uploads (M4) and Upyun FORM (M5) — wrap
//     pkg/uos/transfer.Manager in their own provider package to
//     satisfy Uploader. The fallback wrapper is NOT provided by
//     pkg/uos because transfer.Manager uses local adapter types
//     (UploadRequest, MultipartServiceLike) to avoid an import cycle
//     with pkg/uos; see pkg/uos/transfer/manager.go file doc for the
//     cycle-avoidance rationale.
//
// # Compatibility
//
// Adding Uploader is additive in v0.1.0: every existing ObjectService
// implementation already satisfies it (the interface is a strict
// subset of ObjectService). No driver change is required.
//
// # Frozen-set status
//
// Uploader is NOT one of the three frozen sets pinned by
// TestFrozenSurface (Codes / Capabilities / DirectGrantModes); it is
// a structural interface, not a wire-level value enum. Adding a
// method to Uploader is a major-version-breaking change for downstream
// consumers (any external Uploader implementation breaks); removing
// the interface entirely is also major-breaking. Adding a wholly new
// interface is additive (minor bump).
type Uploader interface {
	Put(ctx context.Context, req PutObjectRequest) (*PutObjectResult, error)
}

// Downloader is the structural interface for single-object download.
//
// A type satisfies Downloader by exposing a Get method matching the
// signature below. ObjectService satisfies Downloader implicitly via
// its Get method. Use Downloader for callers that only need read
// semantics and should not be coupled to the full ObjectService
// surface.
//
// # Frozen-set status
//
// Same as Uploader: Downloader is a structural interface, not a wire
// enum. See Uploader for the additive / major-breaking semantics.
type Downloader interface {
	Get(ctx context.Context, req GetObjectRequest) (*ObjectReader, error)
}

// Compile-time assertions: ObjectService satisfies both Uploader and
// Downloader. If a future change drops Put or Get from ObjectService
// (or changes the signature), the build fails here — making the break
// loud and reviewable rather than silent.
var (
	_ Uploader   = (ObjectService)(nil)
	_ Downloader = (ObjectService)(nil)
)
