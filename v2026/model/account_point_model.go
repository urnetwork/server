package model

import (
	"context"
	"time"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

type AccountPointEvent string

/**
 * Point Events
 */
const (
	AccountPointEventPayout              AccountPointEvent = "payout"
	AccountPointEventPayoutLinkedAccount AccountPointEvent = "payout_linked_account"
	AccountPointEventPayoutMultiplier    AccountPointEvent = "payout_multiplier"
	AccountPointEventReliability         AccountPointEvent = "payout_reliability"
)

type AccountPoint struct {
	AccountPointId   server.Id  `json:"account_point_id"`
	NetworkId        server.Id  `json:"network_id"`
	Event            string     `json:"event"`
	PointValue       NanoPoints `json:"point_value"`
	PaymentPlanId    *server.Id `json:"payment_plan_id,omitempty"`
	AccountPaymentId *server.Id `json:"account_payment_id,omitempty"`
	LinkedNetworkId  *server.Id `json:"linked_network_id,omitempty"`
	CreateTime       time.Time  `json:"create_time"`
}

type ApplyAccountPointsArgs struct {
	NetworkId        server.Id
	Event            AccountPointEvent
	PointValue       NanoPoints
	AccountPaymentId *server.Id
	PaymentPlanId    *server.Id
	LinkedNetworkId  *server.Id
}

func ApplyAccountPoints(
	ctx context.Context,
	args ApplyAccountPointsArgs,
) (returnErr error) {

	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = ApplyAccountPointsInTx(ctx, tx, args)
	})

	return
}

