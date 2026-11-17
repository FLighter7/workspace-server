package fsutil

import "testing"

func TestFreeSpace(t *testing.T) {
	free, err := FreeSpace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free <= 0 {
		t.Fatalf("FreeSpace = %d, want some space on the test volume", free)
	}
	if _, err := FreeSpace(t.TempDir() + "/missing/dir"); err == nil {
		t.Error("FreeSpace of a missing folder did not fail")
	}
}
