//go:build !windows

package store

// isBusy reports whether err means another handle keeps a rename or a
// removal from going through. An open file never does outside Windows.
func isBusy(error) bool { return false }
