package monitor

// SIGNALS.md §2.5 maps to signal_task_health.go and signal_task_health_test.go.
func NewTaskHealthSignal() Signal {
	return &signalAdapter{number: "2.5", key: "task-health", name: "Task-system meta-health", probe: taskDurationProbe{}}
}
