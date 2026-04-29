package streamio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/capability"
)

// fakeClient implements uos.Client with recording stubs for Objects and
// Multipart; the other methods panic so the Writer's contract is
// implicitly verified (it MUST only touch Objects.Put and Multipart.*).
type fakeClient struct {
	objects    map[string]*fakeObjectService
	multiparts map[string]*fakeMultipartService
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		objects:    map[string]*fakeObjectService{},
		multiparts: map[string]*fakeMultipartService{},
	}
}

func (c *fakeClient) Provider() uos.Provider                                 { return "fake" }
func (c *fakeClient) Capabilities(context.Context) (capability.Report, error) { return capability.Report{}, nil }
func (c *fakeClient) Buckets() uos.BucketService                              { panic("Buckets not expected") }
func (c *fakeClient) Signer(string) uos.Signer                               { panic("Signer not expected") }
func (c *fakeClient) As(any) bool                                            { return false }
func (c *fakeClient) Close() error                                           { return nil }

func (c *fakeClient) Objects(bucket string) uos.ObjectService {
	if c.objects[bucket] == nil {
		c.objects[bucket] = &fakeObjectService{}
	}
	return c.objects[bucket]
}

func (c *fakeClient) Multipart(bucket string) uos.MultipartService {
	if c.multiparts[bucket] == nil {
		c.multiparts[bucket] = &fakeMultipartService{}
	}
	return c.multiparts[bucket]
}

type fakeObjectService struct {
	puts   []capturedPut
	putErr error
}

type capturedPut struct {
	Key         string
	Body        []byte
	Size        int64
	ContentType string
	Metadata    uos.Metadata
}

