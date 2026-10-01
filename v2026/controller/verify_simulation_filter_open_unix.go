//go:build unix

package controller

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// verifySimulationOpenAssignmentFilterNoFollow walks an absolute path through
// directory descriptors and refuses symbolic links at every component. Each
// Openat is bound to the directory descriptor returned by the prior Openat, so
// a concurrent rename cannot redirect a checked string path between check and
// use. Replacing the leaf atomically remains valid: the final Openat observes
// either complete regular-file inode.
func verifySimulationOpenAssignmentFilterNoFollow(path string) (*os.File, bool, error) {
	cleanPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanPath) {
		return nil, false, errors.New("verify simulation assignment filter path is not absolute")
	}
	components := strings.Split(strings.TrimPrefix(cleanPath, string(filepath.Separator)), string(filepath.Separator))
	if len(components) == 0 || components[0] == "" {
		return nil, false, errors.New("verify simulation assignment filter path has no leaf")
	}
	directoryFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, fmt.Errorf("open verify simulation assignment filter root: %w", err)
	}
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			unix.Close(directoryFD)
			return nil, false, errors.New("verify simulation assignment filter path component is invalid")
		}
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index+1 < len(components) {
			flags |= unix.O_DIRECTORY
		} else {
			// A swapped FIFO must not block before descriptor metadata rejects
			// it as non-regular in the caller.
			flags |= unix.O_NONBLOCK
		}
		nextFD, openErr := unix.Openat(directoryFD, component, flags, 0)
		unix.Close(directoryFD)
		if openErr != nil {
			if errors.Is(openErr, unix.ENOENT) {
				return nil, true, nil
			}
			return nil, false, fmt.Errorf("open verify simulation assignment filter component %q without following links: %w", component, openErr)
		}
		directoryFD = nextFD
	}
	file := os.NewFile(uintptr(directoryFD), cleanPath)
	if file == nil {
		unix.Close(directoryFD)
		return nil, false, errors.New("open verify simulation assignment filter descriptor failed")
	}
	return file, false, nil
}
