package monitor

// Signal pgbouncer-stalls implements SIGNALS.md §2.11.
func NewPgBouncerStallsSignal() Signal {
	return &signalAdapter{number: "2.11", key: "pgbouncer-stalls", name: "PgBouncer client-write path", probe: pgbouncerProbe{}}
}
