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
	// Tests hold an admitted callback before controller work begins. Nil is a
	// production no-op.
	beforeHandleControlFramesForTest func()
}

// Creates an owned in-band control boundary for one authenticated resident.
// Its context preserves parent values but outlives transport cancellation so
// an admitted database operation can finish before Resident.CloseAndWait
// closes it after joining the internal client callback tree.
func newResidentController(
	parentCtx context.Context,
	clientId server.Id,
	residentContractManager *residentContractManager,
	settings *ExchangeSettings,
) *residentController {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parentCtx))
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
	if self.beforeHandleControlFramesForTest != nil {
		self.beforeHandleControlFramesForTest()
	}
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

// Ends the detached controller lifetime after every admitted client callback
// has joined.
func (self *residentController) Close() {
	self.cancel()
}

// all controller activity moved to `controller.resident_oob_controller` via the api
