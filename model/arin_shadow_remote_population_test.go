// Real ARIN population fixtures retain session triggers while bounding each
// setup transaction's endpoint fences. Collection still sees the full census.
package model

import (
	"context"
	"crypto/md5"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

const arinRemotePopulationBatchSize = 512

// The seed retains its original handler; synthetic peers share twenty owners.
type arinRemotePopulationFixture struct {
	originalHandler server.Id
	handlers        []server.Id
	extraKVs        map[server.Id]server.Id
}

// PgConn executes separate autocommit statements. A single transaction for the
// whole population would retain one migration776 advisory fence per client.
func arinRemoteInsertPopulation(t testing.TB, ctx context.Context, conn server.PgConn, count int, statement string, args ...any) {
	t.Helper()
	for first := 1; first <= count; first += arinRemotePopulationBatchSize {
		last := min(first+arinRemotePopulationBatchSize-1, count)
		arguments := append(append([]any(nil), args...), first, last)
		result, err := conn.Exec(ctx, statement, arguments...)
		server.Raise(err)
		if result.RowsAffected() != int64(last-first+1) {
			t.Fatalf("ARIN fixture lost an original row range: first=%d last=%d inserted=%d", first, last, result.RowsAffected())
		}
	}
}

// Clone every modern column from actual seed rows. Only transaction boundaries
// change; deterministic identities, handler selection and live triggers remain.
func newArinRemotePopulationFixture(t testing.TB, ctx context.Context, seed *egressTestProvider, count, extraCount int) *arinRemotePopulationFixture {
	t.Helper()
	fixture := &arinRemotePopulationFixture{handlers: make([]server.Id, 20), extraKVs: map[server.Id]server.Id{}}
	for i := range fixture.handlers {
		fixture.handlers[i] = server.NewId()
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT handler_id FROM network_client_connection WHERE connection_id=$1`, seed.connectionId).Scan(&fixture.originalHandler))
		for _, spec := range []struct {
			table, key string
			id         server.Id
			count      int
		}{
			{table: "network_client_handler", key: "handler_id", id: fixture.originalHandler, count: 20},
			{table: "network_client", key: "client_id", id: seed.clientId, count: count},
			{table: "provide_key", key: "client_id", id: seed.clientId, count: count},
			{table: "network_client_connection", key: "connection_id", id: seed.connectionId, count: count},
			{table: "network_client_location", key: "connection_id", id: seed.connectionId, count: count},
		} {
			var columns, values []string
			rows, err := conn.Query(ctx, `SELECT quote_ident(attname) FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`, spec.table)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var col string
					server.Raise(rows.Scan(&col))
					columns = append(columns, col)
					switch {
					case spec.table == "network_client_handler" && col == "handler_id":
						values = append(values, `($2::uuid[])[n]`)
					case col == "client_id" || col == "connection_id":
						values = append(values, `md5('arin-full-'||n)::uuid`)
					case spec.table == "network_client_connection" && col == "handler_id":
						values = append(values, `($2::uuid[])[get_byte(uuid_send(md5('arin-full-'||n)::uuid),0)%20+1]`)
					default:
						values = append(values, "seed."+col)
					}
				}
			})
			where := ""
			if spec.table == "provide_key" {
				where = " AND provide_mode=3"
			}
			arinRemoteInsertPopulation(t, ctx, conn, spec.count, `INSERT INTO `+spec.table+` (`+strings.Join(columns, ",")+`) SELECT `+strings.Join(values, ",")+` FROM `+spec.table+` seed CROSS JOIN generate_series($3::integer,$4::integer) n WHERE seed.`+spec.key+`=$1 AND cardinality($2::uuid[])=20`+where, spec.id, fixture.handlers)
		}
	})
	for n := 1; n <= extraCount; n++ {
		connection := md5.Sum([]byte("arin-extra-" + strconv.Itoa(n)))
		client := md5.Sum([]byte("arin-full-" + strconv.Itoa(n)))
		fixture.extraKVs[server.Id(connection)] = server.Id(client)
	}
	if extraCount > 0 {
		server.Db(ctx, func(conn server.PgConn) {
			for _, table := range []string{"network_client_connection", "network_client_location"} {
				var columns, values []string
				rows, err := conn.Query(ctx, `SELECT quote_ident(attname) FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`, table)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var col string
						server.Raise(rows.Scan(&col))
						columns = append(columns, col)
						if col == "connection_id" {
							values = append(values, `md5('arin-extra-'||n)::uuid`)
						} else {
							values = append(values, "seed."+col)
						}
					}
				})
				arinRemoteInsertPopulation(t, ctx, conn, extraCount, `INSERT INTO `+table+` (`+strings.Join(columns, ",")+`) SELECT `+strings.Join(values, ",")+` FROM generate_series($1::integer,$2::integer) n JOIN `+table+` seed ON seed.connection_id=md5('arin-full-'||n)::uuid`)
			}
		})
	}
	server.Db(ctx, func(conn server.PgConn) {
		var rows, connected, admitted, handlers, transactions, maximumAdmits int64
		server.Raise(conn.QueryRow(ctx, `WITH expected AS (
 SELECT md5('arin-full-'||n)::uuid AS client_id,md5('arin-full-'||n)::uuid AS connection_id,1::bigint AS sequence
 FROM generate_series(1,$1::integer) n
 UNION ALL
 SELECT md5('arin-full-'||n)::uuid,md5('arin-extra-'||n)::uuid,2::bigint
 FROM generate_series(1,$2::integer) n
), observed AS (
 SELECT x.client_id,c.handler_id,c.connected,n.network_id,n.active,n.source_client_id,e.transaction_id
 FROM expected x
 LEFT JOIN network_client_connection c ON c.connection_id=x.connection_id AND c.client_id=x.client_id
 LEFT JOIN network_client n ON n.client_id=x.client_id
 LEFT JOIN provider_work_session_event e ON e.client_id=x.client_id AND e.connection_id=x.connection_id
  AND e.sequence=x.sequence AND e.kind='admit' AND e.network_id=$4
), transactions AS (
 SELECT transaction_id,count(*) AS admits FROM observed WHERE transaction_id IS NOT NULL GROUP BY transaction_id
)
SELECT count(*),count(*) FILTER(WHERE connected AND active AND source_client_id IS NULL AND network_id=$4
 AND handler_id=($3::uuid[])[get_byte(uuid_send(client_id),0)%20+1]),count(transaction_id),count(DISTINCT handler_id),
 (SELECT count(*) FROM transactions),COALESCE((SELECT max(admits) FROM transactions),0)
FROM observed`, count, extraCount, fixture.handlers, seed.networkId).Scan(&rows, &connected, &admitted, &handlers, &transactions, &maximumAdmits))
		want := int64(count + extraCount)
		wantTransactions := int64((count+arinRemotePopulationBatchSize-1)/arinRemotePopulationBatchSize + (extraCount+arinRemotePopulationBatchSize-1)/arinRemotePopulationBatchSize)
		if rows != want || connected != want || admitted != want || handlers != 20 || transactions != wantTransactions || maximumAdmits > 512 {
			t.Fatalf("ARIN fixture lost bounded original session transactions: rows=%d connected=%d admitted=%d handlers=%d transactions=%d maximum_admits=%d want=%d want_transactions=%d", rows, connected, admitted, handlers, transactions, maximumAdmits, want, wantTransactions)
		}
	})
	return fixture
}

// Original admission transaction ids prove real commit boundaries, including
// both remainder batches, without relying on a server's shared-lock capacity.
func TestArinRemotePopulationRetainsBoundedOriginalSessionTransactions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Batch", "Batch", "Batch", "zz")
		seed := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		fixture := newArinRemotePopulationFixture(t, ctx, seed, 1025, 513)
		if len(fixture.extraKVs) != 513 || len(fixture.handlers) != 20 {
			t.Fatal("bounded fixture changed the complete synthetic owner population")
		}
	})
}
