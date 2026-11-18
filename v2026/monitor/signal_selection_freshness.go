package monitor

// SIGNALS.md §2.8 maps to signal_selection_freshness.go and signal_selection_freshness_test.go.
func NewSelectionFreshnessSignal() Signal {
	return &signalAdapter{number: "2.8", key: "selection-freshness", name: "Provider-selection freshness", probe: pgSelectionFreshnessProbe{}}
}
