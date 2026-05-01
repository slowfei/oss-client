package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/maqian/oss-client/pkg/uos"
)

const cleanupBatchSize = 1000
const cleanupTimeout = 2 * time.Minute

func newRunPrefix(provider uos.Provider) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("uos-contract/%s/%d/", sanitizeKeySegment(string(provider)), time.Now().UnixNano())
	}
	return fmt.Sprintf("uos-contract/%s/%d-%s/", sanitizeKeySegment(string(provider)), time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func testKey(fut FactoryUnderTest, suffix string) string {
	if fut.keyPrefix == "" {
		return suffix
	}
	return fut.keyPrefix + strings.TrimPrefix(suffix, "/")
}

func cleanupTestArtifacts(ctx context.Context, t testingT, c uos.Client, fut FactoryUnderTest) {
	t.Helper()
	if fut.Bucket == "" || fut.keyPrefix == "" {
		return
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cleanupTimeout)
		defer cancel()
	}
	cleanupMultipartUploads(ctx, t, c, fut)
	cleanupObjects(ctx, t, c, fut)
}

func cleanupMultipartUploads(ctx context.Context, t testingT, c uos.Client, fut FactoryUnderTest) {
	t.Helper()
	mp := c.Multipart(fut.Bucket)
	req := uos.ListMultipartUploadsRequest{
		Bucket:     fut.Bucket,
		Prefix:     fut.keyPrefix,
		MaxResults: cleanupBatchSize,
	}
	var uploads []uos.MultipartUpload
	for {
		out, err := mp.List(ctx, req)
		if err != nil {
			if isNotFound(err) || isUnsupported(err) {
				return
			}
			t.Errorf("cleanup multipart uploads with prefix %q: List: %v", fut.keyPrefix, err)
			return
		}
		for _, upload := range out.Uploads {
			if strings.HasPrefix(upload.Key, fut.keyPrefix) {
				uploads = append(uploads, upload)
			}
		}
		if !out.Truncated {
			break
		}
		if out.NextToken == "" {
			t.Errorf("cleanup multipart uploads with prefix %q: truncated page without NextToken", fut.keyPrefix)
			return
		}
		req.ContinuationToken = out.NextToken
	}
	for _, upload := range uploads {
		if err := mp.Abort(ctx, uos.AbortMultipartRequest{
			Bucket:   fut.Bucket,
			Key:      upload.Key,
			UploadID: upload.UploadID,
		}); err != nil && !isNotFound(err) {
			t.Errorf("cleanup multipart upload %q/%q: %v", upload.Key, upload.UploadID, err)
		}
	}
}

func cleanupObjects(ctx context.Context, t testingT, c uos.Client, fut FactoryUnderTest) {
	t.Helper()
	objects := c.Objects(fut.Bucket)
	req := uos.ListObjectsRequest{
		Bucket:     fut.Bucket,
		Prefix:     fut.keyPrefix,
		MaxResults: cleanupBatchSize,
	}
	var keys []string
	for {
		out, err := objects.List(ctx, req)
		if err != nil {
			if isNotFound(err) {
				return
			}
			t.Errorf("cleanup objects with prefix %q: List: %v", fut.keyPrefix, err)
			return
		}
		for _, item := range out.Items {
			if strings.HasPrefix(item.Key, fut.keyPrefix) {
				keys = append(keys, item.Key)
			}
		}
		if !out.Truncated {
			break
		}
		if out.NextToken == "" {
			t.Errorf("cleanup objects with prefix %q: truncated page without NextToken", fut.keyPrefix)
			return
		}
		req.ContinuationToken = out.NextToken
	}
	deleteObjectBatch(ctx, t, objects, fut, keys)
}

func deleteObjectBatch(ctx context.Context, t testingT, objects uos.ObjectService, fut FactoryUnderTest, keys []string) {
	t.Helper()
	for len(keys) > 0 {
		n := cleanupBatchSize
		if len(keys) < n {
			n = len(keys)
		}
		batch := keys[:n]
		keys = keys[n:]
		res, err := objects.DeleteMany(ctx, uos.DeleteManyRequest{
			Bucket: fut.Bucket,
			Keys:   batch,
		})
		if err != nil && !isNotFound(err) {
			t.Errorf("cleanup objects with prefix %q: DeleteMany: %v", fut.keyPrefix, err)
			continue
		}
		if res == nil {
			continue
		}
		for _, failed := range res.Failed {
			t.Errorf("cleanup object %q failed: %s %s", failed.Key, failed.Code, failed.Message)
		}
	}
}

func sanitizeKeySegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '/':
			b.WriteByte('-')
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "case"
	}
	return out
}

func isNotFound(err error) bool {
	return errors.Is(err, &uos.Error{Code: uos.ErrNotFound})
}

func isUnsupported(err error) bool {
	return errors.Is(err, &uos.Error{Code: uos.ErrUnsupported})
}

type testingT interface {
	Helper()
	Errorf(format string, args ...any)
}
