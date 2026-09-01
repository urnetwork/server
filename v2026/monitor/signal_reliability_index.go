package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §8.10 maps to signal_reliability_index.go and
// signal_reliability_index_test.go. The probe reads only pg_catalog: it never
// starts, resumes, or otherwise mutates the potentially multi-hour index
// upgrade that it diagnoses.
func NewReliabilityIndexSignal() Signal {
	return &signalAdapter{
		number: "8.10",
		key:    "reliability-index",
		name:   "Client reliability covering index",
		probe:  reliabilityIndexProbe{},
	}
}

type reliabilityIndexProbe struct{}

func (reliabilityIndexProbe) id() string             { return "pg/reliability-index" }
func (reliabilityIndexProbe) tier() string           { return tierWarn }
func (reliabilityIndexProbe) cadence() time.Duration { return 5 * time.Minute }

const reliabilityIndexDesiredDefinitionSuffix = " USING btree (valid, block_number, client_address_hash) INCLUDE (network_id, client_id)"

// reliabilityIndexCatalogQuery mirrors the physical contract enforced by
// model.UpgradeClientReliabilitySecondaryIndex. Keeping this as one bounded
// catalog query makes the signal synthetic-testable and avoids scanning the
// multi-billion-row client_reliability data table.
const reliabilityIndexCatalogQuery = `
	WITH table_state AS (
		SELECT c.oid, c.relkind
		FROM pg_class c
		WHERE c.oid = to_regclass('public.client_reliability')
	), desired_index AS (
		SELECT i.indexrelid, i.indisvalid, pg_get_indexdef(i.indexrelid) AS definition
		FROM pg_index i
		INNER JOIN pg_class ic ON ic.oid = i.indexrelid
		INNER JOIN table_state t ON t.oid = i.indrelid
		WHERE ic.relname = 'client_reliability_valid_bnch_net_client'
	), old_index AS (
		SELECT i.indexrelid
		FROM pg_index i
		INNER JOIN pg_class ic ON ic.oid = i.indexrelid
		INNER JOIN table_state t ON t.oid = i.indrelid
		WHERE ic.relname = 'client_reliability_valid_block_number_client_address_hash'
	), table_partitions AS (
		SELECT inheritance.inhrelid
		FROM pg_inherits inheritance
		WHERE inheritance.inhparent = (SELECT oid FROM table_state)
	), attached_children AS (
		SELECT child.indisvalid
		FROM desired_index parent
		INNER JOIN pg_inherits inheritance ON inheritance.inhparent = parent.indexrelid
		INNER JOIN pg_index child ON child.indexrelid = inheritance.inhrelid
	)
	SELECT
		COALESCE((SELECT relkind = 'p' FROM table_state), false),
		EXISTS (SELECT 1 FROM old_index),
		EXISTS (SELECT 1 FROM desired_index),
		COALESCE((SELECT indisvalid FROM desired_index), false),
		COALESCE((SELECT definition FROM desired_index), ''),
		(SELECT count(*) FROM table_partitions),
		(SELECT count(*) FROM attached_children),
		(SELECT count(*) FROM attached_children WHERE NOT indisvalid);
`

type reliabilityIndexState struct {
	partitioned       bool
	oldExists         bool
	desiredExists     bool
	desiredValid      bool
	desiredDefinition string
	partitionCount    int
	attachedCount     int
	invalidChildren   int
}

func parseReliabilityIndexState(row pgRow) (reliabilityIndexState, error) {
	if len(row) != 8 {
		return reliabilityIndexState{}, fmt.Errorf("reliability index query returned %d columns, want 8", len(row))
	}
	parseBool := func(column int, name string) (bool, error) {
		value, err := strconv.ParseBool(row.str(column))
		if err != nil {
			return false, fmt.Errorf("invalid %s boolean %q", name, row.str(column))
		}
		return value, nil
	}
	parseCount := func(column int, name string) (int, error) {
		value, err := strconv.Atoi(row.str(column))
		if err != nil || value < 0 {
			return 0, fmt.Errorf("invalid %s count %q", name, row.str(column))
		}
		return value, nil
	}

	state := reliabilityIndexState{desiredDefinition: row.str(4)}
	var err error
	if state.partitioned, err = parseBool(0, "partitioned"); err != nil {
		return reliabilityIndexState{}, err
	}
	if state.oldExists, err = parseBool(1, "old-index-present"); err != nil {
		return reliabilityIndexState{}, err
	}
	if state.desiredExists, err = parseBool(2, "desired-index-present"); err != nil {
		return reliabilityIndexState{}, err
	}
	if state.desiredValid, err = parseBool(3, "desired-index-valid"); err != nil {
		return reliabilityIndexState{}, err
	}
	if state.partitionCount, err = parseCount(5, "table partition"); err != nil {
		return reliabilityIndexState{}, err
	}
	if state.attachedCount, err = parseCount(6, "attached child index"); err != nil {
		return reliabilityIndexState{}, err
	}
	if state.invalidChildren, err = parseCount(7, "invalid child index"); err != nil {
		return reliabilityIndexState{}, err
	}
	return state, nil
}

func (state reliabilityIndexState) desiredShapeMatches() bool {
	return state.desiredExists && strings.HasSuffix(state.desiredDefinition, reliabilityIndexDesiredDefinitionSuffix)
}

