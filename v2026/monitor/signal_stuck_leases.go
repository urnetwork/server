package monitor

// Signal stuck-leases implements SIGNALS.md §12.3.
func NewStuckLeasesSignal() Signal {
	return &signalAdapter{
		number: "12.3", key: "stuck-leases", name: "Taskworker stranded leases", probe: taskworkerDrainProbe{},
		accept: acceptProbeIDs("pg/task-lease-stranded"),
	}
}
