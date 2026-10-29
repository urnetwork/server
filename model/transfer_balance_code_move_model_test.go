package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// `balance move-redeemed` (support inbox 764, owner decision 2026-10-29). The
// fixture is the case of the item: an AppSumo Tier 1 code (10 TiB over a
// 100-year window, US$40 net) redeemed through the redeem path on a network
// that was deleted afterwards, with the pro flag the July 2026 migration gave
// every paid balance redeemed before it.

const redeemedBalanceMoveTestByteCount = 10 * Tib

const redeemedBalanceMoveTestDuration = 36500 * 24 * time.Hour

type redeemedBalanceMoveFixture struct {
	sourceNetworkId server.Id
	targetNetworkId server.Id
	targetUserId    server.Id
	targetUserAuth  string
	code            *BalanceCode
	balanceId       server.Id
}

func redeemedBalanceMoveTestNetwork(ctx context.Context, name string) (networkId server.Id, userId server.Id, userAuth string) {
	networkId = server.NewId()
	userId = server.NewId()
	userAuth = Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-%s-%s", name, networkId), userId)
	return
}

func redeemedBalanceMoveTestExec(ctx context.Context, sql string, args ...any) (rowsAffected int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		rowsAffected = server.RaisePgResult(tx.Exec(ctx, sql, args...)).RowsAffected()
	})
	return
}

// Creates a code like the AppSumo codes and redeems it on the network through
// the redeem path. grandfatheredPro applies the July 2026 pro migration's
// rule to the new balance: a paid balance keeps pro = true.
func redeemedBalanceMoveTestRedeem(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	grandfatheredPro bool,
) (*BalanceCode, server.Id) {
	code, err := CreateBalanceCode(
		ctx,
		redeemedBalanceMoveTestByteCount,
		redeemedBalanceMoveTestDuration,
		UsdToNanoCents(40),
		"synthetic-move-"+server.NewId().String(),
		"",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RedeemBalanceCode(&RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: networkId}, ctx)
	if err != nil || result == nil || result.Error != nil || result.TransferBalance == nil {
		t.Fatal("the redeem path did not redeem the code", err, result)
	}
	balanceId := result.TransferBalance.TransferBalanceId
	if grandfatheredPro {
		if redeemedBalanceMoveTestExec(
			ctx,
			`
				UPDATE transfer_balance
				SET pro = true
				WHERE balance_id = $1 AND (0 < net_revenue_nano_cents OR 0 < subsidy_net_revenue_nano_cents)
			`,
			balanceId,
		) != 1 {
			t.Fatal("the code's balance carries no revenue")
		}
	}
	return code, balanceId
}

// Deletes the network and its administrator, as removing an account does.
func redeemedBalanceMoveTestDeleteNetwork(ctx context.Context, networkId server.Id) {
	redeemedBalanceMoveTestExec(
		ctx,
		`DELETE FROM network_user WHERE user_id = (SELECT admin_user_id FROM network WHERE network_id = $1)`,
		networkId,
	)
	redeemedBalanceMoveTestExec(ctx, `DELETE FROM network WHERE network_id = $1`, networkId)
}

func newRedeemedBalanceMoveFixture(
	t testing.TB,
	ctx context.Context,
	grandfatheredPro bool,
	deleteSource bool,
) *redeemedBalanceMoveFixture {
	f := &redeemedBalanceMoveFixture{}
	f.sourceNetworkId, _, _ = redeemedBalanceMoveTestNetwork(ctx, "move-source")
	f.targetNetworkId, f.targetUserId, f.targetUserAuth = redeemedBalanceMoveTestNetwork(ctx, "move-target")
	f.code, f.balanceId = redeemedBalanceMoveTestRedeem(t, ctx, f.sourceNetworkId, grandfatheredPro)
	if deleteSource {
		redeemedBalanceMoveTestDeleteNetwork(ctx, f.sourceNetworkId)
	}
	return f
}

// Every row of the tables a move may write, except the rows of the excluded
// ids, as text.
func redeemedBalanceMoveTestTables(ctx context.Context, exclude ...server.Id) string {
	if exclude == nil {
		exclude = []server.Id{}
	}
	b := &strings.Builder{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, query := range []string{
			`
				SELECT balance_id, network_id, start_time, end_time, start_balance_byte_count, balance_byte_count,
					net_revenue_nano_cents, subsidy_net_revenue_nano_cents, pro, grant_kind, purchase_token
				FROM transfer_balance
				WHERE NOT (balance_id = ANY($1::uuid[]))
				ORDER BY balance_id
			`,
			`
				SELECT balance_code_id, network_id, redeem_balance_id, redeem_time, start_time, end_time, cancel_time
				FROM transfer_balance_code
				WHERE NOT (balance_code_id = ANY($1::uuid[]))
				ORDER BY balance_code_id
			`,
			`
				SELECT event_id, event_time, network_id, event_type, event_details
				FROM audit_network_event
				WHERE NOT (event_id = ANY($1::uuid[]))
				ORDER BY event_id
			`,
			`
				SELECT balance_id, revision
				FROM transfer_balance_net_escrow_revision
				ORDER BY balance_id
			`,
		} {
			fmt.Fprintf(b, "--\n")
			var result server.PgResult
			var err error
			if strings.Contains(query, "$1") {
				result, err = conn.Query(ctx, query, exclude)
			} else {
				result, err = conn.Query(ctx, query)
			}
			server.WithPgResult(result, err, func() {
				for result.Next() {
					values, err := result.Values()
					server.Raise(err)
					fmt.Fprintf(b, "%v\n", values)
				}
			})
		}
	})
	return b.String()
}

