package monitor

// SIGNALS.md §2.6 maps to signal_open_contracts.go and signal_open_contracts_test.go.
func NewOpenContractsSignal() Signal {
	return &signalAdapter{number: "2.6", key: "open-contracts", name: "Open-contract set size", probe: &pgOpenSetProbe{}}
}
