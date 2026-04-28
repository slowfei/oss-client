// File manager.go declares the local-adapter-types pattern that lets
// pkg/uos/transfer drive multipart uploads without importing pkg/uos.
// UploadRequest, UploadResult, MultipartServiceLike, and the small part
// description structs (PartSpec, UploadedPart) mirror the subset of the
// pkg/uos request/response surface that orchestration needs; pkg/uos.
// Client wraps them at call sites to avoid the import cycle that would
// otherwise arise from the subpackage layout in architecture_plan §3.2.

package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

// UploadRequest is transfer's local mirror of pkg/uos.PutObjectRequest,
// trimmed to the fields the orchestrator actually consumes. Drivers
// translate their richer request types into this shape (or pkg/uos
// does it on their behalf in the public Upload entrypoint).
//
// Field map vs. pkg/uos.PutObjectRequest (informational):
//   - Bucket / Key  ↔ Bucket / Key
//   - Size          ↔ size of the body when known; -1 when unknown
//   - Body          ↔ the io.Reader payload
//   - ContentType   ↔ ContentHeaders.ContentType (passthrough)
//   - Metadata      ↔ Metadata (passthrough; opaque to Manager)
type UploadRequest struct {
	// Bucket is the destination bucket name.
	Bucket string
	// Key is the destination object key.
	Key string
	// Size is the total payload size in bytes, or -1 when unknown.
	Size int64
	// Body is the payload. Manager reads from it sequentially when
	// performing a single-shot Put, or in PartSize-sized chunks for
	// multipart uploads.
	Body io.Reader
	// ContentType is the MIME type of the payload, passed through to
	// the underlying service unchanged.
	ContentType string
	// Metadata is opaque user metadata, passed through to the
	// underlying service unchanged. Manager performs no normalisation.
	Metadata map[string]string
}

// UploadResult summarises a successful upload.
type UploadResult struct {
	// Bucket / Key name the destination object.
	Bucket string
	Key    string
	// ETag is the entity tag returned by the service, when available.
	ETag string
	// VersionID is the version identifier returned for versioned
	// buckets, when available.
	VersionID string
	// Size is the total number of bytes uploaded.
	Size int64
	// Multipart is true when Manager used the multipart workflow.
	Multipart bool
	// PartCount is the number of parts uploaded, or 1 for single-shot.
	PartCount int
}

// PartSpec describes a single part to upload. Manager constructs one
// per chunk it slices off the body and hands it to MultipartServiceLike.
type PartSpec struct {
	// UploadID identifies the multipart upload this part belongs to.
	UploadID string
	// Bucket / Key name the destination object.
	Bucket string
	Key    string
	// PartNumber is the 1-based index of the part.
	PartNumber int
	// Body is a fresh io.Reader for this part; Manager reads it once.
	Body io.Reader
	// Size is the number of bytes in Body.
	Size int64
}

// UploadedPart is what MultipartServiceLike.UploadPart returns. It is
// the minimal completion descriptor Manager needs to call Complete.
type UploadedPart struct {
	// PartNumber matches the PartSpec the part was uploaded against.
	PartNumber int
	// ETag is the part's entity tag returned by the service.
	ETag string
}

// MultipartServiceLike is transfer's local mirror of the subset of
// pkg/uos.MultipartService that Manager calls. Drivers MAY satisfy
// this interface directly, or pkg/uos.Client MAY adapt its richer
// MultipartService into this shape. Method semantics match pkg/uos.
//
// Field/method map vs. pkg/uos.MultipartService (informational):
//   - Initiate(ctx, bucket, key, contentType, metadata) ↔ Initiate(InitiateMultipartRequest)
//   - UploadPart(ctx, PartSpec)                         ↔ UploadPart(UploadPartRequest)
//   - Complete(ctx, bucket, key, uploadID, parts)       ↔ Complete(CompleteMultipartRequest)
//   - Abort(ctx, bucket, key, uploadID)                 ↔ Abort(AbortMultipartRequest)
//   - SinglePut(ctx, UploadRequest)                     ↔ ObjectService.Put (folded in to keep
//     Manager's surface single-interface).
type MultipartServiceLike interface {
	// Initiate starts a multipart upload and returns its uploadID.
	Initiate(ctx context.Context, req UploadRequest) (uploadID string, err error)
	// UploadPart uploads one part and returns its completion descriptor.
	UploadPart(ctx context.Context, part PartSpec) (UploadedPart, error)
	// Complete finalises the upload by combining the listed parts.
	Complete(ctx context.Context, bucket, key, uploadID string, parts []UploadedPart) (UploadResult, error)
	// Abort releases the storage held by an in-progress multipart
	// upload. Manager calls Abort exactly once on non-resumable failure.
	Abort(ctx context.Context, bucket, key, uploadID string) error
	// SinglePut performs a non-multipart upload for small bodies.
	SinglePut(ctx context.Context, req UploadRequest) (UploadResult, error)
}

// resumeState is the on-disk shape Manager itself persists to the
// StateStore. It is intentionally small; if drivers later want a
// richer schema they can wrap the StateStore and serialise their own
// payload alongside it.
type resumeState struct {
	UploadID string
	Parts    []UploadedPart
}

