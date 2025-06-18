package model

import (
	"context"
	"time"

	"github.com/golang/glog"
	"github.com/urnetwork/server/v2025"
)

type AccountPointEvent string

/**
 * Point Events
 */
const (
	AccountPointEventPayout              AccountPointEvent = "payout"
	AccountPointEventPayoutLinkedAccount AccountPointEvent = "payout_linked_account"
	AccountPointEventPayoutMultiplier    AccountPointEvent = "payout_multiplier"
)

type AccountPoint struct {
	NetworkId        server.Id  `json:"network_id"`
	Event            string     `json:"event"`
	PointValue       int        `json:"point_value"`
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
) error {

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_point (
						network_id,
						event,
						point_value,
						payment_plan_id,
						linked_network_id,
						account_payment_id
				)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			args.NetworkId,
			args.Event,
			args.PointValue,
			args.PaymentPlanId,
			args.LinkedNetworkId,
			args.AccountPaymentId,
		))
	})

	return nil
}

func FetchAccountPoints(ctx context.Context, networkId server.Id) (accountPoints []AccountPoint) {

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
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

/**
 * To be run once to populate points from May 2025
 * Delete after running
 */
func PopulateAccountPoints(ctx context.Context) {

	paymentPlanMap := map[server.Id][]AccountPayment{}

	// network_id -> list of child referral networks
	networkReferrals := GetNetworkReferralsMap(ctx)

	// networkId -> parent referralNetworkId
	referralNetworks := map[server.Id]server.Id{}

	// seeker or saga holders
	seekerHolderNetworkIds := GetAllSeekerHolders(ctx)

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
	        SELECT
				account_payment.payment_id,
				account_payment.payment_plan_id,
				account_payment.network_id,
				account_payment.payout_byte_count,
				account_payment.payout_nano_cents,
				network_referral.referral_network_id
			FROM account_payment
			LEFT JOIN network_referral ON account_payment.network_id = network_referral.network_id
			WHERE account_payment.create_time > '2025-05-15 00:00:00'
				AND account_payment.completed = true
				AND account_payment.payout_nano_cents > 0
				AND account_payment.cancelled = false
			`,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {

				var accountPayment AccountPayment
				var referralNetworkId *server.Id

				server.Raise(result.Scan(
					&accountPayment.PaymentId,
					&accountPayment.PaymentPlanId,
					&accountPayment.NetworkId,
					&accountPayment.PayoutByteCount,
					&accountPayment.Payout,
					&referralNetworkId,
				))

				/**
				 * Map payments to their payment plans
				 */
				paymentPlanMap[accountPayment.PaymentPlanId] = append(paymentPlanMap[accountPayment.PaymentPlanId], accountPayment)

				/**
				 * Network parent referral mapping
				 */
				if referralNetworkId != nil && *referralNetworkId != accountPayment.NetworkId {
					referralNetworks[accountPayment.NetworkId] = *referralNetworkId
				}

			}
		})

		for paymentPlanId, accountPayments := range paymentPlanMap {
			glog.Infof("Payment Plan ID: %s has $d payments", paymentPlanId, len(accountPayments))

			totalPayoutNanoCents := int64(0)

			/**
			 * Calculate total payout for the payment plan
			 */
			for _, accountPayment := range accountPayments {
				totalPayoutNanoCents += accountPayment.Payout
			}

			glog.Infof("[plan]total payout for payment plan %s is %d nano cents\n", paymentPlanId, totalPayoutNanoCents)

			totalPayoutAccountNanoPoints := NanoPoints(0)

			// get total points
			for _, payment := range accountPayments {
				totalPayoutAccountNanoPoints += PointsToNanoPoints(float64(payment.Payout) / float64(totalPayoutNanoCents))
			}

			glog.Infof("[plan]total payout account nano points %d\n", totalPayoutAccountNanoPoints)

			nanoPointsPerPayout := PointsToNanoPoints(float64(EnvSubsidyConfig().AccountPointsPerPayout))
			pointsScaleFactor := (float64(nanoPointsPerPayout) / float64(totalPayoutAccountNanoPoints))

			glog.Infof("[plan]total payout %d nano cents, total account points %d nano points, points scale factor %f\n",
				totalPayoutNanoCents,
				totalPayoutAccountNanoPoints,
				pointsScaleFactor,
			)

			for _, payment := range accountPayments {

				accountNanoPoints := PointsToNanoPoints(float64(payment.Payout) / float64(totalPayoutNanoCents))

				scaledAccountPoints := NanoPoints(pointsScaleFactor * float64(accountNanoPoints))
				glog.Infof("[plan]payout %s with %d nano points (%d nano cents)\n",
					payment.NetworkId,
					scaledAccountPoints,
					payment.Payout,
				)

				accountPointsArgs := ApplyAccountPointsArgs{
					NetworkId:     payment.NetworkId,
					Event:         AccountPointEventPayout,
					PointValue:    scaledAccountPoints,
					PaymentPlanId: &payment.PaymentPlanId,
				}

				ApplyAccountPoints(
					ctx,
					accountPointsArgs,
				)

				/**
				 * Check seeker multipler
				 */
				if seekerHolderNetworkIds[payment.NetworkId] {
					seekerBonus := scaledAccountPoints*SeekerHolderMultiplier - scaledAccountPoints
					scaledAccountPoints *= SeekerHolderMultiplier // apply the multiplier to the account points
					glog.Infof("[plan]payout seeker holder %s with %d bonus points\n", payment.NetworkId, seekerBonus)

					accountPointsArgs = ApplyAccountPointsArgs{
						NetworkId:     payment.NetworkId,
						Event:         AccountPointEventPayoutMultiplier,
						PointValue:    seekerBonus,
						PaymentPlanId: &payment.PaymentPlanId,
					}

					ApplyAccountPoints(ctx, accountPointsArgs)
				}

				/**
				 * Apply bonus points to parent referral network
				 */
				parentReferralNetworkId, ok := referralNetworks[payment.NetworkId]

				if ok {
					if parentReferralNetworkId != payment.NetworkId {

						glog.Infof("[plan]payout applying referral parent network %s with %d points\n", parentReferralNetworkId, accountNanoPoints)

						parentReferralPoints := NanoPoints(float64(scaledAccountPoints) * EnvSubsidyConfig().ReferralParentPayoutFraction)

						accountPointsArgs = ApplyAccountPointsArgs{
							NetworkId:       parentReferralNetworkId,
							Event:           AccountPointEventPayoutLinkedAccount,
							PointValue:      parentReferralPoints,
							PaymentPlanId:   &payment.PaymentPlanId,
							LinkedNetworkId: &payment.NetworkId,
						}

						ApplyAccountPoints(ctx, accountPointsArgs)
					}
				}

				/**
				 * Apply bonus points to child referral networks
				 */
				payoutChildrenReferralNetworks(
					ctx,
					scaledAccountPoints,
					EnvSubsidyConfig().ReferralChildPayoutFraction,
					payment.NetworkId,
					networkReferrals,
					payment.PaymentPlanId,
					payment.PaymentId,
				)

			}

		}

	})

}
