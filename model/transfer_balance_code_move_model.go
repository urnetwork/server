package model

// `bringyourctl balance move-redeemed` moves the unused balance that a
// balance code granted from a network that was deleted to another network
// (support inbox 764, owner decision 2026-10-29). A code redeemed on a
// network that was deleted later leaves its balance on that network, where
// no one can use it, and the code cannot be redeemed again.
//
// The move gives the target network what redeeming the code there gave. It
// changes the owner of the two rows a redeem writes (RedeemBalanceCodeInTx),
// and nothing else:
//   - the code's transfer_balance moves with its window, bytes, revenue and
//     pro flag unchanged, so the target holds the same data and the same Pro
//     entitlement the source held;
//   - the code itself (transfer_balance_code.network_id) moves, so it is
//     listed among the target's redeemed codes.
//
// It takes the redeem path's destination lock (LockPaymentNetworkInTx) and
// the ownership every writer of a granted balance takes
// (tryTransferBalanceOwnershipInTx, as the refund clawback does), and
// refreshes the target's Pro cache after commit, as every Pro writer does
// (UpdateProNetwork).
//
// The pro flag is the balance's own. A code redeemed before the pro column
// was added (July 2026) was grandfathered pro = true for its whole window; a
// code redeemed since grants data only. The move never changes the flag, so it
// never grants Pro to a data-only code and never takes it from a
// grandfathered one.
//
// It refuses unless exactly one code granted the balance, the code is not
// cancelled and names the balance's network, that network no longer exists,
// the balance is unused (no byte spent, no escrow, no debit journal row) and
// unexpired, and the target network exists.
//
// The plan only reads, in one read-only transaction. Its digest binds the
// balance, the code, both networks and every balance field the move carries.
// A commit takes the locks, reads and checks everything again, and refuses
// unless the digest is the expected one. It writes the two owners and an
// audit_network_event on the target network in one transaction. The event's
// id derives from the balance and the target, so a commit run again finds the
// move it made and writes nothing. Once the audit retention
// (AuditNetworkEventExpiration) has removed the event, a commit run again
// finds the balance on a network that exists and refuses, still writing
// nothing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The audit_network_event type of a move, recorded on the target network.
const AuditEventTypeRedeemedBalanceMoved AuditEventType = "redeemed_balance_moved"

const (
	// the balance can move
	RedeemedBalanceMoveStateMove = "move"
	// the move was made: the balance and its code are on the target, and the
	// audit event of the move is there
	RedeemedBalanceMoveStateMoved = "moved"
	// a check failed; nothing will be written
	RedeemedBalanceMoveStateRefused = "refused"
)

var ErrRedeemedBalanceMoveRefused = errors.New("balance move-redeemed: refused; nothing was written")

var ErrRedeemedBalanceMoveDigestMismatch = errors.New("balance move-redeemed: --expect does not match the digest of this move; nothing was written")

// RedeemedBalanceMove is what a move carries: the balance and its code, both
// networks, and every balance field, as they were read before the move.
type RedeemedBalanceMove struct {
	BalanceId             server.Id `json:"balance_id"`
	BalanceCodeId         server.Id `json:"balance_code_id"`
	FromNetworkId         server.Id `json:"from_network_id"`
	ToNetworkId           server.Id `json:"to_network_id"`
	RedeemTime            time.Time `json:"redeem_time"`
	StartTime             time.Time `json:"start_time"`
	EndTime               time.Time `json:"end_time"`
	StartBalanceByteCount ByteCount `json:"start_balance_byte_count"`
	BalanceByteCount      ByteCount `json:"balance_byte_count"`
	NetRevenue            NanoCents `json:"net_revenue_nano_cents"`
	SubsidyNetRevenue     NanoCents `json:"subsidy_net_revenue_nano_cents"`
	Pro                   bool      `json:"pro"`
}

