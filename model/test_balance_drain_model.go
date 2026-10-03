package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Test balance drain lets an acceptance-test account make its OWN available
// balance read as zero for a bounded window, so the apps' insufficient balance
// state can be reproduced against a live deployment.
//
// Gate: the vault resource `testBalanceDrainVaultResource` lists the network ids
// of the acceptance-test accounts. Absent, empty or malformed means disabled:
// no network may drain, and the escrow hot path never queries the drain table.
// The list is re-read every `testBalanceDrainAllowlistRefresh`, so removing an
// entry stops its drain from applying without a restart.
//
// Mechanism: a drain never touches `transfer_balance`, escrow, payment or
// balance history. It inserts one `test_balance_drain` row (the audit record:
// who, when, until when, and the available balance hidden at that moment).
// While a row is in window, `createTransferEscrowInTx` refuses positive-byte
// contracts paid by that network with the normal "Insufficient balance" error
// (the client sees ContractError_InsufficientBalance) and the balance read
// reports zero. Restore ends the window early; an unrestored drain ends by
// itself at `end_time`. Rows are never deleted. Because the balance rows are
// untouched, restore returns the network exactly to the state it would have
// had without the drain (contracts that were already open settle normally).
//
// Scope: the network id always comes from the caller's session. There is no
// parameter that names another network.

const testBalanceDrainVaultResource = "acceptance-balance-drain.yml"

const testBalanceDrainAllowlistRefresh = 60 * time.Second

const (
	DefaultTestBalanceDrainDuration = 15 * time.Minute
	MinTestBalanceDrainDuration     = 1 * time.Minute
	MaxTestBalanceDrainDuration     = 30 * time.Minute
)

var ErrTestBalanceDrainNotAllowed = errors.New("Balance drain is not enabled for this network.")

type testBalanceDrainConfig struct {
	NetworkIds []string `yaml:"network_ids"`
}

// parseTestBalanceDrainAllowlist reads the vault allowlist. Any malformed id
// rejects the whole file, so a typo can never widen the gate.
func parseTestBalanceDrainAllowlist(data []byte) (map[server.Id]bool, error) {
	var config testBalanceDrainConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	allowlist := map[server.Id]bool{}
	for _, value := range config.NetworkIds {
		networkId, err := server.ParseId(value)
		if err != nil {
			return nil, fmt.Errorf("invalid network id in %s: %w", testBalanceDrainVaultResource, err)
		}
		allowlist[networkId] = true
	}
	return allowlist, nil
}

type testBalanceDrainAllowlistSnapshot struct {
	loadTime  time.Time
	allowlist map[server.Id]bool
}

// The escrow hot path reads an immutable snapshot without locking. One caller
// refreshes it from the vault when it is stale.
type testBalanceDrainAllowlistState struct {
	refreshLock sync.Mutex
	snapshot    atomic.Pointer[testBalanceDrainAllowlistSnapshot]
	override    atomic.Pointer[map[server.Id]bool]
}

var testBalanceDrainAllowlist = &testBalanceDrainAllowlistState{}

func (self *testBalanceDrainAllowlistState) allowed(networkId server.Id) bool {
	if override := self.override.Load(); override != nil {
		return (*override)[networkId]
	}
	now := time.Now()
	snapshot := self.snapshot.Load()
	if snapshot == nil || testBalanceDrainAllowlistRefresh <= now.Sub(snapshot.loadTime) {
		if snapshot == nil {
			self.refreshLock.Lock()
			snapshot = self.refresh(now)
			self.refreshLock.Unlock()
		} else if self.refreshLock.TryLock() {
			// others keep using the stale snapshot during a refresh
			snapshot = self.refresh(now)
			self.refreshLock.Unlock()
		}
	}
	return snapshot.allowlist[networkId]
}

// refresh must be called with refreshLock held.
func (self *testBalanceDrainAllowlistState) refresh(now time.Time) *testBalanceDrainAllowlistSnapshot {
	if snapshot := self.snapshot.Load(); snapshot != nil && now.Sub(snapshot.loadTime) < testBalanceDrainAllowlistRefresh {
		return snapshot
	}
	snapshot := &testBalanceDrainAllowlistSnapshot{
		loadTime:  now,
		allowlist: loadTestBalanceDrainAllowlist(),
	}
	self.snapshot.Store(snapshot)
	return snapshot
}

// loadTestBalanceDrainAllowlist fails closed: a missing or unreadable resource
// is an empty allowlist.
func loadTestBalanceDrainAllowlist() map[server.Id]bool {
	resource, err := server.Vault.SimpleResource(testBalanceDrainVaultResource)
	if err != nil {
		return map[server.Id]bool{}
	}
	data, err := resource.BytesE()
	if err != nil {
		glog.Infof("[balance_drain]allowlist unreadable; disabled: %s\n", err)
		return map[server.Id]bool{}
	}
	allowlist, err := parseTestBalanceDrainAllowlist(data)
	if err != nil {
		glog.Infof("[balance_drain]allowlist invalid; disabled: %s\n", err)
		return map[server.Id]bool{}
	}
	return allowlist
}

// Testing_SetTestBalanceDrainAllowlist pins the allowlist for tests. nil
// returns to the vault resource.
func Testing_SetTestBalanceDrainAllowlist(networkIds []server.Id) {
	if networkIds == nil {
		testBalanceDrainAllowlist.override.Store(nil)
		return
	}
	override := map[server.Id]bool{}
	for _, networkId := range networkIds {
		override[networkId] = true
	}
	testBalanceDrainAllowlist.override.Store(&override)
}

