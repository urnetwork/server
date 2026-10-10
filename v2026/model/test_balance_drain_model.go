package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// Test balance drain lets an acceptance-test account make its OWN available
// balance read as zero for a bounded window, so the apps' insufficient balance
// state can be reproduced against a live deployment.
//
// Gate: a network may drain when either
//   - its id is listed in the vault resource `testBalanceDrainVaultResource`
//     (`network_ids`), or
//   - one of its live sign-in emails (the admin user's
//     `network_user_auth_password` or `network_user_auth_sso` records) is on an
//     exact test bypass domain from tests.yml `email_verification.bypass_domains`
//     (`testsVaultResourceName`). Acceptance runs create a fresh account on that
//     domain per run, drain it, restore it, and delete it.
//
// The email is read from the database for the caller's network, never from the
// request or the JWT. Absent, empty or malformed configuration means disabled:
// a malformed allowlist or bypass list rejects that whole list, and with neither
// configured no network may drain and the escrow hot path never queries the
// drain table. Both lists are re-read every `testBalanceDrainGateRefresh`, so
// removing an entry stops its drains from applying without a restart.
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

const testBalanceDrainGateRefresh = 60 * time.Second

// A domain-gated drain applies on other hosts within this interval. One query
// per interval lists the active drains; other payers add no query.
const testBalanceDrainActiveRefresh = 5 * time.Second

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

// testBalanceDrainGate is one immutable reading of both gate lists.
type testBalanceDrainGate struct {
	allowlistNetworkIds map[server.Id]bool
	// exact normalized email domains; empty when not configured or malformed
	bypassDomains map[string]bool
}

func (self *testBalanceDrainGate) enabled() bool {
	return 0 < len(self.allowlistNetworkIds) || 0 < len(self.bypassDomains)
}

// mayDrain reports, without any query, whether the network could be allowed.
func (self *testBalanceDrainGate) mayDrain(networkId server.Id) bool {
	return self.allowlistNetworkIds[networkId] || 0 < len(self.bypassDomains)
}

type testBalanceDrainGateSnapshot struct {
	loadTime time.Time
	gate     *testBalanceDrainGate
}

// The escrow hot path reads an immutable snapshot without locking. One caller
// refreshes it from the vault when it is stale.
type testBalanceDrainGateState struct {
	refreshLock sync.Mutex
	snapshot    atomic.Pointer[testBalanceDrainGateSnapshot]
	override    atomic.Pointer[testBalanceDrainGate]
}

var testBalanceDrainGates = &testBalanceDrainGateState{}

