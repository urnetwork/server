package model

import (
	"context"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// What Team's Stripe events change in a Team's balances and its subscription row (QUOTA-M6-02,
// QUOTA-M6-07; DESIGN.md 20.9's Post-paid; plan-QUOTA-team-renewal-window; owner decision 199):
//
//	invoice.paid, the first        writes the team_subscription row: the subscription and customer
//	                               ids, the billing anchor and the start (AddTeamSubscriptionInTx)
//	invoice.payment_failed         ends the lines in window, so overage refuses at once while the
//	                               included bytes run out (EndTeamCreditLinesInTx); the open failure
//	                               row stops the next grant (RefreshTeamTransferBalances)
//	invoice.paid, a cycle invoice  gives a line ended by a failure its period back
//	                               (RestoreTeamCreditLinesInTx)
//	customer.subscription.deleted  ends the lines and every period no invoice paid for, and ends the
//	                               subscription's row, so the job writes it nothing more
//	                               (EndTeamSubscriptionInTx); the hourly reconciler does the same for
//	                               a subscription Stripe shows deleted whose event was lost
//
// A balance ends by its end_time, never by its bytes, as a refund's clawback ends one: what it
// has reserved still settles, and what it has paid stays paid. Each function here that moves an
// end_time takes the balances' ownership first and raises a contention error when another writer
// holds one, so its caller runs it in a read-committed no-retry transaction and leaves the event
// to Stripe's redelivery. A transaction takes ownership once, for its whole key set
// (server.TryTxOwnership), so a caller runs at most one of the three in a transaction.
//
// team_subscription holds at most one live row a network (end_time NULL), which is what the grant
// job and the meter reports read as a live subscription. An ended row is kept until its network is
// deleted, and a resubscribe writes a second row beside it. No id here is logged.
//
// Nothing here runs unless the binary links MG6, whose transfer_balance.tier and team_subscription
// these read.

// One Team's Stripe subscription, as team_subscription holds it.
type TeamSubscription struct {
	NetworkId      server.Id
	SubscriptionId string
	CustomerId     string
	// The billing cycle anchor: a 00:00:00 UTC whose day of the month every period starts on.
	AnchorTime time.Time
	// The subscription's start: the first period's start.
	StartTime time.Time
	// nil while the subscription is live.
	EndTime *time.Time
}

// Whether the subscription is live at now: not ended, or ended later.
func (self *TeamSubscription) LiveAt(at time.Time) bool {
	return self.EndTime == nil || at.Before(*self.EndTime)
}

const teamSubscriptionColumns = `network_id, subscription_id, customer_id, anchor_time, start_time, end_time`

func scanTeamSubscription(row interface{ Scan(dest ...any) error }) *TeamSubscription {
	subscription := &TeamSubscription{}
	server.Raise(row.Scan(
		&subscription.NetworkId,
		&subscription.SubscriptionId,
		&subscription.CustomerId,
		&subscription.AnchorTime,
		&subscription.StartTime,
		&subscription.EndTime,
	))
	subscription.AnchorTime = subscription.AnchorTime.UTC()
	subscription.StartTime = subscription.StartTime.UTC()
	if subscription.EndTime != nil {
		endTime := subscription.EndTime.UTC()
		subscription.EndTime = &endTime
	}
	return subscription
}

func teamSubscriptionsInConn(ctx context.Context, conn server.PgCanQuery, sql string, args ...any) (subscriptions []*TeamSubscription) {
	result, err := conn.Query(ctx, sql, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			subscriptions = append(subscriptions, scanTeamSubscription(result))
		}
	})
	return
}

// Writes the Team's subscription row at its first paid invoice, in the transaction that credits it.
// written is false when the row is there already or the network holds another live row: a second
// first invoice, or a redelivery, changes nothing.
func AddTeamSubscriptionInTx(ctx context.Context, tx server.PgTx, subscription *TeamSubscription) (written bool) {
	if !server.SidecarMg6Linked() {
		return false
	}
	var endTime *time.Time
	if subscription.EndTime != nil {
		utc := subscription.EndTime.UTC()
		endTime = &utc
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO team_subscription (
				network_id,
				subscription_id,
				customer_id,
				anchor_time,
				start_time,
				end_time
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING
		`,
		subscription.NetworkId,
		subscription.SubscriptionId,
		subscription.CustomerId,
		subscription.AnchorTime.UTC(),
		subscription.StartTime.UTC(),
		endTime,
	))
	return tag.RowsAffected() == 1
}

// Ends the subscription's row at endTime if it is live. Returns whether it ended it: a rerun, or a
// subscription with no row, ends nothing.
func EndTeamSubscriptionRowInTx(ctx context.Context, tx server.PgTx, networkId server.Id, subscriptionId string, endTime time.Time) (ended bool) {
	if !server.SidecarMg6Linked() {
		return false
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE team_subscription
			SET end_time = $3
			WHERE
				network_id = $1 AND
				subscription_id = $2 AND
				end_time IS NULL
		`,
		networkId,
		subscriptionId,
		endTime.UTC(),
	))
	return tag.RowsAffected() == 1
}

