// Real database regressions pin expiry/report ordering, durable restart,
// prospective proof ownership, and quiet-period admission without sleeps.
package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Reads the exact retained bytes as well as their validated accounting view.
func readContractExpiryTestSnapshot(t testing.TB, ctx context.Context, id server.Id) ([]byte, *contractUsageSnapshot) {
	t.Helper()
	var data []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&data))
	})
	snapshot, err := decodeContractUsageSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	return data, snapshot
}

// Authenticated final/checkpoint reports count equally on either orientation,
// with exact same-network attribution and no payment-state dependency.
func TestStContractUsageExpiryCreditsOriginalCheckpointReports(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		network := server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: network, provider: network})
		var ids []server.Id
		for _, c := range []struct{ sourceCheckpoint, destinationCheckpoint bool }{
			{sourceCheckpoint: false, destinationCheckpoint: true},
			{sourceCheckpoint: true, destinationCheckpoint: false},
			{sourceCheckpoint: true, destinationCheckpoint: true},
		} {
			id, err := CreateContractNoEscrow(ctx, network, origin, network, provider, 100)
			if err != nil {
				t.Fatal(err)
			}
			if err := CloseContract(ctx, id, origin, 91, c.sourceCheckpoint); err != nil {
				t.Fatal(err)
			}
			if err := CloseContract(ctx, id, provider, 90, c.destinationCheckpoint); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		count, err := ForceCloseOpenContractIds(ctx, start.Add(time.Hour), 10, 2, 0, 0)
		if err != nil || count != 3 {
			t.Fatalf("expiry: %d, %v", count, err)
		}
		for _, id := range ids {
			before, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
			if snapshot.ByteCount != 90 || snapshot.Expiry == nil || snapshot.Providers[0].ClientId != provider {
				t.Fatalf("original lower bound lost: %s", before)
			}
			if err := CloseContract(ctx, id, provider, 10, false); !errors.Is(err, errContractAlreadySettled) {
				t.Fatalf("late close admitted: %v", err)
			}
			after, _ := readContractExpiryTestSnapshot(t, ctx, id)
			if !bytes.Equal(before, after) {
				t.Fatal("terminal snapshot rewritten")
			}
		}
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil || len(usages) != 1 || usages[0].PayoutByteCount != 270 {
			t.Fatalf("expiry demand total: %+v, %v", usages, err)
		}
	})
}

