package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// ----- grant windows -----
//
// The three recurring grants run on three different schedules, and each balance's
// window extends a little past the end of its period so consecutive grants overlap
// and a client never sees a gap at the boundary. Spending draws the balance that
// ends first, so the old grant's leftover is used up before the new grant.

// How long past the end of the day a daily free balance stays valid.
const FreeGrantGrace = 1 * time.Hour

// How long past the end of the month a monthly Pro balance stays valid. It is also
// the window in which a lapsed subscriber is still Pro, because the Pro entitlement
// is exactly "has an in-window pro balance" (see pro_model.go).
const ProGrantGrace = 24 * time.Hour

// The window for the daily free grant covering `now`:
// [start of day, start of next day + 1 hour).
func FreeGrantWindow(now time.Time) (startTime time.Time, endTime time.Time) {
	year, month, day := now.UTC().Date()
	startTime = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	endTime = startTime.AddDate(0, 0, 1).Add(FreeGrantGrace)
	return
}

// The window for the monthly Pro grant covering `now`:
// [start of month, start of next month + 1 day).
func ProGrantWindow(now time.Time) (startTime time.Time, endTime time.Time) {
	year, month, _ := now.UTC().Date()
	startTime = time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	endTime = startTime.AddDate(0, 1, 0).Add(ProGrantGrace)
	return
}

// The window for one referral grant period, from `now`.
func ReferralGrantWindow(now time.Time) (startTime time.Time, endTime time.Time) {
	startTime = now.UTC()
	endTime = startTime.Add(Pro().ReferralGrantPeriod()).Add(FreeGrantGrace)
	return
}

// The recurring grant that wrote a balance (transfer_balance.grant_kind). The balance
// summary uses it to tell a grant from the next grant of its kind (see
// SupersededGrants). Every other balance -- purchases, subscriptions, data codes,
// prober credit -- has no kind, and so do grants written before the kind was
// recorded.
type GrantKind = string

const (
	GrantKindNone     GrantKind = ""
	GrantKindFree     GrantKind = "free"
	GrantKindPro      GrantKind = "pro"
	GrantKindReferral GrantKind = "referral"
)

// How long a grant of the kind stays valid past the end of its period, i.e. its
// overlap with the next grant of the kind. false for no kind or a kind this binary
// does not know.
func grantGrace(grantKind GrantKind) (time.Duration, bool) {
	switch grantKind {
	case GrantKindFree, GrantKindReferral:
		return FreeGrantGrace, true
	case GrantKindPro:
		return ProGrantGrace, true
	default:
		return 0, false
	}
}

// Adds one recurring grant at no cost -- the daily free grant, the monthly Pro grant
// or a referral bonus -- and records its kind. The Pro grant carries pro = true,
// which is what confers the entitlement (see pro_model.go); the caller must refresh
// the Pro cache (UpdateProNetwork) once the tx commits. Free and referral grants
// never confer Pro.
func AddGrantTransferBalanceInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	grantKind GrantKind,
	transferBalance ByteCount,
	startTime time.Time,
	endTime time.Time,
) (returnErr error) {
	if _, ok := grantGrace(grantKind); !ok {
		returnErr = fmt.Errorf("Unknown grant kind \"%s\".", grantKind)
		return
	}

	_, err := tx.Exec(
		ctx,
		`
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    net_revenue_nano_cents,
                    balance_byte_count,
                    pro,
                    grant_kind
                )
                VALUES ($1, $2, $3, $4, $5, $6, $5, $7, $8)
            `,
		server.NewId(),
		networkId,
		startTime,
		endTime,
		transferBalance,
		NanoCents(0),
		grantKind == GrantKindPro,
		grantKind,
	)
	// a failed insert aborts the caller's transaction, so it raises rather
	// than leave the caller to commit a rollback
	server.Raise(err)
	return
}