func (s *fakeObjectService) Put(_ context.Context, req uos.PutObjectRequest) (*uos.PutObjectResult, error) {
	if s.putErr != nil {
		return nil, s.putErr
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	s.puts = append(s.puts, capturedPut{
		Key: req.Key, Body: body, Size: req.Size,
		ContentType: req.Content.ContentType, Metadata: req.Metadata,
	})
	return &uos.PutObjectResult{ETag: "fake-put-etag"}, nil
}

func (*fakeObjectService) Get(context.Context, uos.GetObjectRequest) (*uos.ObjectReader, error) {
	panic("Get not expected")
}
func (*fakeObjectService) Head(context.Context, uos.HeadObjectRequest) (*uos.ObjectInfo, error) {
	panic("Head not expected")
}
func (*fakeObjectService) Delete(context.Context, uos.DeleteObjectRequest) error {
	panic("Delete not expected")
}
func (*fakeObjectService) Exists(context.Context, uos.HeadObjectRequest) (bool, error) {
	panic("Exists not expected")
}
func (*fakeObjectService) DeleteMany(context.Context, uos.DeleteManyRequest) (*uos.DeleteManyResult, error) {
	panic("DeleteMany not expected")
}
func (*fakeObjectService) Copy(context.Context, uos.CopyObjectRequest) (*uos.CopyObjectResult, error) {
	panic("Copy not expected")
}
func (*fakeObjectService) List(context.Context, uos.ListObjectsRequest) (*uos.ObjectList, error) {
	panic("List not expected")
}

type fakeMultipartService struct {
	initiates    []uos.InitiateMultipartRequest
	uploads      []capturedUploadPart
	completes    []uos.CompleteMultipartRequest
	aborts       []uos.AbortMultipartRequest
	initiateErr  error
	uploadErrAt  int // 1-based; if PartNumber == this, fail
	completeErr  error
}

type capturedUploadPart struct {
	PartNumber int
	UploadID   string
	Body       []byte
}

func (s *fakeMultipartService) Initiate(_ context.Context, req uos.InitiateMultipartRequest) (*uos.MultipartUpload, error) {
	if s.initiateErr != nil {
		return nil, s.initiateErr
	}
	s.initiates = append(s.initiates, req)
	return &uos.MultipartUpload{
		UploadID:  "fake-upload-id",
		Bucket:    req.Bucket,
		Key:       req.Key,
		Initiated: time.Now(),
	}, nil
}

func (s *fakeMultipartService) UploadPart(_ context.Context, req uos.UploadPartRequest) (*uos.UploadedPart, error) {
	if s.uploadErrAt != 0 && req.PartNumber == s.uploadErrAt {
		return nil, errors.New("simulated upload-part failure")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	s.uploads = append(s.uploads, capturedUploadPart{
		PartNumber: req.PartNumber, UploadID: req.UploadID, Body: body,
	})
	return &uos.UploadedPart{
		PartNumber: req.PartNumber,
		ETag:       fmt.Sprintf("etag-%d", req.PartNumber),
		Size:       int64(len(body)),
	}, nil
}

func (s *fakeMultipartService) Complete(_ context.Context, req uos.CompleteMultipartRequest) (*uos.PutObjectResult, error) {
	if s.completeErr != nil {
		return nil, s.completeErr
	}
	s.completes = append(s.completes, req)
	return &uos.PutObjectResult{ETag: "complete-etag"}, nil
}

func (s *fakeMultipartService) Abort(_ context.Context, req uos.AbortMultipartRequest) error {
	s.aborts = append(s.aborts, req)
	return nil
}

func (*fakeMultipartService) List(context.Context, uos.ListMultipartUploadsRequest) (*uos.MultipartUploadList, error) {
	panic("List not expected")
}

// --- tests ---

const testBucket = "test-bucket"
const testKey = "test/key.bin"

func TestNewWriter_Validation(t *testing.T) {
	cases := []struct {
		name   string
		cli    uos.Client
		bucket string
		key    string
		opts   WriterOptions
		want   string // substring
	}{
		{"nil client", nil, "b", "k", WriterOptions{}, "nil client"},
		{"empty bucket", newFakeClient(), "", "k", WriterOptions{}, "bucket is required"},
		{"empty key", newFakeClient(), "b", "", WriterOptions{}, "key is required"},
		{"part size below min", newFakeClient(), "b", "k", WriterOptions{PartSize: 1024}, "below cross-vendor safe minimum"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewWriter(context.Background(), tc.cli, tc.bucket, tc.key, tc.opts)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestWriter_SmallObject_UsesPut(t *testing.T) {
	cli := newFakeClient()
	w, err := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{
		ContentType: "text/plain",
		Metadata:    uos.Metadata{"src": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("small payload")
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mp := cli.multiparts[testBucket]
	if mp != nil && len(mp.initiates) > 0 {
		t.Fatalf("multipart should NOT be initiated for small objects, got %d initiates", len(mp.initiates))
	}
	obj := cli.objects[testBucket]
	if obj == nil || len(obj.puts) != 1 {
		t.Fatalf("expected 1 Put call, got %d", len(obj.puts))
	}
	got := obj.puts[0]
	if got.Key != testKey {
		t.Errorf("key: got %q want %q", got.Key, testKey)
	}
	if !bytes.Equal(got.Body, body) {
		t.Errorf("body mismatch")
	}
	if got.Size != int64(len(body)) {
		t.Errorf("size: got %d want %d", got.Size, len(body))
	}
	if got.ContentType != "text/plain" {
		t.Errorf("content-type: got %q", got.ContentType)
	}
	if got.Metadata["src"] != "test" {
		t.Errorf("metadata: got %v", got.Metadata)
	}
}

func TestWriter_LargeObject_UsesMultipart(t *testing.T) {
	cli := newFakeClient()
	w, err := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Write 12 MiB → 2 full parts (5 MiB each) + 1 final (2 MiB).
	body := bytes.Repeat([]byte("x"), 12*1024*1024)
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mp := cli.multiparts[testBucket]
	if mp == nil {
		t.Fatal("expected multipart to be initiated")
	}
	if len(mp.initiates) != 1 {
		t.Fatalf("expected 1 Initiate, got %d", len(mp.initiates))
	}
	if mp.initiates[0].Content.ContentType != "application/octet-stream" {
		t.Errorf("Initiate ContentType: got %q", mp.initiates[0].Content.ContentType)
	}
	if len(mp.uploads) != 3 {
		t.Fatalf("expected 3 UploadPart, got %d", len(mp.uploads))
	}
	if len(mp.uploads[0].Body) != 5*1024*1024 || len(mp.uploads[1].Body) != 5*1024*1024 || len(mp.uploads[2].Body) != 2*1024*1024 {
		t.Errorf("part sizes: %d, %d, %d", len(mp.uploads[0].Body), len(mp.uploads[1].Body), len(mp.uploads[2].Body))
	}
	if mp.uploads[0].PartNumber != 1 || mp.uploads[1].PartNumber != 2 || mp.uploads[2].PartNumber != 3 {
		t.Errorf("part numbers: %d, %d, %d", mp.uploads[0].PartNumber, mp.uploads[1].PartNumber, mp.uploads[2].PartNumber)
	}
	if len(mp.completes) != 1 {
		t.Fatalf("expected 1 Complete, got %d", len(mp.completes))
	}
	if len(mp.completes[0].Parts) != 3 {
		t.Errorf("Complete parts: got %d want 3", len(mp.completes[0].Parts))
	}
}

func TestWriter_ExactPartBoundary(t *testing.T) {
	// Write exactly 2 PartSize → 2 parts, NO trailing partial.
	cli := newFakeClient()
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{})
	body := bytes.Repeat([]byte("y"), 10*1024*1024)
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mp := cli.multiparts[testBucket]
	if len(mp.uploads) != 2 {
		t.Fatalf("expected 2 UploadPart, got %d", len(mp.uploads))
	}
}

func TestWriter_Abort_ReleasesMultipart(t *testing.T) {
	cli := newFakeClient()
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{})
	if _, err := w.Write(bytes.Repeat([]byte("z"), 6*1024*1024)); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	mp := cli.multiparts[testBucket]
	if len(mp.aborts) != 1 {
		t.Fatalf("expected 1 Abort, got %d", len(mp.aborts))
	}
	if len(mp.completes) != 0 {
		t.Errorf("Complete must NOT be called after Abort")
	}
	// Idempotent
	if err := w.Abort(); err != nil {
		t.Errorf("second Abort returned error: %v", err)
	}
	if len(mp.aborts) != 1 {
		t.Errorf("Abort should be idempotent; got %d total calls", len(mp.aborts))
	}
}

func TestWriter_Abort_NoMultipart_NoOp(t *testing.T) {
	cli := newFakeClient()
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{})
	if _, err := w.Write([]byte("tiny")); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	mp := cli.multiparts[testBucket]
	if mp != nil && len(mp.aborts) != 0 {
		t.Errorf("Abort with no multipart must be no-op; got %d aborts", len(mp.aborts))
	}
}

func TestWriter_Close_Idempotent(t *testing.T) {
	cli := newFakeClient()
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{})
	w.Write([]byte("hi"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
	if len(cli.objects[testBucket].puts) != 1 {
		t.Errorf("Put must run exactly once; got %d", len(cli.objects[testBucket].puts))
	}
}

func TestWriter_Write_AfterClose_Errors(t *testing.T) {
	cli := newFakeClient()
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{})
	w.Close()
	if _, err := w.Write([]byte("late")); err == nil {
		t.Fatal("expected error writing to closed writer")
	}
}

func TestWriter_Write_AfterAbort_Errors(t *testing.T) {
	cli := newFakeClient()
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{})
	w.Abort()
	if _, err := w.Write([]byte("late")); err == nil {
		t.Fatal("expected error writing to aborted writer")
	}
}

func TestWriter_SmallObjectThreshold_ForcesMultipart(t *testing.T) {
	cli := newFakeClient()
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{
		PartSize:             5 * 1024 * 1024,
		SmallObjectThreshold: 1, // anything > 1 byte triggers multipart at Close
	})
	w.Write([]byte("two bytes"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mp := cli.multiparts[testBucket]
	if mp == nil || len(mp.initiates) != 1 {
		t.Fatalf("expected 1 Initiate (forced via SmallObjectThreshold), got %d", len(mp.initiates))
	}
	if len(mp.completes) != 1 {
		t.Fatalf("expected 1 Complete, got %d", len(mp.completes))
	}
	if obj := cli.objects[testBucket]; obj != nil && len(obj.puts) != 0 {
		t.Errorf("Put must NOT be called when multipart was forced; got %d", len(obj.puts))
	}
}

func TestWriter_UploadPartFailure_TriggersAbort(t *testing.T) {
	cli := newFakeClient()
	cli.multiparts[testBucket] = &fakeMultipartService{uploadErrAt: 1}
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{})
	_, err := w.Write(bytes.Repeat([]byte("a"), 6*1024*1024))
	if err == nil {
		t.Fatal("expected upload error")
	}
	// Close should attempt Abort to release vendor state
	if cerr := w.Close(); cerr == nil {
		t.Fatal("Close should return the sticky error")
	}
	mp := cli.multiparts[testBucket]
	if len(mp.aborts) != 1 {
		t.Errorf("expected 1 Abort after failed upload, got %d", len(mp.aborts))
	}
}

func TestWriter_EmptyWrite_NoOp(t *testing.T) {
	cli := newFakeClient()
	w, _ := NewWriter(context.Background(), cli, testBucket, testKey, WriterOptions{})
	n, err := w.Write(nil)
	if err != nil || n != 0 {
		t.Fatalf("nil Write: got n=%d err=%v", n, err)
	}
	n, err = w.Write([]byte{})
	if err != nil || n != 0 {
		t.Fatalf("empty Write: got n=%d err=%v", n, err)
	}
}
