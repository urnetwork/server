package model

// What api's object writers on the first metadata cluster spend of the process's write budget (§11.13): rev/'s
// (SidecarRevStore), hold/'s (AbuseHoldStore) and the authorization log's proj/ and projrows/ (ConfigLogSequencer),
// whose every write is P0 of its writer's kind (QD-084), at api and in taskworker alike. Every PUT and DELETE carries
// P0 in its ctx, as a cluster evsservice.BudgetedBlobs wraps requires, the budget passing it spending nothing, and
// each write is spent once, as P0 of its writer's kind, once the store shows it took effect, at its own one of
// §11.13's cost rows (evsservice.PriceTable): a create ~9 KiB, an overwrite ~14 KiB, since the store writes the old
// xl.meta again, and a delete ~3.5 KiB, on 4 KiB blocks. §11.13 prices a write in drive bytes and a read nothing, so a
// write that writes nothing spends nothing:
//
//	answered   a PUT's success spends it, and a 412 wrote nothing and spends nothing. The store answers a DELETE of a
//	           key gone already as it answers one that deleted, so each deleter reads the key first and sends no
//	           DELETE for none, and a DELETE answered nil after a read found the object spends it
//	lost       a write whose outcome is unknown is settled by the next read of its key: by §19.6's settle rule
//	           (D-OPS-20) it took effect before that read or never. A PUT's bytes there, or a DELETE's key gone, spend
//	           it; a key that shows it never landed, as each writer's neverLanded says, spends nothing; a key that
//	           cannot tell spends it once
//	unsettled  a write whose next read failed too is spent as issued, once, and kept by key, at most
//	           UnsettledWriteCount: a later read that shows it landed settles it, its spend standing, and one that
//	           shows it never landed keeps it, so the next write of the key that takes effect spends only what its own
//	           row prices past what the kept write spent, at most one of the two having taken effect; any other read
//	           forgets it, its spend standing, as does a writer that reads its key holding what it knows another write
//	           put there (forget)
//
// What the process reports is then what the writes that took effect price at, but for a write whose outcome no read
// settled, spent once whether or not it landed; a kept write and the write that settles it, spent at the dearer of
// their two rows; a DELETE racing another deleter's of the same object, which each spends; and an unsettled write
// past UnsettledWriteCount, which the write that settles it spends again. A kept write that took effect and that no
// later read of its key settles, such as a DELETE whose key no LIST shows again, holds its place until the process
// ends. Without a budget nothing is spent. Safe for concurrent use.

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
)

// See the file header.
type apiWriteSpend struct {
	cluster sidecar.BlobCluster
	kind    sidecar.WriteP0Kind
	// how many unsettled writes are kept by key; non-positive keeps none
	unsettledWriteCount int

	stateLock sync.Mutex
	// the process's write budget, nil for none, and §11.13's cost rows it prices each write by
	budget sidecar.WriteBudget
	prices *evsservice.PriceTable
	// writes spent with their outcome unknown, until a read or a later write of the key settles them
	keyUnsettledWrites map[string]*apiUnsettledWrite
}

// Spends writes on cluster as P0 of kind, keeping at most unsettledWriteCount unsettled writes.
func newApiWriteSpend(cluster sidecar.BlobCluster, kind sidecar.WriteP0Kind, unsettledWriteCount int) *apiWriteSpend {
	return &apiWriteSpend{
		cluster:             cluster,
		kind:                kind,
		unsettledWriteCount: unsettledWriteCount,
		keyUnsettledWrites:  map[string]*apiUnsettledWrite{},
	}
}

// One write of a key whose outcome may be unknown: what it does, what it sent, and what shows it never landed.
type apiWrite struct {
	// what the write does to its key, which picks its cost row
	operation evsservice.WriteOperationKind
	// a PUT's bytes and their hash; a DELETE sends none
	byteCount  int
	bodySha256 [sha256.Size]byte
	// what its writer counts it as once a read shows it landed, "" for nothing
	outcome string
	// whether a read that found body under etag, found false for no object, shows this write never landed
	neverLanded func(found bool, body []byte, etag string) bool
}

