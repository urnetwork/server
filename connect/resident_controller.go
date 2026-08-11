package connect

import (
	"context"
	// "sync"
	// "errors"
	// "fmt"

	// "crypto/hmac"
	// "crypto/sha256"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Applies verified resident control frames that cannot carry in-band replies.
type residentController struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientId server.Id

	residentContractManager *residentContractManager
	settings                *ExchangeSettings

	// Tests retain exact dropped response bytes before their final return.
	// Nil is a production no-op.
	beforeDroppedResponseReturnForTest func([]byte)
}

// Creates the in-band control boundary for one authenticated resident.
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

// HandleControlFrames applies frames verified as originating from clientId.
// In-band control cannot carry replies; requests that need a response use the
// API out-of-band controller. Any unexpected replies are dropped here, so this
// boundary must return their pooled payloads.
func (self *residentController) HandleControlFrames(frames []*protocol.Frame) error {
	outFrames, err := controller.ConnectControlFrames(
		self.ctx,
		self.clientId,
		frames,
		self.settings.ContractManagerSettings,
	)
	defer func() {
		for _, frame := range outFrames {
			if self.beforeDroppedResponseReturnForTest != nil {
				self.beforeDroppedResponseReturnForTest(frame.MessageBytes)
			}
			clientconnect.MessagePoolReturn(frame.MessageBytes)
		}
	}()
	if err != nil {
		return err
	}

	if 0 < len(outFrames) {
		glog.Infof("[rr]dropped control reply frames: %d\n", len(outFrames))
	}

	return nil
}

// all controller activity moved to `controller.resident_oob_controller` via the api
