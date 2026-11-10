// Package conformance reads the contract vectors in testdata/conformance at
// the repository root, for tests. The same files are checked by the client
// package's tests (client/), so the server and the client cannot drift apart
// without one side's tests failing.
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Dir is the folder that holds the vectors.
func Dir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("conformance: cannot locate the source file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "conformance")
}

// Read decodes the vector file name into v, failing the test if it cannot.
func Read(t testing.TB, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}
