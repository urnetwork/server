// Package dnshost is server's host of the dns engine (§12, D-DNS-3): the
// dnsservice.DnsStore the API block reaches §12.14's tables through, and the
// host server/cli/dns runs in its two roles (host.go). An edge is given a
// dnsservice.DnsFeed and never a store, so the edge role holds no Postgres or
// Redis handle at all (I-12).
package dnshost

// dnsservice.DnsStore on Postgres and Redis (DNS-M5-09): model's dns functions
// for the tables, and the host's Redis for {dns}serial:<root>, its raises and
// the one shared key-event subscription that wakes the feed's holds, never one
// per poll (server/model/peer_model.go:32-37). sidecartest.RunDnsStoreSuite
// holds it to the fake's promises (D-TEST-2). Every method is safe for
// concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var _ dnsservice.DnsStore = (*Store)(nil)

// What the store needs of Redis: the serial key's script and read, and the
// process's shared key events. A host's sidecar.Redis is one.
type StoreRedis interface {
	model.DnsSerialRedis
	AddKeyEventCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (remove func(), err error)
}

// The store. See the file header.
type Store struct {
	redis  StoreRedis
	remove func()

	stateLock sync.Mutex
	// by root, closed at the next raise this process hears, or at a resync
	raised map[string]chan struct{}
}

// A store over the host's Redis, listening for raises of every root on the
// process's one key-event subscriber. Close ends that.
func NewStore(redis StoreRedis) (*Store, error) {
	if redis == nil {
		return nil, errors.New("dns store: no redis")
	}
	self := &Store{
		redis:  redis,
		raised: map[string]chan struct{}{},
	}
	remove, err := redis.AddKeyEventCallback(model.DnsSerialKeyPattern, self.keyEvent)
	if err != nil {
		return nil, err
	}
	self.remove = remove
	return self, nil
}

// Stops listening for raises. Every waiter is woken once, so none waits on a
// store that will not hear again.
func (self *Store) Close() {
	if self.remove != nil {
		self.remove()
	}
	self.wakeAll()
}

// Runs on the shared subscriber's goroutine: it closes channels and returns.
func (self *Store) keyEvent(event sidecar.RedisKeyEvent) {
	if event.Resync {
		self.wakeAll()
		return
	}
	if root, ok := model.ParseDnsSerialKey(event.Key); ok {
		self.wake(root)
	}
}

func (self *Store) wake(root string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if raised, ok := self.raised[root]; ok {
		close(raised)
		delete(self.raised, root)
	}
}

func (self *Store) wakeAll() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, raised := range self.raised {
		close(raised)
	}
	self.raised = map[string]chan struct{}{}
}

// Runs callback, turning a raise into its error, since the engine reads errors.
func storeCall(callback func() error) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("dns store: %v", recovered)
			}
		}
	}()
	return callback()
}

func (self *Store) EnsureRoot(ctx context.Context, root string) error {
	return storeCall(func() error {
		return model.EnsureDnsRoot(ctx, root)
	})
}

func (self *Store) RootState(ctx context.Context, root string) (state *dnsservice.DnsRootState, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		state, err = model.GetDnsRootState(ctx, root)
		return err
	})
	return
}

func (self *Store) Journal(ctx context.Context, root string, after uint64, limit int) (read *dnsservice.DnsJournalRead, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		read, err = model.GetDnsJournal(ctx, root, after, limit)
		return err
	})
	return
}

func (self *Store) Snapshot(ctx context.Context, root string) (rows dnsservice.DnsSnapshotRows, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		rows, err = model.OpenDnsSnapshot(ctx, root)
		return err
	})
	return
}

func (self *Store) Write(ctx context.Context, write *dnsservice.DnsZoneWrite) (serial uint64, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		serial, err = model.DnsStoreWrite(ctx, self.redis, write)
		return err
	})
	return
}

func (self *Store) ZoneVersion(ctx context.Context, root string, networkId sidecar.Id) (version uint64, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		version, err = model.GetDnsZoneVersion(ctx, root, server.Id(networkId))
		return err
	})
	return
}

func (self *Store) PurgeJournal(ctx context.Context, root string, before time.Time) (purgedCount int, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		purgedCount, err = model.PurgeDnsJournal(ctx, root, before)
		return err
	})
	return
}

func (self *Store) SetRestoring(ctx context.Context, root string, restoring bool) error {
	return storeCall(func() error {
		return model.SetDnsRestoring(ctx, root, restoring)
	})
}

func (self *Store) CommitRestore(ctx context.Context, root string, incidentId sidecar.Id, observedSerial uint64) (restoreSerial uint64, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		restoreSerial, err = model.CommitDnsRestore(ctx, self.redis, root, server.Id(incidentId), observedSerial)
		return err
	})
	return
}

func (self *Store) RecordReloadDiff(ctx context.Context, diff *dnsservice.DnsReloadDiff) error {
	return storeCall(func() error {
		return model.AddDnsReloadDiff(ctx, diff)
	})
}

func (self *Store) ReloadDiffs(ctx context.Context, root string) (diffs []*dnsservice.DnsReloadDiff, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		diffs, err = model.GetDnsReloadDiffs(ctx, root)
		return err
	})
	return
}

func (self *Store) RaisedSerial(ctx context.Context, root string) (serial uint64, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		serial, err = model.GetDnsRaisedSerial(ctx, self.redis, root)
		return err
	})
	return
}

// A channel that closes at the next raise of root this process hears. Take it
// immediately before reading the store, and a new one after it fires.
func (self *Store) SerialRaised(root string) <-chan struct{} {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	raised, ok := self.raised[root]
	if !ok {
		raised = make(chan struct{})
		self.raised[root] = raised
	}
	return raised
}
