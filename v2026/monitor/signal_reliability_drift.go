package monitor

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

const (
	reliabilityDriftLookbackIndex         = 2
	reliabilityDriftClassificationVersion = 1
	reliabilityDriftMinimumWeight         = 0.7
	reliabilityDriftMinimumRows           = 1000
	reliabilityDriftPassingDenominator    = 1000
)

// reliabilityGateEffectivelyEmpty rejects a lone outlier from clearing a
// fleet-wide provider gate collapse. Below the minimum corpus another signal
// owns bootstrap/empty-state visibility; at scale, fewer than one passing row
// per thousand scored rows is conservatively unusable provider diversity.
func reliabilityGateEffectivelyEmpty(scoreRows int, passingRows int) bool {
	return scoreRows >= reliabilityDriftMinimumRows &&
		int64(passingRows)*reliabilityDriftPassingDenominator < int64(scoreRows)
}

// Signal reliability-drift implements SIGNALS.md §2.15. It checks the
// materialized 12-hour provider reliability gate and the durable degraded-block
// classification generation. The query deliberately reads the optional
// version/token through to_jsonb(row): a monitor built with this signal remains
// compatible with production before the accompanying migrations are applied.
func NewReliabilityDriftSignal() Signal {
	return &signalAdapter{
		number: "2.15", key: "reliability-drift", name: "Provider reliability running-sum integrity",
		probe: pgReliabilityDriftProbe{},
	}
}

type pgReliabilityDriftProbe struct{}

func (pgReliabilityDriftProbe) id() string             { return "pg/reliability-drift" }
func (pgReliabilityDriftProbe) tier() string           { return tierPage }
func (pgReliabilityDriftProbe) cadence() time.Duration { return 5 * time.Minute }

