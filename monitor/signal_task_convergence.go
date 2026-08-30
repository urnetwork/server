package monitor

// SIGNALS.md §12.4 maps to signal_task_convergence.go and signal_task_convergence_test.go.
func NewTaskConvergenceSignal() Signal {
	return &signalAdapter{
		number: "12.4", key: "task-convergence", name: "Taskworker post-deploy convergence", probe: taskworkerDrainProbe{},
		accept: acceptProbeIDs("pg/task-due-lag", "pg/task-target-missing"),
	}
}
