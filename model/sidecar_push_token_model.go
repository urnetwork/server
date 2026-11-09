package model

// §18.7's step 0 (SDK-M8-09; D-SDK-16, WK5): sidecar_push_token, one row per client holding its wake
// token, and the projection of each row onto the client's slot, {scp_<C>}tok and {scp_<C>}tokrev, through
// pushservice.WakeTokenApplyScript alone.
//
// Every change to a row raises its rev by one in the statement that makes it: a POST, an empty POST, a
// vendor's rejection (TombstonePushTokenIfRevision) and the client's removal (TombstonePushTokenInTx, a
// §7.10 removal effect), the last three leaving a tombstone with no platform or token. A new row's rev
// starts at its insert's time in milliseconds, on the primary's clock, so it lands above any revision a
// row of the same client lost to a Postgres restore reached, rows changing far less than once a
// millisecond; §19.10's raise (RaisePushTokenRevisions) lifts every row by 2^32 past what a lost primary
// applied.
//
// Each writer reads t0, the TIME of the master serving the client's slot, before its transaction,
// commits, then applies the row as committed. The script refuses an apply over ApplyWindow past t0, one
// below tokrev and, writing nothing, one equal to it; an expired or failed apply rereads t0 and the row and
// applies again, at most ApplyRetryCount times, after which the row stands and taskworker's sweep lands it
// within about 2 minutes (sidecar_push_token_slots.go). A POST equal to the row commits nothing and
// applies the row as it stands.
//
// Nothing here logs a token or puts one in an error (I-13). Every function needs MG8 linked; without it
// the route answers 503 and the removal effect does nothing, since no row can exist.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
)

var (
	// The client is removed, inactive or unknown: it holds no token, and a POST sets none.
	ErrPushTokenClientRemoved = errors.New("push token: the client is removed")
	// A platform or token POST /sidecar/push-token does not take.
	ErrPushTokenInvalid = errors.New("push token: invalid platform or token")
	// MG8 is not in this binary, so no row can be read or written.
	ErrPushTokenUnavailable = errors.New("push token: sidecar_push_token is not in this binary")
)

// The tunables of the token rows' writers.
type SidecarPushTokenSettings struct {
	// How far past its t0 an apply may land: 60 s (§18.7). taskworker's reconcile waits
	// SidecarPushTokenPassSettings.ReconcileWait after it finds a slot whose master changed, which must
	// exceed this and the masters' clock skew, so that every apply begun before that master took the
	// slot has expired. Whole milliseconds.
	ApplyWindow time.Duration
	// How many times an expired or failed apply rereads t0 and its row and applies again: 3 (§18.7).
	// Zero applies once.
	ApplyRetryCount int
	// The longest one Redis call of an apply may take when its caller gave no deadline: 2 s, so a POST
	// never waits on a slow node for long; a lost apply is the sweep's.
	CallTimeout time.Duration
	// How many of a removal's tombstones its post-commit applies at once: 64. The rest wait for
	// taskworker's sweep, so a removal of many clients never holds its post-commit for long.
	PostCommitApplyCount int
	// Rows one statement of RaisePushTokenRevisions lifts: 10,000, so no transaction of a restore holds
	// every row's lock at once.
	RaiseBatchCount int
}

func DefaultSidecarPushTokenSettings() *SidecarPushTokenSettings {
	return &SidecarPushTokenSettings{
		ApplyWindow:          60 * time.Second,
		ApplyRetryCount:      3,
		CallTimeout:          2 * time.Second,
		PostCommitApplyCount: 64,
		RaiseBatchCount:      10000,
	}
}

// What §19.10's restore adds to every row's rev: 2^32, past any revision a lost primary applied.
const PushTokenRestoreRevisionRaise = int64(4294967296)

// One sidecar_push_token row. A tombstone has neither platform nor token.
type SidecarPushTokenRow struct {
	ClientId server.Id
	// apns or fcm; empty on a tombstone.
	Platform string
	// Empty on a tombstone. Never logged (I-13).
	Token       string
	Rev         int64
	UpdateTime  time.Time
	RemovedTime *time.Time
	// The hash slot of the client's {scp_<C>} tag.
	Slot int
}

