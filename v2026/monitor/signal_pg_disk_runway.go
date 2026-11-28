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
		evidence:  "The independent pg-disk-space signal measures current root headroom against the 100 GB line. The storage-runway signal estimates time to that line from the fastest raw decline over 1, 6 and 24 hours.",
		context:   "False-positive qualifiers: hourly temp spills can recover and vacuum completion can release held space; neither raw decline nor cumulative temp bytes establishes lasting net growth. False-negative qualifiers: WAL retained outside relation size, missing tablespace coverage and incomplete recovery history leave runway unknown even when current root space is above its safety line.",
		action:    "Check pg-disk-space for a floor crossing and storage-runway for a raw runway warning or page on the primary root. With the database and storage owners, qualify six complete hourly intervals pairing root availability, temp bytes physically allocated on that filesystem, WAL occupancy and retention, and vacuum/recovery state at their original collection times.",
		verify:    "A future implementation must prove complete aligned history, temp recovery and WAL/vacuum authority with healthy, true-depletion and missing-source controls. This visibility-only implementation cannot certify runway or clear an earlier forecast incident.",
		playbook:  "SIGNALS.md §2.4b, §2.4c and §2.4f",
	}}, nil
}
