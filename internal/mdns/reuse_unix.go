//go:build !windows

package mdns

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reuse lets the socket share its address and port with the system's own
// responder, which holds 5353 too.
func reuse(network, address string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err == nil {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}
	}); cerr != nil {
		return cerr
	}
	return err
}
