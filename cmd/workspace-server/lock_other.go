//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package main

import "os"

// tryLock does nothing: on this system nothing keeps a second server off the
// data directory.
func tryLock(*os.File) error { return nil }

func releaseLock(*os.File) error { return nil }
