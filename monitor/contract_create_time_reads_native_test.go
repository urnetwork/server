// The monitor's create_time windows, read as contract id ranges, count exactly
// what their create_time-only originals count in production's UTC session, on
// contracts created on and around the window boundaries whose ids were minted
// anywhere within the margin of their creation, and count the same in a
// session time zone with daylight saving.
package monitor

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The per-minute count before the contract id range bounded it.
const contractRateCountByCreateTimeSql = `
		SELECT count(*) FROM transfer_contract
		WHERE create_time >= date_trunc('minute',now())-interval '1 minute'
		  AND create_time <  date_trunc('minute',now());
	`

// The stale contract read before the contract id range bounded it.
const staleContractsByCreateTimeSql = `
		WITH stale_success AS MATERIALIZED (
			SELECT
				tc.source_id,
				tc.destination_id,
				tc.source_network_id,
				tc.source_network_id = tc.destination_network_id AS same_network,
				destination.source_client_id IS NOT NULL AS destination_derived,
				destination.source_client_id AS destination_parent_id,
				destination.device_id AS destination_device_id,
				source.source_client_id IS NOT NULL AS source_derived,
				source.source_client_id AS source_parent_id,
				source.device_id AS source_device_id,
				source_parent.active AS source_parent_active,
				source.active AND source.source_client_id IS NULL AS source_active_top,
				extract(epoch FROM tc.create_time - destination.deactivate_time) * 1000 AS inactive_milliseconds,
				CASE
					WHEN tc.create_time - destination.deactivate_time < interval '1 second'
						THEN 'stale-contract-timestamp-ambiguous'
					ELSE 'stale-contract-success'
				END AS boundary_class
			FROM transfer_contract tc
			JOIN network_client destination ON destination.client_id = tc.destination_id
			LEFT JOIN network_client source ON source.client_id = tc.source_id
			LEFT JOIN network_client source_parent ON source_parent.client_id = source.source_client_id
			WHERE
				tc.create_time >= now() - interval '5 minutes' AND
				tc.companion_contract_id IS NULL AND
				NOT destination.active AND
				destination.deactivate_time IS NOT NULL AND
				destination.deactivate_time <= tc.create_time
		)
		SELECT
			boundary_class,
			count(*),
			count(*) FILTER (WHERE same_network),
			count(*) FILTER (WHERE destination_derived),
			count(*) FILTER (WHERE source_active_top IS TRUE),
			count(DISTINCT destination_id),
			count(DISTINCT source_id),
			coalesce(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY inactive_milliseconds)::numeric, 3), 0),
			coalesce(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY inactive_milliseconds)::numeric, 3), 0),
			count(DISTINCT destination_id) FILTER (WHERE same_network),
			count(DISTINCT destination_parent_id) FILTER (WHERE same_network),
			count(DISTINCT destination_device_id) FILTER (WHERE same_network),
			count(DISTINCT source_id) FILTER (WHERE same_network),
			count(DISTINCT source_device_id) FILTER (WHERE same_network),
			count(DISTINCT source_network_id) FILTER (WHERE same_network),
			count(*) FILTER (WHERE NOT same_network AND NOT destination_derived),
			count(*) FILTER (WHERE NOT same_network AND source_derived),
			count(*) FILTER (WHERE NOT same_network AND source_parent_active IS TRUE),
			count(DISTINCT destination_id) FILTER (WHERE NOT same_network),
			count(DISTINCT source_id) FILTER (WHERE NOT same_network),
			count(DISTINCT source_parent_id) FILTER (WHERE NOT same_network),
			count(DISTINCT source_device_id) FILTER (WHERE NOT same_network)
		FROM stale_success
		GROUP BY boundary_class
		ORDER BY boundary_class;
	`

// An id as connect.NewId mints it at the given time.
func monitorContractIdMintedAt(at time.Time) server.Id {
	id := server.ContractIdFloor(at)
	for index := 6; index < len(id); index += 1 {
		id[index] = byte(rand.Uint32())
	}
	return id
}

// The ids minted across the margin around a creation: at it, the whole
// margin before it, almost the whole margin after it, and a millisecond either
// side.
func monitorContractIdsAround(createTime time.Time) []server.Id {
	margin := model.ContractIdCreateTimeMargin
	ids := []server.Id{}
	for _, offset := range []time.Duration{0, -margin, margin - time.Millisecond, -time.Millisecond, time.Millisecond} {
		ids = append(ids, monitorContractIdMintedAt(createTime.Add(offset)))
	}
	return ids
}

// The highest contract id below the range's upper bound for contracts created
// before end.
func monitorHighestContractIdBefore(end time.Time) server.Id {
	id := server.ContractIdFloor(end.Add(model.ContractIdCreateTimeMargin - time.Millisecond))
	for index := 6; index < len(id); index += 1 {
		id[index] = 0xff
	}
	return id
}

func insertMonitorTestContract(t testing.TB, ctx context.Context, tx pgx.Tx, contractId server.Id, destinationId server.Id, createTime time.Time, companion bool) {
	t.Helper()
	var companionId *server.Id
	if companion {
		id := server.NewId()
		companionId = &id
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`INSERT INTO transfer_contract (
			contract_id, source_network_id, source_id, destination_network_id, destination_id,
			transfer_byte_count, create_time, companion_contract_id
		) VALUES ($1, $2, $3, $4, $5, 1024, $6, $7)`,
		contractId, server.NewId(), server.NewId(), server.NewId(), destinationId, createTime.UTC(), companionId,
	))
}

