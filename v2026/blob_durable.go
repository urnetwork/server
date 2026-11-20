// Explicit local service storage uses the shared physical-volume owner. Legacy
// development constructors keep their separate API; no configured service can
// select them by omitting or corrupting a declaration.
package server

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026/durablevolume"
	"golang.org/x/sys/unix"
)

// Callers may reopen the original declared store and read back the exact key.
// The same batch cannot blindly retry an unacknowledged publication.
var ErrLocalBlobPublicationUncertain = errors.New("local blob publication uncertain; close and reconcile original objects")

// Opening validates an already provisioned root without creating one. Per-call
// owners close with the copy, list, batch, reaper or returned reader, so loading
// a store for a request cannot leak a daemon-lifetime descriptor or lease.
func NewDurableLocalBlobStore(ctx context.Context, root, prefix string, maximum int64, reference durablevolume.Reference) (BlobStore, error) {
	store, err := newDurableLocalBlobStore(ctx, root, prefix, maximum, reference, nil)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// Only in-package deterministic tests supply kernel facts. Configuration and
// the exported constructor always use the real host implementation.
func newDurableLocalBlobStore(ctx context.Context, root, prefix string, maximum int64, reference durablevolume.Reference, host durablevolume.Host) (*localBlobStore, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || maximum <= 0 {
		return nil, errors.New("explicit local blob storage needs a canonical non-root path and positive allocation")
	}
	self := NewLocalBlobStoreWithMaxBytes(root, prefix, maximum).(*localBlobStore)
	self.durableReference, self.durableHostForTest = &reference, host
	owner, err := self.openDurableVolume(ctx, durablevolume.ReadOnly)
	if err != nil {
		return nil, err
	}
	directory, err := owner.OpenDirectory("", false)
	if err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	self.durableRootInfo, err = directory.Stat()
	if err := errors.Join(err, owner.CheckDirectory("", directory), directory.Close(), owner.Close()); err != nil {
		return nil, err
	}
	return self, nil
}

// Only construction can capture the initial descriptor. Subsequent operations
// compare against it, in addition to the external declaration's generation.
func (self *localBlobStore) openDurable(ctx context.Context, access durablevolume.Access) (*durablevolume.Owner, error) {
	self.stateLock.Lock()
	failure := self.durableFailure
	self.stateLock.Unlock()
	if failure != nil {
		return nil, failure
	}
	if self.durableRootInfo == nil {
		return nil, errors.New("durable local blob store has no admitted root generation")
	}
	owner, err := self.openDurableVolume(ctx, access)
	if err != nil {
		return nil, self.retainDurableFailure(err)
	}
	directory, err := owner.OpenDirectory("", false)
	if err != nil {
		return nil, self.retainDurableFailure(errors.Join(err, owner.Close()))
	}
	info, statErr := directory.Stat()
	if statErr == nil && !os.SameFile(info, self.durableRootInfo) {
		statErr = errors.Join(durablevolume.ErrIdentity, errors.New("local blob root differs from the original store generation"))
	}
	err = errors.Join(statErr, owner.CheckDirectory("", directory), directory.Close())
	// Concurrent opens share only the sticky verdict, never a filesystem call
	// under the state lock. A prior proven loss wins over this observation.
	err = self.retainDurableFailure(err)
	if err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	return owner, nil
}

func (self *localBlobStore) retainDurableFailure(err error) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.durableFailure == nil && errors.Is(err, durablevolume.ErrIdentity) {
		self.durableFailure = err
	}
	if self.durableFailure != nil {
		return self.durableFailure
	}
	return err
}

