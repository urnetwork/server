//go:build linux || darwin

package server

// Advisory locks are attached to the open inode, not a process-local mutex;
// operating-system close/crash releases them without deleting the owner file.

import (
	"errors"
	"os"
	"syscall"
)

// The persistent coordination file has no payload and grants no other access.
func localBlobCapacityPrivateFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0
}

// Distinguish ordinary contention from unsupported filesystems and I/O errors.
func localBlobCapacityTryLock(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

// The persistent file remains in place after its current owner releases it.
func localBlobCapacityUnlock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
