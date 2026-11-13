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
//
// The process exports the slots its creations hold and the slots its bounds
// have, summed over the bounds its owners have open, so a dashboard reads the
// headroom beside the refusals. A slot counts from its acquire to its release,
// a release run by a panic's unwinding included.
package localclient

import (
	"encoding/base64"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

var contractAdmissionRefusals = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "contract_admission_refusals_total",
		Help:      "Hosted contract creations refused because the owner's concurrent contract creation bound was full",
	},
)

var contractAdmissionInUse = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "contract_admission_in_use",
		Help:      "Hosted contract creation control calls holding a slot of their owner's concurrent contract creation bound",
	},
)

var contractAdmissionCapacity = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "contract_admission_capacity",
		Help:      "Slots of the concurrent contract creation bounds that this process's hosted owners have open",
	},
)

func init() {
	prometheus.MustRegister(contractAdmissionRefusals)
	prometheus.MustRegister(contractAdmissionInUse)
	prometheus.MustRegister(contractAdmissionCapacity)
}

// The concurrent contract creation bound shared by the authorities of one
// owner. A nil admission admits everything. Safe for concurrent use.
type ContractAdmission struct {
	slots     chan struct{}
	closeOnce sync.Once
}

// Nonpositive `maxConcurrentCount` returns nil, which admits everything. The
// bound's slots count toward the process capacity until Close.
func NewContractAdmission(maxConcurrentCount int) *ContractAdmission {
	if maxConcurrentCount <= 0 {
		return nil
	}
	contractAdmissionCapacity.Add(float64(maxConcurrentCount))
	return &ContractAdmission{
		slots: make(chan struct{}, maxConcurrentCount),
	}
}

// Withdraws the bound's slots from the process capacity. The owner calls it
// once every authority that creates contracts through the bound has stopped;
// a later creation is still bounded and counted in use. Idempotent.
func (self *ContractAdmission) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		contractAdmissionCapacity.Sub(float64(cap(self.slots)))
	})
}

// Takes a slot without waiting. A true return must be paired with release.
func (self *ContractAdmission) tryAcquire() bool {
	if self == nil {
		return true
	}
	select {
	case self.slots <- struct{}{}:
		contractAdmissionInUse.Inc()
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
	contractAdmissionInUse.Dec()
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

// The bound's slots, zero for nil (no bound), for owner wiring tests.
func (self *ContractAdmission) Testing_Capacity() int {
	if self == nil {
		return 0
	}
	return cap(self.slots)
}
