// Real migration fixtures execute the bounded production statements on their
// already-owned repeatable-read transaction; they never nest an acquisition.
package monitor

import (
	"context"
	"encoding/csv"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// A fault fixture must keep its uncommitted DDL. Its owner supplies a no-op
// finish; the ordinary reader supplies the actual read-only transaction commit.
// Both paths use every production batch and the same strict frame decoder.
func migrationCensusFixtureInTx(ctx context.Context, tx server.PgTx, query string, observation *migrationCensusObservation, finish func() error) ([]Row, error) {
	plan, err := planMigrationCensus(query, observation.settings)
	if err != nil {
		return nil, err
	}
	rows, err := runMigrationCensus(ctx, query, observation, func(_ string) (string, error) {
		var isolation string
		if err := tx.QueryRow(ctx, "SELECT current_setting('transaction_isolation')").Scan(&isolation); err != nil {
			return "", err
		}
		if isolation != "repeatable read" {
			return "", errors.New("migration fixture does not own one repeatable-read snapshot")
		}
		if _, err := tx.Exec(ctx, "SET LOCAL standard_conforming_strings=on"); err != nil {
			return "", err
		}
		var output strings.Builder
		writer := csv.NewWriter(&output)
		write := func(statement string, one bool) error {
			result, err := tx.Query(ctx, statement, pgx.QueryExecModeSimpleProtocol)
			if err != nil {
				return err
			}
			defer result.Close()
			count := 0
			for result.Next() {
				var frame string
				if err := result.Scan(&frame); err != nil {
					return err
				}
				if err := writer.Write([]string{frame}); err != nil {
					return err
				}
				count++
			}
			if err := result.Err(); err != nil {
				return err
			}
			if one && count != 1 {
				return errors.New("migration fixture lost its single frame")
			}
			return nil
		}
		if err := write(plan.beginQuery(), true); err != nil {
			return "", err
		}
		for _, batch := range plan.batches {
			if err := write(batch.query, true); err != nil {
				return "", err
			}
		}
		var present bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass('public.migration_catalog') IS NOT NULL").Scan(&present); err != nil {
			return "", err
		}
		if present {
			if err := write(plan.catalogCountQuery(), true); err != nil {
				return "", err
			}
			if err := write(plan.catalogIdentityQuery(), false); err != nil {
				return "", err
			}
		} else if err := write("SELECT json_build_object('kind','catalog','present',false,'count',0)::text;", true); err != nil {
			return "", err
		}
		if err := finish(); err != nil {
			return "", err
		}
		// After an owned commit the transaction handle is closed, so this
		// constant, source-bound trailer is encoded without another acquisition.
		encoded := `{"kind":"complete","schema":1,"sha256":"` + plan.digest + `"}`
		if err := writer.Write([]string{encoded}); err != nil {
			return "", err
		}
		writer.Flush()
		return output.String(), writer.Error()
	})
	if err != nil {
		return nil, err
	}
	result := make([]Row, len(rows))
	for index, row := range rows {
		result[index] = Row(row)
	}
	return result, nil
}
