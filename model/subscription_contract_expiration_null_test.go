// Legacy expiry ignores report recency while retaining the ordinary money owner.
package model

import (
	"bytes"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Each real partial-close orientation remains open until expiry selects it.
// Even a newly created NULL-deadline row is due; no quiet-time sleep is needed.
func TestContractExpirationNullClosesRecentPartialReports(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, sample := range []struct {
			name                  string
			source, destination   bool
			sourceCheckpoint      bool
			destinationCheckpoint bool
		}{
			{name: "no reports"},
			{name: "source final", source: true},
			{name: "destination final", destination: true},
			{name: "source checkpoint", source: true, sourceCheckpoint: true},
			{name: "destination checkpoint", destination: true, destinationCheckpoint: true},
			{name: "both checkpoints", source: true, destination: true, sourceCheckpoint: true, destinationCheckpoint: true},
			{name: "final and checkpoint", source: true, destination: true, destinationCheckpoint: true},
		} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
			server.Raise(err)
			if sample.source {
				server.Raise(CloseContract(ctx, id, f.sourceId, 0, sample.sourceCheckpoint))
			}
			if sample.destination {
				server.Raise(CloseContract(ctx, id, f.destinationId, 0, sample.destinationCheckpoint))
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=$1`, id))
			})
			if _, terminal := GetContractClose(ctx, id); terminal {
				t.Fatalf("%s setup already terminal", sample.name)
			}
			cutoff := server.NowUtc().Add(-12 * time.Minute)
			count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			if err != nil || count != 1 {
				t.Fatalf("%s immediate NULL expiry count=%d error=%v", sample.name, count, err)
			}
			if close, terminal := GetContractClose(ctx, id); !terminal || close.Outcome != ContractOutcomeSettled {
				t.Fatalf("%s NULL row stayed open", sample.name)
			}
			deadline, err := GetContractExpirationTime(ctx, id)
			if err != nil || deadline != nil {
				t.Fatal("expiry invented an explicit deadline", err)
			}
			if count, _, err = ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil); err != nil || count != 0 {
				t.Fatalf("%s terminal expiry replay count=%d error=%v", sample.name, count, err)
			}
		}
	})
}

// The locked recheck must agree with SQL selection. Retain the authenticated
// report rather than treating a fresh checkpoint as authority to postpone NULL.
func TestContractExpirationNullLockedProofKeepsRecentReport(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=$1`, id))
			state, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-12*time.Minute))
			if err != nil || state == nil || !state.usageUnverifiedRetained {
				t.Fatalf("fresh NULL checkpoint withdrew from locked expiry: state=%v error=%v", state != nil, err)
			}
		})
		_, proof := readContractExpiryTestSnapshot(t, ctx, id)
		if proof.Expiry == nil || len(proof.Expiry.Reports) != 1 ||
			proof.Expiry.Reports[ContractPartySource].ByteCount != 17 || !proof.Expiry.Reports[ContractPartySource].Checkpoint {
			t.Fatal("immediate expiry lost the original partial report")
		}
	})
}

