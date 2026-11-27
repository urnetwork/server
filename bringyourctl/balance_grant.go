// `bringyourctl balance grant` gives one network a temporary data grant: one
// transfer_balance row from now to --end, at no cost, written by the grant
// writer the daily free grant and the referral bonus use
// (model.AddGrantTransferBalanceInTx). Owner decision 2026-11-25: a payer whose
// daily free grant open contracts hold in full gets a stopgap grant until its
// next daily grant, while the code that billed those contracts to it is fixed.
//
// The row is recorded as a free grant (model.GrantKindFree), the kind of the
// daily grant whose shortfall it covers. grant_kind has no check or enum, and
// the existing kind needs no migration. The kind is read only by the balance
// summary (model.SupersededGrants): a free grant that ends at the daily grant's
// end hour is listed until the next daily grant starts, then left out of the
// summary for the grace hour, as the daily grant itself is. A free grant is
// never Pro, and it is unpaid: pro = false, and paid = false because both of
// its revenues are zero.
//
// Without --commit the command is a dry run in a read-only process, so
// PostgreSQL refuses any write on its path. It prints the network's active
// balances with what open contracts reserve from them, the payer's open escrow
// total, the row a commit inserts, and the digest a commit must name. The
// digest binds the inputs (network, bytes, end, kind and flags) and the
// durable identity of each active balance of the network: its id, window,
// start bytes, kind and flags. It does not bind the bytes left or the open
// escrow, which the payer's traffic moves every second, so a digest over them
// would never match its commit. A balance that starts, ends or drains between
// the dry run and the commit changes the digest, and so does a grant this
// command already wrote, so a stale dry run or a repeated commit writes
// nothing.
//
// A commit takes the lock every data writer takes before a network's deletion
// (model.LockPaymentNetworkInTx), reads the active balances again in its
// transaction, and refuses unless the digest is the expected one. It then
// inserts the grant, starting at the commit's time, and an audit_network_event
// in one transaction.
//
// No cache stands between the new row and contract admission, and no grant
// writer invalidates one except the Pro grant's Pro cache, which this grant
// does not change. The next contract request reads the payer's grants from
// PostgreSQL. A payer that a process refused as exhausted in the last few
// seconds first runs the exhaustion proof (model/subscription_redis_exhaustion.go),
// which defers to the full census as soon as a grant has no Redis counter yet
// or room for the request. The reservation script then starts the new grant's
// counter from its durable credit. So the grant funds the payer's next request.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The audit_network_event type of a grant, recorded on the granted network.
const auditEventTypeBalanceGranted model.AuditEventType = "balance_granted"

// The kind the grant is recorded as (see the file comment).
const balanceGrantKind = model.GrantKindFree

const (
	// the grant can be written
	balanceGrantStateGrant = "grant"
	// a check failed; nothing will be written
	balanceGrantStateRefused = "refused"
)

var errBalanceGrantRefused = errors.New("balance grant: refused; nothing was written")

var errBalanceGrantDigestMismatch = errors.New("balance grant: --expect is not the digest of this grant and the network's active balances now; nothing was written; run the dry run again")

// One active balance of the network. The digest binds every field except the
// bytes left.
type balanceGrantActiveBalance struct {
	BalanceId             server.Id       `json:"balance_id"`
	StartTime             time.Time       `json:"start_time"`
	EndTime               time.Time       `json:"end_time"`
	StartBalanceByteCount model.ByteCount `json:"start_balance_byte_count"`
	// the durable bytes left, which settlement moves: printed, not digested
	BalanceByteCount model.ByteCount `json:"balance_byte_count"`
	Paid             bool            `json:"paid"`
	Pro              bool            `json:"pro"`
	GrantKind        model.GrantKind `json:"grant_kind"`
}

type balanceGrantPlan struct {
	Time      time.Time
	NetworkId server.Id
	State     string
	// why the grant is refused; empty unless the state is refused
	Refusals []string

	NetworkExists    bool
	BalanceByteCount model.ByteCount
	EndTime          time.Time
	// ordered by end, start and id, as the census reads them
	ActiveBalances []*balanceGrantActiveBalance
	// set when the state is grant
	Digest string

	// what the balance reads show (model.GetTransferBalanceSummary): each
	// active balance's available bytes and the payer's open escrow. Read
	// outside the plan's transaction, for the operator only.
	Summary *model.TransferBalanceSummary

	// a commit: whether this run wrote the grant, the row and its audit
	// event, and the balance reads after the commit
	Committed    bool
	BalanceId    *server.Id
	StartTime    time.Time
	AuditEventId *server.Id
	SummaryAfter *model.TransferBalanceSummary
	SummaryError string
}

