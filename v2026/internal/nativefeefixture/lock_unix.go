//go:build unix

package nativefeefixture

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

const (
	lockExclusive   = syscall.LOCK_EX
	lockShared      = syscall.LOCK_SH
	lockNonblocking = syscall.LOCK_NB
)

// Opens path and takes how, a flock, on it; the lock lasts until the file is
// closed. A lock on a file that another process removed or replaced after this
// one opened it guards nothing, so it retries on the file now at path.
func lockFile(path string, how int) (*os.File, error) {
	for {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		for {
			err = syscall.Flock(int(file.Fd()), how)
			if !errors.Is(err, syscall.EINTR) {
				break
			}
		}
		if err != nil {
			file.Close()
			return nil, err
		}
		held, heldErr := file.Stat()
		current, currentErr := os.Stat(path)
		if heldErr == nil && currentErr == nil && os.SameFile(held, current) {
			return file, nil
		}
		file.Close()
		if heldErr != nil {
			return nil, heldErr
		}
		if currentErr != nil && !errors.Is(currentErr, fs.ErrNotExist) {
			return nil, currentErr
		}
	}
}

// nativefee.Invoke admits only an image with one link.
func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Nlink) == 1
}
