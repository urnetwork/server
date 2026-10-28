// Protected, bounded filesystem operations retain every partial output on failure.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Digests have one lowercase canonical spelling.
func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// Byte identity never interprets the underlying private data.
func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// Canonical relative names refuse aliases, traversal, backslashes, and control bytes.
func validMember(name string) bool {
	if name == "" || name == "." || name == ".." || path.Clean(name) != name ||
		path.IsAbs(name) || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
		return false
	}
	for _, character := range name {
		if character < 32 || character == 127 {
			return false
		}
	}
	return true
}

// Every protected component is owned and non-writable by other principals.
func checkPath(value string, policy archivePolicy, missingLeaf bool) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value ||
		!filepath.IsAbs(policy.Anchor) || filepath.Clean(policy.Anchor) != policy.Anchor {
		return errors.New("canonical protected path required")
	}
	relative, err := filepath.Rel(policy.Anchor, value)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("protected path escapes its anchor")
	}
	current := policy.Anchor
	parts := []string{}
	if relative != "." {
		parts = strings.Split(relative, string(filepath.Separator))
	}
	for index := 0; index <= len(parts); index++ {
		if index > 0 {
			current = filepath.Join(current, parts[index-1])
		}
		info, err := os.Lstat(current)
		if missingLeaf && index == len(parts) && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return errors.New("protected path unavailable")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != policy.OwnerUid || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0022 != 0 || (!info.IsDir() && !info.Mode().IsRegular()) ||
			(index < len(parts) && !info.IsDir()) || (info.Mode().IsRegular() && stat.Nlink != 1) {
			return errors.New("unprotected, linked, or nonregular path refused")
		}
	}
	return nil
}

// Plans, inventories and tars are private leaves; source privacy follows its 0700 root.
func openProtected(value string, policy archivePolicy, private bool) (*os.File, error) {
	if err := checkPath(value, policy, false); err != nil {
		return nil, err
	}
	before, err := os.Lstat(value)
	if err != nil || !before.Mode().IsRegular() || (private && before.Mode().Perm()&0077 != 0) {
		return nil, errors.New("private regular input required")
	}
	fd, err := syscall.Open(value, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("protected input open refused")
	}
	file := os.NewFile(uintptr(fd), value)
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		file.Close()
		return nil, errors.New("protected input changed during open")
	}
	return file, nil
}

// An open inode and its pathname must remain identical through an opaque read.
func recheckFile(file *os.File, before os.FileInfo) error {
	after, err := file.Stat()
	current, pathErr := os.Lstat(file.Name())
	if err != nil || pathErr != nil || !os.SameFile(before, after) || !os.SameFile(before, current) ||
		after.Size() != before.Size() || after.ModTime() != before.ModTime() ||
		after.Mode() != before.Mode() || current.Mode() != after.Mode() ||
		current.Size() != after.Size() || current.ModTime() != after.ModTime() {
		return errors.New("protected input changed during read")
	}
	beforeStat, beforeOk := before.Sys().(*syscall.Stat_t)
	afterStat, afterOk := after.Sys().(*syscall.Stat_t)
	currentStat, currentOk := current.Sys().(*syscall.Stat_t)
	if !beforeOk || !afterOk || !currentOk || beforeStat.Uid != afterStat.Uid ||
		afterStat.Uid != currentStat.Uid || beforeStat.Gid != afterStat.Gid ||
		afterStat.Gid != currentStat.Gid || beforeStat.Nlink != afterStat.Nlink ||
		afterStat.Nlink != currentStat.Nlink {
		return errors.New("protected input ownership or links changed during read")
	}
	return nil
}

// Reads metadata only; larger opaque payloads always use streaming hashes.
func readPinned(pin filePin, policy archivePolicy, settings archiveSettings) ([]byte, error) {
	if !validDigest(pin.Sha256) {
		return nil, errors.New("canonical metadata pin required")
	}
	file, err := openProtected(pin.Path, policy, true)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || before.Size() > settings.MaximumMetadataBytes {
		return nil, errors.New("metadata exceeds its bound")
	}
	content, err := io.ReadAll(io.LimitReader(file, settings.MaximumMetadataBytes+1))
	if err != nil || int64(len(content)) > settings.MaximumMetadataBytes || digestBytes(content) != pin.Sha256 {
		return nil, errors.New("metadata digest or size mismatch")
	}
	if err := recheckFile(file, before); err != nil {
		return nil, err
	}
	return content, nil
}

// Canonical recursive keys prevent Go's case-folded approval/config aliases.
func decodeMetadata(content []byte, target any, settings archiveSettings) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	var visit func(int) error
	visit = func(depth int) error {
		if depth > settings.MaximumMetadataDepth {
			return errors.New("metadata nesting exceeds bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid metadata")
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || name == "" || name[0] < 'a' || name[0] > 'z' || seen[name] {
					return errors.New("duplicate or noncanonical metadata key")
				}
				for _, character := range name {
					if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_') {
						return errors.New("noncanonical metadata key")
					}
				}
				seen[name] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid metadata delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing metadata refused")
	}
	decoder = json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("metadata schema mismatch")
	}
	return nil
}

