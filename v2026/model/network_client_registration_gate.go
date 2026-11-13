package model

// Registration allocation is serialized per network by a transaction-scoped
// advisory lock (`networkClientRegistrationOwner.resumeInTx`). Every
// transaction waiting on that lock holds a pooled connection, so a burst of
// one network's registrations used to queue hundreds of waiters in
// PostgreSQL and take the whole pool (2026-10-25). The gate admits one
// registration per network per process into its transaction; the others wait
// here, in memory, holding no connection, until it finishes or their caller
// stops. Across processes the advisory lock still serializes, so PostgreSQL
// sees at most one waiter per process per network, each bounded by the
// transaction's lock timeout (`server.PgLockTimeout`).
//
// Safe for concurrent use.

import (
	"context"
	"sync"

	"github.com/urnetwork/server/v2026"
)

type networkClientRegistrationGate struct {
	stateLock      sync.Mutex
	networkEntries map[server.Id]*networkClientRegistrationGateEntry
}

type networkClientRegistrationGateEntry struct {
	// one slot: the registration admitted into its transaction
	permit chan struct{}
	// the registrations admitted or waiting; the entry leaves the map at zero
	refCount int
}

func newNetworkClientRegistrationGate() *networkClientRegistrationGate {
	return &networkClientRegistrationGate{
		networkEntries: map[server.Id]*networkClientRegistrationGateEntry{},
	}
}

// The process's registration gate.
var networkClientRegistrations = newNetworkClientRegistrationGate()

// Waits until this registration may open its transaction, or until ctx ends.
// The caller runs `leave` once its transaction has ended.
func (self *networkClientRegistrationGate) enter(ctx context.Context, networkId server.Id) (leave func(), returnErr error) {
	entry := func() *networkClientRegistrationGateEntry {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		entry, ok := self.networkEntries[networkId]
		if !ok {
			entry = &networkClientRegistrationGateEntry{
				permit: make(chan struct{}, 1),
			}
			self.networkEntries[networkId] = entry
		}
		entry.refCount += 1
		return entry
	}()
	select {
	case entry.permit <- struct{}{}:
		return func() {
			<-entry.permit
			self.release(networkId, entry)
		}, nil
	case <-ctx.Done():
		self.release(networkId, entry)
		return nil, ctx.Err()
	}
}

func (self *networkClientRegistrationGate) release(networkId server.Id, entry *networkClientRegistrationGateEntry) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry.refCount -= 1
	if entry.refCount == 0 {
		delete(self.networkEntries, networkId)
	}
}

// The networks with a registration admitted or waiting. Tests only.
func (self *networkClientRegistrationGate) networkCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.networkEntries)
}

// The registrations of the network admitted or waiting. Tests only.
func (self *networkClientRegistrationGate) refCountFor(networkId server.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if entry, ok := self.networkEntries[networkId]; ok {
		return entry.refCount
	}
	return 0
}