func ApplyAccountPointsInTx(
	ctx context.Context,
	tx server.PgTx,
	args ApplyAccountPointsArgs,
) error {

	accountPointId := server.NewId()

	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO account_point (
					account_point_id,
					network_id,
					event,
					point_value,
					payment_plan_id,
					linked_network_id,
					account_payment_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`,
		accountPointId,
		args.NetworkId,
		args.Event,
		args.PointValue,
		args.PaymentPlanId,
		args.LinkedNetworkId,
		args.AccountPaymentId,
	))

	return nil
}

// ApplyAccountPointsBatchInTx inserts many account_point rows in a single
// batched round-trip instead of one Exec per row. The payment plan applies
// points to every payee plus their referral parents/children, which is
// thousands of rows on a large plan; queuing them as one batch is what keeps
// the plan transaction from paying a network round-trip per point.
func ApplyAccountPointsBatchInTx(
	ctx context.Context,
	tx server.PgTx,
	argsList []ApplyAccountPointsArgs,
) {
	if len(argsList) == 0 {
		return
	}

	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for _, args := range argsList {
			batch.Queue(
				`
					INSERT INTO account_point (
							account_point_id,
							network_id,
							event,
							point_value,
							payment_plan_id,
							linked_network_id,
							account_payment_id
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
				`,
				server.NewId(),
				args.NetworkId,
				args.Event,
				args.PointValue,
				args.PaymentPlanId,
				args.LinkedNetworkId,
				args.AccountPaymentId,
			)
		}
	})
}

func FetchAccountPoints(ctx context.Context, networkId server.Id) (accountPoints []AccountPoint) {
	// non-nil for the same reason as GetLeaderboard: a nil slice becomes JSON
	// `null`, which the gomobile binding turns into a nil pointer and the
	// android client crashes on. Feeds both `network_points` and its
	// `account_points` alias.
	accountPoints = []AccountPoint{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
						account_point_id,
						network_id,
						event,
						point_value,
						account_payment_id,
						payment_plan_id,
						linked_network_id,
						create_time
				FROM
						account_point
				WHERE
						network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {

				accountPoint := AccountPoint{}
				server.Raise(result.Scan(
					&accountPoint.AccountPointId,
					&accountPoint.NetworkId,
					&accountPoint.Event,
					&accountPoint.PointValue,
					&accountPoint.AccountPaymentId,
					&accountPoint.PaymentPlanId,
					&accountPoint.LinkedNetworkId,
					&accountPoint.CreateTime,
				))
				accountPoints = append(accountPoints, accountPoint)
			}
		})
	})

	return accountPoints
}

type AccountPointReport struct {
	NetworkId          server.Id  `json:"network_id"`
	PaymentId          server.Id  `json:"payment_id"`
	NanoCents          int64      `json:"nano_cents"`
	UnscaledNanoPoints NanoPoints `json:"unscaled_nano_points"`
	ScaledNanoPoints   NanoPoints `json:"nano_points"`
	ByteCount          int64      `json:"byte_count"`
	Event              string     `json:"event"`
	ReferralNetworkId  *server.Id `json:"referral_network_id,omitempty"`
}

// payoutChildrenReferralNetworksInTx walks the referral tree and hands each
// child's account_point row to `apply` rather than inserting it directly, so
// the caller decides how to persist: the plan collects the rows and inserts
// them as one batch, while tooling can apply them one at a time.
func payoutChildrenReferralNetworksInTx(
	ctx context.Context,
	apply func(ApplyAccountPointsArgs),
	basePayoutAmount NanoPoints,
	payoutFraction float64,
	networkId server.Id,
	networkReferrals map[server.Id][]server.Id,
	paymentPlanId server.Id,
	accountPaymentId *server.Id,
	accountPointsReports []AccountPointReport,
	depth int,
	visited map[server.Id]struct{},
) []AccountPointReport {

	glog.Infof("payout recursion depth is: %d", depth)
	glog.Infof("report count is: %d", len(accountPointsReports))

	if _, alreadyVisited := visited[networkId]; alreadyVisited {
		return accountPointsReports // Prevent cycles and double payouts
	}
	visited[networkId] = struct{}{}

	glog.Infof("[plan]payout %s for referral network %s has %d referrals with fraction %f\n",
		accountPaymentId,
		networkId,
		len(networkReferrals[networkId]),
		payoutFraction,
	)

	if len(networkReferrals[networkId]) == 0 {
		glog.Infof("[plan]payout referral network %s has no referrals, skipping\n", networkId)
		return accountPointsReports
	}

	childPayoutAmount := NanoPoints(float64(basePayoutAmount) * payoutFraction)
	if childPayoutAmount <= 0 {
		glog.Infof("[plan]payout referral network %s with 0 points, skipping\n", networkId)
		return accountPointsReports
	}

	for _, childNetworkId := range networkReferrals[networkId] {

		glog.Infof("[plan]child payout referral network %s with %d points\n", childNetworkId, childPayoutAmount)

		args := ApplyAccountPointsArgs{
			NetworkId:        childNetworkId,
			Event:            AccountPointEventPayoutLinkedAccount,
			PointValue:       childPayoutAmount,
			LinkedNetworkId:  &networkId,
			PaymentPlanId:    &paymentPlanId,
			AccountPaymentId: accountPaymentId,
		}

		apply(args)

		accountPointsReports = append(accountPointsReports, AccountPointReport{
			NetworkId:         childNetworkId,
			Event:             string(AccountPointEventPayoutLinkedAccount),
			ScaledNanoPoints:  childPayoutAmount,
			ReferralNetworkId: &networkId,
			PaymentId:         *accountPaymentId,
			// PaymentPlanId:   &paymentPlanId,
		})

		// recursively payout to child referral networks
		reports := payoutChildrenReferralNetworksInTx(
			ctx,
			apply,
			basePayoutAmount,
			payoutFraction*0.5, // reduce the payout fraction for each level of referral
			childNetworkId,
			networkReferrals,
			paymentPlanId,
			accountPaymentId,
			[]AccountPointReport{},
			depth+1,
			visited,
		)

		accountPointsReports = append(accountPointsReports, reports...)

	}

	return accountPointsReports

}
