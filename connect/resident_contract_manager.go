package main

import (
	"context"
	// "sync"
	// "time"
    // "fmt"

    // "golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	// "bringyour.com/bringyour/model"
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

// all controller activity moved to `controller.resident_oob_controller` via the api
