package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The connection indexes after the write-path migrations: open rows by client
// and by address, open rows by handler, and closed rows by disconnect time.
const (
	testingOpenClientConnectionIndex  = "network_client_connection_connected_client_id_extender_id"
	testingOpenAddressConnectionIndex = "network_client_connection_connected_client_address_hash"
	testingOpenHandlerConnectionIndex = "network_client_connection_connected_handler_id"
	testingClosedConnectionIndex      = "network_client_connection_closed_disconnect_time"
	testingConnectionPrimaryKey       = "network_client_connection_pkey"
)

// Seeds a connection table shaped like Main's: 4,000 closed rows waiting for
// the reaper and 200 open ones, one open row per client, and analyzes it so
// each reader is planned against that distribution. Returns an open client
// and its address hash.
func testingSeedConnectionDistribution(t testing.TB, ctx context.Context) (clientId server.Id, clientAddressHash []byte) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO network_client_connection (
				client_id, connection_id, connected, connect_time, disconnect_time,
				connection_host, connection_service, connection_block, client_address_hash, handler_id
			)
			SELECT
				md5('index-client-'||(n % 200))::uuid,
				md5('index-connection-'||n)::uuid,
				n <= 200,
				$1::timestamp - interval '1 day',
				CASE WHEN n <= 200 THEN NULL ELSE $1::timestamp - (n % 480) * interval '1 minute' END,
				'connect.example', 'connect', 'test',
				decode(md5('index-address-'||(n % 200)), 'hex'),
				md5('index-handler-'||(n % 4))::uuid
			FROM generate_series(1, 4200) AS n
		`, server.NowUtc()))
	})
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE network_client_connection`))
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT client_id, client_address_hash FROM network_client_connection WHERE connected ORDER BY connection_id LIMIT 1`,
		).Scan(&clientId, &clientAddressHash))
	})
	return
}

// Every hot reader of network_client_connection reads it only through indexes
// its predicate can use: a literal connected implies the open-row partial
// indexes' predicate and connected = false the closed one's. A reader whose
// predicate did not imply them would scan the table instead, which sequential
// scans being off for planning makes visible.
func TestNetworkClientConnectionReadersUsePartialIndexes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId, clientAddressHash := testingSeedConnectionDistribution(t, ctx)
		now := server.NowUtc()
		openIndexes := []string{testingOpenClientConnectionIndex, testingOpenAddressConnectionIndex, testingOpenHandlerConnectionIndex}

		for _, reader := range []struct {
			name  string
			query string
			args  []any
			// the plan reads at least one of these
			required []string
			// and no other connection index
			allowed []string
		}{
			{
				name:     "contract insert endpoint extenders",
				query:    contractExtenderInsertSql,
				args:     []any{server.NewId(), clientId, clientId, "source", "destination"},
				required: []string{testingOpenClientConnectionIndex},
				allowed:  []string{testingOpenClientConnectionIndex},
			},
			{
				name:     "address connected",
				query:    networkClientAddressConnectedSql,
				args:     []any{clientAddressHash},
				required: []string{testingOpenAddressConnectionIndex},
				allowed:  []string{testingOpenAddressConnectionIndex},
			},
			{
				name:     "provider work session open connections",
				query:    providerWorkSessionOpenConnectionsSql,
				args:     []any{clientId, int64(65)},
				required: []string{testingOpenClientConnectionIndex},
				allowed:  []string{testingOpenClientConnectionIndex},
			},
			{
				name:     "handler retirement connections of a client",
				query:    networkClientOrphanConnectionPageSql,
				args:     []any{clientId, 128},
				required: []string{testingOpenClientConnectionIndex},
				allowed:  []string{testingOpenClientConnectionIndex},
			},
			{
				name:     "handler retirement discovery",
				query:    networkClientOrphanClientSql,
				required: []string{testingOpenHandlerConnectionIndex},
				allowed:  []string{testingOpenHandlerConnectionIndex},
			},
			{
				name:     "subscriber eligibility",
				query:    providerSubscriberExclusionsSql,
				args:     []any{[]server.Id{clientId}, now, []server.Id{}, int64(0)},
				required: openIndexes,
				allowed:  openIndexes,
			},
			{
				name:     "named subscriber eligibility",
				query:    providerNamedSubscriberExclusionsSql,
				args:     []any{[]server.Id{clientId}, now, []server.Id{}},
				required: openIndexes,
				allowed:  openIndexes,
			},
			{
				name:    "location pass open connections",
				query:   clientLocationReliabilityConnectedSql,
				args:    []any{now, false, []server.Id{}},
				allowed: append([]string{testingConnectionPrimaryKey}, openIndexes...),
			},
			{
				name:     "location pass recently closed connections",
				query:    clientLocationReliabilityRecentlyClosedSql,
				args:     []any{now.Add(-time.Hour), now, false},
				required: []string{testingClosedConnectionIndex},
				allowed:  []string{testingClosedConnectionIndex, testingConnectionPrimaryKey},
			},
			{
				name:     "reaper",
				query:    removeDisconnectedConnectionsSql,
				args:     []any{now.Add(-4 * time.Hour), 50000},
				required: []string{testingClosedConnectionIndex},
				allowed:  []string{testingClosedConnectionIndex, testingConnectionPrimaryKey},
			},
			{
				name:    "device version census",
				query:   deviceVersionStatsSql,
				args:    []any{ProvideModePublic},
				allowed: append([]string{testingConnectionPrimaryKey}, openIndexes...),
			},
		} {
			indexes, sequentialTables := writePathTestPlan(t, ctx, reader.query, reader.args...)
			if sequentialTables["network_client_connection"] {
				t.Errorf("%s scans network_client_connection: its predicate implies no index predicate", reader.name)
				continue
			}
			read := []string{}
			for index := range indexes {
				if strings.HasPrefix(index, "network_client_connection_") {
					read = append(read, index)
				}
			}
			for _, index := range read {
				allowed := false
				for _, candidate := range reader.allowed {
					allowed = allowed || index == candidate
				}
				if !allowed {
					t.Errorf("%s reads %s; it may read only %v", reader.name, index, reader.allowed)
				}
			}
			if 0 < len(reader.required) {
				found := false
				for _, index := range read {
					for _, candidate := range reader.required {
						found = found || index == candidate
					}
				}
				if !found {
					t.Errorf("%s reads %v, want one of %v", reader.name, read, reader.required)
				}
			}
		}
	})
}

// The reaper deletes the closed connections that disconnected before its
// cutoff, with their location, latency and speed rows, and keeps the open
// connections and the newer closed ones.
func TestRemoveDisconnectedNetworkClientsReapsOnlyOldClosedConnections(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		cutoff := now.Add(-8 * time.Hour)
		clientId := server.NewId()

		insert := func(connected bool, disconnectTime *time.Time) server.Id {
			connectionId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_client_connection (
						client_id, connection_id, connected, connect_time, disconnect_time,
						connection_host, connection_service, connection_block
					) VALUES ($1, $2, $3, $4, $5, 'connect.example', 'connect', 'test')`,
					clientId, connectionId, connected, now.Add(-24*time.Hour), disconnectTime,
				))
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_client_location (connection_id, client_id, city_location_id, region_location_id, country_location_id)
					VALUES ($1, $2, $3, $4, $5)`,
					connectionId, clientId, server.NewId(), server.NewId(), server.NewId(),
				))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_latency (connection_id, latency_ms) VALUES ($1, 10)`, connectionId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_speed (connection_id, bytes_per_second) VALUES ($1, 1024)`, connectionId))
			})
			return connectionId
		}
		oldDisconnectTime := cutoff.Add(-time.Hour)
		newDisconnectTime := cutoff.Add(time.Hour)
		oldClosedId := insert(false, &oldDisconnectTime)
		newClosedId := insert(false, &newDisconnectTime)
		openId := insert(true, nil)

		// clients are kept: no auth time is that old
		longAgo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		RemoveDisconnectedNetworkClients(ctx, cutoff, longAgo, longAgo)

		count := func(table string, connectionId server.Id) (n int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE connection_id = $1`, connectionId).Scan(&n))
			})
			return
		}
		for _, table := range []string{"network_client_connection", "network_client_location", "network_client_latency", "network_client_speed"} {
			if n := count(table, oldClosedId); n != 0 {
				t.Errorf("%s keeps %d rows of the connection closed before the cutoff", table, n)
			}
			if n := count(table, newClosedId); n != 1 {
				t.Errorf("%s has %d rows of the connection closed after the cutoff, want 1", table, n)
			}
			if n := count(table, openId); n != 1 {
				t.Errorf("%s has %d rows of the open connection, want 1", table, n)
			}
		}
	})
}
