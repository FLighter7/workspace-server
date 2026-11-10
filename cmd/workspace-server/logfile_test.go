package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The start keeps the last lines of an oversized log, cut at a line start.
func TestOpenLogFileKeepsTheLastLines(t *testing.T) {
	const three = "first line\nsecond line\nthird line\n" // 11 + 12 + 11 bytes
	for _, c := range []struct {
		name    string
		content string
		limit   int64
		want    string
	}{
		{"under the limit", three, 100, three},
		{"cut inside a line", three, 20, "third line\n"},
		{"cut at a line start", three, 23, "second line\nthird line\n"},
		{"one long line", "0123456789abcdef", 6, "abcdef"},
		{"a long last line", "ab\n0123456789\n", 6, "56789\n"},
		{"no file", "", 10, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.log")
			if c.content != "" {
				writeFile(t, path, c.content)
			}
			// keep = limit + 5 leaves room for "next\n" without a second trim.
			f, err := openLogFile(path, c.limit+5, c.limit)
			if err != nil {
				t.Fatal(err)
			}
			if c.content != "" && int64(len(c.content)) <= c.limit+5 {
				// Within the limit the file is left as it is.
				if got, _ := os.ReadFile(path); string(got) != c.content {
					t.Errorf("log within the limit was changed to %q", got)
				}
				c.want = c.content
			}
			if _, err := f.Write([]byte("next\n")); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if got, _ := os.ReadFile(path); string(got) != c.want+"next\n" {
				t.Errorf("log = %q, want %q", got, c.want+"next\n")
			}
		})
	}
}

func TestLogFileNeverExceedsTheLimit(t *testing.T) {
	const limit, keep = 1024, 512
	path := filepath.Join(t.TempDir(), "server.log")
	f, err := openLogFile(path, limit, keep)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := range 500 {
		if _, err := fmt.Fprintf(f, "line %04d of the log, padded to some length\n", i); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > limit {
			t.Fatalf("after line %d the log is %d bytes, over the %d limit", i, fi.Size(), limit)
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "line 0499 of the log, padded to some length\n") {
		t.Errorf("the newest line is missing:\n%s", data)
	}
	if strings.Contains(string(data), "line 0000 ") {
		t.Errorf("the oldest line is still there")
	}
	if !strings.HasPrefix(string(data), "line ") {
		t.Errorf("the log does not start at a line start: %q", data[:min(len(data), 40)])
	}
}

func TestOpenLogFileCutsAnOversizedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	var big bytes.Buffer
	for i := 0; big.Len() < 5<<10; i++ {
		fmt.Fprintf(&big, "old line %d\n", i)
	}
	writeFile(t, path, big.String())
	f, err := openLogFile(path, 1024, 512)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := os.Stat(path)
	if err != nil || fi.Size() > 1024 {
		t.Fatalf("after open: %v bytes, err %v; want at most 1024", fi.Size(), err)
	}
}

func TestLogFileConcurrentWritersKeepWholeLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	f, err := openLogFile(path, 4096, 2048)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Go(func() {
			for i := range 1000 {
				fmt.Fprintf(f, "writer %d line %04d end\n", w, i)
			}
		})
	}
	wg.Wait()
	data, _ := os.ReadFile(path)
	if len(data) > 4096 {
		t.Errorf("log is %d bytes, over the limit", len(data))
	}
	// A cut removes only the oldest lines, so each writer's lines in the
	// file run without a gap and, if any are left, up to its last one. A
	// writer that finished early may have been cut out entirely.
	seen := map[int][]int{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var w, i int
		if n, _ := fmt.Sscanf(line, "writer %d line %d end", &w, &i); n != 2 || line != fmt.Sprintf("writer %d line %04d end", w, i) {
			t.Errorf("a broken line: %q", line)
			continue
		}
		seen[w] = append(seen[w], i)
	}
	if len(seen) == 0 {
		t.Fatal("no line survived")
	}
	for w, idx := range seen {
		for k := 1; k < len(idx); k++ {
			if idx[k] != idx[k-1]+1 {
				t.Errorf("writer %d: line %d follows line %d; a line in the kept window was lost", w, idx[k], idx[k-1])
			}
		}
		if last := idx[len(idx)-1]; last != 999 {
			t.Errorf("writer %d: the file ends its lines at %d, want 999", w, last)
		}
	}
}

func TestLogFileLineLongerThanTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	f, err := openLogFile(path, 1024, 512)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "short\n")
	long := strings.Repeat("x", 8<<10) + "\n"
	if _, err := f.Write([]byte(long)); err != nil {
		t.Errorf("Write = %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Size() > 1024 || fi.Size() == 0 {
		t.Errorf("after an 8 KiB line the log is %d bytes, want 1..1024", fi.Size())
	}
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "xxx\n") {
		t.Errorf("the end of the long line was not kept")
	}
}

// A trim that cannot reopen the file (its folder is gone) drops lines; it
// never panics and never fails the writer.
func TestLogFileSurvivesAFailedReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := openLogFile(filepath.Join(dir, "server.log"), 256, 128)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// The state a trim leaves when it cannot reopen the file: closed, with
	// the folder gone. Windows would not delete the folder under an open
	// file, so the file is closed first, the way a trim closes it.
	f.mu.Lock()
	_ = f.f.Close()
	f.f = nil
	f.mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		if _, err := fmt.Fprintf(f, "line %d after the folder went\n", i); err != nil {
			t.Fatalf("Write = %v, want no error", err)
		}
	}
}
