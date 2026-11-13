// The SN25 extender amendment's epoch read in short slices
// (GetStEpochAmendedUsageInSlices) on a real database: the totals of the read
// in one snapshot, each slice in a snapshot of its own, a failed slice read
// again from nothing, and a refused row refused at once. Each test runs on the
// test environment's fresh database, with every migration applied.
//
// The defect (server CPUOPT5.md #1): the extender shadow read a 7-day epoch's
// contracts in one REPEATABLE READ snapshot on Main's primary, which pinned
// the primary's xmin horizon for the whole read.
package model

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"

	"github.com/urnetwork/server"
)

// A three-hour window's contracts, by hour: same-network contracts funded,
// unfunded and paid by a measurement network, a cross-network contract, a
// zero-byte one, and in the second hour enough ordinary contracts that its
// slice reads them whole, in one block-ordered pass.
type stAmendedSliceFixture struct {
	start, end  time.Time
	measurement map[server.Id]bool
	provider    ContractParticipant
}

func newStAmendedSliceFixture(t testing.TB) *stAmendedSliceFixture {
	t.Helper()
	ctx := t.Context()
	id := contractPayoutTestId
	f := &stAmendedSliceFixture{start: time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)}
	f.end = f.start.Add(3 * time.Hour)
	netN, netM, netA, netX := id(104), id(105), id(101), id(103)
	f.measurement = map[server.Id]bool{netM: true}
	f.provider = ContractParticipant{ClientId: id(2), NetworkId: id(102)}
	r, rM, x := ContractParticipant{ClientId: id(5), NetworkId: netN}, ContractParticipant{ClientId: id(7), NetworkId: netM}, ContractParticipant{ClientId: id(3), NetworkId: netX}
	for hour := range 3 {
		at := f.start.Add(time.Duration(hour)*time.Hour + time.Minute)
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at, sourceId: id(4), sourceNetworkId: netN, destinationId: r.ClientId, destinationNetworkId: netN, originIsSource: stAmendedDbBool(true), payerNetworkId: &netN, snapshot: stAmendedDbSnapshot(t, ByteCount(2_001+hour), r, x)})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at.Add(time.Minute), sourceId: id(4), sourceNetworkId: netN, destinationId: r.ClientId, destinationNetworkId: netN, originIsSource: stAmendedDbBool(true), snapshot: stAmendedDbSnapshot(t, ByteCount(301+hour), r, x)})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at.Add(2 * time.Minute), sourceId: id(6), sourceNetworkId: netM, destinationId: rM.ClientId, destinationNetworkId: netM, originIsSource: stAmendedDbBool(true), payerNetworkId: &netM, snapshot: stAmendedDbSnapshot(t, ByteCount(50+hour), rM)})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at.Add(3 * time.Minute), sourceId: id(1), sourceNetworkId: netA, destinationId: f.provider.ClientId, destinationNetworkId: f.provider.NetworkId, originIsSource: stAmendedDbBool(true), payerNetworkId: &netA, snapshot: stAmendedDbSnapshot(t, ByteCount(1_001+hour), f.provider, x)})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at.Add(4 * time.Minute), sourceId: id(1), sourceNetworkId: netA, destinationId: f.provider.ClientId, destinationNetworkId: f.provider.NetworkId, snapshot: stAmendedDbSnapshot(t, 0, f.provider)})
	}
	// the second hour past the census record bound
	ordinary := stAmendedDbSnapshot(t, 11, f.provider)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (contract_id,source_id,source_network_id,destination_id,destination_network_id,
				transfer_byte_count,outcome,close_time,provider_usage,usage_origin_is_source,payer_network_id)
			SELECT md5('sliced-'||n)::uuid,md5('sliced-source-'||n)::uuid,$2,md5('sliced-destination-'||n)::uuid,$3,
				1000000,'settled',$1::timestamp+n*interval '10 milliseconds',$4,true,$2
			FROM generate_series(0,$5::int-1) n
		`, f.start.Add(time.Hour+10*time.Minute), netA, f.provider.NetworkId, ordinary, payoutartifact.MaxClosedWorkRecords+7))
	})
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `VACUUM ANALYZE transfer_contract`))
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_provider_usage_archive`))
	})
	return f
}