// Whether the row holds no token.
func (self *SidecarPushTokenRow) Tombstone() bool {
	return self.Token == ""
}

// The row as the apply script carries it.
func (self *SidecarPushTokenRow) wakeTokenRow() *pushservice.WakeTokenRow {
	return &pushservice.WakeTokenRow{
		ClientId: connect.Id(self.ClientId),
		Revision: self.Rev,
		Platform: self.Platform,
		Token:    self.Token,
	}
}

// What the row shows when printed: never its token.
func (self SidecarPushTokenRow) String() string {
	return fmt.Sprintf("SidecarPushTokenRow{ClientId: %s, Platform: %q, Token: (%d bytes), Rev: %d, Slot: %d}", self.ClientId, self.Platform, len(self.Token), self.Rev, self.Slot)
}

// The same as String.
func (self SidecarPushTokenRow) GoString() string {
	return self.String()
}

// The hash slot of clientId's {scp_<C>} tag: its row's slot, where every key of the client lives.
func PushTokenSlot(clientId server.Id) int {
	return server.RedisHashSlot(pushservice.HintTag(connect.Id(clientId)))
}

// The process's Redis for the token's scripts: server.Redis through the push adapter, which refuses a
// call whose keys share no tag.
var sidecarPushTokenRedis = sync.OnceValue(func() *SidecarPushRedis {
	return NewSidecarPushRedis(&SidecarPushRedisSettings{
		CallTimeout: DefaultSidecarPushTokenSettings().CallTimeout,
	})
})

// The columns every read takes, in scanPushTokenRow's order.
const pushTokenColumns = `client_id, platform, token, rev, update_time, removed_time, slot`

// One row of a query that selected pushTokenColumns.
func scanPushTokenRow(result server.PgResult) (*SidecarPushTokenRow, error) {
	row := &SidecarPushTokenRow{}
	var platform, token *string
	var removedTime *time.Time
	if err := result.Scan(&row.ClientId, &platform, &token, &row.Rev, &row.UpdateTime, &removedTime, &row.Slot); err != nil {
		return nil, err
	}
	if platform != nil {
		row.Platform = *platform
	}
	if token != nil {
		row.Token = *token
	}
	if removedTime != nil {
		utc := removedTime.UTC()
		row.RemovedTime = &utc
	}
	row.UpdateTime = row.UpdateTime.UTC()
	return row, nil
}

