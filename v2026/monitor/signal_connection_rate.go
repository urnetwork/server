package monitor

// SIGNALS.md §2.7 maps to signal_connection_rate.go and signal_connection_rate_test.go.
// The stateful instance converts PostgreSQL's cumulative counter to a rate.
func NewConnectionRateSignal() Signal {
	return &signalAdapter{number: "2.7", key: "connection-rate", name: "New-connection rate", probe: &pgConnectRateProbe{}}
}
