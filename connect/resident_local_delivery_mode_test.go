// The in-process delivery modes an exchange under test runs in
// (resident_local_transport.go, resident_local_forward.go): one way for a test
// to set both switches and their code ceilings without Redis, so one test body
// covers the socket path and the in-process path. Integration suites run each
// case in both modes (connectgen's in-process axis), and
// TestLocalDeliveryModesAttachOrDial proves that neither mode silently falls
// back to the other.
package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/server/model"
)

// One way an exchange delivers to a resident of its own: over the exchange
// socket and accept path, or in process. control leaves the code ceilings on
// with their production defaults and reads the real Redis control.
type localDeliveryTestMode struct {
	name string
	// transports and forwards whose resident is in the exchange attach in
	// process, with no Redis read
	local bool
	// the ceilings on with the production defaults, read from the Redis
	// control as in production
	control bool
}

var localDeliveryTestTcp = localDeliveryTestMode{name: "tcp"}
var localDeliveryTestInProcess = localDeliveryTestMode{name: "in_process", local: true}
var localDeliveryTestControl = localDeliveryTestMode{name: "control", control: true}

// The two modes every dual-mode suite runs, the socket path first.
var localDeliveryTestModes = []localDeliveryTestMode{localDeliveryTestTcp, localDeliveryTestInProcess}

// Sets both switches of settings for the mode:
//   - tcp turns both ceilings off, so no switch exists and every transport and
//     forward dials, as in alt and before in-process delivery;
//   - in_process turns both ceilings on with every block allowed and reads a
//     fixed control in place of Redis, so neither a missing Redis nor a control
//     left in the test Redis can turn it off;
//   - control turns both ceilings on with the production defaults (off) and
//     reads the Redis control, as the connect service does.
func (self localDeliveryTestMode) apply(settings *ExchangeSettings) {
	defaults := DefaultExchangeLocalTransportSettings()
	switch {
	case self.local:
		settings.EnableLocalTransport = true
		settings.LocalTransportDefault = model.ConnectLocalTransportAll
		settings.EnableLocalForward = true
		settings.LocalForwardDefault = model.ConnectLocalTransportAll
		settings.localControlReadForTest = func(_ context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
			// a control that sets no field: the defaults above choose
			return model.NarrowConnectMoveControl(nil, ceiling), nil
		}
	case self.control:
		settings.EnableLocalTransport = true
		settings.LocalTransportDefault = defaults.LocalTransportDefault
		settings.EnableLocalForward = true
		settings.LocalForwardDefault = defaults.LocalForwardDefault
		settings.localControlReadForTest = nil
	default:
		settings.EnableLocalTransport = false
		settings.EnableLocalForward = false
		settings.localControlReadForTest = nil
	}
}

// Fails the test unless exchange's switches are what the mode set: in_process,
// both exist and allow attaching; tcp, neither exists. A mode whose settings
// did not reach the exchange would otherwise run the other path unnoticed.
func (self localDeliveryTestMode) requireExchange(t testing.TB, exchange *Exchange) {
	t.Helper()
	switch {
	case self.local:
		if exchange.localTransport == nil || !exchange.localTransport.allows() {
			t.Errorf("mode %s: the exchange's transport switch does not allow attaching", self.name)
		}
		if exchange.localForward == nil || !exchange.localForward.allows() {
			t.Errorf("mode %s: the exchange's forward switch does not allow attaching", self.name)
		}
	case self.control:
		if exchange.localTransport == nil || exchange.localForward == nil {
			t.Errorf("mode %s: the exchange has no switch to read the control", self.name)
		}
	default:
		if exchange.localTransport != nil || exchange.localForward != nil {
			t.Errorf("mode %s: the exchange built an in-process switch", self.name)
		}
	}
}