// The network's row for the subscription, live or ended, or nil.
func GetTeamSubscriptionInTx(ctx context.Context, tx server.PgTx, networkId server.Id, subscriptionId string) *TeamSubscription {
	if !server.SidecarMg6Linked() {
		return nil
	}
	subscriptions := teamSubscriptionsInConn(
		ctx,
		tx,
		`SELECT `+teamSubscriptionColumns+` FROM team_subscription WHERE network_id = $1 AND subscription_id = $2`,
		networkId,
		subscriptionId,
	)
	if len(subscriptions) == 0 {
		return nil
	}
	return subscriptions[0]
}

// The network's row for the subscription, live or ended, or nil.
func GetTeamSubscription(ctx context.Context, networkId server.Id, subscriptionId string) (subscription *TeamSubscription) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Tx(ctx, func(tx server.PgTx) {
		subscription = GetTeamSubscriptionInTx(ctx, tx, networkId, subscriptionId)
	}, server.TxReadCommitted)
	return
}

// The network's live subscription, or nil.
func GetLiveTeamSubscriptionInConn(connOwner server.PgConn, ctx context.Context, networkId server.Id) (subscription *TeamSubscription) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		subscriptions := teamSubscriptionsInConn(
			ctx,
			conn,
			`SELECT `+teamSubscriptionColumns+` FROM team_subscription WHERE network_id = $1 AND end_time IS NULL`,
			networkId,
		)
		if 0 < len(subscriptions) {
			subscription = subscriptions[0]
		}
	})
	return
}

// The network's live subscription, or nil.
func GetLiveTeamSubscription(ctx context.Context, networkId server.Id) *TeamSubscription {
	return GetLiveTeamSubscriptionInConn(nil, ctx, networkId)
}

// The network's rows, oldest start first: every subscription it had, live or ended.
func GetTeamSubscriptions(ctx context.Context, networkId server.Id) (subscriptions []*TeamSubscription) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		subscriptions = teamSubscriptionsInConn(
			ctx,
			conn,
			`SELECT `+teamSubscriptionColumns+` FROM team_subscription WHERE network_id = $1 ORDER BY start_time, subscription_id`,
			networkId,
		)
	})
	return
}

// A page of live subscriptions by network id after after (nil: the first), at most limit: what the
// reconciler checks against Stripe.
func ListLiveTeamSubscriptionsInConn(connOwner server.PgConn, ctx context.Context, after *server.Id, limit int) (subscriptions []*TeamSubscription) {
	if !server.SidecarMg6Linked() || limit <= 0 {
		return nil
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		subscriptions = teamSubscriptionsInConn(
			ctx,
			conn,
			`
				SELECT `+teamSubscriptionColumns+`
				FROM team_subscription
				WHERE
					end_time IS NULL AND
					($1::uuid IS NULL OR $1 < network_id)
				ORDER BY network_id
				LIMIT $2
			`,
			after,
			limit,
		)
	})
	return
}

