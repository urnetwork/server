//go:build unix

package server

import (
	// "net"
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

func SoReusePort(network string, address string, rawConn syscall.RawConn) error {
	var setErr error
	err := rawConn.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	return errors.Join(err, setErr)
}
