// A hosted owner bounds the contract creations its devices run at once. A
// hosted device's control runs in its owner's process, one goroutine per
// request, with nothing in front of the controller. Around the connect deploys
// of 2026-10-25 and 2026-10-26 the windows of a proxy process requested
// contracts together, escrow slowed, and each process piled up thousands of
// creations, every one holding its goroutine, database and Redis work, and
// compensation. Past the bound a request that only creates contracts is refused
// at once, before any credential or database read. The device counts the
// refusal as a failed control round trip, which also feeds its process-wide
// degraded-backend gate, and its send sequence asks again on its own 1 to 5 s
// schedule. Control that closes or settles state is never refused.
package localclient

import (
	"encoding/base64"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

var contractAdmissionRefusals = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "contract_admission_refusals_total",
		Help:      "Hosted contract creations refused because the owner's concurrent contract creation bound was full",
	},
)

func init() {
	prometheus.MustRegister(contractAdmissionRefusals)
}

// The concurrent contract creation bound shared by the authorities of one
// owner. A nil admission admits everything. Safe for concurrent use.
type ContractAdmission struct {
	slots chan struct{}
}

// Nonpositive `maxConcurrentCount` returns nil, which admits everything.
func NewContractAdmission(maxConcurrentCount int) *ContractAdmission {
	if maxConcurrentCount <= 0 {
		return nil
	}
	return &ContractAdmission{
		slots: make(chan struct{}, maxConcurrentCount),
	}
}

// Takes a slot without waiting. A true return must be paired with release.
func (self *ContractAdmission) tryAcquire() bool {
	if self == nil {
		return true
	}
	select {
	case self.slots <- struct{}{}:
		return true
	default:
		contractAdmissionRefusals.Inc()
		return false
	}
}

func (self *ContractAdmission) release() {
	if self == nil {
		return
	}
	<-self.slots
}

// A pack is bounded only when every frame creates a contract. A close, a
// provide or a key in the same pack keeps it unbounded, because refusing it
// would withhold the release of escrow or state. A pack that does not decode is
// left to the controller, which rejects it.
func createsContractsOnly(encodedPack string) bool {
	packBytes, err := base64.StdEncoding.DecodeString(encodedPack)
	if err != nil {
		return false
	}
	pack := &protocol.Pack{}
	if err := proto.Unmarshal(packBytes, pack); err != nil || len(pack.Frames) == 0 {
		return false
	}
	for _, frame := range pack.Frames {
		if frame.GetMessageType() != protocol.MessageType_TransferCreateContract {
			return false
		}
	}
	return true
}

// The refusal reads as a rate limit. No contract was attempted, so there is no
// escrow to release and no result for the device to close.
func contractAdmissionRefusedError() error {
	return &connect.HttpStatusError{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Body:       []byte("hosted contract creation is at its concurrent bound"),
	}
}

// The owner's bound this authority creates contracts through, for owner wiring
// tests.
func (self *Authority) Testing_ContractAdmission() *ContractAdmission {
	return self.contractAdmission
}
