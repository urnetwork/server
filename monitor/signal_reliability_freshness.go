// SIGNALS.md §2.15a checks score publication age independently of reliability values.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

const (
	reliabilityFreshnessWarnAfter = 40 * time.Minute
	reliabilityFreshnessPageAfter = time.Hour
	reliabilityFreshnessDrainAge  = 10 * time.Minute
)

// Fresh, perfectly passing values may still describe an old interval. No product gate is changed.
func NewReliabilityFreshnessSignal() Signal {
	return &signalAdapter{number: "2.15a", key: "reliability-freshness", name: "Client reliability publication freshness", probe: reliabilityFreshnessProbe{}}
}

type reliabilityFreshnessProbe struct{}

func (reliabilityFreshnessProbe) id() string             { return "pg/reliability-freshness" }
func (reliabilityFreshnessProbe) tier() string           { return tierWarn }
func (reliabilityFreshnessProbe) cadence() time.Duration { return 5 * time.Minute }

// Sixteen indexed queue endpoints and at most 48 exact score-key reads, never a fleet/history scan.
const reliabilityFreshnessQuery = `
WITH anchors(slot_id) AS (VALUES (0),(128),(256),(384),(512),(640),(768),(896)),
heads AS MATERIALIZED (
 SELECT h.client_id FROM anchors a CROSS JOIN LATERAL (
  SELECT c.client_id FROM provider_egress_probe_cycle c
  WHERE c.eligible AND c.slot_id=a.slot_id ORDER BY c.next_attempt_at,c.client_id LIMIT 1
 ) h
 UNION ALL
 SELECT h.client_id FROM anchors a CROSS JOIN LATERAL (
  SELECT c.client_id FROM provider_egress_probe_cycle c
  WHERE c.eligible AND c.slot_id=a.slot_id ORDER BY c.next_attempt_at DESC,c.client_id DESC LIMIT 1
 ) h
), sample AS MATERIALIZED (
 SELECT DISTINCT client_id FROM heads
), scores AS MATERIALIZED (
 SELECT v.lookback_index,count(r.client_id) AS score_rows,
  min(r.max_block_number) AS oldest_max_exclusive,max(r.max_block_number) AS newest_max_exclusive
 FROM sample s CROSS JOIN (VALUES (0),(1),(2)) v(lookback_index)
 LEFT JOIN client_connection_reliability_score r
  ON r.client_id=s.client_id AND r.lookback_index=v.lookback_index
 GROUP BY v.lookback_index
), tasks AS MATERIALIZED (
 SELECT count(*) AS rows,
  count(*) FILTER (WHERE claim_time>(statement_timestamp() AT TIME ZONE 'UTC')-interval '30 seconds'
   AND release_time>(statement_timestamp() AT TIME ZONE 'UTC')) AS fresh_claims,
  min(extract(epoch FROM run_at-(statement_timestamp() AT TIME ZONE 'UTC'))) AS due_in_seconds
 FROM pending_task WHERE run_once_key IN ('["update_reliabilities"]','update_reliabilities')
)
SELECT jsonb_build_object(
 'schema',1,'observed_unix',extract(epoch FROM statement_timestamp()),
 'sample_count',(SELECT count(*) FROM sample),
 'drain',(SELECT jsonb_build_object('max_exclusive',max_drained_block+1,
  'update_age_seconds',extract(epoch FROM (statement_timestamp() AT TIME ZONE 'UTC')-update_time))
  FROM client_reliability_rollup WHERE singleton_id=1),
 'task_rows',t.rows,'fresh_claims',t.fresh_claims,'task_due_in_seconds',t.due_in_seconds,
 'windows',(SELECT jsonb_agg(jsonb_build_object(
  'index',v.lookback_index,'running_max_exclusive',w.max_block_number,
  'score_rows',coalesce(s.score_rows,0),'oldest_max_exclusive',s.oldest_max_exclusive,
  'newest_max_exclusive',s.newest_max_exclusive) ORDER BY v.lookback_index)
  FROM (VALUES (0),(1),(2),(1000)) v(lookback_index)
  LEFT JOIN client_reliability_running_window w USING(lookback_index)
  LEFT JOIN scores s USING(lookback_index))
) FROM tasks t;
`

// Nullable markers remain unknown; a missing score never becomes a current zero-age score.
type reliabilityFreshnessWindow struct {
	Index               int    `json:"index"`
	RunningMaxExclusive *int64 `json:"running_max_exclusive"`
	ScoreRows           int    `json:"score_rows"`
	OldestMaxExclusive  *int64 `json:"oldest_max_exclusive"`
	NewestMaxExclusive  *int64 `json:"newest_max_exclusive"`
}

type reliabilityFreshnessSnapshot struct {
	Schema           int                          `json:"schema"`
	ObservedUnix     float64                      `json:"observed_unix"`
	SampleCount      int                          `json:"sample_count"`
	Drain            *reliabilityFreshnessDrain   `json:"drain"`
	TaskRows         int                          `json:"task_rows"`
	FreshClaims      int                          `json:"fresh_claims"`
	TaskDueInSeconds *float64                     `json:"task_due_in_seconds"`
	Windows          []reliabilityFreshnessWindow `json:"windows"`
}

