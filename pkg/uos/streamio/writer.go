package streamio

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/maqian/oss-client/pkg/uos"
)

// MinPartSize is the smallest non-final part size the helper accepts.
// 5 MiB matches the strictest vendor minimum in the v1 set (S3 + family);
// Azure permits 4 MiB but we standardise on 5 MiB for cross-provider
// safety. Final parts (the last one before Complete) may be any size.
const MinPartSize = 5 * 1024 * 1024

// WriterOptions parameterises a NewWriter call. The zero value is a
// valid choice: PartSize defaults to MinPartSize and the small-object
// threshold to PartSize.
type WriterOptions struct {
	// ContentType is the MIME type of the resulting object.
	ContentType string
	// Metadata is user-defined metadata; lower-cased keys.
	Metadata uos.Metadata
	// StorageClass is the vendor-defined storage class. Empty = vendor default.
	StorageClass string
	// ACL is the vendor-defined canned ACL. Empty = vendor default.
	ACL string
	// PartSize overrides the multipart-part size. 0 = MinPartSize.
	// Must be >= MinPartSize when non-zero.
	PartSize int
	// SmallObjectThreshold caps the total size below which Close uses a
	// single Put instead of multipart. 0 = PartSize. Set lower than
	// PartSize to force multipart even for small objects (e.g. for
	// testing the multipart path).
	SmallObjectThreshold int
}

// Writer streams writes into a single new object, automatically using
// multipart upload when the total size exceeds SmallObjectThreshold.
//
// Lifecycle:
//
//   - NewWriter constructs the writer; no I/O happens until the first
//     Write or Close.
//   - Each Write appends to an in-memory buffer. When the buffer
//     reaches PartSize, the writer initiates multipart (on first
//     overflow) and uploads a part, draining the buffer.
//   - Close finalises the upload. If multipart was never initiated AND
//     the buffer fits in SmallObjectThreshold, Close issues a single
//     Put (small-object fast path); otherwise it initiates multipart
//     (if needed), uploads any remaining bytes as the final part, and
//     calls Complete.
//   - Abort releases vendor-side multipart state without committing.
//     Safe to call multiple times or after Close.
//
// Concurrency: NOT safe for concurrent use. Wrap with a mutex if
// multiple goroutines write to the same Writer.
//
// Errors: a failed UploadPart sets a sticky error; subsequent Write
// calls return it. Close attempts an Abort to release vendor state and
// then returns the original error. The caller MAY also call Abort
// explicitly for clarity.
type Writer struct {
	ctx    context.Context
	cli    uos.Client
	bucket string
	key    string
	opts   WriterOptions

	buf      []byte
	upload   *uos.MultipartUpload
	parts    []uos.UploadedPart
	nextPart int

	closed  bool
	aborted bool
	err     error // sticky error from a failed Write
}

// Compile-time assertion that Writer satisfies io.WriteCloser.
var _ io.WriteCloser = (*Writer)(nil)

// NewWriter constructs a Writer that streams into the named (bucket, key).
// The object is created on Close; Abort releases vendor state without commit.
//
// Returns an error if cli is nil, bucket or key is empty, or PartSize is
// non-zero and below MinPartSize.
func NewWriter(ctx context.Context, cli uos.Client, bucket, key string, opts WriterOptions) (*Writer, error) {
	if cli == nil {
		return nil, fmt.Errorf("streamio.NewWriter: nil client")
	}
	if bucket == "" {
		return nil, fmt.Errorf("streamio.NewWriter: bucket is required")
	}
	if key == "" {
		return nil, fmt.Errorf("streamio.NewWriter: key is required")
	}
	if opts.PartSize == 0 {
		opts.PartSize = MinPartSize
	}
	if opts.PartSize < MinPartSize {
		return nil, fmt.Errorf("streamio.NewWriter: PartSize %d below cross-vendor safe minimum %d (5 MiB)", opts.PartSize, MinPartSize)
	}
	if opts.SmallObjectThreshold == 0 {
		opts.SmallObjectThreshold = opts.PartSize
	}
	return &Writer{
		ctx:      ctx,
		cli:      cli,
		bucket:   bucket,
		key:      key,
		opts:     opts,
		nextPart: 1,
	}, nil
}

// Write appends p to the buffer. When the buffer reaches PartSize, the
// writer initiates multipart (if not already) and uploads parts until
// the buffer drops below PartSize. Returns len(p) on success.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("streamio: write on closed writer")
	}
	if w.aborted {
		return 0, fmt.Errorf("streamio: write on aborted writer")
	}
	if w.err != nil {
		return 0, w.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	w.buf = append(w.buf, p...)
	for len(w.buf) >= w.opts.PartSize {
		if err := w.flushOnePart(); err != nil {
			w.err = err
			return 0, err
		}
	}
	return len(p), nil
}