type redeemedBalanceMoveTestBalanceRow struct {
	NetworkId             server.Id
	StartTime             time.Time
	EndTime               time.Time
	StartBalanceByteCount ByteCount
	BalanceByteCount      ByteCount
	NetRevenue            NanoCents
	SubsidyNetRevenue     NanoCents
	Pro                   bool
	Paid                  bool
	Active                bool
	GrantKind             *string
	PurchaseToken         *string
}

func redeemedBalanceMoveTestBalance(t testing.TB, ctx context.Context, balanceId server.Id) *redeemedBalanceMoveTestBalanceRow {
	var row *redeemedBalanceMoveTestBalanceRow
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, start_time, end_time, start_balance_byte_count, balance_byte_count,
					net_revenue_nano_cents, subsidy_net_revenue_nano_cents, pro, paid, active, grant_kind, purchase_token
				FROM transfer_balance
				WHERE balance_id = $1
			`,
			balanceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = &redeemedBalanceMoveTestBalanceRow{}
				server.Raise(result.Scan(
					&row.NetworkId,
					&row.StartTime,
					&row.EndTime,
					&row.StartBalanceByteCount,
					&row.BalanceByteCount,
					&row.NetRevenue,
					&row.SubsidyNetRevenue,
					&row.Pro,
					&row.Paid,
					&row.Active,
					&row.GrantKind,
					&row.PurchaseToken,
				))
			}
		})
	})
	if row == nil {
		t.Fatal("no balance", balanceId)
	}
	return row
}

type redeemedBalanceMoveTestCodeRow struct {
	NetworkId       *server.Id
	RedeemBalanceId *server.Id
	RedeemTime      *time.Time
	StartTime       time.Time
	EndTime         time.Time
	CancelTime      *time.Time
	Redeemed        bool
}

func redeemedBalanceMoveTestCode(t testing.TB, ctx context.Context, balanceCodeId server.Id) *redeemedBalanceMoveTestCodeRow {
	var row *redeemedBalanceMoveTestCodeRow
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, redeem_balance_id, redeem_time, start_time, end_time, cancel_time, redeemed
				FROM transfer_balance_code
				WHERE balance_code_id = $1
			`,
			balanceCodeId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = &redeemedBalanceMoveTestCodeRow{}
				server.Raise(result.Scan(
					&row.NetworkId,
					&row.RedeemBalanceId,
					&row.RedeemTime,
					&row.StartTime,
					&row.EndTime,
					&row.CancelTime,
					&row.Redeemed,
				))
			}
		})
	})
	if row == nil {
		t.Fatal("no balance code", balanceCodeId)
	}
	return row
}

// The move audit events on the network: their ids and details.
func redeemedBalanceMoveTestEvents(ctx context.Context, networkId server.Id) map[server.Id]string {
	events := map[server.Id]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT event_id, event_details
				FROM audit_network_event
				WHERE network_id = $1 AND event_type = $2
			`,
			networkId,
			AuditEventTypeRedeemedBalanceMoved,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var eventId server.Id
				var details *string
				server.Raise(result.Scan(&eventId, &details))
				if details != nil {
					events[eventId] = *details
				} else {
					events[eventId] = ""
				}
			}
		})
	})
	return events
}

// Tries one write; true when PostgreSQL refused it.
func redeemedBalanceMoveTestWriteRefused(ctx context.Context, networkId server.Id) (refused bool) {
	defer func() {
		refused = recover() != nil
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO audit_network_event (event_id, network_id, event_type) VALUES ($1, $2, 'synthetic_write_probe')`,
			server.NewId(),
			networkId,
		))
	}, server.OptNoRetry())
	return false
}

func redeemedBalanceMoveTestHasRefusal(plan *RedeemedBalanceMovePlan, refusal string) bool {
	if plan == nil {
		return false
	}
	for _, r := range plan.Refusals {
		if strings.Contains(r, refusal) {
			return true
		}
	}
	return false
}

