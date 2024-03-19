package main

import (
	"context"
	"sync"
	"time"

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
        pairContractIds: model.GetOpenContractIdsForSourceOrDestination(ctx, clientId),
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

        pairContractIds_ := model.GetOpenContractIdsForSourceOrDestination(self.ctx, self.clientId)
        self.stateLock.Lock()
        self.pairContractIds = pairContractIds_
        // if a contract was added between the sync and set, it will be looked up from the model on miss
        self.stateLock.Unlock()

        // FIXME close expired contracts
    }
}

// this is the "min" or most specific relationship
func (self *residentContractManager) GetProvideRelationship(sourceId bringyour.Id, destinationId bringyour.Id) model.ProvideMode {
    if sourceId == ControlId || destinationId == ControlId {
        return model.ProvideModeNetwork
    }

    if sourceId == destinationId {
        return model.ProvideModeNetwork
    }

    if sourceClient := model.GetNetworkClient(self.ctx, sourceId); sourceClient != nil {
        if destinationClient := model.GetNetworkClient(self.ctx, destinationId); destinationClient != nil {
            if sourceClient.NetworkId == destinationClient.NetworkId {
                return model.ProvideModeNetwork
            }
        }
    }

    // TODO network and friends-and-family not implemented yet
    // FIXME these exist in the association model now, can be added

    return model.ProvideModePublic
}

func (self *residentContractManager) GetProvideMode(destinationId bringyour.Id) model.ProvideMode {

    if destinationId == ControlId {
        return model.ProvideModeNetwork
    }

    provideMode, err := model.GetProvideMode(self.ctx, destinationId)
    if err != nil {
        return model.ProvideModeNone
    }
    return provideMode
}


func (self *residentContractManager) HasActiveContract(sourceId bringyour.Id, destinationId bringyour.Id) bool {
    transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)

    self.stateLock.Lock()
    contracts, ok := self.pairContractIds[transferPair]
    self.stateLock.Unlock()

    if !ok {
        contractIds := model.GetOpenContractIds(self.ctx, sourceId, destinationId)
        contracts := map[bringyour.Id]bool{}
        for _, contractId := range contractIds {
            contracts[contractId] = true
        }
        self.stateLock.Lock()
        // if no contracts, store an empty map as a cache miss until the next `syncContracts` iteration
        self.pairContractIds[transferPair] = contracts
        self.stateLock.Unlock()
    }

    return 0 < len(contracts)
}

func (self *residentContractManager) CreateContract(
    sourceId bringyour.Id,
    destinationId bringyour.Id,
    companionContract bool,
    transferByteCount ByteCount,
    provideMode model.ProvideMode,
) (contractId bringyour.Id, contractTransferByteCount ByteCount, returnErr error) {
    sourceNetworkId, err := model.FindClientNetwork(self.ctx, sourceId)
    if err != nil {
        // the source is not a real client
        returnErr = err
        return
    }
    destinationNetworkId, err := model.FindClientNetwork(self.ctx, destinationId)
    if err != nil {
        // the destination is not a real client
        returnErr = err
        return
    }
    
    contractTransferByteCount = max(self.settings.MinContractTransferByteCount, transferByteCount)

    if provideMode < model.ProvideModePublic {
        contractId, err = model.CreateContractNoEscrow(
            self.ctx,
            sourceNetworkId,
            sourceId,
            destinationNetworkId,
            destinationId,
            contractTransferByteCount,
        )
        if err != nil {
            returnErr = err
            return
        }
    } else if companionContract {
    	escrow, err := model.CreateCompanionTransferEscrow(
            self.ctx,
            sourceNetworkId,
            sourceId,
            destinationNetworkId,
            destinationId,
            contractTransferByteCount,
        )
        if err != nil {
            returnErr = err
            return
        }
        contractId = escrow.ContractId
    } else {
        escrow, err := model.CreateTransferEscrow(
            self.ctx,
            sourceNetworkId,
            sourceId,
            destinationNetworkId,
            destinationId,
            contractTransferByteCount,
        )
        if err != nil {
            returnErr = err
            return
        }
        contractId = escrow.ContractId
    }

    // update the cache
    transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)
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

    return
}

func (self *residentContractManager) CloseContract(
    contractId bringyour.Id,
    clientId bringyour.Id,
    usedTransferByteCount ByteCount,
) error {
    // update the cache
    func() {
        self.stateLock.Lock()
        defer self.stateLock.Unlock()
        for transferPair, contracts := range self.pairContractIds {
            if transferPair.A == clientId || transferPair.B == clientId {
                delete(contracts, contractId)
            }
        }
    }()

    err := model.CloseContract(self.ctx, contractId, clientId, usedTransferByteCount)
    if err != nil {
        return err
    }

    return nil
}
