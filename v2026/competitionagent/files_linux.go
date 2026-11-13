// Linux-only descriptor ownership matches the systemd executor platform.
package competitionagent

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// The root is captured once; every subsequent child lookup is descriptor
// relative. This plain open never retries a confined leaf symlink.
func openDirectoryNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// os.Root.OpenFile follows confined relative symlinks despite O_NOFOLLOW.
// Exact-inode callers instead reject every path component with raw openat.
func openRelativeNoFollow(root *os.File, name string, flags int, mode os.FileMode) (*os.File, error) {
	if strings.Contains(name, "\\") {
		return nil, errors.New("subevaluation descriptor path is not a canonical relative identity")
	}
	return openKernelRelativeNoFollow(root, name, flags, mode)
}

// Native cgroup names can contain literal systemd path-unit escapes. Linux
// does not interpret those backslashes. Keep them literal while rejecting
// traversal and every symlink; artifact/journal callers retain the stricter
// wrapper above. Native callers separately prove the held cgroup filesystem.
func openKernelRelativeNoFollow(root *os.File, name string, flags int, mode os.FileMode) (*os.File, error) {
	if root == nil || !fs.ValidPath(name) || strings.ContainsAny(name, "\x00\r\n") || mode&0777 != mode {
		return nil, errors.New("kernel descriptor path is not a canonical relative identity")
	}
	parts := strings.Split(name, "/")
	parent, held := int(root.Fd()), -1
	defer func() {
		if held >= 0 {
			_ = syscall.Close(held)
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		next, err := syscall.Openat(parent, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if held >= 0 {
			_ = syscall.Close(held)
			held = -1
		}
		if err != nil {
			return nil, err
		}
		parent, held = next, next
	}
	fd, err := syscall.Openat(parent, parts[len(parts)-1], flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, uint32(mode))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "subevaluation-confined-property"), nil
}

// Creation and traversal share held descriptors. Existing relative aliases are
// rejected before any deeper mkdir; each new directory entry is synchronized.
func mkdirRelativeNoFollow(root *os.File, name string, mode os.FileMode) error {
	if root == nil || !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\\x00\r\n") || mode&0777 != mode {
		return errors.New("subevaluation directory path is not an exact relative identity")
	}
	parent, err := openRelativeNoFollow(root, ".", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	for _, part := range strings.Split(name, "/") {
		if err := syscall.Mkdirat(int(parent.Fd()), part, uint32(mode)); err != nil && !errors.Is(err, syscall.EEXIST) {
			return err
		}
		child, err := openRelativeNoFollow(parent, part, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		if err := parent.Sync(); err != nil {
			_ = child.Close()
			return err
		}
		_ = parent.Close()
		parent = child
	}
	return nil
}

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
