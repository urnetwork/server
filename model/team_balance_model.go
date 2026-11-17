package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// A Team's transfer balances (DESIGN.md 20.5's Source, 20.9's first paragraph; D-QUOTA-1,
// D-QUOTA-9). Each billing period writes two balances:
//
//	grant  kind team: pro's data (10 TiB) with pro = true and tier 'team', so Pro's readers stay
//	       right and NetworkTierOf answers team (pro_tier_model.go)
//	line   kind team_credit: the credit_line bytes (10 TiB) that transfer overage draws last
//	       (escrow_draw_order.go), pro = false and no tier, so it never confers the tier, carrying
//	       net_revenue_nano_cents at overage_usd.transfer_tib, so settlement pays providers from it
//	       at the overage rate (E-e)
//
// A missing transfer_tib price writes no line, so overage is withheld (D-QUOTA-2); so does an
// escrow_draw_order gate that has not read on for its reread (EscrowDrawOrderOpenForCredit), since
// a line any site would draw in today's order breaks E-a.
//
// Periods start at a 00:00:00 UTC anchor (Team checkout sets billing_cycle_anchor there, QUOTA-M6-02),
// and each window runs ProGrantGrace past its period, as Pro's, so lapse follows Pro: when the
// windows end, pro falls with them and the tier drops. Team renewals are subscription_renewal rows
// of SubscriptionTypeTeam, so AddProTransferBalanceToAllNetworks, which reads supporter renewals,
// never grants a Team Pro's data, and AddFreeTransferBalanceToAllNetworks skips a network holding
// an in-window team grant.
//
// Who writes a period (plan-QUOTA-team-renewal-window): invoice.paid writes the first period's
// renewal and balances (QUOTA-M6-02, through AddTeamGrantInTx) and the team_subscription row
// (QUOTA-M6-07, owner decision 199), which holds the billing anchor. The first period runs from
// the checkout to the anchor, under a day and an hour. Each later period is written at its anchor
// by RefreshTeamTransferBalances for every Team whose row is live with no unresolved payment
// failure, before that period's invoice is collected, since a cycle invoice finalizes 72 h into
// its period; the job reads the anchor's day of the month from the row. It writes only the period
// that follows the newest paid renewal of that subscription, so a Team whose cycle invoice never
// pays gets one period more and then lapses: the exposure the resolution accepts. A subscription
// whose row ended (EndTeamSubscriptionInTx, team_subscription_model.go) is not live, so the job
// writes it nothing more.

// Why a period's line was not written.
const (
	TeamCreditLineWithheldNoPrice     = "no_transfer_tib_price"
	TeamCreditLineWithheldGate        = "escrow_draw_order_not_open"
	TeamCreditLineWithheldNoCreditMax = "no_credit_line"
)

var ErrTeamGrantUnavailable = errors.New("Team balances need MG6.")
var ErrTeamGrantNotTeam = errors.New("Team balances are written only for a Team network.")

// Settings of the Team balance job.
type TeamBalanceSettings struct {
	// The most Teams one run of RefreshTeamTransferBalances writes a period for. A run past it
	// leaves the rest to the next run, a day later, inside every period's one-day grace.
	MaxTeamsPerRun int
}

func DefaultTeamBalanceSettings() *TeamBalanceSettings {
	return &TeamBalanceSettings{
		// ~1,000 Teams in year two (DESIGN.md 20.9's cost row), with room
		MaxTeamsPerRun: 10000,
	}
}

var teamBalanceSettings = DefaultTeamBalanceSettings()

// One period's Team balances as written.
type TeamGrant struct {
	NetworkId      server.Id
	StartTime      time.Time
	EndTime        time.Time
	GrantBalanceId server.Id
	// nil when the line was withheld, with the reason in LineWithheld
	LineBalanceId *server.Id
	LineWithheld  string
}