func (self *testBalanceDrainGateState) current() *testBalanceDrainGate {
	if override := self.override.Load(); override != nil {
		return override
	}
	now := time.Now()
	snapshot := self.snapshot.Load()
	if snapshot == nil || testBalanceDrainGateRefresh <= now.Sub(snapshot.loadTime) {
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
	return snapshot.gate
}

// refresh must be called with refreshLock held.
func (self *testBalanceDrainGateState) refresh(now time.Time) *testBalanceDrainGateSnapshot {
	if snapshot := self.snapshot.Load(); snapshot != nil && now.Sub(snapshot.loadTime) < testBalanceDrainGateRefresh {
		return snapshot
	}
	snapshot := &testBalanceDrainGateSnapshot{
		loadTime: now,
		gate: &testBalanceDrainGate{
			allowlistNetworkIds: loadTestBalanceDrainAllowlist(),
			bypassDomains:       loadTestBalanceDrainBypassDomains(),
		},
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

// loadTestBalanceDrainBypassDomains reads the same exact domain list as the
// auth verification bypass and fails closed the same way: a missing tests.yml,
// an unexpected version, or any malformed domain is an empty list.
func loadTestBalanceDrainBypassDomains() map[string]bool {
	config, err := loadTestsVaultConfig()
	if err != nil {
		return map[string]bool{}
	}
	if config.Version != 1 {
		glog.Infof("[balance_drain]tests.yml version %d; bypass domains disabled\n", config.Version)
		return map[string]bool{}
	}
	bypassDomains, valid := testEmailBypassDomains(config)
	if !valid {
		glog.Infof("[balance_drain]tests.yml bypass domains invalid; disabled\n")
		return map[string]bool{}
	}
	return bypassDomains
}

// Testing_SetTestBalanceDrainAllowlist pins the allowlist for tests, with no
// bypass domains. nil returns to the vault resources.
func Testing_SetTestBalanceDrainAllowlist(networkIds []server.Id) {
	if networkIds == nil {
		testBalanceDrainGates.override.Store(nil)
		return
	}
	Testing_SetTestBalanceDrainGate(networkIds, []string{})
}

// Testing_SetTestBalanceDrainGate pins both gate lists for tests. Domains are
// normalized like tests.yml; one malformed domain disables the domain gate.
func Testing_SetTestBalanceDrainGate(networkIds []server.Id, bypassDomains []string) {
	gate := &testBalanceDrainGate{allowlistNetworkIds: map[server.Id]bool{}}
	for _, networkId := range networkIds {
		gate.allowlistNetworkIds[networkId] = true
	}
	config := &testsVaultConfig{}
	config.EmailVerification.BypassDomains = bypassDomains
	if normalDomains, valid := testEmailBypassDomains(config); valid {
		gate.bypassDomains = normalDomains
	} else {
		gate.bypassDomains = map[string]bool{}
	}
	testBalanceDrainGates.override.Store(gate)
	testBalanceDrainActiveNetworks.snapshot.Store(nil)
}

// testBalanceDrainSignInEmails returns the network's live sign-in identities
// (password-auth user_auth and SSO emails of the network's admin user).
// Replaced in tests.
var testBalanceDrainSignInEmails = func(ctx context.Context, query server.PgCanQuery, networkId server.Id) []string {
	userAuths := []string{}
	result, err := query.Query(
		ctx,
		`
            SELECT network_user_auth_password.user_auth
            FROM network
            INNER JOIN network_user_auth_password ON
                network_user_auth_password.user_id = network.admin_user_id
            WHERE network.network_id = $1

            UNION ALL

            SELECT network_user_auth_sso.user_auth
            FROM network
            INNER JOIN network_user_auth_sso ON
                network_user_auth_sso.user_id = network.admin_user_id
            WHERE network.network_id = $1
        `,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userAuth string
			server.Raise(result.Scan(&userAuth))
			userAuths = append(userAuths, userAuth)
		}
	})
	return userAuths
}

// testBalanceDrainDb runs fn with a database connection. Replaced in tests.
var testBalanceDrainDb = func(ctx context.Context, fn func(query server.PgCanQuery)) {
	server.Db(ctx, func(conn server.PgConn) {
		fn(conn)
	})
}

// Testing_SetTestBalanceDrainSignInEmails replaces the sign-in email lookup
// and its database access for tests. The returned func restores both.
func Testing_SetTestBalanceDrainSignInEmails(networkIdEmails func(networkId server.Id) []string) func() {
	previousSignInEmails := testBalanceDrainSignInEmails
	previousDb := testBalanceDrainDb
	if networkIdEmails != nil {
		testBalanceDrainSignInEmails = func(ctx context.Context, query server.PgCanQuery, networkId server.Id) []string {
			return networkIdEmails(networkId)
		}
		testBalanceDrainDb = func(ctx context.Context, fn func(query server.PgCanQuery)) {
			fn(nil)
		}
	}
	return func() {
		testBalanceDrainSignInEmails = previousSignInEmails
		testBalanceDrainDb = previousDb
	}
}

// testBalanceDrainEligible applies the gate to one network. Only the bypass
// domain path queries, and only the network's own sign-in records.
func testBalanceDrainEligible(ctx context.Context, query server.PgCanQuery, gate *testBalanceDrainGate, networkId server.Id) bool {
	if gate.allowlistNetworkIds[networkId] {
		return true
	}
	if len(gate.bypassDomains) == 0 {
		return false
	}
	for _, userAuth := range testBalanceDrainSignInEmails(ctx, query, networkId) {
		normalUserAuth, authType := NormalUserAuthV1(&userAuth)
		if normalUserAuth != nil && authType == UserAuthTypeEmail && testEmailOnBypassDomain(*normalUserAuth, gate.bypassDomains) {
			return true
		}
	}
	return false
}

// TestBalanceDrainAllowed reports whether the network may start a drain.
func TestBalanceDrainAllowed(ctx context.Context, networkId server.Id) (allowed bool) {
	gate := testBalanceDrainGates.current()
	if !gate.mayDrain(networkId) {
		return false
	}
	if gate.allowlistNetworkIds[networkId] {
		return true
	}
	testBalanceDrainDb(ctx, func(query server.PgCanQuery) {
		allowed = testBalanceDrainEligible(ctx, query, gate, networkId)
	})
	return
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

// testBalanceDrainActiveNetworkIds lists every network with a drain in window.
// The partial index on (network_id, end_time) WHERE restore_time IS NULL keeps
// this to the few open drain rows. Replaced in tests.
var testBalanceDrainActiveNetworkIds = func(ctx context.Context, query server.PgCanQuery, now time.Time) []server.Id {
	networkIds := []server.Id{}
	result, err := query.Query(
		ctx,
		`
            SELECT DISTINCT network_id
            FROM test_balance_drain
            WHERE
                restore_time IS NULL AND
                $1 < end_time
        `,
		now,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			server.Raise(result.Scan(&networkId))
			networkIds = append(networkIds, networkId)
		}
	})
	return networkIds
}

