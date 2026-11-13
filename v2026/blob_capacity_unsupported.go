//go:build !linux && !darwin && !windows

package server

// A backend without qualified filesystem locking cannot advertise a shared
// local capacity owner. The independently configured MinIO backend is unchanged.

import (
	"errors"
	"os"
)

// No local coordination owner is advertised on an unsupported platform.
func localBlobCapacityPrivateFile(os.FileInfo) bool { return false }

// Unsupported local writers fail closed instead of using a process-only lock.
func localBlobCapacityTryLock(*os.File) (bool, error) {
	return false, errors.New("local blob capacity locking supports Linux, Darwin and Windows only")
}

// No unsupported lock can have been acquired.
func localBlobCapacityUnlock(*os.File) error {
	return errors.New("local blob capacity lock is unavailable on this platform")
}
