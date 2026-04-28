// Package transfer holds the orchestration layer that turns provider
// MultipartService primitives into a complete upload (and, in v1.x,
// download) experience: planner, worker pool, abort-on-failure cleanup,
// resume hooks, and progress / rate-limit callbacks.
//
// This package is a leaf in the dependency graph: it MUST NOT import
// pkg/uos. The bridging types Manager.Upload consumes (UploadRequest,
// MultipartServiceLike) are local-to-transfer adapters declared in
// manager.go to avoid an import cycle. pkg/uos.Client wraps them to
// expose the public Upload entrypoint. See architecture_plan §3.2.
package transfer

import (
	"context"
	"time"
)

// UnknownSizePolicy controls how Manager handles an UploadRequest whose
// Body is an io.Reader without a known Size. See architecture_plan §4.9
// for the full state-machine description.
type UnknownSizePolicy uint8

const (
	// UnknownSizeReject is the default. Manager fails fast with a
	// length-required error when Size is unknown.
	UnknownSizeReject UnknownSizePolicy = iota
	// UnknownSizeBuffer asks Manager to read up to BufferLimit bytes
	// into memory to learn the size. If the reader exceeds BufferLimit,
	// Manager falls back to UnknownSizeTempFile (or rejects when the
	// fallback is also unavailable).
	UnknownSizeBuffer
	// UnknownSizeTempFile asks Manager to spool the body to a temporary
	// file under TempDir for length detection prior to upload. The temp
	// file is removed once the upload completes (success or failure).
	UnknownSizeTempFile
)

// ProgressCallback is invoked during an upload or download with the
// cumulative number of bytes transferred and the total size in bytes.
// total is -1 when the size is unknown at the start of the transfer.
// Callbacks MUST be quick (offload heavy work to a goroutine) and MUST
// be safe to call from multiple goroutines concurrently.
type ProgressCallback func(transferred, total int64)

// RateLimiter throttles bytes-per-second across all in-flight transfers
// orchestrated by a single Manager. Implementations MUST be safe for
// concurrent use. Wait blocks until n bytes of allowance are available
// or ctx is cancelled.
type RateLimiter interface {
	// Wait blocks until n bytes of bandwidth are available. Returns
	// ctx.Err() if the context is cancelled before allowance is granted.
	Wait(ctx context.Context, n int) error
}

// Config bundles every Manager knob. The zero value is usable: it
// produces a Manager that rejects unknown-size uploads, picks a 16 MiB
// multipart threshold, an 8 MiB part size, and 4-way concurrency. All
// fields are read at NewManager time and never mutated by the Manager.
type Config struct {
	// MultipartThreshold is the size in bytes at or above which Manager
	// switches from single-shot to multipart upload. Zero selects the
	// default (16 MiB).
	MultipartThreshold int64

	// PartSize is the part size in bytes used for multipart uploads.
	// Zero selects the default (8 MiB). Drivers MAY clamp this to the
	// vendor minimum (typically 5 MiB for S3-compatible providers).
	PartSize int64

	// MaxConcurrency is the maximum number of part uploads Manager will
	// run in parallel for a single multipart upload. Zero or negative
	// selects the default (4).
	MaxConcurrency int

	// UnknownSizePolicy selects the strategy for io.Reader bodies whose
	// Size is unknown. Zero value is UnknownSizeReject.
	UnknownSizePolicy UnknownSizePolicy

	// BufferLimit caps the in-memory buffer used by UnknownSizeBuffer.
	// Zero selects the default (8 MiB). Ignored unless UnknownSizePolicy
	// is UnknownSizeBuffer.
	BufferLimit int64

	// TempDir is the directory used by UnknownSizeTempFile for spooling.
	// Empty selects the OS default temp directory.
	TempDir string

	// StateStore persists opaque resume payloads. Optional; required
	// only when callers want to resume an interrupted multipart upload
	// across process restarts.
	StateStore StateStore

	// ResumeKey computes the StateStore key for a given UploadRequest.
	// When nil, Manager uses bucket+"/"+key as the resume key. Callers
	// MAY provide a richer scheme (e.g. content-addressed) to dedupe
	// retries across logical operations.
	ResumeKey func(bucket, key string) string

	// Progress, when non-nil, receives cumulative-bytes / total-bytes
	// updates during transfers. See ProgressCallback for concurrency
	// expectations.
	Progress ProgressCallback

	// RateLimiter, when non-nil, throttles bytes-per-second across all
	// in-flight transfers managed by this Manager.
	RateLimiter RateLimiter

	// PartUploadTimeout caps per-part upload duration. Zero disables the
	// cap; rely on ctx for cancellation in that case.
	PartUploadTimeout time.Duration
}

// Default values applied when a Config field is left at its zero value.
const (
	DefaultMultipartThreshold int64 = 16 * 1024 * 1024 // 16 MiB
	DefaultPartSize           int64 = 8 * 1024 * 1024  // 8 MiB
	DefaultMaxConcurrency     int   = 4
	DefaultBufferLimit        int64 = 8 * 1024 * 1024 // 8 MiB
)

// withDefaults returns a copy of cfg with zero-valued fields replaced by
// their package defaults. Kept unexported because callers should only
// see the original Config they passed in.
func (cfg Config) withDefaults() Config {
	out := cfg
	if out.MultipartThreshold <= 0 {
		out.MultipartThreshold = DefaultMultipartThreshold
	}
	if out.PartSize <= 0 {
		out.PartSize = DefaultPartSize
	}
	if out.MaxConcurrency <= 0 {
		out.MaxConcurrency = DefaultMaxConcurrency
	}
	if out.BufferLimit <= 0 {
		out.BufferLimit = DefaultBufferLimit
	}
	return out
}