// The move puts the balance and its code on the target, with every balance
// field unchanged, records its audit event in the same transaction, refreshes
// the target's Pro cache, and touches no other row.
func TestRedeemedBalanceMoveMovesTheBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newRedeemedBalanceMoveFixture(t, ctx, true, true)

		// balances that must not move: another code's balance left on another
		// deleted network, a code's balance on a live network, and a free grant
		// on the target
		other := newRedeemedBalanceMoveFixture(t, ctx, true, true)
		bystanderNetworkId, _, _ := redeemedBalanceMoveTestNetwork(ctx, "move-bystander")
		redeemedBalanceMoveTestRedeem(t, ctx, bystanderNetworkId, true)
		now := server.NowUtc()
		redeemedBalanceMoveTestExec(
			ctx,
			`
				INSERT INTO transfer_balance (
					balance_id, network_id, start_time, end_time, start_balance_byte_count,
					balance_byte_count, net_revenue_nano_cents, pro, grant_kind
				)
				VALUES ($1, $2, $3, $4, $5, $5, 0, false, 'free')
			`,
			server.NewId(),
			f.targetNetworkId,
			now,
			now.Add(24*time.Hour),
			32*Gib,
		)

		eventId := RedeemedBalanceMoveEventId(f.balanceId, f.targetNetworkId)
		othersBefore := redeemedBalanceMoveTestTables(ctx, f.balanceId, f.code.BalanceCodeId, eventId)
		balanceBefore := redeemedBalanceMoveTestBalance(t, ctx, f.balanceId)
		codeBefore := redeemedBalanceMoveTestCode(t, ctx, f.code.BalanceCodeId)
		if balanceBefore.NetworkId != f.sourceNetworkId || codeBefore.NetworkId == nil || *codeBefore.NetworkId != f.sourceNetworkId ||
			!balanceBefore.Pro || balanceBefore.BalanceByteCount != redeemedBalanceMoveTestByteCount {
			t.Fatalf("fixture: balance %+v code %+v", balanceBefore, codeBefore)
		}
		if IsProNetwork(ctx, f.targetNetworkId) {
			t.Fatal("fixture: the target is Pro before the move")
		}

		plan, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
		if err != nil {
			t.Fatal(err)
		}
		if plan.State != RedeemedBalanceMoveStateMove || len(plan.Refusals) != 0 || len(plan.Digest) != 64 {
			t.Fatalf("plan state %s refusals %v digest %q", plan.State, plan.Refusals, plan.Digest)
		}
		move := plan.Move
		if move == nil || move.BalanceId != f.balanceId || move.BalanceCodeId != f.code.BalanceCodeId ||
			move.FromNetworkId != f.sourceNetworkId || move.ToNetworkId != f.targetNetworkId ||
			!move.StartTime.Equal(balanceBefore.StartTime) || !move.EndTime.Equal(balanceBefore.EndTime) ||
			move.StartBalanceByteCount != redeemedBalanceMoveTestByteCount || move.BalanceByteCount != redeemedBalanceMoveTestByteCount ||
			move.NetRevenue != UsdToNanoCents(40) || move.SubsidyNetRevenue != 0 || !move.Pro ||
			codeBefore.RedeemTime == nil || !move.RedeemTime.Equal(*codeBefore.RedeemTime) {
			t.Fatalf("plan move %+v", move)
		}
		if plan.Digest != move.Digest() || plan.FromNetworkExists || plan.ToNetworkPro ||
			plan.EscrowCount != 0 || plan.DebitJournalCount != 0 || plan.CodeCount != 1 {
			t.Fatalf("plan facts %+v", plan)
		}
		if plan.ToNetwork == nil || plan.ToNetwork.NetworkId != f.targetNetworkId || plan.ToNetwork.AdminUserId != f.targetUserId ||
			len(plan.ToNetwork.SignIns) != 1 || plan.ToNetwork.SignIns[0] != "password "+f.targetUserAuth {
			t.Fatalf("plan target %+v", plan.ToNetwork)
		}

		moved, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, plan.Digest)
		if err != nil {
			t.Fatal(err)
		}
		if !moved.Committed || moved.Digest != plan.Digest || moved.AuditEventId == nil || *moved.AuditEventId != eventId ||
			moved.ToNetworkProAfter == nil || !*moved.ToNetworkProAfter || moved.ProCacheError != "" {
			t.Fatalf("commit %+v", moved)
		}

		// the balance and its code are on the target; nothing else about them changed
		balanceAfter := redeemedBalanceMoveTestBalance(t, ctx, f.balanceId)
		expectedBalance := *balanceBefore
		expectedBalance.NetworkId = f.targetNetworkId
		if *balanceAfter != expectedBalance {
			t.Fatalf("balance after the move %+v, expected %+v", balanceAfter, expectedBalance)
		}
		codeAfter := redeemedBalanceMoveTestCode(t, ctx, f.code.BalanceCodeId)
		if codeAfter.NetworkId == nil || *codeAfter.NetworkId != f.targetNetworkId ||
			codeAfter.RedeemBalanceId == nil || *codeAfter.RedeemBalanceId != f.balanceId || !codeAfter.Redeemed ||
			codeAfter.CancelTime != nil || !codeAfter.RedeemTime.Equal(*codeBefore.RedeemTime) ||
			!codeAfter.StartTime.Equal(codeBefore.StartTime) || !codeAfter.EndTime.Equal(codeBefore.EndTime) {
			t.Fatalf("code after the move %+v", codeAfter)
		}

		// the audit line
		events := redeemedBalanceMoveTestEvents(ctx, f.targetNetworkId)
		details, ok := events[eventId]
		if len(events) != 1 || !ok {
			t.Fatal("audit events on the target", events)
		}
		audit := &redeemedBalanceMoveAudit{}
		if err := json.Unmarshal([]byte(details), audit); err != nil {
			t.Fatal(err)
		}
		// the digest covers every field of the move
		if audit.Digest != plan.Digest || audit.Move == nil || audit.Move.Digest() != move.Digest() ||
			audit.Command != "bringyourctl balance move-redeemed" {
			t.Fatalf("audit details %s", details)
		}

		// no other row changed
		if othersAfter := redeemedBalanceMoveTestTables(ctx, f.balanceId, f.code.BalanceCodeId, eventId); othersAfter != othersBefore {
			t.Fatalf("the move changed other rows:\nbefore\n%s\nafter\n%s", othersBefore, othersAfter)
		}
		if otherBalance := redeemedBalanceMoveTestBalance(t, ctx, other.balanceId); otherBalance.NetworkId != other.sourceNetworkId {
			t.Fatal("another deleted network's balance moved")
		}

		// the target is Pro at once, in both cache tiers
		localPro, localOk, cachedPro, cachedOk := Testing_ProNetworkCacheEntries(ctx, f.targetNetworkId)
		if !localOk || !localPro || !cachedOk || !cachedPro {
			t.Fatal("the target's Pro cache was not refreshed", localPro, localOk, cachedPro, cachedOk)
		}
		if !IsProNetwork(ctx, f.targetNetworkId) || !loadProNetwork(ctx, f.targetNetworkId).pro {
			t.Fatal("the target is not Pro after the move")
		}
	})
}

