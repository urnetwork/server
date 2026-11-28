package model

// The SN25 validator data grant against the database: one active grant per
// validator hotkey, the next one in the active grant's last days or after it
// ends, and one grant among concurrent mints. Every time is injected, so no
// test waits for a window to pass.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// One balance of a network's validator grants, as the database holds it.
type testingValidatorDataGrantBalance struct {
	startTime             time.Time
	endTime               time.Time
	startBalanceByteCount ByteCount
	balanceByteCount      ByteCount
	netRevenue            NanoCents
	subsidyNetRevenue     NanoCents
	pro                   bool
	paid                  bool
	purchaseToken         *string
}

// The network's validator grant balances, oldest first.
func testingValidatorDataGrantBalances(t testing.TB, ctx context.Context, networkId server.Id) []testingValidatorDataGrantBalance {
	t.Helper()
	balances := []testingValidatorDataGrantBalance{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					start_time,
					end_time,
					start_balance_byte_count,
					balance_byte_count,
					net_revenue_nano_cents,
					subsidy_net_revenue_nano_cents,
					pro,
					paid,
					purchase_token
				FROM transfer_balance
				WHERE network_id = $1 AND grant_kind = $2
				ORDER BY start_time
			`,
			networkId,
			GrantKindSn25Validator,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balance testingValidatorDataGrantBalance
				server.Raise(result.Scan(
					&balance.startTime,
					&balance.endTime,
					&balance.startBalanceByteCount,
					&balance.balanceByteCount,
					&balance.netRevenue,
					&balance.subsidyNetRevenue,
					&balance.pro,
					&balance.paid,
					&balance.purchaseToken,
				))
				balances = append(balances, balance)
			}
		})
	})
	return balances
}

// The window the hotkey's receipt holds; ok false without a receipt.
func testingValidatorDataGrantReceipt(t testing.TB, ctx context.Context, hotkey [32]byte) (startTime time.Time, endTime time.Time, ok bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT start_time, end_time
				FROM transfer_balance_grant_run
				WHERE run_id = $1 AND grant_kind = $2
			`,
			ValidatorDataGrantId(hotkey),
			GrantKindSn25Validator,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&startTime, &endTime))
				ok = true
			}
		})
	})
	return
}

// A synthetic hotkey.
func testingValidatorHotkey(seed byte) [32]byte {
	var hotkey [32]byte
	for i := range hotkey {
		hotkey[i] = seed + byte(i)
	}
	return hotkey
}

// The first mint grants the network 350 GiB for 30 days: data only, at no
// cost. Every later mint while that grant is active, up to the last instant
// before its last 2 days, grants nothing, and the receipt keeps its window.
func TestGrantValidatorDataOnceWhileActive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "validator-grant", server.NewId())
		hotkey := testingValidatorHotkey(0x51)
		settings := DefaultValidatorDataGrantSettings()
		now := server.NowUtc()

		grant, err := GrantValidatorData(ctx, networkId, hotkey, &settings, now)
		connect.AssertEqual(t, err, nil)
		if grant == nil {
			t.Fatal("the first admitted mint granted nothing")
		}
		connect.AssertEqual(t, grant.NetworkId, networkId)
		connect.AssertEqual(t, grant.BalanceByteCount, 350*Gib)
		connect.AssertEqual(t, grant.StartTime, now)
		connect.AssertEqual(t, grant.EndTime, now.Add(30*24*time.Hour))

		balances := testingValidatorDataGrantBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		balance := balances[0]
		connect.AssertEqual(t, balance.startTime, now)
		connect.AssertEqual(t, balance.endTime, now.Add(30*24*time.Hour))
		connect.AssertEqual(t, balance.startBalanceByteCount, 350*Gib)
		connect.AssertEqual(t, balance.balanceByteCount, 350*Gib)
		connect.AssertEqual(t, balance.netRevenue, NanoCents(0))
		connect.AssertEqual(t, balance.subsidyNetRevenue, NanoCents(0))
		connect.AssertEqual(t, balance.pro, false)
		connect.AssertEqual(t, balance.paid, false)
		connect.AssertEqual(t, balance.purchaseToken, (*string)(nil))
		receiptStartTime, receiptEndTime, ok := testingValidatorDataGrantReceipt(t, ctx, hotkey)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, receiptStartTime, now)
		connect.AssertEqual(t, receiptEndTime, now.Add(30*24*time.Hour))

		for _, later := range []time.Duration{
			time.Second,
			time.Hour,
			27 * 24 * time.Hour,
			28*24*time.Hour - time.Microsecond,
		} {
			grant, err := GrantValidatorData(ctx, networkId, hotkey, &settings, now.Add(later))
			connect.AssertEqual(t, err, nil)
			if grant != nil {
				t.Fatalf("a mint %s after the grant granted again: %+v", later, grant)
			}
		}
		connect.AssertEqual(t, len(testingValidatorDataGrantBalances(t, ctx, networkId)), 1)
		_, receiptEndTime, _ = testingValidatorDataGrantReceipt(t, ctx, hotkey)
		connect.AssertEqual(t, receiptEndTime, now.Add(30*24*time.Hour))
		connect.AssertEqual(t, HasActiveTransferBalance(ctx, networkId, 350*Gib), true)
	})
}

