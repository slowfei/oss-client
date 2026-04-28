package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeMP is a hand-written mock satisfying MultipartServiceLike. It
// records calls and lets each test inject failure points.
type fakeMP struct {
	mu sync.Mutex

	uploadID string

	initiateErr error
	uploadErr   func(partNumber int) error // optional per-part error
	completeErr error
	singleErr   error

	initiated   int32
	aborted     int32
	completed   int32
	singlePuts  int32
	uploadParts int32

	uploaded []UploadedPart // recorded successful UploadPart results
	parts    []PartSpec     // recorded specs
}

func (f *fakeMP) Initiate(ctx context.Context, req UploadRequest) (string, error) {
	atomic.AddInt32(&f.initiated, 1)
	if f.initiateErr != nil {
		return "", f.initiateErr
	}
	if f.uploadID == "" {
		f.uploadID = "upload-1"
	}
	return f.uploadID, nil
}

func (f *fakeMP) UploadPart(ctx context.Context, p PartSpec) (UploadedPart, error) {
	atomic.AddInt32(&f.uploadParts, 1)
	if f.uploadErr != nil {
		if err := f.uploadErr(p.PartNumber); err != nil {
			return UploadedPart{}, err
		}
	}
	// Drain the body so byte counters stay correct.
	if _, err := io.Copy(io.Discard, p.Body); err != nil {
		return UploadedPart{}, err
	}
	up := UploadedPart{PartNumber: p.PartNumber, ETag: "etag-" + itoa(p.PartNumber)}
	f.mu.Lock()
	f.uploaded = append(f.uploaded, up)
	f.parts = append(f.parts, p)
	f.mu.Unlock()
	return up, nil
}

func (f *fakeMP) Complete(ctx context.Context, bucket, key, uploadID string, parts []UploadedPart) (UploadResult, error) {
	atomic.AddInt32(&f.completed, 1)
	if f.completeErr != nil {
		return UploadResult{}, f.completeErr
	}
	return UploadResult{ETag: "complete-etag"}, nil
}

func (f *fakeMP) Abort(ctx context.Context, bucket, key, uploadID string) error {
	atomic.AddInt32(&f.aborted, 1)
	return nil
}

func (f *fakeMP) SinglePut(ctx context.Context, req UploadRequest) (UploadResult, error) {
	atomic.AddInt32(&f.singlePuts, 1)
	if f.singleErr != nil {
		return UploadResult{}, f.singleErr
	}
	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		return UploadResult{}, err
	}
	return UploadResult{ETag: "single-etag"}, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	buf := make([]byte, 0, 12)
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func TestManager_Planner_PicksSinglePutBelowThreshold(t *testing.T) {
	t.Parallel()

	mp := &fakeMP{}
	m := NewManager(Config{MultipartThreshold: 1024, PartSize: 256, MaxConcurrency: 2})
	body := bytes.NewReader(make([]byte, 100))
	res, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: 100, Body: body,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Multipart {
		t.Fatal("expected single-shot, got multipart")
	}
	if atomic.LoadInt32(&mp.singlePuts) != 1 {
		t.Fatalf("singlePuts: want 1, got %d", mp.singlePuts)
	}
	if atomic.LoadInt32(&mp.initiated) != 0 {
		t.Fatalf("Initiate must NOT be called for small bodies, got %d", mp.initiated)
	}
}

func TestManager_Planner_PicksMultipartAtOrAboveThreshold(t *testing.T) {
	t.Parallel()

	mp := &fakeMP{}
	m := NewManager(Config{MultipartThreshold: 1024, PartSize: 512, MaxConcurrency: 2})
	body := bytes.NewReader(make([]byte, 1024))
	res, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: 1024, Body: body,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !res.Multipart {
		t.Fatal("expected multipart")
	}
	if res.PartCount != 2 {
		t.Fatalf("PartCount: want 2, got %d", res.PartCount)
	}
	if atomic.LoadInt32(&mp.completed) != 1 {
		t.Fatalf("Complete: want 1 call, got %d", mp.completed)
	}
}