// Both funding paths preserve exact delivered work. Existing legacy intents
// remain byte-identical across an expiry revisit until their payer owner commits.
func TestContractExpirationNullRetainsFinancialOwnersAndReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, redis := range []bool{false, true} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			var contract *TransferEscrow
			if redis {
				contract = createRedisAdmissionTest(ctx, f, 1000)
			} else {
				var posts []func() any
				contract, posts = createNetEscrowOrderingTestContract(ctx, f, 1000)
				server.RunPosts(ctx, posts...)
			}
			id := contract.ContractId
			server.Raise(CloseContract(ctx, id, f.sourceId, 300, true))
			server.Raise(CloseContract(ctx, id, f.destinationId, 300, true))
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=$1`, id))
			})
			cutoff := server.NowUtc().Add(-12 * time.Minute)
			count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			want := int64(0)
			if redis {
				want = 1
			}
			if err != nil || count != want {
				t.Fatalf("redis=%t NULL funding owner count=%d want=%d error=%v", redis, count, want, err)
			}
			proof, decoded := readContractExpiryTestSnapshot(t, ctx, id)
			if decoded.Expiry == nil || decoded.ByteCount != 300 || len(decoded.Expiry.Reports) != 2 {
				t.Fatal("NULL expiry lost the bilateral delivered proof")
			}
			if redis {
				result, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
				if err != nil || result.Failed != 0 {
					t.Fatal("NULL Redis debit failed", err)
				}
			} else {
				requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 1000)
				readIntent := func() []byte {
					var raw []byte
					server.Db(ctx, func(conn server.PgConn) {
						server.Raise(conn.QueryRow(ctx, `SELECT to_jsonb(i) FROM legacy_settlement_intent i WHERE contract_id=$1`, id).Scan(&raw))
					})
					return raw
				}
				before := readIntent()
				count, _, err = ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
				if err != nil || count != 0 || !bytes.Equal(before, readIntent()) {
					t.Fatal("expiry rewrote the existing payer intent", err)
				}
				complete, busy, _, err := flushLegacySettlement(ctx, id)
				if err != nil || !complete || busy {
					t.Fatalf("NULL legacy continuation complete=%t busy=%t error=%v", complete, busy, err)
				}
			}
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 700, 0)
			requireLegacyProviderDurability(t, ctx, f, id, 300)
			count, _, err = ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			after, _ := readContractExpiryTestSnapshot(t, ctx, id)
			if err != nil || count != 0 || !bytes.Equal(proof, after) {
				t.Fatal("NULL expiry replay changed the original proof", err)
			}
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 700, 0)
			requireLegacyProviderDurability(t, ctx, f, id, 300)
		}
	})
}

// A retained old sweep must admit a NULL row created inside the quiet window.
// The explicit future neighbor is scanned under the same bounded policy and
// keeps its deadline, reports and accounting unchanged.
func TestContractExpirationNullFairPageAdmitsPresentTail(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		create := func() server.Id {
			id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
			server.Raise(err)
			server.Raise(CloseContract(ctx, id, f.sourceId, 0, true))
			return id
		}
		legacy, neighbor := create(), create()
		now := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`, legacy, now.Add(-2*time.Second)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, neighbor, now.Add(-time.Second)))
		})
		before := readRedisExpiryRepairTestState(ctx, neighbor)
		cursor := &ContractExpirySweepCursor{
			Historical:  &ContractExpiryCursor{ScanBefore: now.Add(-72 * time.Hour)},
			Recent:      &ContractExpiryCursor{ScanBefore: now.Add(-24 * time.Hour)},
			RecentAfter: now.Add(-72 * time.Hour),
		}
		count, next, err := ForceCloseOpenContractIdsFairPage(ctx, now.Add(-12*time.Minute), 2, 1, 1, 0, cursor)
		if err != nil || count != 1 || next == nil || next.Fresh == nil || next.Fresh.Open == nil ||
			!next.Fresh.ScanBefore.After(now.Add(-time.Second)) {
			t.Fatalf("NULL tail remained behind quiet scan horizon: count=%d next=%v error=%v", count, next != nil, err)
		}
		if _, terminal := GetContractClose(ctx, legacy); !terminal {
			t.Fatal("fair expiry did not retire the immediate NULL row")
		}
		if !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, neighbor)) {
			t.Fatal("immediate legacy admission changed the explicit future neighbor")
		}
	})
}

// All retained sweep lanes call the same real bounded selector. Keep old
// epochs and positions while NULL rows with recent reports become eligible.
func TestContractExpirationNullAcrossRetainedSweepLanes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, lane := range []string{"historical", "recent", "fresh", "catchup"} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			now := server.NowUtc().Truncate(time.Microsecond)
			epoch := now.Add(-72 * time.Hour)
			created := now.Add(-time.Minute)
			switch lane {
			case "historical":
				created = epoch.Add(-time.Hour)
			case "recent":
				created = epoch.Add(time.Hour)
			case "catchup":
				created = now.Add(-6 * time.Hour)
			}
			ids := []server.Id{}
			for _, disputed := range []bool{false, true} {
				id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
				server.Raise(err)
				server.Raise(CloseContract(ctx, id, f.sourceId, 0, true))
				server.Raise(CloseContract(ctx, id, f.destinationId, 0, true))
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL,dispute=$3 WHERE contract_id=$1`, id, created, disputed))
				})
				ids = append(ids, id)
			}
			cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}, RecentAfter: epoch}
			page := func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
				return ForceCloseOpenContractIdsPage(ctx, now.Add(-12*time.Minute), 32, 1, 1, 0, position)
			}
			var count int64
			var err error
			switch lane {
			case "historical", "recent":
				cursor.HistoricalNext = lane == "historical"
				count, _, err = forceCloseContractExpirySweepPage(now, now, cursor, page)
			case "fresh":
				count, _, err = forceCloseContractExpiryFreshPage(now, now, cursor, page)
			case "catchup":
				cursor.Recent = &ContractExpiryCursor{ScanBefore: now.Add(-24 * time.Hour)}
				cursor.CatchupTurn = 2
				var handled bool
				count, _, handled, err = forceCloseContractExpiryCatchupPage(now, cursor, page)
				if !handled {
					t.Fatal("catch-up fixture did not enter its retained lane")
				}
			}
			if err != nil || count != 2 {
				t.Fatalf("%s NULL open/dispute lane count=%d error=%v", lane, count, err)
			}
			for _, id := range ids {
				if _, terminal := GetContractClose(ctx, id); !terminal {
					t.Fatalf("%s retained a NULL contract", lane)
				}
			}
		}
	})
}
