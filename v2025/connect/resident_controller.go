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

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/controller"

	// "github.com/urnetwork/connect/v2025"
	"github.com/urnetwork/connect/protocol"
)

type residentController struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientId server.Id

	residentContractManager *residentContractManager
	settings                *ExchangeSettings
}

func newResidentController(
	ctx context.Context,
	cancel context.CancelFunc,
	clientId server.Id,
	residentContractManager *residentContractManager,
	settings *ExchangeSettings,
) *residentController {
	return &residentController{
		ctx:                     ctx,
		cancel:                  cancel,
		clientId:                clientId,
		residentContractManager: residentContractManager,
		settings:                settings,
	}
}

// the frames are verified from source `clientId`
// control messages are not allowed to have replies
// messages with replies must use resident_oob_controller in the api
func (self *residentController) HandleControlFrames(frames []*protocol.Frame) error {
	outFrames, err := controller.ConnectControlFrames(
		self.ctx,
		self.clientId,
		frames,
	)
	if err != nil {
		return err
	}

	if 0 < len(outFrames) {
		glog.Infof("[rr]dropped control reply frames: %d\n", len(outFrames))
	}

	return nil
}

// all controller activity moved to `controller.resident_oob_controller` via the api
