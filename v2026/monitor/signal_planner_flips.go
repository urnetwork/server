package monitor

// SIGNALS.md §2.3 maps to signal_planner_flips.go and signal_planner_flips_test.go.
func NewPlannerFlipsSignal() Signal {
	return &signalAdapter{number: "2.3", key: "planner-flips", name: "Planner-flip detection", probe: pgStatsLandmineProbe{}}
}
