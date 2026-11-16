package monitor

// SIGNALS.md §2.4 maps to signal_vacuum_health.go and signal_vacuum_health_test.go.
func NewVacuumHealthSignal() Signal {
	return &signalAdapter{number: "2.4", key: "vacuum-health", name: "Vacuum health", probe: pgVacuumProbe{}}
}