// The client's row, or nil while it has none.
func GetPushToken(ctx context.Context, clientId server.Id) (*SidecarPushTokenRow, error) {
	if !server.SidecarMg8Linked() {
		return nil, ErrPushTokenUnavailable
	}
	var row *SidecarPushTokenRow
	var returnErr error
	server.Db(ctx, func(conn server.PgConn) {
		row, returnErr = nil, nil
		result, err := conn.Query(
			ctx,
			`SELECT `+pushTokenColumns+` FROM sidecar_push_token WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row, returnErr = scanPushTokenRow(result)
			}
		})
	})
	return row, returnErr
}

// What SetPushToken did to the row.
type PushTokenOutcome string

const (
	// The client had no row: one was inserted, its rev the insert's time in milliseconds.
	PushTokenInserted PushTokenOutcome = "inserted"
	// The row changed, its rev raised by one.
	PushTokenChanged PushTokenOutcome = "changed"
	// The POST was equal to the row: nothing was committed, and the row was applied as it stands.
	PushTokenUnchanged PushTokenOutcome = "unchanged"
)

// What one SetPushToken did.
type SetPushTokenResult struct {
	Outcome PushTokenOutcome
	// The row as committed, or as it stands for an unchanged POST.
	Row *SidecarPushTokenRow
	// What the last apply answered: applied, current or stale once it landed, expired when every try
	// expired, empty when every try failed, which leaves the row to taskworker's sweep.
	Applied pushservice.WakeTokenApplyResult
}

// POST /sidecar/push-token's write (§18.7's step 0): sets clientId's token, or tombstones it when token is
// empty, then applies the row. Reads t0 first, then in one transaction locks the client's network_client
// row FOR SHARE, so a removal that has committed refuses it with ErrPushTokenClientRemoved and one that
// runs after it waits and tombstones what it wrote, and locks or inserts the token row. An apply that does
// not land is no error: the row stands and taskworker's sweep lands it.
func SetPushToken(ctx context.Context, clientId server.Id, platform string, token string, settings *SidecarPushTokenSettings) (*SetPushTokenResult, error) {
	if !server.SidecarMg8Linked() {
		return nil, ErrPushTokenUnavailable
	}
	if settings == nil {
		settings = DefaultSidecarPushTokenSettings()
	}
	if token == "" {
		if !sidecar.ValidPushTokenDelete(platform) {
			return nil, ErrPushTokenInvalid
		}
		// a tombstone names no platform
		platform = ""
	} else if !sidecar.ValidPushToken(platform, token) {
		return nil, ErrPushTokenInvalid
	}

	t0, t0Err := readPushTokenTime(ctx, clientId, settings)

	var result *SetPushTokenResult
	var removed bool
	server.Tx(ctx, func(tx server.PgTx) {
		result, removed = nil, false
		var active bool
		err := tx.QueryRow(
			ctx,
			`
				SELECT active
				FROM network_client
				WHERE client_id = $1
				FOR SHARE
			`,
			clientId,
		).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
			removed = true
			return
		}
		server.Raise(err)
		result = setPushTokenInTx(ctx, tx, clientId, platform, token)
	}, server.TxReadCommitted)
	if removed {
		return nil, ErrPushTokenClientRemoved
	}

	var firstT0 *int64
	if t0Err == nil {
		firstT0 = &t0
	}
	result.Applied = applyPushTokenWithRetries(ctx, result.Row, firstT0, settings)
	return result, nil
}

// The row after this POST, locking it or inserting it, in tx.
func setPushTokenInTx(ctx context.Context, tx server.PgTx, clientId server.Id, platform string, token string) *SetPushTokenResult {
	nullable := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	lockRow := func() *SidecarPushTokenRow {
		var row *SidecarPushTokenRow
		result, err := tx.Query(
			ctx,
			`SELECT `+pushTokenColumns+` FROM sidecar_push_token WHERE client_id = $1 FOR UPDATE`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var scanErr error
				row, scanErr = scanPushTokenRow(result)
				server.Raise(scanErr)
			}
		})
		return row
	}

	row := lockRow()
	if row == nil {
		// rev is the insert's time in milliseconds, on the primary's clock, as update_time
		var inserted *SidecarPushTokenRow
		result, err := tx.Query(
			ctx,
			`
				WITH insert_clock AS MATERIALIZED (
					SELECT clock_timestamp() AT TIME ZONE 'UTC' AS update_time
				)
				INSERT INTO sidecar_push_token (client_id, platform, token, rev, update_time, removed_time, slot)
				SELECT
					$1,
					$2,
					$3,
					floor(extract(epoch FROM insert_clock.update_time) * 1000)::bigint,
					insert_clock.update_time,
					NULL,
					$4
				FROM insert_clock
				ON CONFLICT (client_id) DO NOTHING
				RETURNING `+pushTokenColumns,
			clientId,
			nullable(platform),
			nullable(token),
			PushTokenSlot(clientId),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var scanErr error
				inserted, scanErr = scanPushTokenRow(result)
				server.Raise(scanErr)
			}
		})
		if inserted != nil {
			return &SetPushTokenResult{Outcome: PushTokenInserted, Row: inserted}
		}
		// a concurrent POST inserted it first, and has committed: change that row as any other
		row = lockRow()
		if row == nil {
			server.Raise(errors.New("push token: the row a conflict named is gone"))
		}
	}

	if row.Platform == platform && row.Token == token {
		return &SetPushTokenResult{Outcome: PushTokenUnchanged, Row: row}
	}
	var changed *SidecarPushTokenRow
	result, err := tx.Query(
		ctx,
		`
			UPDATE sidecar_push_token
			SET
				platform = $2,
				token = $3,
				rev = rev + 1,
				update_time = clock_timestamp() AT TIME ZONE 'UTC'
			WHERE client_id = $1
			RETURNING `+pushTokenColumns,
		clientId,
		nullable(platform),
		nullable(token),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var scanErr error
			changed, scanErr = scanPushTokenRow(result)
			server.Raise(scanErr)
		}
	})
	if changed == nil {
		server.Raise(errors.New("push token: the locked row is gone"))
	}
	return &SetPushTokenResult{Outcome: PushTokenChanged, Row: changed}
}

// The removal's part (§7.10, §18.7): every client the removal ends has its row, if it has one,
// tombstoned in the removal's transaction. Registered as RemovalEffectPushToken.
const RemovalEffectPushToken = "push_token"

func init() {
	RegisterRemovalEffect(RemovalEffectPushToken, applyPushTokenRemovalEffectInTx)
}

// Tombstones the rows of the removal's clients. Idempotent: a row already removed is not written again,
// so §19.10's replay of a removal changes nothing.
func applyPushTokenRemovalEffectInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	if !server.SidecarMg8Linked() {
		return nil
	}
	subjects := removal.SubjectsOf(sidecar.RevocationKindClient)
	if len(subjects) == 0 {
		return nil
	}
	clientIds := make([]server.Id, 0, len(subjects))
	for _, subject := range subjects {
		clientIds = append(clientIds, subject.SubjectId)
	}
	_, err := TombstonePushTokenInTx(ctx, tx, clientIds)
	return err
}

// Tombstones clientIds' rows in tx, the removal's own transaction: no platform or token, rev raised by
// one, update_time and removed_time the primary's clock. A row already removed is left as it is. Returns
// the clients whose rows it tombstoned, whose applies run after the commit, at most
// PostCommitApplyCount of them; the rest, and any the post-commit loses, land by taskworker's sweep. Holds
// the rows' locks until tx ends. A binary without MG8 does nothing.
func TombstonePushTokenInTx(ctx context.Context, tx server.PgTx, clientIds []server.Id) ([]server.Id, error) {
	if !server.SidecarMg8Linked() || len(clientIds) == 0 {
		return nil, nil
	}
	tombstonedClientIds := []server.Id{}
	result, err := tx.Query(
		ctx,
		`
			WITH removal_clock AS MATERIALIZED (
				SELECT clock_timestamp() AT TIME ZONE 'UTC' AS removed_time
			)
			UPDATE sidecar_push_token
			SET
				platform = NULL,
				token = NULL,
				rev = sidecar_push_token.rev + 1,
				update_time = removal_clock.removed_time,
				removed_time = removal_clock.removed_time
			FROM removal_clock
			WHERE
				sidecar_push_token.client_id = ANY($1) AND
				sidecar_push_token.removed_time IS NULL
			RETURNING sidecar_push_token.client_id
		`,
		clientIds,
	)
	if err != nil {
		return nil, err
	}
	for result.Next() {
		var clientId server.Id
		if err := result.Scan(&clientId); err != nil {
			result.Close()
			return nil, err
		}
		tombstonedClientIds = append(tombstonedClientIds, clientId)
	}
	result.Close()
	if err := result.Err(); err != nil {
		return nil, err
	}
	if len(tombstonedClientIds) == 0 {
		return tombstonedClientIds, nil
	}
	settings := DefaultSidecarPushTokenSettings()
	applyClientIds := tombstonedClientIds[:min(len(tombstonedClientIds), settings.PostCommitApplyCount)]
	applyClientIds = slices.Clone(applyClientIds)
	server.AddTxPostCommit(tx, "push_token_tombstone/"+server.NewId().String(), func() any {
		applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), server.TxPostCommitTimeout)
		defer cancel()
		for _, clientId := range applyClientIds {
			if applyCtx.Err() != nil {
				// the sweep lands the rest
				return nil
			}
			server.HandleError(func() {
				ApplyPushToken(applyCtx, clientId, settings)
			})
		}
		return nil
	})
	return tombstonedClientIds, nil
}

// The push sender's tombstone on a vendor's 410 or UNREGISTERED (§18.7's step 4; SDK-M8-10's
// WakeTokenStore): tombstones clientId's row only while its rev equals rev, the tokrev the sender read with
// the rejected token, so a newer registration is never removed, then applies the tombstone. Returns
// whether it tombstoned. A row already a tombstone, or at another rev, is left as it is.
func TombstonePushTokenIfRevision(ctx context.Context, clientId server.Id, rev int64, settings *SidecarPushTokenSettings) (bool, error) {
	if !server.SidecarMg8Linked() {
		return false, ErrPushTokenUnavailable
	}
	if settings == nil {
		settings = DefaultSidecarPushTokenSettings()
	}
	t0, t0Err := readPushTokenTime(ctx, clientId, settings)
	var tombstoned *SidecarPushTokenRow
	server.Tx(ctx, func(tx server.PgTx) {
		tombstoned = nil
		result, err := tx.Query(
			ctx,
			`
				UPDATE sidecar_push_token
				SET
					platform = NULL,
					token = NULL,
					rev = rev + 1,
					update_time = clock_timestamp() AT TIME ZONE 'UTC'
				WHERE
					client_id = $1 AND
					rev = $2 AND
					token IS NOT NULL
				RETURNING `+pushTokenColumns,
			clientId,
			rev,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var scanErr error
				tombstoned, scanErr = scanPushTokenRow(result)
				server.Raise(scanErr)
			}
		})
	}, server.TxReadCommitted)
	if tombstoned == nil {
		return false, nil
	}
	var firstT0 *int64
	if t0Err == nil {
		firstT0 = &t0
	}
	applyPushTokenWithRetries(ctx, tombstoned, firstT0, settings)
	return true, nil
}

// Reads t0 and then clientId's row, and applies it, as a writer's retry does, up to ApplyRetryCount more
// times while the apply expires or fails. Returns the row it last read, nil while the client has none, and
// what the last apply answered, as SetPushTokenResult.Applied.
func ApplyPushToken(ctx context.Context, clientId server.Id, settings *SidecarPushTokenSettings) (*SidecarPushTokenRow, pushservice.WakeTokenApplyResult) {
	if settings == nil {
		settings = DefaultSidecarPushTokenSettings()
	}
	var row *SidecarPushTokenRow
	var applied pushservice.WakeTokenApplyResult
	for attempt := 0; attempt <= settings.ApplyRetryCount && ctx.Err() == nil; attempt += 1 {
		var found, landed bool
		row, applied, found, landed = applyPushTokenOnce(ctx, clientId, settings)
		if !found || landed {
			break
		}
	}
	return row, applied
}

// One reread and apply: t0, then the row, then the script. found is false only for a client with no row;
// landed is true once the script answered anything but expired.
func applyPushTokenOnce(ctx context.Context, clientId server.Id, settings *SidecarPushTokenSettings) (row *SidecarPushTokenRow, applied pushservice.WakeTokenApplyResult, found bool, landed bool) {
	t0, err := readPushTokenTime(ctx, clientId, settings)
	if err != nil {
		return nil, "", true, false
	}
	row, err = GetPushToken(ctx, clientId)
	if err != nil {
		return nil, "", true, false
	}
	if row == nil {
		return nil, "", false, false
	}
	applied, err = applyPushTokenRow(ctx, row, t0, settings)
	if err != nil {
		return row, "", true, false
	}
	return row, applied, true, applied != pushservice.WakeTokenExpired
}

// Applies row with firstT0, when its writer read one before its transaction, then, while that expires or
// fails, or when there was no t0 to apply with, rereads t0 and the row and applies again, at most
// ApplyRetryCount times. What the last apply answered.
func applyPushTokenWithRetries(ctx context.Context, row *SidecarPushTokenRow, firstT0 *int64, settings *SidecarPushTokenSettings) pushservice.WakeTokenApplyResult {
	var applied pushservice.WakeTokenApplyResult
	if firstT0 != nil {
		result, err := applyPushTokenRow(ctx, row, *firstT0, settings)
		if err == nil {
			if result != pushservice.WakeTokenExpired {
				return result
			}
			applied = result
		}
	}
	for attempt := 0; attempt < settings.ApplyRetryCount && ctx.Err() == nil; attempt += 1 {
		_, result, found, landed := applyPushTokenOnce(ctx, row.ClientId, settings)
		if !found {
			return ""
		}
		applied = result
		if landed {
			break
		}
	}
	return applied
}

// The TIME of the master serving clientId's slot, in milliseconds, bounded by CallTimeout when ctx has no
// deadline.
func readPushTokenTime(ctx context.Context, clientId server.Id, settings *SidecarPushTokenSettings) (int64, error) {
	callCtx, cancel := pushTokenCallContext(ctx, settings)
	defer cancel()
	t0, err := pushservice.ReadWakeTokenTime(callCtx, sidecarPushTokenRedis(), connect.Id(clientId))
	if err != nil && glog.V(1) {
		glog.Infof("[pt]t0 for %s not read: %s\n", clientId, err)
	}
	return t0, err
}

// One apply of row with t0, bounded by CallTimeout when ctx has no deadline.
func applyPushTokenRow(ctx context.Context, row *SidecarPushTokenRow, t0 int64, settings *SidecarPushTokenSettings) (pushservice.WakeTokenApplyResult, error) {
	callCtx, cancel := pushTokenCallContext(ctx, settings)
	defer cancel()
	applied, err := pushservice.ApplyWakeToken(callCtx, sidecarPushTokenRedis(), row.wakeTokenRow(), t0, settings.ApplyWindow)
	if err != nil && glog.V(1) {
		// the error is the store's or the script's, never the row's token
		glog.Infof("[pt]apply of %s at rev %d failed: %s\n", row.ClientId, row.Rev, err)
	}
	return applied, err
}

// ctx with CallTimeout when it has no deadline of its own.
func pushTokenCallContext(ctx context.Context, settings *SidecarPushTokenSettings) (context.Context, context.CancelFunc) {
	if _, bounded := ctx.Deadline(); bounded || settings.CallTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, settings.CallTimeout)
}

// §19.10's api step (FS-42): raises every row's rev by 2^32 and its update_time to now, so taskworker's
// sweep applies each row above any revision a lost primary applied. Runs in batches of RaiseBatchCount by
// a client_id cursor, each its own transaction; a POST between batches only adds one more. Returns how
// many rows it raised.
func RaisePushTokenRevisions(ctx context.Context, settings *SidecarPushTokenSettings) (int64, error) {
	if !server.SidecarMg8Linked() {
		return 0, ErrPushTokenUnavailable
	}
	if settings == nil {
		settings = DefaultSidecarPushTokenSettings()
	}
	if settings.RaiseBatchCount <= 0 {
		return 0, fmt.Errorf("push token: a raise batch of %d", settings.RaiseBatchCount)
	}
	var raisedCount int64
	cursor := server.Id{}
	for {
		var batchClientIds []server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			batchClientIds = nil
			result, err := tx.Query(
				ctx,
				`
					WITH batch AS (
						SELECT client_id
						FROM sidecar_push_token
						WHERE client_id > $1
						ORDER BY client_id
						LIMIT $2
						FOR UPDATE
					)
					UPDATE sidecar_push_token
					SET
						rev = sidecar_push_token.rev + $3,
						update_time = clock_timestamp() AT TIME ZONE 'UTC'
					FROM batch
					WHERE sidecar_push_token.client_id = batch.client_id
					RETURNING sidecar_push_token.client_id
				`,
				cursor,
				settings.RaiseBatchCount,
				PushTokenRestoreRevisionRaise,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					server.Raise(result.Scan(&clientId))
					batchClientIds = append(batchClientIds, clientId)
				}
			})
		}, server.TxReadCommitted)
		raisedCount += int64(len(batchClientIds))
		if len(batchClientIds) < settings.RaiseBatchCount {
			return raisedCount, nil
		}
		cursor = slices.MaxFunc(batchClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	}
}
