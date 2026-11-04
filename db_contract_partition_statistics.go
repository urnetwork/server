// Extended statistics follow relation OIDs through renames. Keep the original
// objects on the legacy tables for rollback and publish copies on the parents.
package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const contractLegacyStatisticsSuffix = "_l"

// The definition is captured before the table rename, so its FROM clause binds
// to the newly published parent when replayed. No identifier rewriting is needed.
type contractStatistics struct {
	schema     string
	name       string
	definition string
	shape      string
	owner      string
	target     int
	comment    *string
}

// Uses the swap or rollback transaction's connection and catalog snapshot.
func readContractStatistics(ctx context.Context, conn contractCatalogQuerier, table string) (statistics []contractStatistics) {
	result, err := conn.Query(ctx, `
		SELECT n.nspname, s.stxname, pg_get_statisticsobjdef(s.oid),
			pg_get_userbyid(s.stxowner), s.stxstattarget,
			obj_description(s.oid, 'pg_statistic_ext'),
			ARRAY(
				SELECT a.attname::text
				FROM unnest(s.stxkeys) WITH ORDINALITY AS k(attnum, position)
				JOIN pg_attribute a ON a.attrelid = s.stxrelid AND a.attnum = k.attnum
				ORDER BY k.position
			)::text || ':' || s.stxkind::text || ':' || COALESCE(pg_get_expr(s.stxexprs, s.stxrelid), '')
		FROM pg_statistic_ext s
		JOIN pg_namespace n ON n.oid = s.stxnamespace
		WHERE s.stxrelid = to_regclass($1)
		ORDER BY n.nspname, s.stxname
	`, pgx.Identifier{"public", table}.Sanitize())
	WithPgResult(result, err, func() {
		for result.Next() {
			var statistic contractStatistics
			Raise(result.Scan(&statistic.schema, &statistic.name, &statistic.definition, &statistic.owner, &statistic.target, &statistic.comment, &statistic.shape))
			statistics = append(statistics, statistic)
		}
	})
	return
}

// Preserves object metadata as well as the expression and kinds in the catalog
// definition. Original objects retain their OIDs and analyzed data on legacy.
func contractStatisticsSwapSql(statistics []contractStatistics) (rename []string, create []string, err error) {
	for _, statistic := range statistics {
		legacyName := statistic.name + contractLegacyStatisticsSuffix
		if pgMaxIdentifierLength < len(legacyName) {
			return nil, nil, fmt.Errorf("statistics name %s.%s is too long to preserve for rollback", statistic.schema, statistic.name)
		}
		name := pgx.Identifier{statistic.schema, statistic.name}.Sanitize()
		rename = append(rename, fmt.Sprintf("ALTER STATISTICS %s RENAME TO %s", name, quoteIdentifier(legacyName)))
		create = append(create, statistic.definition, fmt.Sprintf("ALTER STATISTICS %s SET STATISTICS %d", name, statistic.target))
		if statistic.comment != nil {
			literal := "E'" + strings.ReplaceAll(strings.ReplaceAll(*statistic.comment, "\\", "\\\\"), "'", "''") + "'"
			create = append(create, fmt.Sprintf("COMMENT ON STATISTICS %s IS %s", name, literal))
		}
		create = append(create, fmt.Sprintf("ALTER STATISTICS %s OWNER TO %s", name, quoteIdentifier(statistic.owner)))
	}
	return
}

// Reads definitions only after the swap acquired every table lock. Name collisions
// fail the same transaction, leaving every table and original object unchanged.
func contractPartitionSwap(ctx context.Context, statements []string) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		var create []string
		locked := false
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
			if !locked && strings.HasPrefix(statement, "LOCK TABLE ") {
				locked = true
				// Force deparsed relations and application functions to carry
				// their schemas, then restore the caller's exact search path.
				var searchPath string
				Raise(tx.QueryRow(ctx, `SELECT current_setting('search_path')`).Scan(&searchPath))
				RaisePgResult(tx.Exec(ctx, `SELECT set_config('search_path', 'pg_catalog', true)`))
				for _, table := range ContractPartitionTables {
					statistics := readContractStatistics(ctx, tx, table)
					rename, _, err := contractStatisticsSwapSql(statistics)
					Raise(err)
					for _, statement := range rename {
						RaisePgResult(tx.Exec(ctx, statement))
					}
					// RENAME owns each object's lock. Refresh mutable metadata in
					// case ALTER STATISTICS committed between the read and rename.
					for _, legacy := range readContractStatistics(ctx, tx, table) {
						for i := range statistics {
							if legacy.schema == statistics[i].schema && legacy.name == statistics[i].name+contractLegacyStatisticsSuffix {
								statistics[i].owner = legacy.owner
								statistics[i].target = legacy.target
								statistics[i].comment = legacy.comment
								break
							}
						}
					}
					_, copies, err := contractStatisticsSwapSql(statistics)
					Raise(err)
					create = append(create, copies...)
				}
				RaisePgResult(tx.Exec(ctx, `SELECT set_config('search_path', $1, true)`, searchPath))
			}
		}
		for _, statement := range create {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// The parent's copies have been dropped before the original names are restored.
func contractStatisticsRollbackSql(statistics []contractStatistics, parents []contractStatistics) (statements []string, err error) {
	for _, statistic := range statistics {
		if name, ok := strings.CutSuffix(statistic.name, contractLegacyStatisticsSuffix); ok {
			matched := false
			for _, parent := range parents {
				if parent.schema == statistic.schema && parent.name == name && parent.shape == statistic.shape && parent.owner == statistic.owner && parent.target == statistic.target && ((parent.comment == nil && statistic.comment == nil) || (parent.comment != nil && statistic.comment != nil && *parent.comment == *statistic.comment)) {
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("legacy statistics %s.%s has no matching parent object; refusing to guess its rollback name", statistic.schema, statistic.name)
			}
			statements = append(statements, fmt.Sprintf("ALTER STATISTICS %s RENAME TO %s", pgx.Identifier{statistic.schema, statistic.name}.Sanitize(), quoteIdentifier(name)))
		}
	}
	return
}