func (self *balanceGrantPlan) refuse(format string, args ...any) {
	self.Refusals = append(self.Refusals, fmt.Sprintf(format, args...))
}

// The audit_network_event details of a grant.
type balanceGrantAudit struct {
	Command          string          `json:"command"`
	Digest           string          `json:"digest"`
	BalanceId        server.Id       `json:"balance_id"`
	BalanceByteCount model.ByteCount `json:"balance_byte_count"`
	StartTime        time.Time       `json:"start_time"`
	EndTime          time.Time       `json:"end_time"`
	GrantKind        model.GrantKind `json:"grant_kind"`
}

func balanceGrantCommand(opts docopt.Opts, stdout io.Writer, stderr io.Writer) int {
	networkIdValue, _ := opts.String("--network_id")
	balanceValue, _ := opts.String("--balance")
	endValue, _ := opts.String("--end")
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	// The dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path.
	if !commit {
		server.RequirePgReadOnly()
	}
	return runBalanceGrant(context.Background(), stdout, stderr, networkIdValue, balanceValue, endValue, commit, expect)
}

// Prints the plan and returns the exit code: 0 for a dry run that can grant
// and for a commit that granted; 1 for a refusal or a failure, which write
// nothing; 2 for an argument that does not parse. A dry run that can grant
// ends with the command that grants exactly what it printed.
func runBalanceGrant(
	ctx context.Context,
	stdout io.Writer,
	stderr io.Writer,
	networkIdValue string,
	balanceValue string,
	endValue string,
	commit bool,
	expect string,
) int {
	networkId, err := server.ParseId(networkIdValue)
	if err != nil {
		fmt.Fprintf(stderr, "balance grant: --network_id %q: %s\n", networkIdValue, err)
		return 2
	}
	balanceByteCount, err := model.ParseByteCount(balanceValue)
	if err != nil {
		fmt.Fprintf(stderr, "balance grant: --balance %q: %s\n", balanceValue, err)
		return 2
	}
	endTime, err := time.Parse(time.RFC3339, endValue)
	if err != nil {
		fmt.Fprintf(stderr, "balance grant: --end %q: %s\n", endValue, err)
		return 2
	}
	// the database's time domain: UTC, in microseconds
	endTime = endTime.UTC().Truncate(time.Microsecond)

	var plan *balanceGrantPlan
	if commit {
		plan, err = commitBalanceGrant(ctx, networkId, balanceByteCount, endTime, expect)
	} else {
		plan, err = planBalanceGrant(ctx, networkId, balanceByteCount, endTime)
	}
	if plan != nil {
		fmt.Fprint(stdout, balanceGrantText(plan, commit))
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	if plan == nil {
		fmt.Fprintf(stderr, "balance grant: no plan; nothing was written\n")
		return 1
	}
	if plan.State == balanceGrantStateRefused {
		fmt.Fprintf(stderr, "%s\n", errBalanceGrantRefused)
		return 1
	}
	if commit {
		if !plan.Committed || plan.BalanceId == nil || plan.AuditEventId == nil {
			fmt.Fprintf(stderr, "balance grant: the commit reported no grant\n")
			return 1
		}
		// the audit line, also in the process log
		line := fmt.Sprintf(
			"audit %s event_id=%s network_id=%s balance_id=%s balance_byte_count=%d start_time=%s end_time=%s grant_kind=%s digest=%s",
			auditEventTypeBalanceGranted,
			*plan.AuditEventId,
			plan.NetworkId,
			*plan.BalanceId,
			plan.BalanceByteCount,
			plan.StartTime.UTC().Format(time.RFC3339Nano),
			plan.EndTime.UTC().Format(time.RFC3339Nano),
			balanceGrantKind,
			plan.Digest,
		)
		fmt.Fprintf(stdout, "%s\n", line)
		glog.Infof("[balance]%s\n", line)
		return 0
	}
	fmt.Fprintf(
		stdout,
		"to grant exactly this: bringyourctl balance grant --network_id=%s --balance=%s --end=%s --commit --expect=%s\n",
		plan.NetworkId,
		balanceValue,
		plan.EndTime.Format(time.RFC3339Nano),
		plan.Digest,
	)
	return 0
}

// The dry run: the balance reads, then the plan in one read-only transaction.
func planBalanceGrant(
	ctx context.Context,
	networkId server.Id,
	balanceByteCount model.ByteCount,
	endTime time.Time,
) (plan *balanceGrantPlan, returnErr error) {
	server.HandleError(func() {
		summary := model.GetTransferBalanceSummary(ctx, networkId)
		server.Tx(ctx, func(tx server.PgTx) {
			plan = readBalanceGrantPlan(ctx, tx, networkId, balanceByteCount, endTime, server.NowUtc())
		}, pgx.ReadOnly)
		plan.Summary = summary
	}, func(err error) {
		returnErr = err
	})
	return
}

// Writes the grant when its digest is expect, and reads the balances again
// after the commit. Returns the plan it read under the lock, and an error when
// it refuses or fails, in which case nothing was written.
func commitBalanceGrant(
	ctx context.Context,
	networkId server.Id,
	balanceByteCount model.ByteCount,
	endTime time.Time,
	expect string,
) (plan *balanceGrantPlan, returnErr error) {
	server.HandleError(func() {
		summary := model.GetTransferBalanceSummary(ctx, networkId)
		server.Tx(ctx, func(tx server.PgTx) {
			plan, returnErr = commitBalanceGrantInTx(ctx, tx, networkId, balanceByteCount, endTime, expect, server.NowUtc())
		}, server.TxReadCommitted, server.OptNoRetry())
		if plan != nil {
			plan.Summary = summary
		}
	}, func(err error) {
		if plan != nil {
			plan.Committed = false
			plan.BalanceId = nil
			plan.AuditEventId = nil
		}
		returnErr = err
	})
	if returnErr != nil || plan == nil || !plan.Committed {
		return
	}
	// what the balance reads show now, the grant included. The grant has
	// committed; a failed read only leaves it unconfirmed here.
	server.HandleError(func() {
		plan.SummaryAfter = model.GetTransferBalanceSummary(ctx, networkId)
	}, func(err error) {
		plan.SummaryError = err.Error()
	})
	return
}

// The body of a commit, in a read-committed no-retry transaction. Every
// refusal returns before the first write, so the transaction ends having
// written nothing; any failure after the first write raises, which rolls the
// whole grant back.
func commitBalanceGrantInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	balanceByteCount model.ByteCount,
	endTime time.Time,
	expect string,
	now time.Time,
) (*balanceGrantPlan, error) {
	// the network cannot be deleted before this transaction ends
	if err := model.LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
		if !errors.Is(err, model.ErrPaymentNetworkNotFound) {
			return nil, err
		}
	}
	plan := readBalanceGrantPlan(ctx, tx, networkId, balanceByteCount, endTime, now)
	if plan.State == balanceGrantStateRefused {
		return plan, errBalanceGrantRefused
	}
	if plan.Digest != expect {
		return plan, errBalanceGrantDigestMismatch
	}

	server.Raise(model.AddGrantTransferBalanceInTx(tx, ctx, networkId, balanceGrantKind, balanceByteCount, now, endTime))
	// From here on a failure raises: the grant is written. The writer does
	// not return the row, so read it back by its exact fields.
	balanceIds := []server.Id{}
	result, err := tx.Query(
		ctx,
		`
			SELECT balance_id
			FROM transfer_balance
			WHERE
				network_id = $1 AND
				active AND
				start_time = $2 AND
				end_time = $3 AND
				start_balance_byte_count = $4 AND
				balance_byte_count = $4 AND
				net_revenue_nano_cents = 0 AND
				subsidy_net_revenue_nano_cents = 0 AND
				NOT paid AND
				NOT pro AND
				grant_kind = $5
		`,
		networkId,
		now,
		endTime,
		balanceByteCount,
		balanceGrantKind,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			server.Raise(result.Scan(&balanceId))
			balanceIds = append(balanceIds, balanceId)
		}
	})
	if len(balanceIds) != 1 {
		server.Raise(fmt.Errorf("balance grant: %d rows match the grant just written; nothing was written", len(balanceIds)))
	}
	balanceId := balanceIds[0]

	// the audit line, in the same transaction as the grant
	details, err := json.Marshal(&balanceGrantAudit{
		Command:          "bringyourctl balance grant",
		Digest:           plan.Digest,
		BalanceId:        balanceId,
		BalanceByteCount: balanceByteCount,
		StartTime:        now,
		EndTime:          endTime,
		GrantKind:        balanceGrantKind,
	})
	server.Raise(err)
	eventId := server.NewId()
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
		networkId,
		auditEventTypeBalanceGranted,
		string(details),
	))

	plan.Committed = true
	plan.BalanceId = &balanceId
	plan.StartTime = now
	plan.AuditEventId = &eventId
	return plan, nil
}