// Writes one period's Team balances in the caller's transaction: the team grant and, where a price
// and the escrow_draw_order gate allow it, the credit line, both over [periodStart, periodEnd +
// ProGrantGrace). subsidyNetRevenue is the period's base revenue for provider subsidy accounting,
// as Pro's monthly grant carries its renewal's. The caller refreshes the tier after commit
// (UpdateProNetwork), never inside the transaction.
func AddTeamGrantInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	periodStart time.Time,
	periodEnd time.Time,
	subsidyNetRevenue NanoCents,
) (*TeamGrant, error) {
	if !server.SidecarMg6Linked() {
		return nil, ErrTeamGrantUnavailable
	}
	if GetNetworkTeamInTx(ctx, tx, networkId) == nil {
		return nil, ErrTeamGrantNotTeam
	}
	if !periodStart.Before(periodEnd) {
		return nil, fmt.Errorf("Team period must end after it starts.")
	}
	if subsidyNetRevenue < 0 {
		return nil, fmt.Errorf("Team period revenue must not be negative.")
	}
	config := Pro()
	data := config.TierFor(NetworkTierTeam).Data
	if data <= 0 {
		return nil, fmt.Errorf("Team data is not configured (is pro.yml present?).")
	}
	grace, _ := grantGrace(GrantKindTeam)
	startTime := periodStart.UTC()
	endTime := periodEnd.UTC().Add(grace)

	grant := &TeamGrant{
		NetworkId:      networkId,
		StartTime:      startTime,
		EndTime:        endTime,
		GrantBalanceId: server.NewId(),
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO transfer_balance (
				balance_id,
				network_id,
				start_time,
				end_time,
				start_balance_byte_count,
				net_revenue_nano_cents,
				subsidy_net_revenue_nano_cents,
				balance_byte_count,
				pro,
				grant_kind,
				tier
			)
			VALUES ($1, $2, $3, $4, $5, 0, $6, $5, true, $7, $8)
		`,
		grant.GrantBalanceId,
		networkId,
		startTime,
		endTime,
		data,
		subsidyNetRevenue,
		GrantKindTeam,
		string(NetworkTierTeam),
	))

	lineRevenue, withheld := teamCreditLineRevenue(config)
	if withheld == "" && !EscrowDrawOrderOpenForCredit() {
		withheld = TeamCreditLineWithheldGate
	}
	if withheld != "" {
		grant.LineWithheld = withheld
		glog.Infof("[quota][team]%s period from %s: credit line withheld (%s)\n", networkId, startTime.Format(time.RFC3339), withheld)
		return grant, nil
	}
	lineBalanceId := server.NewId()
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO transfer_balance (
				balance_id,
				network_id,
				start_time,
				end_time,
				start_balance_byte_count,
				net_revenue_nano_cents,
				subsidy_net_revenue_nano_cents,
				balance_byte_count,
				pro,
				grant_kind
			)
			VALUES ($1, $2, $3, $4, $5, $6, 0, $5, false, $7)
		`,
		lineBalanceId,
		networkId,
		startTime,
		endTime,
		config.Team.CreditLine,
		lineRevenue,
		GrantKindTeamCredit,
	))
	grant.LineBalanceId = &lineBalanceId
	return grant, nil
}

// The line's revenue at overage_usd.transfer_tib, or why no line is written. The revenue is the
// whole line at the overage rate, so settlement pays a provider ProviderRevenueShare of the rate
// per byte taken from it (contractSettlementSweepPayouts). A line with no revenue would read as a
// free grant to the large-free-grant spread, so a price that rounds to nothing withholds it too.
func teamCreditLineRevenue(config *ProConfig) (revenue NanoCents, withheld string) {
	if config.Team.CreditLine <= 0 {
		return 0, TeamCreditLineWithheldNoCreditMax
	}
	priceUsd, ok := config.Team.OverageTransferTibUsd()
	if !ok {
		return 0, TeamCreditLineWithheldNoPrice
	}
	revenue = UsdToNanoCents(priceUsd * float64(config.Team.CreditLine) / float64(Tib))
	if revenue <= 0 {
		return 0, TeamCreditLineWithheldNoPrice
	}
	return revenue, ""
}

// The end of a monthly period that starts at periodStart, for a subscription anchored on
// anchorDay: the anchor day of the next month at 00:00:00 UTC, or that month's last day when it
// has no such day, as Stripe bills a monthly cycle.
func teamPeriodEnd(anchorDay int, periodStart time.Time) time.Time {
	year, month, _ := periodStart.UTC().Date()
	// the first of the month after, normalized past December
	next := time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
	lastDay := next.AddDate(0, 1, -1).Day()
	return time.Date(next.Year(), next.Month(), min(anchorDay, lastDay), 0, 0, 0, 0, time.UTC)
}

