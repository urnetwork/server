//go:build unix

// Native descriptor checks never wrap, read, write or close rejected handles.
package server

import "golang.org/x/sys/unix"

// Check direction before type: a Darwin runtime kqueue may stat as a pipe.
// The caller must validate its complete descriptor set before taking ownership.
func ValidateTestProcessPipe(fd uintptr, access TestProcessPipeAccess) error {
	var expectedAccess int
	switch access {
	case TestProcessPipeRead:
		expectedAccess = unix.O_RDONLY
	case TestProcessPipeWrite:
		expectedAccess = unix.O_WRONLY
	default:
		return errInvalidTestProcessPipe
	}
	flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil || flags&unix.O_ACCMODE != expectedAccess {
		return errInvalidTestProcessPipe
	}
	var info unix.Stat_t
	if err := unix.Fstat(int(fd), &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFIFO {
		return errInvalidTestProcessPipe
	}
	return nil
}