// The canonical text of the digest, one field per line.
func (self *RedeemedBalanceMove) digestLines() []string {
	formatTime := func(t time.Time) string {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return []string{
		"urnetwork balance move-redeemed v1",
		"balance " + self.BalanceId.String(),
		"code " + self.BalanceCodeId.String(),
		"from " + self.FromNetworkId.String(),
		"to " + self.ToNetworkId.String(),
		"redeem_time " + formatTime(self.RedeemTime),
		"window " + formatTime(self.StartTime) + " " + formatTime(self.EndTime),
		fmt.Sprintf("bytes %d %d", self.StartBalanceByteCount, self.BalanceByteCount),
		fmt.Sprintf("revenue_nano_cents %d %d", self.NetRevenue, self.SubsidyNetRevenue),
		"pro " + strconv.FormatBool(self.Pro),
	}
}

// Digest is the sha256, in hex, of the move's canonical text.
func (self *RedeemedBalanceMove) Digest() string {
	sum := sha256.Sum256([]byte(strings.Join(self.digestLines(), "\n")))
	return hex.EncodeToString(sum[:])
}

// The id of a move's audit event. One balance moves to one network at most
// once (a balance moves only off a deleted network), so a commit run again
// finds the event of the move it made.
func RedeemedBalanceMoveEventId(balanceId server.Id, toNetworkId server.Id) server.Id {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"urnetwork/balance-move-redeemed/event/v1",
		balanceId.String(),
		toNetworkId.String(),
	}, "\x00")))
	id := sum[:16]
	// a version 8 (custom) RFC 9562 UUID
	id[6] = (id[6] & 0x0f) | 0x80
	id[8] = (id[8] & 0x3f) | 0x80
	return server.RequireIdFromBytes(id)
}

// The audit_network_event details of a move.
type redeemedBalanceMoveAudit struct {
	Command string               `json:"command"`
	Digest  string               `json:"digest"`
	Move    *RedeemedBalanceMove `json:"move"`
}

// A network named by a move, with its administrator's sign-ins, so the
// operator can see whose network it is.
type RedeemedBalanceMoveNetwork struct {
	NetworkId   server.Id `json:"network_id"`
	NetworkName string    `json:"network_name"`
	AdminUserId server.Id `json:"admin_user_id"`
	// "password <user_auth>", "<sso type> <user_auth>" and "wallet <chain>"
	SignIns []string `json:"sign_ins"`
}

type RedeemedBalanceMovePlan struct {
	Time        time.Time `json:"time"`
	BalanceId   server.Id `json:"balance_id"`
	ToNetworkId server.Id `json:"to_network_id"`
	State       string    `json:"state"`
	// why the move is refused; empty unless the state is refused
	Refusals []string `json:"refusals,omitempty"`

	// what the move carries, set once the balance and one code were read; for
	// a move already made, what it carried
	Move *RedeemedBalanceMove `json:"move,omitempty"`
	// the digest of Move, set when the state is move or moved
	Digest string `json:"digest,omitempty"`

	// the facts the checks read
	BalanceFound bool `json:"balance_found"`
	// the network that holds the balance now
	BalanceNetworkId *server.Id `json:"balance_network_id,omitempty"`
	// the codes that granted the balance (one, for a move)
	CodeCount int `json:"code_count"`
	// the network the code names now
	CodeNetworkId *server.Id `json:"code_network_id,omitempty"`
	CodeCancelled bool       `json:"code_cancelled"`
	// whether the network holding the balance exists
	FromNetworkExists bool `json:"from_network_exists"`
	// nil when no network has the target id
	ToNetwork         *RedeemedBalanceMoveNetwork `json:"to_network,omitempty"`
	EscrowCount       int64                       `json:"escrow_count"`
	DebitJournalCount int64                       `json:"debit_journal_count"`
	// the target's Pro entitlement as read by the plan, from the database
	ToNetworkPro bool `json:"to_network_pro"`

	// the audit event of a move already made
	MovedEventId *server.Id `json:"moved_event_id,omitempty"`
	MovedTime    *time.Time `json:"moved_time,omitempty"`

	// a commit: whether this run wrote the move, its audit event, and the
	// target's entitlement and cache refresh afterwards
	Committed         bool       `json:"committed"`
	AuditEventId      *server.Id `json:"audit_event_id,omitempty"`
	ToNetworkProAfter *bool      `json:"to_network_pro_after,omitempty"`
	ProCacheError     string     `json:"pro_cache_error,omitempty"`
}

