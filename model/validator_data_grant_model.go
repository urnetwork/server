// The data grant of an admitted SN25 validator (owner 2026-11-25). A validator
// mints its rate class at POST /sn/validator-class only while the reserved
// upload admission admits its activation: a VPK class consent for its bearer,
// and its hotkey's on-chain SN25 validator permit. Each such mint asks for the
// grant: the minting client's network gets BalanceByteCount, valid for Duration
// from the mint. It is data only and costs nothing: no Pro and no revenue, like
// a balance code created at no cost.
//
// Each validator hotkey holds one current grant. The hotkey's receipt in
// transfer_balance_grant_run, keyed by ValidatorDataGrantId and the kind
// sn25_validator, holds the window of its current grant. A mint claims the
// receipt only when that window ends within RenewBefore of the mint, or has
// ended, and writes the grant in the same transaction. So a repeated mint while
// a grant is active adds nothing, and the first mint in the grant's last
// RenewBefore grants the next period, so the grant renews while the validator
// stays admitted. Concurrent mints serialize on the receipt's primary key: the
// first claims it, and each other one waits for that commit, finds the new
// window and grants nothing. The receipt outlives the balance, which retention
// deletes after it ends. The table and its key exist (migration 811), so the
// grant needs no migration.
//
// The previous grant keeps what is left of it until its own end, at most
// RenewBefore after the next grant starts. Both are active then: the old one
// ends first, so it is spent first, and the balance summary shows both
// (grantGrace).
//
// The grant goes to the network of the client that mints. A hotkey that signs
// in from another network gets its next grant there, once its receipt's window
// allows one.
package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
)

// pro.yml validator_grant: an absent key keeps its default. The absent pro.yml
// (ProConfig{}) holds the zero value, which grants nothing.
type ValidatorDataGrantSettings struct {
	// The data of each grant (pro.yml validator_grant.data). 350 GiB, sized
	// from our own validator's measurement spend (owner 2026-11-24).
	// Non-positive: no grant.
	BalanceByteCount ByteCount
	// How long each grant is valid, from the mint that creates it (pro.yml
	// validator_grant.duration). 30 days. Non-positive: no grant.
	Duration time.Duration
	// The first admitted mint once the active grant ends within this creates
	// the next grant. 2 days, so a validator that mints every few minutes
	// never runs out between grants. Negative, or not shorter than Duration,
	// would grant at every mint: no grant.
	RenewBefore time.Duration
	// Bounds one grant's database work, so a slow database delays a mint's
	// answer at most this long; the next mint asks again. 5 seconds, half the
	// mint's own bound. Non-positive: only the mint's bound.
	Timeout time.Duration
}

// The owner's sizing, as a new instance on every call.
func DefaultValidatorDataGrantSettings() ValidatorDataGrantSettings {
	return ValidatorDataGrantSettings{
		BalanceByteCount: 350 * Gib,
		Duration:         30 * 24 * time.Hour,
		RenewBefore:      2 * 24 * time.Hour,
		Timeout:          5 * time.Second,
	}
}

// The defaults with pro.yml's keys applied. A malformed value panics, as every
// other pro.yml value does.
func parseValidatorDataGrant(y proValidatorGrantYaml) ValidatorDataGrantSettings {
	settings := DefaultValidatorDataGrantSettings()
	if y.Data != "" {
		settings.BalanceByteCount = mustParseByteCount(y.Data)
	}
	if y.Duration != "" {
		settings.Duration = mustParseDuration(y.Duration)
	}
	return settings
}

// Whether an admitted mint grants: an amount and a duration, and a renewal
// window that leaves the grant active between mints.
func (self *ValidatorDataGrantSettings) Enabled() bool {
	return 0 < self.BalanceByteCount && 0 < self.Duration && 0 <= self.RenewBefore && self.RenewBefore < self.Duration
}

// Settings that grant nothing (Enabled).
var errValidatorDataGrantDisabled = errors.New("validator data grant is disabled")

// Derives a hotkey's receipt id. A protocol constant: changing it gives every
// hotkey a new receipt, and so a second active grant.
const validatorDataGrantIdNamespace = "urnetwork:sn25-validator-data-grant:v1"