// Writes, for every Team whose subscription is live with no unresolved payment failure, the period
// that follows its newest paid renewal once that period's anchor has passed and before it ends, in
// one transaction with the run's replay receipt (claimTransferGrantRunInTx), then refreshes each
// written network's tier after the commit. A period already written is never written again, so a
// rerun grants once. Teams past MaxTeamsPerRun wait for the next run.
func RefreshTeamTransferBalances(ctx context.Context, now time.Time, runIds ...server.Id) (grants []*TeamGrant) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	now = now.UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	server.Tx(ctx, func(tx server.PgTx) {
		grants = nil
		if !claimTransferGrantRunInTx(ctx, tx, GrantKindTeam, dayStart, dayStart.AddDate(0, 0, 1), runIds) {
			return
		}
		type teamRenewal struct {
			networkId  server.Id
			startTime  time.Time
			endTime    time.Time
			revenue    NanoCents
			anchorTime time.Time
		}
		renewals := []*teamRenewal{}
		result, err := tx.Query(
			ctx,
			`
				SELECT
					network_team.network_id,
					renewal.start_time,
					renewal.end_time,
					renewal.net_revenue_nano_cents,
					subscription.anchor_time
				FROM network_team
				-- the live subscription, at most one a network (owner decision 199)
				INNER JOIN team_subscription subscription ON
					subscription.network_id = network_team.network_id AND
					subscription.end_time IS NULL
				CROSS JOIN LATERAL (
					-- the newest renewal of that subscription, none of whose renewals starts
					-- before it does
					SELECT start_time, end_time, net_revenue_nano_cents
					FROM subscription_renewal
					WHERE
						subscription_renewal.network_id = network_team.network_id AND
						subscription_renewal.subscription_type = $1 AND
						subscription.start_time <= subscription_renewal.start_time
					ORDER BY end_time DESC, start_time DESC
					LIMIT 1
				) AS renewal
				WHERE
					-- the period after the newest renewal has started and not ended
					renewal.end_time <= $2 AND
					NOT EXISTS (
						SELECT 1
						FROM stripe_invoice_payment_failure failure
						WHERE
							failure.network_id = network_team.network_id AND
							failure.resolve_time IS NULL
					)
				ORDER BY network_team.network_id
				LIMIT $3
			`,
			SubscriptionTypeTeam,
			now,
			teamBalanceSettings.MaxTeamsPerRun,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				renewal := &teamRenewal{}
				server.Raise(result.Scan(
					&renewal.networkId,
					&renewal.startTime,
					&renewal.endTime,
					&renewal.revenue,
					&renewal.anchorTime,
				))
				renewals = append(renewals, renewal)
			}
		})
		for _, renewal := range renewals {
			periodStart := renewal.endTime.UTC()
			periodEnd := teamPeriodEnd(renewal.anchorTime.UTC().Day(), periodStart)
			if !now.Before(periodEnd) {
				// a whole period passed with no paid renewal: the Team has lapsed
				continue
			}
			var written bool
			server.Raise(tx.QueryRow(
				ctx,
				`
					SELECT EXISTS (
						SELECT 1
						FROM transfer_balance
						WHERE
							network_id = $1 AND
							pro = true AND
							tier = 'team' AND
							start_time = $2
					)
				`,
				renewal.networkId,
				periodStart,
			).Scan(&written))
			if written {
				continue
			}
			// the base revenue of the paid period before, pro-rated to this one, for provider
			// subsidy accounting as Pro's monthly grant carries its renewal's
			subsidyNetRevenue := NanoCents(0)
			if paidDuration := renewal.endTime.Sub(renewal.startTime); 0 < paidDuration {
				subsidyNetRevenue = NanoCents(float64(periodEnd.Sub(periodStart)) / float64(paidDuration) * float64(renewal.revenue))
			}
			grant, err := AddTeamGrantInTx(ctx, tx, renewal.networkId, periodStart, periodEnd, max(0, subsidyNetRevenue))
			if err != nil {
				glog.Errorf("[quota][team]%s period from %s not written: %s\n", renewal.networkId, periodStart.Format(time.RFC3339), err)
				continue
			}
			grants = append(grants, grant)
		}
	})
	networkIds := []server.Id{}
	for _, grant := range grants {
		networkIds = append(networkIds, grant.NetworkId)
	}
	UpdateProNetworks(ctx, networkIds...)
	return
}

