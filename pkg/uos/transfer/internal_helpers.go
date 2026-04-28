package transfer

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// readWithLimit reads up to limit bytes from r. It returns the data
// read, the number of bytes, and overflow=true when r had more bytes
// available beyond limit.
func readWithLimit(r io.Reader, limit int64) ([]byte, int64, bool, error) {
	buf := bytes.NewBuffer(make([]byte, 0, limit))
	n, err := io.CopyN(buf, r, limit)
	if err != nil && err != io.EOF {
		return nil, 0, false, err
	}
	// Probe one more byte to detect overflow.
	probe := make([]byte, 1)
	pn, _ := r.Read(probe)
	if pn > 0 {
		buf.Write(probe[:pn])
		return buf.Bytes(), n + int64(pn), true, nil
	}
	return buf.Bytes(), n, false, nil
}

// readerFromBytes wraps a byte slice in an io.Reader without copying.
func readerFromBytes(b []byte) io.Reader {
	return bytes.NewReader(b)
}

// spoolToTempFile writes r to a temp file under dir (or the OS default
// when dir is empty), returning a re-opened reader positioned at the
// start, the byte count, and a cleanup closure that removes the file.
func spoolToTempFile(r io.Reader, dir string) (io.Reader, int64, func(), error) {
	f, err := os.CreateTemp(dir, "uos-transfer-*")
	if err != nil {
		return nil, 0, nil, err
	}
	n, err := io.Copy(f, r)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, 0, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, 0, nil, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	return f, n, cleanup, nil
}

// encodeResume serialises a resumeState with the json package. Errors
// are intentionally ignored at the call site (state is best-effort).
func encodeResume(s resumeState) []byte {
	data, _ := json.Marshal(s)
	return data
}

// decodeResume is the inverse of encodeResume.
func decodeResume(data []byte) (resumeState, error) {
	var s resumeState
	if err := json.Unmarshal(data, &s); err != nil {
		return resumeState{}, err
	}
	return s, nil
}
