package monitor

// Signal pg-state implements SIGNALS.md §1.3. One PostgreSQL state snapshot
// yields the active-pileup, idle-in-transaction, and zombie-transaction alerts.
func NewPostgresStateSignal() Signal {
	return &signalAdapter{number: "1.3", key: "pg-state", name: "PostgreSQL transaction state", probe: newPgStateProbe()}
}
