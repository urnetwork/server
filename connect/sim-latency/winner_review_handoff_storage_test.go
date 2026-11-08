// The native test-driver allocates real disk blocks before any provider draw.
// It releases only its own empty reservation as bounded writes consume allowance;
// no original wrapper journal, archive object or candidate artifact is removed.
package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Single-owner only: all calls are sequential; Close runs after transport joins.
type winnerHandoffStorage struct {
	file                *os.File
	path                string
	allocated, consumed int64
	blockSize           int64
	device, inode       uint64
}

func readWinnerHandoffMemory() (string, int64, int64, int64, error) {
	content, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", 0, 0, 0, err
	}
	path := ""
	for _, line := range strings.Split(string(content), "\n") {
		if relative, found := strings.CutPrefix(line, "0::"); found && filepath.IsAbs(relative) && filepath.Clean(relative) == relative {
			path = filepath.Join("/sys/fs/cgroup", relative)
		}
	}
	if path == "" {
		return "", 0, 0, 0, errors.New("native review requires a current cgroup-v2 memory owner")
	}
	read := func(name string) (int64, error) {
		content, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	}
	limit, err := read("memory.max")
	if err != nil {
		return "", 0, 0, 0, errors.New("native review memory.max must be an actual finite hard limit")
	}
	current, err := read("memory.current")
	if err != nil {
		return "", 0, 0, 0, err
	}
	info, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "", 0, 0, 0, err
	}
	available := int64(0)
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			value, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if parseErr != nil || value < 0 || value > (1<<63-1)/1024 {
				return "", 0, 0, 0, errors.New("native available memory is malformed")
			}
			available = value * 1024
		}
	}
	return path, limit, current, available, nil
}

// The capture directory, wrapper/member-state roots and temporary archive root
// must all live on this actual reserved filesystem. Unknown extra roots refuse.
func reserveWinnerHandoffStorage(directory string, paths []string, capacity *winnerMultipassCapacity) (*winnerHandoffStorage, winnerHandoffCapacityReservation, error) {
	if capacity == nil || capacity.RequiredDurableBytes <= 0 {
		return nil, winnerHandoffCapacityReservation{}, errors.New("review storage census is missing")
	}
	cgroup, limit, current, available, err := readWinnerHandoffMemory()
	if err != nil {
		return nil, winnerHandoffCapacityReservation{}, err
	}
	encoded, _ := json.Marshal(capacity)
	proof := winnerHandoffCapacityReservation{CapacitySha256: winnerMultipassSha256(encoded), MemoryCgroup: cgroup, MemoryLimitBytes: limit, MemoryCurrentBytes: current, HostAvailableBytes: available, StoragePaths: append([]string{}, paths...)}
	if limit > capacity.Settings.MaximumMemoryBytes || current < 0 || limit-current < capacity.RequiredMemoryBytes || available < capacity.RequiredMemoryBytes {
		return nil, proof, errors.New("native cgroup or available host memory cannot own the fixed evidence budget")
	}
	storage, err := allocateWinnerHandoffStorage(directory, paths, capacity.RequiredDurableBytes)
	if err != nil {
		return nil, proof, err
	}
	proof.ReservationPath, proof.ReservationInode, proof.Device, proof.ReservedBytes = storage.path, storage.inode, storage.device, storage.allocated
	if err := validateWinnerHandoffReservation(capacity, proof); err != nil {
		storage.Close()
		return nil, proof, err
	}
	return storage, proof, nil
}

func allocateWinnerHandoffStorage(directory string, paths []string, required int64) (*winnerHandoffStorage, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || required <= 0 || len(paths) == 0 {
		return nil, errors.New("invalid private storage reservation")
	}
	var device uint64
	for index, path := range append([]string{directory}, paths...) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path {
			return nil, errors.New("review storage root is not canonical")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("review storage root is not private")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) {
			return nil, errors.New("review storage root is not owned by the native process")
		}
		if index == 0 {
			device = uint64(stat.Dev)
		} else if device != uint64(stat.Dev) {
			return nil, errors.New("review storage roots span unreserved filesystems")
		}
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs(directory, &filesystem); err != nil {
		return nil, err
	}
	block := int64(filesystem.Bsize)
	if block <= 0 || required > (1<<63-1)-block {
		return nil, errors.New("review allocation size is invalid")
	}
	allocated := (required + block - 1) / block * block
	if uint64(allocated/block) > filesystem.Bavail {
		return nil, errors.New("actual filesystem lacks the complete review reservation")
	}
	path := filepath.Join(directory, "capacity.reservation")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	storage := &winnerHandoffStorage{file: file, path: path, allocated: allocated, blockSize: block, device: device}
	if err := unix.Fallocate(int(file.Fd()), 0, 0, allocated); err != nil {
		file.Close()
		os.Remove(path)
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		os.Remove(path)
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks*512 < allocated || stat.Nlink != 1 {
		file.Close()
		os.Remove(path)
		return nil, errors.New("filesystem did not allocate the promised review blocks")
	}
	storage.inode = stat.Ino
	return storage, nil
}

// Punch whole blocks only after adding the exact allowance. The final partial
// block remains reserved until its full allowance or final Close is reached.
func (self *winnerHandoffStorage) Consume(bytes int64) error {
	if self == nil || self.file == nil || bytes <= 0 || bytes > self.allocated-self.consumed {
		return errors.New("review attempted to spend unreserved capacity")
	}
	before := self.consumed / self.blockSize * self.blockSize
	after := (self.consumed + bytes) / self.blockSize * self.blockSize
	if after > before {
		if err := unix.Fallocate(int(self.file.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, before, after-before); err != nil {
			return err
		}
	}
	self.consumed += bytes
	return nil
}

func (self *winnerHandoffStorage) Close() error {
	if self == nil || self.file == nil {
		return nil
	}
	closeErr := self.file.Close()
	self.file = nil
	info, err := os.Lstat(self.path)
	if err != nil {
		return errors.Join(closeErr, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino != self.inode || uint64(stat.Dev) != self.device || stat.Nlink != 1 {
		return errors.Join(closeErr, errors.New("review reservation identity changed; refusing removal"))
	}
	return errors.Join(closeErr, os.Remove(self.path))
}

func TestWinnerHandoffStorageAllocatesAndConsumesOnlyItsOwnReservation(t *testing.T) {
	directory := t.TempDir()
	storage, err := allocateWinnerHandoffStorage(directory, []string{directory}, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err := storage.Consume(4096); err != nil {
		t.Fatal(err)
	}
	if err := storage.Consume(storage.allocated); err == nil {
		t.Fatal("overspent immutable reservation")
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "capacity.reservation")); !os.IsNotExist(err) {
		t.Fatal("owned empty reservation remained", err)
	}
}

func TestWinnerHandoffStorageRejectsAliasAndNeverReplacesExistingReservation(t *testing.T) {
	directory := t.TempDir()
	storage, err := allocateWinnerHandoffStorage(directory, []string{directory}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := allocateWinnerHandoffStorage(directory, []string{directory}, 4096); err == nil {
		t.Fatal("existing reservation was replaced")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := allocateWinnerHandoffStorage(alias, []string{alias}, 4096); err == nil {
		t.Fatal("symlink storage accepted")
	}
}
