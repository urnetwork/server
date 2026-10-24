package model

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/urnetwork/server"
)

// Select the cursor page before checking parent membership, then lock each
// orphan through its complete primary key and return the locked keys, one array
// per key column in key order. The lock can follow a concurrent update to a
// version newer than this statement's snapshot. The caller deletes by those keys
// in a second statement in the same read-committed transaction, where that
// version is visible. Parent membership is rechecked there; a newly visible
// parent conservatively retains its child.
const orphanSweepPageSqlTemplate = `
	WITH slice AS MATERIALIZED (
		SELECT {{keys}}
		FROM {{table}}
		WHERE {{cursor_predicate}}
		ORDER BY {{keys}}
		LIMIT {{limit_parameter}}
	), locked AS MATERIALIZED (
		SELECT matched.*
		FROM slice
		CROSS JOIN LATERAL (
			SELECT {{target_keys}}
			FROM {{table}} AS sweep_target
			WHERE {{key_match}}
				AND NOT EXISTS (
					SELECT 1 FROM transfer_contract
					WHERE transfer_contract.{{parent_column}} = sweep_target.{{parent_column}}
					LIMIT 1 OFFSET 0
				)
			LIMIT 1 OFFSET 0
			FOR UPDATE OF sweep_target
		) AS matched
	), bound AS MATERIALIZED (
		SELECT {{keys}} FROM slice ORDER BY {{descending_keys}} LIMIT 1
	)
	SELECT (SELECT count(*) FROM slice), {{locked_key_arrays}}, {{bound_keys}}
	FROM bound
`

// A primary key names exactly one row of a partitioned table, while a tuple
// address names a row only within one partition. The locked rows stay locked
// until commit, so each key still names the row its page selected.
const orphanSweepDeleteSqlTemplate = `
	DELETE FROM {{table}} AS sweep_delete
	USING unnest({{locked_key_parameters}}) AS locked({{keys}})
	WHERE {{delete_key_match}}
		AND NOT EXISTS (
			SELECT 1 FROM transfer_contract
			WHERE transfer_contract.{{parent_column}} = sweep_delete.{{parent_column}}
			LIMIT 1 OFFSET 0
		)
`

// Identifiers come only from the fixed step list below. First and resumed
// pages use distinct SQL so generic plans retain the primary-key range rather
// than filtering all preceding keys through a boolean-OR cursor predicate. The
// locked-key array types follow the cursor targets, so both halves of a page
// carry the step's own key types.
func newSweepOrphanContractStep(table string, keys []string, newCursorTargets func() []any) sweepOrphanStep {
	parameters := make([]string, len(keys))
	keyMatches := make([]string, len(keys))
	descendingKeys := make([]string, len(keys))
	boundKeys := make([]string, len(keys))
	targetKeys := make([]string, len(keys))
	lockedKeyArrays := make([]string, len(keys))
	deleteKeyMatches := make([]string, len(keys))
	for i, key := range keys {
		parameters[i] = fmt.Sprintf("$%d", i+1)
		keyMatches[i] = fmt.Sprintf("sweep_target.%s = slice.%s", key, key)
		descendingKeys[i] = key + " DESC"
		boundKeys[i] = "bound." + key
		targetKeys[i] = "sweep_target." + key
		// Every array is ordered by the complete key, so the arrays align.
		lockedKeyArrays[i] = fmt.Sprintf("ARRAY(SELECT %s FROM locked ORDER BY %s)", key, strings.Join(keys, ", "))
		deleteKeyMatches[i] = fmt.Sprintf("sweep_delete.%s = locked.%s", key, key)
	}
	lockedKeyParameters := make([]string, len(keys))
	for i, target := range newCursorTargets() {
		switch target.(type) {
		case *server.Id:
			lockedKeyParameters[i] = fmt.Sprintf("$%d::uuid[]", i+1)
		case *string:
			lockedKeyParameters[i] = fmt.Sprintf("$%d::text[]", i+1)
		default:
			// a new key column type must extend the locked-key arrays too
			panic(fmt.Errorf("unsupported orphan sweep key type %T", target))
		}
	}
	pageSql := strings.NewReplacer(
		"{{table}}", table,
		"{{keys}}", strings.Join(keys, ", "),
		"{{key_match}}", strings.Join(keyMatches, " AND "),
		"{{target_keys}}", strings.Join(targetKeys, ", "),
		"{{parent_column}}", keys[0],
		"{{descending_keys}}", strings.Join(descendingKeys, ", "),
		"{{locked_key_arrays}}", strings.Join(lockedKeyArrays, ", "),
		"{{bound_keys}}", strings.Join(boundKeys, ", "),
	).Replace(orphanSweepPageSqlTemplate)
	return sweepOrphanStep{
		table: table,
		deleteSql: strings.NewReplacer(
			"{{table}}", table,
			"{{locked_key_parameters}}", strings.Join(lockedKeyParameters, ", "),
			"{{keys}}", strings.Join(keys, ", "),
			"{{delete_key_match}}", strings.Join(deleteKeyMatches, " AND "),
			"{{parent_column}}", keys[0],
		).Replace(orphanSweepDeleteSqlTemplate),
		firstSql: strings.NewReplacer(
			"{{cursor_predicate}}", "true",
			"{{limit_parameter}}", "$1",
		).Replace(pageSql),
		sql: strings.NewReplacer(
			"{{cursor_predicate}}", fmt.Sprintf("(%s) > (%s)", strings.Join(keys, ", "), strings.Join(parameters, ", ")),
			"{{limit_parameter}}", fmt.Sprintf("$%d", len(keys)+1),
		).Replace(pageSql),
		newCursorTargets: newCursorTargets,
	}
}

// One typed array target per key column, shaped like the step's cursor targets,
// for the locked keys of a page.
func newSweepLockedKeyTargets(cursorTargets []any) []any {
	lockedKeyTargets := make([]any, len(cursorTargets))
	for i, cursorTarget := range cursorTargets {
		lockedKeyTargets[i] = reflect.New(reflect.SliceOf(reflect.TypeOf(cursorTarget).Elem())).Interface()
	}
	return lockedKeyTargets
}

// Step order is the persisted cursor's table identity; keep existing positions
// stable. Each child's leading primary-key column is its parent lookup key.
func sweepOrphanContractSteps() []sweepOrphanStep {
	return []sweepOrphanStep{
		newSweepOrphanContractStep("contract_close", []string{"contract_id", "party"}, func() []any {
			return []any{new(server.Id), new(string)}
		}),
		newSweepOrphanContractStep("transfer_escrow", []string{"contract_id", "balance_id"}, func() []any {
			return []any{new(server.Id), new(server.Id)}
		}),
		newSweepOrphanContractStep("transfer_escrow_sweep", []string{"contract_id", "balance_id", "network_id"}, func() []any {
			return []any{new(server.Id), new(server.Id), new(server.Id)}
		}),
		newSweepOrphanContractStep("contract_participant", []string{"stream_id", "client_id"}, func() []any {
			return []any{new(server.Id), new(server.Id)}
		}),
		newSweepOrphanContractStep("contract_extender", []string{"contract_id", "extender_id", "party"}, func() []any {
			return []any{new(server.Id), new(server.Id), new(string)}
		}),
	}
}