// The rows a query returns, as text.
func monitorQueryRows(t testing.TB, ctx context.Context, tx pgx.Tx, query string) [][]string {
	t.Helper()
	rows := [][]string{}
	result, err := tx.Query(ctx, strings.TrimSuffix(strings.TrimSpace(query), ";"), pgx.QueryExecModeSimpleProtocol)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			values, err := result.Values()
			server.Raise(err)
			row := make([]string, len(values))
			for i, value := range values {
				if value != nil {
					row[i] = fmt.Sprint(value)
				}
			}
			rows = append(rows, row)
		}
	})
	return rows
}

// Runs callback in a transaction that rolls back, so now() is one instant for
// every read in it.
func monitorRollbackTx(t testing.TB, ctx context.Context, callback func(tx pgx.Tx)) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
		server.Raise(err)
		defer func() {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}()
		callback(tx)
	})
}

// The last complete minute's count equals the create_time-only original's.
func TestContractRateCountMatchesCreateTime(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := captureSignalQuery(t, NewContractRateSignal(), "FROM transfer_contract", []Row{{"5000"}})
		monitorRollbackTx(t, ctx, func(tx pgx.Tx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'`))
			var end time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT date_trunc('minute', now()) AT TIME ZONE 'UTC'`).Scan(&end))
			end = end.UTC()
			start := end.Add(-time.Minute)
			for _, createTime := range []time.Time{
				start.Add(-time.Minute),
				start.Add(-time.Microsecond),
				start,
				start.Add(time.Microsecond),
				start.Add(30 * time.Second),
				end.Add(-time.Microsecond),
				end,
				end.Add(time.Microsecond),
			} {
				for _, id := range monitorContractIdsAround(createTime) {
					insertMonitorTestContract(t, ctx, tx, id, server.NewId(), createTime, false)
				}
			}
			// the extreme ids of the minute's range
			insertMonitorTestContract(t, ctx, tx, server.ContractIdFloor(start.Add(-model.ContractIdCreateTimeMargin)), server.NewId(), start, false)
			insertMonitorTestContract(t, ctx, tx, monitorHighestContractIdBefore(end), server.NewId(), end.Add(-time.Microsecond), false)

			original := monitorQueryRows(t, ctx, tx, contractRateCountByCreateTimeSql)
			rewritten := monitorQueryRows(t, ctx, tx, query)
			if !reflect.DeepEqual(rewritten, original) || len(original) != 1 || original[0][0] == "0" {
				t.Fatalf("minute count %v, the create_time original %v", rewritten, original)
			}
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL TIME ZONE 'America/Chicago'`))
			if zoned := monitorQueryRows(t, ctx, tx, query); !reflect.DeepEqual(zoned, original) {
				t.Fatalf("minute count %v in America/Chicago, %v in UTC", zoned, original)
			}
		})
	})
}

// The stale contract read, every aggregate of it, equals the create_time-only
// original's.
func TestStaleContractsMatchesCreateTime(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := captureSignalQuery(t, NewStaleContractsSignal(), "stale_success", nil)
		monitorRollbackTx(t, ctx, func(tx pgx.Tx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'`))
			var now time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT now() AT TIME ZONE 'UTC'`).Scan(&now))
			now = now.UTC()
			start := now.Add(-5 * time.Minute)
			inactiveDestination := func(createTime time.Time, lead time.Duration) server.Id {
				destinationId := server.NewId()
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client (client_id, network_id, active, deactivate_time) VALUES ($1, $2, false, $3)`,
					destinationId, server.NewId(), createTime.Add(-lead).UTC(),
				))
				return destinationId
			}
			for index, createTime := range []time.Time{
				start.Add(-time.Minute),
				start.Add(-time.Microsecond),
				start,
				start.Add(time.Microsecond),
				now.Add(-time.Minute),
				now.Add(-time.Microsecond),
			} {
				for offset, id := range monitorContractIdsAround(createTime) {
					// a second and a half before creation is a stale success,
					// half a second an ambiguous timestamp
					lead := 1500 * time.Millisecond
					if (index+offset)%3 == 0 {
						lead = 500 * time.Millisecond
					}
					insertMonitorTestContract(t, ctx, tx, id, inactiveDestination(createTime, lead), createTime, offset == 4)
				}
				// an active destination is not stale
				activeId := server.NewId()
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`, activeId, server.NewId()))
				insertMonitorTestContract(t, ctx, tx, monitorContractIdMintedAt(createTime), activeId, createTime, false)
			}
			// the lowest id of the window's range
			insertMonitorTestContract(t, ctx, tx, server.ContractIdFloor(start.Add(-model.ContractIdCreateTimeMargin)), inactiveDestination(start, 1500*time.Millisecond), start, false)

			original := monitorQueryRows(t, ctx, tx, staleContractsByCreateTimeSql)
			rewritten := monitorQueryRows(t, ctx, tx, query)
			if !reflect.DeepEqual(rewritten, original) || len(original) != 2 {
				t.Fatalf("stale contracts %v, the create_time original %v", rewritten, original)
			}
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL TIME ZONE 'America/Chicago'`))
			if zoned := monitorQueryRows(t, ctx, tx, query); !reflect.DeepEqual(zoned, original) {
				t.Fatalf("stale contracts %v in America/Chicago, %v in UTC", zoned, original)
			}
		})
	})
}