// A PUT of body, a create or an overwrite, that its writer counts as outcome once it lands.
func newApiWrite(operation evsservice.WriteOperationKind, body []byte, outcome string, neverLanded func(found bool, body []byte, etag string) bool) *apiWrite {
	return &apiWrite{
		operation:   operation,
		byteCount:   len(body),
		bodySha256:  sha256.Sum256(body),
		outcome:     outcome,
		neverLanded: neverLanded,
	}
}

// A DELETE of the object a read found under etag: its key gone shows it landed, and that object still standing shows
// it never landed and never will.
func newApiDelete(etag string) *apiWrite {
	return &apiWrite{
		operation: evsservice.WriteOperationDelete,
		neverLanded: func(found bool, body []byte, heldEtag string) bool {
			return found && heldEtag == etag
		},
	}
}

// Whether a read that found an object whose bytes hash to bodySha256, found false for none, shows this write landed:
// a PUT's own bytes there, or a DELETE's key gone.
func (self *apiWrite) landedIn(found bool, bodySha256 [sha256.Size]byte) bool {
	if self.operation == evsservice.WriteOperationDelete {
		return !found
	}
	return found && bodySha256 == self.bodySha256
}

// A write kept unsettled, and the priced bytes spent for it and for any kept write of its key it took the place of.
type apiUnsettledWrite struct {
	write           *apiWrite
	pricedByteCount int64
}

// Spends every write that takes effect from budget, at its row of prices, and passes every PUT and DELETE through it
// as P0 (see the file header). nil prices is the table a budget built with evsservice's default settings prices with
// (WriteBudgetSettings.PriceTable), as api's and taskworker's are; nil budget removes it.
func (self *apiWriteSpend) setBudget(budget sidecar.WriteBudget, prices *evsservice.PriceTable) {
	if prices == nil {
		prices = evsservice.DefaultWriteBudgetSettings().PriceTable
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.budget, self.prices = budget, prices
}

// The process's write budget and its cost rows, nil for none.
func (self *apiWriteSpend) writeBudget() (sidecar.WriteBudget, *evsservice.PriceTable) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.budget, self.prices
}

// ctx for one PUT or DELETE: with a budget installed, carrying P0, which the budget passes spending nothing yet,
// since the write may write nothing.
func (self *apiWriteSpend) p0Context(ctx context.Context) context.Context {
	budget, _ := self.writeBudget()
	if budget == nil {
		return ctx
	}
	return budget.IssueP0(ctx, self.cluster.ClusterId(), self.kind, 0)
}

// write's priced bytes: its row of prices for its object's size.
func apiWritePricedByteCount(prices *evsservice.PriceTable, write *apiWrite) int64 {
	pricedByteCount, err := prices.Price(evsservice.WriteOperation{Kind: write.operation, ObjectByteCount: int64(write.byteCount), Count: 1})
	if err != nil {
		// only an operation no table prices, which these writers never send: its bytes, so the write spends
		return int64(write.byteCount)
	}
	return pricedByteCount
}

// write, of key, took effect, as the store showed, or may have and no read can tell any more: spent at its row, less
// what the unsettled write of key spent, which by the settle rule never landed if this one did.
func (self *apiWriteSpend) spendWritten(ctx context.Context, key string, write *apiWrite) {
	budget, prices := self.writeBudget()
	paidByteCount := func() int64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		unsettled, ok := self.keyUnsettledWrites[key]
		if !ok {
			return 0
		}
		delete(self.keyUnsettledWrites, key)
		return unsettled.pricedByteCount
	}()
	if budget == nil {
		return
	}
	if pricedByteCount := apiWritePricedByteCount(prices, write); paidByteCount < pricedByteCount {
		budget.IssueP0(ctx, self.cluster.ClusterId(), self.kind, pricedByteCount-paidByteCount)
	}
}