// Removes the network's rows in its deletion transaction, the one way a row goes.
func removeNetworkTeamSubscriptionsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM team_subscription WHERE network_id = $1`,
		networkId,
	))
}

// Whether the period that starts at periodStart has its team grant, live or ended.
func HasTeamGrantInTx(ctx context.Context, tx server.PgTx, networkId server.Id, periodStart time.Time) (written bool) {
	if !server.SidecarMg6Linked() {
		return false
	}
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
		networkId,
		periodStart.UTC(),
	).Scan(&written))
	return
}

// Takes the ownership every writer of these balances' amounts and windows takes, and raises when
// another holds one.
func ownTeamBalancesInTx(ctx context.Context, tx server.PgTx, balanceIds []server.Id) {
	admitted, err := tryTransferBalanceOwnershipInTx(ctx, tx, balanceIds)
	server.Raise(err)
	if !admitted {
		server.Raise(errTransferBalanceOwnershipBusy)
	}
}

func teamBalanceIdsInTx(ctx context.Context, tx server.PgTx, sql string, args ...any) (balanceIds []server.Id) {
	result, err := tx.Query(ctx, sql, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			server.Raise(result.Scan(&balanceId))
			balanceIds = append(balanceIds, balanceId)
		}
	})
	return
}

func endTeamBalancesInTx(ctx context.Context, tx server.PgTx, networkId server.Id, balanceIds []server.Id, now time.Time) (endedBalanceIds []server.Id) {
	if len(balanceIds) == 0 {
		return nil
	}
	ownTeamBalancesInTx(ctx, tx, balanceIds)
	return teamBalanceIdsInTx(
		ctx,
		tx,
		`
			UPDATE transfer_balance
			SET end_time = $3
			WHERE
				network_id = $1 AND
				balance_id = ANY($2::uuid[]) AND
				$3 < end_time
			RETURNING balance_id
		`,
		networkId,
		balanceIds,
		now.UTC(),
	)
}

// The network's credit lines that have not ended at now: those in window and any written for a
// period to come.
func teamCreditLineIdsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, now time.Time) []server.Id {
	return teamBalanceIdsInTx(
		ctx,
		tx,
		`
			SELECT balance_id
			FROM transfer_balance
			WHERE
				network_id = $1 AND
				grant_kind = $2 AND
				$3 < end_time
			ORDER BY balance_id
		`,
		networkId,
		GrantKindTeamCredit,
		now,
	)
}

// Ends every credit line of the network that has not ended, at now: the lines in window and any
// written for a period to come. The grants stay, so included bytes go on and overage refuses as
// an empty balance does. Returns the lines ended; a rerun ends none.
func EndTeamCreditLinesInTx(ctx context.Context, tx server.PgTx, networkId server.Id, now time.Time) (endedBalanceIds []server.Id) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	now = now.UTC()
	endedBalanceIds = endTeamBalancesInTx(ctx, tx, networkId, teamCreditLineIdsInTx(ctx, tx, networkId, now), now)
	if 0 < len(endedBalanceIds) {
		glog.Infof("[quota][team]%s credit lines ended: %d\n", networkId, len(endedBalanceIds))
	}
	return
}

// Gives each line that ended before its period's grant the grant's end again, for every period
// whose grant is still in its window: a line a payment failure ended comes back when the invoice
// is paid. A line whose grant ended stays ended, as does every line while the escrow_draw_order
// gate reads off, since a site would draw it in today's order (E-a). Returns the lines restored;
// a rerun restores none.
func RestoreTeamCreditLinesInTx(ctx context.Context, tx server.PgTx, networkId server.Id, now time.Time) (restoredBalanceIds []server.Id) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	if !EscrowDrawOrderEnabled() {
		glog.Infof("[quota][team]%s credit lines not restored: the escrow_draw_order gate reads off\n", networkId)
		return nil
	}
	now = now.UTC()
	balanceIds := teamBalanceIdsInTx(
		ctx,
		tx,
		`
			SELECT line.balance_id
			FROM transfer_balance line
			INNER JOIN transfer_balance team_grant ON
				team_grant.network_id = line.network_id AND
				team_grant.pro = true AND
				team_grant.tier = 'team' AND
				team_grant.start_time = line.start_time
			WHERE
				line.network_id = $1 AND
				line.grant_kind = $2 AND
				line.end_time < team_grant.end_time AND
				$3 < team_grant.end_time
			ORDER BY line.balance_id
		`,
		networkId,
		GrantKindTeamCredit,
		now,
	)
	if len(balanceIds) == 0 {
		return nil
	}
	ownTeamBalancesInTx(ctx, tx, balanceIds)
	restoredBalanceIds = teamBalanceIdsInTx(
		ctx,
		tx,
		`
			UPDATE transfer_balance line
			SET end_time = team_grant.end_time
			FROM transfer_balance team_grant
			WHERE
				line.network_id = $1 AND
				line.balance_id = ANY($2::uuid[]) AND
				team_grant.network_id = line.network_id AND
				team_grant.pro = true AND
				team_grant.tier = 'team' AND
				team_grant.start_time = line.start_time AND
				line.end_time < team_grant.end_time AND
				$3 < team_grant.end_time
			RETURNING line.balance_id
		`,
		networkId,
		balanceIds,
		now,
	)
	if 0 < len(restoredBalanceIds) {
		glog.Infof("[quota][team]%s credit lines restored: %d\n", networkId, len(restoredBalanceIds))
	}
	return
}

// The grant and line that have not ended at now of every period of the network that no paid
// renewal covers: those that start at or after the end of its newest team renewal, which
// RefreshTeamTransferBalances wrote at an anchor ahead of the period's invoice.
func unpaidTeamPeriodBalanceIdsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, now time.Time) []server.Id {
	var paidEndTime *time.Time
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT MAX(end_time)
			FROM subscription_renewal
			WHERE
				network_id = $1 AND
				subscription_type = $2
		`,
		networkId,
		SubscriptionTypeTeam,
	).Scan(&paidEndTime))
	if paidEndTime == nil {
		return nil
	}
	return teamBalanceIdsInTx(
		ctx,
		tx,
		`
			SELECT balance_id
			FROM transfer_balance
			WHERE
				network_id = $1 AND
				(grant_kind = $2 OR grant_kind = $3) AND
				$4 <= start_time AND
				$5 < end_time
			ORDER BY balance_id
		`,
		networkId,
		GrantKindTeam,
		GrantKindTeamCredit,
		paidEndTime.UTC(),
		now,
	)
}

