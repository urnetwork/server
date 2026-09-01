package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §2.2a maps to signal_reindex_debris.go and
// signal_reindex_debris_test.go. The probe reads only PostgreSQL catalogs and
// excludes indexes belonging to an active CREATE/REINDEX operation and keeps
// that operation's bounded progress counters beside the obscured candidates.
func NewReindexDebrisSignal() Signal {
	return &signalAdapter{
		number: "2.2a",
		key:    "reindex-debris",
		name:   "Incomplete concurrent-index debris",
		probe:  reindexDebrisProbe{},
	}
}

type reindexDebrisProbe struct{}

func (reindexDebrisProbe) id() string             { return "pg/reindex-debris" }
func (reindexDebrisProbe) tier() string           { return tierWarn }
func (reindexDebrisProbe) cadence() time.Duration { return 5 * time.Minute }

const reindexDebrisCatalogQuery = `
	WITH public_tables AS MATERIALIZED (
		SELECT table_class.oid, table_class.reltoastrelid,
		       table_class.relname AS table_name
		FROM pg_class table_class
		INNER JOIN pg_namespace table_namespace
			ON table_namespace.oid = table_class.relnamespace
		WHERE table_namespace.nspname = 'public'
		  AND table_class.relkind IN ('r', 'p')
	), progress AS MATERIALIZED (
		SELECT progress.relid,
		       progress.index_relid,
		       format(
			       'relation=%s index=%s command=%L phase=%L query_age_s=%s wait=%s:%s blockers=%s blocks=%s/%s tuples=%s/%s lockers=%s/%s partitions=%s/%s',
			       progress.relid::regclass::text,
			       progress.index_relid::regclass::text,
			       progress.command,
			       progress.phase,
			       coalesce(round(extract(epoch FROM clock_timestamp() - activity.query_start))::bigint, -1),
			       coalesce(activity.wait_event_type, '-'),
			       coalesce(activity.wait_event, '-'),
			       coalesce(cardinality(pg_blocking_pids(progress.pid)), 0),
			       coalesce(progress.blocks_done, 0),
			       coalesce(progress.blocks_total, 0),
			       coalesce(progress.tuples_done, 0),
			       coalesce(progress.tuples_total, 0),
			       coalesce(progress.lockers_done, 0),
			       coalesce(progress.lockers_total, 0),
			       coalesce(progress.partitions_done, 0),
			       coalesce(progress.partitions_total, 0)
		       ) AS detail
		FROM pg_stat_progress_create_index progress
		LEFT JOIN pg_stat_activity activity USING (pid)
	), candidates AS (
		SELECT public_tables.oid AS table_oid,
		       public_tables.reltoastrelid AS toast_oid,
		       public_tables.table_name,
		       index_class.oid AS index_oid,
		       index_state.indisready,
		       pg_relation_size(index_class.oid) AS index_bytes,
		       format('%I.%I', index_namespace.nspname, index_class.relname) AS qualified_name,
		       EXISTS (
			       SELECT 1
			       FROM progress
			       WHERE progress.relid IN (public_tables.oid, public_tables.reltoastrelid)
		       ) AS relation_in_progress,
		       EXISTS (
			       SELECT 1
			       FROM progress
			       WHERE progress.index_relid = index_class.oid
		       ) AS exact_index_in_progress
		FROM public_tables
		INNER JOIN pg_index index_state
			ON index_state.indrelid IN (public_tables.oid, public_tables.reltoastrelid)
		INNER JOIN pg_class index_class
			ON index_class.oid = index_state.indexrelid
		INNER JOIN pg_namespace index_namespace
			ON index_namespace.oid = index_class.relnamespace
		WHERE index_state.indisvalid = false
		  AND index_class.relname ~ '_(ccnew|ccold)[0-9]*$'
	), incomplete AS (
		SELECT *
		FROM candidates
		WHERE NOT exact_index_in_progress
	), ranked AS (
		SELECT incomplete.*,
		       row_number() OVER (
			       PARTITION BY table_name, relation_in_progress
			       ORDER BY qualified_name
		       ) AS sample_rank
		FROM incomplete
	)
	SELECT table_name,
	       count(*) FILTER (WHERE NOT relation_in_progress)::int,
	       count(*) FILTER (WHERE NOT relation_in_progress AND indisready)::int,
	       coalesce(sum(index_bytes) FILTER (WHERE NOT relation_in_progress), 0)::bigint,
	       coalesce(string_agg(qualified_name, ', ' ORDER BY qualified_name)
	                FILTER (WHERE NOT relation_in_progress AND sample_rank <= 5), ''),
	       count(*) FILTER (WHERE relation_in_progress)::int,
	       count(*) FILTER (WHERE relation_in_progress AND indisready)::int,
	       coalesce(sum(index_bytes) FILTER (WHERE relation_in_progress), 0)::bigint,
	       coalesce(string_agg(qualified_name, ', ' ORDER BY qualified_name)
	                FILTER (WHERE relation_in_progress AND sample_rank <= 5), ''),
	       coalesce((
		       SELECT string_agg(progress.detail, '; ' ORDER BY progress.index_relid)
		       FROM progress
		       WHERE progress.relid IN (ranked.table_oid, ranked.toast_oid)
	       ), '') AS active_progress
	FROM ranked
	GROUP BY table_oid, toast_oid, table_name
	ORDER BY coalesce(sum(index_bytes), 0) DESC, count(*) DESC, table_name;
`

