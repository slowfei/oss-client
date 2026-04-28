package contract

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/maqian/oss-client/pkg/uos"
)

// runSignerCases enumerates the Signer contract cases. SignURL cases
// make raw HTTP calls against the returned URL to prove the URL is
// usable; DirectGrant cases assert grant shape only because the
// concrete vendor verifier varies per provider.
func runSignerCases(t *testing.T, fut FactoryUnderTest) {
	t.Helper()

	runCase(t, fut, "sign_url_get_round_trip", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		key := "presign-get"
		body := []byte("presigned hello")
		if _, err := c.Objects(fut.Bucket).Put(ctx, uos.PutObjectRequest{
			Bucket: fut.Bucket, Key: key,
			Body: bytes.NewReader(body), Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("seed Put: %v", err)
		}
		signed, err := c.Signer(fut.Bucket).SignURL(ctx, uos.SignURLRequest{
			Bucket: fut.Bucket, Key: key,
			Method: http.MethodGet, ExpiresIn: 5 * time.Minute,
		})
		if err != nil {
			t.Fatalf("SignURL: %v", err)
		}
		if signed.URL == "" || signed.Method != http.MethodGet {
			t.Fatalf("SignURL: unexpected shape %+v", signed)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, signed.URL, nil)
		for k, vv := range signed.Headers {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("http.Get presigned: %v", err)
		}
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(got, body) {
			t.Fatalf("presigned GET body mismatch: want %q got %q", body, got)
		}
	})

	runCase(t, fut, "sign_url_put_round_trip", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		key := "presign-put"
		body := []byte("presigned put")
		signed, err := c.Signer(fut.Bucket).SignURL(ctx, uos.SignURLRequest{
			Bucket: fut.Bucket, Key: key,
			Method: http.MethodPut, ExpiresIn: 5 * time.Minute,
		})
		if err != nil {
			t.Fatalf("SignURL PUT: %v", err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, signed.URL, bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		for k, vv := range signed.Headers {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("http.Put presigned: %v", err)
		}
		_ = resp.Body.Close()
		// Verify via Get that the object was actually stored.
		r, err := c.Objects(fut.Bucket).Get(ctx, uos.GetObjectRequest{Bucket: fut.Bucket, Key: key})
		if err != nil {
			t.Fatalf("Get after presigned put: %v", err)
		}
		defer r.Body.Close()
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) {
			t.Fatalf("presigned PUT body mismatch: want %q got %q", body, got)
		}
	})

	runCase(t, fut, "issue_direct_grant_shape", func(t *testing.T, c uos.Client) {
		ctx := context.Background()
		ensureBucket(t, c, fut.Bucket)
		grant, err := c.Signer(fut.Bucket).IssueDirectGrant(ctx, uos.DirectGrantRequest{
			Bucket: fut.Bucket, Key: "direct-grant-target",
			Operation: uos.DirectGrantUpload, ExpiresIn: 5 * time.Minute,
		})
		if err != nil {
			t.Fatalf("IssueDirectGrant: %v", err)
		}
		if grant.ExpiresAt.IsZero() {
			t.Fatal("DirectGrant.ExpiresAt MUST be set for every Mode")
		}
		switch grant.Mode {
		case uos.DirectGrantModeURL:
			if grant.URL == "" {
				t.Fatal("DirectGrantModeURL: URL is required")
			}
		case uos.DirectGrantModeForm:
			if grant.URL == "" || grant.FormFields == nil {
				t.Fatal("DirectGrantModeForm: URL and FormFields are required")
			}
		case uos.DirectGrantModeToken:
			if grant.Token == "" {
				t.Fatal("DirectGrantModeToken: Token is required")
			}
		case uos.DirectGrantModeHeaders:
			if grant.URL == "" || grant.Headers == nil {
				t.Fatal("DirectGrantModeHeaders: URL and Headers are required")
			}
		default:
			t.Fatalf("DirectGrant.Mode %q is not one of the four frozen values", grant.Mode)
		}
	})
}
