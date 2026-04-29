// Package streamio provides streaming-write helpers on top of the
// pkg/uos.Client interface.
//
// The Writer type wraps the unified MultipartService into an
// io.WriteCloser, hiding part-number tracking, minimum-part-size
// buffering, completion / abort lifecycle, and the small-object fast
// path. Callers stream bytes; the helper picks single-Put or
// multipart-upload at Close time based on total size.
//
// Every v1 provider supports the underlying multipart primitive, so
// streamio.Writer Just Works on all 10 drivers (aws, minio, alibaba,
// tencent, huawei, volcengine, gcs, azure, qiniu, upyun) without any
// vendor branching.
//
// Quickstart:
//
//	w, err := streamio.NewWriter(ctx, cli, "my-bucket", "log.txt", streamio.WriterOptions{
//	    ContentType: "text/plain",
//	})
//	if err != nil { return err }
//	defer w.Close()
//
//	for line := range incoming {
//	    if _, err := w.Write([]byte(line + "\n")); err != nil {
//	        w.Abort() // release vendor multipart state
//	        return err
//	    }
//	}
//	return w.Close() // commits via Complete (or single Put if small)
//
// See examples/streaming_write/ for a runnable demo.
package streamio
