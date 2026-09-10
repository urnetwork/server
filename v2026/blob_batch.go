package server

// Finite immutable publication scopes reuse a live physical-root quota owner.
// No accounted usage survives Close or authorizes another process/instance
// after the shared operating-system locks have been released.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const MaximumLocalBlobBatchWrites = 16 * 1024

// Wrappers may advertise their exact local backing store without bypassing
// their own Put/Get instrumentation. Unknown or mixed backends do not batch.
type LocalBlobBatchSource interface {
	LocalBlobBatchSource() BlobStore
}

// The ordinary local implementation is its own bounded publication source.
func (self *localBlobStore) LocalBlobBatchSource() BlobStore {
	return self
}

// The context route is private, so only an admitted lease can supply quota.
type localBlobWriteBatchKey struct{}

// One distinct physical root owns its initial census and actual commit deltas.
type localBlobWriteBatchRoot struct {
	store *localBlobStore
	path  string
	info  os.FileInfo
	owner *localBlobCapacityOwner
	usage int64
}

// Calls are serialized by a cancellable operation token, not a global cache.
// Close joins the active operation, verifies all accounting, then unlocks every
// root. A caller must defer Close and must not return publication success until
// it succeeds. The finite write count includes immutable collisions.
type LocalBlobWriteBatch struct {
	ctx           context.Context
	operation     chan struct{}
	stores        map[*localBlobStore]*localBlobWriteBatchRoot
	roots         []*localBlobWriteBatchRoot
	maximumWrites int
	writes        int
	closed        bool
	closeErr      error
}

// Bind all roots before taking locks, sort their physical names to prevent
// opposite-order deadlocks, and scan each once under its persistent OS owner.
// Nil without error means a declared backend cannot use this local-only scope;
// every store then keeps its existing independent per-call behavior.
func BeginLocalBlobWriteBatch(ctx context.Context, stores []BlobStore, maximumWrites int) (*LocalBlobWriteBatch, error) {
	if ctx == nil || len(stores) == 0 || len(stores) > 256 || maximumWrites <= 0 || maximumWrites > MaximumLocalBlobBatchWrites {
		return nil, errors.New("local blob batch requires bounded context, stores and writes")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, nested := ctx.Value(localBlobWriteBatchKey{}).(*LocalBlobWriteBatch); nested {
		return nil, errors.New("local blob write batches cannot be nested")
	}
	sources := make([]*localBlobStore, 0, len(stores))
	for _, store := range stores {
		source, ok := store.(LocalBlobBatchSource)
		if !ok {
			return nil, nil
		}
		local, ok := source.LocalBlobBatchSource().(*localBlobStore)
		if !ok || local == nil {
			return nil, errors.New("local blob batch source does not name a local store")
		}
		sources = append(sources, local)
	}
	batch := &LocalBlobWriteBatch{operation: make(chan struct{}, 1), stores: map[*localBlobStore]*localBlobWriteBatchRoot{}, maximumWrites: maximumWrites}
	batch.operation <- struct{}{}
	batch.ctx = context.WithValue(ctx, localBlobWriteBatchKey{}, batch)
	byPath := map[string]*localBlobWriteBatchRoot{}
	for _, store := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(store.root, 0o755); err != nil {
			return nil, err
		}
		absolute, err := filepath.Abs(store.root)
		if err != nil {
			return nil, err
		}
		root, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return nil, errors.Join(errors.New("local blob batch root is not a directory"), err)
		}
		entry := byPath[root]
		if entry == nil {
			for _, known := range batch.roots {
				if os.SameFile(info, known.info) {
					entry = known
					break
				}
			}
		}
		if entry == nil {
			entry = &localBlobWriteBatchRoot{store: store, path: root, info: info}
			byPath[root] = entry
			batch.roots = append(batch.roots, entry)
		} else if !os.SameFile(info, entry.info) {
			return nil, errors.New("local blob batch root changed during declaration")
		}
		byPath[root] = entry
		batch.stores[store] = entry
	}
	sort.Slice(batch.roots, func(i, j int) bool { return batch.roots[i].path < batch.roots[j].path })
	refuse := func(cause error) (*LocalBlobWriteBatch, error) {
		for index := len(batch.roots) - 1; index >= 0; index-- {
			cause = errors.Join(cause, batch.roots[index].owner.Close())
		}
		return nil, cause
	}
	for {
		complete := true
		for _, root := range batch.roots {
			owner, err := root.store.tryLockCapacity(ctx)
			if err != nil {
				return refuse(err)
			}
			if owner == nil {
				complete = false
				break
			}
			root.owner = owner
			if owner.root != root.path || !os.SameFile(owner.rootInfo, root.info) {
				return refuse(errors.New("local blob batch root changed before locking"))
			}
		}
		if complete {
			break
		}
		var releaseErr error
		for index := len(batch.roots) - 1; index >= 0; index-- {
			releaseErr = errors.Join(releaseErr, batch.roots[index].owner.Close())
			batch.roots[index].owner = nil
		}
		if releaseErr != nil {
			return nil, releaseErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	for _, root := range batch.roots {
		var err error
		root.usage, err = root.store.usageBytesAtRootWithContext(ctx, root.owner.root)
		if err != nil {
			return refuse(err)
		}
	}
	return batch, nil
}

// Use this route for every actual write; retaining the original store preserves
// its wrapper methods, namespace identity and independent readback behavior.
func (self *LocalBlobWriteBatch) Context() context.Context {
	return self.ctx
}

// Bind an already admitted quota owner without changing this caller's values,
// deadline or cancellation. Every write still checks both independent contexts;
// Close remains the batch owner's responsibility before any caller reports success.
func (self *LocalBlobWriteBatch) ContextFor(ctx context.Context) (context.Context, error) {
	if self == nil || self.ctx == nil || ctx == nil {
		return nil, errors.New("local blob batch context owner is incomplete")
	}
	if existing, ok := ctx.Value(localBlobWriteBatchKey{}).(*LocalBlobWriteBatch); ok && existing != self {
		return nil, errors.New("local blob batch context belongs to another owner")
	}
	if err := errors.Join(ctx.Err(), self.ctx.Err()); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, localBlobWriteBatchKey{}, self), nil
}