func (state reliabilityIndexState) driftReason() string {
	switch {
	case !state.partitioned:
		return ""
	case state.finalizationOnly():
		return "the covering replacement is complete, but the old non-covering parent index still needs finalization"
	case state.oldExists:
		return "the old non-covering parent index is still present"
	case !state.desiredExists:
		return "the desired covering parent index is missing"
	case !state.desiredShapeMatches():
		return "the desired-name parent index has the wrong key or INCLUDE shape"
	case !state.desiredValid:
		return "the desired parent index is invalid because its partition upgrade is incomplete"
	case state.attachedCount != state.partitionCount:
		return fmt.Sprintf("only %d of %d table partitions have an attached child index", state.attachedCount, state.partitionCount)
	case state.invalidChildren != 0:
		return fmt.Sprintf("%d attached child index(es) are invalid", state.invalidChildren)
	default:
		return ""
	}
}

func (state reliabilityIndexState) finalizationOnly() bool {
	return state.partitioned &&
		state.oldExists &&
		state.desiredExists &&
		state.desiredValid &&
		state.desiredShapeMatches() &&
		state.attachedCount == state.partitionCount &&
		state.invalidChildren == 0
}

func (reliabilityIndexProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, reliabilityIndexCatalogQuery)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("reliability index query returned %d rows, want 1", len(rows))
	}
	state, err := parseReliabilityIndexState(rows[0])
	if err != nil {
		return nil, err
	}

	target := pgTarget(env)
	reason := state.driftReason()
	if reason == "" {
		return []finding{healthyFinding("pg/reliability-index", tierWarn, "reliability-index-drift", target)}, nil
	}

	shapeMatches := state.desiredShapeMatches()
	evidence := "pg_catalog identifies the physical parent and attached child indexes without reading client_reliability rows."
	if state.desiredExists {
		evidence += " desired_definition=" + state.desiredDefinition
	}
	mechanism := "Production was partitioned before the covering index gained INCLUDE (network_id, client_id). CREATE INDEX IF NOT EXISTS cannot reshape the existing parent or its partition children, so score-window scans retain heap fetches until the explicit per-partition online upgrade finishes."
	alertContext := "This is an operational database-maintenance alert, not a deploy-only repair. A service release cannot create the physical index, and the monitor deliberately does not start it. Each partition build scans and sorts one large partition, so beginning it during a protected measurement can disturb I/O even though CREATE INDEX CONCURRENTLY keeps writes available. Repeated taskworker warnings are repeated observations of this one catalog state, not independent defects."
	action := "Do not start the upgrade while the current measurement must remain undisturbed. After explicit maintenance authorization, run `bringyourctl model upgrade-client-reliability-index` from the current server source with bounded parallelism, monitor database I/O, locks, replication lag, and free space, and rerun the same command if interrupted. Do not CREATE INDEX on the partitioned parent inline, drop the old index first, or deploy/restart taskworkers to silence this alert. If the desired-name index has the wrong shape, stop for DBA inspection before dropping anything."
	if state.finalizationOnly() {
		mechanism = "The covering replacement already has the exact INCLUDE shape, every partition child is attached and valid, and PostgreSQL has marked the parent valid. Only the supported upgrade's final old-parent DROP remains; an earlier run may have stopped before that step or exhausted its bounded lock retries."
		alertContext = fmt.Sprintf("This is finalization-only operational maintenance, not a service deployment or another partition build. The supported command will skip every completed child rather than scan or sort the %d partitions again, then attempt the old partitioned-index DROP under a 15-second lock timeout with bounded retries. The drop still takes a metadata lock, so wait until the protected measurement permits that lock even though the expensive build phase is complete.", state.partitionCount)
		action = "Do not finalize while the current measurement must remain undisturbed. After explicit maintenance authorization, rerun `bringyourctl model upgrade-client-reliability-index`; require it to skip all completed partition children and finish the supported old-parent drop. If the lock is busy, let the command's bounded retries fail and rerun later. Do not manually drop either index, rebuild the already-valid children, or deploy/restart taskworkers to silence this alert."
	}
	return []finding{{
		probeId: "pg/reliability-index", tier: tierWarn,
		class: "reliability-index-drift", target: target, frame: "table=client_reliability",
		symptom:   "client_reliability index drift: " + reason,
		mechanism: mechanism,
		baseline:  "The old parent index is absent; client_reliability_valid_bnch_net_client has the exact covering shape, is valid, and has one valid attached child index per table partition.",
		observed: fmt.Sprintf(
			"partitioned=%t old_index_present=%t desired_index_present=%t desired_index_valid=%t desired_shape_matches=%t table_partitions=%d attached_child_indexes=%d invalid_child_indexes=%d",
			state.partitioned,
			state.oldExists,
			state.desiredExists,
			state.desiredValid,
			shapeMatches,
			state.partitionCount,
			state.attachedCount,
			state.invalidChildren,
		),
		evidence: evidence,
		context:  alertContext,
		action:   action,
		verify:   "The command reports that client_reliability_valid_bnch_net_client matches the desired shape across every partition; this probe then returns healthy, the old parent is absent, the desired parent and every attached child are valid, and no new [crp] secondary-index-drift warning appears for five minutes after log-ingestion delay.",
		playbook: "SIGNALS.md §8.10",
	}}, nil
}
