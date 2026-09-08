package monitor

// Signal task-canaries implements SIGNALS.md §1.2. It covers the end-to-end
// task canary plus overdue and parked task states.
func NewTaskCanariesSignal() Signal {
	return &signalAdapter{number: "1.2", key: "task-canaries", name: "Task canaries", probe: taskCanaryProbe{}}
}