// flushOnePart drains exactly opts.PartSize bytes as one part,
// initiating multipart on first call.
func (w *Writer) flushOnePart() error {
	if w.upload == nil {
		init, err := w.cli.Multipart(w.bucket).Initiate(w.ctx, uos.InitiateMultipartRequest{
			Bucket:       w.bucket,
			Key:          w.key,
			Content:      uos.ContentHeaders{ContentType: w.opts.ContentType},
			Metadata:     w.opts.Metadata,
			StorageClass: w.opts.StorageClass,
			ACL:          w.opts.ACL,
		})
		if err != nil {
			return fmt.Errorf("streamio: initiate: %w", err)
		}
		w.upload = init
	}
	chunk := w.buf[:w.opts.PartSize]
	w.buf = w.buf[w.opts.PartSize:]
	part, err := w.cli.Multipart(w.bucket).UploadPart(w.ctx, uos.UploadPartRequest{
		Bucket:     w.bucket,
		Key:        w.key,
		UploadID:   w.upload.UploadID,
		PartNumber: w.nextPart,
		Body:       bytes.NewReader(chunk),
		Size:       int64(len(chunk)),
	})
	if err != nil {
		return fmt.Errorf("streamio: upload part %d: %w", w.nextPart, err)
	}
	w.parts = append(w.parts, *part)
	w.nextPart++
	return nil
}

// Close finalises the upload. Either commits via Complete (multipart
// path) or via single Put (small-object fast path). Idempotent: a
// second Close call returns nil. Aborted writers are no-ops.
//
// On a sticky write error, Close attempts to release vendor state via
// Abort and returns the original error.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.aborted {
		return nil
	}
	if w.err != nil {
		if w.upload != nil {
			_ = w.cli.Multipart(w.bucket).Abort(context.Background(), uos.AbortMultipartRequest{
				Bucket:   w.bucket,
				Key:      w.key,
				UploadID: w.upload.UploadID,
			})
		}
		return w.err
	}

	// Small-object fast path: never started multipart AND buffer fits.
	if w.upload == nil && len(w.buf) <= w.opts.SmallObjectThreshold {
		_, err := w.cli.Objects(w.bucket).Put(w.ctx, uos.PutObjectRequest{
			Bucket:       w.bucket,
			Key:          w.key,
			Body:         bytes.NewReader(w.buf),
			Size:         int64(len(w.buf)),
			Content:      uos.ContentHeaders{ContentType: w.opts.ContentType},
			Metadata:     w.opts.Metadata,
			StorageClass: w.opts.StorageClass,
			ACL:          w.opts.ACL,
		})
		if err != nil {
			return fmt.Errorf("streamio: put: %w", err)
		}
		return nil
	}

	// Multipart path: initiate if SmallObjectThreshold pushed us here
	// without a prior part, then flush remaining buffer as final part.
	if w.upload == nil {
		init, err := w.cli.Multipart(w.bucket).Initiate(w.ctx, uos.InitiateMultipartRequest{
			Bucket:       w.bucket,
			Key:          w.key,
			Content:      uos.ContentHeaders{ContentType: w.opts.ContentType},
			Metadata:     w.opts.Metadata,
			StorageClass: w.opts.StorageClass,
			ACL:          w.opts.ACL,
		})
		if err != nil {
			return fmt.Errorf("streamio: initiate (close): %w", err)
		}
		w.upload = init
	}
	if len(w.buf) > 0 {
		part, err := w.cli.Multipart(w.bucket).UploadPart(w.ctx, uos.UploadPartRequest{
			Bucket:     w.bucket,
			Key:        w.key,
			UploadID:   w.upload.UploadID,
			PartNumber: w.nextPart,
			Body:       bytes.NewReader(w.buf),
			Size:       int64(len(w.buf)),
		})
		if err != nil {
			_ = w.cli.Multipart(w.bucket).Abort(context.Background(), uos.AbortMultipartRequest{
				Bucket:   w.bucket,
				Key:      w.key,
				UploadID: w.upload.UploadID,
			})
			return fmt.Errorf("streamio: final part upload: %w", err)
		}
		w.parts = append(w.parts, *part)
		w.buf = w.buf[:0]
	}
	if _, err := w.cli.Multipart(w.bucket).Complete(w.ctx, uos.CompleteMultipartRequest{
		Bucket:   w.bucket,
		Key:      w.key,
		UploadID: w.upload.UploadID,
		Parts:    w.parts,
	}); err != nil {
		return fmt.Errorf("streamio: complete: %w", err)
	}
	return nil
}

// Abort releases the vendor-side multipart state without committing.
// Safe to call multiple times, before Close, or after Close. Idempotent.
//
// Returns nil if multipart was never initiated (e.g. small object that
// would have gone via single Put). If multipart was initiated, calls
// MultipartService.Abort and returns its error wrapped.
func (w *Writer) Abort() error {
	if w.aborted {
		return nil
	}
	w.aborted = true
	if w.upload == nil {
		return nil
	}
	if err := w.cli.Multipart(w.bucket).Abort(context.Background(), uos.AbortMultipartRequest{
		Bucket:   w.bucket,
		Key:      w.key,
		UploadID: w.upload.UploadID,
	}); err != nil {
		return fmt.Errorf("streamio: abort: %w", err)
	}
	return nil
}