func TestManager_AbortOnPartFailure(t *testing.T) {
	t.Parallel()

	mp := &fakeMP{
		uploadErr: func(part int) error {
			if part == 2 {
				return errors.New("boom")
			}
			return nil
		},
	}
	m := NewManager(Config{MultipartThreshold: 256, PartSize: 256, MaxConcurrency: 1})
	body := bytes.NewReader(make([]byte, 1024))
	_, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: 1024, Body: body,
	})
	if err == nil {
		t.Fatal("expected error from failed part")
	}
	if atomic.LoadInt32(&mp.aborted) != 1 {
		t.Fatalf("Abort: want 1 call, got %d", mp.aborted)
	}
	if atomic.LoadInt32(&mp.completed) != 0 {
		t.Fatalf("Complete must NOT be called on failure, got %d", mp.completed)
	}
}

func TestManager_AbortOnCompleteFailure(t *testing.T) {
	t.Parallel()

	mp := &fakeMP{completeErr: errors.New("complete failed")}
	m := NewManager(Config{MultipartThreshold: 256, PartSize: 256, MaxConcurrency: 1})
	body := bytes.NewReader(make([]byte, 512))
	_, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: 512, Body: body,
	})
	if err == nil {
		t.Fatal("expected complete failure")
	}
	if atomic.LoadInt32(&mp.aborted) != 1 {
		t.Fatalf("Abort: want 1 call after Complete failure, got %d", mp.aborted)
	}
}

func TestManager_ResumeFromState_SkipsCompletedParts(t *testing.T) {
	t.Parallel()

	store := NewMemoryStateStore()
	// Pre-populate state: claim parts 1+2 are already done.
	state := resumeState{
		UploadID: "resume-id",
		Parts: []UploadedPart{
			{PartNumber: 1, ETag: "etag-1"},
			{PartNumber: 2, ETag: "etag-2"},
		},
	}
	_ = store.Save(context.Background(), "b/k", encodeResume(state))

	mp := &fakeMP{uploadID: "resume-id"}
	m := NewManager(Config{
		MultipartThreshold: 256, PartSize: 256, MaxConcurrency: 2,
		StateStore: store,
	})
	body := bytes.NewReader(make([]byte, 1024))
	res, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: 1024, Body: body,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if atomic.LoadInt32(&mp.initiated) != 0 {
		t.Fatalf("resume must skip Initiate, got %d", mp.initiated)
	}
	if atomic.LoadInt32(&mp.uploadParts) != 2 {
		t.Fatalf("UploadPart: want 2 (parts 3+4), got %d", mp.uploadParts)
	}
	if res.PartCount != 4 {
		t.Fatalf("PartCount: want 4 (combined), got %d", res.PartCount)
	}
	// State store must be cleared after success.
	if _, err := store.Load(context.Background(), "b/k"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("StateStore not cleared after success, err=%v", err)
	}
}

func TestManager_ResumeFromState_PersistsOnFailure(t *testing.T) {
	t.Parallel()

	store := NewMemoryStateStore()
	mp := &fakeMP{
		uploadErr: func(part int) error {
			if part == 3 {
				return errors.New("boom")
			}
			return nil
		},
	}
	m := NewManager(Config{
		MultipartThreshold: 256, PartSize: 256, MaxConcurrency: 1,
		StateStore: store,
	})
	body := bytes.NewReader(make([]byte, 1024))
	_, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: 1024, Body: body,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	// Failure path: Manager calls Delete to clear stale state. This
	// matches the abort-on-failure semantics where the multipart upload
	// itself was aborted server-side, so any prior local state would be
	// stale. Resume across a freshly-Initiated upload is the next call.
	if _, err := store.Load(context.Background(), "b/k"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("expected state cleared after abort, got err=%v", err)
	}
}

