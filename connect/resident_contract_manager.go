// Forward authorization checks only the Redis contract projection. Missing or
// unavailable leases refuse; pair admission never waits or retains retries.
package connect

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const residentContractCheckMinTimeout = time.Second

// Owns recent positive results and per-pair admission. All maps are guarded by
// stateLock; Redis reads run outside it. No waiter or retry queue is retained.
type residentContractManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientId server.Id
	settings *ExchangeSettings

	stateLock       sync.Mutex
	activeContracts map[model.TransferPair]*activeContractEntry
	activeReads     map[model.TransferPair]bool
	checkLimiters   map[model.TransferPair]*limiter
	readContract    func(context.Context, server.Id, server.Id) residentContractAllowance
	readsClosed     bool
	readWorkers     sync.WaitGroup
	// Nil in production. Tests pause after the first clock sample and before
	// taking the map lock, without relying on scheduling of a blocked mutex.
	beforeCheckLockForTest func()

	nextActiveContractSweepTime time.Time
}

// Creates the packet-side reader; durable contract creation and repair stay in
// independently owned model control operations.
func newResidentContractManager(
	ctx context.Context,
	cancel context.CancelFunc,
	clientId server.Id,
	settings *ExchangeSettings,
) *residentContractManager {
	manager := &residentContractManager{
		ctx:             server.WithoutPostgres(ctx),
		cancel:          cancel,
		clientId:        clientId,
		settings:        settings,
		activeContracts: map[model.TransferPair]*activeContractEntry{},
		activeReads:     map[model.TransferPair]bool{},
		checkLimiters:   map[model.TransferPair]*limiter{},
	}
	manager.readContract = func(packetCtx context.Context, source, destination server.Id) residentContractAllowance {
		return readResidentContractAllowance(packetCtx, source, destination)
	}
	return manager
}

// Cancellation is an inactive result; programming failures keep their cause.
func handleContractManagerDone(do func()) {
	defer func() {
		if recovered := recover(); recovered != nil && !server.IsDoneError(recovered) {
			panic(recovered)
		}
	}()
	do()
}

// Reuses the existing positive freshness window and half-window refresh. A
// refused cold check returns immediately, including when a read is in flight.
func (self *residentContractManager) HasActiveContract(sourceId, destinationId server.Id) bool {
	if self.ctx.Err() != nil {
		return false
	}
	pair := model.NewUnorderedTransferPair(sourceId, destinationId)
	now := time.Now()
	cached, read := false, false
	var cachedDeadline time.Time
	if self.beforeCheckLockForTest != nil {
		self.beforeCheckLockForTest()
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.readsClosed || self.ctx.Err() != nil {
			return
		}
		if !now.Before(self.nextActiveContractSweepTime) {
			for key, entry := range self.activeContracts {
				if self.settings.ContractManagerCheckTimeout <= 0 || !now.Before(entry.deadline(self.settings.ContractManagerCheckTimeout)) {
					delete(self.activeContracts, key)
				}
			}
			for key, checkLimiter := range self.checkLimiters {
				if !self.activeReads[key] && self.activeContracts[key] == nil && checkLimiter.expired(now) {
					delete(self.checkLimiters, key)
				}
			}
			self.nextActiveContractSweepTime = now.Add(residentContractCheckMinTimeout)
		}
		// Admission may have waited for a map sweep or another short owner
		// operation; a timestamp from before taking the lock is not authority.
		now = time.Now()
		entry := self.activeContracts[pair]
		if entry != nil {
			cachedDeadline = entry.deadline(self.settings.ContractManagerCheckTimeout)
		}
		cached = entry != nil && self.settings.ContractManagerCheckTimeout > 0 && now.Before(cachedDeadline)
		if cached && now.Before(entry.checkTime.Add(cachedDeadline.Sub(entry.checkTime)/2)) {
			return
		}
		if self.activeReads[pair] {
			return
		}
		checkLimiter := self.checkLimiters[pair]
		if checkLimiter == nil {
			// Reuse the resident's destination budget for recent denied pairs
			// too; rotating unknown ids must not allocate unbounded history.
			if len(self.checkLimiters) >= max(0, self.settings.MaxConcurrentForwardsPerResident) {
				return
			}
			checkLimiter = newLimiter(self.ctx, residentContractCheckMinTimeout)
			self.checkLimiters[pair] = checkLimiter
		}
		if read = checkLimiter.allow(now); read {
			self.activeReads[pair] = true
			self.readWorkers.Add(1)
		}
	}()
	cached = cached && time.Now().Before(cachedDeadline)
	if !read {
		if !cached {
			defaultResidentContractAllowanceMetrics.add(contractAllowanceCheckRefused)
		}
		return cached
	}
	if cached {
		go server.HandleError(func() { self.performActiveContractRead(pair, sourceId, destinationId) }, self.cancel)
		return time.Now().Before(cachedDeadline)
	}
	return self.performActiveContractRead(pair, sourceId, destinationId)
}

// Publishes one admitted result and releases its ownership on all exits.
// Redis reads share the owner join and strictly bounded positive cache.
func (self *residentContractManager) performActiveContractRead(pair model.TransferPair, sourceId, destinationId server.Id) bool {
	defer self.readWorkers.Done()
	var allowance residentContractAllowance
	var active, completed bool
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		handleContractManagerDone(func() {
			allowance = self.readContract(self.ctx, sourceId, destinationId)
			completed = true
		})
	}()
	self.stateLock.Lock()
	delete(self.activeReads, pair)
	now := time.Now()
	active = allowance.active && now.Before(allowance.validUntil)
	if self.ctx.Err() != nil {
		active = false
	}
	if completed {
		if active && self.settings.ContractManagerCheckTimeout > 0 {
			self.activeContracts[pair] = &activeContractEntry{checkTime: now, validUntil: allowance.validUntil}
		} else {
			delete(self.activeContracts, pair)
		}
	}
	self.stateLock.Unlock()
	if panicValue != nil {
		panic(panicValue)
	}
	return active && time.Now().Before(allowance.validUntil)
}

// Closes read admission under the same lock as worker registration. The owner
// cancellation interrupts Redis I/O; existing reads still belong to the join.
func (self *residentContractManager) Close() {
	self.stateLock.Lock()
	self.readsClosed = true
	self.stateLock.Unlock()
	self.cancel()
}

// Joins cold reads and half-window refreshes after admission is closed.
func (self *residentContractManager) CloseAndWait(ctx context.Context) error {
	self.Close()
	return waitForWorkerGroup(ctx, &self.readWorkers, "resident contract reads")
}

// Records only successful reads; denied reads retain admission state, not an
// authorization result.
type activeContractEntry struct {
	checkTime  time.Time
	validUntil time.Time
}

// Source authority and local freshness are independent ceilings. A missing
// source deadline is never an unlimited permission, including after refresh.
func (self *activeContractEntry) deadline(timeout time.Duration) time.Time {
	deadline := self.checkTime.Add(timeout)
	if self.validUntil.Before(deadline) {
		deadline = self.validUntil
	}
	return deadline
}