// The hotkey's receipt id: sha256 of the namespace and the hotkey, cut to an id.
// Every api instance derives the same id, so concurrent mints contend for one
// row. It names the hotkey without holding it.
func ValidatorDataGrantId(hotkey [32]byte) server.Id {
	sum := sha256.Sum256(append([]byte(validatorDataGrantIdNamespace+"/"), hotkey[:]...))
	return server.RequireIdFromBytes(sum[:16])
}

// A grant that a mint created.
type ValidatorDataGrant struct {
	NetworkId        server.Id
	BalanceByteCount ByteCount
	StartTime        time.Time
	EndTime          time.Time
}

// Runs in each grant's transaction after its network lock and before it
// claims the receipt, so a test can hold concurrent grants there. Unset
// outside tests.
var validatorDataGrantBeforeClaim atomic.Pointer[func()]

// Sets the hook, returning a function that restores the previous one.
func Testing_SetValidatorDataGrantBeforeClaim(hook func()) func() {
	previous := validatorDataGrantBeforeClaim.Swap(&hook)
	return func() {
		validatorDataGrantBeforeClaim.Store(previous)
	}
}

// Grants the validator's network its data grant at now, unless the hotkey's
// grant is active past the renewal window: then nil. Refused for settings that
// grant nothing and for a network that does not exist
// (ErrPaymentNetworkNotFound). Raises on a database failure, which grants
// nothing.
func GrantValidatorData(
	ctx context.Context,
	networkId server.Id,
	hotkey [32]byte,
	settings *ValidatorDataGrantSettings,
	now time.Time,
) (grant *ValidatorDataGrant, returnErr error) {
	if !settings.Enabled() {
		return nil, errValidatorDataGrantDisabled
	}
	// the database's time domain (server.NowUtc)
	now = now.UTC().Truncate(time.Microsecond)
	grantId := ValidatorDataGrantId(hotkey)
	renewTime := now.Add(settings.RenewBefore)
	// Nearly every mint finds the grant active. Reading the receipt first
	// leaves those mints no lock to take; a grant is still decided by the claim.
	active := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT 1
				FROM transfer_balance_grant_run
				WHERE
					run_id = $1 AND
					grant_kind = $2 AND
					$3 < end_time
			`,
			grantId,
			GrantKindSn25Validator,
			renewTime,
		)
		server.WithPgResult(result, err, func() {
			active = result.Next()
		})
	})
	if active {
		return nil, nil
	}

	startTime := now
	endTime := now.Add(settings.Duration)
	// ReadCommitted: a mint that waits on another's claim reads the window that
	// claim committed, rather than failing to serialize and retrying
	server.Tx(ctx, func(tx server.PgTx) {
		grant, returnErr = nil, nil
		// the deletion ordering of every data writer: a network deleted first
		// is granted nothing, and its receipt is left unclaimed
		if err := LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
			returnErr = err
			return
		}
		if hook := validatorDataGrantBeforeClaim.Load(); hook != nil && *hook != nil {
			(*hook)()
		}

		// The receipt is claimed when it is new, or when its window ends by
		// the renewal point. A claim waiting on a concurrent claim's row lock
		// sees that commit's window, which ends past the renewal point, so it
		// claims nothing. A receipt that is not claimed stays locked only
		// until this transaction commits, at once.
		claimed := false
		result, err := tx.Query(
			ctx,
			`
				INSERT INTO transfer_balance_grant_run (
					run_id,
					grant_kind,
					start_time,
					end_time
				)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (run_id, grant_kind) DO UPDATE
				SET
					start_time = EXCLUDED.start_time,
					end_time = EXCLUDED.end_time
				WHERE transfer_balance_grant_run.end_time <= $5
				RETURNING run_id
			`,
			grantId,
			GrantKindSn25Validator,
			startTime,
			endTime,
			renewTime,
		)
		server.WithPgResult(result, err, func() {
			claimed = result.Next()
		})
		if !claimed {
			return
		}

		// data only, at no cost: pro = false and no revenue
		server.Raise(AddGrantTransferBalanceInTx(
			tx,
			ctx,
			networkId,
			GrantKindSn25Validator,
			settings.BalanceByteCount,
			startTime,
			endTime,
		))
		grant = &ValidatorDataGrant{
			NetworkId:        networkId,
			BalanceByteCount: settings.BalanceByteCount,
			StartTime:        startTime,
			EndTime:          endTime,
		}
	}, server.TxReadCommitted)
	return
}
