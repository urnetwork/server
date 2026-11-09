package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// network_sidecar_limit (DESIGN.md 20.5, §2.8 api-13): per-network overrides of the
// sidecar limits, by the limit_key QuotaLimits, QuotaRefusal and pro.yml's key table
// share (pro_sidecar.go). A row raises a limit (an ops change) or tightens one (a §21
// case), cites which in its reason, and lives until it is lifted or its network goes.
//
// NetworkSidecarLimits is the network's tier's limits, then its rows. Readers never
// query per request: every process keeps a snapshot of the whole small table, and a
// read that finds it older than NetworkSidecarLimitSettings.SnapshotTtl starts one
// reload in the background and answers from what it has, so a network with no row costs
// no query (R-QUOTA-5). Until a process's first snapshot is in, a read reads its own
// network's rows. A write reloads its own process's snapshot after it commits, so this
// process enforces a change at once and the others within the ttl.
//
// No row moves erasure's fixed numbers (owner decision 146): evs_closure_bytes_floor and
// evs_closure_uploads_per_s take no row, and evs_closure_bytes_per_day none below the
// floor. ValidateNetworkSidecarLimit refuses such a row, and §21's POST /abuse/action
// calls it; a row written to the table directly is clamped to the floor or, for the
// fixed keys, ignored. No key exists for forwarding or sockets (owner decision 66).
//
// The table belongs to migration group MG1, pending until its Gn-out. A binary that does
// not link MG1 reads no row and refuses every write (server.SidecarMg1Linked), so its
// networks have their tier's limits alone.

// One override row.
type NetworkSidecarLimitRow struct {
	NetworkId  server.Id
	LimitKey   string
	LimitValue int64
	// the ops change or §21 case the row cites
	Reason string
}

// Refused: this binary does not link MG1, so the table does not exist for it.
var ErrNetworkSidecarLimitUnavailable = errors.New("network sidecar limits are not available")

// Refused: no row may hold this key, value or reason. The error's text says which.
var ErrNetworkSidecarLimitRefused = errors.New("network sidecar limit refused")

// The longest reason, the column's width.
const networkSidecarLimitReasonMaxByteCount = 256

// How the snapshot of every row is kept. Every value here is a tunable of the process
// that reads limits.
type NetworkSidecarLimitSettings struct {
	// How long a snapshot serves reads before a read starts a reload in the background.
	// Also how long a failed reload waits before the next. A change made in another
	// process reaches this one within about this long. Non-positive reloads at every read.
	SnapshotTtl time.Duration
	// The longest one reload of the whole table may run.
	ReloadTimeout time.Duration
}

// 5 s, as producers cache limits (§20.7).
func DefaultNetworkSidecarLimitSettings() *NetworkSidecarLimitSettings {
	return &NetworkSidecarLimitSettings{
		SnapshotTtl:   5 * time.Second,
		ReloadTimeout: 30 * time.Second,
	}
}

// Refuses a row that may not stand, before anything writes it: an unknown key, a fixed
// key, a value its kind cannot take, an evs_closure_bytes_per_day below
// evs_closure_bytes_floor, or a reason that cites nothing. A value of -1 is `none`, no
// limit. §21's POST /abuse/action calls this, as the writes here do.
func ValidateNetworkSidecarLimit(limitKey string, limitValue int64, reason string) error {
	if err := validateNetworkSidecarLimitValue(limitKey, limitValue); err != nil {
		return err
	}
	trimmedReason := strings.TrimSpace(reason)
	if trimmedReason == "" {
		return fmt.Errorf("%w: a row needs a reason citing an ops change or a §21 case", ErrNetworkSidecarLimitRefused)
	}
	if networkSidecarLimitReasonMaxByteCount < len(reason) || !utf8.ValidString(reason) {
		return fmt.Errorf("%w: a reason is valid utf-8 of at most %d bytes", ErrNetworkSidecarLimitRefused, networkSidecarLimitReasonMaxByteCount)
	}
	for _, c := range reason {
		if unicode.IsControl(c) {
			return fmt.Errorf("%w: a reason holds no control character", ErrNetworkSidecarLimitRefused)
		}
	}
	return nil
}

