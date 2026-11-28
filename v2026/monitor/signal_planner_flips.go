// SIGNALS.md §2.3: daily structural protection for transfer_contract query plans.
package monitor

// Registers the daily three-index/statistics-target guard. A healthy result
// covers that static protection; hourly per-query observation remains
// unimplemented and needs a separate qualified history producer.
func NewPlannerFlipsSignal() Signal {
	return &signalAdapter{number: "2.3", key: "planner-flips", name: "Planner-flip detection", probe: pgStatsLandmineProbe{}}
}
