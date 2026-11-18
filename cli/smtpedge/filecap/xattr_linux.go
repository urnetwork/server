//go:build linux

package filecap

import (
	"errors"
	"fmt"
	"syscall"
)

// Writes the capability on path, replacing any it held. Writing security.capability needs CAP_SETFCAP, which a
// build stage's root holds.
func SetFileCapability(path string, capability FileCapability) error {
	value, err := capability.Encode()
	if err != nil {
		return err
	}
	if err := syscall.Setxattr(path, AttributeName, value, 0); err != nil {
		return fmt.Errorf("set %s on %s: %w", AttributeName, path, err)
	}
	return nil
}

// Reads path's attribute bytes, as the kernel answers them to this process; a file that holds none answers
// ErrNoFileCapability.
func ReadFileCapability(path string) ([]byte, error) {
	read := func(value []byte) (int, error) {
		n, err := syscall.Getxattr(path, AttributeName, value)
		if errors.Is(err, syscall.ENODATA) {
			return 0, ErrNoFileCapability
		}
		if err != nil {
			return 0, fmt.Errorf("read %s of %s: %w", AttributeName, path, err)
		}
		return n, nil
	}
	// an empty buffer asks the attribute's length
	byteCount, err := read(nil)
	if err != nil {
		return nil, err
	}
	value := make([]byte, byteCount)
	n, err := read(value)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

// Sets SO_MARK on a fresh socket, the call smtpedge's dialer makes for each marked dial, and answers the kernel's
// refusal: EPERM unless this process holds CAP_NET_ADMIN, or, from Linux 5.17, CAP_NET_RAW, which docker's default
// set gives a container's root but not its uid 65532.
func ProbeSoMark() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)
	// any mark: the kernel checks the capability before the value
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_MARK, 1); err != nil {
		return fmt.Errorf("SO_MARK: %w", err)
	}
	return nil
}
