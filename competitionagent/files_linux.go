// Linux-only descriptor ownership matches the systemd executor platform.
package competitionagent

import (
	"errors"
	"os"
	"syscall"
)

// Never follows a peer-controlled leaf symlink or accepts a device or pipe.
func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("subevaluation input is not a regular file")
	}
	return file, nil
}
