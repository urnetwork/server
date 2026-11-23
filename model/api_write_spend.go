package model

// What api's object writers on the first metadata cluster spend of the process's write budget (§11.13): rev/'s
// (SidecarRevStore) and hold/'s (AbuseHoldStore), by the rule the authorization log's sequencer spends its own objects
// by (config_log_model.go's file header). Every PUT carries P0 in its ctx, as a cluster evsservice.BudgetedBlobs wraps
// requires, the budget passing it spending nothing, and each write is spent once, as P0 of its writer's kind at its
// object's price, once the store shows it took effect. §11.13 prices a write in drive bytes and a read nothing, so a
// PUT that writes nothing spends nothing:
//
//	answered   a PUT's success spends it, and a 412 wrote nothing and spends nothing
//	lost       a PUT whose outcome is unknown is settled by the next read of its key: by §19.6's settle rule (D-OPS-20)
//	           it took effect before that read or never. Its bytes there spend it; a key that shows it never landed,
//	           as each writer's neverLanded says, spends nothing; a key that cannot tell spends it once
//	unsettled  a PUT whose next read failed too is spent as issued, once, and kept by key, at most
//	           UnsettledWriteCount: a later read that finds its bytes settles it as landed, its spend standing, and
//	           one that shows it never landed keeps it, so the next write of the key that takes effect spends nothing
//	           more, at most one of the two having taken effect; any other read forgets it, its spend standing
//
// What the process reports is then what the writes that took effect price at, but for a write whose outcome no read
// settled, spent once whether or not it landed, and an unsettled write past UnsettledWriteCount, which the write that
// settles it spends again. Without a budget nothing is spent. Safe for concurrent use.

import (
	"context"
	"crypto/sha256"
	"sync"

	"github.com/urnetwork/sidecar"
)

// See the file header.
type apiWriteSpend struct {
	cluster sidecar.BlobCluster
	kind    sidecar.WriteP0Kind
	// how many unsettled writes are kept by key; non-positive keeps none
	unsettledWriteCount int

	stateLock sync.Mutex
	// the process's write budget, nil for none, and how it prices a write of an object (§11.13's cost rows)
	budget sidecar.WriteBudget
	price  func(objectByteCount int64) int64
	// writes spent with their outcome unknown, until a read or a later write of the key settles them
	keyUnsettledWrites map[string]*apiWrite
}

// Spends writes on cluster as P0 of kind, keeping at most unsettledWriteCount unsettled writes.
func newApiWriteSpend(cluster sidecar.BlobCluster, kind sidecar.WriteP0Kind, unsettledWriteCount int) *apiWriteSpend {
	return &apiWriteSpend{
		cluster:             cluster,
		kind:                kind,
		unsettledWriteCount: unsettledWriteCount,
		keyUnsettledWrites:  map[string]*apiWrite{},
	}
}

// One PUT of a key whose outcome is unknown: what it sent, and what shows it never landed.
type apiWrite struct {
	byteCount  int
	bodySha256 [sha256.Size]byte
	// what its writer counts it as once a read shows it landed, "" for nothing
	outcome string
	// whether a read that found body under etag, found false for no object and body other than this write's, shows
	// this write never landed
	neverLanded func(found bool, body []byte, etag string) bool
}

// A PUT of body that its writer counts as outcome once it lands.
func newApiWrite(body []byte, outcome string, neverLanded func(found bool, body []byte, etag string) bool) *apiWrite {
	return &apiWrite{
		byteCount:   len(body),
		bodySha256:  sha256.Sum256(body),
		outcome:     outcome,
		neverLanded: neverLanded,
	}
}

// Spends every write that takes effect from budget, priced by price, the bytes of its object, and passes every PUT
// through it as P0 (see the file header). nil removes it.
func (self *apiWriteSpend) setBudget(budget sidecar.WriteBudget, price func(objectByteCount int64) int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.budget, self.price = budget, price
}

// The process's write budget and its price, nil for none.
func (self *apiWriteSpend) writeBudget() (sidecar.WriteBudget, func(int64) int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.budget, self.price
}

// ctx for one PUT: with a budget installed, carrying P0, which the budget passes spending nothing yet, since the PUT
// may write nothing.
func (self *apiWriteSpend) p0Context(ctx context.Context) context.Context {
	budget, _ := self.writeBudget()
	if budget == nil {
		return ctx
	}
	return budget.IssueP0(ctx, self.cluster.ClusterId(), self.kind, 0)
}

// Spends one write of an object of byteCount bytes, priced, as P0 of the writer's kind.
func (self *apiWriteSpend) spendP0(ctx context.Context, byteCount int) {
	budget, price := self.writeBudget()
	if budget == nil {
		return
	}
	pricedByteCount := int64(byteCount)
	if price != nil {
		pricedByteCount = price(int64(byteCount))
	}
	budget.IssueP0(ctx, self.cluster.ClusterId(), self.kind, pricedByteCount)
}

// A write of key of byteCount bytes took effect, as the store showed, or may have and no read can tell any more:
// spent, unless the unsettled write of key paid for it, which by the settle rule never landed if this one did.
func (self *apiWriteSpend) spendWritten(ctx context.Context, key string, byteCount int) {
	paid := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if _, ok := self.keyUnsettledWrites[key]; !ok {
			return false
		}
		delete(self.keyUnsettledWrites, key)
		return true
	}()
	if !paid {
		self.spendP0(ctx, byteCount)
	}
}

// write, a PUT of key, may have taken effect and no read could tell: spent as issued, once, and kept, so the write of
// key that settles it spends nothing more (UnsettledWriteCount). An unsettled write of key kept already paid for it,
// at most one of the two having taken effect, and write takes its place.
func (self *apiWriteSpend) spendUnsettled(ctx context.Context, key string, write *apiWrite) {
	paid := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		_, paid := self.keyUnsettledWrites[key]
		if paid || len(self.keyUnsettledWrites) < self.unsettledWriteCount {
			self.keyUnsettledWrites[key] = write
		}
		return paid
	}()
	if !paid {
		self.spendP0(ctx, write.byteCount)
	}
}

// Settles what a read of key shows, found false for no object. write is the caller's PUT of key whose outcome was
// unknown, nil for none: its bytes there land it (writeLanded), spent; a read that shows it never landed spends
// nothing; and one that cannot tell spends it once. The unsettled write of key, if any: its bytes there settle it as
// landed, returned for its writer to count, its spend standing; a read that shows it never landed keeps it; and any
// other forgets it, its spend standing.
func (self *apiWriteSpend) settleRead(ctx context.Context, key string, write *apiWrite, found bool, body []byte, etag string) (writeLanded bool, unsettledLanded *apiWrite) {
	bodySha256 := sha256.Sum256(body)
	if write != nil && found && bodySha256 == write.bodySha256 {
		// landed, its answer lost
		self.spendWritten(ctx, key, write.byteCount)
		return true, nil
	}
	unsettled := func() *apiWrite {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.keyUnsettledWrites[key]
	}()
	if unsettled != nil {
		landed := found && bodySha256 == unsettled.bodySha256
		if landed || !unsettled.neverLanded(found, body, etag) {
			forgotten := func() bool {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				// a concurrent write of the key may have settled it first
				if self.keyUnsettledWrites[key] != unsettled {
					return false
				}
				delete(self.keyUnsettledWrites, key)
				return true
			}()
			if landed && forgotten {
				unsettledLanded = unsettled
			}
		}
	}
	if write != nil && !write.neverLanded(found, body, etag) {
		// it may have landed before another write replaced it: spent once
		self.spendWritten(ctx, key, write.byteCount)
	}
	return false, unsettledLanded
}
