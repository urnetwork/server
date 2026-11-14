//go:build linux

// Native writes address one held, freshly rechecked engine cgroup descriptor.
// The owner never writes limits, global cache controls or evaluation cgroups.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Fixed kernel/control identities, not configurable targets or policy tunables.
const (
	cachePrivatePath = "/sys/fs/cgroup/urnetwork.slice/urnetwork-management.slice/urnetwork-management-private.slice"
	cacheEngineName  = "urnetwork-private-engine.service"
	cacheJournalPath = "/var/lib/urnetwork/competition-management-cache"
	cacheCgroupMagic = 0x63677270
)

// Traversal refuses aliases and externally writable authority parents.
func openProtectedCacheDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("cache authority requires a canonical absolute directory")
	}
	current, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" {
			continue
		}
		fd, openErr := syscall.Openat(int(current.Fd()), component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = current.Close()
		if openErr != nil {
			return nil, openErr
		}
		current = os.NewFile(uintptr(fd), component)
		info, statErr := current.Stat()
		stat, valid := func() (*syscall.Stat_t, bool) {
			if info == nil {
				return nil, false
			}
			value, ok := info.Sys().(*syscall.Stat_t)
			return value, ok
		}()
		if statErr != nil || !valid || stat.Uid != 0 || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			current.Close()
			return nil, errors.New("cache authority directory is not protected root custody")
		}
	}
	return current, nil
}

// All leaves are bounded, no-follow regular files inside held directories.
func readCacheFile(directory *os.File, name string, maximum int64) ([]byte, error) {
	fd, err := syscall.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("cache observation is not a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, errors.New("cache observation exceeded its document bound")
	}
	return content, nil
}

// Kernel values are canonical nonnegative decimal integers, never unlimited.
func cacheNumber(content []byte) (int64, error) {
	value := strings.TrimSuffix(string(content), "\n")
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 || strconv.FormatInt(number, 10) != value {
		return 0, errors.New("cache observation is not a canonical finite counter")
	}
	return number, nil
}

// Duplicate, omitted and malformed counters cannot become positive cache evidence.
func cacheMemoryCounters(content []byte, maximum int) (map[string]int64, error) {
	values := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(values) >= maximum {
			return nil, errors.New("cache memory statistics exceeded their closed bound")
		}
		if _, duplicate := values[fields[0]]; duplicate {
			return nil, errors.New("cache memory statistics repeat a counter")
		}
		value, err := cacheNumber([]byte(fields[1]))
		if err != nil {
			return nil, err
		}
		values[fields[0]] = value
	}
	for _, name := range []string{"file", "inactive_file", "shmem", "file_dirty", "file_writeback"} {
		if _, present := values[name]; !present {
			return nil, errors.New("cache memory statistics omitted a required file counter")
		}
	}
	return values, nil
}

// Production construction always selects the fixed kernel path and filesystem.
type nativeCacheKernel struct {
	settings      cacheSettings
	parent        *os.File
	engine        *os.File
	parentPath    string
	requireNative bool
}

// No root operation, native-path waiver or target is accepted from CLI input.
func newNativeCacheKernel(settings cacheSettings) (*nativeCacheKernel, error) {
	parent, err := openProtectedCacheDirectory(cachePrivatePath)
	if err != nil {
		return nil, err
	}
	engine, err := openProtectedCacheDirectory(filepath.Join(cachePrivatePath, cacheEngineName))
	if err != nil {
		parent.Close()
		return nil, err
	}
	return &nativeCacheKernel{settings: settings, parent: parent, engine: engine, parentPath: cachePrivatePath, requireNative: true}, nil
}

// Both held descriptors remain owned until the one pass returns.
func (self *nativeCacheKernel) Close() error {
	return errors.Join(self.engine.Close(), self.parent.Close())
}

