//go:build linux || darwin

// Custody files are opened through pinned directory descriptors. Every path
// component refuses symlinks, and publication is atomic and create-only.
package strecovery

import (
	"github.com/urnetwork/connect/durablesys"

	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Parent directories may be shared, but the selected custody object must be
// owned by this effective user, private, regular and singly linked.
func privateInfo(file *os.File, directory bool) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 ||
		directory && !info.IsDir() || !directory && (!info.Mode().IsRegular() || stat.Nlink != 1) {
		return nil, errors.New("custody object must be an owner-private physical directory or singly linked regular file")
	}
	return info, nil
}

// All components are resolved relative to the last admitted descriptor; a
// concurrent path replacement cannot redirect later file operations.
func openPrivatePath(path string, directory bool) (*os.File, error) {
	if !absolutePath(path) {
		return nil, errors.New("custody path must be absolute and canonical")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	components := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for index, component := range components {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index+1 < len(components) || directory {
			flags |= unix.O_DIRECTORY
		}
		nextFd, err := unix.Openat(fd, component, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = nextFd
	}
	file := os.NewFile(uintptr(fd), path)
	if _, err := privateInfo(file, directory); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// Relative names contain no traversal or path separator. Nonblocking open
// prevents a substituted fifo from hanging a bounded collector.
func openPrivateChild(directory *os.File, name string) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, errors.New("custody child name is invalid")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := privateInfo(file, false); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// Size, metadata and cancellation are checked across the bounded read.
func readOpened(ctx context.Context, file *os.File, limit int) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("custody read context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := privateInfo(file, false)
	if err != nil || before.Size() <= 0 || before.Size() > int64(limit) {
		return nil, errors.New("custody file is empty, unavailable or exceeds its byte bound")
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	after, err := privateInfo(file, false)
	if err != nil || int64(len(raw)) != before.Size() || after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		return nil, errors.New("custody file changed during bounded read")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

// Configs, credentials and archives share the same local custody boundary.
func readPrivateFile(ctx context.Context, path string, limit int) (raw []byte, resultErr error) {
	file, err := openPrivatePath(path, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			raw, resultErr = nil, errors.Join(resultErr, err)
		}
	}()
	return readOpened(ctx, file, limit)
}

// A fresh directory handle allows a second complete name census without a
// shared directory offset; limits apply before allocation of a full listing.
func directoryNames(directory *os.File, limit int) ([]string, error) {
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	copy := os.NewFile(uintptr(fd), directory.Name())
	names, readErr := copy.Readdirnames(limit + 1)
	closeErr := copy.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(names) > limit {
		return nil, errors.New("evidence store exceeds its explicit file bound")
	}
	sort.Strings(names)
	return names, nil
}

// Store owners must quiesce writes. Two complete name/byte passes refuse an
// observed change; separate stores are not a distributed atomic snapshot.
func readStore(ctx context.Context, source StoreSource, limits Limits) (result []StoreFile, resultErr error) {
	directory, err := openPrivatePath(source.Directory, true)
	if err != nil {
		return nil, &Refusal{Source: source.Id, Cause: "private evidence directory is unavailable"}
	}
	defer func() {
		if err := directory.Close(); err != nil {
			result, resultErr = nil, errors.Join(resultErr, err)
		}
	}()
	names, err := directoryNames(directory, limits.MaximumAttempts)
	if err != nil {
		return nil, &Refusal{Source: source.Id, Cause: "complete evidence directory census is unavailable or exceeds its bound"}
	}
	result, totalBytes := []StoreFile{}, 0
	for _, name := range names {
		if kind, _ := storeFilename(name); kind == "" {
			return nil, &Refusal{Source: source.Id, Record: name, Cause: "unrecognized evidence filename requires explicit separation before census"}
		}
		file, err := openPrivateChild(directory, name)
		if err != nil {
			return nil, &Refusal{Source: source.Id, Record: name, Cause: "private evidence file is unavailable"}
		}
		raw, readErr := readOpened(ctx, file, limits.MaximumTransactionBytes)
		if err := errors.Join(readErr, file.Close()); err != nil {
			return nil, errors.Join(&Refusal{Source: source.Id, Record: name, Cause: "complete evidence file read failed"}, ctx.Err())
		}
		totalBytes += len(raw)
		if totalBytes > limits.MaximumTotalBytes {
			return nil, &Refusal{Source: source.Id, Record: name, Cause: "evidence store exceeds its explicit byte bound"}
		}
		result = append(result, StoreFile{Name: name, Raw: raw})
	}
	finalNames, err := directoryNames(directory, limits.MaximumAttempts)
	if err != nil || !equalNames(names, finalNames) {
		return nil, &Refusal{Source: source.Id, Cause: "evidence directory changed during census"}
	}
	for _, item := range result {
		file, err := openPrivateChild(directory, item.Name)
		if err != nil {
			return nil, &Refusal{Source: source.Id, Record: item.Name, Cause: "evidence file changed during census"}
		}
		raw, readErr := readOpened(ctx, file, limits.MaximumTransactionBytes)
		if err := errors.Join(readErr, file.Close()); err != nil || !bytes.Equal(raw, item.Raw) {
			return nil, &Refusal{Source: source.Id, Record: item.Name, Cause: "evidence file changed during census"}
		}
	}
	finalNames, err = directoryNames(directory, limits.MaximumAttempts)
	if err != nil || !equalNames(names, finalNames) {
		return nil, &Refusal{Source: source.Id, Cause: "evidence directory changed during verification"}
	}
	return result, ctx.Err()
}

// Publication never opens or truncates an existing final name. The synced
// temporary inode becomes visible in one no-replace rename, then the directory
// is synced. A process interruption can leave only unreferenced temp evidence.
func publishFile(ctx context.Context, directory *os.File, name string, raw []byte) (created bool, resultErr error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return false, errors.New("custody publication name is invalid")
	}
	existing, err := openPrivateChild(directory, name)
	if err == nil {
		value, readErr := readOpened(ctx, existing, len(raw))
		if err := errors.Join(readErr, existing.Close()); err != nil {
			return false, err
		}
		if !bytes.Equal(raw, value) {
			return false, errors.New("existing custody file conflicts; exact original bytes were preserved")
		}
		return false, directory.Sync()
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return false, err
	}
	temporary := ".strecovery-" + hex.EncodeToString(random[:]) + ".tmp"
	fd, err := unix.Openat(int(directory.Fd()), temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), temporary)
	defer func() {
		file.Close()
		if err := unix.Unlinkat(int(directory.Fd()), temporary, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			resultErr = errors.Join(resultErr, errors.New("custody temporary cleanup failed"))
		}
	}()
	if _, err := file.Write(raw); err != nil {
		return false, err
	}
	if err := file.Chmod(0400); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := durablesys.RenameNoReplace(int(directory.Fd()), temporary, int(directory.Fd()), name); err != nil {
		return false, err
	}
	return true, directory.Sync()
}

// Nonblocking flock coordinates other recovery processes using this directory
// descriptor without leaving a lock file in the signature store.
func lockDirectory(directory *os.File) (func(), error) {
	if err := unix.Flock(int(directory.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.New("another recovery owner holds the destination directory")
	}
	return func() { unix.Flock(int(directory.Fd()), unix.LOCK_UN) }, nil
}

// Directory names have already been sorted and bounded.
func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