// The key and value half of ValidateNetworkSidecarLimit, which also decides what a row
// written directly to the table may do: a row it refuses is ignored, except one under
// the closure floor, which the floor clamps (ProSidecarLimits.Limit).
func validateNetworkSidecarLimitValue(limitKey string, limitValue int64) error {
	row, ok := proSidecarLimitRowForKey(limitKey)
	if !ok {
		return fmt.Errorf("%w: no sidecar limit is named %q", ErrNetworkSidecarLimitRefused, limitKey)
	}
	if row.fixed {
		return fmt.Errorf("%w: %s is fixed, and no row sets it (owner decision 146)", ErrNetworkSidecarLimitRefused, limitKey)
	}
	switch row.kind {
	case proSidecarKindBool:
		if limitValue != 0 && limitValue != 1 {
			return fmt.Errorf("%w: %s is 0 or 1", ErrNetworkSidecarLimitRefused, limitKey)
		}
	case proSidecarKindDuration:
		if limitValue <= 0 {
			return fmt.Errorf("%w: %s is a positive number of milliseconds", ErrNetworkSidecarLimitRefused, limitKey)
		}
	default:
		if limitValue < proSidecarLimitNone {
			return fmt.Errorf("%w: %s is at least 0, or -1 for none", ErrNetworkSidecarLimitRefused, limitKey)
		}
	}
	if row.minValueKey != "" && limitValue != proSidecarLimitNone {
		if floor := Pro().SidecarAbuse.Limit(row.minValueKey); limitValue < floor {
			return fmt.Errorf("%w: %s is never below %s, %d (owner decision 146)", ErrNetworkSidecarLimitRefused, limitKey, row.minValueKey, floor)
		}
	}
	return nil
}

// The network's limits: its tier's (NetworkTierOf), then its rows. Never nil. A row the
// table holds and this build cannot apply is ignored, and one under the closure floor
// reads the floor. Without MG1 linked it is the tier's limits alone, and asks nothing.
func NetworkSidecarLimits(ctx context.Context, networkId server.Id) *ProSidecarLimits {
	_, limits := networkSidecarLimitsWithTier(ctx, nil, networkId)
	return limits
}

// NetworkSidecarLimits for a caller holding a transaction, which reads through tx
// wherever the caches miss (connect/CODESTYLE.md, Locking).
func NetworkSidecarLimitsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) *ProSidecarLimits {
	_, limits := networkSidecarLimitsWithTier(ctx, tx, networkId)
	return limits
}

// The network's tier and its limits, through tx when tx is not nil.
func networkSidecarLimitsWithTier(ctx context.Context, tx server.PgTx, networkId server.Id) (NetworkTier, *ProSidecarLimits) {
	var tier NetworkTier
	if tx != nil {
		tier = NetworkTierOfInTx(ctx, tx, networkId)
	} else {
		tier = NetworkTierOf(ctx, networkId)
	}
	limits := Pro().TierFor(tier).Sidecar
	for limitKey, limitValue := range networkSidecarLimitSnapshotLocal.keyValues(ctx, tx, networkId) {
		limits = limits.withLimit(limitKey, limitValue)
	}
	return tier, &limits
}

// Whether the network has a row for the key, which a higher tier then does not lift.
func networkSidecarLimitHasRow(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string) bool {
	_, ok := networkSidecarLimitSnapshotLocal.keyValues(ctx, tx, networkId)[limitKey]
	return ok
}

// Sets one of the network's limits, replacing any row for its key, after
// ValidateNetworkSidecarLimit. This process's snapshot reloads once it commits.
func SetNetworkSidecarLimit(ctx context.Context, networkId server.Id, limitKey string, limitValue int64, reason string) (returnErr error) {
	if !server.SidecarMg1Linked() {
		return ErrNetworkSidecarLimitUnavailable
	}
	if err := ValidateNetworkSidecarLimit(limitKey, limitValue, reason); err != nil {
		return err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = SetNetworkSidecarLimitInTx(ctx, tx, networkId, limitKey, limitValue, reason)
	})
	if returnErr == nil {
		ReloadNetworkSidecarLimits(ctx)
	}
	return
}

