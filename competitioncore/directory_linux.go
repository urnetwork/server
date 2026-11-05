// Raw openat descriptors enforce no-follow at every relative component. Go's
// os.Root retry semantics are not used as a leaf O_NOFOLLOW security boundary.
package competitioncore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// Directory leases cannot escape through a relative symlink or replaced parent.
type directory struct{ file *os.File }

// Absolute paths are walked from a held root fd, with no symlink expansion.
func openDirectory(value string, protected bool) (*directory, error) {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsRune(value, 0) {
		return nil, errors.New("noncanonical core directory authority")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	self := &directory{file: os.NewFile(uintptr(fd), "core-directory")}
	if value != "/" {
		next, err := self.OpenDirectory(strings.TrimPrefix(value, "/"))
		self.Close()
		if err != nil {
			return nil, err
		}
		self = next
	}
	if protected {
		// Rewalk with ownership checks on the exact opened descriptors. This
		// path is fixed root control metadata, never candidate proc ownership.
		self.Close()
		return openProtectedDirectory(value)
	}
	return self, nil
}

// Protected control ancestors cannot be exchanged by an unprivileged writer.
func openProtectedDirectory(value string) (*directory, error) {
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	self := &directory{file: os.NewFile(uintptr(fd), "core-protected-directory")}
	for _, component := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		info, err := self.file.Stat()
		if err != nil || !protectedDirectoryInfo(info) {
			self.Close()
			return nil, errors.New("core directory descriptor is not protected root authority")
		}
		if component == "" {
			continue
		}
		next, err := self.OpenDirectory(component)
		self.Close()
		if err != nil {
			return nil, err
		}
		self = next
	}
	info, err := self.file.Stat()
	if err != nil || !protectedDirectoryInfo(info) {
		self.Close()
		return nil, errors.New("core directory leaf is not protected root authority")
	}
	return self, nil
}

func protectedDirectoryInfo(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

// No absolute path, traversal or cleaned alias reaches an openat syscall.
func directoryComponents(value string) ([]string, error) {
	if value == "." {
		return []string{"."}, nil
	}
	if value == "" || filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsRune(value, 0) {
		return nil, errors.New("invalid descriptor-relative core path")
	}
	parts := strings.Split(value, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("core path traverses outside its exact descriptor authority")
		}
	}
	return parts, nil
}

// Ancestors are always real directories. The leaf is never retried on ELOOP.
func (self *directory) OpenFile(value string, flags int, mode os.FileMode) (*os.File, error) {
	parts, err := directoryComponents(value)
	if err != nil || self == nil || self.file == nil {
		return nil, errors.New("missing exact core directory descriptor")
	}
	parent := int(self.file.Fd())
	owned := -1
	defer func() {
		if owned >= 0 {
			syscall.Close(owned)
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		fd, err := syscall.Openat(parent, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		if owned >= 0 {
			syscall.Close(owned)
		}
		parent, owned = fd, fd
	}
	fd, err := syscall.Openat(parent, parts[len(parts)-1], flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "core-relative-file"), nil
}

func (self *directory) OpenDirectory(value string) (*directory, error) {
	file, err := self.OpenFile(value, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	return &directory{file: file}, nil
}

// Every directory scan receives a fresh offset, not a dup sharing seek state.
func (self *directory) Open(value string) (*os.File, error) {
	return self.OpenFile(value, os.O_RDONLY|syscall.O_DIRECTORY, 0)
}

// O_PATH|O_NOFOLLOW observes a symlink as a symlink; it never dereferences it.
func (self *directory) Lstat(value string) (os.FileInfo, error) {
	const openPath = 0x200000
	file, err := self.OpenFile(value, openPath, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

func (self *directory) Stat(value string) (os.FileInfo, error) { return self.Lstat(value) }

// Registry mutations use only fixed, validated plain leaves under one held fd.
func (self *directory) Rename(old, next string) error {
	if filepath.Base(old) != old || filepath.Base(next) != next || old == "." || next == "." || old == ".." || next == ".." {
		return errors.New("invalid exact core registry rename")
	}
	return syscall.Renameat(int(self.file.Fd()), old, int(self.file.Fd()), next)
}

func (self *directory) Remove(value string) error {
	if filepath.Base(value) != value || value == "." || value == ".." {
		return errors.New("invalid exact core registry unlink")
	}
	name, err := syscall.BytePtrFromString(value)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, self.file.Fd(), uintptr(unsafe.Pointer(name)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func (self *directory) Close() error { return self.file.Close() }
