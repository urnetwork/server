// The census retains exact original completed-work rows from the same database
// snapshot as usage, independently of billing and current membership.
package model

import (
	"bytes"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Both actual terminal-report paths contribute equal completed bytes; deleting
// cash sweeps or changing directory rows cannot rewrite their original census.
func TestStClosedWorkCensusIncludesActualPaidAndFreeOriginals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := server.NowUtc().Truncate(time.Microsecond)
		paidNetwork, providerNetwork, freeNetwork := server.NewId(), server.NewId(), server.NewId()
		payer, provider, freeOrigin, freeProvider := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3), contractPayoutTestId(4)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{payer: paidNetwork, provider: providerNetwork, freeOrigin: freeNetwork, freeProvider: freeNetwork})
		addContractPayoutTestBalance(ctx, paidNetwork, 242)
		paid, err := CreateTransferEscrow(ctx, paidNetwork, payer, providerNetwork, provider, 121)
		if err != nil {
			t.Fatal(err)
		}
		free, err := CreateContractNoEscrow(ctx, freeNetwork, freeOrigin, freeNetwork, freeProvider, 121)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []struct{ id, source, destination server.Id }{{id: paid.ContractId, source: payer, destination: provider}, {id: free, source: freeOrigin, destination: freeProvider}} {
			if err := CloseContract(ctx, value.id, value.source, 121, false); err != nil {
				t.Fatal(err)
			}
			if err := CloseContract(ctx, value.id, value.destination, 120, false); err != nil {
				t.Fatal(err)
			}
		}
		usages, census, err := GetStEpochProviderUsageCensus(ctx, 17, start, start.Add(time.Hour))
		if err != nil || census == nil || census.Count != 2 || len(census.Records) != 2 || len(usages) != 2 || usages[0].PayoutByteCount != 120 || usages[1].PayoutByteCount != 120 {
			t.Fatal("actual paid/free terminal rows did not produce complete original census", usages, census, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, record := range census.Records {
				var original []byte
				server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, server.Id(record.ContractId)).Scan(&original))
				if !bytes.Equal(original, record.Original) {
					t.Fatal("census substituted reconstructed rows for original database bytes")
				}
			}
		})
		originalHash := census.Hash()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow_sweep WHERE contract_id=$1`, paid.ContractId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id=$2 WHERE client_id=ANY($1::uuid[])`, []server.Id{provider, freeProvider}, server.NewId()))
		})
		_, again, err := GetStEpochProviderUsageCensus(ctx, 17, start, start.Add(time.Hour))
		if err != nil || again == nil || again.Hash() != originalHash {
			t.Fatal("billing or directory mutation changed original completed work", again, err)
		}
	})
}

// Real archive publication and live deletion cannot expose a gap or duplicate
// to the one-statement census. An uncommitted deletion stays invisible.
func TestStClosedWorkCensusRetainsLiveAndArchivedOriginalRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := server.NowUtc().Truncate(time.Microsecond)
		first := addStContractUsageSnapshotTestRow(t, ctx, start, &contractUsageSnapshot{Version: 1, ByteCount: 121, Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 121}}})
		addStContractUsageSnapshotTestRow(t, ctx, start, &contractUsageSnapshot{Version: 1, ByteCount: 73, Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 73}}})
		_, before, err := GetStEpochProviderUsageCensus(ctx, 17, start, start.Add(time.Hour))
		if err != nil || before == nil || before.Count != 2 {
			t.Fatal("original live census", before, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.Begin(ctx)
			server.Raise(err)
			defer tx.Rollback(ctx)
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, first))
			_, visible, err := GetStEpochProviderUsageCensus(ctx, 17, start, start.Add(time.Hour))
			if err != nil || visible == nil || visible.Hash() != before.Hash() {
				t.Fatal("uncommitted retention exposed a partial census", visible, err)
			}
			server.Raise(tx.Commit(ctx))
		})
		usages, after, err := GetStEpochProviderUsageCensus(ctx, 17, start, start.Add(time.Hour))
		if err != nil || after == nil || after.Hash() != before.Hash() || len(usages) != 2 {
			t.Fatal("committed archive changed original closed-work census", after, err)
		}
	})
}

// A missing original proof still refuses the existing payout operation; the
// optional recorder cannot convert one valid prefix into a complete witness.
func TestStClosedWorkCensusMissingOriginalNeverReturnsPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := server.NowUtc().Truncate(time.Microsecond)
		addStContractUsageSnapshotTestRow(t, ctx, start, &contractUsageSnapshot{Version: 1, ByteCount: 73, Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 73}}})
		addStContractUsageSnapshotTestRow(t, ctx, start, nil)
		usages, census, err := GetStEpochProviderUsageCensus(ctx, 17, start, start.Add(time.Hour))
		if err == nil || usages != nil || census != nil {
			t.Fatal("incomplete original work returned a successful census prefix", usages, census, err)
		}
	})
}