// SetNetworkSidecarLimit in the caller's transaction, such as §21's action with its
// case. The caller reloads this process's snapshot after its commit
// (ReloadNetworkSidecarLimits); others take the row within the snapshot's ttl.
func SetNetworkSidecarLimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string, limitValue int64, reason string) error {
	if !server.SidecarMg1Linked() {
		return ErrNetworkSidecarLimitUnavailable
	}
	if err := ValidateNetworkSidecarLimit(limitKey, limitValue, reason); err != nil {
		return err
	}
	if !networkExistsInTx(ctx, tx, networkId) {
		return ErrNetworkNotFound
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_sidecar_limit (
				network_id,
				limit_key,
				limit_value,
				reason
			)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (network_id, limit_key) DO UPDATE
			SET
				limit_value = EXCLUDED.limit_value,
				reason = EXCLUDED.reason
		`,
		networkId,
		limitKey,
		limitValue,
		reason,
	))
	return nil
}

// Lifts the network's row for the key, which then reads its tier's limit again.
// Reports whether a row went. This process's snapshot reloads once it commits.
func ClearNetworkSidecarLimit(ctx context.Context, networkId server.Id, limitKey string) (cleared bool, returnErr error) {
	if !server.SidecarMg1Linked() {
		return false, ErrNetworkSidecarLimitUnavailable
	}
	server.Tx(ctx, func(tx server.PgTx) {
		cleared, returnErr = ClearNetworkSidecarLimitInTx(ctx, tx, networkId, limitKey)
	})
	if returnErr == nil {
		ReloadNetworkSidecarLimits(ctx)
	}
	return
}

// ClearNetworkSidecarLimit in the caller's transaction. The caller reloads this
// process's snapshot after its commit, as with SetNetworkSidecarLimitInTx.
func ClearNetworkSidecarLimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string) (bool, error) {
	if !server.SidecarMg1Linked() {
		return false, ErrNetworkSidecarLimitUnavailable
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_sidecar_limit
			WHERE network_id = $1 AND limit_key = $2
		`,
		networkId,
		limitKey,
	))
	return tag.RowsAffected() == 1, nil
}

// The network's rows as the table holds them, by key, unusable ones included, for ops
// to see what stands. Read from the table, not the snapshot.
func GetNetworkSidecarLimitRows(ctx context.Context, networkId server.Id) (rows []*NetworkSidecarLimitRow, returnErr error) {
	if !server.SidecarMg1Linked() {
		return nil, ErrNetworkSidecarLimitUnavailable
	}
	rows = []*NetworkSidecarLimitRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT limit_key, limit_value, reason
				FROM network_sidecar_limit
				WHERE network_id = $1
				ORDER BY limit_key
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &NetworkSidecarLimitRow{NetworkId: networkId}
				server.Raise(result.Scan(&row.LimitKey, &row.LimitValue, &row.Reason))
				rows = append(rows, row)
			}
		})
	})
	return
}

