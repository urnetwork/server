// Absolute deadlines retain the original close proof and normal financial owner.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Equality belongs to expiry. Missing deadlines use the creation clock plus
// 60 minutes; explicit deadlines and early quiet closes retain their policy.
func TestContractExpirationExactDeadline(t *testing.T) {
	now := time.UnixMilli(2_000_000_000_000).UTC()
	cutoff := now.Add(-5 * time.Minute)
	future, past := now.Add(time.Millisecond), now.Add(-time.Millisecond)
	for _, test := range []struct {
		name       string
		expiration *time.Time
		created    time.Time
		lastReport time.Time
		want       bool
	}{
		{name: "before deadline", expiration: &future, lastReport: now},
		{name: "exact deadline", expiration: &now, lastReport: now, want: true},
		{name: "after deadline", expiration: &past, lastReport: now, want: true},
		{name: "legacy fresh", created: now, lastReport: now},
		{name: "legacy before deadline", created: now.Add(-60*time.Minute + time.Nanosecond), lastReport: now},
		{name: "legacy exact deadline", created: now.Add(-60 * time.Minute), lastReport: now, want: true},
		{name: "legacy after deadline", created: now.Add(-61 * time.Minute), lastReport: now, want: true},
		{name: "legacy quiet", created: now.Add(-10 * time.Minute), lastReport: cutoff, want: true},
		{name: "explicit deadline overrides legacy age", expiration: &future, created: now.Add(-2 * time.Hour), lastReport: now},
		{name: "early quiet close", expiration: &future, lastReport: cutoff, want: true},
	} {
		if got := contractExpirationDue(test.expiration, test.created, test.lastReport, cutoff, now); got != test.want {
			t.Errorf("%s: due=%t want=%t", test.name, got, test.want)
		}
	}
}

