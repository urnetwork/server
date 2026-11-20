// Keep the absent qualified forecast visible independently of the measured
// root-filesystem safety line. No source fetch can supply missing history.
package monitor

import (
	"context"
	"time"
)

// SIGNALS.md §2.4c exposes incomplete forecast coverage; it does not measure
// runway or certify healthy growth. §2.4b owns the independent absolute floor.
func NewPgDiskRunwaySignal() Signal {
	return &signalAdapter{
		number: "2.4c", key: "pg-disk-runway", name: "Database disk runway coverage",
		probe: pgDiskRunwayProbe{},
	}
}

// This explicit unknown has no collector until paired history is qualified.
type pgDiskRunwayProbe struct{}

// Forecast visibility has an independent incident/clear scope.
func (pgDiskRunwayProbe) id() string { return "pg/disk-runway" }

// Missing forecast authority is a warning, never a measured pressure page.
func (pgDiskRunwayProbe) tier() string { return tierWarn }

// Match the independent floor's approved normal observation cadence.
func (pgDiskRunwayProbe) cadence() time.Duration { return 15 * time.Minute }

// Do not substitute raw free-space decline, archive storage or a database
// size series for paired temp/WAL/recovery history. No healthy result exists.
func (pgDiskRunwayProbe) check(ctx context.Context, _ *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []finding{{
		probeId: "pg/disk-runway", tier: tierWarn, class: "pg-disk-runway-unobservable", target: "primary/root", sustain: 2,
		symptom:   "The database's qualified disk-runway forecast is unavailable",
		mechanism: "A complete paired history of filesystem availability, resident temp bytes, WAL and recovery/vacuum state has not been implemented. Raw decline or hourly maxima cannot prove continuing net growth.",
		baseline:  "Approved future policy: WARN after two qualified observations of runway <=48 hours to the 100000000000-byte safety line, at a fifteen-minute cadence.",
		observed:  "forecast=unknown reason=paired-history-not-implemented qualification_window_hours=6 complete_hourly_intervals_required=6",
		evidence:  "This coverage observation makes no remote query, samples no archive volume and does not calculate a slope. The separate pg-disk-space signal measures only present root headroom.",
		context:   "False-positive qualifiers: hourly temp spills can recover and vacuum completion can release held space; neither raw decline nor cumulative temp bytes establishes lasting net growth. False-negative qualifiers: WAL retained outside relation size, missing tablespace coverage and incomplete recovery history leave runway unknown even when current root space is above its safety line.",
		action:    "Complete and qualify the paired historical source contract for six complete hourly intervals before enabling a forecast; keep storage and retention actions with their owners.",
		verify:    "A future implementation must prove complete aligned history, temp recovery and WAL/vacuum authority with healthy, true-depletion and missing-source controls. This visibility-only implementation cannot certify runway or clear an earlier forecast incident.",
		playbook:  "SIGNALS.md §2.4b and §2.4c",
	}}, nil
}