func (self *RedeemedBalanceMovePlan) refuse(format string, args ...any) {
	self.Refusals = append(self.Refusals, fmt.Sprintf(format, args...))
}

// PlanRedeemedBalanceMove is the dry run. It reads in one read-only
// transaction and writes nothing: not the database, and not the Pro cache.
func PlanRedeemedBalanceMove(
	ctx context.Context,
	balanceId server.Id,
	toNetworkId server.Id,
) (plan *RedeemedBalanceMovePlan, returnErr error) {
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			plan = readRedeemedBalanceMovePlan(ctx, tx, balanceId, toNetworkId, server.NowUtc(), false)
		}, pgx.ReadOnly)
	}, func(err error) {
		returnErr = err
	})
	return
}

// CommitRedeemedBalanceMove makes the move when its digest is expect. It
// returns the plan it read under the locks, and an error when it refuses or
// fails, in which case nothing was written. A move already made with that
// digest writes nothing and returns no error.
func CommitRedeemedBalanceMove(
	ctx context.Context,
	balanceId server.Id,
	toNetworkId server.Id,
	expect string,
) (plan *RedeemedBalanceMovePlan, returnErr error) {
	if balanceId == (server.Id{}) || toNetworkId == (server.Id{}) {
		// a zero id has no ownership key; no balance or network has it
		plan = &RedeemedBalanceMovePlan{
			Time:        server.NowUtc(),
			BalanceId:   balanceId,
			ToNetworkId: toNetworkId,
			State:       RedeemedBalanceMoveStateRefused,
		}
		plan.refuse("the balance id and the target network id must not be the zero id")
		return plan, ErrRedeemedBalanceMoveRefused
	}
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			plan, returnErr = commitRedeemedBalanceMoveInTx(ctx, tx, balanceId, toNetworkId, expect, server.NowUtc())
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) {
		if plan != nil {
			plan.Committed = false
			plan.AuditEventId = nil
		}
		returnErr = err
	})
	if returnErr != nil || plan == nil || !plan.Committed {
		return
	}
	// The target's entitlement changed (a pro balance moved to it): refresh
	// both cache tiers now rather than after ProCacheTtl, as every Pro writer
	// does after its commit. The move has committed; a cache failure only
	// leaves the refresh to the ttl.
	server.HandleError(func() {
		pro := UpdateProNetwork(ctx, toNetworkId)
		plan.ToNetworkProAfter = &pro
	}, func(err error) {
		plan.ProCacheError = err.Error()
	})
	return
}