// All three insert owners sample the database clock once, after admission,
// and the response value is exactly what was persisted at millisecond precision.
func TestContractExpirationDefaultAcrossCreationPaths(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		legacy, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		reserved := createRedisAdmissionTest(ctx, f, 100)
		companion, err := CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId,
			f.sourceNetworkId, f.sourceId, 100, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		noEscrow, noEscrowExpiration, err := CreateContractNoEscrowWithExpiration(ctx, f.sourceNetworkId, f.sourceId,
			f.destinationNetworkId, f.destinationId, 100, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			id       server.Id
			returned time.Time
		}{
			{id: legacy.ContractId, returned: legacy.ExpirationTime},
			{id: reserved.ContractId, returned: reserved.ExpirationTime},
			{id: companion.ContractId, returned: companion.ExpirationTime},
			{id: noEscrow, returned: noEscrowExpiration},
		} {
			expires, err := GetContractExpirationTime(ctx, test.id)
			if err != nil || expires == nil {
				t.Fatalf("created contract lost its deadline: %v", err)
			}
			var created time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT create_time FROM transfer_contract WHERE contract_id=$1`, test.id).Scan(&created))
			})
			if DefaultContractExpiration != 60*time.Minute || !expires.Equal(created.Truncate(time.Millisecond).Add(60*time.Minute)) ||
				!expires.Equal(time.UnixMilli(expires.UnixMilli())) {
				t.Fatalf("deadline=%s created=%s, want 60 minutes at wire precision", expires, created)
			}
			if test.returned.IsZero() || !test.returned.Equal(*expires) {
				t.Fatal("returned reservation deadline differs from its committed row")
			}
		}
	})
}

// A fresh checkpoint used to postpone retirement indefinitely. Force the
// persisted absolute boundary directly, then run the real bounded selector,
// locked proof owner, financial continuation and replay; no sleep owns the test.
func TestContractExpirationCheckpointCannotExtendLegacyEscrow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 1000)
		server.RunPosts(ctx, posts...)
		before, err := GetContractExpirationTime(ctx, contract.ContractId)
		if err != nil || before == nil {
			t.Fatal("missing original deadline", err)
		}
		server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 300, true))
		server.Raise(CloseContract(ctx, contract.ContractId, f.destinationId, 300, true))
		after, err := GetContractExpirationTime(ctx, contract.ContractId)
		if err != nil || after == nil || !after.Equal(*before) {
			t.Fatal("checkpoint moved absolute deadline", err)
		}
		cutoff := server.NowUtc().Add(-5 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, contract.ContractId, cutoff))
		})
		count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || count != 1 {
			t.Fatalf("legacy deadline did not finish its financial closure: count=%d err=%v", count, err)
		}
		requireLegacySettlementTestState(t, ctx, f, contract.ContractId, false, true, 700, 0)
		requireDeadlineProviderDurability(t, ctx, f.destinationNetworkId, contract.ContractId, 300)
		_, proof := readContractExpiryTestSnapshot(t, ctx, contract.ContractId)
		if proof.Expiry == nil || proof.ByteCount != 300 || len(proof.Expiry.Reports) != 2 ||
			!proof.Expiry.Reports[ContractPartySource].Checkpoint || !proof.Expiry.Reports[ContractPartyDestination].Checkpoint {
			t.Fatal("absolute expiry lost the original delivered-work checkpoints")
		}
		complete, busy, _, err := flushLegacySettlement(ctx, contract.ContractId)
		if err != nil || complete || !busy {
			t.Fatalf("retired intent was not classified as gone: complete=%t busy_or_gone=%t err=%v", complete, busy, err)
		}
		requireLegacySettlementTestState(t, ctx, f, contract.ContractId, false, true, 700, 0)
		requireLegacyProviderDurability(t, ctx, f, contract.ContractId, 300)
		if _, _, _, err := flushLegacySettlement(ctx, contract.ContractId); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, contract.ContractId, f.sourceId, 300, true); !errors.Is(err, errContractAlreadySettled) {
			t.Fatalf("late checkpoint revived expired accounting: %v", err)
		}
		requireLegacySettlementTestState(t, ctx, f, contract.ContractId, false, true, 700, 0)
		requireLegacyProviderDurability(t, ctx, f, contract.ContractId, 300)
	})
}

// Redis admission owns the other financial path. A reportless hard expiry
// releases its full reservation while a fresh neighbor keeps its own capacity.
func TestContractExpirationReleasesRedisReservationOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		expired := createRedisAdmissionTest(ctx, f, 600)
		neighbor := createRedisAdmissionTest(ctx, f, 100)
		cutoff := server.NowUtc().Add(-5 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, expired.ContractId, cutoff))
		})
		count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || count != 1 {
			t.Fatalf("Redis expiration count=%d err=%v", count, err)
		}
		_, proof := readContractExpiryTestSnapshot(t, ctx, expired.ContractId)
		if proof.Expiry == nil || len(proof.Expiry.Reports) != 0 || proof.ByteCount != 0 {
			t.Fatal("reportless expiration invented provider work")
		}
		flushed, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
		if err != nil || flushed.Failed != 0 {
			t.Fatal("expired Redis debit failed", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 100 {
			t.Fatalf("neighbor reservation=%d want=100", got)
		}
		if available := GetActiveTransferBalanceByteCount(ctx, f.sourceNetworkId); available != 900 {
			t.Fatalf("released capacity=%d want=900", available)
		}
		if _, terminal := GetContractClose(ctx, neighbor.ContractId); terminal {
			t.Fatal("expiration closed the live neighbor")
		}
		count, _, err = ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || count != 0 {
			t.Fatal("expiry replay repeated financial retirement", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 100 {
			t.Fatal("replay released another contract's capacity")
		}
	})
}

// A new companion cannot extend an expired open origin. An eligible newer
// origin is selected normally, and the companion receives its own lifetime.
func TestContractExpirationCompanionSelectsLiveRenewal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		expired := createRedisAdmissionTest(ctx, f, 100)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, expired.ContractId, server.NowUtc().Add(-time.Minute)))
		})
		create := func() (*TransferEscrow, error) {
			return CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, f.sourceId, 100, time.Minute)
		}
		if _, err := create(); !errors.Is(err, ErrMissingCompanionOrigin) {
			t.Fatalf("expired origin admitted companion: %v", err)
		}
		server.Raise(CloseContract(ctx, expired.ContractId, f.sourceId, 0, false))
		server.Raise(CloseContract(ctx, expired.ContractId, f.destinationId, 0, false))
		if _, err := create(); !errors.Is(err, ErrMissingCompanionOrigin) {
			t.Fatalf("settlement revived expired origin through close linger: %v", err)
		}
		live := createRedisAdmissionTest(ctx, f, 100)
		companion, err := create()
		if err != nil || companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != live.ContractId {
			t.Fatalf("companion did not use renewed origin: %v", err)
		}
		if !companion.ExpirationTime.After(live.ExpirationTime.Add(-time.Millisecond)) {
			t.Fatal("new companion inherited the expired deadline")
		}
		// A reverse companion remains a valid origin for the returning control
		// carrier. Both directions retain independent deadlines and accounting.
		AddBasicTransferBalance(ctx, f.destinationNetworkId, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour))
		reverse, err := CreateCompanionTransferEscrow(ctx, f.sourceNetworkId, f.sourceId,
			f.destinationNetworkId, f.destinationId, 100, time.Minute)
		if err != nil || reverse == nil || reverse.CompanionContractId == nil || *reverse.CompanionContractId != companion.ContractId {
			t.Fatalf("reverse companion renewal failed: %v", err)
		}
		server.Raise(CloseContract(ctx, companion.ContractId, f.destinationId, 0, true))
		server.Raise(CloseContract(ctx, companion.ContractId, f.destinationId, 0, false))
		server.Raise(CloseContract(ctx, companion.ContractId, f.sourceId, 0, false))
		originalDeadline, err := GetContractExpirationTime(ctx, live.ContractId)
		if err != nil || originalDeadline == nil || !originalDeadline.Equal(live.ExpirationTime) {
			t.Fatal("companion reporting changed its original's deadline", err)
		}
		if _, terminal := GetContractClose(ctx, live.ContractId); terminal {
			t.Fatal("companion close retired the live original")
		}
		if _, terminal := GetContractClose(ctx, expired.ContractId); !terminal {
			t.Fatal("companion close revived the expired original")
		}
	})
}

// Real client-row locks hold the creator after its origin selection and
// funding. The deadline becomes due before that lock is released; neither
// Redis admission nor the legacy funding owner may publish the stale child.
func TestContractExpirationCompanionRechecksAfterClientWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 30*time.Second)
		defer cancel()
		for _, test := range []struct {
			redisAdmission bool
			nullDeadline   bool
		}{
			{redisAdmission: true},
			{redisAdmission: false},
			{redisAdmission: true, nullDeadline: true},
			{redisAdmission: false, nullDeadline: true},
		} {
			func() {
				redisAdmission := test.redisAdmission
				f := newNetEscrowOrderingTestFixture(t, ctx)
				origin := createRedisAdmissionTest(ctx, f, 100)
				if test.nullDeadline {
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=$1`, origin.ContractId))
					})
				}
				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				held, err := conn.Begin(ctx)
				server.Raise(err)
				defer held.Rollback(context.Background())
				server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM network_client WHERE client_id=$1 FOR UPDATE`, f.destinationId))
				pid := contractLifecycleTestBackendPid(t, ctx, held)
				done := make(chan error, 1)
				go func() {
					var callErr error
					panicErr := server.HandleError(func() {
						var child *TransferEscrow
						if redisAdmission {
							child, callErr = CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId,
								f.sourceNetworkId, f.sourceId, 100, time.Minute)
						} else {
							child, callErr = createCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId,
								f.sourceNetworkId, f.sourceId, 100, time.Minute)
						}
						if child != nil && callErr == nil {
							callErr = errors.New("expired origin published a new child")
						}
					})
					if panicErr != nil {
						if err, ok := panicErr.(error); ok {
							callErr = err
						} else {
							callErr = errors.New("unexpected admission panic")
						}
					}
					done <- callErr
				}()
				requireContractLifecycleBlockedBy(t, ctx, held, pid)
				if redisAdmission && Testing_NetEscrowByteCount(ctx, f.balanceId) != 200 {
					t.Fatal("client barrier preceded the child's actual Redis reservation")
				}
				if test.nullDeadline {
					server.RaisePgResult(held.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, origin.ContractId, server.NowUtc().Add(-61*time.Minute)))
				} else {
					server.RaisePgResult(held.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, origin.ContractId, server.NowUtc().Add(-time.Minute)))
				}
				server.Raise(held.Commit(ctx))
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal("creator failed to leave the explicit lock barrier", ctx.Err())
				}
				if !errors.Is(err, ErrMissingCompanionOrigin) {
					t.Fatalf("redis=%t null=%t stale origin result: %v", redisAdmission, test.nullDeadline, err)
				}
				if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 100 {
					t.Fatalf("refused child's reservation was retained: %d", got)
				}
				requireRedisRefusalOnlyContractWithByteCount(t, ctx, f, origin.ContractId, 100)
			}()
		}
	})
}

// A contract can be disputed before its deadline. The separate bounded
// disputed scan must reach the same absolute retirement and usage proof owner.
func TestContractExpirationDisputedCheckpointIgnoresQuietExtension(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, id, f.sourceId, 0, true))
		server.Raise(CloseContract(ctx, id, f.destinationId, 0, true))
		SetContractDispute(ctx, id, true)
		cutoff := server.NowUtc().Add(-5 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, cutoff))
		})
		count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || count != 1 {
			t.Fatalf("absolute disputed expiration count=%d err=%v", count, err)
		}
		if _, terminal := GetContractClose(ctx, id); !terminal {
			t.Fatal("expired disputed contract remains open")
		}
	})
}