// The dry run writes nothing: not a row, and not the Pro cache. It reads in
// a read-only transaction, and runs in a process whose sessions are read
// only.
func TestRedeemedBalanceMoveDryRunWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newRedeemedBalanceMoveFixture(t, ctx, true, true)
		before := redeemedBalanceMoveTestTables(ctx)

		plan, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
		if err != nil || plan.State != RedeemedBalanceMoveStateMove {
			t.Fatal("dry run", err, plan)
		}
		if after := redeemedBalanceMoveTestTables(ctx); after != before {
			t.Fatalf("the dry run wrote:\nbefore\n%s\nafter\n%s", before, after)
		}
		if _, localOk, _, cachedOk := Testing_ProNetworkCacheEntries(ctx, f.targetNetworkId); localOk || cachedOk {
			t.Fatal("the dry run filled the target's Pro cache")
		}

		// the command's dry run process: every session is read only
		server.RequirePgReadOnly()
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		readOnlyPlan, readOnlyErr := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
		writeRefused := redeemedBalanceMoveTestWriteRefused(ctx, f.targetNetworkId)
		server.Testing_ReleasePgReadOnly()
		if readOnlyErr != nil || readOnlyPlan.State != RedeemedBalanceMoveStateMove {
			t.Fatal("the dry run needs a write", readOnlyErr, readOnlyPlan)
		}
		if !writeRefused {
			t.Fatal("the dry run's process accepted a write")
		}
		if readOnlyPlan.Digest != plan.Digest {
			t.Fatal("a second dry run has another digest", plan.Digest, readOnlyPlan.Digest)
		}
		if after := redeemedBalanceMoveTestTables(ctx); after != before {
			t.Fatalf("the read-only dry run wrote:\nbefore\n%s\nafter\n%s", before, after)
		}
		if balance := redeemedBalanceMoveTestBalance(t, ctx, f.balanceId); balance.NetworkId != f.sourceNetworkId {
			t.Fatal("the dry run moved the balance")
		}
	})
}

// A commit run again, with the same digest, finds the move it made and
// writes nothing, also after the target has used the balance. Another digest
// is refused, and writes nothing either.
func TestRedeemedBalanceMoveRerunIsANoOp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newRedeemedBalanceMoveFixture(t, ctx, true, true)
		eventId := RedeemedBalanceMoveEventId(f.balanceId, f.targetNetworkId)

		plan, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
		if err != nil || plan.State != RedeemedBalanceMoveStateMove {
			t.Fatal("dry run", err, plan)
		}
		first, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, plan.Digest)
		if err != nil || !first.Committed {
			t.Fatal("first commit", err, first)
		}
		afterFirst := redeemedBalanceMoveTestTables(ctx)

		again, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, plan.Digest)
		if err != nil {
			t.Fatal("the rerun failed", err)
		}
		if again.Committed || again.AuditEventId != nil || again.State != RedeemedBalanceMoveStateMoved ||
			again.Digest != plan.Digest || again.MovedEventId == nil || *again.MovedEventId != eventId ||
			again.Move == nil || again.Move.Digest() != plan.Move.Digest() {
			t.Fatalf("rerun %+v", again)
		}
		if after := redeemedBalanceMoveTestTables(ctx); after != afterFirst {
			t.Fatalf("the rerun wrote:\nbefore\n%s\nafter\n%s", afterFirst, after)
		}

		// a dry run after the move shows it, with the same digest
		dryRun, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
		if err != nil || dryRun.State != RedeemedBalanceMoveStateMoved || dryRun.Digest != plan.Digest || dryRun.MovedTime == nil {
			t.Fatal("dry run after the move", err, dryRun)
		}

		// another digest is refused
		mismatch, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, strings.Repeat("0", 64))
		if !errors.Is(err, ErrRedeemedBalanceMoveDigestMismatch) || mismatch == nil || mismatch.Committed {
			t.Fatal("a rerun with another digest", err, mismatch)
		}
		if after := redeemedBalanceMoveTestTables(ctx); after != afterFirst {
			t.Fatalf("the refused rerun wrote:\nbefore\n%s\nafter\n%s", afterFirst, after)
		}

		// the target uses the balance; a rerun still writes nothing
		redeemedBalanceMoveTestExec(
			ctx,
			`UPDATE transfer_balance SET balance_byte_count = balance_byte_count - 1024 WHERE balance_id = $1`,
			f.balanceId,
		)
		afterUse := redeemedBalanceMoveTestTables(ctx)
		third, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, plan.Digest)
		if err != nil || third.Committed || third.State != RedeemedBalanceMoveStateMoved || third.Digest != plan.Digest {
			t.Fatal("a rerun after use", err, third)
		}
		if after := redeemedBalanceMoveTestTables(ctx); after != afterUse {
			t.Fatalf("the rerun after use wrote:\nbefore\n%s\nafter\n%s", afterUse, after)
		}
		if events := redeemedBalanceMoveTestEvents(ctx, f.targetNetworkId); len(events) != 1 {
			t.Fatal("audit events", events)
		}
	})
}