// Manager orchestrates uploads using the planner / worker-pool /
// abort-on-failure pattern. It is intentionally a concrete struct
// (not an interface) so that drivers don't reimplement orchestration
// and additive method additions remain non-breaking. See
// architecture_plan §4.10 and pre-mortem #1 for the rationale.
type Manager struct {
	cfg Config
}

// NewManager constructs a Manager bound to cfg. cfg is captured by
// value; callers may mutate their copy after the call without affecting
// the Manager.
func NewManager(cfg Config) *Manager {
	return &Manager{cfg: cfg.withDefaults()}
}

// Config returns a copy of the Manager's effective configuration with
// zero-valued fields filled in by package defaults. Useful for tests
// and observability.
func (m *Manager) Config() Config {
	return m.cfg
}

// Upload runs the planner: single-shot Put for small or known-size
// bodies under the multipart threshold, multipart upload otherwise.
// On non-resumable failure it calls mp.Abort exactly once. When
// cfg.StateStore is non-nil, Upload (a) consults the store at start
// to skip already-completed parts and (b) persists progress after
// each successful part so a future call can resume.
func (m *Manager) Upload(ctx context.Context, mp MultipartServiceLike, req UploadRequest) (UploadResult, error) {
	if mp == nil {
		return UploadResult{}, errors.New("transfer: MultipartServiceLike is required")
	}
	if req.Body == nil {
		return UploadResult{}, errors.New("transfer: UploadRequest.Body is required")
	}
	if req.Bucket == "" || req.Key == "" {
		return UploadResult{}, errors.New("transfer: UploadRequest.Bucket and Key are required")
	}

	body, size, cleanup, err := m.materializeBody(req.Body, req.Size)
	if err != nil {
		return UploadResult{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	req.Body = body
	req.Size = size

	if size >= 0 && size < m.cfg.MultipartThreshold {
		m.reportProgress(0, size)
		res, err := mp.SinglePut(ctx, req)
		if err != nil {
			return UploadResult{}, err
		}
		m.reportProgress(size, size)
		res.Bucket = req.Bucket
		res.Key = req.Key
		res.Size = size
		res.Multipart = false
		res.PartCount = 1
		return res, nil
	}

	return m.uploadMultipart(ctx, mp, req)
}

// materializeBody applies UnknownSizePolicy when size < 0. On success
// it returns a body whose total length is reflected in size. The
// cleanup closure is non-nil when a temp file was spooled.
func (m *Manager) materializeBody(body io.Reader, size int64) (io.Reader, int64, func(), error) {
	if size >= 0 {
		return body, size, nil, nil
	}
	switch m.cfg.UnknownSizePolicy {
	case UnknownSizeReject:
		return nil, 0, nil, errors.New("transfer: upload size is unknown and UnknownSizePolicy is UnknownSizeReject")
	case UnknownSizeBuffer:
		buf, n, overflow, err := readWithLimit(body, m.cfg.BufferLimit)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("transfer: buffering unknown-size body: %w", err)
		}
		if overflow {
			return nil, 0, nil, errors.New("transfer: unknown-size body exceeded BufferLimit; configure UnknownSizeTempFile to handle larger streams")
		}
		return readerFromBytes(buf), n, nil, nil
	case UnknownSizeTempFile:
		spooled, n, cleanup, err := spoolToTempFile(body, m.cfg.TempDir)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("transfer: spooling unknown-size body: %w", err)
		}
		return spooled, n, cleanup, nil
	default:
		return nil, 0, nil, fmt.Errorf("transfer: unknown UnknownSizePolicy value %d", m.cfg.UnknownSizePolicy)
	}
}

// uploadMultipart drives the Initiate → parallel UploadPart → Complete
// flow with abort-on-failure cleanup and optional resume.
func (m *Manager) uploadMultipart(ctx context.Context, mp MultipartServiceLike, req UploadRequest) (UploadResult, error) {
	resume := m.loadResume(ctx, req)
	uploadID := resume.UploadID
	if uploadID == "" {
		id, err := mp.Initiate(ctx, req)
		if err != nil {
			return UploadResult{}, fmt.Errorf("transfer: initiate multipart: %w", err)
		}
		uploadID = id
	}

	completed := make(map[int]UploadedPart, len(resume.Parts))
	for _, p := range resume.Parts {
		completed[p.PartNumber] = p
	}

	parts, err := m.runWorkers(ctx, mp, req, uploadID, completed)
	if err != nil {
		// abort-on-failure: best-effort; we surface the original error.
		_ = mp.Abort(context.Background(), req.Bucket, req.Key, uploadID)
		if m.cfg.StateStore != nil {
			_ = m.cfg.StateStore.Delete(context.Background(), m.resumeKey(req))
		}
		return UploadResult{}, err
	}

	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })

	res, err := mp.Complete(ctx, req.Bucket, req.Key, uploadID, parts)
	if err != nil {
		_ = mp.Abort(context.Background(), req.Bucket, req.Key, uploadID)
		return UploadResult{}, fmt.Errorf("transfer: complete multipart: %w", err)
	}
	if m.cfg.StateStore != nil {
		_ = m.cfg.StateStore.Delete(context.Background(), m.resumeKey(req))
	}
	res.Bucket = req.Bucket
	res.Key = req.Key
	res.Size = req.Size
	res.Multipart = true
	res.PartCount = len(parts)
	return res, nil
}