// Reads the network and its active balances at now and decides the state.
func readBalanceGrantPlan(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	balanceByteCount model.ByteCount,
	endTime time.Time,
	now time.Time,
) *balanceGrantPlan {
	plan := &balanceGrantPlan{
		Time:             now,
		NetworkId:        networkId,
		State:            balanceGrantStateRefused,
		BalanceByteCount: balanceByteCount,
		EndTime:          endTime,
		ActiveBalances:   []*balanceGrantActiveBalance{},
	}
	result, err := tx.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM network WHERE network_id = $1)`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&plan.NetworkExists))
		}
	})
	// the census's read (model/prober_grant_selection_model.go), ordered by
	// the active network/end/start/id index
	result, err = tx.Query(
		ctx,
		`
			SELECT
				balance_id,
				start_time,
				end_time,
				start_balance_byte_count,
				balance_byte_count,
				paid,
				pro,
				COALESCE(grant_kind, '')
			FROM transfer_balance
			WHERE
				network_id = $1 AND
				active AND
				start_time <= $2 AND
				$2 < end_time
			ORDER BY end_time, start_time, balance_id
		`,
		networkId,
		now,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			balance := &balanceGrantActiveBalance{}
			server.Raise(result.Scan(
				&balance.BalanceId,
				&balance.StartTime,
				&balance.EndTime,
				&balance.StartBalanceByteCount,
				&balance.BalanceByteCount,
				&balance.Paid,
				&balance.Pro,
				&balance.GrantKind,
			))
			plan.ActiveBalances = append(plan.ActiveBalances, balance)
		}
	})

	if !plan.NetworkExists {
		plan.refuse("no network has id %s", networkId)
	}
	if balanceByteCount <= 0 {
		plan.refuse("the grant must be more than 0 bytes, not %d", balanceByteCount)
	}
	if !now.Before(endTime) {
		plan.refuse("the grant must end after now (%s), not at %s", now.Format(time.RFC3339Nano), endTime.Format(time.RFC3339Nano))
	}
	if len(plan.Refusals) == 0 {
		plan.State = balanceGrantStateGrant
		plan.Digest = balanceGrantDigest(networkId, balanceByteCount, endTime, plan.ActiveBalances)
	}
	return plan
}

// The sha256, in hex, of the grant's canonical text: the inputs, then one line
// per active balance in the census's order.
func balanceGrantDigest(
	networkId server.Id,
	balanceByteCount model.ByteCount,
	endTime time.Time,
	activeBalances []*balanceGrantActiveBalance,
) string {
	formatTime := func(t time.Time) string {
		return t.UTC().Format(time.RFC3339Nano)
	}
	lines := []string{
		"urnetwork balance grant v1",
		"network " + networkId.String(),
		fmt.Sprintf("bytes %d", balanceByteCount),
		"end " + formatTime(endTime),
		"grant_kind " + balanceGrantKind,
		"pro false",
		"paid false",
		"revenue_nano_cents 0 0",
	}
	for _, balance := range activeBalances {
		lines = append(lines, fmt.Sprintf(
			"active %s %s %s %d paid=%t pro=%t kind=%s",
			balance.BalanceId,
			formatTime(balance.StartTime),
			formatTime(balance.EndTime),
			balance.StartBalanceByteCount,
			balance.Paid,
			balance.Pro,
			balance.GrantKind,
		))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func balanceGrantText(plan *balanceGrantPlan, commit bool) string {
	formatTime := func(t time.Time) string {
		return t.UTC().Format(time.RFC3339Nano)
	}
	formatBytes := func(byteCount model.ByteCount) string {
		return fmt.Sprintf("%d (%s)", byteCount, model.ByteCountHumanReadable(byteCount))
	}
	b := &strings.Builder{}
	mode := "dry run (read only)"
	if commit {
		mode = "commit"
	}
	fmt.Fprintf(b, "balance grant: %s at %s\n", mode, formatTime(plan.Time))
	if plan.NetworkExists {
		fmt.Fprintf(b, "network %s\n", plan.NetworkId)
	} else {
		fmt.Fprintf(b, "network %s (no such network)\n", plan.NetworkId)
	}

	// each balance's available bytes, as the balance reads show them
	availableByteCounts := map[server.Id]model.ByteCount{}
	if plan.Summary != nil {
		for _, transferBalance := range plan.Summary.ActiveTransferBalances {
			availableByteCounts[transferBalance.BalanceId] = transferBalance.BalanceByteCount
		}
	}
	fmt.Fprintf(b, "active balances: %d\n", len(plan.ActiveBalances))
	for _, balance := range plan.ActiveBalances {
		grantKind := balance.GrantKind
		if grantKind == model.GrantKindNone {
			grantKind = "none"
		}
		fmt.Fprintf(b, "  balance %s kind %s pro %t paid %t\n", balance.BalanceId, grantKind, balance.Pro, balance.Paid)
		fmt.Fprintf(b, "    window %s to %s\n", formatTime(balance.StartTime), formatTime(balance.EndTime))
		available := "not read"
		if availableByteCount, ok := availableByteCounts[balance.BalanceId]; ok {
			available = formatBytes(availableByteCount)
		}
		fmt.Fprintf(b, "    start %s, left %s, available %s\n", formatBytes(balance.StartBalanceByteCount), formatBytes(balance.BalanceByteCount), available)
	}
	if summary := plan.Summary; summary != nil {
		fmt.Fprintf(
			b,
			"balance reads: available %s, open escrow %s, stranded %s\n",
			formatBytes(summary.BalanceByteCount),
			formatBytes(summary.OpenTransferByteCount),
			formatBytes(summary.StrandedTransferByteCount),
		)
	}

	startTime := "the commit's time"
	if !plan.StartTime.IsZero() {
		startTime = formatTime(plan.StartTime)
	}
	fmt.Fprintf(b, "insert one transfer_balance row:\n")
	fmt.Fprintf(b, "  network %s\n", plan.NetworkId)
	fmt.Fprintf(
		b,
		"  window %s to %s (%s from now)\n",
		startTime,
		formatTime(plan.EndTime),
		plan.EndTime.Sub(plan.Time).Round(time.Second),
	)
	fmt.Fprintf(b, "  bytes %s\n", formatBytes(plan.BalanceByteCount))
	fmt.Fprintf(b, "  grant_kind %s, pro false, paid false, net revenue 0, subsidy net revenue 0\n", balanceGrantKind)
	fmt.Fprintf(b, "state %s\n", plan.State)
	for _, refusal := range plan.Refusals {
		fmt.Fprintf(b, "  refused: %s\n", refusal)
	}
	if plan.Digest != "" {
		fmt.Fprintf(b, "digest %s\n", plan.Digest)
	}
	if plan.Committed && plan.BalanceId != nil {
		fmt.Fprintf(b, "granted: balance %s on network %s\n", *plan.BalanceId, plan.NetworkId)
		if after := plan.SummaryAfter; after != nil {
			listed := false
			for _, transferBalance := range after.ActiveTransferBalances {
				if transferBalance.BalanceId == *plan.BalanceId {
					listed = true
				}
			}
			fmt.Fprintf(
				b,
				"  balance reads now: grant listed %t, available %s, open escrow %s\n",
				listed,
				formatBytes(after.BalanceByteCount),
				formatBytes(after.OpenTransferByteCount),
			)
		}
		if plan.SummaryError != "" {
			fmt.Fprintf(b, "  balance reads not confirmed: %s\n", plan.SummaryError)
		}
	} else if commit {
		fmt.Fprintf(b, "nothing was written\n")
	}
	return b.String()
}
