package fsutil

// SyncDir does nothing on Windows: a directory cannot be flushed through a
// handle opened for reading, and NTFS journals renames itself.
func SyncDir(dir string) error { return nil }