// The first mint in the active grant's last 2 days grants the next 30 days
// from that mint, once; the old grant keeps its own end. A mint after a grant
// ended grants the next one.
func TestGrantValidatorDataRenewsInTheLastDaysAndAfterTheEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "validator-grant-renew", server.NewId())
		hotkey := testingValidatorHotkey(0x52)
		settings := DefaultValidatorDataGrantSettings()
		now := server.NowUtc()

		first, err := GrantValidatorData(ctx, networkId, hotkey, &settings, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, first, (*ValidatorDataGrant)(nil))

		// exactly the renewal point: the grant ends 2 days from this mint
		renewTime := now.Add(28 * 24 * time.Hour)
		second, err := GrantValidatorData(ctx, networkId, hotkey, &settings, renewTime)
		connect.AssertEqual(t, err, nil)
		if second == nil {
			t.Fatal("the first mint in the grant's last 2 days granted nothing")
		}
		connect.AssertEqual(t, second.StartTime, renewTime)
		connect.AssertEqual(t, second.EndTime, renewTime.Add(30*24*time.Hour))
		repeated, err := GrantValidatorData(ctx, networkId, hotkey, &settings, renewTime.Add(time.Minute))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, repeated, (*ValidatorDataGrant)(nil))

		balances := testingValidatorDataGrantBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 2)
		connect.AssertEqual(t, balances[0].endTime, now.Add(30*24*time.Hour))
		connect.AssertEqual(t, balances[1].startTime, renewTime)
		connect.AssertEqual(t, balances[1].endTime, renewTime.Add(30*24*time.Hour))
		connect.AssertEqual(t, balances[1].startBalanceByteCount, 350*Gib)

		// the second grant ends unrenewed; a mint 3 days later grants again
		afterEndTime := renewTime.Add(33 * 24 * time.Hour)
		third, err := GrantValidatorData(ctx, networkId, hotkey, &settings, afterEndTime)
		connect.AssertEqual(t, err, nil)
		if third == nil {
			t.Fatal("a mint after the grant ended granted nothing")
		}
		connect.AssertEqual(t, third.StartTime, afterEndTime)
		connect.AssertEqual(t, third.EndTime, afterEndTime.Add(30*24*time.Hour))
		connect.AssertEqual(t, len(testingValidatorDataGrantBalances(t, ctx, networkId)), 3)
		receiptStartTime, receiptEndTime, _ := testingValidatorDataGrantReceipt(t, ctx, hotkey)
		connect.AssertEqual(t, receiptStartTime, afterEndTime)
		connect.AssertEqual(t, receiptEndTime, afterEndTime.Add(30*24*time.Hour))
	})
}

// The grant is per hotkey: two validators that share a network get one grant
// each, and a hotkey that signs in from another network while its grant is
// active gets nothing there.
func TestGrantValidatorDataIsPerHotkey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "validator-grant-shared", server.NewId())
		Testing_CreateNetwork(ctx, otherNetworkId, "validator-grant-other", server.NewId())
		hotkey := testingValidatorHotkey(0x53)
		otherHotkey := testingValidatorHotkey(0x54)
		settings := DefaultValidatorDataGrantSettings()
		now := server.NowUtc()

		for _, validatorHotkey := range [][32]byte{hotkey, otherHotkey} {
			grant, err := GrantValidatorData(ctx, networkId, validatorHotkey, &settings, now)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, grant, (*ValidatorDataGrant)(nil))
		}
		connect.AssertEqual(t, len(testingValidatorDataGrantBalances(t, ctx, networkId)), 2)

		grant, err := GrantValidatorData(ctx, otherNetworkId, hotkey, &settings, now.Add(time.Hour))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant, (*ValidatorDataGrant)(nil))
		connect.AssertEqual(t, len(testingValidatorDataGrantBalances(t, ctx, otherNetworkId)), 0)
	})
}