// Every fresh owner authenticates the external policy; a previous operation's
// successful admission cannot authorize a later mount or declaration change.
func (self *localBlobStore) openDurableVolume(ctx context.Context, access durablevolume.Access) (*durablevolume.Owner, error) {
	if ctx == nil || self.durableReference == nil {
		return nil, errors.New("local blob durable declaration or context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self.durableHostForTest != nil {
		return durablevolume.OpenWithHost(*self.durableReference, self.root, access, self.durableHostForTest)
	}
	return durablevolume.Open(*self.durableReference, self.root, access)
}

// Capacity admission retains both the mount lease and the original local
// quota-lock inode. Missing approved roots never become mkdir opportunities.
func (self *localBlobStore) acquireDurableCapacity(ctx context.Context, wait bool) (_ *localBlobCapacityOwner, resultErr error) {
	volume, err := self.openDurable(ctx, durablevolume.ReadWrite)
	if err != nil {
		return nil, err
	}
	owner := &localBlobCapacityOwner{root: self.root, volume: volume}
	transferred := false
	defer func() {
		if !transferred {
			if owner.file != nil {
				resultErr = errors.Join(resultErr, owner.Close())
			} else {
				if owner.directory != nil {
					resultErr = errors.Join(resultErr, owner.directory.Close())
				}
				resultErr = errors.Join(resultErr, volume.Close())
			}
		}
	}()
	owner.directory, err = volume.OpenDirectory("", false)
	if err != nil {
		return nil, err
	}
	owner.rootInfo, err = owner.directory.Stat()
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(owner.directory.Fd()), localBlobCapacityLockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	owner.file = os.NewFile(uintptr(fd), filepath.Join(self.root, localBlobCapacityLockName))
	if err := owner.directory.Sync(); err != nil {
		return nil, err
	}
	for {
		if err := errors.Join(ctx.Err(), owner.check()); err != nil {
			return nil, err
		}
		locked, err := localBlobCapacityTryLock(owner.file)
		if err != nil {
			return nil, err
		}
		if locked {
			if err := errors.Join(ctx.Err(), owner.check()); err != nil {
				return nil, err
			}
			transferred = true
			return owner, nil
		}
		if self.afterCapacityContentionForTest != nil {
			self.afterCapacityContentionForTest()
		}
		if !wait {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Resource observation failures remain retryable. Proven quota-lock identity
// loss and an uncertain publication cannot be repaired within this batch.
func (self *localBlobCapacityOwner) checkDurable() error {
	if self.failed != nil {
		return self.failed
	}
	if self.directory == nil || self.file == nil {
		return errors.New("durable local blob owner is closed")
	}
	if err := self.volume.CheckDirectory("", self.directory); err != nil {
		return err
	}
	var opened, named unix.Stat_t
	if err := unix.Fstat(int(self.file.Fd()), &opened); err != nil {
		return err
	}
	if err := unix.Fstatat(int(self.directory.Fd()), localBlobCapacityLockName, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			self.failed = errors.Join(durablevolume.ErrIdentity, errors.New("original blob quota owner disappeared"), err)
			return self.failed
		}
		return err
	}
	if opened.Dev != named.Dev || opened.Ino != named.Ino || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&0077 != 0 || named.Size != 0 || named.Nlink != 1 {
		self.failed = errors.Join(durablevolume.ErrIdentity, errors.New("original blob quota owner changed"))
		return self.failed
	}
	return self.volume.CheckWrite()
}

// A bounded descriptor traversal is shared by accounting, lists and reaping.
// Every opened descendant remains physically tied to the declared root.
func walkDurableBlobs(ctx context.Context, volume *durablevolume.Owner, maximum int, visit func(*os.File, string, string, unix.Stat_t) error) error {
	count := 0
	var walk func(string, int) error
	walk = func(relative string, depth int) (resultErr error) {
		if depth > 64 || count > maximum {
			return errLocalBlobListScanLimitExceeded
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		directory, err := volume.OpenDirectory(relative, false)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
		for {
			if err := errors.Join(ctx.Err(), volume.CheckDirectory(relative, directory)); err != nil {
				return err
			}
			entries, readErr := directory.ReadDir(128)
			for _, entry := range entries {
				count++
				if count > maximum {
					return errLocalBlobListScanLimitExceeded
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				var state unix.Stat_t
				if err := unix.Fstatat(int(directory.Fd()), entry.Name(), &state, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					return err
				}
				key := filepath.Join(relative, entry.Name())
				if state.Mode&unix.S_IFMT == unix.S_IFDIR {
					if err := walk(key, depth+1); err != nil {
						return err
					}
					continue
				}
				if strings.HasSuffix(entry.Name(), blobPartialSuffix) {
					continue
				}
				if state.Mode&unix.S_IFMT != unix.S_IFREG || state.Size < 0 || state.Nlink != 1 {
					return errors.New("durable blob census contains a nonregular or aliased object")
				}
				if err := visit(directory, entry.Name(), filepath.ToSlash(key), state); err != nil {
					return err
				}
			}
			if err := errors.Join(ctx.Err(), volume.CheckDirectory(relative, directory)); err != nil {
				return err
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	return walk("", 0)
}

func (self *localBlobStore) durableUsage(ctx context.Context, root string) (total int64, resultErr error) {
	if root != self.root {
		return 0, errors.New("durable blob census root differs from its declaration")
	}
	volume, err := self.openDurable(ctx, durablevolume.ReadOnly)
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, volume.Close()) }()
	err = walkDurableBlobs(ctx, volume, maximumLocalBlobListScanEntries, func(_ *os.File, _, key string, state unix.Stat_t) error {
		if self.afterUsageScanEntryForTest != nil {
			self.afterUsageScanEntryForTest(filepath.Join(root, key))
		}
		if state.Size > math.MaxInt64-total {
			return errors.New("durable blob usage exceeds integer range")
		}
		total += state.Size
		return nil
	})
	return total, err
}

func (self *localBlobStore) listDurable(ctx context.Context, prefix, after string, limit, maximum int) (_ []BlobObject, _ bool, resultErr error) {
	volume, err := self.openDurable(ctx, durablevolume.ReadOnly)
	if err != nil {
		return nil, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, volume.Close()) }()
	candidates := &blobObjectMaxHeap{}
	heap.Init(candidates)
	err = walkDurableBlobs(ctx, volume, maximum, func(_ *os.File, _, key string, state unix.Stat_t) error {
		if self.afterListScanEntryForTest != nil {
			self.afterListScanEntryForTest()
		}
		if strings.HasPrefix(key, prefix) && key > after {
			object := BlobObject{Key: key, Size: state.Size}
			if candidates.Len() < limit+1 {
				heap.Push(candidates, object)
			} else if key < (*candidates)[0].Key {
				heap.Pop(candidates)
				heap.Push(candidates, object)
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	objects := append([]BlobObject(nil), (*candidates)...)
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	more := len(objects) > limit
	if more {
		objects = objects[:limit]
	}
	return objects, more, nil
}

// Guarded immutable publication owns the original capacity inode throughout
// data sync, descriptor-relative link/rename and parent sync acknowledgement.
func (self *localBlobStore) putDurableAtCapacity(ctx context.Context, key, source string, sourceInfo os.FileInfo, absent bool, owner *localBlobCapacityOwner, accounted *int64) (created bool, resultErr error) {
	publicationAttempted := false
	defer func() {
		if resultErr != nil {
			created = false
			if publicationAttempted {
				owner.failed = errors.Join(ErrLocalBlobPublicationUncertain, resultErr)
				resultErr = owner.failed
			}
		}
	}()
	if err := errors.Join(ctx.Err(), owner.check()); err != nil {
		return false, err
	}
	if _, err := localBlobWritePath(owner.root, key); err != nil {
		return false, err
	}
	relative := filepath.Dir(filepath.FromSlash(key))
	if relative == "." {
		relative = ""
	}
	parent, err := owner.volume.OpenDirectory(relative, true)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	name, parentFd := filepath.Base(key), int(parent.Fd())
	var previous unix.Stat_t
	statErr := unix.Fstatat(parentFd, name, &previous, unix.AT_SYMLINK_NOFOLLOW)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	if statErr == nil && (previous.Mode&unix.S_IFMT != unix.S_IFREG || previous.Nlink != 1) {
		return false, errors.New("durable blob destination is not a retained regular object")
	}
	if absent && statErr == nil {
		return false, errors.Join(owner.check(), owner.volume.CheckDirectory(relative, parent))
	}
	usage := int64(0)
	if accounted == nil {
		usage, err = self.durableUsage(ctx, owner.root)
		if err != nil {
			return false, err
		}
	} else {
		usage = *accounted
	}
	replaced := int64(0)
	if statErr == nil {
		replaced = previous.Size
	}
	available, err := localBlobAvailableBytes(usage, replaced, sourceInfo.Size(), self.maxBytes)
	if err != nil {
		return false, err
	}
	input, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, sourceInfo) {
		return false, errors.Join(errors.New("durable blob source changed"), err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return false, err
	}
	stage := ".incoming-" + hex.EncodeToString(nonce[:]) + blobPartialSuffix
	fd, err := unix.Openat(parentFd, stage, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := unix.Unlinkat(parentFd, stage, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	output := os.NewFile(uintptr(fd), stage)
	var staged unix.Stat_t
	if err := unix.Fstat(fd, &staged); err != nil {
		return false, errors.Join(err, output.Close())
	}
	limit := available
	if limit < math.MaxInt64 {
		limit++
	}
	written, copyErr := io.Copy(output, io.LimitReader(&localBlobCapacityReader{ctx: ctx, reader: input}, limit))
	syncErr := output.Sync()
	if syncErr == nil && self.syncFileForTest != nil {
		syncErr = self.syncFileForTest(output)
	}
	if err := errors.Join(copyErr, syncErr, output.Close()); err != nil {
		return false, err
	}
	if _, err := localBlobAvailableBytes(usage, replaced, written, self.maxBytes); err != nil {
		return false, err
	}
	if absent && self.beforeCreateCommitForTest != nil {
		self.beforeCreateCommitForTest()
	}
	if err := errors.Join(ctx.Err(), owner.check(), owner.volume.CheckDirectory(relative, parent)); err != nil {
		return false, err
	}
	var stagedName unix.Stat_t
	if err := unix.Fstatat(parentFd, stage, &stagedName, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, err
	}
	if stagedName.Dev != staged.Dev || stagedName.Ino != staged.Ino {
		return false, errors.Join(durablevolume.ErrIdentity, errors.New("durable blob staged object changed before publication"))
	}
	var predecessor unix.Stat_t
	err = unix.Fstatat(parentFd, name, &predecessor, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) && !errors.Is(err, unix.ELOOP) {
		return false, errors.Join(&durablevolume.UnavailableError{Reason: "cannot observe blob publication predecessor"}, err)
	}
	if statErr == nil {
		if err != nil || predecessor.Dev != previous.Dev || predecessor.Ino != previous.Ino || predecessor.Size != previous.Size {
			return false, errors.Join(durablevolume.ErrIdentity, errors.New("durable blob predecessor changed before publication"), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, errors.Join(errors.New("durable blob destination appeared before publication"), err)
	}
	publicationAttempted = true
	if absent {
		err = unix.Linkat(parentFd, stage, parentFd, name, 0)
		if errors.Is(err, os.ErrExist) {
			publicationAttempted = false
			return false, nil
		}
		if err == nil {
			err = unix.Unlinkat(parentFd, stage, 0)
		}
	} else {
		err = unix.Renameat(parentFd, stage, parentFd, name)
	}
	if err != nil {
		return false, err
	}
	syncErr = parent.Sync()
	if syncErr == nil && self.syncDirectoryForTest != nil {
		syncErr = self.syncDirectoryForTest(parent)
	}
	if err := errors.Join(syncErr, owner.check(), owner.volume.CheckDirectory(relative, parent), ctx.Err()); err != nil {
		return false, err
	}
	var published unix.Stat_t
	if err := unix.Fstatat(parentFd, name, &published, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, err
	}
	if published.Dev != staged.Dev || published.Ino != staged.Ino || published.Size != written {
		return false, errors.Join(durablevolume.ErrIdentity, errors.New("durable blob published object changed before acknowledgement"))
	}
	if accounted != nil {
		*accounted = usage - replaced + written
	}
	return true, nil
}

// The reader retains the lease until Close. Actual byte reads cannot be
// redirected by directory replacement, and resource pressure does not prevent
// inspection of original objects because this owner is read-only.
type durableBlobReader struct {
	ctx            context.Context
	volume         *durablevolume.Owner
	parent, file   *os.File
	relative, name string
	anchor         unix.Stat_t
	failed         error
	// Instance-only ordering seam after real descriptor I/O; production nil.
	afterReadForTest func(int, error)
}

func (self *localBlobStore) getDurable(ctx context.Context, key string) (_ io.ReadCloser, resultErr error) {
	if _, err := localBlobWritePath(self.root, key); err != nil {
		return nil, err
	}
	volume, err := self.openDurable(ctx, durablevolume.ReadOnly)
	if err != nil {
		return nil, err
	}
	reader := &durableBlobReader{ctx: ctx, volume: volume, relative: filepath.Dir(filepath.FromSlash(key)), name: filepath.Base(key)}
	if reader.relative == "." {
		reader.relative = ""
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, reader.Close())
		}
	}()
	reader.parent, err = volume.OpenDirectory(reader.relative, false)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(reader.parent.Fd()), reader.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	reader.file = os.NewFile(uintptr(fd), key)
	if err := unix.Fstat(fd, &reader.anchor); err != nil {
		return nil, err
	}
	if err := reader.check(); err != nil {
		return nil, err
	}
	return reader, nil
}

func (self *durableBlobReader) check() error {
	if self.failed != nil {
		return self.failed
	}
	if self.volume == nil || self.parent == nil || self.file == nil {
		return errors.New("durable blob reader is closed")
	}
	if err := errors.Join(self.ctx.Err(), self.volume.CheckDirectory(self.relative, self.parent)); err != nil {
		return err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(int(self.parent.Fd()), self.name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			self.failed = errors.Join(durablevolume.ErrIdentity, err)
			return self.failed
		}
		return err
	}
	if named.Dev != self.anchor.Dev || named.Ino != self.anchor.Ino || named.Size != self.anchor.Size || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Nlink != 1 {
		self.failed = errors.Join(durablevolume.ErrIdentity, errors.New("durable blob reader lost its original object"))
		return self.failed
	}
	return nil
}

func (self *durableBlobReader) Read(raw []byte) (int, error) {
	if err := self.check(); err != nil {
		return 0, err
	}
	n, err := self.file.Read(raw[:min(len(raw), 64*1024)])
	if self.afterReadForTest != nil {
		self.afterReadForTest(n, err)
	}
	if checkErr := self.check(); checkErr != nil {
		// Full-buffer consumers may discard an error alongside enough bytes.
		// Withhold this read; the retained descriptor still advances normally.
		return 0, errors.Join(err, checkErr)
	}
	return n, err
}

func (self *durableBlobReader) Close() error {
	var result error
	if self.file != nil {
		result = errors.Join(self.check(), self.file.Close())
		self.file = nil
	}
	if self.parent != nil {
		result = errors.Join(result, self.parent.Close())
		self.parent = nil
	}
	if self.volume != nil {
		result = errors.Join(result, self.volume.Close())
		self.volume = nil
	}
	return result
}

func (self *localBlobStore) reapDurable(ctx context.Context, rules []BlobLifecycleRule) (resultErr error) {
	owner, err := self.lockCapacity(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Close()) }()
	now := NowUtc()
	return walkDurableBlobs(ctx, owner.volume, maximumLocalBlobListScanEntries, func(parent *os.File, name, key string, state unix.Stat_t) error {
		for _, rule := range rules {
			if rule.TTL > 0 && strings.HasPrefix(key, rule.KeyPrefix) && rule.TTL <= now.Sub(time.Unix(state.Mtim.Sec, state.Mtim.Nsec)) {
				if self.beforeReapCommitForTest != nil {
					self.beforeReapCommitForTest(key)
				}
				if err := errors.Join(ctx.Err(), owner.check()); err != nil {
					return err
				}
				var named unix.Stat_t
				if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
						return self.retainDurableFailure(errors.Join(durablevolume.ErrIdentity, errors.New("expired blob candidate disappeared before removal"), err))
					}
					return errors.Join(&durablevolume.UnavailableError{Reason: "cannot observe expired blob candidate"}, err)
				}
				if named.Dev != state.Dev || named.Ino != state.Ino || named.Size != state.Size || named.Mtim != state.Mtim || named.Ctim != state.Ctim || named.Mode != state.Mode || named.Nlink != 1 {
					return self.retainDurableFailure(errors.Join(durablevolume.ErrIdentity, errors.New("expired blob candidate changed before removal")))
				}
				if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
					return err
				}
				syncErr := parent.Sync()
				if syncErr == nil && self.syncDirectoryForTest != nil {
					syncErr = self.syncDirectoryForTest(parent)
				}
				if err := errors.Join(syncErr, owner.check()); err != nil {
					owner.failed = errors.Join(ErrLocalBlobPublicationUncertain, err)
					return owner.failed
				}
				return nil
			}
		}
		return nil
	})
}
