package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/urnetwork/glog/v2025"
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

func FetchAccountPoints(ctx context.Context, networkId server.Id) (accountPoints []AccountPoint) {

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

type PlanPointsPayoutReport struct {
	TotalPayoutCount              int64                `json:"total_payout_count"`
	TotalPayoutNanoCents          int64                `json:"total_payout_nano_cents"`
	TotalUnscaledPayoutNanoPoints NanoPoints           `json:"total_unscaled_payout_nano_points"`
	TotalScaledPayoutNanoPoints   NanoPoints           `json:"total_scaled_nano_points,omitempty"`
	TotalBonusNanoPoints          NanoPoints           `json:"total_bonus_nano_points,omitempty"`
	ScaleFactor                   float64              `json:"scale_factor"`
	AccountPoints                 []AccountPointReport `json:"account_points"`
}

func PopulatePlanAccountPoints(ctx context.Context, planId server.Id) {

	// paymentPlanMap := map[server.Id][]AccountPayment{}

	accountPayments := []AccountPayment{}

	// network_id -> list of child referral networks
	networkReferrals := GetNetworkReferralsMap(ctx)

	// networkId -> parent referralNetworkId
	referralNetworks := map[server.Id]server.Id{}

	// seeker or saga holders
	seekerHolderNetworkIds := GetAllSeekerHolders(ctx)

	totalPayoutCount := int64(0)
	totalPayoutNanoCents := int64(0)
	totalPayoutUnscaledNanoPoints := NanoPoints(0)
	totalScaledNanoPoints := NanoPoints(0)
	totalBonusNanoPoints := NanoPoints(0)
	scaleFactor := float64(0.0)
	accountPointReports := []AccountPointReport{}

	subsidyConfig := EnvSubsidyConfig()

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
			WHERE account_payment.payment_plan_id = $1
				AND account_payment.completed = true
				AND account_payment.payout_nano_cents > 0
				AND account_payment.canceled = false
			`,
			planId,
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
				// paymentPlanMap[accountPayment.PaymentPlanId] = append(paymentPlanMap[accountPayment.PaymentPlanId], accountPayment)

				accountPayments = append(accountPayments, accountPayment)

				/**
				 * Network parent referral mapping
				 */
				if referralNetworkId != nil && *referralNetworkId != accountPayment.NetworkId {
					referralNetworks[accountPayment.NetworkId] = *referralNetworkId
				}

				totalPayoutNanoCents += accountPayment.Payout

				totalPayoutCount++

			}
		})

		for _, accountPayment := range accountPayments {
			totalPayoutUnscaledNanoPoints += PointsToNanoPoints(float64(accountPayment.Payout) / float64(totalPayoutNanoCents))
		}

		nanoPointsPerPayout := PointsToNanoPoints(float64(subsidyConfig.AccountPointsPerPayout))
		scaleFactor = (float64(nanoPointsPerPayout) / float64(totalPayoutUnscaledNanoPoints))

		for i, payment := range accountPayments {

			glog.Infof("%d/%d ------------", i, len(accountPayments))

			accountNanoPoints := PointsToNanoPoints(float64(payment.Payout) / float64(totalPayoutNanoCents))

			scaledAccountPoints := NanoPoints(scaleFactor * float64(accountNanoPoints))
			glog.Infof("[plan]payout %s with %d nano points (%d nano cents)\n",
				payment.NetworkId,
				scaledAccountPoints,
				payment.Payout,
			)

			accountPointsArgs := ApplyAccountPointsArgs{
				NetworkId:        payment.NetworkId,
				Event:            AccountPointEventPayout,
				PointValue:       scaledAccountPoints,
				PaymentPlanId:    &payment.PaymentPlanId,
				AccountPaymentId: &payment.PaymentId,
			}

			totalScaledNanoPoints += scaledAccountPoints

			ApplyAccountPoints(
				ctx,
				accountPointsArgs,
			)

			accountPointReports = append(accountPointReports, AccountPointReport{
				NetworkId:          payment.NetworkId,
				PaymentId:          payment.PaymentId,
				NanoCents:          payment.Payout,
				UnscaledNanoPoints: accountNanoPoints,
				ScaledNanoPoints:   scaledAccountPoints,
				ByteCount:          payment.PayoutByteCount,
				Event:              string(AccountPointEventPayout),
				// ReferralNetworkId: referralNetworks[payment.NetworkId],
			})

			/**
			 * Check seeker multipler
			 */
			if seekerHolderNetworkIds[payment.NetworkId] {
				seekerBonus := NanoPoints(float64(scaledAccountPoints)*subsidyConfig.SeekerHolderMultiplier) - scaledAccountPoints
				scaledAccountPoints = NanoPoints(float64(scaledAccountPoints) * subsidyConfig.SeekerHolderMultiplier) // apply the multiplier to the account points
				glog.Infof("[plan]payout seeker holder %s with %d bonus points\n", payment.NetworkId, seekerBonus)

				accountPointsArgs = ApplyAccountPointsArgs{
					NetworkId:        payment.NetworkId,
					Event:            AccountPointEventPayoutMultiplier,
					PointValue:       seekerBonus,
					PaymentPlanId:    &payment.PaymentPlanId,
					AccountPaymentId: &payment.PaymentId,
				}

				ApplyAccountPoints(ctx, accountPointsArgs)

				accountPointReports = append(accountPointReports, AccountPointReport{
					NetworkId: payment.NetworkId,
					PaymentId: payment.PaymentId,
					// NanoCents:  payment.Payout,
					ScaledNanoPoints: seekerBonus,
					// ByteCount:  payment.PayoutByteCount,
					Event: string(AccountPointEventPayoutMultiplier),
					// ReferralNetworkId: referralNetworks[payment.NetworkId],
				})

				totalBonusNanoPoints += seekerBonus
			}

			/**
			 * Apply bonus points to parent referral network
			 */
			parentReferralNetworkId, ok := referralNetworks[payment.NetworkId]

			if ok {
				if parentReferralNetworkId != payment.NetworkId {

					glog.Infof("[plan]payout applying referral parent network %s with %d points\n", parentReferralNetworkId, accountNanoPoints)

					parentReferralPoints := NanoPoints(float64(scaledAccountPoints) * subsidyConfig.ReferralParentPayoutFraction)

					accountPointsArgs = ApplyAccountPointsArgs{
						NetworkId:        parentReferralNetworkId,
						Event:            AccountPointEventPayoutLinkedAccount,
						PointValue:       parentReferralPoints,
						PaymentPlanId:    &payment.PaymentPlanId,
						LinkedNetworkId:  &payment.NetworkId,
						AccountPaymentId: &payment.PaymentId,
					}

					ApplyAccountPoints(ctx, accountPointsArgs)

					accountPointReports = append(accountPointReports, AccountPointReport{
						NetworkId: parentReferralNetworkId,
						PaymentId: payment.PaymentId,
						// NanoCents:  payment.Payout,
						ScaledNanoPoints: parentReferralPoints,
						// ByteCount:  payment.PayoutByteCount,
						Event:             string(AccountPointEventPayoutLinkedAccount),
						ReferralNetworkId: &payment.NetworkId,
					})

					totalBonusNanoPoints += parentReferralPoints

				}
			}

			glog.Infof("About to apply bonuses to child networks")

			/**
			 * Apply bonus points to child referral networks
			 */
			visited := make(map[server.Id]struct{})
			reports := payoutChildrenReferralNetworksInTx(
				ctx,
				tx,
				scaledAccountPoints,
				subsidyConfig.ReferralChildPayoutFraction,
				payment.NetworkId,
				networkReferrals,
				payment.PaymentPlanId,
				&payment.PaymentId,
				[]AccountPointReport{},
				0,
				visited,
			)

			for _, report := range reports {
				totalBonusNanoPoints += report.ScaledNanoPoints
			}

			glog.Infof("Applied bonuses to child networks, got %d reports", len(reports))

			accountPointReports = append(accountPointReports, reports...)

			glog.Infof("------------")
		}

	})

	report := PlanPointsPayoutReport{
		TotalPayoutCount:              totalPayoutCount,
		TotalPayoutNanoCents:          totalPayoutNanoCents,
		TotalScaledPayoutNanoPoints:   totalScaledNanoPoints,
		TotalUnscaledPayoutNanoPoints: totalPayoutUnscaledNanoPoints,
		TotalBonusNanoPoints:          totalBonusNanoPoints,
		ScaleFactor:                   scaleFactor,
		AccountPoints:                 accountPointReports,
	}

	// Write the PlanPointsPayoutReport to a JSON file
	jsonData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		glog.Errorf("Failed to marshal PlanPointsPayoutReport to JSON: %v", err)
		return
	}

	filename := fmt.Sprintf("plan_points_payout_report_%s.json", planId)
	err = os.WriteFile(filename, jsonData, 0644)
	if err != nil {
		glog.Errorf("Failed to write PlanPointsPayoutReport to file %s: %v", filename, err)
		return
	}

	glog.Infof("Successfully wrote PlanPointsPayoutReport to %s", filename)
}

type NetworkReliabilitySubsidy struct {
	Usdc   NanoCents
	Points NanoPoints
}

func calculateReliabilityPayout(
	ctx context.Context,
	subsidyStartTime time.Time,
	subsidyScale float64,
) (networkSubsidyAmounts map[server.Id]NetworkReliabilitySubsidy) {
	now := server.NowUtc()
	UpdateClientLocationReliabilities(ctx, subsidyStartTime, now)

	server.Tx(ctx, func(tx server.PgTx) {
		networkSubsidyAmounts = calculateReliabilityPayoutInTx(
			ctx,
			tx,
			subsidyStartTime,
			subsidyScale,
		)
	})
	return
}

/**
 * Returns map networkId -> NetworkReliabilitySubsidy
 * This is the amount of subsidy paid to each network for reliability
 */
func calculateReliabilityPayoutInTx(
	ctx context.Context,
	tx server.PgTx,
	subsidyStartTime time.Time,
	subsidyScale float64,
) map[server.Id]NetworkReliabilitySubsidy {

	now := server.NowUtc()
	networkReliabilitySubsidies := map[server.Id]NetworkReliabilitySubsidy{}

	// note `complete=false` is used here since dependencies are computed outside of the tx at the top of the plan function
	UpdateNetworkReliabilityScoresInTx(tx, ctx, subsidyStartTime, now, false)

	// get reliability scores
	reliabilityScores := GetAllMultipliedNetworkReliabilityScoresInTx(tx, ctx)

	reliabilityPointsPerPayout := PointsToNanoPoints(float64(EnvSubsidyConfig().ReliabilityPointsPerPayout) * subsidyScale)

	totalReliabilityWeight := 0.0

	for _, score := range reliabilityScores {
		totalReliabilityWeight += score.ReliabilityWeight
	}

	if totalReliabilityWeight == 0 {
		glog.Infof("[plan]no reliability scores found, skipping reliability payout")
		return networkReliabilitySubsidies
	}

	reliabilitySubsidyPerPayout := UsdToNanoCents(float64(EnvSubsidyConfig().ReliabilitySubsidyPerPayoutUsd))

	for networkId, score := range reliabilityScores {

		subsidy := NetworkReliabilitySubsidy{}
		weight := score.ReliabilityWeight

		/**
		 * Account Points
		 */
		if reliabilityPoints := float64(reliabilityPointsPerPayout) * float64(weight/totalReliabilityWeight); reliabilityPoints > 0 {
			subsidy.Points = NanoPoints(reliabilityPoints)
		}

		/*
		 * USDC subsidy
		 */

		if reliabilitySubsidyPerPayout > 0 {

			reliabilitySubsidy := NanoCents(float64(reliabilitySubsidyPerPayout) * (weight / totalReliabilityWeight))
			if reliabilitySubsidy >= 0 {
				subsidy.Usdc = reliabilitySubsidy
			}
		}

		networkReliabilitySubsidies[networkId] = subsidy

	}

	return networkReliabilitySubsidies

}

func payoutChildrenReferralNetworksInTx(
	ctx context.Context,
	tx server.PgTx,
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

		err := ApplyAccountPointsInTx(ctx, tx, args)
		if err != nil {
			glog.Errorf("[plan]could not apply referral points to %s: %v\n", childNetworkId, err)
		}

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
			tx,
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
