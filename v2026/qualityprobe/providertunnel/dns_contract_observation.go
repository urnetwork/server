package providertunnel

import (
	"sync/atomic"
	"time"
)

// A true SDK witness proves an outstanding/failed local contract acquisition
// with no attempted provider write in this exact tunnel. False cannot prove
// provider contact, successful credit, or provider fault. No timing of the
// acquisition itself is inferred from the DNS wave's wall residence.
var dnsContractLabels = [...]string{"unobserved", "local_contract_no_provider_write", "not_proved"}

type dnsContractCell struct {
	count  atomic.Uint64
	micros atomic.Uint64
}

type DnsContractObservation struct {
	Result, Evidence string
	Count            uint64
	WaveSeconds      float64
}

func (self *DnsObservations) ContractSnapshot() [len(dnsResultLabels) * len(dnsContractLabels)]DnsContractObservation {
	var rows [len(dnsResultLabels) * len(dnsContractLabels)]DnsContractObservation
	i := 0
	for result, label := range dnsResultLabels {
		for evidence, name := range dnsContractLabels {
			rows[i] = DnsContractObservation{Result: label, Evidence: name}
			if self != nil {
				cell := &self.contract[result][evidence]
				rows[i].Count = cell.count.Load()
				rows[i].WaveSeconds = float64(cell.micros.Load()) / 1e6
			}
			i++
		}
	}
	return rows
}

func (self *DnsObservations) recordContract(result dnsResult, evidence int, elapsed time.Duration) {
	if self == nil || result < 0 || int(result) >= len(dnsResultLabels) || evidence < 0 || evidence >= len(dnsContractLabels) {
		return
	}
	cell := &self.contract[result][evidence]
	cell.micros.Add(uint64(max(0, elapsed.Microseconds())))
	cell.count.Add(1)
}