// Cancellation remains owned by the operation during every streaming copy.
type contextReader struct {
	Context context.Context
	Reader  io.Reader
}

// Each read checks cancellation without creating a goroutine or retry owner.
func (self contextReader) Read(content []byte) (int, error) {
	if err := self.Context.Err(); err != nil {
		return 0, err
	}
	return self.Reader.Read(content)
}

// Source streams are bounded and checked for inode/size changes after hashing.
func hashSource(ctx context.Context, value string, policy archivePolicy, settings archiveSettings) (archiveEntry, error) {
	file, err := openProtected(value, policy, false)
	if err != nil {
		return archiveEntry{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || before.Size() < 0 || before.Size() > settings.MaximumArchiveBytes {
		return archiveEntry{}, errors.New("source file exceeds archive bound")
	}
	digest := sha256.New()
	size, err := io.CopyBuffer(digest, io.LimitReader(contextReader{Context: ctx, Reader: file}, before.Size()+1), make([]byte, settings.BufferBytes))
	if err != nil || size != before.Size() {
		return archiveEntry{}, errors.New("source file read incomplete or changed")
	}
	if err := recheckFile(file, before); err != nil {
		return archiveEntry{}, err
	}
	return archiveEntry{Bytes: size, Sha256: hex.EncodeToString(digest.Sum(nil))}, nil
}

// Exclusive durable output intentionally remains spent after any partial write.
func writeExclusive(value string, content []byte, policy archivePolicy, settings archiveSettings) error {
	if int64(len(content)) > settings.MaximumMetadataBytes {
		return errors.New("output metadata exceeds bound")
	}
	if err := checkPath(value, policy, true); err != nil {
		return err
	}
	file, err := os.OpenFile(value, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("exclusive metadata creation refused")
	}
	count, writeErr := file.Write(content)
	if count != len(content) {
		writeErr = errors.Join(writeErr, io.ErrShortWrite)
	}
	if err := errors.Join(writeErr, file.Chmod(0400), file.Sync(), file.Close()); err != nil {
		return errors.New("partial metadata retained; no automatic retry")
	}
	return syncDirectory(filepath.Dir(value))
}

// Parent-directory fsync commits each exclusive name before the next operation.
func syncDirectory(value string) error {
	file, err := os.Open(value)
	if err != nil {
		return errors.New("cannot sync metadata directory")
	}
	return errors.Join(file.Sync(), file.Close())
}

// Statfs observes available bytes only; it does not scan or hash the filesystem.
func diskAvailableBytes(value string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(value, &stat); err != nil || stat.Bsize <= 0 ||
		uint64(stat.Bavail) > uint64(math.MaxInt64)/uint64(stat.Bsize) {
		return 0, errors.New("disk headroom unavailable")
	}
	return int64(stat.Bavail) * stat.Bsize, nil
}

// The original native scoring inode is a narrowly retained legacy exception.
// It is never created, changed, or used to relax root-owned evidence paths.
func acquireNativeSlot() (io.Closer, error) {
	const lockPath = "/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock"
	ancestors := []struct {
		path string
		uid  uint32
		mode os.FileMode
	}{
		{path: "/", uid: 0, mode: 0755}, {path: "/home", uid: 0, mode: 0755},
		{path: "/home/by", uid: 1000, mode: 0750},
		{path: "/home/by/urnetwork", uid: 1000, mode: 0775},
		{path: "/home/by/urnetwork/temp", uid: 1000, mode: 0775},
		{path: filepath.Dir(lockPath), uid: 1000, mode: 0700},
	}
	check := func() (os.FileInfo, error) {
		for _, ancestor := range ancestors {
			info, err := os.Lstat(ancestor.path)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
				info.Mode().Perm() != ancestor.mode || info.Sys().(*syscall.Stat_t).Uid != ancestor.uid {
				return nil, errors.New("canonical scoring-lock ancestry changed")
			}
		}
		info, err := os.Lstat(lockPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 0 {
			return nil, errors.New("canonical scoring inode changed")
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != 1000 || uint64(stat.Dev) != 64512 || stat.Ino != 35309334 || stat.Nlink != 1 {
			return nil, errors.New("canonical scoring inode identity changed")
		}
		return info, nil
	}
	before, err := check()
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(lockPath, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("canonical scoring slot unavailable")
	}
	file := os.NewFile(uintptr(fd), lockPath)
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		file.Close()
		return nil, errors.New("canonical scoring slot active or changed")
	}
	current, err := check()
	if err != nil || !os.SameFile(current, opened) {
		file.Close()
		return nil, errors.New("canonical scoring pathname changed while locking")
	}
	return file, nil
}