// write, of key, may have taken effect and no read could tell: spent as issued, at its row, once, and kept, so the
// write of key that settles it spends only what its own row prices past this (UnsettledWriteCount). An unsettled
// write of key kept already paid for it as far as its own row reached, at most one of the two having taken effect,
// and write takes its place.
func (self *apiWriteSpend) spendUnsettled(ctx context.Context, key string, write *apiWrite) {
	budget, prices := self.writeBudget()
	pricedByteCount := int64(0)
	if budget != nil {
		pricedByteCount = apiWritePricedByteCount(prices, write)
	}
	issueByteCount := func() int64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		paidByteCount := int64(0)
		unsettled, kept := self.keyUnsettledWrites[key]
		if kept {
			paidByteCount = unsettled.pricedByteCount
		}
		issueByteCount := max(0, pricedByteCount-paidByteCount)
		if kept || len(self.keyUnsettledWrites) < self.unsettledWriteCount {
			self.keyUnsettledWrites[key] = &apiUnsettledWrite{write: write, pricedByteCount: paidByteCount + issueByteCount}
		}
		return issueByteCount
	}()
	if 0 < issueByteCount {
		budget.IssueP0(ctx, self.cluster.ClusterId(), self.kind, issueByteCount)
	}
}

// A read of key showed what another write put there, or a write its writer knows was spent already: the unsettled
// write of key, if any, is forgotten, its spend standing.
func (self *apiWriteSpend) forget(key string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.keyUnsettledWrites, key)
}

// Settles what a read of key shows, found false for no object. write is the caller's write of key whose outcome was
// unknown, nil for none: shown landed (writeLanded), it is spent; a read that shows it never landed spends nothing;
// and one that cannot tell spends it once. The unsettled write of key, if any: shown landed, it is settled, its spend
// standing, and returned for its writer to count when it has an outcome; a read that shows it never landed keeps it;
// and any other forgets it, its spend standing.
func (self *apiWriteSpend) settleRead(ctx context.Context, key string, write *apiWrite, found bool, body []byte, etag string) (writeLanded bool, unsettledLanded *apiWrite) {
	bodySha256 := sha256.Sum256(body)
	if write != nil && write.landedIn(found, bodySha256) {
		// landed, its answer lost
		self.spendWritten(ctx, key, write)
		return true, nil
	}
	unsettled := func() *apiUnsettledWrite {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.keyUnsettledWrites[key]
	}()
	if unsettled != nil {
		landed := unsettled.write.landedIn(found, bodySha256)
		if landed || !unsettled.write.neverLanded(found, body, etag) {
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
			if landed && forgotten && unsettled.write.outcome != "" {
				unsettledLanded = unsettled.write
			}
		}
	}
	if write != nil && !write.neverLanded(found, body, etag) {
		// it may have landed before another write replaced it: spent once
		self.spendWritten(ctx, key, write)
	}
	return false, unsettledLanded
}

// Deletes key, whose object a read just found under etag, as P0 at §11.13's delete row, spent once the store shows the
// DELETE took effect (see the file header): its nil spends it, and a refusal, which deleted nothing, spends nothing.
// After an unknown outcome readAgain reads the key, found false for none, and settleRead decides: its key gone spends
// it, the object under etag still standing shows it never landed, and any other spends it once; a read that fails too
// keeps it unsettled. nil once the DELETE took effect, else the DELETE's error.
func (self *apiWriteSpend) deleteObject(ctx context.Context, key string, etag string, readAgain func() (found bool, body []byte, etag string, err error)) error {
	write := newApiDelete(etag)
	err := self.cluster.Delete(self.p0Context(ctx), key)
	var unknown *sidecar.BlobUnknownError
	switch {
	case err == nil:
		self.spendWritten(ctx, key, write)
		return nil
	case errors.As(err, &unknown):
		found, body, heldEtag, readErr := readAgain()
		if readErr != nil {
			// no read told: spent as issued, once, for the write that settles it
			self.spendUnsettled(ctx, key, write)
			return err
		}
		if writeLanded, _ := self.settleRead(ctx, key, write, found, body, heldEtag); writeLanded {
			// landed, its answer lost
			return nil
		}
		return err
	default:
		return err
	}
}