// runWorkers dispatches PartSpec values to a fixed pool of MaxConcurrency
// workers. It returns the full set of UploadedPart values (combining
// already-completed and freshly uploaded) on success, or the first
// observed error on failure (cancelling sibling workers via ctx).
func (m *Manager) runWorkers(ctx context.Context, mp MultipartServiceLike, req UploadRequest, uploadID string, completed map[int]UploadedPart) ([]UploadedPart, error) {
	specs, err := m.sliceParts(req, uploadID, completed)
	if err != nil {
		return nil, err
	}

	parts := make([]UploadedPart, 0, len(specs)+len(completed))
	for _, p := range completed {
		parts = append(parts, p)
	}

	if len(specs) == 0 {
		return parts, nil
	}

	jobCh := make(chan PartSpec)
	resCh := make(chan UploadedPart, len(specs))
	errCh := make(chan error, m.cfg.MaxConcurrency)

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < m.cfg.MaxConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for spec := range jobCh {
				up, err := mp.UploadPart(workCtx, spec)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("transfer: upload part %d: %w", spec.PartNumber, err):
					default:
					}
					cancel()
					return
				}
				up.PartNumber = spec.PartNumber
				resCh <- up
			}
		}()
	}

	go func() {
		defer close(jobCh)
		for _, s := range specs {
			select {
			case <-workCtx.Done():
				return
			case jobCh <- s:
			}
		}
	}()

	wg.Wait()
	close(resCh)

	select {
	case err := <-errCh:
		return nil, err
	default:
	}
	if err := workCtx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return nil, err
	}

	var transferred int64
	for _, c := range completed {
		_ = c
	}
	for up := range resCh {
		parts = append(parts, up)
		atomic.AddInt64(&transferred, m.cfg.PartSize)
		m.reportProgress(atomic.LoadInt64(&transferred), req.Size)
		if m.cfg.StateStore != nil {
			snap := resumeState{UploadID: uploadID, Parts: parts}
			_ = m.cfg.StateStore.Save(ctx, m.resumeKey(req), encodeResume(snap))
		}
	}
	return parts, nil
}

// sliceParts breaks the request body into PartSpec values, skipping
// part numbers already present in completed. Each spec carries an
// independent in-memory copy of its slice so that worker goroutines
// can read concurrently without contending on the shared body Reader.
func (m *Manager) sliceParts(req UploadRequest, uploadID string, completed map[int]UploadedPart) ([]PartSpec, error) {
	if req.Size <= 0 {
		return nil, errors.New("transfer: multipart upload requires a positive size")
	}
	partSize := m.cfg.PartSize
	totalParts := int((req.Size + partSize - 1) / partSize)
	specs := make([]PartSpec, 0, totalParts)
	for i := 0; i < totalParts; i++ {
		partNumber := i + 1
		offset := int64(i) * partSize
		size := partSize
		if remaining := req.Size - offset; remaining < partSize {
			size = remaining
		}
		if _, ok := completed[partNumber]; ok {
			// Drop the corresponding bytes so the next part starts at the
			// right offset.
			if _, err := io.CopyN(io.Discard, req.Body, size); err != nil {
				return nil, fmt.Errorf("transfer: skipping completed part %d: %w", partNumber, err)
			}
			continue
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(req.Body, buf); err != nil {
			return nil, fmt.Errorf("transfer: reading part %d: %w", partNumber, err)
		}
		specs = append(specs, PartSpec{
			UploadID:   uploadID,
			Bucket:     req.Bucket,
			Key:        req.Key,
			PartNumber: partNumber,
			Body:       bytes.NewReader(buf),
			Size:       size,
		})
	}
	return specs, nil
}

// loadResume consults StateStore for a resume payload matching req.
// Missing or unreadable state yields an empty resumeState (no error).
func (m *Manager) loadResume(ctx context.Context, req UploadRequest) resumeState {
	if m.cfg.StateStore == nil {
		return resumeState{}
	}
	data, err := m.cfg.StateStore.Load(ctx, m.resumeKey(req))
	if err != nil {
		return resumeState{}
	}
	state, err := decodeResume(data)
	if err != nil {
		return resumeState{}
	}
	return state
}

// resumeKey computes the StateStore key for req, falling back to
// bucket+"/"+key when cfg.ResumeKey is nil.
func (m *Manager) resumeKey(req UploadRequest) string {
	if m.cfg.ResumeKey != nil {
		return m.cfg.ResumeKey(req.Bucket, req.Key)
	}
	return req.Bucket + "/" + req.Key
}

// reportProgress invokes the progress callback when configured. Errors
// in user code are not surfaced.
func (m *Manager) reportProgress(transferred, total int64) {
	if m.cfg.Progress == nil {
		return
	}
	m.cfg.Progress(transferred, total)
}
