// Native control files are fixed protected metadata, never the core stream.
// Tests use the same descriptor-relative file code under private directories.
package competitioncore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Every native authority ancestor is root-owned, non-writable by other users
// and non-symlinked. Registry parents must already exist; this creates none.
func protectedPath(value string, directory bool) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return errors.New("invalid protected core authority path")
	}
	for current := value; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("unprotected core authority path")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || (current != value || directory) && !info.IsDir() || current == value && !directory && !info.Mode().IsRegular() {
			return errors.New("core authority is not an exact root-owned object")
		}
		if current == "/" {
			return nil
		}
	}
}

// O_NONBLOCK prevents a substituted FIFO from parking the handler before stat.
func readRelative(root *directory, name string, maximum int64, protected bool) ([]byte, error) {
	if root == nil || maximum <= 0 || maximum >= int64(^uint64(0)>>1) || strings.ContainsRune(name, 0) {
		return nil, errors.New("invalid bounded core metadata read")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("core metadata is not a finite regular file")
	}
	if protected {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return nil, errors.New("core metadata descriptor lost protected ownership")
		}
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, errors.New("core metadata exceeds its finite read bound")
	}
	return content, nil
}

// The final component is opened beneath a held protected parent descriptor.
func readProtected(value string, maximum int64) ([]byte, error) {
	if err := protectedPath(value, false); err != nil {
		return nil, err
	}
	root, err := openDirectory(filepath.Dir(value), true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readRelative(root, filepath.Base(value), maximum, true)
}

// Device/inode identity is retained across all relative proc/cgroup reads.
type fileIdentity struct{ device, inode uint64 }

// A missing stat is unknown rather than an invented process generation.
func identity(info os.FileInfo) (fileIdentity, error) {
	if info == nil {
		return fileIdentity{}, errors.New("missing core authority identity")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, errors.New("missing native core authority identity")
	}
	return fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}