type reindexDebrisState struct {
	tableName             string
	count                 int64
	readyCount            int64
	bytes                 int64
	samples               string
	activeTableCount      int64
	activeTableReadyCount int64
	activeTableBytes      int64
	activeTableSamples    string
	activeProgress        string
}

func parseReindexDebrisState(row pgRow) (reindexDebrisState, error) {
	if len(row) != 10 {
		return reindexDebrisState{}, fmt.Errorf("reindex debris query returned %d columns, want 10", len(row))
	}
	parseNonnegative := func(column int, name string) (int64, error) {
		value, err := strconv.ParseInt(row.str(column), 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("invalid reindex debris %s %q", name, row.str(column))
		}
		return value, nil
	}

	state := reindexDebrisState{
		tableName:          row.str(0),
		samples:            row.str(4),
		activeTableSamples: row.str(8),
		activeProgress:     row.str(9),
	}
	if state.tableName == "" {
		return reindexDebrisState{}, fmt.Errorf("reindex debris query returned an empty table name")
	}
	var err error
	if state.count, err = parseNonnegative(1, "count"); err != nil {
		return reindexDebrisState{}, err
	}
	if state.readyCount, err = parseNonnegative(2, "ready count"); err != nil {
		return reindexDebrisState{}, err
	}
	if state.bytes, err = parseNonnegative(3, "bytes"); err != nil {
		return reindexDebrisState{}, err
	}
	if state.activeTableCount, err = parseNonnegative(5, "active-table count"); err != nil {
		return reindexDebrisState{}, err
	}
	if state.activeTableReadyCount, err = parseNonnegative(6, "active-table ready count"); err != nil {
		return reindexDebrisState{}, err
	}
	if state.activeTableBytes, err = parseNonnegative(7, "active-table bytes"); err != nil {
		return reindexDebrisState{}, err
	}
	if state.count < state.readyCount {
		return reindexDebrisState{}, fmt.Errorf(
			"invalid reindex debris counts count=%d ready=%d",
			state.count,
			state.readyCount,
		)
	}
	if state.activeTableCount < state.activeTableReadyCount {
		return reindexDebrisState{}, fmt.Errorf(
			"invalid active-table reindex candidate counts count=%d ready=%d",
			state.activeTableCount,
			state.activeTableReadyCount,
		)
	}
	if state.activeTableCount > 0 && state.activeProgress == "" {
		return reindexDebrisState{}, fmt.Errorf(
			"active-table reindex candidates for %q have no progress detail",
			state.tableName,
		)
	}
	if state.activeTableCount == 0 && state.activeProgress != "" {
		return reindexDebrisState{}, fmt.Errorf(
			"inactive reindex owner %q unexpectedly has progress detail",
			state.tableName,
		)
	}
	if state.count == 0 && state.activeTableCount == 0 {
		return reindexDebrisState{}, fmt.Errorf("reindex debris query returned an empty owner %q", state.tableName)
	}
	return state, nil
}