// Every refusal: the dry run reports it, and a commit refuses it with the
// digest of a dry run taken before the change, writing nothing.
func TestRedeemedBalanceMoveRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		type refusalCase struct {
			name string
			// the balance's network is not deleted
			keepSource bool
			// changes the fixture after the dry run that could move, and
			// returns the balance and target to move
			change  func(f *redeemedBalanceMoveFixture) (server.Id, server.Id)
			refusal string
		}
		cases := []refusalCase{
			{
				name: "no such balance",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					return server.NewId(), f.targetNetworkId
				},
				refusal: "no balance has id",
			},
			{
				name: "no code granted the balance",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					// a balance on a deleted network that no code names
					balanceId := server.NewId()
					now := server.NowUtc()
					redeemedBalanceMoveTestExec(
						ctx,
						`
							INSERT INTO transfer_balance (
								balance_id, network_id, start_time, end_time, start_balance_byte_count,
								balance_byte_count, net_revenue_nano_cents, pro
							)
							VALUES ($1, $2, $3, $4, $5, $5, $6, true)
						`,
						balanceId,
						f.sourceNetworkId,
						now,
						now.Add(redeemedBalanceMoveTestDuration),
						redeemedBalanceMoveTestByteCount,
						UsdToNanoCents(40),
					)
					return balanceId, f.targetNetworkId
				},
				refusal: "no balance code granted balance",
			},
			{
				name: "two codes name the balance",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					second, err := CreateBalanceCode(ctx, Gib, 24*time.Hour, 1, "synthetic-move-"+server.NewId().String(), "", "")
					server.Raise(err)
					redeemedBalanceMoveTestExec(
						ctx,
						`UPDATE transfer_balance_code SET redeem_balance_id = $2, redeem_time = $3, network_id = $4 WHERE balance_code_id = $1`,
						second.BalanceCodeId,
						f.balanceId,
						server.NowUtc(),
						f.sourceNetworkId,
					)
					return f.balanceId, f.targetNetworkId
				},
				refusal: "2 balance codes name balance",
			},
			{
				name: "the code is cancelled",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(ctx, `UPDATE transfer_balance_code SET cancel_time = $2 WHERE balance_code_id = $1`, f.code.BalanceCodeId, server.NowUtc())
					return f.balanceId, f.targetNetworkId
				},
				refusal: "is cancelled",
			},
			{
				name: "the code does not record its network",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(ctx, `UPDATE transfer_balance_code SET network_id = NULL WHERE balance_code_id = $1`, f.code.BalanceCodeId)
					return f.balanceId, f.targetNetworkId
				},
				refusal: "does not record its network",
			},
			{
				name: "the code names another network",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(ctx, `UPDATE transfer_balance_code SET network_id = $2 WHERE balance_code_id = $1`, f.code.BalanceCodeId, server.NewId())
					return f.balanceId, f.targetNetworkId
				},
				refusal: "but the balance is on network",
			},
			{
				name: "the balance is a recurring grant",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(ctx, `UPDATE transfer_balance SET grant_kind = 'free' WHERE balance_id = $1`, f.balanceId)
					return f.balanceId, f.targetNetworkId
				},
				refusal: "not a code's balance",
			},
			{
				name: "the balance is a store purchase",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(ctx, `UPDATE transfer_balance SET purchase_token = 'synthetic-token' WHERE balance_id = $1`, f.balanceId)
					return f.balanceId, f.targetNetworkId
				},
				refusal: "not a code's balance",
			},
			{
				name:       "the balance's network exists",
				keepSource: true,
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					return f.balanceId, f.targetNetworkId
				},
				refusal: "exists; only a balance left on a deleted network moves",
			},
			{
				name:       "the balance is already on the target",
				keepSource: true,
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					return f.balanceId, f.sourceNetworkId
				},
				refusal: "is already on network",
			},
			{
				name: "no such target network",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					return f.balanceId, server.NewId()
				},
				refusal: "no network has id",
			},
			{
				name: "the target was deleted after the dry run",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestDeleteNetwork(ctx, f.targetNetworkId)
					return f.balanceId, f.targetNetworkId
				},
				refusal: "no network has id",
			},
			{
				name: "bytes were spent",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(ctx, `UPDATE transfer_balance SET balance_byte_count = balance_byte_count - 1 WHERE balance_id = $1`, f.balanceId)
					return f.balanceId, f.targetNetworkId
				},
				refusal: "the balance is used",
			},
			{
				name: "an escrow holds the balance",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(
						ctx,
						`INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count) VALUES ($1, $2, 1024)`,
						server.NewId(),
						f.balanceId,
					)
					return f.balanceId, f.targetNetworkId
				},
				refusal: "the balance is used",
			},
			{
				name: "a debit is journaled",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(
						ctx,
						`
							INSERT INTO transfer_debit_journal (contract_id, balance_id, shard, debit_byte_count)
							VALUES ($1, $2, get_byte(uuid_send($2::uuid), 15) % 16, 1024)
						`,
						server.NewId(),
						f.balanceId,
					)
					return f.balanceId, f.targetNetworkId
				},
				refusal: "the balance is used",
			},
			{
				name: "the balance expired",
				change: func(f *redeemedBalanceMoveFixture) (server.Id, server.Id) {
					redeemedBalanceMoveTestExec(ctx, `UPDATE transfer_balance SET end_time = $2 WHERE balance_id = $1`, f.balanceId, server.NowUtc().Add(-time.Hour))
					return f.balanceId, f.targetNetworkId
				},
				refusal: "the balance expired",
			},
		}

		for _, c := range cases {
			f := newRedeemedBalanceMoveFixture(t, ctx, true, !c.keepSource)
			// what an operator would commit with
			expect := strings.Repeat("0", 64)
			if !c.keepSource {
				valid, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
				if err != nil || valid.State != RedeemedBalanceMoveStateMove {
					t.Fatal(c.name, "fixture", err, valid)
				}
				expect = valid.Digest
			}
			balanceId, toNetworkId := c.change(f)
			before := redeemedBalanceMoveTestTables(ctx)

			plan, err := PlanRedeemedBalanceMove(ctx, balanceId, toNetworkId)
			if err != nil || plan.State != RedeemedBalanceMoveStateRefused || plan.Digest != "" ||
				!redeemedBalanceMoveTestHasRefusal(plan, c.refusal) {
				t.Fatalf("%s: dry run %v %+v", c.name, err, plan)
			}
			committed, err := CommitRedeemedBalanceMove(ctx, balanceId, toNetworkId, expect)
			if !errors.Is(err, ErrRedeemedBalanceMoveRefused) || committed == nil || committed.Committed ||
				committed.State != RedeemedBalanceMoveStateRefused || !redeemedBalanceMoveTestHasRefusal(committed, c.refusal) {
				t.Fatalf("%s: commit %v %+v", c.name, err, committed)
			}
			if after := redeemedBalanceMoveTestTables(ctx); after != before {
				t.Fatalf("%s: the refused commit wrote:\nbefore\n%s\nafter\n%s", c.name, before, after)
			}
		}

		// a zero id
		f := newRedeemedBalanceMoveFixture(t, ctx, true, true)
		before := redeemedBalanceMoveTestTables(ctx)
		for _, ids := range [][2]server.Id{{{}, f.targetNetworkId}, {f.balanceId, {}}} {
			committed, err := CommitRedeemedBalanceMove(ctx, ids[0], ids[1], strings.Repeat("0", 64))
			if !errors.Is(err, ErrRedeemedBalanceMoveRefused) || !redeemedBalanceMoveTestHasRefusal(committed, "zero id") {
				t.Fatal("a zero id", err, committed)
			}
		}
		if after := redeemedBalanceMoveTestTables(ctx); after != before {
			t.Fatal("a refused zero id wrote")
		}
	})
}

