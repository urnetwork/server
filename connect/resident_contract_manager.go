package main

import (
	"context"
	"sync"
	"time"
    "fmt"

    "golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
)


type residentContractManager struct {
    ctx context.Context
    cancel context.CancelFunc

    clientId bringyour.Id

    settings *ExchangeSettings

    stateLock sync.Mutex
    // unordered transfer pair -> contract ids
    pairContractIds map[model.TransferPair]map[bringyour.Id]bool
}

func newResidentContractManager(
    ctx context.Context,
    cancel context.CancelFunc,
    clientId bringyour.Id,
    settings *ExchangeSettings,
) *residentContractManager {
    residentContractManager := &residentContractManager {
        ctx: ctx,
        cancel: cancel,
        clientId: clientId,
        settings: settings,
        pairContractIds: model.GetOpenContractIdsForSourceOrDestinationWithNoPartialClose(ctx, clientId),
    }

    go bringyour.HandleError(residentContractManager.syncContracts, cancel)

    return residentContractManager
}

func (self *residentContractManager) syncContracts() {
    for {
        select {
        case <- self.ctx.Done():
            return
        case <- time.After(self.settings.ContractSyncTimeout):
        }

        pairContractIds := model.GetOpenContractIdsForSourceOrDestinationWithNoPartialClose(self.ctx, self.clientId)

        func () {
            self.stateLock.Lock()
            defer self.stateLock.Unlock()
            self.pairContractIds = pairContractIds
            // if a contract was added between the sync and set, it will be looked up from the model on miss
        }()

        // FIXME close expired contracts
    }
}



func (self *residentContractManager) HasActiveContract(sourceId bringyour.Id, destinationId bringyour.Id) bool {
    self.stateLock.Lock()
    defer self.stateLock.Unlock()

    transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)    
    contracts, ok := self.pairContractIds[transferPair]
    if !ok {
        contracts = map[bringyour.Id]bool{}
        // match the behavior of `syncContracts`/`GetOpenContractIdsForSourceOrDestinationWithNoPartialClose`
        // look for active contracts in either direction
        maps.Copy(
            contracts,
            model.GetOpenContractIdsWithNoPartialClose(self.ctx, sourceId, destinationId),
        )
        maps.Copy(
            contracts,
            model.GetOpenContractIdsWithNoPartialClose(self.ctx, destinationId, sourceId),
        )
        // if no contracts, store an empty map as a cache miss until the next `syncContracts` iteration
        self.pairContractIds[transferPair] = contracts
    }

    return 0 < len(contracts)
}




func (self *residentContractManager) CreateContractHole(
    destinationId bringyour.Id,
    contractId bringyour.Id,
) error {
    // update the cache
    transferPair := model.NewUnorderedTransferPair(self.clientId, destinationId)
    func() {
        self.stateLock.Lock()
        defer self.stateLock.Unlock()
        contracts, ok := self.pairContractIds[transferPair]
        if !ok {
            contracts = map[bringyour.Id]bool{}
            self.pairContractIds[transferPair] = contracts
        }
        contracts[contractId] = true
    }()

    return nil
}

func (self *residentContractManager) CloseContract(
    contractId bringyour.Id,
    usedTransferByteCount ByteCount,
) error {
    // update the cache
    func() {
        self.stateLock.Lock()
        defer self.stateLock.Unlock()
        for transferPair, contracts := range self.pairContractIds {
            if transferPair.A == self.clientId || transferPair.B == self.clientId {
                delete(contracts, contractId)
            }
        }
    }()

    fmt.Printf("CONTROLLER CLOSE CONTRACT (%s) %s\n", self.clientId.String(), contractId.String())
    err := model.CloseContract(self.ctx, contractId, self.clientId, usedTransferByteCount)
    if err != nil {
        fmt.Printf("CLOSE CONTRACT ERROR %s\n", err)
        return err
    }

    return nil
}