// The body of a commit, in a read-committed no-retry transaction. Every
// refusal returns before the first write, so the transaction ends having
// written nothing; any failure after the first write raises, which rolls the
// whole move back.
func commitRedeemedBalanceMoveInTx(
	ctx context.Context,
	tx server.PgTx,
	balanceId server.Id,
	toNetworkId server.Id,
	expect string,
	now time.Time,
) (*RedeemedBalanceMovePlan, error) {
	// the ownership of every writer of a granted balance, before any row lock
	admitted, err := tryTransferBalanceOwnershipInTx(ctx, tx, []server.Id{balanceId})
	if err != nil {
		return nil, err
	}
	if !admitted {
		return nil, errTransferBalanceOwnershipBusy
	}
	// the redeem path's destination lock: the target cannot be deleted
	// before this transaction ends
	if err := LockPaymentNetworkInTx(tx, ctx, toNetworkId); err != nil && !errors.Is(err, ErrPaymentNetworkNotFound) {
		return nil, err
	}

	plan := readRedeemedBalanceMovePlan(ctx, tx, balanceId, toNetworkId, now, true)
	switch plan.State {
	case RedeemedBalanceMoveStateRefused:
		return plan, ErrRedeemedBalanceMoveRefused
	case RedeemedBalanceMoveStateMoved:
		if plan.Digest != expect {
			return plan, ErrRedeemedBalanceMoveDigestMismatch
		}
		// made before, with this digest: nothing to write
		return plan, nil
	}
	if plan.Digest != expect {
		return plan, ErrRedeemedBalanceMoveDigestMismatch
	}

	move := plan.Move
	// the two owners a redeem writes, each only as the plan read it
	balanceTag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE transfer_balance
			SET network_id = $2
			WHERE
				balance_id = $1 AND
				network_id = $3 AND
				start_time = $4 AND
				end_time = $5 AND
				start_balance_byte_count = $6 AND
				balance_byte_count = $7 AND
				net_revenue_nano_cents = $8 AND
				subsidy_net_revenue_nano_cents = $9 AND
				pro = $10
		`,
		move.BalanceId,
		move.ToNetworkId,
		move.FromNetworkId,
		move.StartTime,
		move.EndTime,
		move.StartBalanceByteCount,
		move.BalanceByteCount,
		move.NetRevenue,
		move.SubsidyNetRevenue,
		move.Pro,
	))
	if balanceTag.RowsAffected() != 1 {
		// cannot happen under the row lock; nothing was written
		return plan, fmt.Errorf("balance move-redeemed: the balance changed under its lock (%d rows); nothing was written", balanceTag.RowsAffected())
	}
	// From here on a failure raises: the balance row is written.
	codeTag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE transfer_balance_code
			SET network_id = $3
			WHERE
				balance_code_id = $1 AND
				redeem_balance_id = $2 AND
				network_id = $4 AND
				cancel_time IS NULL
		`,
		move.BalanceCodeId,
		move.BalanceId,
		move.ToNetworkId,
		move.FromNetworkId,
	))
	if codeTag.RowsAffected() != 1 {
		// cannot happen under the row lock; the raise rolls the balance back
		server.Raise(fmt.Errorf("balance move-redeemed: the code changed under its lock (%d rows); nothing was written", codeTag.RowsAffected()))
	}

	// the audit line, in the same transaction as the move
	details, err := json.Marshal(&redeemedBalanceMoveAudit{
		Command: "bringyourctl balance move-redeemed",
		Digest:  plan.Digest,
		Move:    move,
	})
	server.Raise(err)
	eventId := RedeemedBalanceMoveEventId(move.BalanceId, move.ToNetworkId)
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO audit_network_event (
				event_id,
				event_time,
				network_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5)
		`,
		eventId,
		now,
		move.ToNetworkId,
		AuditEventTypeRedeemedBalanceMoved,
		string(details),
	))

	plan.Committed = true
	plan.AuditEventId = &eventId
	return plan, nil
}

// Reads every fact of the move and decides its state. With lock, the balance
// and code rows are locked for update (the commit); without, it only reads
// (the dry run).
func readRedeemedBalanceMovePlan(
	ctx context.Context,
	tx server.PgTx,
	balanceId server.Id,
	toNetworkId server.Id,
	now time.Time,
	lock bool,
) *RedeemedBalanceMovePlan {
	plan := &RedeemedBalanceMovePlan{
		Time:        now,
		BalanceId:   balanceId,
		ToNetworkId: toNetworkId,
		State:       RedeemedBalanceMoveStateRefused,
	}
	forUpdate := ""
	if lock {
		forUpdate = "FOR UPDATE"
	}

	move := &RedeemedBalanceMove{
		BalanceId:   balanceId,
		ToNetworkId: toNetworkId,
	}
	var balanceNetworkId server.Id
	var grantKind *string
	var purchaseToken bool
	result, err := tx.Query(
		ctx,
		`
			SELECT
				network_id,
				start_time,
				end_time,
				start_balance_byte_count,
				balance_byte_count,
				net_revenue_nano_cents,
				subsidy_net_revenue_nano_cents,
				pro,
				grant_kind,
				purchase_token IS NOT NULL
			FROM transfer_balance
			WHERE balance_id = $1
		`+forUpdate,
		balanceId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			plan.BalanceFound = true
			server.Raise(result.Scan(
				&balanceNetworkId,
				&move.StartTime,
				&move.EndTime,
				&move.StartBalanceByteCount,
				&move.BalanceByteCount,
				&move.NetRevenue,
				&move.SubsidyNetRevenue,
				&move.Pro,
				&grantKind,
				&purchaseToken,
			))
		}
	})
	if !plan.BalanceFound {
		plan.refuse("no balance has id %s", balanceId)
		return plan
	}
	plan.BalanceNetworkId = &balanceNetworkId
	move.FromNetworkId = balanceNetworkId

	type codeRow struct {
		balanceCodeId server.Id
		networkId     *server.Id
		redeemTime    *time.Time
		cancelTime    *time.Time
	}
	codes := []codeRow{}
	result, err = tx.Query(
		ctx,
		`
			SELECT
				balance_code_id,
				network_id,
				redeem_time,
				cancel_time
			FROM transfer_balance_code
			WHERE redeem_balance_id = $1
			ORDER BY balance_code_id
		`+forUpdate,
		balanceId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var code codeRow
			server.Raise(result.Scan(
				&code.balanceCodeId,
				&code.networkId,
				&code.redeemTime,
				&code.cancelTime,
			))
			codes = append(codes, code)
		}
	})
	plan.CodeCount = len(codes)
	if len(codes) == 1 {
		code := codes[0]
		move.BalanceCodeId = code.balanceCodeId
		if code.redeemTime != nil {
			move.RedeemTime = *code.redeemTime
		}
		plan.CodeNetworkId = code.networkId
		plan.CodeCancelled = code.cancelTime != nil
		plan.Move = move
	}

	plan.FromNetworkExists = redeemedBalanceMoveNetworkExists(ctx, tx, balanceNetworkId)
	plan.ToNetwork = readRedeemedBalanceMoveNetwork(ctx, tx, toNetworkId)
	if plan.ToNetwork != nil {
		plan.ToNetworkPro = loadProNetworkWithConn(ctx, tx, toNetworkId).pro
	}
	result, err = tx.Query(
		ctx,
		`
			SELECT
				(SELECT count(*) FROM transfer_escrow WHERE balance_id = $1),
				(SELECT count(*) FROM transfer_debit_journal WHERE balance_id = $1)
		`,
		balanceId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&plan.EscrowCount, &plan.DebitJournalCount))
		}
	})

	// A move already made: its audit event is on the target, and the balance
	// and its code are there.
	eventId := RedeemedBalanceMoveEventId(balanceId, toNetworkId)
	var movedTime time.Time
	var movedDetails *string
	movedFound := false
	result, err = tx.Query(
		ctx,
		`
			SELECT event_time, event_details
			FROM audit_network_event
			WHERE event_id = $1 AND network_id = $2 AND event_type = $3
		`,
		eventId,
		toNetworkId,
		AuditEventTypeRedeemedBalanceMoved,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			movedFound = true
			server.Raise(result.Scan(&movedTime, &movedDetails))
		}
	})
	if movedFound {
		plan.MovedEventId = &eventId
		plan.MovedTime = &movedTime
		audit := &redeemedBalanceMoveAudit{}
		if movedDetails == nil || json.Unmarshal([]byte(*movedDetails), audit) != nil || audit.Move == nil {
			plan.refuse("the audit event %s of the move to %s cannot be read", eventId, toNetworkId)
			return plan
		}
		if audit.Move.Digest() != audit.Digest || audit.Move.BalanceId != balanceId || audit.Move.ToNetworkId != toNetworkId {
			plan.refuse("the audit event %s of the move to %s does not match its digest", eventId, toNetworkId)
			return plan
		}
		plan.Move = audit.Move
		if balanceNetworkId != toNetworkId || plan.CodeNetworkId == nil || *plan.CodeNetworkId != toNetworkId ||
			plan.CodeCount != 1 || move.BalanceCodeId != audit.Move.BalanceCodeId {
			plan.refuse(
				"the balance was moved to %s at %s, but it and its code are no longer there",
				toNetworkId,
				movedTime.UTC().Format(time.RFC3339),
			)
			return plan
		}
		plan.State = RedeemedBalanceMoveStateMoved
		plan.Digest = audit.Digest
		return plan
	}

	switch len(codes) {
	case 0:
		plan.refuse("no balance code granted balance %s; only a redeemed code's balance moves", balanceId)
	case 1:
	default:
		plan.refuse("%d balance codes name balance %s", len(codes), balanceId)
	}
	if plan.CodeCancelled {
		plan.refuse("the balance code %s is cancelled", move.BalanceCodeId)
	}
	if len(codes) == 1 {
		switch {
		case plan.CodeNetworkId == nil:
			plan.refuse("the balance code %s does not record its network", move.BalanceCodeId)
		case *plan.CodeNetworkId != balanceNetworkId:
			plan.refuse("the balance code %s names network %s, but the balance is on network %s", move.BalanceCodeId, *plan.CodeNetworkId, balanceNetworkId)
		}
	}
	if grantKind != nil || purchaseToken {
		plan.refuse("the balance is a grant or a store purchase, not a code's balance")
	}
	switch {
	case balanceNetworkId == toNetworkId:
		plan.refuse("the balance is already on network %s", toNetworkId)
	case plan.FromNetworkExists:
		plan.refuse("the balance's network %s exists; only a balance left on a deleted network moves", balanceNetworkId)
	}
	if plan.ToNetwork == nil {
		plan.refuse("no network has id %s", toNetworkId)
	}
	if move.BalanceByteCount != move.StartBalanceByteCount || 0 < plan.EscrowCount || 0 < plan.DebitJournalCount {
		plan.refuse(
			"the balance is used: %d of %d bytes left, %d escrows, %d debit journal rows; only an unused balance moves",
			move.BalanceByteCount,
			move.StartBalanceByteCount,
			plan.EscrowCount,
			plan.DebitJournalCount,
		)
	}
	if !now.Before(move.EndTime) {
		plan.refuse("the balance expired at %s", move.EndTime.UTC().Format(time.RFC3339))
	}

	if len(plan.Refusals) == 0 {
		plan.State = RedeemedBalanceMoveStateMove
		plan.Digest = move.Digest()
	}
	return plan
}

func redeemedBalanceMoveNetworkExists(ctx context.Context, tx server.PgTx, networkId server.Id) (exists bool) {
	result, err := tx.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM network WHERE network_id = $1)`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&exists))
		}
	})
	return
}

// The network with its administrator's sign-ins, or nil when none has the id.
func readRedeemedBalanceMoveNetwork(ctx context.Context, tx server.PgTx, networkId server.Id) *RedeemedBalanceMoveNetwork {
	var network *RedeemedBalanceMoveNetwork
	result, err := tx.Query(
		ctx,
		`SELECT network_name, admin_user_id FROM network WHERE network_id = $1`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			network = &RedeemedBalanceMoveNetwork{
				NetworkId: networkId,
				SignIns:   []string{},
			}
			server.Raise(result.Scan(&network.NetworkName, &network.AdminUserId))
		}
	})
	if network == nil {
		return nil
	}
	result, err = tx.Query(
		ctx,
		`
			SELECT 'password ' || user_auth FROM network_user_auth_password WHERE user_id = $1
			UNION ALL
			SELECT auth_type || ' ' || user_auth FROM network_user_auth_sso WHERE user_id = $1
			UNION ALL
			SELECT 'wallet ' || blockchain FROM network_user_auth_wallet WHERE user_id = $1
			ORDER BY 1
		`,
		network.AdminUserId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var signIn string
			server.Raise(result.Scan(&signIn))
			network.SignIns = append(network.SignIns, signIn)
		}
	})
	return network
}