func (pgReliabilityDriftProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, `
		WITH target_window AS MATERIALIZED (
			SELECT
				w.min_block_number,
				w.max_block_number,
				w.last_recompute_block,
				COALESCE((to_jsonb(w)->>'degraded_classification_version')::int, 0) AS classification_version,
				COALESCE(to_jsonb(w)->>'degraded_classification_write_token', '') <> '' AS classification_write_token_present
			FROM (VALUES (1)) seed(singleton)
			LEFT JOIN client_reliability_running_window w ON w.lookback_index = 2
		), classification_guard AS (
			SELECT EXISTS (
				SELECT 1
				FROM pg_trigger t
				JOIN pg_proc p ON p.oid = t.tgfoid
				WHERE
					t.tgrelid = 'client_reliability_running_window'::regclass AND
					t.tgname = 'client_reliability_running_window_classification_guard' AND
					p.proname = 'client_reliability_running_window_classification_guard' AND
					t.tgenabled IN ('O', 'A') AND
					NOT t.tgisinternal
			) AS present
		), score_stats AS (
			SELECT
				COUNT(*) AS score_rows,
				COUNT(*) FILTER (WHERE independent_reliability_weight >= 0.7) AS passing_rows,
				COALESCE(MAX(independent_reliability_weight), 0) AS max_weight,
				COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY independent_reliability_weight), 0) AS median_weight
			FROM client_connection_reliability_score
			WHERE lookback_index = 2
		), running_stats AS (
			SELECT
				COALESCE(MAX(independent_sum), 0) AS max_running_sum,
				COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY independent_sum), 0) AS median_running_sum
			FROM client_reliability_running
			WHERE lookback_index = 2
		), current_block_stats AS (
			SELECT
				COUNT(b.block_number) AS block_rows,
				COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY b.valid_client_count), 0) AS median_valid_clients
			FROM target_window w
			LEFT JOIN client_reliability_block b ON
				w.min_block_number <= b.block_number AND
				b.block_number < w.max_block_number
		), moving_degraded AS (
			SELECT COUNT(*) AS block_rows
			FROM target_window w
			JOIN client_reliability_block b ON
				w.min_block_number <= b.block_number AND
				b.block_number < w.max_block_number
			CROSS JOIN current_block_stats s
			WHERE
				s.block_rows >= 10 AND
				s.median_valid_clients >= 20 AND
				b.valid_client_count < 0.95 * s.median_valid_clients
		), immutable_degraded AS (
			SELECT COUNT(*) AS block_rows
			FROM target_window w
			JOIN client_reliability_block candidate ON
				w.min_block_number <= candidate.block_number AND
				candidate.block_number < w.max_block_number
			CROSS JOIN LATERAL (
				SELECT
					COUNT(*) AS block_count,
					percentile_cont(0.5) WITHIN GROUP (ORDER BY neighborhood.valid_client_count) AS median_valid_clients
				FROM client_reliability_block neighborhood
				WHERE
					candidate.block_number - 59 <= neighborhood.block_number AND
					neighborhood.block_number <= candidate.block_number
			) local
			WHERE
				local.block_count >= 10 AND
				local.median_valid_clients >= 20 AND
				candidate.valid_client_count < 0.95 * local.median_valid_clients
		), anchor_stats AS (
			SELECT
				COUNT(b.block_number) AS block_rows,
				COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY b.valid_client_count), 0) AS median_valid_clients
			FROM target_window w
			LEFT JOIN client_reliability_block b ON
				w.last_recompute_block - (w.max_block_number - w.min_block_number) <= b.block_number AND
				b.block_number < w.last_recompute_block
		), anchor_moving_degraded AS (
			SELECT COUNT(*) AS block_rows
			FROM target_window w
			JOIN client_reliability_block b ON
				w.last_recompute_block - (w.max_block_number - w.min_block_number) <= b.block_number AND
				b.block_number < w.last_recompute_block
			CROSS JOIN anchor_stats s
			WHERE
				s.block_rows >= 10 AND
				s.median_valid_clients >= 20 AND
				b.valid_client_count < 0.95 * s.median_valid_clients
		)
		SELECT
			w.classification_version,
			w.min_block_number,
			w.max_block_number,
			w.last_recompute_block,
			s.score_rows,
			s.passing_rows,
			s.max_weight,
			s.median_weight,
			r.max_running_sum,
			r.median_running_sum,
			c.block_rows,
			c.median_valid_clients,
			m.block_rows,
			i.block_rows,
			a.block_rows,
			w.classification_write_token_present,
			g.present
		FROM target_window w
		CROSS JOIN classification_guard g
		CROSS JOIN score_stats s
		CROSS JOIN running_stats r
		CROSS JOIN current_block_stats c
		CROSS JOIN moving_degraded m
		CROSS JOIN immutable_degraded i
		CROSS JOIN anchor_moving_degraded a;
	`)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) < 17 {
		return nil, fmt.Errorf("reliability drift query returned %d malformed rows", len(rows))
	}

	row := rows[0]
	classificationVersion := atoiRow(row, 0)
	scoreRows := atoiRow(row, 4)
	passingRows := atoiRow(row, 5)
	maxWeight := atof(row.str(6))
	medianWeight := atof(row.str(7))
	writeTokenPresent := row.str(15) == "t" || row.str(15) == "true"
	classificationGuardPresent := row.str(16) == "t" || row.str(16) == "true"
	if classificationVersion >= reliabilityDriftClassificationVersion &&
		writeTokenPresent &&
		classificationGuardPresent &&
		scoreRows > 0 &&
		!reliabilityGateEffectivelyEmpty(scoreRows, passingRows) {
		return []finding{healthyFinding("pg/reliability-drift", tierPage, "reliability-classification-drift", pgTarget(env))}, nil
	}

	frame := "gate-collapse"
	mechanism := "Fewer than one in 1,000 scored clients passes the 12-hour provider gate at its 0.70 minimum. One extreme outlier cannot supply meaningful provider diversity. This can be genuine fleet-wide unreliability, but it is also the exact downstream state produced when a rolling numerator and denominator disagree; use the classification version and block counts to distinguish them."
	action := "Inspect the reliability inputs and recent fleet events. Do not loosen the 0.70 product threshold, delete score rows, edit Redis cache keys, or restart Connect clients to manufacture recovery."
	if classificationVersion < reliabilityDriftClassificationVersion {
		frame = "moving-median-v0"
		mechanism = "The running window still uses degraded-classification version 0. That algorithm classified blocks with one median over the moving lookback: after a sustained fleet drop became the new median, previously omitted blocks re-entered the denominator without re-entering the materialized numerator. The resulting artificial weight collapse removes established providers and can drive window clients into rapid replacement churn."
		action = "Apply both degraded-classification migrations through schema head 603, then deploy every Taskworker from the current Server revision and let its existing UpdateReliabilities task perform the mandatory one-time re-anchor. Do not schedule a duplicate task or mutate PostgreSQL/Redis score state by hand."
		if writeTokenPresent && classificationGuardPresent {
			frame = "legacy-writer-reset"
			mechanism = "The durable write token and database guard are present, but the classification version is back at 0. This is the fail-safe signature of a legacy Taskworker updating a running-window row after a current writer: because the old UPSERT cannot rotate the token, the trigger atomically revoked trust in the corresponding sums instead of leaving a false version-1 marker."
			action = "Finish converging every Taskworker on the current Server revision before trusting another repair. Then let the existing serialized UpdateReliabilities task re-anchor every version-0 lookback and rotate its guarded token; do not restart workers, schedule a duplicate task, or edit the marker/sums manually."
		}
	} else if !writeTokenPresent || !classificationGuardPresent {
		frame = "unguarded-version"
		mechanism = "The row claims degraded-classification version 1 without both a rotated write token and its enabled database trigger. That marker cannot prove which writer last replaced the running sums: a legacy Taskworker can preserve the numeric version while publishing the incompatible moving-window result."
		action = "Apply schema migration 603 and deploy every Taskworker from the current Server revision. Let the serialized reliability task re-anchor and publish a guarded token; do not trust or manually edit the unguarded version marker."
	}

	return []finding{{
		probeId: "pg/reliability-drift", tier: tierPage,
		class: "reliability-classification-drift", target: pgTarget(env), frame: frame, sustain: 1,
		symptom: fmt.Sprintf(
			"12-hour provider reliability gate admits %d of %d scored clients (max weight %.4f; required %.2f)",
			passingRows,
			scoreRows,
			maxWeight,
			reliabilityDriftMinimumWeight,
		),
		mechanism: mechanism,
		baseline:  "Degraded-block membership is immutable for a given block, every running window is guarded classification version 1 with a current write token, and a nonzero established-provider population remains at or above the 12-hour 0.70 gate.",
		observed: fmt.Sprintf(
			"classification_version=%d classification_write_token_present=%t classification_guard_present=%t lookback_index=%d window=[%s,%s) last_recompute_block=%s score_rows=%d passing_rows=%d max_weight=%s median_weight=%s max_running_sum=%s median_running_sum=%s health_blocks=%s median_valid_clients=%s moving_window_degraded=%s immutable_local_degraded=%s anchor_moving_degraded=%s",
			classificationVersion,
			writeTokenPresent,
			classificationGuardPresent,
			reliabilityDriftLookbackIndex,
			row.str(1),
			row.str(2),
			row.str(3),
			scoreRows,
			passingRows,
			strconv.FormatFloat(maxWeight, 'f', 6, 64),
			strconv.FormatFloat(medianWeight, 'f', 6, 64),
			row.str(8),
			row.str(9),
			row.str(10),
			row.str(11),
			row.str(12),
			row.str(13),
			row.str(14),
		),
		evidence: "PostgreSQL supplies the durable running-window bounds/version, Boolean token presence, enabled trigger identity, and the complete 12-hour score distribution. The two block counts apply the former moving-window median and the current per-block trailing median to the same small client_reliability_block range; no token value or client identifier is exported.",
		context:  "Correlate the last UpdateReliabilities completion, the first subsequent UpdateClientScores publication, provider destination diversity, and §2.7 child-client lifetime. A cache export can complete successfully while publishing a catastrophically narrowed nonempty market, so freshness and empty-cache checks alone do not clear this incident.",
		action:   action,
		verify:   "All Taskworkers have converged; the classification guard is enabled; all four client_reliability_running_window rows reach version 1 with nonempty rotated tokens; a completed re-anchor restores a nonzero 12-hour passing population; the next UpdateClientScores publication expands destination diversity; and child creation plus connection lifetime recover for two consecutive probes.",
		playbook: "SIGNALS.md §2.15, §2.9, §2.8, and §2.7",
	}}, nil
}