// When the network left a tier, and which: when its entitlement to a tier above its tier now ended
// (QD-946, owner decision 523; DESIGN.md 20.5's Source and 20.7's downgrade clock). That is the
// end_time of the newest window, already ended, of a balance that conferred such a tier, each
// balance conferring what NetworkTierOf reads from it: team with pro = true and tier 'team', a Team
// period's grant, whose window holds its one-day grace; pro with pro = true and any other tier,
// Pro's monthly grant and the balances a Pro subscription or purchase writes. The newest end wins,
// a tie going to the higher tier. So a Team whose cycle invoice never pays, which
// RefreshTeamTransferBalances granted a period past its newest renewal, left team when that
// period's grant ended, the moment NetworkTierOf dropped, not when the renewal ended, and 20.7's
// 14 days count from there for a Team as for Pro. ok is false while the network has left no tier:
// no balance of a tier above its tier now has ended. A balance that has not ended, in its window or
// not yet started, is never the answer.
//
// A binary that does not link MG6 has no tier column and no Team, and answers as before MG6 from
// renewals: the end of the newest renewal, already ended, of a tier above the network's tier now, a
// team renewal for team and a supporter renewal for pro. It tells by server.SidecarMg6Linked, the
// switch the tier's own load reads (loadNetworkTierWithConn), so the tier and the clock read one
// source in either binary.
//
// QUOTA-M6-05's 14-day withholding counts from this time, and DNS-M6-07's effects and the TLS listen
// facts' standing grants with it. Read from the source of truth, never the tier cache, for a daily
// job.
func NetworkDowngradeTime(ctx context.Context, networkId server.Id) (downgradeTime time.Time, fromTier NetworkTier, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		_, tier := loadNetworkTierWithConn(ctx, conn, networkId)
		// read after the tier, so a balance that ends between the two reads was in its window for the
		// tier's read, which answered its tier or a higher one
		now := server.NowUtc()
		// the tiers above the network's, highest first, so a tie goes to the higher
		leftTiers := []NetworkTier{}
		switch tier.tier {
		case NetworkTierFree:
			leftTiers = append(leftTiers, NetworkTierTeam, NetworkTierPro)
		case NetworkTierPro:
			leftTiers = append(leftTiers, NetworkTierTeam)
		}
		if len(leftTiers) == 0 {
			return
		}
		// the newest end, at or before now, of what conferred each tier; nil when none has ended
		tierEndTimes := map[NetworkTier]*time.Time{}
		if server.SidecarMg6Linked() {
			var teamEndTime *time.Time
			var proEndTime *time.Time
			server.Raise(conn.QueryRow(
				ctx,
				`
					SELECT
						MAX(end_time) FILTER (WHERE tier = 'team'),
						MAX(end_time) FILTER (WHERE tier IS DISTINCT FROM 'team')
					FROM transfer_balance
					WHERE
						network_id = $1 AND
						pro = true AND
						end_time <= $2
				`,
				networkId,
				now,
			).Scan(&teamEndTime, &proEndTime))
			tierEndTimes[NetworkTierTeam] = teamEndTime
			tierEndTimes[NetworkTierPro] = proEndTime
		} else {
			for _, leftTier := range leftTiers {
				subscriptionType := SubscriptionTypeSupporter
				if leftTier == NetworkTierTeam {
					subscriptionType = SubscriptionTypeTeam
				}
				var endTime *time.Time
				server.Raise(conn.QueryRow(
					ctx,
					`
						SELECT MAX(end_time)
						FROM subscription_renewal
						WHERE
							network_id = $1 AND
							subscription_type = $2 AND
							end_time <= $3
					`,
					networkId,
					subscriptionType,
					now,
				).Scan(&endTime))
				tierEndTimes[leftTier] = endTime
			}
		}
		for _, leftTier := range leftTiers {
			endTime := tierEndTimes[leftTier]
			if endTime == nil {
				continue
			}
			if !ok || downgradeTime.Before(*endTime) {
				downgradeTime = endTime.UTC()
				fromTier = leftTier
				ok = true
			}
		}
	})
	return
}
