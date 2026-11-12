// The SN25 extender pool's SQL on a real database (sn WHITEPAPER.md §8.6.2):
// the amended usage statement, read small and whole-window, the census's
// records, the census and amendments stores, and the epoch rows' extender
// component. Each test runs on the test environment's fresh database, with
// every migration applied, the extender pool's included.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/payoutartifact"

	"github.com/urnetwork/server"
)

// A live contract row as the amended statement reads it.
type stAmendedDbContract struct {
	closeTime            time.Time
	sourceId             server.Id
	sourceNetworkId      server.Id
	destinationId        server.Id
	destinationNetworkId server.Id
	// nil writes NULL, a legacy direction
	originIsSource *bool
	payerNetworkId *server.Id
	snapshot       *contractUsageSnapshot
}

func stAmendedDbBool(value bool) *bool { return &value }

func addStAmendedDbContract(t testing.TB, ctx context.Context, c stAmendedDbContract) server.Id {
	t.Helper()
	contractId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (contract_id,source_id,source_network_id,destination_id,destination_network_id,
				transfer_byte_count,outcome,close_time,provider_usage,usage_origin_is_source,payer_network_id)
			VALUES ($1,$2,$3,$4,$5,1000000,'settled',$6,$7,$8,$9)
		`, contractId, c.sourceId, c.sourceNetworkId, c.destinationId, c.destinationNetworkId, c.closeTime, c.snapshot, c.originIsSource, c.payerNetworkId))
	})
	return contractId
}

// The snapshot settlement writes for participants, as newContractUsageSnapshot
// splits them.
func stAmendedDbSnapshot(t testing.TB, bytes ByteCount, participants ...ContractParticipant) *contractUsageSnapshot {
	t.Helper()
	snapshot, err := newContractUsageSnapshot(bytes, participants)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func stAmendedDbShare(snapshot *contractUsageSnapshot, clientId server.Id) int64 {
	for _, provider := range snapshot.Providers {
		if provider.ClientId == clientId {
			return int64(provider.ByteCount)
		}
	}
	return -1
}

func stAmendedDbUsages(usages []*StProviderUsage) map[server.Id]int64 {
	out := map[server.Id]int64{}
	for _, usage := range usages {
		out[usage.ClientId] = usage.PayoutByteCount
	}
	return out
}

// The amended statement reads each live contract's usage origin network from
// the endpoint its usage_origin_is_source names and its payer from
// payer_network_id, in the usage statement's own snapshot:
//   - c1, a cross-network contract with an extender hop: every share is
//     provider usage;
//   - c2, a same-network contract its network funded: the receiving client's
//     share is intra-network Z, the extender keeps its share;
//   - c3, the same unfunded: the receiving client's share is left out;
//   - c4, a same-network contract a measurement network paid: left out;
//   - c5, a contract whose usage origin is its destination: the source is the
//     provider, and a hop on the destination's network is intra-network;
//   - c6, a zero-byte contract of a legacy direction, read as before;
//   - c7, closed at the window's end, belongs to the next epoch.
//
// Before E_x the same window reads exactly as the reader before the
// amendment reads it, with no pool.
func TestStExtenderDbAmendedUsageReadsTheLiveContractRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 11, 27, 18, 29, 0, 0, time.UTC)
		end := start.Add(time.Hour)
		at := start.Add(time.Minute)
		id := contractPayoutTestId
		netA, netP, netX, netN, netM, netS, netD := id(101), id(102), id(103), id(104), id(105), id(106), id(107)
		o1, p, x := ContractParticipant{ClientId: id(1), NetworkId: netA}, ContractParticipant{ClientId: id(2), NetworkId: netP}, ContractParticipant{ClientId: id(3), NetworkId: netX}
		oN, r := ContractParticipant{ClientId: id(4), NetworkId: netN}, ContractParticipant{ClientId: id(5), NetworkId: netN}
		oM, rM := ContractParticipant{ClientId: id(6), NetworkId: netM}, ContractParticipant{ClientId: id(7), NetworkId: netM}
		s, d, dHop := ContractParticipant{ClientId: id(8), NetworkId: netS}, ContractParticipant{ClientId: id(9), NetworkId: netD}, ContractParticipant{ClientId: id(10), NetworkId: netD}

		c1 := stAmendedDbSnapshot(t, 1_001, p, x)
		c2 := stAmendedDbSnapshot(t, 2_001, r, x)
		c3 := stAmendedDbSnapshot(t, 301, r, x)
		c4 := stAmendedDbSnapshot(t, 50, rM)
		c5 := stAmendedDbSnapshot(t, 99, s, dHop)
		c6 := stAmendedDbSnapshot(t, 0, p)
		c7 := stAmendedDbSnapshot(t, 70, r)
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at, sourceId: o1.ClientId, sourceNetworkId: netA, destinationId: p.ClientId, destinationNetworkId: netP, originIsSource: stAmendedDbBool(true), payerNetworkId: &netA, snapshot: c1})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at, sourceId: oN.ClientId, sourceNetworkId: netN, destinationId: r.ClientId, destinationNetworkId: netN, originIsSource: stAmendedDbBool(true), payerNetworkId: &netN, snapshot: c2})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: start, sourceId: oN.ClientId, sourceNetworkId: netN, destinationId: r.ClientId, destinationNetworkId: netN, originIsSource: stAmendedDbBool(true), snapshot: c3})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at, sourceId: oM.ClientId, sourceNetworkId: netM, destinationId: rM.ClientId, destinationNetworkId: netM, originIsSource: stAmendedDbBool(true), payerNetworkId: &netM, snapshot: c4})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: end.Add(-time.Microsecond), sourceId: s.ClientId, sourceNetworkId: netS, destinationId: d.ClientId, destinationNetworkId: netD, originIsSource: stAmendedDbBool(false), payerNetworkId: &netD, snapshot: c5})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: at, sourceId: o1.ClientId, sourceNetworkId: netA, destinationId: p.ClientId, destinationNetworkId: netP, snapshot: c6})
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: end, sourceId: oN.ClientId, sourceNetworkId: netN, destinationId: r.ClientId, destinationNetworkId: netN, originIsSource: stAmendedDbBool(true), payerNetworkId: &netN, snapshot: c7})

		amendment := StExtenderAmendment{FirstEpoch: 8}
		measurement := map[server.Id]bool{netM: true}
		usages, pool, err := GetStEpochAmendedUsageAtEpoch(ctx, 8, start, end, amendment, measurement)
		if err != nil {
			t.Fatal(err)
		}
		want := map[server.Id]int64{
			p.ClientId: stAmendedDbShare(c1, p.ClientId),
			x.ClientId: stAmendedDbShare(c1, x.ClientId) + stAmendedDbShare(c2, x.ClientId) + stAmendedDbShare(c3, x.ClientId),
			s.ClientId: stAmendedDbShare(c5, s.ClientId),
		}
		if got := stAmendedDbUsages(usages); !stAmendedDbEqual(got, want) {
			t.Fatalf("usages %v, want %v", got, want)
		}
		for i := 1; i < len(usages); i++ {
			if !usages[i-1].ClientId.Less(usages[i].ClientId) {
				t.Fatal("usages are not in client id order")
			}
		}
		wantPool := StEpochExtenderPool{
			IntraNetwork: ByteCount(stAmendedDbShare(c2, r.ClientId) + stAmendedDbShare(c5, dHop.ClientId)), Contracts: 2, Payers: 2,
			UnfundedBytes: ByteCount(stAmendedDbShare(c3, r.ClientId)), UnfundedContracts: 1,
			MeasurementBytes: 50, MeasurementContracts: 1,
		}
		if stExtenderPoolTotals(pool) != stExtenderPoolTotals(&wantPool) {
			t.Fatalf("pool %+v, want %+v", *pool, wantPool)
		}
		// every pooled share, funded or not, is some client's
		var pooledTotal ByteCount
		for _, bytes := range pool.PooledByClient {
			pooledTotal += bytes
		}
		if uint64(pooledTotal) != pool.PooledBytes() {
			t.Fatalf("clients' pooled shares sum to %d, not the pool's %d", pooledTotal, pool.PooledBytes())
		}
		var credited int64
		for _, bytes := range want {
			credited += bytes
		}
		if total := credited + int64(pool.Bytes()) + int64(pool.UnfundedBytes) + int64(pool.MeasurementBytes); total != 1_001+2_001+301+50+99 {
			t.Fatalf("the window's bytes are not each in one place: %d", total)
		}

		// before E_x: the reader before the amendment, exactly
		before, poolBefore, err := GetStEpochAmendedUsageAtEpoch(ctx, 7, start, end, amendment, measurement)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := GetStEpochProviderUsageAtEpoch(ctx, 7, start, end)
		if err != nil {
			t.Fatal(err)
		}
		if poolBefore != nil || !slices.EqualFunc(before, plain, func(a, b *StProviderUsage) bool { return *a == *b }) {
			t.Fatalf("before E_x: %+v and %+v, want the reader before the amendment's %+v", before, poolBefore, plain)
		}
		if got := stAmendedDbUsages(before); got[r.ClientId] != stAmendedDbShare(c2, r.ClientId)+stAmendedDbShare(c3, r.ClientId) || got[rM.ClientId] != 50 || got[dHop.ClientId] != stAmendedDbShare(c5, dHop.ClientId) {
			t.Fatalf("before E_x the same-network shares are not provider usage: %v", got)
		}
	})
}

func stAmendedDbEqual(a, b map[server.Id]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

// The amended statement refuses what it cannot classify: a contract with
// bytes and a legacy usage direction, and an archived contract with bytes,
// whose origin the archive does not keep. Before E_x both read as before.
func TestStExtenderDbAmendedUsageRefusesWhatItCannotClassify(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		id := contractPayoutTestId
		amendment := StExtenderAmendment{FirstEpoch: 8}
		provider := ContractParticipant{ClientId: id(2), NetworkId: id(102)}

		legacyStart := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
		addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: legacyStart, sourceId: id(1), sourceNetworkId: id(101), destinationId: provider.ClientId, destinationNetworkId: provider.NetworkId, snapshot: stAmendedDbSnapshot(t, 10, provider)})
		if _, _, err := GetStEpochAmendedUsageAtEpoch(ctx, 8, legacyStart, legacyStart.Add(time.Hour), amendment, nil); err == nil || !strings.Contains(err.Error(), "no usage origin") {
			t.Fatalf("a legacy direction with bytes: %v", err)
		}
		if usages, _, err := GetStEpochAmendedUsageAtEpoch(ctx, 7, legacyStart, legacyStart.Add(time.Hour), amendment, nil); err != nil || len(usages) != 1 {
			t.Fatalf("before E_x a legacy direction reads as before: %v %+v", err, usages)
		}

		archiveStart := legacyStart.Add(2 * time.Hour)
		archived := addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: archiveStart, sourceId: id(1), sourceNetworkId: id(101), destinationId: provider.ClientId, destinationNetworkId: provider.NetworkId, originIsSource: stAmendedDbBool(true), payerNetworkId: &provider.NetworkId, snapshot: stAmendedDbSnapshot(t, 10, provider)})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id = $1`, archived))
		}, server.TxReadCommitted, server.OptNoRetry())
		if _, _, err := GetStEpochAmendedUsageAtEpoch(ctx, 8, archiveStart, archiveStart.Add(time.Hour), amendment, nil); err == nil || !strings.Contains(err.Error(), "archived") {
			t.Fatalf("an archived contract with bytes: %v", err)
		}
		if usages, _, err := GetStEpochAmendedUsageAtEpoch(ctx, 7, archiveStart, archiveStart.Add(time.Hour), amendment, nil); err != nil || len(usages) != 1 || usages[0].PayoutByteCount != 10 {
			t.Fatalf("before E_x an archived contract reads as before: %v %+v", err, usages)
		}
	})
}

