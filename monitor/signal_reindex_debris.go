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
// excludes indexes belonging to an active CREATE/REINDEX operation.
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
	), incomplete AS (
		SELECT public_tables.table_name,
		       index_class.oid AS index_oid,
		       index_state.indisready,
		       pg_relation_size(index_class.oid) AS index_bytes,
		       format('%I.%I', index_namespace.nspname, index_class.relname) AS qualified_name
		FROM public_tables
		INNER JOIN pg_index index_state
			ON index_state.indrelid IN (public_tables.oid, public_tables.reltoastrelid)
		INNER JOIN pg_class index_class
			ON index_class.oid = index_state.indexrelid
		INNER JOIN pg_namespace index_namespace
			ON index_namespace.oid = index_class.relnamespace
		WHERE index_state.indisvalid = false
		  AND index_class.relname ~ '_(ccnew|ccold)[0-9]*$'
		  AND NOT EXISTS (
			SELECT 1
			FROM pg_stat_progress_create_index progress
			WHERE progress.relid IN (public_tables.oid, public_tables.reltoastrelid)
			   OR progress.index_relid = index_class.oid
		  )
	), ranked AS (
		SELECT incomplete.*,
		       row_number() OVER (PARTITION BY table_name ORDER BY qualified_name) AS sample_rank
		FROM incomplete
	)
	SELECT table_name,
	       count(*)::int,
	       count(*) FILTER (WHERE indisready)::int,
	       coalesce(sum(index_bytes), 0)::bigint,
	       coalesce(string_agg(qualified_name, ', ' ORDER BY qualified_name)
	                FILTER (WHERE sample_rank <= 5), '')
	FROM ranked
	GROUP BY table_name
	ORDER BY coalesce(sum(index_bytes), 0) DESC, count(*) DESC, table_name;
`

type reindexDebrisState struct {
	tableName  string
	count      int64
	readyCount int64
	bytes      int64
	samples    string
}

func parseReindexDebrisState(row pgRow) (reindexDebrisState, error) {
	if len(row) != 5 {
		return reindexDebrisState{}, fmt.Errorf("reindex debris query returned %d columns, want 5", len(row))
	}
	parseNonnegative := func(column int, name string) (int64, error) {
		value, err := strconv.ParseInt(row.str(column), 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("invalid reindex debris %s %q", name, row.str(column))
		}
		return value, nil
	}

	state := reindexDebrisState{tableName: row.str(0), samples: row.str(4)}
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
	if state.count == 0 || state.count < state.readyCount {
		return reindexDebrisState{}, fmt.Errorf(
			"invalid reindex debris counts count=%d ready=%d",
			state.count,
			state.readyCount,
		)
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
	for _, row := range rows {
		state, parseErr := parseReindexDebrisState(row)
		if parseErr != nil {
			return nil, parseErr
		}
		states = append(states, state)
		totalCount += state.count
		totalReadyCount += state.readyCount
		totalBytes += state.bytes
	}
	if len(states) == 0 {
		return []finding{healthyFinding("pg/reindex-debris", tierWarn, "reindex-debris", target)}, nil
	}

	evidence := make([]string, 0, min(8, len(states)))
	for _, state := range states[:min(8, len(states))] {
		evidence = append(evidence, fmt.Sprintf(
			"%s: indexes=%d write_ready=%d bytes=%d samples=%s",
			state.tableName,
			state.count,
			state.readyCount,
			state.bytes,
			state.samples,
		))
	}
	mechanism := "Interrupted or timed-out concurrent index operations left invalid _ccnew/_ccold relations. They consume storage and each later retry must skip or work around stale names instead of beginning from a clean catalog."
	if totalReadyCount > 0 {
		mechanism += fmt.Sprintf(" PostgreSQL marks %d of them ready for writes, so those invalid indexes can also add index and WAL work while remaining unusable by query plans.", totalReadyCount)
	}
	return []finding{{
		probeId: "pg/reindex-debris", tier: tierWarn,
		class: "reindex-debris", target: target, frame: "catalog", sustain: 1,
		symptom: fmt.Sprintf(
			"%d table(s) own %d inactive incomplete concurrent-index artifact(s), totaling %.2f GiB",
			len(states),
			totalCount,
			float64(totalBytes)/(1<<30),
		),
		mechanism: mechanism,
		baseline:  "No inactive invalid public-table or associated TOAST index ends in _ccnew, _ccnewN, _ccold, or _ccoldN.",
		observed: fmt.Sprintf(
			"tables=%d incomplete_indexes=%d write_ready_indexes=%d bytes=%d table_sample_limit=8 index_sample_limit=5",
			len(states),
			totalCount,
			totalReadyCount,
			totalBytes,
		),
		evidence: "largest catalog owners:\n  " + strings.Join(evidence, "\n  "),
		context:  "The catalog query includes each public table's TOAST relation and excludes any table or index currently represented in pg_stat_progress_create_index. These are persistent artifacts, not the temporary invalid index of a live concurrent build. One aggregate alert preserves the shared cleanup boundary without emitting one duplicate alert per table.",
		action:   "First let any protected measurement or in-progress rebuild reach its bounded outcome. Deploy the taskworker maintenance fix that skips full-table transfer_escrow rebuilds and performs cleanup before and after each selected object. Then, with explicit database-maintenance authorization and no active index operation, run the supported cleanup-only full cycle (`bringyourctl db maintenance all --cleanup`). Do not wildcard-drop indexes or cancel a live rebuild to clear this alert.",
		verify:   "The catalog contains zero inactive _ccnew/_ccold artifacts, transfer_escrow never appears in a later full-table maintenance progress row, and one complete post-deploy maintenance cycle creates no new debris or clustered WAL/storage wait incident.",
		playbook: "SIGNALS.md §2.2a and §2.2",
	}}, nil
}
