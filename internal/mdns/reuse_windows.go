//go:build windows

package mdns

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// reuse lets the socket share its address and port with the system's own
// responder (the DNS Client), which holds 5353 too.
func reuse(network, address string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		err = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	}); cerr != nil {
		return cerr
	}
	return err
}
