// Pins pair lookup query count and nullable lifecycle semantics using the real
// PostgreSQL path, with a caller-owned query wrapper rather than global hooks.
package model

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Counts actual commands and rows crossing one owned read boundary.
type activeClientPairTestQuery struct {
	server.PgCanQuery
	queries int
	rows    int
}

// Delegates the production SQL unchanged and counts its returned row.
func (self *activeClientPairTestQuery) Query(ctx context.Context, sql string, args ...any) (server.PgResult, error) {
	self.queries++
	rows, err := self.PgCanQuery.Query(ctx, sql, args...)
	if err != nil {
		return rows, err
	}
	return &activeClientPairTestRows{PgResult: rows, rows: &self.rows}, nil
}

// Tracks iteration without substituting a synthetic query result.
type activeClientPairTestRows struct {
	server.PgResult
	rows *int
}

// A failed first row must retain its database error instead of becoming an
// absent endpoint or the impossible successful-query/no-row sentinel.
type failedActiveClientPairQuery struct {
	server.PgCanQuery
	err error
}

func (self failedActiveClientPairQuery) Query(context.Context, string, ...any) (server.PgResult, error) {
	return failedActiveClientPairRows{err: self.err}, nil
}

type failedActiveClientPairRows struct {
	server.PgResult
	err error
}

func (self failedActiveClientPairRows) Next() bool { return false }
func (self failedActiveClientPairRows) Err() error { return self.err }
func (self failedActiveClientPairRows) Close()     {}

func TestFindActiveClientPairNetworksPreservesRowError(t *testing.T) {
	want := errors.New("pair lookup read failed")
	defer func() {
		if got := recover(); got != want {
			t.Fatalf("pair lookup panic = %v, want original row error", got)
		}
	}()
	findActiveClientPairNetworks(t.Context(), failedActiveClientPairQuery{err: want}, server.NewId(), server.NewId())
}

// A successful iteration is one row actually received from PostgreSQL.
func (self *activeClientPairTestRows) Next() bool {
	if !self.PgResult.Next() {
		return false
	}
	*self.rows++
	return true
}

// Presence, activity, identical endpoints and network ownership all keep the
// individual lookup's meaning, with one statement and one returned row.
func TestFindActiveClientPairNetworksUsesOneQuery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		sourceId, destinationId := server.NewId(), server.NewId()
		sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{sourceId: sourceNetworkId, destinationId: destinationNetworkId})
		for _, test := range []struct {
			name              string
			sourceId          server.Id
			destinationId     server.Id
			sourceActive      bool
			destinationActive bool
			wantSource        *server.Id
			wantDestination   *server.Id
		}{
			{name: "active different networks", sourceId: sourceId, destinationId: destinationId, sourceActive: true, destinationActive: true, wantSource: &sourceNetworkId, wantDestination: &destinationNetworkId},
			{name: "inactive source", sourceId: sourceId, destinationId: destinationId, destinationActive: true, wantDestination: &destinationNetworkId},
			{name: "inactive destination", sourceId: sourceId, destinationId: destinationId, sourceActive: true, wantSource: &sourceNetworkId},
			{name: "both inactive", sourceId: sourceId, destinationId: destinationId},
			{name: "missing source", sourceId: server.NewId(), destinationId: destinationId, sourceActive: true, destinationActive: true, wantDestination: &destinationNetworkId},
			{name: "missing destination", sourceId: sourceId, destinationId: server.NewId(), sourceActive: true, destinationActive: true, wantSource: &sourceNetworkId},
			{name: "both missing", sourceId: server.NewId(), destinationId: server.NewId()},
			{name: "same active endpoint", sourceId: sourceId, destinationId: sourceId, sourceActive: true, wantSource: &sourceNetworkId, wantDestination: &sourceNetworkId},
			{name: "same inactive endpoint", sourceId: sourceId, destinationId: sourceId},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = CASE client_id WHEN $1 THEN $3::boolean ELSE $4::boolean END WHERE client_id IN ($1,$2)`, sourceId, destinationId, test.sourceActive, test.destinationActive))
			})
			server.Db(ctx, func(conn server.PgConn) {
				query := &activeClientPairTestQuery{PgCanQuery: conn}
				source, destination := findActiveClientPairNetworks(ctx, query, test.sourceId, test.destinationId)
				if query.queries != 1 || query.rows != 1 {
					t.Fatalf("%s: queries=%d rows=%d, want 1 each", test.name, query.queries, query.rows)
				}
				matches := func(got, want *server.Id) bool {
					return got == nil && want == nil || got != nil && want != nil && *got == *want
				}
				if !matches(source, test.wantSource) || !matches(destination, test.wantDestination) {
					t.Fatalf("%s: active pair network mappings changed", test.name)
				}
			})
		}
	})
}

// The fresh pair result is only preflight data: a committed deactivation after
// it must still be rejected by both transaction-side contract writers.
func TestFindActiveClientPairNetworksDoesNotBypassWriteLocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		sourceId, destinationId := server.NewId(), server.NewId()
		sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{sourceId: sourceNetworkId, destinationId: destinationNetworkId})
		source, destination := FindActiveClientPairNetworks(ctx, sourceId, destinationId)
		if source == nil || destination == nil {
			t.Fatal("active pair missing before deactivation")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, destinationId))
		})
		contractId, err := CreateContractNoEscrow(ctx, *source, sourceId, *destination, destinationId, 1024)
		if !errors.Is(err, ErrContractDestinationInactive) || contractId != (server.Id{}) {
			t.Fatalf("no-escrow writer accepted a deactivated destination: %v", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			escrow, posts, err := createTransferEscrowInTx(ctx, tx, *source, sourceId, *destination, destinationId, *source, 0, nil)
			if !errors.Is(err, ErrContractDestinationInactive) || escrow != nil || len(posts) != 0 {
				t.Fatalf("escrow writer accepted a deactivated destination: %v", err)
			}
		})
		if count := contractLifecycleTestCount(t, ctx, sourceId, destinationId); count != 0 {
			t.Fatalf("deactivated pair wrote %d contracts", count)
		}
	})
}