type testBalanceDrainActiveSnapshot struct {
	loadTime   time.Time
	networkIds map[server.Id]bool
}

// testBalanceDrainActiveState caches the domain-gated drained networks, so a
// payer that is neither allowlisted nor drained never adds a query per escrow.
type testBalanceDrainActiveState struct {
	refreshLock sync.Mutex
	snapshot    atomic.Pointer[testBalanceDrainActiveSnapshot]
}

var testBalanceDrainActiveNetworks = &testBalanceDrainActiveState{}

func (self *testBalanceDrainActiveState) stale(snapshot *testBalanceDrainActiveSnapshot, now time.Time) bool {
	return snapshot == nil || testBalanceDrainActiveRefresh <= now.Sub(snapshot.loadTime) || now.Before(snapshot.loadTime)
}

func (self *testBalanceDrainActiveState) active(ctx context.Context, query server.PgCanQuery, gate *testBalanceDrainGate, networkId server.Id, now time.Time) bool {
	snapshot := self.snapshot.Load()
	if self.stale(snapshot, now) {
		if snapshot == nil {
			self.refreshLock.Lock()
			snapshot = self.refresh(ctx, query, gate, now)
			self.refreshLock.Unlock()
		} else if self.refreshLock.TryLock() {
			// others keep using the stale snapshot during a refresh
			snapshot = self.refresh(ctx, query, gate, now)
			self.refreshLock.Unlock()
		}
	}
	return snapshot.networkIds[networkId]
}

// refresh must be called with refreshLock held. A drained network counts only
// while it still passes the gate, so removing its domain stops the drain.
func (self *testBalanceDrainActiveState) refresh(ctx context.Context, query server.PgCanQuery, gate *testBalanceDrainGate, now time.Time) *testBalanceDrainActiveSnapshot {
	if snapshot := self.snapshot.Load(); !self.stale(snapshot, now) {
		return snapshot
	}
	snapshot := &testBalanceDrainActiveSnapshot{
		loadTime:   now,
		networkIds: map[server.Id]bool{},
	}
	for _, networkId := range testBalanceDrainActiveNetworkIds(ctx, query, now) {
		if testBalanceDrainEligible(ctx, query, gate, networkId) {
			snapshot.networkIds[networkId] = true
		}
	}
	self.snapshot.Store(snapshot)
	return snapshot
}

// invalidate makes this host see its own drain or restore immediately.
func (self *testBalanceDrainActiveState) invalidate() {
	self.snapshot.Store(nil)
}

// testBalanceDrainActive checks the gate in memory first. With neither list
// configured, or for a payer outside the allowlist when no bypass domain is
// configured, it never queries. An allowlisted network is checked directly;
// domain-gated drains come from the shared active-drain snapshot.
func testBalanceDrainActive(ctx context.Context, query server.PgCanQuery, networkId server.Id, now time.Time) bool {
	gate := testBalanceDrainGates.current()
	if !gate.mayDrain(networkId) {
		return false
	}
	if !gate.allowlistNetworkIds[networkId] {
		return testBalanceDrainActiveNetworks.active(ctx, query, gate, networkId, now)
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
	if !testBalanceDrainGates.current().mayDrain(networkId) {
		return false
	}
	testBalanceDrainDb(ctx, func(query server.PgCanQuery) {
		active = testBalanceDrainActive(ctx, query, networkId, server.NowUtc())
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
// network's own balance. It refuses any network the gate does not allow.
func DrainTestBalance(ctx context.Context, networkId server.Id, duration time.Duration) (*TestBalanceDrain, error) {
	if !TestBalanceDrainAllowed(ctx, networkId) {
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
	testBalanceDrainActiveNetworks.invalidate()
	glog.Infof("[balance_drain]drain network=%s drain=%s end=%s\n", networkId, drain.DrainId, drain.EndTime)
	return drain, nil
}

// RestoreTestBalance ends every in-window drain of the network. It is not
// gated so cleanup always works; it only changes the caller's own drain rows,
// which exist only if the network passed the gate when it drained.
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
	testBalanceDrainActiveNetworks.invalidate()
	if 0 < restoredCount {
		glog.Infof("[balance_drain]restore network=%s count=%d\n", networkId, restoredCount)
	}
	return
}
