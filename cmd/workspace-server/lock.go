package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// errLocked is tryLock's error when another process holds the lock.
var errLocked = errors.New("locked by another process")

// lockServerDir locks server.lock in the server folder dir, so that a second
// server cannot use the folder: the two would overwrite each other's files. The lock lasts until unlock is called or the process ends. The file
// stays on disk: removing it would let a server that still holds the lock on
// the removed file and one that locks a new file run together.
//
// The error wraps errLocked when another process holds the lock. Any other
// error means the lock cannot be taken here, as on a file system without
// locks.
func lockServerDir(dir string) (unlock func(), err error) {
	path := filepath.Join(dir, lockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = releaseLock(f)
		_ = f.Close()
	}, nil
}
