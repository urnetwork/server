//go:build windows

package server

// The same private zero-length inode owns one exclusive byte-range lock on
// Windows. LockFileEx permits a locked range beyond end-of-file.

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows access is enforced by OpenFile and the directory's inherited ACL;
// portable FileMode permission bits do not represent that ACL.
func localBlobCapacityPrivateFile(info os.FileInfo) bool {
	return info.Mode().IsRegular()
}

// Immediate failure is ordinary contention only for an actual lock violation.
func localBlobCapacityTryLock(file *os.File) (bool, error) {
	overlapped := windows.Overlapped{}
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

// Unlock exactly the range acquired by the capacity owner.
func localBlobCapacityUnlock(file *os.File) error {
	overlapped := windows.Overlapped{}
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
