//go:build !windows

package main

import "os"

// detachConsole does nothing: only a Windows console program owns a window
// to release.
func detachConsole() {}

// stdinIsTerminal reports whether standard input is a terminal (a character
// device), where a person can answer a question.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
