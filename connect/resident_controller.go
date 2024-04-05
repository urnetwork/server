package main

import (
	"context"
	// "sync"
	// "errors"
	// "fmt"

	// "crypto/hmac"
	// "crypto/sha256"

	// "google.golang.org/protobuf/proto"

	"github.com/golang/glog"

	"bringyour.com/bringyour"
	// "bringyour.com/bringyour/model"
	// "bringyour.com/connect"
	// "bringyour.com/protocol"
)


type residentController struct {
	ctx context.Context
    cancel context.CancelFunc

    clientId bringyour.Id

	residentContractManager *residentContractManager
	settings *ExchangeSettings
}

func newResidentController(
	ctx context.Context,
    cancel context.CancelFunc,
    clientId bringyour.Id,
    residentContractManager *residentContractManager,
    settings *ExchangeSettings,
) *residentController {
	return &residentController{
		ctx: ctx,
		cancel: cancel,
		clientId: clientId,
		residentContractManager: residentContractManager,
		settings: settings,
	}
}

// the message is verified from source `clientId`
// control messages are not allowed to have replies
// messages with replies must use resident_oob_controller in the api
func (self *residentController) HandleControlMessage(message any) {
	switch v := message.(type) {
	default:
		glog.Infof("[resident]Unknown control message: %T", v)
	}
}

// all controller activity moved to `controller.resident_oob_controller` via the api