// A present drain row without its timestamp cannot establish freshness.
type reliabilityFreshnessDrain struct {
	MaxExclusive     int64    `json:"max_exclusive"`
	UpdateAgeSeconds *float64 `json:"update_age_seconds"`
}

// Validate the entire fixed shape before drawing a conclusion from any sibling window.
func parseReliabilityFreshness(rows []pgRow) (*reliabilityFreshnessSnapshot, error) {
	if len(rows) != 1 || len(rows[0]) != 1 || len(rows[0][0]) > 64*1024 {
		return nil, fmt.Errorf("reliability freshness observation has invalid shape")
	}
	var snapshot reliabilityFreshnessSnapshot
	decoder := json.NewDecoder(strings.NewReader(rows[0][0]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("reliability freshness observation is invalid JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("reliability freshness observation has trailing data")
	}
	if snapshot.Schema != 1 || snapshot.ObservedUnix <= 0 || math.IsNaN(snapshot.ObservedUnix) || math.IsInf(snapshot.ObservedUnix, 0) || snapshot.SampleCount < 0 || snapshot.SampleCount > 16 || snapshot.TaskRows < 0 || snapshot.TaskRows > 2 || snapshot.FreshClaims < 0 || snapshot.FreshClaims > snapshot.TaskRows || len(snapshot.Windows) != 4 {
		return nil, fmt.Errorf("reliability freshness observation exceeds fixed bounds")
	}
	validMax := func(value int64) bool { return value > 0 && float64(value)*60 <= snapshot.ObservedUnix+60 }
	if snapshot.Drain != nil && (!validMax(snapshot.Drain.MaxExclusive) || snapshot.Drain.UpdateAgeSeconds == nil || *snapshot.Drain.UpdateAgeSeconds < 0) {
		return nil, fmt.Errorf("reliability freshness drain marker is invalid")
	}
	for index, window := range snapshot.Windows {
		if window.Index != [4]int{0, 1, 2, 1000}[index] || window.ScoreRows < 0 || window.ScoreRows > snapshot.SampleCount || window.Index == 1000 && window.ScoreRows != 0 {
			return nil, fmt.Errorf("reliability freshness window shape is invalid")
		}
		if window.RunningMaxExclusive != nil && !validMax(*window.RunningMaxExclusive) {
			return nil, fmt.Errorf("reliability freshness running marker is invalid")
		}
		if window.ScoreRows == 0 {
			if window.OldestMaxExclusive != nil || window.NewestMaxExclusive != nil {
				return nil, fmt.Errorf("reliability freshness empty sample has fabricated bounds")
			}
		} else if window.OldestMaxExclusive == nil || window.NewestMaxExclusive == nil || !validMax(*window.OldestMaxExclusive) || !validMax(*window.NewestMaxExclusive) || *window.OldestMaxExclusive > *window.NewestMaxExclusive {
			return nil, fmt.Errorf("reliability freshness sampled score bounds are invalid")
		}
	}
	return &snapshot, nil
}

// One fixed query returns aggregate metadata only. PostgreSQL failure remains observation failure.
func (reliabilityFreshnessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, reliabilityFreshnessQuery)
	if err != nil {
		return nil, err
	}
	snapshot, err := parseReliabilityFreshness(rows)
	if err != nil {
		return nil, err
	}
	return reliabilityFreshnessFindings(snapshot, pgTarget(env)), nil
}

// Markers measure data freshness, not task liveness or current provider truth.
func reliabilityFreshnessFindings(snapshot *reliabilityFreshnessSnapshot, target string) []finding {
	unknown := func(frame string, reason string) finding {
		return finding{probeId: "pg/reliability-freshness", tier: tierWarn, class: "reliability-freshness-unobservable", target: target, frame: frame, sustain: 2,
			symptom:   "Client reliability freshness is not fully observable: " + reason,
			mechanism: "Missing or partial metadata is unavailable, not proof of a current publication or an empty provider population.",
			baseline:  "A fresh advancing rollup, current running markers and a complete finite sample of physical score rows support the diagnostic.",
			observed:  fmt.Sprintf("sampled_providers=%d task_rows=%d fresh_claims=%d reason=%s", snapshot.SampleCount, snapshot.TaskRows, snapshot.FreshClaims, reason),
			action:    "Check the rollup and exact serialized UpdateReliabilities task using the bounded task-health diagnostics. Do not delete missing/stale scores or alter admission thresholds.",
			verify:    "Repeat the bounded observation and obtain real source markers; absence must not turn into zero age."}
	}
	if snapshot.Drain == nil {
		return []finding{unknown("source", "rollup marker absent")}
	}
	drainEndAge := snapshot.ObservedUnix - float64(snapshot.Drain.MaxExclusive)*60
	if *snapshot.Drain.UpdateAgeSeconds >= reliabilityFreshnessDrainAge.Seconds() || drainEndAge >= reliabilityFreshnessDrainAge.Seconds() {
		return []finding{unknown("source", "rollup is stale or its endpoint has not advanced")}
	}
	findings := []finding{healthyFinding("pg/reliability-freshness", tierWarn, "reliability-freshness-unobservable", target)}
	findings[0].frame = "source"
	ownerState := "unavailable"
	if snapshot.TaskRows == 1 && snapshot.TaskDueInSeconds != nil {
		switch {
		case snapshot.FreshClaims == 1:
			ownerState = "fresh-claim-and-lease"
		case *snapshot.TaskDueInSeconds > 0:
			ownerState = "scheduled"
		default:
			ownerState = "overdue-without-fresh-claim"
		}
	}
	for _, window := range snapshot.Windows[:3] {
		frame := fmt.Sprintf("lookback-%d", window.Index)
		complete := window.RunningMaxExclusive != nil && snapshot.SampleCount == 16 && window.ScoreRows == snapshot.SampleCount && ownerState != "unavailable"
		if !complete {
			findings = append(findings, unknown(frame, fmt.Sprintf("lookback %d has %d physical scores in %d sampled candidates, or missing running/task metadata", window.Index, window.ScoreRows, snapshot.SampleCount)))
		} else {
			healthy := healthyFinding("pg/reliability-freshness", tierWarn, "reliability-freshness-unobservable", target)
			healthy.frame = frame
			findings = append(findings, healthy)
		}
		if window.ScoreRows == 0 {
			continue
		}
		scoreAge := snapshot.ObservedUnix - float64(*window.OldestMaxExclusive)*60
		readyBlocks := snapshot.Drain.MaxExclusive - *window.OldestMaxExclusive
		if scoreAge <= reliabilityFreshnessWarnAfter.Seconds() || readyBlocks <= 0 {
			if complete {
				healthy := healthyFinding("pg/reliability-freshness", tierWarn, "reliability-score-stale", target)
				healthy.frame = frame
				findings = append(findings, healthy)
			}
			continue
		}
		boundary := "running-marker-unavailable"
		runningAge := "unavailable"
		if window.RunningMaxExclusive != nil {
			runningAge = fmt.Sprintf("%.1f", snapshot.ObservedUnix-float64(*window.RunningMaxExclusive)*60)
			boundary = "running-window-lag"
			if *window.RunningMaxExclusive > *window.OldestMaxExclusive {
				boundary = "score-publication-lag"
			}
		}
		severity := tierWarn
		if scoreAge >= reliabilityFreshnessPageAfter.Seconds() {
			severity = tierPage
		}
		findings = append(findings, finding{probeId: "pg/reliability-freshness", tier: severity, class: "reliability-score-stale", target: target, frame: frame, sustain: 2,
			symptom:   fmt.Sprintf("Sampled lookback %d client scores end %.1f minutes ago despite fresh drain", window.Index, scoreAge/60),
			mechanism: "The rolling source can advance without publishing client scores. Long checkpoint/network work plus the completion-plus-30-minute schedule can retain old weights that still pass admission; a fresh claim proves recent worker activity, not score freshness or a dead task.",
			baseline:  "Allow the existing 30-minute cadence plus 10-minute drain grace. Warn beyond 40 minutes and page at 60 minutes after two observations with fresh drain; these are diagnostic bands, not product TTLs.",
			observed:  fmt.Sprintf("lookback_index=%d sampled_providers=%d physical_score_rows=%d oldest_score_age_seconds=%.1f newest_score_age_seconds=%.1f running_age_seconds=%s ready_unincorporated_score_blocks=%d drain_update_age_seconds=%.1f drain_end_age_seconds=%.1f boundary=%s task_state=%s", window.Index, snapshot.SampleCount, window.ScoreRows, scoreAge, snapshot.ObservedUnix-float64(*window.NewestMaxExclusive)*60, runningAge, readyBlocks, *snapshot.Drain.UpdateAgeSeconds, drainEndAge, boundary, ownerState),
			context:   "The fixed due-head/tail sample is biased and is not a native-Quality count, fleet false-positive rate or raw-history audit. Missing histories remain unknown. A window with no usable covered/nondegraded blocks intentionally retains prior scores; confirm that condition before blaming the writer. Current claims and stages do not establish historical execution health. Index 0 is 5-minute ranking, index 1 the 1-hour gate, index 2 the 12-hour gate; index 3 is not emitted by the current writer.",
			action:    "Use bounded per-lookback progress and physical score reads plus exact task status to distinguish active slow work, publication barriers and scheduling gaps. Preserve checkpoints and singleton ownership. Do not restart or duplicate the task, loosen reliability floors, invent a missing-history score, or shorten the product evidence window.",
			verify:    "Require the next physical score sample, not just a completed task or running marker, to advance and clear the age band while drain stays fresh."})
	}
	return findings
}
