package main

import (
	"context"
	"sync"
	"time"

	// "fmt"

	// "golang.org/x/exp/maps"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type residentContractManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientId server.Id

	settings *ExchangeSettings

	stateLock       sync.Mutex
	activeContracts map[model.TransferPair]*activeContractEntry
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
	}

	return residentContractManager
}

// all other controller activity moved to `controller.resident_oob_controller` via the api

func (self *residentContractManager) HasActiveContract(sourceId server.Id, destinationId server.Id) bool {
	transferPair := model.NewTransferPair(sourceId, destinationId)

	// entry is either not expired or nil
	var entry *activeContractEntry
	refresh := false

	if 0 < self.settings.ContractManagerCheckTimeout {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

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
		}()
	}

	next := func() (nextEntry *activeContractEntry) {
		c := func() bool {
			contractIds := model.GetOpenContractIdsWithNoPartialClose(self.ctx, sourceId, destinationId)
			return 0 < len(contractIds)
		}
		hasActiveContract := c()

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if hasActiveContract {
				nextEntry = &activeContractEntry{
					checkTime: time.Now(),
					refresh:   false,
				}
				self.activeContracts[transferPair] = nextEntry
			} else {
				delete(self.activeContracts, transferPair)
			}
		}()
		return
	}

	if entry == nil {
		entry = next()
	} else if refresh {
		go next()
	}

	return entry != nil
}

type activeContractEntry struct {
	checkTime time.Time
	refresh   bool
}