// Reopening the exact namespace path rejects stale/deleted cgroup generations.
func (self *nativeCacheKernel) verifyGeneration() error {
	for _, item := range []struct {
		path string
		held *os.File
	}{{path: self.parentPath, held: self.parent}, {path: filepath.Join(self.parentPath, cacheEngineName), held: self.engine}} {
		fd, err := syscall.Open(item.path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		fresh := os.NewFile(uintptr(fd), item.path)
		before, firstErr := item.held.Stat()
		after, secondErr := fresh.Stat()
		_ = fresh.Close()
		if firstErr != nil || secondErr != nil || !os.SameFile(before, after) {
			return errors.New("cache cgroup generation changed")
		}
		if self.requireNative {
			var filesystem syscall.Statfs_t
			if err := syscall.Fstatfs(int(item.held.Fd()), &filesystem); err != nil || filesystem.Type != cacheCgroupMagic {
				return errors.New("cache authority is not the native cgroup filesystem")
			}
		}
	}
	return nil
}

// Only systemd's empty control leaf may descend from the daemon. Active
// evaluation, evidence or build groups are never traversed or reclaimed.
func (self *nativeCacheKernel) verifyChildren() error {
	fd, err := syscall.Openat(int(self.engine.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), "engine-census")
	defer directory.Close()
	entries, err := directory.ReadDir(self.settings.MaximumCgroupEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > self.settings.MaximumCgroupEntries {
		return errors.New("cache engine census exceeded its bounded work")
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("cache engine contains an aliased control leaf")
		}
		if !entry.IsDir() {
			continue
		}
		if entry.Name() != ".control" {
			return errors.New("cache engine has an unapproved descendant")
		}
		childFd, err := syscall.Openat(int(self.engine.Fd()), entry.Name(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		child := os.NewFile(uintptr(childFd), "engine-control")
		content, readErr := readCacheFile(child, "cgroup.events", self.settings.DocumentBytes)
		_ = child.Close()
		if readErr != nil || !strings.Contains("\n"+string(content), "\npopulated 0\n") {
			return errors.New("cache engine control descendant is populated or unknown")
		}
	}
	return nil
}

// No engine shmem or swap may be targeted, even if other file pages look idle.
func (self *nativeCacheKernel) Observe(ctx context.Context) (cacheSnapshot, error) {
	var snapshot cacheSnapshot
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	if err := self.verifyGeneration(); err != nil {
		return snapshot, err
	}
	if err := self.verifyChildren(); err != nil {
		return snapshot, err
	}
	for _, directory := range []*os.File{self.parent, self.engine} {
		content, err := readCacheFile(directory, "cgroup.type", self.settings.DocumentBytes)
		if err != nil || string(content) != "domain\n" {
			return snapshot, errors.New("cache maintenance requires the original domain cgroup boundary")
		}
	}
	readNumber := func(directory *os.File, name string) (int64, error) {
		content, err := readCacheFile(directory, name, self.settings.DocumentBytes)
		if err != nil {
			return 0, err
		}
		return cacheNumber(content)
	}
	var err error
	snapshot.PrivateMaximumBytes, err = readNumber(self.parent, "memory.max")
	if err != nil {
		return snapshot, err
	}
	snapshot.PrivateCurrentBytes, err = readNumber(self.parent, "memory.current")
	if err != nil {
		return snapshot, err
	}
	for _, item := range []struct {
		directory *os.File
		name      string
	}{{directory: self.parent, name: "memory.swap.max"}, {directory: self.engine, name: "memory.swap.current"}} {
		value, err := readNumber(item.directory, item.name)
		if err != nil || value != 0 {
			return snapshot, errors.New("cache maintenance requires the original zero-swap boundary")
		}
	}
	parentCpu, err := readCacheFile(self.parent, "cpuset.cpus.effective", self.settings.DocumentBytes)
	if err != nil {
		return snapshot, err
	}
	engineCpu, err := readCacheFile(self.engine, "cpuset.cpus.effective", self.settings.DocumentBytes)
	if err != nil || len(bytes.TrimSpace(parentCpu)) == 0 || !bytes.Equal(parentCpu, engineCpu) {
		return snapshot, errors.New("cache engine escaped its existing management CPU boundary")
	}
	content, err := readCacheFile(self.engine, "memory.stat", self.settings.DocumentBytes)
	if err != nil {
		return snapshot, err
	}
	counters, err := cacheMemoryCounters(content, self.settings.MaximumStatKeys)
	if err != nil {
		return snapshot, err
	}
	snapshot.EngineFileBytes, snapshot.InactiveFileBytes, snapshot.ShmemBytes = counters["file"], counters["inactive_file"], counters["shmem"]
	snapshot.DirtyBytes, snapshot.WritebackBytes = counters["file_dirty"], counters["file_writeback"]
	if _, err := snapshot.headroom(); err != nil {
		return snapshot, err
	}
	if err := self.verifyGeneration(); err != nil {
		return snapshot, err
	}
	return snapshot, ctx.Err()
}

// The pre-issue re-observation closes cache/cgroup changes since journaling.
// A kernel write may reclaim less than requested; only the later observation counts.
func (self *nativeCacheKernel) Reclaim(ctx context.Context, amount int64) (bool, error) {
	snapshot, err := self.Observe(ctx)
	if err != nil {
		return false, err
	}
	headroom, err := snapshot.headroom()
	if err != nil {
		return false, err
	}
	if headroom >= self.settings.TargetHeadroomBytes {
		return false, nil
	}
	if amount <= 0 || amount > self.settings.MaximumReclaimBytes || amount > snapshot.inactiveCleanBytes() || amount%int64(os.Getpagesize()) != 0 {
		return false, errors.New("cache reclaim no longer has its positive bounded inactive-file proof")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	fd, err := syscall.Openat(int(self.engine.Fd()), "memory.reclaim", syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), "engine-memory-reclaim")
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		file.Close()
		return false, errors.New("cache reclaim control is not a regular kernel file")
	}
	if self.requireNative {
		var filesystem syscall.Statfs_t
		if err := syscall.Fstatfs(fd, &filesystem); err != nil || filesystem.Type != cacheCgroupMagic {
			file.Close()
			return false, errors.New("cache reclaim control is not on the native cgroup filesystem")
		}
	}
	value := strconv.FormatInt(amount, 10)
	count, writeErr := file.WriteString(value)
	closeErr := file.Close()
	if writeErr == nil && count != len(value) {
		writeErr = io.ErrShortWrite
	}
	return true, errors.Join(writeErr, closeErr, ctx.Err())
}

// One root-owned lock serializes timer and manual passes without waiting.
type nativeCacheJournal struct {
	directory *os.File
	lock      *os.File
	settings  cacheSettings
}

// Journal custody is independent of agent admission, workload data and credentials.
func newNativeCacheJournal(settings cacheSettings) (*nativeCacheJournal, error) {
	parent, err := openProtectedCacheDirectory(filepath.Dir(cacheJournalPath))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err := syscall.Mkdirat(int(parent.Fd()), filepath.Base(cacheJournalPath), 0700); err != nil && !errors.Is(err, syscall.EEXIST) {
		return nil, err
	}
	directory, err := openProtectedCacheDirectory(cacheJournalPath)
	if err != nil {
		return nil, err
	}
	info, err := directory.Stat()
	if err != nil || info.Mode().Perm() != 0700 {
		directory.Close()
		return nil, errors.New("cache journal is not private root custody")
	}
	fd, err := syscall.Openat(int(directory.Fd()), "owner.lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		directory.Close()
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "cache-owner-lock")
	info, err = lock.Stat()
	if err != nil {
		lock.Close()
		directory.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if err != nil || !ok || stat.Uid != 0 || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		lock.Close()
		directory.Close()
		return nil, errors.New("cache journal lock has ambiguous custody")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		directory.Close()
		return nil, errors.New("cache maintenance already has an independent owner")
	}
	return &nativeCacheJournal{directory: directory, lock: lock, settings: settings}, nil
}

// Releasing the lock never removes the durable episode debit.
func (self *nativeCacheJournal) Close() error {
	return errors.Join(self.lock.Close(), self.directory.Close())
}

// Missing state is the sole empty journal; malformed or oversized bytes refuse.
func (self *nativeCacheJournal) Load() (cacheState, error) {
	fd, err := syscall.Openat(int(self.directory.Fd()), "state.json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return cacheState{Schema: 1}, nil
	}
	if err != nil {
		return cacheState{}, err
	}
	file := os.NewFile(uintptr(fd), "cache-state")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return cacheState{}, err
	}
	directoryInfo, err := self.directory.Stat()
	if err != nil {
		return cacheState{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	directoryStat, directoryOk := directoryInfo.Sys().(*syscall.Stat_t)
	if !ok || !directoryOk || stat.Uid != directoryStat.Uid || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return cacheState{}, errors.New("cache journal state has ambiguous custody")
	}
	content, err := io.ReadAll(io.LimitReader(file, self.settings.DocumentBytes+1))
	if err != nil || int64(len(content)) > self.settings.DocumentBytes {
		return cacheState{}, errors.New("cache journal state exceeds its document bound")
	}
	var state cacheState
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return state, errors.New("cache journal has trailing content")
	}
	return state, nil
}

// Atomic replacement and both fsyncs precede the corresponding kernel issue.
func (self *nativeCacheJournal) Save(state cacheState) error {
	content, err := json.Marshal(state)
	if err != nil || int64(len(content)) > self.settings.DocumentBytes {
		return errors.New("cache journal exceeds its document bound")
	}
	// An interrupted commit leaves one inspectable unknown leaf. Exclusive
	// creation refuses a crash loop instead of accumulating orphaned files.
	name := "state.pending"
	fd, err := syscall.Openat(int(self.directory.Fd()), name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = syscall.Unlinkat(int(self.directory.Fd()), name) }()
	count, writeErr := file.Write(content)
	if writeErr == nil && count != len(content) {
		writeErr = io.ErrShortWrite
	}
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := syscall.Renameat(int(self.directory.Fd()), name, int(self.directory.Fd()), "state.json"); err != nil {
		return err
	}
	return self.directory.Sync()
}