func (reindexDebrisProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, reindexDebrisCatalogQuery)
	if err != nil {
		return nil, err
	}
	target := pgTarget(env)
	states := make([]reindexDebrisState, 0, len(rows))
	var totalCount int64
	var totalReadyCount int64
	var totalBytes int64
	var activeTableCount int64
	var activeTableReadyCount int64
	var activeTableBytes int64
	confirmedTableCount := 0
	activeOwnerCount := 0
	for _, row := range rows {
		state, parseErr := parseReindexDebrisState(row)
		if parseErr != nil {
			return nil, parseErr
		}
		states = append(states, state)
		totalCount += state.count
		totalReadyCount += state.readyCount
		totalBytes += state.bytes
		activeTableCount += state.activeTableCount
		activeTableReadyCount += state.activeTableReadyCount
		activeTableBytes += state.activeTableBytes
		if state.count > 0 {
			confirmedTableCount++
		}
		if state.activeTableCount > 0 {
			activeOwnerCount++
		}
	}
	if len(states) == 0 {
		return []finding{healthyFinding("pg/reindex-debris", tierWarn, "reindex-debris", target)}, nil
	}

	confirmedEvidence := make([]string, 0, min(8, len(states)))
	activeEvidence := make([]string, 0, min(8, len(states)))
	for _, state := range states {
		if state.count > 0 && len(confirmedEvidence) < 8 {
			confirmedEvidence = append(confirmedEvidence, fmt.Sprintf(
				"%s: indexes=%d write_ready=%d bytes=%d samples=%s",
				state.tableName,
				state.count,
				state.readyCount,
				state.bytes,
				state.samples,
			))
		}
		if state.activeTableCount > 0 && len(activeEvidence) < 8 {
			activeEvidence = append(activeEvidence, fmt.Sprintf(
				"%s: invalid_candidates=%d write_ready=%d bytes=%d samples=%s progress={%s}",
				state.tableName,
				state.activeTableCount,
				state.activeTableReadyCount,
				state.activeTableBytes,
				state.activeTableSamples,
				state.activeProgress,
			))
		}
	}
	if totalCount == 0 {
		if activeTableCount == 0 {
			return []finding{healthyFinding("pg/reindex-debris", tierWarn, "reindex-debris", target)}, nil
		}
		return []finding{{
			probeId: "pg/reindex-debris", tier: tierWarn,
			class: "reindex-debris-obscured", target: target, frame: "active-catalog", sustain: 2,
			symptom: fmt.Sprintf(
				"%d invalid concurrent-index candidate(s), totaling %.2f GiB, share %d table(s) with active index work and cannot yet be classified as live or debris",
				activeTableCount,
				float64(activeTableBytes)/(1<<30),
				activeOwnerCount,
			),
			mechanism: "PostgreSQL exposes the table and target index for active CREATE/REINDEX progress, but a concurrent table rebuild can use transient _ccnew/_ccold relations whose exact OIDs are not the progress row's index_relid. Treating every sibling as inactive would risk prescribing deletion of live work; excluding the whole table without reporting its bytes would falsely resemble cleanup.",
			baseline:  "No non-exact invalid _ccnew/_ccold candidate remains unclassified behind index progress for two five-minute probes.",
			observed: fmt.Sprintf(
				"active_table_owners=%d active_table_candidates=%d active_table_write_ready=%d active_table_bytes=%d confirmed_inactive_indexes=0",
				activeOwnerCount,
				activeTableCount,
				activeTableReadyCount,
				activeTableBytes,
			),
			evidence: "active-table candidate owners:\n  " + strings.Join(activeEvidence, "\n  "),
			context:  "This is an explicit classification boundary, not reclaimed storage and not proof that the active operation is stuck. The next probe after progress exits will classify every surviving candidate as inactive debris.",
			action:   "Let protected index work reach its configured outcome and retain its progress/wait evidence. Do not drop a candidate or cancel the operation to resolve this alert. If the progress row outlives its task deadline, diagnose that operation separately; after it exits, use the ordinary reindex-debris finding and authorized cleanup path for survivors.",
			verify:   "The active progress row exits; the next catalog probe reports zero candidates or moves every surviving relation into the confirmed inactive count without an apparent storage-reclamation dip.",
			playbook: "SIGNALS.md §2.2a and §2.2",
		}}, nil
	}

	evidence := "largest confirmed inactive owners:\n  " + strings.Join(confirmedEvidence, "\n  ")
	maskedContext := "No invalid candidate is currently hidden behind active index work."
	maskedSymptom := ""
	if activeTableCount > 0 {
		maskedSymptom = fmt.Sprintf(
			"; %d additional invalid candidate(s), totaling %.2f GiB, share %d active table(s) and are not counted as reclaimed",
			activeTableCount,
			float64(activeTableBytes)/(1<<30),
			activeOwnerCount,
		)
		maskedContext = fmt.Sprintf(
			"Active table work obscures classification of %d additional invalid candidate(s) totaling %d bytes. They are reported separately from the confirmed lower bound; a fall in confirmed bytes while this value rises is not cleanup.",
			activeTableCount,
			activeTableBytes,
		)
		evidence += "\nactive-table candidates (not counted as reclaimed):\n  " + strings.Join(activeEvidence, "\n  ")
	}
	mechanism := "Interrupted or timed-out concurrent index operations left invalid _ccnew/_ccold relations. They consume storage and each later retry must skip or work around stale names instead of beginning from a clean catalog."
	if totalReadyCount > 0 {
		mechanism += fmt.Sprintf(" PostgreSQL marks %d of them ready for writes, so those invalid indexes can also add index and WAL work while remaining unusable by query plans.", totalReadyCount)
	}
	return []finding{{
		probeId: "pg/reindex-debris", tier: tierWarn,
		class: "reindex-debris", target: target, frame: "catalog", sustain: 1,
		symptom: fmt.Sprintf(
			"%d table(s) own at least %d inactive incomplete concurrent-index artifact(s), totaling at least %.2f GiB%s",
			confirmedTableCount,
			totalCount,
			float64(totalBytes)/(1<<30),
			maskedSymptom,
		),
		mechanism: mechanism,
		baseline:  "No inactive invalid public-table or associated TOAST index ends in _ccnew, _ccnewN, _ccold, or _ccoldN.",
		observed: fmt.Sprintf(
			"confirmed_tables=%d incomplete_indexes=%d write_ready_indexes=%d bytes=%d active_table_owners=%d active_table_candidates=%d active_table_write_ready=%d active_table_bytes=%d table_sample_limit=8 index_sample_limit=5",
			confirmedTableCount,
			totalCount,
			totalReadyCount,
			totalBytes,
			activeOwnerCount,
			activeTableCount,
			activeTableReadyCount,
			activeTableBytes,
		),
		evidence: evidence,
		context:  "The catalog query includes each public table's TOAST relation and excludes an exact index represented in pg_stat_progress_create_index. It reports non-exact candidates sharing an active table separately instead of hiding their storage or calling them inactive. " + maskedContext + " One aggregate alert preserves the shared cleanup boundary without emitting one duplicate alert per table.",
		action:   "First let any protected measurement or in-progress rebuild reach its bounded outcome. With no active index operation, satisfy §8.13 and deploy Taskworker from a clean server descendant containing 908a8b2c and d8392c83: it excludes the large/high-churn contract and escrow tables from full-table reindex, performs cleanup before and after each permitted object, and keeps DbMaintenance owned when only its pooled timestamp refresh stalls. These are the current-main, patch-identical equivalents of the former 7676014f and abfd976b commits. Then, with explicit database-maintenance authorization, run the supported cleanup-only full cycle (`bringyourctl db maintenance all --cleanup`). Do not wildcard-drop indexes or cancel a live rebuild to clear this alert.",
		verify:   "§8.12 proves every active Taskworker is a clean descendant of both fixes; the catalog contains zero inactive _ccnew/_ccold artifacts; no excluded large/high-churn table appears in later full-table maintenance progress; and one complete post-deploy maintenance cycle creates no new debris or clustered WAL/storage wait incident.",
		playbook: "SIGNALS.md §2.2a and §2.2",
	}}, nil
}