// Reports whether two reads hold the same usages, in the same order, and the
// same pool.
func stAmendedSameRead(usagesA []*StProviderUsage, poolA *StEpochExtenderPool, usagesB []*StProviderUsage, poolB *StEpochExtenderPool) bool {
	if !slices.EqualFunc(usagesA, usagesB, func(a, b *StProviderUsage) bool { return *a == *b }) {
		return false
	}
	if stExtenderPoolTotals(poolA) != stExtenderPoolTotals(poolB) || len(poolA.ClientIdPooledBytes) != len(poolB.ClientIdPooledBytes) {
		return false
	}
	for clientId, bytes := range poolA.ClientIdPooledBytes {
		if poolB.ClientIdPooledBytes[clientId] != bytes {
			return false
		}
	}
	return true
}

// Records each slice attempt a sliced read starts, for the test's life.
func stAmendedSliceStarts(t testing.TB) *[]time.Time {
	t.Helper()
	starts := &[]time.Time{}
	t.Cleanup(Testing_SetStEpochUsageSliceHook(func(sliceStart time.Time, attempt int, read bool) error {
		if !read {
			*starts = append(*starts, sliceStart)
		}
		return nil
	}))
	return starts
}

// An hour-sliced read of the three-hour window reads three slices, one an hour
// and the second whole, to exactly the totals of the read in one snapshot; a
// span longer than the window reads it in one slice, and a non-positive span
// is the one-snapshot read.
func TestStExtenderDbAmendedUsageInSlicesHasTheTotalsOfOneSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newStAmendedSliceFixture(t)
		amendment := StExtenderAmendment{FirstEpoch: 8}
		usages, pool, err := GetStEpochAmendedUsageAtEpoch(ctx, 8, f.start, f.end, amendment, f.measurement)
		if err != nil {
			t.Fatal(err)
		}
		if got := stAmendedDbUsages(usages)[f.provider.ClientId]; got <= int64(payoutartifact.MaxClosedWorkRecords+7)*11 {
			t.Fatalf("the provider's usage %d misses the whole-read hour or its cross-network shares", got)
		}
		if pool.Contracts != 3 || pool.UnfundedContracts != 3 || pool.MeasurementContracts != 3 || pool.Payers != 1 {
			t.Fatalf("pool %+v", pool)
		}
		starts := stAmendedSliceStarts(t)
		sliced, slicedPool, err := GetStEpochAmendedUsageInSlices(ctx, 8, f.start, f.end, amendment, f.measurement, StEpochUsageSlices{Span: time.Hour, Attempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		if !stAmendedSameRead(usages, pool, sliced, slicedPool) {
			t.Fatalf("sliced %+v %+v, want %+v %+v", sliced, slicedPool, usages, pool)
		}
		if want := []time.Time{f.start, f.start.Add(time.Hour), f.start.Add(2 * time.Hour)}; !slices.EqualFunc(*starts, want, time.Time.Equal) {
			t.Fatalf("slices from %v, want %v", *starts, want)
		}
		*starts = nil
		once, oncePool, err := GetStEpochAmendedUsageInSlices(ctx, 8, f.start, f.end, amendment, f.measurement, StEpochUsageSlices{Span: 24 * time.Hour})
		if err != nil || !stAmendedSameRead(usages, pool, once, oncePool) || len(*starts) != 1 {
			t.Fatalf("one slice: %v, %d slices", err, len(*starts))
		}
		*starts = nil
		whole, wholePool, err := GetStEpochAmendedUsageInSlices(ctx, 8, f.start, f.end, amendment, f.measurement, StEpochUsageSlices{})
		if err != nil || !stAmendedSameRead(usages, pool, whole, wholePool) || len(*starts) != 0 {
			t.Fatalf("no span: %v, %d slices", err, len(*starts))
		}
	})
}