// A window past the census record bound reads the amended statement whole,
// under the whole-window plan: a bitmap read of transfer_contract, never its
// index order or a sequential scan, and the totals of the small read.
func TestStExtenderDbAmendedUsageWholeWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC)
		end := start.Add(time.Hour)
		id := contractPayoutTestId
		provider := ContractParticipant{ClientId: id(2), NetworkId: id(102)}
		count := payoutartifact.MaxClosedWorkRecords + 7
		ordinary := stAmendedDbSnapshot(t, 11, provider)
		payer := id(101)
		// between distinct endpoints, as TestStEpochUsageWholeWindowPlan's
		// contracts are, so no endpoint index is the planner's cheapest bitmap
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_contract (contract_id,source_id,source_network_id,destination_id,destination_network_id,
					transfer_byte_count,outcome,close_time,provider_usage,usage_origin_is_source,payer_network_id)
				SELECT md5('amended-'||n)::uuid,md5('amended-source-'||n)::uuid,$2,md5('amended-destination-'||n)::uuid,$3,
					1000000,'settled',$1::timestamp+n*interval '10 milliseconds',$4,true,$2
				FROM generate_series(0,$5::int-1) n
			`, start, payer, provider.NetworkId, ordinary, count))
		})
		network := id(104)
		receiver, extender := ContractParticipant{ClientId: id(5), NetworkId: network}, ContractParticipant{ClientId: id(3), NetworkId: id(103)}
		sameNetwork := stAmendedDbSnapshot(t, 1_001, receiver, extender)
		for range 3 {
			addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: start.Add(30 * time.Minute), sourceId: id(4), sourceNetworkId: network, destinationId: receiver.ClientId, destinationNetworkId: network, originIsSource: stAmendedDbBool(true), payerNetworkId: &network, snapshot: sameNetwork})
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `VACUUM ANALYZE transfer_contract`))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_provider_usage_archive`))
		})

		usages, pool, err := GetStEpochAmendedUsageAtEpoch(ctx, 8, start, end, StExtenderAmendment{FirstEpoch: 8}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := map[server.Id]int64{provider.ClientId: int64(count) * 11, extender.ClientId: 3 * stAmendedDbShare(sameNetwork, extender.ClientId)}
		if got := stAmendedDbUsages(usages); !stAmendedDbEqual(got, want) {
			t.Fatalf("usages %v, want %v", got, want)
		}
		if pool.IntraNetwork != ByteCount(3*stAmendedDbShare(sameNetwork, receiver.ClientId)) || pool.Contracts != 3 || pool.Payers != 1 {
			t.Fatalf("pool %+v", pool)
		}

		// the plan the read ran under, as TestStEpochUsageWholeWindowPlan reads
		// the usage statement's, under Main's costs
		var raw []byte
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			server.Raise(err)
			defer tx.Rollback(context.WithoutCancel(ctx))
			for _, setting := range stWholeWindowPlanMainCosts {
				server.RaisePgResult(tx.Exec(ctx, setting))
			}
			server.Raise(stEpochProviderUsageWholeWindowPlanInTx(ctx, tx))
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+stEpochAmendedUsageSql, start, end).Scan(&raw))
		})
		var plan any
		server.Raise(json.Unmarshal(raw, &plan))
		windowRows := 0.0
		windowBitmaps := []string{}
		for _, node := range stWholeWindowPlanNodes(plan, nil) {
			switch {
			case node.Relation == "contract_close_report_evidence" || node.Relation == "contract_close_report":
				t.Fatalf("the whole-window amended read touched a close report: %s", raw)
			case node.Relation == "transfer_contract" && node.Type != "Bitmap Heap Scan":
				t.Fatalf("the whole-window amended read fetched contracts by %s, not by bitmap: %s", node.Type, raw)
			case node.Type == "Materialize" || node.Type == "Gather" || node.Type == "Gather Merge" || (node.Type == "CTE Scan" && node.Cte != "missing_time"):
				t.Fatalf("the whole-window amended read has a %s node: %s", node.Type, raw)
			case node.TempBlocks > 0:
				t.Fatalf("the whole-window amended read spilled %v temp blocks in %s: %s", node.TempBlocks, node.Type, raw)
			case node.Type == "Hash" && node.HashBatches != 1:
				t.Fatalf("the archive hash ran in %v batches: %s", node.HashBatches, raw)
			case node.Type == "Sort" && node.Rows != 0:
				t.Fatalf("the whole-window amended read sorted %v rows: %s", node.Rows, raw)
			}
			if node.Type == "Bitmap Heap Scan" && node.Relation == "transfer_contract" && 0 < node.Rows {
				windowRows += node.Rows * node.Loops
				windowBitmaps = append(windowBitmaps, node.Bitmaps...)
			}
		}
		if windowRows != float64(count+3) || len(windowBitmaps) != 1 || windowBitmaps[0] != "transfer_contract_closed_usage" {
			t.Fatalf("the whole-window amended read lost its index or rows (%v, %v rows): %s", windowBitmaps, windowRows, raw)
		}
	})
}

