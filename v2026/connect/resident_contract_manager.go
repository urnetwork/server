package connect

import (
	"context"
	"sync"
	"time"

	// "fmt"

	// "maps"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type residentContractManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientId server.Id

	settings *ExchangeSettings

	stateLock                   sync.Mutex
	activeContracts             map[model.TransferPair]*activeContractEntry
	activeReads                 map[model.TransferPair]*activeContractRead
	readSequence                uint64
	readContract                func(context.Context, server.Id, server.Id) bool
	nextActiveContractSweepTime time.Time
}

func newResidentContractManager(
	ctx context.Context,
	cancel context.CancelFunc,
	clientId server.Id,
	settings *ExchangeSettings,
) *residentContractManager {
	residentContractManager := &residentContractManager{
		ctx:             ctx,
		cancel:          cancel,
		clientId:        clientId,
		settings:        settings,
		activeContracts: map[model.TransferPair]*activeContractEntry{},
		activeReads:     map[model.TransferPair]*activeContractRead{},
		readContract:    model.HasOpenContractForPair,
	}

	return residentContractManager
}

func handleContractManagerDone(do func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if !server.IsDoneError(recovered) {
				panic(recovered)
			}
		}
	}()
	do()
}

// all other controller activity moved to `controller.resident_oob_controller` via the api

func (self *residentContractManager) HasActiveContract(sourceId server.Id, destinationId server.Id) bool {
	if self.ctx.Err() != nil {
		return false
	}

	transferPair := model.NewTransferPair(sourceId, destinationId)

	// entry is either not expired or nil
	var entry *activeContractEntry
	refresh := false
	var startedSequence uint64

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		startedSequence = self.readSequence
		now := time.Now()
		// A resident can outlive many provider pairs. Reclaim expired positive
		// checks on the next use instead of retaining their historical keys for
		// the resident's lifetime. The existing freshness interval bounds sweep
		// frequency; active reads retain their separate ownership and result.
		if !now.Before(self.nextActiveContractSweepTime) {
			for pair, cached := range self.activeContracts {
				if self.settings.ContractManagerCheckTimeout <= 0 || cached.checkTime.Add(self.settings.ContractManagerCheckTimeout).Before(now) {
					delete(self.activeContracts, pair)
				}
			}
			self.nextActiveContractSweepTime = now.Add(self.settings.ContractManagerCheckTimeout)
		}

		if 0 < self.settings.ContractManagerCheckTimeout {
			var ok bool
			entry, ok = self.activeContracts[transferPair]
			if ok {
				if entry.checkTime.Add(self.settings.ContractManagerCheckTimeout).Before(time.Now()) {
					entry = nil
				} else if !entry.refresh && entry.checkTime.Add(self.settings.ContractManagerCheckTimeout/2).Before(time.Now()) {
					entry.refresh = true
					refresh = true
				}
			}
		}
	}()

	if entry == nil {
		entry = self.readActiveContract(transferPair, sourceId, destinationId, startedSequence, nil)
	} else if refresh {
		go server.HandleError(func() {
			self.readActiveContract(transferPair, sourceId, destinationId, 0, entry)
		})
	}

	return entry != nil
}

// Reads for one pair share their result without holding stateLock during the
// database call or the wait. A negative result from a read that started before
// this invocation must be checked again: a contract may have opened meanwhile.
func (self *residentContractManager) readActiveContract(
	transferPair model.TransferPair,
	sourceId server.Id,
	destinationId server.Id,
	startedSequence uint64,
	refreshEntry *activeContractEntry,
) *activeContractEntry {
	for {
		if self.ctx.Err() != nil {
			return nil
		}
		self.stateLock.Lock()
		entry := self.activeContracts[transferPair]
		if refreshEntry != nil {
			if entry != refreshEntry {
				self.stateLock.Unlock()
				return entry
			}
		} else if entry != nil && 0 < self.settings.ContractManagerCheckTimeout && !entry.checkTime.Add(self.settings.ContractManagerCheckTimeout).Before(time.Now()) {
			self.stateLock.Unlock()
			return entry
		}
		read := self.activeReads[transferPair]
		if read == nil {
			self.readSequence++
			read = &activeContractRead{done: make(chan struct{}), sequence: self.readSequence}
			self.activeReads[transferPair] = read
			self.stateLock.Unlock()
			return self.performActiveContractRead(transferPair, sourceId, destinationId, read)
		}
		read.waiters++
		self.stateLock.Unlock()

		select {
		case <-read.done:
		case <-self.ctx.Done():
		}
		self.stateLock.Lock()
		read.waiters--
		self.stateLock.Unlock()
		if self.ctx.Err() != nil {
			return nil
		}
		if read.panicValue != nil {
			panic(read.panicValue)
		}
		if read.entry != nil || read.sequence > startedSequence {
			return read.entry
		}
	}
}

func (self *residentContractManager) performActiveContractRead(
	transferPair model.TransferPair,
	sourceId server.Id,
	destinationId server.Id,
	read *activeContractRead,
) *activeContractEntry {
	var hasActiveContract, completed bool
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		handleContractManagerDone(func() {
			hasActiveContract = self.readContract(self.ctx, sourceId, destinationId)
			completed = true
		})
	}()

	self.stateLock.Lock()
	if completed {
		if hasActiveContract {
			read.entry = &activeContractEntry{checkTime: time.Now()}
			if 0 < self.settings.ContractManagerCheckTimeout {
				self.activeContracts[transferPair] = read.entry
			}
		} else {
			delete(self.activeContracts, transferPair)
		}
	}
	read.panicValue = panicValue
	delete(self.activeReads, transferPair)
	close(read.done)
	self.stateLock.Unlock()
	if panicValue != nil {
		panic(panicValue)
	}
	return read.entry
}

type activeContractEntry struct {
	checkTime time.Time
	refresh   bool
}

type activeContractRead struct {
	done       chan struct{}
	sequence   uint64
	waiters    int
	entry      *activeContractEntry
	panicValue any
}
