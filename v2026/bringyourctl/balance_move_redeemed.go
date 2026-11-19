// `bringyourctl balance move-redeemed` (model/transfer_balance_code_move_model.go):
// moves the unused balance that a balance code granted from a deleted network
// to another network, as if the code had been redeemed there. Support inbox
// 764, owner decision 2026-10-29.
package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func balanceMoveRedeemedCommand(opts docopt.Opts, stdout io.Writer, stderr io.Writer) int {
	balanceIdValue, _ := opts.String("--balance_id")
	toNetworkIdValue, _ := opts.String("--to_network_id")
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	// The dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path. Its plan reads the Pro entitlement from
	// the database, so it does not fill the Pro cache either.
	if !commit {
		server.RequirePgReadOnly()
	}
	ctx := context.Background()
	return runBalanceMoveRedeemed(
		stdout,
		stderr,
		balanceIdValue,
		toNetworkIdValue,
		commit,
		func(balanceId server.Id, toNetworkId server.Id) (*model.RedeemedBalanceMovePlan, error) {
			if commit {
				return model.CommitRedeemedBalanceMove(ctx, balanceId, toNetworkId, expect)
			}
			return model.PlanRedeemedBalanceMove(ctx, balanceId, toNetworkId)
		},
	)
}

// Prints the plan and returns the exit code: 0 for a dry run that can move or
// found the move made, and for a commit that moved or found the move made
// with its digest; 1 for a refusal or a failure, which write nothing; 2 for
// an id that does not parse. A dry run that can move ends with the command
// that moves exactly what it printed.
func runBalanceMoveRedeemed(
	stdout io.Writer,
	stderr io.Writer,
	balanceIdValue string,
	toNetworkIdValue string,
	commit bool,
	run func(balanceId server.Id, toNetworkId server.Id) (*model.RedeemedBalanceMovePlan, error),
) int {
	balanceId, err := server.ParseId(balanceIdValue)
	if err != nil {
		fmt.Fprintf(stderr, "balance move-redeemed: --balance_id %q: %s\n", balanceIdValue, err)
		return 2
	}
	toNetworkId, err := server.ParseId(toNetworkIdValue)
	if err != nil {
		fmt.Fprintf(stderr, "balance move-redeemed: --to_network_id %q: %s\n", toNetworkIdValue, err)
		return 2
	}

	plan, err := run(balanceId, toNetworkId)
	if plan != nil {
		fmt.Fprint(stdout, redeemedBalanceMoveText(plan, commit))
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	if plan == nil {
		fmt.Fprintf(stderr, "balance move-redeemed: no plan; nothing was written\n")
		return 1
	}
	switch plan.State {
	case model.RedeemedBalanceMoveStateRefused:
		fmt.Fprintf(stderr, "%s\n", model.ErrRedeemedBalanceMoveRefused)
		return 1
	case model.RedeemedBalanceMoveStateMove:
		if commit && plan.Committed && plan.AuditEventId != nil && plan.Move != nil {
			// the audit line, also in the process log
			line := redeemedBalanceMoveAuditLine(plan)
			fmt.Fprintf(stdout, "%s\n", line)
			glog.Infof("[balance]%s\n", line)
		} else if !commit {
			fmt.Fprintf(
				stdout,
				"to move exactly this: bringyourctl balance move-redeemed --balance_id=%s --to_network_id=%s --commit --expect=%s\n",
				plan.BalanceId,
				plan.ToNetworkId,
				plan.Digest,
			)
		}
	}
	return 0
}

// One line naming the event, the balance, the code, both networks and the
// digest.
func redeemedBalanceMoveAuditLine(plan *model.RedeemedBalanceMovePlan) string {
	return fmt.Sprintf(
		"audit %s event_id=%s balance_id=%s balance_code_id=%s from_network_id=%s to_network_id=%s pro=%t digest=%s",
		model.AuditEventTypeRedeemedBalanceMoved,
		*plan.AuditEventId,
		plan.Move.BalanceId,
		plan.Move.BalanceCodeId,
		plan.Move.FromNetworkId,
		plan.Move.ToNetworkId,
		plan.Move.Pro,
		plan.Digest,
	)
}

func redeemedBalanceMoveText(plan *model.RedeemedBalanceMovePlan, commit bool) string {
	formatTime := func(t time.Time) string {
		return t.UTC().Format(time.RFC3339Nano)
	}
	b := &strings.Builder{}
	mode := "dry run (read only)"
	if commit {
		mode = "commit"
	}
	fmt.Fprintf(b, "balance move-redeemed: %s at %s\n", mode, formatTime(plan.Time))
	fmt.Fprintf(b, "balance %s\n", plan.BalanceId)
	if move := plan.Move; move != nil {
		fmt.Fprintf(b, "  code %s, redeemed %s\n", move.BalanceCodeId, formatTime(move.RedeemTime))
		fromState := "deleted"
		if plan.State != model.RedeemedBalanceMoveStateMoved && plan.FromNetworkExists {
			fromState = "exists"
		}
		fmt.Fprintf(b, "  from network %s (%s)\n", move.FromNetworkId, fromState)
		fmt.Fprintf(b, "  window %s to %s\n", formatTime(move.StartTime), formatTime(move.EndTime))
		fmt.Fprintf(
			b,
			"  bytes %d of %d left (%s)\n",
			move.BalanceByteCount,
			move.StartBalanceByteCount,
			model.ByteCountHumanReadable(move.BalanceByteCount),
		)
		fmt.Fprintf(
			b,
			"  revenue %d nano cents (%.2f USD), subsidy %d nano cents\n",
			move.NetRevenue,
			model.NanoCentsToUsd(move.NetRevenue),
			move.SubsidyNetRevenue,
		)
		fmt.Fprintf(b, "  pro %t\n", move.Pro)
	} else if plan.BalanceNetworkId != nil {
		fmt.Fprintf(b, "  on network %s; %d codes name it\n", *plan.BalanceNetworkId, plan.CodeCount)
	}
	if plan.BalanceFound {
		fmt.Fprintf(b, "  use: %d escrows, %d debit journal rows\n", plan.EscrowCount, plan.DebitJournalCount)
	}
	if network := plan.ToNetwork; network != nil {
		fmt.Fprintf(b, "to network %s %s\n", network.NetworkId, network.NetworkName)
		fmt.Fprintf(b, "  admin %s signs in with: %s\n", network.AdminUserId, strings.Join(network.SignIns, ", "))
		fmt.Fprintf(b, "  pro before: %t\n", plan.ToNetworkPro)
	} else {
		fmt.Fprintf(b, "to network %s (no such network)\n", plan.ToNetworkId)
	}
	fmt.Fprintf(b, "state %s\n", plan.State)
	for _, refusal := range plan.Refusals {
		fmt.Fprintf(b, "  refused: %s\n", refusal)
	}
	if plan.State == model.RedeemedBalanceMoveStateMoved && plan.MovedTime != nil && plan.MovedEventId != nil {
		fmt.Fprintf(
			b,
			"  already moved at %s (audit event %s); a commit with this digest writes nothing\n",
			formatTime(*plan.MovedTime),
			*plan.MovedEventId,
		)
	}
	if plan.Digest != "" {
		fmt.Fprintf(b, "digest %s\n", plan.Digest)
	}
	if plan.Committed {
		fmt.Fprintf(b, "moved: the balance and its code are on network %s\n", plan.ToNetworkId)
		if plan.ToNetworkProAfter != nil {
			fmt.Fprintf(b, "  pro after: %t (cache refreshed)\n", *plan.ToNetworkProAfter)
		}
		if plan.ProCacheError != "" {
			fmt.Fprintf(b, "  pro cache not refreshed (it refreshes within %s): %s\n", model.ProCacheTtl, plan.ProCacheError)
		}
	} else if commit && plan.State == model.RedeemedBalanceMoveStateMoved {
		fmt.Fprintf(b, "nothing was written\n")
	}
	return b.String()
}
