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

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026/model"
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

// The mode's name, tcp for the zero value an unset config holds.
func (self localDeliveryTestMode) label() string {
	if self.name == "" {
		return localDeliveryTestTcp.name
	}
	return self.name
}

// The in-process links of each operation over every exchange of the process,
// as the exchange_local metrics count them: those attached now and those
// detached so far.
type localLinkCounts struct {
	attached [exchangeIOOpCount]float64
	detached [exchangeIOOpCount]float64
}

func snapshotLocalLinkCounts() localLinkCounts {
	var counts localLinkCounts
	for _, op := range localLinkOps {
		counts.attached[op] = testutil.ToFloat64(exchangeLocalAttachments[op])
		for reason := localTransportDetachResidentDone; reason < localTransportDetachReasonCount; reason++ {
			if counter := exchangeLocalDetachesCounters[op][reason]; counter != nil {
				counts.detached[op] += testutil.ToFloat64(counter)
			}
		}
	}
	return counts
}

// The links of op that attached after before: the change in attached links
// plus the links that detached in between.
func (self localLinkCounts) attachesSince(before localLinkCounts, op exchangeIOOp) float64 {
	return self.attached[op] - before.attached[op] + self.detached[op] - before.detached[op]
}

// A settings mutator that runs mutate, then sets the mode, for fixtures that
// take one mutator.
func (self localDeliveryTestMode) after(mutate func(*ExchangeSettings)) func(*ExchangeSettings) {
	return func(settings *ExchangeSettings) {
		if mutate != nil {
			mutate(settings)
		}
		self.apply(settings)
	}
}

// Fails unless the in-process links since before are what the mode allows on a
// fixture whose every resident shares its transports' and forwards' exchange:
// in process, at least minTransports transport links and minForwards forward
// links attached; over the socket path, none. On such a fixture an eligible
// transport or forward that dialed instead is a silent fallback.
func (self localDeliveryTestMode) requireLinksSince(t testing.TB, before localLinkCounts, minTransports int, minForwards int) {
	t.Helper()
	after := snapshotLocalLinkCounts()
	transportLinks := after.attachesSince(before, exchangeIOOpTransport)
	forwardLinks := after.attachesSince(before, exchangeIOOpForward)
	t.Logf("[local delivery]mode=%s in_process_transport_links=%v in_process_forward_links=%v", self.label(), transportLinks, forwardLinks)
	if self.local {
		if transportLinks < float64(minTransports) || forwardLinks < float64(minForwards) {
			t.Errorf("mode %s: %v transport and %v forward links attached in process, want at least %d and %d", self.label(), transportLinks, forwardLinks, minTransports, minForwards)
		}
	} else if transportLinks != 0 || forwardLinks != 0 {
		t.Errorf("mode %s: %v transport and %v forward links attached in process, want none", self.label(), transportLinks, forwardLinks)
	}
}