// Removes the network's rows in its deletion transaction, the other way a row goes.
func removeNetworkSidecarLimitsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_sidecar_limit
			WHERE network_id = $1
		`,
		networkId,
	))
}

// Reloads this process's snapshot now, for a writer whose transaction has committed. It
// waits for the reload, which reads the whole table. A failure leaves the snapshot as it
// was, and the next stale read retries.
func ReloadNetworkSidecarLimits(ctx context.Context) {
	if !server.SidecarMg1Linked() {
		return
	}
	server.HandleError(func() {
		networkSidecarLimitSnapshotLocal.reload(ctx)
	})
}

// ----- the snapshot -----

// Every network's usable rows, as of one read of the whole table. Safe for concurrent
// use. Loads are ordered by when they start: a load installs only over an older one, so
// a reload that began before a write commits never replaces the writer's own reload.
type networkSidecarLimitSnapshot struct {
	stateLock sync.Mutex
	settings  *NetworkSidecarLimitSettings
	// nil until a load installs one
	networkIdKeyValues map[server.Id]map[string]int64
	// the sequence of the installed load, and the last one handed out
	installedSequence uint64
	lastSequence      uint64
	// when the last background reload began, whether or not it succeeded
	attemptTime time.Time
	// a background reload is running
	reloading bool
}

func newNetworkSidecarLimitSnapshot(settings *NetworkSidecarLimitSettings) *networkSidecarLimitSnapshot {
	return &networkSidecarLimitSnapshot{
		settings: settings,
	}
}

var networkSidecarLimitSnapshotLocal = newNetworkSidecarLimitSnapshot(DefaultNetworkSidecarLimitSettings())

// The network's usable rows, by key, which callers must not change. With no snapshot
// installed yet it reads the network's own rows, through tx when tx is not nil. A stale
// or missing snapshot starts one background reload.
func (self *networkSidecarLimitSnapshot) keyValues(ctx context.Context, tx server.PgTx, networkId server.Id) map[string]int64 {
	if !server.SidecarMg1Linked() {
		return nil
	}
	now := time.Now()
	var keyValues map[string]int64
	installed := false
	startReload := false
	var reloadTimeout time.Duration
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.networkIdKeyValues != nil {
			keyValues = self.networkIdKeyValues[networkId]
			installed = true
		}
		if !self.reloading && self.settings.SnapshotTtl <= now.Sub(self.attemptTime) {
			self.reloading = true
			self.attemptTime = now
			startReload = true
			reloadTimeout = self.settings.ReloadTimeout
		}
	}()
	if startReload {
		go server.HandleError(func() {
			defer func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.reloading = false
			}()
			reloadCtx, reloadCancel := context.WithTimeout(context.Background(), reloadTimeout)
			defer reloadCancel()
			self.reload(reloadCtx)
		})
	}
	if installed {
		return keyValues
	}
	if tx != nil {
		return readNetworkSidecarLimitKeyValues(ctx, tx, networkId)
	}
	server.Db(ctx, func(conn server.PgConn) {
		keyValues = readNetworkSidecarLimitKeyValues(ctx, conn, networkId)
	})
	return keyValues
}

// Reads the whole table and installs it, unless a load that began later has installed
// already. Raises on a read failure, installing nothing.
func (self *networkSidecarLimitSnapshot) reload(ctx context.Context) {
	sequence := func() uint64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastSequence += 1
		return self.lastSequence
	}()

	networkIdKeyValues := map[server.Id]map[string]int64{}
	ignoredCount := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, limit_key, limit_value
				FROM network_sidecar_limit
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var limitKey string
				var limitValue int64
				server.Raise(result.Scan(&networkId, &limitKey, &limitValue))
				if !usableNetworkSidecarLimitRow(limitKey, limitValue) {
					ignoredCount += 1
					continue
				}
				keyValues, ok := networkIdKeyValues[networkId]
				if !ok {
					keyValues = map[string]int64{}
					networkIdKeyValues[networkId] = keyValues
				}
				keyValues[limitKey] = limitValue
			}
		})
	})
	if 0 < ignoredCount {
		glog.Warningf("[quota]%d network_sidecar_limit rows set no limit this build can apply, or a fixed one. They are ignored.\n", ignoredCount)
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.installedSequence < sequence {
		self.networkIdKeyValues = networkIdKeyValues
		self.installedSequence = sequence
	}
}

// One network's usable rows, by key.
func readNetworkSidecarLimitKeyValues(ctx context.Context, conn server.PgCanQuery, networkId server.Id) map[string]int64 {
	keyValues := map[string]int64{}
	result, err := conn.Query(
		ctx,
		`
			SELECT limit_key, limit_value
			FROM network_sidecar_limit
			WHERE network_id = $1
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var limitKey string
			var limitValue int64
			server.Raise(result.Scan(&limitKey, &limitValue))
			if usableNetworkSidecarLimitRow(limitKey, limitValue) {
				keyValues[limitKey] = limitValue
			}
		}
	})
	return keyValues
}

// Whether a row in the table may apply. A row under the closure floor may, and the floor
// clamps it when read; one that sets a fixed key, an unknown key or a value its kind
// cannot take may not.
func usableNetworkSidecarLimitRow(limitKey string, limitValue int64) bool {
	err := validateNetworkSidecarLimitValue(limitKey, limitValue)
	if err == nil {
		return true
	}
	row, ok := proSidecarLimitRowForKey(limitKey)
	return ok && !row.fixed && row.minValueKey != "" && 0 <= limitValue
}

// Replaces the snapshot's settings and drops what it holds, returning a restore function
// that drops it again. Test only: callers restore it before another test reads limits.
func Testing_SetNetworkSidecarLimitSettings(settings *NetworkSidecarLimitSettings) func() {
	reset := func(settings *NetworkSidecarLimitSettings) {
		self := networkSidecarLimitSnapshotLocal
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.settings = settings
		self.networkIdKeyValues = nil
		self.attemptTime = time.Time{}
	}
	prev := func() *NetworkSidecarLimitSettings {
		self := networkSidecarLimitSnapshotLocal
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.settings
	}()
	reset(settings)
	return func() { reset(prev) }
}

// Whether this process has a snapshot installed, and the keys it holds for the network,
// sorted, for tests.
func Testing_NetworkSidecarLimitSnapshotKeys(networkId server.Id) (installed bool, limitKeys []string) {
	self := networkSidecarLimitSnapshotLocal
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.networkIdKeyValues == nil {
		return false, nil
	}
	for limitKey := range self.networkIdKeyValues[networkId] {
		limitKeys = append(limitKeys, limitKey)
	}
	slices.Sort(limitKeys)
	return true, limitKeys
}
