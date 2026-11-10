package main

import (
	"bytes"
	"errors"
	"os"
	"sync"

	"github.com/FLighter7/workspace-server/internal/fsutil"
)

// logFile writes server.log and keeps it within limit bytes, at start and
// while the server runs, in one file. A file can only be cut at its end, so
// dropping the oldest lines means rewriting what stays; to do that rarely,
// a write that would pass limit first cuts the file to its last keep bytes,
// at a line start. The file is closed for the cut: on Windows an open handle
// blocks the rename that replaces it.
//
// Write never fails and never panics: a line that cannot be written within
// the limit, or while the file cannot be reopened, is dropped. The log's
// other copy, stderr, still gets it.
type logFile struct {
	path        string
	limit, keep int64

	mu   sync.Mutex
	f    *os.File // nil while the file cannot be opened
	size int64
}

// openLogFile cuts the log at path to its last keep bytes if it is over
// limit, and opens it for appending. keep must not exceed limit.
func openLogFile(path string, limit, keep int64) (*logFile, error) {
	l := &logFile{path: path, limit: limit, keep: keep}
	if err := trimLog(path, limit, keep); err != nil {
		return nil, err
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// open opens the file for appending and reads its size. The caller holds mu
// or owns l alone.
func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *logFile) Write(p []byte) (int, error) {
	n := len(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	if int64(len(p)) > l.limit {
		// Its end says the most; a line this long is rare.
		p = p[int64(len(p))-l.keep:]
	}
	if l.f == nil || l.size+int64(len(p)) > l.limit {
		l.cut(int64(len(p)))
	}
	if l.f == nil || l.size+int64(len(p)) > l.limit {
		return n, nil
	}
	w, _ := l.f.Write(p)
	l.size += int64(w)
	return n, nil
}

// cut closes the file, keeps its last bytes so that room more bytes fit
// under the limit, and reopens it. On a failure the file stays closed until
// the next write tries again. The caller holds mu.
func (l *logFile) cut(room int64) {
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	_ = trimLog(l.path, 0, min(l.keep, l.limit-room))
	_ = l.open()
}

// Close closes the file.
func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// trimLog cuts the log at path to its last keep bytes, at a line start,
// when it is larger than over. A missing log is left missing.
func trimLog(path string, over, keep int64) error {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || fi.Size() <= over || fi.Size() <= keep {
		return err
	}
	tail, err := logTail(path, keep)
	if err != nil || tail == nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, tail, 0o600)
}

// logTail returns the last limit bytes of the log at path, from the first
// line start among them, or nil when the log is missing or within limit.
// The file is closed when it returns, so that it can be replaced on Windows.
func logTail(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= limit {
		return nil, err
	}
	// One byte more than is kept tells whether the kept part starts a line.
	buf := make([]byte, limit+1)
	if n, err := f.ReadAt(buf, fi.Size()-limit-1); n < len(buf) {
		return nil, err
	}
	// A newline in the last byte would keep nothing; the last line is kept
	// cut instead.
	if i := bytes.IndexByte(buf[:limit], '\n'); i >= 0 {
		return buf[i+1:], nil
	}
	return buf[1:], nil
}
