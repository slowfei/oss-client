package middleware

import (
	"context"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestNoopChain_AllNil(t *testing.T) {
	t.Parallel()
	c := NoopChain()
	// Methods MUST NOT panic when their hook is nil.
	c.Log(context.Background(), Event{})
	c.Observe(context.Background(), Event{})
	ctx, finish := c.Start(context.Background(), "Op")
	if ctx == nil {
		t.Fatal("Start returned nil context")
	}
	if finish == nil {
		t.Fatal("Start returned nil finish func")
	}
	finish(Event{}) // MUST NOT panic
}

type recordingLogger struct {
	count atomic.Int64
	last  Event
}

func (r *recordingLogger) Log(_ context.Context, ev Event) {
	r.count.Add(1)
	r.last = ev
}

type recordingMetrics struct {
	count atomic.Int64
}

func (r *recordingMetrics) Observe(_ context.Context, _ Event) { r.count.Add(1) }

type recordingTracer struct {
	starts   atomic.Int64
	finishes atomic.Int64
}

func (r *recordingTracer) Start(ctx context.Context, _ Op) (context.Context, func(Event)) {
	r.starts.Add(1)
	return ctx, func(Event) { r.finishes.Add(1) }
}

func TestChain_DispatchesToHooks(t *testing.T) {
	t.Parallel()
	lg := &recordingLogger{}
	mt := &recordingMetrics{}
	tr := &recordingTracer{}
	c := Chain{Logger: lg, Metrics: mt, Tracer: tr}

	c.Log(context.Background(), Event{Op: "Get"})
	c.Observe(context.Background(), Event{Op: "Get"})
	_, finish := c.Start(context.Background(), "Get")
	finish(Event{})

	if lg.count.Load() != 1 {
		t.Fatalf("Logger called %d times, want 1", lg.count.Load())
	}
	if mt.count.Load() != 1 {
		t.Fatalf("Metrics called %d times, want 1", mt.count.Load())
	}
	if tr.starts.Load() != 1 || tr.finishes.Load() != 1 {
		t.Fatalf("Tracer starts=%d finishes=%d, want 1/1", tr.starts.Load(), tr.finishes.Load())
	}
	if lg.last.Op != "Get" {
		t.Fatalf("Logger received Op=%q, want Get", lg.last.Op)
	}
}

func TestNoopTypes_SatisfyInterfaces(t *testing.T) {
	t.Parallel()
	// Compile-time guarantee is in middleware.go; this test exercises
	// the methods to make sure they don't panic.
	NoopLogger{}.Log(context.Background(), Event{})
	NoopMetrics{}.Observe(context.Background(), Event{})
	ctx, finish := NoopTracer{}.Start(context.Background(), "x")
	if ctx == nil {
		t.Fatal("NoopTracer.Start returned nil context")
	}
	finish(Event{})
}

func TestRedactHeaders_Nil(t *testing.T) {
	t.Parallel()
	if got := RedactHeaders(nil); got != nil {
		t.Fatalf("RedactHeaders(nil) = %v, want nil", got)
	}
}

func TestRedactHeaders_RedactsSensitive(t *testing.T) {
	t.Parallel()
	in := http.Header{
		"Authorization":        {"AWS4-HMAC-SHA256 Credential=AKIA..."},
		"X-Amz-Security-Token": {"FwoGZXIvYXdzE..."},
		"X-Amz-Signature":      {"abcdef"},
		"X-Goog-Signature":     {"deadbeef"},
		"X-Ms-Signature":       {"sigvalue"},
		"Cookie":               {"session=secret"},
		"Content-Type":         {"application/json"},
		"X-Request-Id":         {"req-1"},
	}
	out := RedactHeaders(in)
	if out == nil {
		t.Fatal("expected non-nil output")
	}
	for _, k := range []string{
		"Authorization", "X-Amz-Security-Token", "X-Amz-Signature",
		"X-Goog-Signature", "X-Ms-Signature", "Cookie",
	} {
		if vs, ok := out[k]; !ok || len(vs) == 0 || vs[0] != redactedValue {
			t.Errorf("header %q not redacted: got %v", k, vs)
		}
	}
	// Non-sensitive headers must round-trip verbatim.
	if got := out["Content-Type"][0]; got != "application/json" {
		t.Errorf("Content-Type mutated: %q", got)
	}
	if got := out["X-Request-Id"][0]; got != "req-1" {
		t.Errorf("X-Request-Id mutated: %q", got)
	}

	// Original input MUST NOT be mutated.
	if in["Authorization"][0] != "AWS4-HMAC-SHA256 Credential=AKIA..." {
		t.Errorf("input was mutated by RedactHeaders")
	}
}

func TestRedactHeaders_CaseInsensitive(t *testing.T) {
	t.Parallel()
	in := http.Header{
		"AUTHORIZATION":   {"x"},
		"x-amz-signature": {"y"},
		"x-Upload-Token":  {"z"},
	}
	out := RedactHeaders(in)
	for k := range in {
		if out[k][0] != redactedValue {
			t.Errorf("header %q not redacted: got %q", k, out[k][0])
		}
	}
}

func TestRedactHeaders_DefensiveCopy(t *testing.T) {
	t.Parallel()
	in := map[string][]string{"X-Custom": {"a", "b"}}
	out := RedactHeaders(in)
	out["X-Custom"][0] = "mutated"
	if in["X-Custom"][0] == "mutated" {
		t.Fatal("RedactHeaders did not defensively copy slice")
	}
}

func TestRedactQuery_Nil(t *testing.T) {
	t.Parallel()
	if got := RedactQuery(nil); got != nil {
		t.Fatalf("RedactQuery(nil) = %v, want nil", got)
	}
}

func TestRedactQuery_RedactsSensitive(t *testing.T) {
	t.Parallel()
	in := url.Values{
		"X-Amz-Signature":  {"sigvalue"},
		"X-Amz-Credential": {"AKIA.../20260427/us-east-1/s3/aws4_request"},
		"signature":        {"abc"},
		"prefix":           {"foo/"},
	}
	out := RedactQuery(in)
	for _, k := range []string{"X-Amz-Signature", "X-Amz-Credential", "signature"} {
		if out[k][0] != redactedValue {
			t.Errorf("query %q not redacted: got %q", k, out[k][0])
		}
	}
	if out["prefix"][0] != "foo/" {
		t.Errorf("prefix mutated: %q", out["prefix"][0])
	}
}

func TestRedactURL_Nil(t *testing.T) {
	t.Parallel()
	if got := RedactURL(nil); got != nil {
		t.Fatalf("RedactURL(nil) = %v, want nil", got)
	}
}

func TestRedactURL_RedactsUserInfoAndQuery(t *testing.T) {
	t.Parallel()
	u, err := url.Parse("https://user:pass@example.com/bucket/key?X-Amz-Signature=sig&prefix=foo")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := RedactURL(u)
	if out.User == nil {
		t.Fatal("expected user-info to be present after redaction")
	}
	if out.User.String() != redactedValue {
		t.Errorf("user-info not redacted: %q", out.User.String())
	}
	q := out.Query()
	if q.Get("X-Amz-Signature") != redactedValue {
		t.Errorf("query signature not redacted: %q", q.Get("X-Amz-Signature"))
	}
	if q.Get("prefix") != "foo" {
		t.Errorf("non-sensitive query mutated: %q", q.Get("prefix"))
	}

	// Original URL MUST NOT be mutated.
	if u.User.String() != "user:pass" {
		t.Errorf("original URL was mutated")
	}
}