// An individual copy observes both lifetimes. Err synchronously samples the
// originals before a commit, so cancellation does not depend on scheduling an
// AfterFunc callback. Only this operation owns and joins that callback.
type localBlobBatchOperationContext struct {
	context.Context
	member context.Context
	owner  context.Context
	cancel context.CancelFunc
}

// Close the operation's Done before returning either original cancellation.
func (self *localBlobBatchOperationContext) Err() error {
	if err := self.member.Err(); err != nil {
		self.cancel()
		return err
	}
	if err := self.owner.Err(); err != nil {
		self.cancel()
		return err
	}
	return self.Context.Err()
}

// Only admitted immutable writes may reuse this census. Errors do not invent
// commit credit: the shared writer updates usage only after an actual link.
func (self *LocalBlobWriteBatch) putFile(ctx context.Context, store *localBlobStore, key, source string, sourceInfo os.FileInfo, ifAbsent bool) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-self.ctx.Done():
		return false, errors.Join(ctx.Err(), self.ctx.Err())
	case <-self.operation:
	}
	defer func() { self.operation <- struct{}{} }()
	if self.closed {
		return false, errors.New("local blob write batch is closed")
	}
	if err := errors.Join(ctx.Err(), self.ctx.Err()); err != nil {
		return false, err
	}
	root := self.stores[store]
	if root == nil || !ifAbsent {
		return false, errors.New("local blob batch write is outside its declared immutable stores")
	}
	info, err := os.Stat(store.root)
	if err != nil || !os.SameFile(info, root.info) {
		return false, errors.Join(errors.New("local blob batch store changed physical root"), err)
	}
	if self.writes >= self.maximumWrites {
		return false, errors.New("local blob batch write count is exhausted")
	}
	self.writes++
	if ctx == self.ctx {
		return store.putFileAtCapacity(ctx, key, source, sourceInfo, true, root.owner, &root.usage)
	}
	operationCtx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	stop := context.AfterFunc(self.ctx, func() {
		defer close(joined)
		cancel()
	})
	defer func() {
		if !stop() {
			<-joined
		}
		cancel()
	}()
	ownedCtx := &localBlobBatchOperationContext{Context: operationCtx, member: ctx, owner: self.ctx, cancel: cancel}
	return store.putFileAtCapacity(ownedCtx, key, source, sourceInfo, true, root.owner, &root.usage)
}

// A final fresh census detects unaccounted net byte changes before the caller
// may report publication success. Cooperating writers and reapers retain the
// shared lock contract. Same-size changes remain the independent authenticated
// readback's responsibility; no usage estimate survives this owner.
func (self *LocalBlobWriteBatch) Close() error {
	if self == nil {
		return nil
	}
	<-self.operation
	defer func() { self.operation <- struct{}{} }()
	if self.closed {
		return self.closeErr
	}
	self.closed = true
	self.closeErr = self.ctx.Err()
	for store, root := range self.stores {
		info, err := os.Stat(store.root)
		if err != nil || !os.SameFile(info, root.info) {
			self.closeErr = errors.Join(self.closeErr, errors.New("local blob batch store changed physical root"), err)
		}
	}
	for _, root := range self.roots {
		self.closeErr = errors.Join(self.closeErr, root.owner.check())
		actual, err := root.store.usageBytesAtRootWithContext(self.ctx, root.owner.root)
		if err == nil && actual != root.usage {
			err = fmt.Errorf("local blob batch root has unaccounted bytes: actual=%d accounted=%d", actual, root.usage)
		}
		self.closeErr = errors.Join(self.closeErr, err, root.owner.check())
	}
	for index := len(self.roots) - 1; index >= 0; index-- {
		self.closeErr = errors.Join(self.closeErr, self.roots[index].owner.Close())
	}
	return self.closeErr
}