// Two mints that reach the claim together grant once. Both hold their
// network lock at the hook before either claims, so each claim must decide
// against the other's: a check-then-insert would grant twice here.
func TestGrantValidatorDataConcurrentMintsGrantOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "validator-grant-concurrent", server.NewId())
		hotkey := testingValidatorHotkey(0x55)
		settings := DefaultValidatorDataGrantSettings()
		now := server.NowUtc()

		const mintCount = 2
		var arrivals sync.WaitGroup
		arrivals.Add(mintCount)
		release := make(chan struct{})
		var arrivalLock sync.Mutex
		arrivalCount := 0
		defer Testing_SetValidatorDataGrantBeforeClaim(func() {
			// a retried transaction passes the hook again, after the release
			func() {
				arrivalLock.Lock()
				defer arrivalLock.Unlock()
				arrivalCount += 1
				if arrivalCount <= mintCount {
					arrivals.Done()
				}
			}()
			<-release
		})()

		grants := make(chan *ValidatorDataGrant, mintCount)
		errs := make(chan error, mintCount)
		for i := 0; i < mintCount; i += 1 {
			go func() {
				grant, err := GrantValidatorData(ctx, networkId, hotkey, &settings, now.Add(time.Duration(i)*time.Millisecond))
				grants <- grant
				errs <- err
			}()
		}
		arrived := make(chan struct{})
		go func() {
			arrivals.Wait()
			close(arrived)
		}()
		select {
		case <-arrived:
		case <-time.After(time.Minute):
			t.Fatal("the mints did not both reach the claim")
		}
		close(release)

		created := 0
		for i := 0; i < mintCount; i += 1 {
			connect.AssertEqual(t, <-errs, nil)
			if <-grants != nil {
				created += 1
			}
		}
		connect.AssertEqual(t, created, 1)
		connect.AssertEqual(t, len(testingValidatorDataGrantBalances(t, ctx, networkId)), 1)
	})
}

// Settings that grant nothing are refused before any write, and a network
// that does not exist is granted nothing and leaves the receipt unclaimed, so
// the validator's next mint on its real network still grants.
func TestGrantValidatorDataRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "validator-grant-refusals", server.NewId())
		hotkey := testingValidatorHotkey(0x56)
		now := server.NowUtc()

		for _, disabled := range []ValidatorDataGrantSettings{
			{BalanceByteCount: 0, Duration: 30 * 24 * time.Hour, RenewBefore: 2 * 24 * time.Hour},
			{BalanceByteCount: 350 * Gib, Duration: 0, RenewBefore: 0},
			{BalanceByteCount: 350 * Gib, Duration: 24 * time.Hour, RenewBefore: 2 * 24 * time.Hour},
		} {
			grant, err := GrantValidatorData(ctx, networkId, hotkey, &disabled, now)
			connect.AssertEqual(t, grant, (*ValidatorDataGrant)(nil))
			connect.AssertEqual(t, errors.Is(err, errValidatorDataGrantDisabled), true)
		}
		_, _, ok := testingValidatorDataGrantReceipt(t, ctx, hotkey)
		connect.AssertEqual(t, ok, false)

		settings := DefaultValidatorDataGrantSettings()
		grant, err := GrantValidatorData(ctx, server.NewId(), hotkey, &settings, now)
		connect.AssertEqual(t, grant, (*ValidatorDataGrant)(nil))
		connect.AssertEqual(t, errors.Is(err, ErrPaymentNetworkNotFound), true)
		_, _, ok = testingValidatorDataGrantReceipt(t, ctx, hotkey)
		connect.AssertEqual(t, ok, false)

		grant, err = GrantValidatorData(ctx, networkId, hotkey, &settings, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, grant, (*ValidatorDataGrant)(nil))
		connect.AssertEqual(t, len(testingValidatorDataGrantBalances(t, ctx, networkId)), 1)
	})
}

// The grant's size and duration, and its renewal point, follow the settings.
func TestGrantValidatorDataFollowsSettings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "validator-grant-settings", server.NewId())
		hotkey := testingValidatorHotkey(0x57)
		settings := ValidatorDataGrantSettings{
			BalanceByteCount: 7 * Gib,
			Duration:         7 * 24 * time.Hour,
			RenewBefore:      24 * time.Hour,
			Timeout:          5 * time.Second,
		}
		now := server.NowUtc()

		grant, err := GrantValidatorData(ctx, networkId, hotkey, &settings, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, grant, (*ValidatorDataGrant)(nil))
		skipped, err := GrantValidatorData(ctx, networkId, hotkey, &settings, now.Add(6*24*time.Hour-time.Second))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, skipped, (*ValidatorDataGrant)(nil))
		renewed, err := GrantValidatorData(ctx, networkId, hotkey, &settings, now.Add(6*24*time.Hour))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, renewed, (*ValidatorDataGrant)(nil))

		balances := testingValidatorDataGrantBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 2)
		for _, balance := range balances {
			connect.AssertEqual(t, balance.startBalanceByteCount, 7*Gib)
			connect.AssertEqual(t, balance.endTime.Sub(balance.startTime), 7*24*time.Hour)
		}
	})
}