// The digest binds what the move carries: a commit refuses the digest of a
// dry run when the balance changed since in a way that passes every check.
func TestRedeemedBalanceMoveDigestBindsTheBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, change := range []string{
			`UPDATE transfer_balance SET end_time = end_time - interval '1 day' WHERE balance_id = $1`,
			`UPDATE transfer_balance SET pro = false WHERE balance_id = $1`,
			`UPDATE transfer_balance SET net_revenue_nano_cents = net_revenue_nano_cents - 1 WHERE balance_id = $1`,
			`UPDATE transfer_balance SET start_balance_byte_count = start_balance_byte_count - 1, balance_byte_count = balance_byte_count - 1 WHERE balance_id = $1`,
		} {
			f := newRedeemedBalanceMoveFixture(t, ctx, true, true)
			plan, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
			if err != nil || plan.State != RedeemedBalanceMoveStateMove {
				t.Fatal("dry run", err, plan)
			}
			redeemedBalanceMoveTestExec(ctx, change, f.balanceId)
			before := redeemedBalanceMoveTestTables(ctx)
			committed, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, plan.Digest)
			if !errors.Is(err, ErrRedeemedBalanceMoveDigestMismatch) || committed == nil || committed.Committed ||
				committed.State != RedeemedBalanceMoveStateMove || committed.Digest == plan.Digest {
				t.Fatal(change, err, committed)
			}
			if after := redeemedBalanceMoveTestTables(ctx); after != before {
				t.Fatal(change, "the refused commit wrote")
			}
		}
	})
}