// The census reads the records known in the epoch, created before its end
// and not revoked before its start, each with its addresses and their state,
// its tier and its canary channel, and builds rows the census rules accept.
func TestStExtenderDbCensusRecords(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 11, 20, 18, 28, 0, 0, time.UTC)
		end := start.Add(7 * 24 * time.Hour)
		id := contractPayoutTestId
		create := func(extender byte, created time.Time, revoked *time.Time, active bool, addresses ...*NetworkExtenderAddress) server.Id {
			extenderId := id(extender)
			for _, address := range addresses {
				address.ExtenderId = extenderId
				address.ActivateTime = created
				address.Carriers = []string{"tcp"}
				if address.Ip.Is4() {
					address.IpVersion = 4
				} else {
					address.IpVersion = 6
				}
			}
			Testing_CreateNetworkExtender(ctx, &NetworkExtender{
				ExtenderId: extenderId, NetworkId: id(extender + 100), ClientId: id(extender + 50), PublicKey: []byte{extender, 0xe7},
				CreateTime: created, TcpPort: 443, UdpPort: 443, DnsPort: 53, DnsTld: "ur.xyz.", CountryCode: "us", Active: active, RevokeTime: revoked,
			}, addresses)
			return extenderId
		}
		address := func(ip string, active bool) *NetworkExtenderAddress {
			return &NetworkExtenderAddress{Ip: netip.MustParseAddr(ip), Active: active}
		}
		before := start.Add(-24 * time.Hour)
		revokedInside, revokedBefore := start.Add(time.Hour), start.Add(-time.Hour)
		both := create(1, before, nil, true, address("198.51.100.9", true), address("2001:db8:1::9", true))
		inactiveAddress := create(2, before, nil, true, address("198.51.100.20", false))
		revoked := create(3, before, &revokedInside, false, address("198.51.100.30", false))
		create(4, before, &revokedBefore, false, address("198.51.100.40", false))
		create(5, end, nil, true, address("198.51.100.50", true))
		bare := create(6, before, nil, true)
		gated := create(7, start.Add(time.Hour), nil, true, address("2001:db8:7::7", true))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender SET directory_tier = 1, canary_channel = 'gated' WHERE extender_id = $1`, gated))
		})

		records := GetStExtenderCensusRecords(ctx, start, end)
		byId := map[server.Id]*StExtenderCensusRecord{}
		for _, record := range records {
			byId[record.ExtenderId] = record
		}
		if len(records) != 5 || byId[both] == nil || byId[inactiveAddress] == nil || byId[revoked] == nil || byId[bare] == nil || byId[gated] == nil {
			t.Fatalf("records %+v, want extenders 1, 2, 3, 6 and 7", records)
		}
		if r := byId[both]; len(r.Addresses) != 2 || !r.Active || r.Revoked || r.ClientId != id(51) || r.NetworkId != id(101) ||
			r.Addresses[0].Ip != netip.MustParseAddr("198.51.100.9") || !r.Addresses[0].Active || r.Addresses[1].Ip != netip.MustParseAddr("2001:db8:1::9") {
			t.Fatalf("extender 1: %+v", r)
		}
		if r := byId[inactiveAddress]; len(r.Addresses) != 1 || r.Addresses[0].Active {
			t.Fatalf("extender 2: %+v", r)
		}
		if r := byId[revoked]; !r.Revoked || r.Active {
			t.Fatalf("extender 3: %+v", r)
		}
		if r := byId[bare]; len(r.Addresses) != 0 {
			t.Fatalf("extender 6: %+v", r)
		}
		if r := byId[gated]; r.DirectoryTier != 1 || !r.Canary {
			t.Fatalf("extender 7: %+v", r)
		}
		rows, err := BuildStExtenderCensus(records, stCensusTestRules())
		if err != nil {
			t.Fatal(err)
		}
		flags := map[server.Id]ledger.CensusFlags{}
		for _, row := range rows {
			flags[server.Id(row.ExtenderID)] = row.Flags
		}
		if flags[both] != ledger.CensusActive || flags[inactiveAddress] != 0 || flags[revoked] != ledger.CensusRevoked || flags[bare] != 0 || flags[gated] != ledger.CensusActive|ledger.CensusGated|ledger.CensusCanary {
			t.Fatalf("flags %v", flags)
		}
	})
}

// An epoch's census is captured once: a second capture keeps and returns the
// first. Pools and epochs are apart, and what is stored decodes to the rows.
func TestStExtenderDbCensusStoredOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		rows, err := BuildStExtenderCensus([]*StExtenderCensusRecord{stCensusTestRecord(1, 1, "198.51.100.1"), stCensusTestRecord(2, 2, "-198.51.100.2")}, stCensusTestRules())
		if err != nil {
			t.Fatal(err)
		}
		if GetStLedgerV2ExtenderCensus(ctx, testStDeploymentKey, 1, 8) != nil {
			t.Fatal("a census before its capture")
		}
		first := time.Date(2026, 12, 4, 18, 31, 0, 0, time.UTC)
		captured := CaptureStLedgerV2ExtenderCensus(ctx, testStDeploymentKey, 1, 8, EncodeStExtenderCensus(rows), uint64(len(rows)), first)
		if captured == nil || captured.Rows != 2 || !captured.CaptureTime.Equal(first) {
			t.Fatalf("captured %+v", captured)
		}
		again := CaptureStLedgerV2ExtenderCensus(ctx, testStDeploymentKey, 1, 8, EncodeStExtenderCensus(rows[:1]), 1, first.Add(time.Hour))
		if again == nil || again.Rows != 2 || !again.CaptureTime.Equal(first) || string(again.Census) != string(captured.Census) {
			t.Fatalf("a second capture replaced the first: %+v", again)
		}
		decoded, err := DecodeStExtenderCensus(GetStLedgerV2ExtenderCensus(ctx, testStDeploymentKey, 1, 8).Census)
		if err != nil || !slices.Equal(decoded, rows) {
			t.Fatalf("stored census %+v %v", decoded, err)
		}
		if GetStLedgerV2ExtenderCensus(ctx, testStDeploymentKey, 2, 8) != nil || GetStLedgerV2ExtenderCensus(ctx, testStDeploymentKey, 1, 9) != nil ||
			GetStLedgerV2ExtenderCensus(ctx, StDeploymentKey("945:0x1000000000000000000000000000000000000001"), 1, 8) != nil {
			t.Fatal("a census read under another pool, epoch or deployment")
		}
		empty := CaptureStLedgerV2ExtenderCensus(ctx, testStDeploymentKey, 1, 9, EncodeStExtenderCensus(nil), 0, first)
		if decoded, err := DecodeStExtenderCensus(empty.Census); empty.Rows != 0 || err != nil || len(decoded) != 0 {
			t.Fatalf("an empty census %+v %v", empty, err)
		}
	})
}

// The amendments store keeps one file per deployment, replaced by the next.
func TestStExtenderDbAmendmentsStore(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		other := StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		if raw := GetStAmendments(ctx, testStDeploymentKey); raw != nil {
			t.Fatalf("a file before the first: %q", raw)
		}
		SaveStAmendments(ctx, testStDeploymentKey, []byte("sn: 25\n"))
		SaveStAmendments(ctx, other, []byte("sn: 350\n"))
		if raw := GetStAmendments(ctx, testStDeploymentKey); string(raw) != "sn: 25\n" {
			t.Fatalf("stored %q", raw)
		}
		SaveStAmendments(ctx, testStDeploymentKey, []byte("sn: 25\nsigned_amendments: []\n"))
		if raw := GetStAmendments(ctx, testStDeploymentKey); string(raw) != "sn: 25\nsigned_amendments: []\n" {
			t.Fatalf("replaced with %q", raw)
		}
		if raw := GetStAmendments(ctx, other); string(raw) != "sn: 350\n" {
			t.Fatalf("another deployment's file %q", raw)
		}
	})
}

// An epoch's extender component is recorded with its rows, once: Z as an exact
// uint64 up to 2^64 - 1, its census chunks, none for an epoch without one or
// an unrecoverable one. Recording the same again is a no-op; another
// component, or none, is refused; an operator rebuild replaces both.
func TestStExtenderDbEpochRowsExtenderComponent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		key := testStDeploymentKey
		chunks := []string{ledger.RefOf([]byte("p8")).String()}
		component := &StLedgerV2EpochExtenders{Bytes: math.MaxUint64, CensusChunks: []string{ledger.RefOf([]byte("c1")).String(), ledger.RefOf([]byte("c2")).String()},
			IntraPlatform: math.MaxUint64 - 1, IntraNetwork: 1, Contracts: math.MaxInt64, Payers: 3}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 8, chunks, component); err != nil {
			t.Fatal(err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 7, []string{}); err != nil {
			t.Fatal(err)
		}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 9, []string{}, &StLedgerV2EpochExtenders{Bytes: 0, CensusChunks: nil}); err != nil {
			t.Fatal(err)
		}
		if err := DeclareStLedgerV2EpochUnrecoverable(ctx, key, 1, 10, "operator"); err != nil {
			t.Fatal(err)
		}
		records := GetStLedgerV2EpochRows(ctx, key, 1, 7, 10)
		if len(records) != 4 || !records[8].Extenders.Equal(component) || !slices.Equal(records[8].ProviderChunks, chunks) ||
			records[7].Extenders != nil || records[10].Extenders != nil || !records[10].Null() {
			t.Fatalf("records %+v", records)
		}
		if got := records[9].Extenders; got == nil || got.Bytes != 0 || got.CensusChunks == nil || len(got.CensusChunks) != 0 {
			t.Fatalf("an empty census's component %+v", got)
		}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 8, chunks, component); err != nil {
			t.Fatalf("recording the same again: %v", err)
		}
		for name, other := range map[string]*StLedgerV2EpochExtenders{
			"other bytes":  {Bytes: math.MaxUint64 - 1, CensusChunks: component.CensusChunks, IntraPlatform: math.MaxUint64 - 2, IntraNetwork: 1, Contracts: math.MaxInt64, Payers: 3},
			"other census": {Bytes: math.MaxUint64, CensusChunks: component.CensusChunks[:1], IntraPlatform: math.MaxUint64 - 1, IntraNetwork: 1, Contracts: math.MaxInt64, Payers: 3},
			"other parts":  {Bytes: math.MaxUint64, CensusChunks: component.CensusChunks, IntraPlatform: math.MaxUint64 - 2, IntraNetwork: 2, Contracts: math.MaxInt64, Payers: 3},
			"other counts": {Bytes: math.MaxUint64, CensusChunks: component.CensusChunks, IntraPlatform: math.MaxUint64 - 1, IntraNetwork: 1, Contracts: math.MaxInt64, Payers: 4},
			"none":         nil,
		} {
			if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 8, chunks, other); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// a component whose parts are not its Z is refused before anything is
		// written
		for name, invalid := range map[string]*StLedgerV2EpochExtenders{
			"parts not Z":       {Bytes: 5, IntraNetwork: 4, Contracts: 1, Payers: 1},
			"Z without a payer": {Bytes: 5, IntraNetwork: 5, Contracts: 1},
		} {
			if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 11, []string{}, invalid); err == nil || errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if record := GetStLedgerV2EpochRows(ctx, key, 1, 11, 11)[11]; record != nil {
			t.Fatalf("an invalid component was recorded: %+v", record)
		}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 7, []string{}, component); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("an epoch recorded without a component gained one: %v", err)
		}
		if requested, err := RequestStLedgerV2EpochRebuild(ctx, key, 1, 8, "operator"); err != nil || !requested {
			t.Fatalf("rebuild request: %v %v", requested, err)
		}
		rebuilt := &StLedgerV2EpochExtenders{Bytes: 12, CensusChunks: []string{ledger.RefOf([]byte("c3")).String()}, IntraPlatform: 2, IntraNetwork: 10, Contracts: 2, Payers: 1}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 8, []string{}, rebuilt); err != nil {
			t.Fatal(err)
		}
		if record := GetStLedgerV2EpochRows(ctx, key, 1, 8, 8)[8]; !record.Extenders.Equal(rebuilt) || len(record.ProviderChunks) != 0 || record.RebuildRequestedBy != "" {
			t.Fatalf("rebuilt record %+v", record)
		}
	})
}
