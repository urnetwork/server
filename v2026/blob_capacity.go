package server

// One persistent private inode serializes capacity admission for every local
// store and process sharing a physical root. It is never unlinked on release:
// deleting a lock file would let a later opener acquire a different owner.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const localBlobCapacityLockName = ".capacity-owner.partial"

// Regular-file copies check cancellation between bounded reads instead of
// retaining the root owner until an entire large source has been consumed.
type localBlobCapacityReader struct {
	ctx    context.Context
	reader io.Reader
}

// Preserve the underlying EOF identity used by io.Copy's completion check.
func (self *localBlobCapacityReader) Read(data []byte) (int, error) {
	if err := self.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := self.reader.Read(data)
	if canceled := self.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

// The physical root is fixed before locking so path aliases share one owner.
type localBlobCapacityOwner struct {
	root     string
	rootInfo os.FileInfo
	file     *os.File
}

// Reserved temporary names and escaping paths cannot become uncharged objects.
// Existing directory aliases are refused; the configured root itself may be an
// alias because its physical target was independently resolved by the owner.
func localBlobWritePath(root, key string) (string, error) {
	if key == "" || strings.Contains(key, "\\") || filepath.IsAbs(key) || filepath.VolumeName(key) != "" {
		return "", errors.New("local blob key is outside its owned namespace")
	}
	relative := filepath.FromSlash(key)
	if filepath.Clean(relative) != relative || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || strings.HasSuffix(relative, blobPartialSuffix) {
		return "", errors.New("local blob key is noncanonical or uses a private partial name")
	}
	path := root
	parts := strings.Split(relative, string(filepath.Separator))
	for index, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || index < len(parts)-1 && !info.IsDir() || index == len(parts)-1 && !info.Mode().IsRegular() {
			return "", errors.New("local blob key contains an unowned filesystem entry")
		}
	}
	return path, nil
}

// Contention is observed only after a real nonblocking operating-system lock
// refusal. Waiting callers own no staged bytes and cancellation closes their fd.
func (self *localBlobStore) lockCapacity(ctx context.Context) (*localBlobCapacityOwner, error) {
	return self.acquireCapacity(ctx, true)
}

// A multi-root owner drops every acquired root before waiting on contention;
// this also prevents opposite-order deadlocks through physical mount aliases.
func (self *localBlobStore) tryLockCapacity(ctx context.Context) (*localBlobCapacityOwner, error) {
	return self.acquireCapacity(ctx, false)
}

// All acquisitions share identical physical-path, inode and cancellation checks.
func (self *localBlobStore) acquireCapacity(ctx context.Context, wait bool) (*localBlobCapacityOwner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self.beforeCapacityLockForTest != nil {
		self.beforeCapacityLockForTest()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(self.root, 0o755); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(self.root)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return nil, errors.Join(errors.New("local blob capacity root is not a directory"), err)
	}
	path := filepath.Join(root, localBlobCapacityLockName)
	if info, err := os.Lstat(path); err == nil {
		if !localBlobCapacityPrivateFile(info) || info.Size() != 0 {
			return nil, errors.New("local blob capacity owner is not a private empty regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	owner := &localBlobCapacityOwner{root: root, rootInfo: rootInfo, file: file}
	refuse := func(cause error) (*localBlobCapacityOwner, error) {
		return nil, errors.Join(cause, file.Close())
	}
	if err := owner.check(); err != nil {
		return refuse(err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return refuse(err)
		}
		locked, err := localBlobCapacityTryLock(file)
		if err != nil {
			return refuse(err)
		}
		if locked {
			if err := errors.Join(ctx.Err(), owner.check()); err != nil {
				return nil, errors.Join(err, owner.Close())
			}
			return owner, nil
		}
		if self.afterCapacityContentionForTest != nil {
			self.afterCapacityContentionForTest()
		}
		if !wait {
			return nil, file.Close()
		}
		select {
		case <-ctx.Done():
			return refuse(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Existing owner and root identities must survive a wait before mutation.
func (self *localBlobCapacityOwner) check() error {
	rootInfo, err := os.Stat(self.root)
	if err != nil || !os.SameFile(rootInfo, self.rootInfo) {
		return errors.Join(errors.New("local blob capacity root changed"), err)
	}
	opened, err := self.file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(filepath.Join(self.root, localBlobCapacityLockName))
	if err != nil || !localBlobCapacityPrivateFile(current) || current.Size() != 0 || !os.SameFile(opened, current) {
		return errors.Join(errors.New("local blob capacity owner changed"), err)
	}
	return nil
}

// Both release errors are retained; a closed descriptor also releases its lock.
func (self *localBlobCapacityOwner) Close() error {
	if self == nil || self.file == nil {
		return nil
	}
	file := self.file
	self.file = nil
	return errors.Join(localBlobCapacityUnlock(file), file.Close())
}

// Keep capacity arithmetic nonnegative and avoid addition overflow on refusal.
func localBlobAvailableBytes(usage, replacement, incoming, maximum int64) (int64, error) {
	if usage < 0 || replacement < 0 || replacement > usage || incoming < 0 || maximum <= 0 {
		return 0, errors.New("invalid local blob capacity accounting")
	}
	base := usage - replacement
	if base > maximum || incoming > maximum-base {
		return 0, fmt.Errorf("local blob capacity exceeded: usage=%d replacement=%d incoming=%d max=%d", usage, replacement, incoming, maximum)
	}
	return maximum - base, nil
}