// A balance moved on again, after its first target was deleted: the first
// commit run again finds its audit event, but the balance is gone from that
// target, so it refuses and writes nothing.
func TestRedeemedBalanceMoveRefusesARerunAfterTheBalanceMovedOn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newRedeemedBalanceMoveFixture(t, ctx, true, true)
		plan, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
		if err != nil || plan.State != RedeemedBalanceMoveStateMove {
			t.Fatal("dry run", err, plan)
		}
		if moved, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, plan.Digest); err != nil || !moved.Committed {
			t.Fatal("first move", err, moved)
		}

		redeemedBalanceMoveTestDeleteNetwork(ctx, f.targetNetworkId)
		nextNetworkId, _, _ := redeemedBalanceMoveTestNetwork(ctx, "move-next")
		next, err := PlanRedeemedBalanceMove(ctx, f.balanceId, nextNetworkId)
		if err != nil || next.State != RedeemedBalanceMoveStateMove || next.Move.FromNetworkId != f.targetNetworkId {
			t.Fatal("dry run of the second move", err, next)
		}
		if moved, err := CommitRedeemedBalanceMove(ctx, f.balanceId, nextNetworkId, next.Digest); err != nil || !moved.Committed {
			t.Fatal("second move", err, moved)
		}
		if balance := redeemedBalanceMoveTestBalance(t, ctx, f.balanceId); balance.NetworkId != nextNetworkId {
			t.Fatal("the second move did not move the balance")
		}

		before := redeemedBalanceMoveTestTables(ctx)
		rerun, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, plan.Digest)
		if !errors.Is(err, ErrRedeemedBalanceMoveRefused) || !redeemedBalanceMoveTestHasRefusal(rerun, "no longer there") {
			t.Fatal("rerun of the first move", err, rerun)
		}
		if after := redeemedBalanceMoveTestTables(ctx); after != before {
			t.Fatal("the refused rerun wrote")
		}
	})
}