// Each slice is its own snapshot: a contract that closes in the third hour,
// written once the first slice is read, is counted by the sliced read, which a
// read in one snapshot taken at its start could not see.
func TestStExtenderDbAmendedUsageInSlicesReadsEachSliceInItsOwnSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newStAmendedSliceFixture(t)
		amendment := StExtenderAmendment{FirstEpoch: 8}
		before, beforePool, err := GetStEpochAmendedUsageAtEpoch(ctx, 8, f.start, f.end, amendment, f.measurement)
		if err != nil {
			t.Fatal(err)
		}
		late := ContractParticipant{ClientId: contractPayoutTestId(30), NetworkId: contractPayoutTestId(130)}
		netA := contractPayoutTestId(101)
		t.Cleanup(Testing_SetStEpochUsageSliceHook(func(sliceStart time.Time, attempt int, read bool) error {
			if read && sliceStart.Equal(f.start) {
				addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: f.start.Add(2*time.Hour + 30*time.Minute), sourceId: contractPayoutTestId(1), sourceNetworkId: netA, destinationId: late.ClientId, destinationNetworkId: late.NetworkId, originIsSource: stAmendedDbBool(true), payerNetworkId: &netA, snapshot: stAmendedDbSnapshot(t, 77, late)})
			}
			return nil
		}))
		usages, pool, err := GetStEpochAmendedUsageInSlices(ctx, 8, f.start, f.end, amendment, f.measurement, StEpochUsageSlices{Span: time.Hour, Attempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		got := stAmendedDbUsages(usages)
		if got[late.ClientId] != 77 || len(usages) != len(before)+1 || stExtenderPoolTotals(pool) != stExtenderPoolTotals(beforePool) {
			t.Fatalf("the contract written after the first slice: %v (%d usages, %d before)", got, len(usages), len(before))
		}
	})
}

// A slice that fails once it is read is read again from nothing: the totals
// are the one-snapshot read's, never its rows twice. With one attempt the
// read fails, and a refused row fails it at once, whatever the attempts.
func TestStExtenderDbAmendedUsageInSlicesReadsAFailedSliceAgain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newStAmendedSliceFixture(t)
		amendment := StExtenderAmendment{FirstEpoch: 8}
		usages, pool, err := GetStEpochAmendedUsageAtEpoch(ctx, 8, f.start, f.end, amendment, f.measurement)
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("the slice's connection was lost")
		attempts := map[time.Time]int{}
		t.Cleanup(Testing_SetStEpochUsageSliceHook(func(sliceStart time.Time, attempt int, read bool) error {
			if !read {
				attempts[sliceStart]++
				return nil
			}
			// the whole-read hour fails once it is read
			if sliceStart.Equal(f.start.Add(time.Hour)) && attempt == 1 {
				return failure
			}
			return nil
		}))
		again, againPool, err := GetStEpochAmendedUsageInSlices(ctx, 8, f.start, f.end, amendment, f.measurement, StEpochUsageSlices{Span: time.Hour, Attempts: 2})
		if err != nil {
			t.Fatal(err)
		}
		if !stAmendedSameRead(usages, pool, again, againPool) {
			t.Fatalf("after a failed slice %+v %+v, want %+v %+v", again, againPool, usages, pool)
		}
		if attempts[f.start] != 1 || attempts[f.start.Add(time.Hour)] != 2 || attempts[f.start.Add(2*time.Hour)] != 1 {
			t.Fatalf("attempts %v", attempts)
		}
		clear(attempts)
		if _, _, err := GetStEpochAmendedUsageInSlices(ctx, 8, f.start, f.end, amendment, f.measurement, StEpochUsageSlices{Span: time.Hour, Attempts: 1}); !errors.Is(err, failure) {
			t.Fatalf("one attempt: %v", err)
		}

		// an archived contract with bytes is refused, and not read again
		provider := ContractParticipant{ClientId: contractPayoutTestId(31), NetworkId: contractPayoutTestId(131)}
		archived := addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: f.start.Add(2*time.Hour + 40*time.Minute), sourceId: contractPayoutTestId(1), sourceNetworkId: contractPayoutTestId(101), destinationId: provider.ClientId, destinationNetworkId: provider.NetworkId, originIsSource: stAmendedDbBool(true), payerNetworkId: &provider.NetworkId, snapshot: stAmendedDbSnapshot(t, 10, provider)})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id = $1`, archived))
		}, server.TxReadCommitted, server.OptNoRetry())
		clear(attempts)
		_, _, err = GetStEpochAmendedUsageInSlices(ctx, 8, f.start, f.end, amendment, f.measurement, StEpochUsageSlices{Span: time.Hour, Attempts: 3})
		if err == nil || !strings.Contains(err.Error(), "archived") || attempts[f.start.Add(2*time.Hour)] != 1 {
			t.Fatalf("a refused row: %v, attempts %v", err, attempts)
		}
	})
}