// A Team's Stripe subscription ended: when its row is live, ends, at now, every credit line that
// has not ended and the grant of every period no paid renewal covers, and ends the row, so the job
// writes the network nothing more. A paid period keeps its grant to its end, so lapse follows Pro.
// Returns whether the row ended and the balances ended; a rerun, or a subscription with no live
// row (it never paid an invoice, or it ended already), ends nothing. The caller refreshes the tier
// after commit (UpdateProNetwork).
func EndTeamSubscriptionInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	subscriptionId string,
	now time.Time,
) (ended bool, endedBalanceIds []server.Id) {
	if !server.SidecarMg6Linked() {
		return false, nil
	}
	now = now.UTC()
	subscription := GetTeamSubscriptionInTx(ctx, tx, networkId, subscriptionId)
	if subscription == nil || subscription.EndTime != nil {
		return false, nil
	}
	// one key set for the transaction's one ownership probe
	balanceIds := teamCreditLineIdsInTx(ctx, tx, networkId, now)
	for _, balanceId := range unpaidTeamPeriodBalanceIdsInTx(ctx, tx, networkId, now) {
		if !slices.Contains(balanceIds, balanceId) {
			balanceIds = append(balanceIds, balanceId)
		}
	}
	endedBalanceIds = endTeamBalancesInTx(ctx, tx, networkId, balanceIds, now)
	ended = EndTeamSubscriptionRowInTx(ctx, tx, networkId, subscriptionId, now)
	glog.Infof("[quota][team]%s subscription ended, balances ended: %d\n", networkId, len(endedBalanceIds))
	return
}

// Whether a failed renewal of the network is on file and unresolved, which stops the next grant
// and keeps an ended line ended.
func HasOpenStripeInvoicePaymentFailureInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (open bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT EXISTS (
				SELECT 1
				FROM stripe_invoice_payment_failure
				WHERE
					network_id = $1 AND
					resolve_time IS NULL
			)
		`,
		networkId,
	).Scan(&open))
	return
}

// pro.yml's team: prices as a test sets them.
type Testing_TeamPrices struct {
	BaseUsd            float64
	SeatUsd            float64
	IncludedSeats      int64
	TransferTibUsd     float64
	StorageGibMonthUsd float64
}

// Stands in for pro.yml's team: prices, with pro's data and the credit line at 10 TiB, on the
// process's parsed config until the returned restore function runs. Same caveats as
// Testing_SetEnforceConcurrentClients. It works with no pro.yml, where every price reads zero.
func Testing_SetTeamPrices(prices Testing_TeamPrices) func() {
	c := Pro()
	prevData := c.Pro.Data
	prevTeam := c.Team
	c.Pro.Data = 10 * Tib
	c.Team.PriceBaseUsd = prices.BaseUsd
	c.Team.PriceSeatUsd = prices.SeatUsd
	c.Team.IncludedSeats = prices.IncludedSeats
	c.Team.CreditLine = 10 * Tib
	c.Team.overageTransferTibUsd = prices.TransferTibUsd
	c.Team.overageStorageGibMonthUsd = prices.StorageGibMonthUsd
	return func() {
		c.Pro.Data = prevData
		c.Team = prevTeam
	}
}
