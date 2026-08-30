package monitor

// SIGNALS.md §1.1 maps to signal_contract_rate.go and signal_contract_rate_test.go.
// Keep the catalog number, Probe key, and semantic filename mapping together.
func NewContractRateSignal() Signal {
	return &signalAdapter{number: "1.1", key: "contract-rate", name: "Contract creation rate", probe: pgContractRateProbe{}}
}
