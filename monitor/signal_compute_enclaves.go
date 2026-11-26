package monitor

// Signal compute-enclaves implements SIGNALS.md §32.5 (CSRV-M12-06; DESIGN-COMPUTE §14.7, §11.6): the enclaves'
// metrics, enclaves by state, read each minute from Mimir while monitor.yml's compute block enables the enclaves
// family (compute_signals.go). §14.7 names no enclave page, so the family reports only whether its series is current:
// an enabled family without it reads compute-enclaves-unobservable, which says the orchestrator publishes nothing.

// SIGNALS.md §32.5's signal.
func NewComputeEnclavesSignal() Signal {
	return newComputeFamilySignal(computeEnclavesFamily())
}

// The enclaves' family: its liveness alone.
func computeEnclavesFamily() *computeFamily {
	return &computeFamily{
		family: computeFamilyEnclaves, number: "32.5", key: "compute-enclaves", name: "Compute enclaves",
		probeId: "compute/enclaves", liveness: computeMetricEnclaves, unobservableClass: "compute-enclaves-unobservable",
	}
}
