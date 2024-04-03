package main

import (
	"context"
	// "sync"
	// "time"
    "fmt"

    // "golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
)


type residentContractManager struct {
    ctx context.Context
    cancel context.CancelFunc

    clientId bringyour.Id

    settings *ExchangeSettings
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
    }

    return residentContractManager
}



func (self *residentContractManager) CloseContract(
    contractId bringyour.Id,
    usedTransferByteCount ByteCount,
    checkpoint bool,
) error {

    fmt.Printf("CONTROLLER CLOSE CONTRACT (%s) %s\n", self.clientId.String(), contractId.String())
    err := model.CloseContract(self.ctx, contractId, self.clientId, usedTransferByteCount, checkpoint)
    if err != nil {
        fmt.Printf("CLOSE CONTRACT ERROR %s\n", err)
        return err
    }

    return nil
}