// The target ends up as a network that redeemed the code itself: the same
// balance, the same listed code, the same summary and the same Pro
// entitlement as a network where an identical code was redeemed through the
// redeem path, and the source keeps nothing. Both for a code redeemed before
// July 2026 (pro, grandfathered) and for a data-only code.
func TestRedeemedBalanceMoveEquivalentToAFreshRedeem(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, grandfatheredPro := range []bool{true, false} {
			f := newRedeemedBalanceMoveFixture(t, ctx, grandfatheredPro, true)
			freshNetworkId, freshUserId, _ := redeemedBalanceMoveTestNetwork(ctx, "move-fresh")
			freshCode, freshBalanceId := redeemedBalanceMoveTestRedeem(t, ctx, freshNetworkId, grandfatheredPro)

			plan, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
			if err != nil || plan.State != RedeemedBalanceMoveStateMove {
				t.Fatal("dry run", err, plan)
			}
			committed, err := CommitRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId, plan.Digest)
			if err != nil || !committed.Committed || committed.ToNetworkProAfter == nil || *committed.ToNetworkProAfter != grandfatheredPro {
				t.Fatal("commit", err, committed)
			}

			// the balance rows
			moved := redeemedBalanceMoveTestBalance(t, ctx, f.balanceId)
			fresh := redeemedBalanceMoveTestBalance(t, ctx, freshBalanceId)
			if moved.NetworkId != f.targetNetworkId || fresh.NetworkId != freshNetworkId {
				t.Fatal("networks", moved.NetworkId, fresh.NetworkId)
			}
			if moved.EndTime.Sub(moved.StartTime) != fresh.EndTime.Sub(fresh.StartTime) ||
				moved.StartBalanceByteCount != fresh.StartBalanceByteCount || moved.BalanceByteCount != fresh.BalanceByteCount ||
				moved.NetRevenue != fresh.NetRevenue || moved.SubsidyNetRevenue != fresh.SubsidyNetRevenue ||
				moved.Pro != fresh.Pro || moved.Pro != grandfatheredPro || moved.Paid != fresh.Paid || moved.Active != fresh.Active ||
				moved.GrantKind != nil || fresh.GrantKind != nil || moved.PurchaseToken != nil || fresh.PurchaseToken != nil {
				t.Fatalf("pro %t: moved balance %+v, fresh %+v", grandfatheredPro, moved, fresh)
			}

			// the code rows
			movedCode := redeemedBalanceMoveTestCode(t, ctx, f.code.BalanceCodeId)
			freshCodeRow := redeemedBalanceMoveTestCode(t, ctx, freshCode.BalanceCodeId)
			if movedCode.NetworkId == nil || *movedCode.NetworkId != f.targetNetworkId || !movedCode.Redeemed ||
				movedCode.RedeemBalanceId == nil || *movedCode.RedeemBalanceId != f.balanceId || movedCode.CancelTime != nil ||
				freshCodeRow.NetworkId == nil || *freshCodeRow.NetworkId != freshNetworkId || !freshCodeRow.Redeemed ||
				freshCodeRow.RedeemBalanceId == nil || *freshCodeRow.RedeemBalanceId != freshBalanceId || freshCodeRow.CancelTime != nil ||
				movedCode.EndTime.Sub(movedCode.StartTime) != freshCodeRow.EndTime.Sub(freshCodeRow.StartTime) {
				t.Fatalf("pro %t: moved code %+v, fresh %+v", grandfatheredPro, movedCode, freshCodeRow)
			}

			// the redeemed codes each network lists
			targetSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(f.targetNetworkId, f.targetUserId, "a", false, false))
			freshSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(freshNetworkId, freshUserId, "a", false, false))
			sourceSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(f.sourceNetworkId, server.NewId(), "a", false, false))
			movedCodes, err := FetchNetworkRedeemedBalanceCodes(targetSession)
			if err != nil {
				t.Fatal(err)
			}
			freshCodes, err := FetchNetworkRedeemedBalanceCodes(freshSession)
			if err != nil {
				t.Fatal(err)
			}
			sourceCodes, err := FetchNetworkRedeemedBalanceCodes(sourceSession)
			if err != nil {
				t.Fatal(err)
			}
			if len(movedCodes) != 1 || len(freshCodes) != 1 || len(sourceCodes) != 0 ||
				movedCodes[0].BalanceCodeId != f.code.BalanceCodeId || freshCodes[0].BalanceCodeId != freshCode.BalanceCodeId ||
				movedCodes[0].BalanceByteCount != freshCodes[0].BalanceByteCount || movedCodes[0].Secret != f.code.Secret {
				t.Fatalf("pro %t: listed codes %+v, fresh %+v, source %+v", grandfatheredPro, movedCodes, freshCodes, sourceCodes)
			}

			// the balances and summary each network reads
			movedBalances := GetActiveTransferBalances(ctx, f.targetNetworkId)
			freshBalances := GetActiveTransferBalances(ctx, freshNetworkId)
			if len(movedBalances) != 1 || len(freshBalances) != 1 || len(GetActiveTransferBalances(ctx, f.sourceNetworkId)) != 0 {
				t.Fatal("active balances", len(movedBalances), len(freshBalances))
			}
			movedActive, freshActive := *movedBalances[0], *freshBalances[0]
			if movedActive.BalanceId != f.balanceId || freshActive.BalanceId != freshBalanceId {
				t.Fatal("active balance ids")
			}
			movedActive.BalanceId, movedActive.NetworkId, movedActive.StartTime, movedActive.EndTime = server.Id{}, server.Id{}, time.Time{}, time.Time{}
			freshActive.BalanceId, freshActive.NetworkId, freshActive.StartTime, freshActive.EndTime = server.Id{}, server.Id{}, time.Time{}, time.Time{}
			if fmt.Sprintf("%+v", movedActive) != fmt.Sprintf("%+v", freshActive) {
				t.Fatalf("pro %t: active balance %+v, fresh %+v", grandfatheredPro, movedActive, freshActive)
			}
			movedSummary := GetTransferBalanceSummary(ctx, f.targetNetworkId)
			freshSummary := GetTransferBalanceSummary(ctx, freshNetworkId)
			if movedSummary.BalanceByteCount != freshSummary.BalanceByteCount ||
				movedSummary.StartBalanceByteCount != freshSummary.StartBalanceByteCount ||
				movedSummary.OpenTransferByteCount != freshSummary.OpenTransferByteCount ||
				movedSummary.BalanceByteCount != redeemedBalanceMoveTestByteCount {
				t.Fatalf("pro %t: summary %+v, fresh %+v", grandfatheredPro, movedSummary, freshSummary)
			}
			if !HasActiveTransferBalance(ctx, f.targetNetworkId, redeemedBalanceMoveTestByteCount) ||
				!HasActiveTransferBalance(ctx, freshNetworkId, redeemedBalanceMoveTestByteCount) {
				t.Fatal("readiness")
			}

			// Pro included: the target is Pro exactly when the network that
			// redeemed the code itself is, through the cache and in the
			// database, and the source keeps no entitlement
			if IsProNetwork(ctx, f.targetNetworkId) != grandfatheredPro || IsProNetwork(ctx, freshNetworkId) != grandfatheredPro ||
				loadProNetwork(ctx, f.targetNetworkId).pro != grandfatheredPro || loadProNetwork(ctx, freshNetworkId).pro != grandfatheredPro ||
				loadProNetwork(ctx, f.sourceNetworkId).pro {
				t.Fatal("pro", grandfatheredPro)
			}
		}
	})
}
