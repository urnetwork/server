//go:build !linux

package privateheapprofile

import (
	"errors"
	"net"
	"os"
)

func directoryOwnedBySelf(os.FileInfo) bool { return false }

func rootPeer(*net.UnixConn) bool       { return false }
func processCPU() (int64, int64, error) { return 0, 0, errors.New("private_heap_linux_only") }
func currentIdentity(string, string) (Identity, error) {
	return Identity{}, errors.New("private_heap_linux_only")
}
