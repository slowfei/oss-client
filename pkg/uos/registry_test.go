package uos

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/slowfei/oss-client/pkg/uos/capability"
)

// fakeFactory is a Factory used only by registry tests. It records
// Validate / Open invocations and lets each test set the canned return
// values. Defined here (not in registry.go) so the production binary
// stays free of test scaffolding.
type fakeFactory struct {
	provider    Provider
	validateErr error
	openErr     error
	openClient  Client
	mu          sync.Mutex
	validateCnt int
	openCnt     int
	lastConfig  Config
}

func (f *fakeFactory) Provider() Provider { return f.provider }

func (f *fakeFactory) Validate(cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validateCnt++
	f.lastConfig = cfg
	return f.validateErr
}

func (f *fakeFactory) Open(_ context.Context, cfg Config) (Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCnt++
	f.lastConfig = cfg
	return f.openClient, f.openErr
}

// stubClient satisfies the Client interface with no-op implementations.
// It exists only as a return value for fakeFactory.Open in tests.
type stubClient struct{}

func (stubClient) Provider() Provider { return "stub" }
func (stubClient) Capabilities(context.Context) (capability.Report, error) {
	return capability.Report{Items: map[capability.Capability]capability.CapabilityStatus{}}, nil
}
func (stubClient) Buckets() BucketService            { return nil }
func (stubClient) Objects(string) ObjectService      { return nil }
func (stubClient) Multipart(string) MultipartService { return nil }
func (stubClient) Signer(string) Signer              { return nil }
func (stubClient) As(any) bool                       { return false }
func (stubClient) Close() error                      { return nil }

// Compile-time check that stubClient satisfies Client.
var _ Client = stubClient{}

func TestNewRegistry_OpenUnknownProvider(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_, err := r.Open(context.Background(), Config{Provider: "nope"})
	if err == nil {
		t.Fatal("expected error opening unknown provider")
	}
	var uerr *Error
	if !errors.As(err, &uerr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if uerr.Code != ErrInvalidArgument {
		t.Fatalf("expected ErrInvalidArgument, got %s", uerr.Code)
	}
}

func TestRegistry_Register_Nil(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	err := r.Register(nil)
	if err == nil {
		t.Fatal("expected error registering nil Factory")
	}
	var uerr *Error
	if !errors.As(err, &uerr) || uerr.Code != ErrInvalidArgument {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

func TestRegistry_Register_EmptyProvider(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	err := r.Register(&fakeFactory{provider: ""})
	if err == nil {
		t.Fatal("expected error registering empty Provider id")
	}
	var uerr *Error
	if !errors.As(err, &uerr) || uerr.Code != ErrInvalidArgument {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

func TestRegistry_Register_Duplicate(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(&fakeFactory{provider: "dup"}); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	err := r.Register(&fakeFactory{provider: "dup"})
	if err == nil {
		t.Fatal("expected error on duplicate register")
	}
	var uerr *Error
	if !errors.As(err, &uerr) || uerr.Code != ErrAlreadyExists {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

func TestRegistry_Open_EmptyProvider(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_, err := r.Open(context.Background(), Config{})
	if err == nil {
		t.Fatal("expected error for empty Config.Provider")
	}
	var uerr *Error
	if !errors.As(err, &uerr) || uerr.Code != ErrInvalidArgument {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

func TestRegistry_Open_ValidateError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	want := errors.New("bad config")
	f := &fakeFactory{provider: "ok", validateErr: want}
	if err := r.Register(f); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	_, err := r.Open(context.Background(), Config{Provider: "ok"})
	if !errors.Is(err, want) {
		t.Fatalf("expected validate error to surface, got %v", err)
	}
	if f.validateCnt != 1 {
		t.Fatalf("expected Validate to be called once, got %d", f.validateCnt)
	}
	if f.openCnt != 0 {
		t.Fatalf("Open MUST NOT be called when Validate fails, got %d", f.openCnt)
	}
}

func TestRegistry_Open_HappyPath(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	f := &fakeFactory{provider: "ok", openClient: stubClient{}}
	if err := r.Register(f); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	c, err := r.Open(context.Background(), Config{Provider: "ok", Region: "r"})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil Client")
	}
	if f.lastConfig.Region != "r" {
		t.Fatalf("expected Config to flow through, got %q", f.lastConfig.Region)
	}
	if f.validateCnt != 1 || f.openCnt != 1 {
		t.Fatalf("expected Validate=1 Open=1, got Validate=%d Open=%d", f.validateCnt, f.openCnt)
	}
}

func TestRegistry_ConcurrentRegister_Unique(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	var wg sync.WaitGroup
	const N = 50
	wg.Add(N)
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := Provider("p" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
			errCh <- r.Register(&fakeFactory{provider: id})
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		// All ids are unique so all registers should succeed.
		if err != nil {
			t.Errorf("concurrent register returned: %v", err)
		}
	}
}

func TestRegistry_ConcurrentRegister_SameID_OneWins(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	var wg sync.WaitGroup
	const N = 20
	wg.Add(N)
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			errCh <- r.Register(&fakeFactory{provider: "race"})
		}()
	}
	wg.Wait()
	close(errCh)
	successes := 0
	failures := 0
	for err := range errCh {
		if err == nil {
			successes++
			continue
		}
		failures++
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Code != ErrAlreadyExists {
			t.Errorf("expected ErrAlreadyExists for losers, got %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one successful register, got %d", successes)
	}
	if failures != N-1 {
		t.Fatalf("expected %d failures, got %d", N-1, failures)
	}
}

func TestDefaultRegistry_NotNil(t *testing.T) {
	t.Parallel()
	if DefaultRegistry() == nil {
		t.Fatal("DefaultRegistry returned nil")
	}
}

func TestDefaultIsIdempotent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		op   string
		want bool
	}{
		{"GetObject", true},
		{"HeadObject", true},
		{"ListObjects", true},
		{"ListBuckets", true},
		{"StatBucket", true},
		{"ExistsObject", true},
		{"ListMultipartUploads", true},
		{"SignURL", true},
		{"PutObject", false},
		{"DeleteObject", false},
		{"InitiateMultipart", false},
		{"UploadPart", false},
		{"CompleteMultipart", false},
		{"AbortMultipart", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := DefaultIsIdempotent(tc.op); got != tc.want {
			t.Errorf("DefaultIsIdempotent(%q) = %v, want %v", tc.op, got, tc.want)
		}
	}
}
