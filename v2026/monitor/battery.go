// Escalation batteries: the perishable evidence-collection steps from the
// SIGNALS.md §5 playbooks, run once when a signal trips so the ticket arrives
// pre-loaded with the measurements a diagnostician would run first. Batteries
// are read-only and bounded; a battery error degrades to a note in the
// evidence, never a probe failure.
package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// batteryLatch runs a probe's escalation battery once per trip: the battery
// fires when its finding key transitions healthy -> broken, and a healthy tick
// re-arms it. While the key stays broken, the trip-time output is reattached
// to each subsequent finding (annotated with when it was collected), so ticket
// updates keep their evidence without re-landing battery load on an
// already-sick target every tick. One latch per probe instance; a probe never
// overlaps itself, but the lock keeps the map safe regardless.
type batteryLatch struct {
	lock sync.Mutex
	// finding key -> battery output captured when the key tripped; a key is
	// present exactly while its finding is broken (disarmed)
	tripped map[string]trippedBattery
	now     func() time.Time
}

type trippedBattery struct {
	at       time.Time
	evidence string
}

func newBatteryLatch() *batteryLatch {
	return &batteryLatch{
		tripped: map[string]trippedBattery{},
		now:     time.Now,
	}
}

// broken folds one broken tick of key, invoking battery only on the first
// broken tick since the key was last healthy. Returns the evidence to attach:
// fresh output on the trip, the cached trip-time output afterward.
func (self *batteryLatch) broken(key string, battery func() string) string {
	cached, alreadyTripped := func() (trippedBattery, bool) {
		self.lock.Lock()
		defer self.lock.Unlock()
		cached, ok := self.tripped[key]
		return cached, ok
	}()
	if alreadyTripped {
		return cached.evidence + fmt.Sprintf(
			"\n(battery collected once at trip %s; re-runs after a healthy tick re-arms)",
			cached.at.UTC().Format(time.RFC3339))
	}
	// run the battery outside the lock — batteries block (15s snapshot deltas)
	evidence := battery()
	func() {
		self.lock.Lock()
		defer self.lock.Unlock()
		self.tripped[key] = trippedBattery{at: self.now(), evidence: evidence}
	}()
	return evidence
}

// healthy folds one healthy tick of key, re-arming its battery so the next
// trip collects fresh evidence.
func (self *batteryLatch) healthy(key string) {
	self.lock.Lock()
	defer self.lock.Unlock()
	delete(self.tripped, key)
}