// An original one-sided proof remains one-sided across a crash, a late real
// report, and a legacy marker whose current bilateral rows may be synthetic.
func TestStContractUsageExpiryRestartNeverReconstructsMissingPeer(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		network := server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: network, provider: network})
		for _, legacy := range []bool{false, true} {
			id, err := CreateContractNoEscrow(ctx, network, origin, network, provider, 100)
			if err != nil {
				t.Fatal(err)
			}
			if err := CloseContract(ctx, id, provider, 90, true); err != nil {
				t.Fatal(err)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				if legacy {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_unverified=true WHERE contract_id=$1`, id))
				} else {
					state, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(time.Hour))
					server.Raise(err)
					if state == nil {
						t.Fatal("fixture failed to own expiry")
					}
				}
			})
			// A restarted owner cannot tell this real late peer from an old
			// synthesized row without the immutable pre-expiry proof.
			if err := CloseContract(ctx, id, origin, 90, false); err != nil {
				t.Fatal(err)
			}
			count, err := ForceCloseOpenContractIds(ctx, server.NowUtc().Add(-24*time.Hour), 10, 1, 0, 0)
			if err != nil || count != 1 {
				t.Fatalf("owned expiry delayed by new close timestamp: %d, %v", count, err)
			}
			data, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
			if snapshot.ByteCount != 0 || snapshot.ExcludedReason != "expired_unconfirmed" {
				t.Fatalf("reconstructed synthetic credit: %s", data)
			}
			if !legacy && (snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 1) {
				t.Fatalf("original absence was not retained: %s", data)
			}
		}
	})
}

// A row-lock barrier proves a real CloseContract cannot modify reports while
// expiry is choosing its immutable lower bound. Later deltas remain billing
// inputs, but cannot rewrite already owned usage or duplicate its credit.
func TestStContractUsageExpirySerializesRealReports(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		network := server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: network, provider: network})
		id, err := CreateContractNoEscrow(ctx, network, origin, network, provider, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			client server.Id
			count  ByteCount
		}{{client: origin, count: 20}, {client: provider, count: 30}} {
			if err := CloseContract(ctx, id, c.client, c.count, true); err != nil {
				t.Fatal(err)
			}
		}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		blocker := contractLifecycleTestBackendPid(t, ctx, tx)
		if state, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(time.Hour)); err != nil || state == nil {
			t.Fatalf("prepare original reports: %+v, %v", state, err)
		}
		done := runPaymentModelTest(func() error { return CloseContract(ctx, id, origin, 10, false) })
		requireContractLifecycleBlockedBy(t, ctx, tx, blocker)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		before, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
		if snapshot.ByteCount != 20 || snapshot.Expiry.Reports[ContractPartySource].ByteCount != 20 {
			t.Fatalf("late report altered original proof: %s", before)
		}
		if _, err := ForceCloseOpenContractIds(ctx, server.NowUtc().Add(-time.Hour), 10, 1, 0, 0); err != nil {
			t.Fatal(err)
		}
		after, _ := readContractExpiryTestSnapshot(t, ctx, id)
		if !bytes.Equal(before, after) {
			t.Fatal("retirement replay replaced retained proof")
		}
	})
}

// A recent authenticated report withdraws an old creation-time candidate.
// Filtering before LIMIT also prevents that active contract from hiding the
// next genuinely idle contract in every task iteration.
func TestStContractUsageExpiryQuietPeriodAndScanAdmission(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := server.NowUtc().Add(-time.Hour)
		network := server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: network, provider: network})
		ids := []server.Id{}
		for range 2 {
			id, err := CreateContractNoEscrow(ctx, network, origin, network, provider, 100)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, ids[0], cutoff.Add(-2*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, ids[1], cutoff.Add(-time.Hour)))
		})
		if err := CloseContract(ctx, ids[0], provider, 20, true); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			state, err := prepareContractExpiryInTx(ctx, tx, ids[0], cutoff)
			if err != nil || state != nil {
				t.Fatalf("fresh report ignored after candidate selection: %+v, %v", state, err)
			}
		})
		count, err := ForceCloseOpenContractIds(ctx, cutoff, 1, 1, 0, 0)
		if err != nil || count != 1 {
			t.Fatalf("active candidate starved idle contract: %d, %v", count, err)
		}
		if _, closed := GetContractClose(ctx, ids[0]); closed {
			t.Fatal("recent report contract was retired")
		}
		if _, closed := GetContractClose(ctx, ids[1]); !closed {
			t.Fatal("idle contract did not retire")
		}
		// The exact cutoff is inclusive, and both recent-report directions
		// use the same bound independently of the original creation time.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, ids[0], cutoff))
		})
		if count, err := ForceCloseOpenContractIds(ctx, cutoff, 1, 1, 0, 0); err != nil || count != 1 {
			t.Fatalf("quiet cutoff equality did not retire: %d, %v", count, err)
		}
	})
}

// An aborted expiry owns no durable marker. A later real report remains
// eligible to become part of the eventual original bilateral proof.
func TestStContractUsageExpiryProofRollback(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		network := server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: network, provider: network})
		id, err := CreateContractNoEscrow(ctx, network, origin, network, provider, 100)
		if err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, id, origin, 80, true); err != nil {
			t.Fatal(err)
		}
		abort := errors.New("synthetic expiry transaction abort")
		if recovered := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(time.Hour))
				server.Raise(err)
				panic(abort)
			})
		}); recovered != abort {
			t.Fatalf("unexpected abort: %v", recovered)
		}
		var unverified bool
		var data []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT usage_unverified,provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&unverified, &data))
		})
		if unverified || len(data) != 0 {
			t.Fatalf("aborted proof leaked: %t, %s", unverified, data)
		}
		if err := CloseContract(ctx, id, provider, 70, true); err != nil {
			t.Fatal(err)
		}
		if _, err := ForceCloseOpenContractIds(ctx, server.NowUtc().Add(time.Hour), 10, 1, 0, 0); err != nil {
			t.Fatal(err)
		}
		data, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
		if snapshot.ByteCount != 70 || len(snapshot.Expiry.Reports) != 2 {
			t.Fatalf("recovery lost independently reported work: %s", data)
		}
	})
}
