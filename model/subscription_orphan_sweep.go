package model

import (
	"fmt"
	"strings"

	"github.com/urnetwork/server"
)

// Select the cursor page before checking parent membership, then lock each
// orphan through its complete primary key. The lock can follow a concurrent
// update, so its returned tuple address can be newer than this statement's
// snapshot. The caller deletes those locked versions in a second statement in
// the same read-committed transaction, where they are visible. Parent membership
// is rechecked there; a newly visible parent conservatively retains its child.
const orphanSweepPageSqlTemplate = `
	WITH slice AS MATERIALIZED (
		SELECT {{keys}}
		FROM {{table}}
		WHERE {{cursor_predicate}}
		ORDER BY {{keys}}
		LIMIT {{limit_parameter}}
	), locked AS MATERIALIZED (
		SELECT matched.ctid
		FROM slice
		CROSS JOIN LATERAL (
			SELECT sweep_target.ctid
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
	SELECT (SELECT count(*) FROM slice), ARRAY(SELECT ctid::text FROM locked), {{bound_keys}}
	FROM bound
`

const orphanSweepDeleteSqlTemplate = `
	DELETE FROM {{table}} AS sweep_delete
	WHERE ctid = ANY($1::text[]::tid[])
		AND NOT EXISTS (
			SELECT 1 FROM transfer_contract
			WHERE transfer_contract.{{parent_column}} = sweep_delete.{{parent_column}}
			LIMIT 1 OFFSET 0
		)
`

// Identifiers come only from the fixed step list below. First and resumed
// pages use distinct SQL so generic plans retain the primary-key range rather
// than filtering all preceding keys through a boolean-OR cursor predicate.
func newSweepOrphanContractStep(table string, keys []string, newCursorTargets func() []any) sweepOrphanStep {
	parameters := make([]string, len(keys))
	keyMatches := make([]string, len(keys))
	descendingKeys := make([]string, len(keys))
	boundKeys := make([]string, len(keys))
	for i, key := range keys {
		parameters[i] = fmt.Sprintf("$%d", i+1)
		keyMatches[i] = fmt.Sprintf("sweep_target.%s = slice.%s", key, key)
		descendingKeys[i] = key + " DESC"
		boundKeys[i] = "bound." + key
	}
	pageSql := strings.NewReplacer(
		"{{table}}", table,
		"{{keys}}", strings.Join(keys, ", "),
		"{{key_match}}", strings.Join(keyMatches, " AND "),
		"{{parent_column}}", keys[0],
		"{{descending_keys}}", strings.Join(descendingKeys, ", "),
		"{{bound_keys}}", strings.Join(boundKeys, ", "),
	).Replace(orphanSweepPageSqlTemplate)
	return sweepOrphanStep{
		table: table,
		deleteSql: strings.NewReplacer(
			"{{table}}", table,
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