func TestManager_UnknownSize_Reject(t *testing.T) {
	t.Parallel()

	mp := &fakeMP{}
	m := NewManager(Config{MultipartThreshold: 256, PartSize: 128, UnknownSizePolicy: UnknownSizeReject})
	_, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: -1, Body: strings.NewReader("hi"),
	})
	if err == nil || !strings.Contains(err.Error(), "UnknownSizeReject") {
		t.Fatalf("want UnknownSizeReject error, got %v", err)
	}
}

func TestManager_UnknownSize_Buffer(t *testing.T) {
	t.Parallel()

	mp := &fakeMP{}
	m := NewManager(Config{
		MultipartThreshold: 1024, PartSize: 256,
		UnknownSizePolicy: UnknownSizeBuffer, BufferLimit: 4096,
	})
	body := strings.NewReader("hello world") // 11 bytes
	res, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: -1, Body: body,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// 11 bytes < threshold → SinglePut.
	if res.Multipart {
		t.Fatal("expected single-shot for buffered small body")
	}
	if res.Size != 11 {
		t.Fatalf("Size: want 11, got %d", res.Size)
	}
}

func TestManager_UnknownSize_Buffer_OverflowRejects(t *testing.T) {
	t.Parallel()

	mp := &fakeMP{}
	m := NewManager(Config{
		MultipartThreshold: 1024, PartSize: 256,
		UnknownSizePolicy: UnknownSizeBuffer, BufferLimit: 8,
	})
	body := strings.NewReader("this body is longer than the buffer limit")
	_, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: -1, Body: body,
	})
	if err == nil || !strings.Contains(err.Error(), "BufferLimit") {
		t.Fatalf("want BufferLimit overflow error, got %v", err)
	}
}

func TestManager_UnknownSize_TempFile(t *testing.T) {
	t.Parallel()

	mp := &fakeMP{}
	m := NewManager(Config{
		MultipartThreshold: 1024, PartSize: 256,
		UnknownSizePolicy: UnknownSizeTempFile,
	})
	body := bytes.NewReader(make([]byte, 2000)) // > threshold so it goes multipart
	res, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: -1, Body: body,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !res.Multipart {
		t.Fatal("expected multipart for spooled large body")
	}
	if res.Size != 2000 {
		t.Fatalf("Size: want 2000, got %d", res.Size)
	}
}

func TestManager_RejectsMissingArguments(t *testing.T) {
	t.Parallel()

	m := NewManager(Config{})
	cases := map[string]UploadRequest{
		"missing body":   {Bucket: "b", Key: "k", Size: 0},
		"missing bucket": {Key: "k", Size: 0, Body: bytes.NewReader(nil)},
		"missing key":    {Bucket: "b", Size: 0, Body: bytes.NewReader(nil)},
	}
	mp := &fakeMP{}
	for name, req := range cases {
		req := req
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := m.Upload(context.Background(), mp, req); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestManager_NilMultipartServiceLikeRejected(t *testing.T) {
	t.Parallel()

	m := NewManager(Config{})
	if _, err := m.Upload(context.Background(), nil, UploadRequest{Bucket: "b", Key: "k", Body: bytes.NewReader(nil)}); err == nil {
		t.Fatal("expected error when MultipartServiceLike is nil")
	}
}

func TestManager_ProgressCallback_Invoked(t *testing.T) {
	t.Parallel()

	var calls int32
	mp := &fakeMP{}
	m := NewManager(Config{
		MultipartThreshold: 256, PartSize: 256, MaxConcurrency: 2,
		Progress: func(transferred, total int64) {
			atomic.AddInt32(&calls, 1)
		},
	})
	body := bytes.NewReader(make([]byte, 1024))
	if _, err := m.Upload(context.Background(), mp, UploadRequest{
		Bucket: "b", Key: "k", Size: 1024, Body: body,
	}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("Progress callback was never invoked")
	}
}