func TestBalanceDrainAllowed(networkId server.Id) bool {
	return testBalanceDrainAllowlist.allowed(networkId)
}

// testBalanceDrainEndTime bounds every drain so a failed cleanup expires.
func testBalanceDrainEndTime(now time.Time, requested time.Duration) time.Time {
	duration := requested
	if duration <= 0 {
		duration = DefaultTestBalanceDrainDuration
	}
	duration = min(max(duration, MinTestBalanceDrainDuration), MaxTestBalanceDrainDuration)
	return now.Add(duration)
}

// applyTestBalanceDrain zeroes the available bytes of a drained network's
// balances for display. The rows themselves are never modified.
func applyTestBalanceDrain(transferBalances []*TransferBalance, drained bool) {
	if !drained {
		return
	}
	for _, transferBalance := range transferBalances {
		transferBalance.BalanceByteCount = 0
	}
}

// testBalanceDrainEscrowError is the escrow refusal. Zero-byte contracts keep
// working as they do for any empty balance.
func testBalanceDrainEscrowError(drained bool, contractTransferByteCount ByteCount) error {
	if drained && 0 < contractTransferByteCount {
		return fmt.Errorf("Insufficient balance (%d).", 0)
	}
	return nil
}

const testBalanceDrainActiveSql = `
    SELECT EXISTS (
        SELECT 1 FROM test_balance_drain
        WHERE
            network_id = $1 AND
            restore_time IS NULL AND
            $2 < end_time
    )
`

// testBalanceDrainActive checks the allowlist in memory first, so a network
// that is not allowlisted never adds a query to the escrow hot path.
func testBalanceDrainActive(ctx context.Context, query server.PgCanQuery, networkId server.Id, now time.Time) bool {
	if !TestBalanceDrainAllowed(networkId) {
		return false
	}
	active := false
	result, err := query.Query(ctx, testBalanceDrainActiveSql, networkId, now)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&active))
		}
	})
	return active
}

func IsTestBalanceDrainActive(ctx context.Context, networkId server.Id) (active bool) {
	if !TestBalanceDrainAllowed(networkId) {
		return false
	}
	server.Db(ctx, func(conn server.PgConn) {
		active = testBalanceDrainActive(ctx, conn, networkId, server.NowUtc())
	})
	return
}

type TestBalanceDrain struct {
	DrainId   server.Id `json:"drain_id"`
	NetworkId server.Id `json:"network_id"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	// the available balance hidden by the drain, for the audit record
	DrainedBalanceByteCount ByteCount `json:"drained_balance_byte_count"`
}

// DrainTestBalance starts (or returns the already active) drain of the
// network's own balance. It refuses any network not in the allowlist.
func DrainTestBalance(ctx context.Context, networkId server.Id, duration time.Duration) (*TestBalanceDrain, error) {
	if !TestBalanceDrainAllowed(networkId) {
		return nil, ErrTestBalanceDrainNotAllowed
	}
	now := server.NowUtc()
	// read before the drain row exists so the audit holds the real amount
	drainedBalanceByteCount := ByteCount(0)
	for _, transferBalance := range getActiveTransferBalancesWithoutDrain(ctx, networkId) {
		drainedBalanceByteCount += transferBalance.BalanceByteCount
	}

	var drain *TestBalanceDrain
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
                SELECT drain_id, start_time, end_time, drained_balance_byte_count
                FROM test_balance_drain
                WHERE
                    network_id = $1 AND
                    restore_time IS NULL AND
                    $2 < end_time
                ORDER BY end_time DESC
                LIMIT 1
            `,
			networkId,
			now,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				drain = &TestBalanceDrain{NetworkId: networkId}
				server.Raise(result.Scan(&drain.DrainId, &drain.StartTime, &drain.EndTime, &drain.DrainedBalanceByteCount))
			}
		})
		if drain != nil {
			return
		}
		drain = &TestBalanceDrain{
			DrainId:                 server.NewId(),
			NetworkId:               networkId,
			StartTime:               now,
			EndTime:                 testBalanceDrainEndTime(now, duration),
			DrainedBalanceByteCount: drainedBalanceByteCount,
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO test_balance_drain (
                    drain_id,
                    network_id,
                    start_time,
                    end_time,
                    drained_balance_byte_count
                )
                VALUES ($1, $2, $3, $4, $5)
            `,
			drain.DrainId,
			drain.NetworkId,
			drain.StartTime,
			drain.EndTime,
			drain.DrainedBalanceByteCount,
		))
	})
	glog.Infof("[balance_drain]drain network=%s drain=%s end=%s\n", networkId, drain.DrainId, drain.EndTime)
	return drain, nil
}

// RestoreTestBalance ends every in-window drain of the network. It is not
// gated by the allowlist so cleanup always works; it only changes the
// caller's own drain rows, which exist only if the network was allowlisted.
func RestoreTestBalance(ctx context.Context, networkId server.Id) (restoredCount int64) {
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE test_balance_drain
                SET restore_time = $2
                WHERE
                    network_id = $1 AND
                    restore_time IS NULL AND
                    $2 < end_time
            `,
			networkId,
			now,
		))
		restoredCount = tag.RowsAffected()
	})
	if 0 < restoredCount {
		glog.Infof("[balance_drain]restore network=%s count=%d\n", networkId, restoredCount)
	}
	return
}