// planWallBattery is SIGNALS.md 5.8 steps 2–3, the discriminators for a
// query-plan cpu wall beyond the query_id grouping (which activeBattery
// already collects):
//   - a 15s pg_stat_statements counter delta for the top active query_ids
//     (current per-call mean far above lifetime mean = the step-change tell);
//   - a 15s idx_scan snapshot delta on transfer_contract (the intended pair
//     indexes at 0 while one wrong index takes all scans);
//   - the pg_stats landmine check on transfer_contract.open (n_distinct=1 /
//     mcv {v}@1.0 = the 2.3 rare-value poisoning).
//
// Two snapshots are taken 15s apart and diffed here — temp tables are
// unavailable to a read-only session, and this keeps no backend occupied in
// between. Only validated numeric fields and fixed labels leave this helper;
// malformed optional evidence does not erase the independent state violation.
func planWallBattery(ctx context.Context, env *probeEnv) string {
	parts := []string{}

	snapSql := `
		SELECT 'stmt', s.queryid::text, s.calls::text, round(s.total_exec_time)::text
		FROM pg_stat_statements s
		WHERE s.queryid IN (
			SELECT query_id FROM pg_stat_activity
			WHERE backend_type='client backend' AND state='active' AND query_id IS NOT NULL
			GROUP BY query_id ORDER BY count(*) DESC LIMIT 3
		)
		UNION ALL
		SELECT 'idx', indexrelname, idx_scan::text, '0'
		FROM pg_stat_user_indexes WHERE relname='transfer_contract';
	`
	type stmtSnap struct {
		calls   float64
		totalMs float64
	}
	parseSnap := func(rows []pgRow) (map[string]stmtSnap, map[string]float64, error) {
		stmtSnaps := map[string]stmtSnap{}
		idxScans := map[string]float64{}
		for _, r := range rows {
			if len(r) != 4 {
				return nil, nil, fmt.Errorf("invalid response for plan-wall snapshot")
			}
			count, countErr := strconv.ParseInt(r.str(2), 10, 64)
			totalMs, totalErr := strconv.ParseFloat(r.str(3), 64)
			if countErr != nil || count < 0 || totalErr != nil || totalMs < 0 || math.IsNaN(totalMs) || math.IsInf(totalMs, 0) || math.Trunc(totalMs) != totalMs {
				return nil, nil, fmt.Errorf("invalid response for plan-wall snapshot")
			}
			switch r.str(0) {
			case "stmt":
				queryId, err := strconv.ParseInt(r.str(1), 10, 64)
				if err != nil {
					return nil, nil, fmt.Errorf("invalid response for plan-wall snapshot")
				}
				stmtSnaps[strconv.FormatInt(queryId, 10)] = stmtSnap{calls: float64(count), totalMs: totalMs}
			case "idx":
				if r.str(1) == "" || totalMs != 0 {
					return nil, nil, fmt.Errorf("invalid response for plan-wall snapshot")
				}
				idxScans[r.str(1)] = float64(count)
			default:
				return nil, nil, fmt.Errorf("invalid response for plan-wall snapshot")
			}
		}
		return stmtSnaps, idxScans, nil
	}

	rowsA, errA := env.runner.pg(ctx, snapSql)
	select {
	case <-ctx.Done():
		return "battery canceled"
	case <-time.After(15 * time.Second):
	}
	rowsB, errB := env.runner.pg(ctx, snapSql)
	var stmtSnapsA, stmtSnapsB map[string]stmtSnap
	var idxScansA, idxScansB map[string]float64
	if errA == nil {
		stmtSnapsA, idxScansA, errA = parseSnap(rowsA)
	}
	if errB == nil {
		stmtSnapsB, idxScansB, errB = parseSnap(rowsB)
	}
	if errA != nil || errB != nil {
		if errA != nil {
			parts = append(parts, "snapshot delta failed: first_error_class="+classifyObservationError(errA))
		}
		if errB != nil {
			parts = append(parts, "snapshot delta failed: second_error_class="+classifyObservationError(errB))
		}
	} else {
		parts = append(parts, "pg_stat_statements 15s delta (current vs lifetime ms/call):")
		for queryId, b := range stmtSnapsB {
			a, ok := stmtSnapsA[queryId]
			if !ok {
				continue
			}
			calls := b.calls - a.calls
			currentMs := 0.0
			if calls > 0 {
				currentMs = (b.totalMs - a.totalMs) / calls
			}
			lifetimeMs := 0.0
			if b.calls > 0 {
				lifetimeMs = b.totalMs / b.calls
			}
			parts = append(parts, fmt.Sprintf("  qid=%s calls_15s=%.0f current=%.1fms lifetime=%.1fms",
				queryId, calls, currentMs, lifetimeMs))
		}
		parts = append(parts, "transfer_contract idx_scan 15s delta:")
		type indexDelta struct {
			name  string
			delta float64
		}
		indexDeltas := []indexDelta{}
		for name, b := range idxScansB {
			indexDeltas = append(indexDeltas, indexDelta{name: name, delta: b - idxScansA[name]})
		}
		sort.Slice(indexDeltas, func(i, j int) bool { return indexDeltas[i].delta > indexDeltas[j].delta })
		// Names identify source-owned roles, not verified index definitions.
		indexRole := func(name string) string {
			switch name {
			case "transfer_contract_unresolved_source_pair_create_time":
				return "source-pair"
			case "transfer_contract_unresolved_destination_pair_create_time":
				return "destination-pair"
			case "transfer_contract_unresolved_payer_transfer_byte_count":
				return "payer"
			case "transfer_contract_pair_open_create_time":
				return "legacy-pair"
			case "transfer_contract_open_partial_create_time":
				return "legacy-open-create-time"
			default:
				return "other-name-withheld"
			}
		}
		for i, d := range indexDeltas {
			if i >= 6 {
				break
			}
			parts = append(parts, fmt.Sprintf("  rank=%d role=%s delta=%.0f", i+1, indexRole(d.name), d.delta))
		}
	}

	// the 2.3 rare-value landmine check
	rows, err := env.runner.pg(ctx, `
		SELECT n_distinct::text,
		       coalesce(most_common_vals::text, '-'),
		       coalesce(array_to_string(most_common_freqs, ','), '-')
		FROM pg_stats WHERE tablename='transfer_contract' AND attname='open';
	`)
	if err != nil {
		parts = append(parts, "pg_stats check failed: error_class="+classifyObservationError(err))
	} else if len(rows) > 0 {
		// The selected generated column is Boolean. Multiple rows are ambiguous
		// because the existing query does not constrain the schema.
		projectStats := func() string {
			invalid := "pg_stats check failed: error_class=" + observationErrorClassInvalidResponse
			if len(rows) != 1 || len(rows[0]) != 3 {
				return invalid
			}
			r := rows[0]
			nDistinct, err := strconv.ParseFloat(r.str(0), 64)
			if err != nil || math.IsNaN(nDistinct) || math.IsInf(nDistinct, 0) || nDistinct < -1 || nDistinct > 2 || (nDistinct > 0 && nDistinct != 1 && nDistinct != 2) {
				return invalid
			}
			mcv, frequencies := "unavailable", "unavailable"
			valueCount := 0
			switch r.str(1) {
			case "-":
				if r.str(2) != "-" {
					return invalid
				}
			case "{}":
				if r.str(2) != "" {
					return invalid
				}
			case "{f}", "{t}":
				mcv, valueCount = r.str(1), 1
			case "{f,t}", "{t,f}":
				mcv, valueCount = r.str(1), 2
			default:
				return invalid
			}
			positiveFrequencies, totalFrequency := 0, 0.0
			if valueCount > 0 {
				values := strings.Split(r.str(2), ",")
				if len(values) != valueCount || nDistinct == 0 || (nDistinct == 1 && valueCount != 1) {
					return invalid
				}
				projected := make([]string, len(values))
				for i, raw := range values {
					frequency, err := strconv.ParseFloat(raw, 64)
					if err != nil || math.IsNaN(frequency) || math.IsInf(frequency, 0) || frequency < 0 || frequency > 1 {
						return invalid
					}
					if frequency > 0 {
						positiveFrequencies++
					}
					totalFrequency += frequency
					projected[i] = strconv.FormatFloat(frequency, 'g', -1, 64)
				}
				// pg_stats frequencies are real (float32), so allow rounding at 1.
				if totalFrequency > 1.000001 {
					return invalid
				}
				frequencies = strings.Join(projected, ",")
			}
			verdict := "unknown (Boolean statistics do not establish a complete two-valued sample)"
			if nDistinct == 2 && valueCount == 2 && positiveFrequencies == 2 {
				verdict = "both Boolean values present in sampled statistics; plan health unproved"
			} else if nDistinct == 1 && valueCount == 1 && totalFrequency == 1 {
				verdict = "single-valued Boolean sample (2.3): confirm the legacy-reader plan before the bounded target-300 column-only ANALYZE procedure; verify the isolated pair/payer predicate-index migration for durable protection"
			}
			return fmt.Sprintf("pg_stats transfer_contract.open: n_distinct=%s mcv=%s freqs=%s -> %s",
				strconv.FormatFloat(nDistinct, 'g', -1, 64), mcv, frequencies, verdict)
		}
		parts = append(parts, projectStats())
	}

	return strings.Join(parts, "\n")
}

// logWindowBattery pulls the recent log window for a service around an
// incident (non-tail escalation read; the standing tailers are §3.7 of the
// design).
func logWindowBattery(ctx context.Context, env *probeEnv, service string, since time.Duration, query string) string {
	args := []string{"logs", env.cfg.env, service, "--since=" + since.String(), "--limit=200"}
	if query != "" {
		args = append(args, "--query="+query)
	}
	out, err := env.runner.warpctl(ctx, args...)
	if err != nil {
		return fmt.Sprintf("warpctl logs %s failed: %s", service, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 20 {
		lines = append(lines[:20], fmt.Sprintf("... (%d more)", len(lines)-20))
	}
	return strings.Join(lines, "\n")
}