// The balances, among one network's active balances, that are grants superseded by
// the next grant of their kind. Consecutive grants of a kind overlap by the kind's
// grace, so at every boundary two are active: the old grant in its grace and the new
// grant. The old grant is superseded once a recorded grant of its kind has started at
// or after the end of the old grant's period (its end time less the grace). It stays
// spendable, and since it ends first it is still drawn first; only the summary leaves
// it out. Grants of the same period all count: a network that is both a referrer and
// a referee gets two referral grants from one run, and a refresh can add a second
// grant of the day.
//
// A balance without a recorded kind was written before grant kinds were recorded,
// or by a binary that predates them during a rollout. It counts as a grant only when
// legacyGrantKind recognizes its exact window, and it can be superseded but never
// supersedes: only a recorded grant replaces a balance in the summary, so a legacy
// balance never hides another one.
func SupersededGrants(transferBalances []*TransferBalance) map[server.Id]bool {
	// recognizes a grant that has no recorded kind by its exact window, which
	// the grant writers compute from the grant time (see the grant windows
	// above), and by its flags. A free window starts at midnight UTC and lasts a
	// day plus the grace; a Pro window starts at midnight UTC on the first of a
	// month and lasts the month plus the grace; a referral window lasts the
	// referral period plus the grace from whenever the run started. Free and
	// referral grants carry no revenue and no Pro. The Pro grant carries Pro and
	// no net revenue (the monthly grant does carry subsidy revenue). Purchases,
	// subscriptions, data codes and prober credit start when they are bought or
	// granted and run for their own durations, so they match none of these
	// windows exactly.
	legacyGrantKind := func(transferBalance *TransferBalance) GrantKind {
		hasWindow := func(grantWindow func(time.Time) (time.Time, time.Time)) bool {
			startTime, endTime := grantWindow(transferBalance.StartTime)
			return startTime.Equal(transferBalance.StartTime) && endTime.Equal(transferBalance.EndTime)
		}

		if transferBalance.Pro {
			if transferBalance.NetRevenue == 0 && hasWindow(ProGrantWindow) {
				return GrantKindPro
			}
			return GrantKindNone
		}
		if transferBalance.Paid {
			return GrantKindNone
		}
		if hasWindow(FreeGrantWindow) {
			return GrantKindFree
		}
		if hasWindow(ReferralGrantWindow) {
			return GrantKindReferral
		}
		return GrantKindNone
	}

	// the newest recorded grant of each kind. A balance is superseded by some
	// recorded grant of its kind exactly when it is superseded by the newest one,
	// since both tests only bound the newer grant's start from below.
	grantKindNewestStartTimes := map[GrantKind]time.Time{}
	for _, transferBalance := range transferBalances {
		if _, ok := grantGrace(transferBalance.GrantKind); !ok {
			continue
		}
		newestStartTime, ok := grantKindNewestStartTimes[transferBalance.GrantKind]
		if !ok || newestStartTime.Before(transferBalance.StartTime) {
			grantKindNewestStartTimes[transferBalance.GrantKind] = transferBalance.StartTime
		}
	}

	supersededBalanceIds := map[server.Id]bool{}
	for _, transferBalance := range transferBalances {
		grantKind := transferBalance.GrantKind
		if grantKind == GrantKindNone {
			grantKind = legacyGrantKind(transferBalance)
		}
		grace, ok := grantGrace(grantKind)
		if !ok {
			continue
		}
		newestStartTime, ok := grantKindNewestStartTimes[grantKind]
		if !ok {
			continue
		}
		periodEndTime := transferBalance.EndTime.Add(-grace)
		if transferBalance.StartTime.Before(newestStartTime) && !newestStartTime.Before(periodEndTime) {
			supersededBalanceIds[transferBalance.BalanceId] = true
		}
	}
	return supersededBalanceIds
}

// The data balance the apps show (the "Daily Data Balance" bar): the start, available
// and pending bytes of the network's active balances, with superseded grants left out
// (see SummarizeTransferBalances).
type TransferBalanceSummary struct {
	StartBalanceByteCount ByteCount
	// available: the balance less what open contracts reserve from it
	BalanceByteCount ByteCount
	// pending: what open contracts reserve
	OpenTransferByteCount ByteCount
	// every active balance, superseded grants included
	ActiveTransferBalances []*TransferBalance
}

// Reads the network's active balances and open contracts and summarizes them (see
// SummarizeTransferBalances).
func GetTransferBalanceSummary(ctx context.Context, networkId server.Id) *TransferBalanceSummary {
	transferBalances := GetActiveTransferBalances(ctx, networkId)
	openTransferByteCount := GetOpenTransferByteCount(ctx, networkId)
	return SummarizeTransferBalances(transferBalances, openTransferByteCount, server.NowUtc())
}

// Sums the active balances. A grant superseded by the next grant of its kind (see
// SupersededGrants) is left out of all three numbers: its start, its available bytes,
// and the bytes open contracts reserve from it, which openTransferByteCount (every
// open contract of the payer) includes. So from 00:00 to 01:00 UTC the daily bar
// reads today's grant with nothing used, instead of yesterday's and today's grants
// added together. Spending is unchanged: the superseded grant ends first, so its
// leftover is still drawn first, and until it ends the summary understates what is
// available by that leftover.
func SummarizeTransferBalances(
	transferBalances []*TransferBalance,
	openTransferByteCount ByteCount,
	now time.Time,
) *TransferBalanceSummary {
	summary := &TransferBalanceSummary{
		OpenTransferByteCount:  openTransferByteCount,
		ActiveTransferBalances: transferBalances,
	}
	supersededBalanceIds := SupersededGrants(transferBalances)
	for _, transferBalance := range transferBalances {
		if !transferBalance.EndTime.After(now) {
			continue
		}
		if supersededBalanceIds[transferBalance.BalanceId] {
			summary.OpenTransferByteCount -= transferBalance.reservedByteCount
			continue
		}
		summary.BalanceByteCount += transferBalance.BalanceByteCount
		summary.StartBalanceByteCount += transferBalance.StartBalanceByteCount
	}
	// the reservations come from the per-balance escrow counters and the open
	// total from the contracts, which can drift apart
	summary.OpenTransferByteCount = max(0, summary.OpenTransferByteCount)
	return summary
}
