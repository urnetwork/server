package model

import (
	"context"
	"fmt"
	// "slices"
	// "sync"
	"time"

	// "encoding/json"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	// "github.com/urnetwork/server/v2025/session"
)

// the payment planner computes a payout in one large transaction
// the coponents of the payout are:
// 1. escrow net revenue share (the `NetRevenue` part of the escrow is 100% transferred to the provider)
//    note all `NetRevenue` will be zero when fully migrated to points/token,
//    since the point/token system is based on a pooled "subsidy" payout each period instead of pass through revenue share
// 2. transfer+reliability subsidy
// 3. (only if there is a subsidy) proportional points and referral shares (points only)

type PaymentPlanner struct {
	ctx           context.Context
	subsidyConfig *SubsidyConfig

	// all the planning is done inside a transaction
	tx server.PgTx

	paymentPlanId server.Id

	networkReferrals       map[server.Id][]server.Id
	seekerHolderNetworkIds map[server.Id]bool

	// networkId -> AccountPayment
	networkPayments map[server.Id]*AccountPayment

	networkEscrowIds map[server.Id][]*EscrowId

	// escrow ids -> payment id
	escrowPaymentIds map[EscrowId]server.Id

	// networkId -> parent referralNetworkId
	referralNetworks map[server.Id]server.Id

	networkReliabilitySubsidies map[server.Id]NetworkReliabilitySubsidy

	networkIdsToRemove []server.Id

	paymentPlan    *PaymentPlan
	subsidyPayment *SubsidyPayment
}

func CreatePaymentPlan(ctx context.Context, subsidyConfig *SubsidyConfig) (paymentPlan *PaymentPlan, returnErr error) {
	now := server.NowUtc()
	UpdateClientLocationReliabilities(ctx, now.Add(-30*24*time.Hour), now)

	// network_id -> list of child referral networks
	networkReferrals := GetNetworkReferralsMap(ctx)
	seekerHolderNetworkIds := GetAllSeekerHolders(ctx)

	server.Tx(ctx, func(tx server.PgTx) {
		planner := &PaymentPlanner{
			ctx:                    ctx,
			subsidyConfig:          subsidyConfig,
			tx:                     tx,
			paymentPlanId:          server.NewId(),
			networkReferrals:       networkReferrals,
			seekerHolderNetworkIds: seekerHolderNetworkIds,
			networkPayments:        map[server.Id]*AccountPayment{},
		}

		returnErr = planner.planPayments()
		if returnErr != nil {
			return
		}

		// note `subsidyPayment` will be nil if the subsidy cannot be paid
		// this will be the case if there was already a subsidy payment to overlap this time range
		returnErr = planner.planSubsidyPayments()
		if returnErr != nil {
			return
		}

		planner.withholdSmallPayments()
		planner.setWallets()

		totalPayout := NanoCents(0)
		for _, payment := range planner.networkPayments {
			totalPayout += payment.Payout
		}
		if planner.subsidyPayment != nil && 0 < planner.subsidyPayment.SubsidyScale && 0 < totalPayout {
			planner.applyPayoutPoints(
				planner.subsidyPayment.SubsidyScale,
				totalPayout,
			)
			// `ForcePoints` can be set to test points when there is no subsidy
		} else if subsidyConfig.ForcePoints {
			planner.applyPayoutPoints(
				1.0,
				totalPayout,
			)
		}

		planner.finalizePayments()

		paymentPlan = &PaymentPlan{
			PaymentPlanId:      planner.paymentPlanId,
			NetworkPayments:    planner.networkPayments,
			SubsidyPayment:     planner.subsidyPayment,
			WithheldNetworkIds: planner.networkIdsToRemove,
		}

		// set the bonus weights for next payout
		UpdateClientLocationReliabilityMultipliersWithDefaultsInTx(tx, ctx)

	}, server.TxReadCommitted)

	return
}

func (self *PaymentPlanner) planPayments() (returnErr error) {
	self.networkEscrowIds = map[server.Id][]*EscrowId{}

	// escrow ids -> payment id
	self.escrowPaymentIds = map[EscrowId]server.Id{}

	// networkId -> parent referralNetworkId
	self.referralNetworks = map[server.Id]server.Id{}

	server.RaisePgResult(self.tx.Exec(
		self.ctx,
		`
        CREATE TEMPORARY TABLE temp_account_payment ON COMMIT DROP

        AS

        SELECT
            transfer_escrow_sweep.contract_id,
            transfer_escrow_sweep.balance_id

        FROM transfer_escrow_sweep

        LEFT JOIN account_payment ON
            account_payment.payment_id = transfer_escrow_sweep.payment_id

        WHERE
            account_payment.payment_id IS NULL OR
            account_payment.canceled = true
        `,
	))

	result, err := self.tx.Query(
		self.ctx,
		`
		SELECT
			transfer_escrow_sweep.network_id,
			network_referral.referral_network_id,
			transfer_escrow_sweep.contract_id,
			transfer_escrow_sweep.balance_id,
			transfer_escrow_sweep.payout_byte_count,
			transfer_escrow_sweep.payout_net_revenue_nano_cents,
			transfer_escrow_sweep.sweep_time

		FROM transfer_escrow_sweep

		LEFT JOIN network_referral
      	ON transfer_escrow_sweep.network_id = network_referral.network_id

		INNER JOIN temp_account_payment ON
				temp_account_payment.contract_id = transfer_escrow_sweep.contract_id AND
				temp_account_payment.balance_id = transfer_escrow_sweep.balance_id
  		`,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			var referralNetworkId *server.Id
			var contractId server.Id
			var balanceId server.Id
			var payoutByteCount ByteCount
			var payoutNetRevenue NanoCents
			var sweepTime time.Time
			// var walletId *server.Id
			server.Raise(result.Scan(
				&networkId,
				&referralNetworkId,
				&contractId,
				&balanceId,
				&payoutByteCount,
				&payoutNetRevenue,
				&sweepTime,
				// &walletId,
			))

			/**
			 * Assign the network referral, if a referring network exists
			 */
			if referralNetworkId != nil && *referralNetworkId != networkId {
				self.referralNetworks[networkId] = *referralNetworkId
			}

			payment, ok := self.networkPayments[networkId]
			if !ok {
				paymentId := server.NewId()
				payment = &AccountPayment{
					PaymentId:     paymentId,
					PaymentPlanId: self.paymentPlanId,
					NetworkId:     networkId,
					// WalletId:      walletId,
					CreateTime: server.NowUtc(),
				}
				self.networkPayments[networkId] = payment
			}
			payment.PayoutByteCount += payoutByteCount
			payment.Payout += payoutNetRevenue

			if payment.MinSweepTime.IsZero() {
				payment.MinSweepTime = sweepTime
			} else {
				payment.MinSweepTime = server.MinTime(payment.MinSweepTime, sweepTime)
			}

			escrowId := EscrowId{
				ContractId: contractId,
				BalanceId:  balanceId,
			}
			self.escrowPaymentIds[escrowId] = payment.PaymentId
			self.networkEscrowIds[networkId] = append(self.networkEscrowIds[networkId], &escrowId)
		}
	})

	// log the network referral map
	glog.Infof("[plan]network referrals count: %d\n", len(self.referralNetworks))
	for networkId, referralNetworkId := range self.referralNetworks {
		glog.Infof("[plan]network %s referred by %s\n", networkId, referralNetworkId)
	}

	return
}

// this assumes the table `temp_account_payment` exists in the transaction
func (self *PaymentPlanner) planSubsidyPayments() (returnErr error) {
	// roll up all the sweeps per payer network, payee network
	type networkSweep struct {
		payerNetworkId           server.Id
		payeeNetworkId           server.Id
		minSweepTime             time.Time
		netPayoutByteCountPaid   ByteCount
		netPayoutByteCountUnpaid ByteCount
		// payoutWalletId           server.Id
		payout NanoCents
	}

	payerPayeeNetworkSweeps := map[server.Id]map[server.Id]*networkSweep{}
	payerSubsidyNetRevenues := map[server.Id]NanoCents{}

	result, err := self.tx.Query(
		self.ctx,
		`
        	SELECT
            	transfer_balance.network_id AS payer_network_id,
                transfer_escrow_sweep.network_id AS payee_network_id,
                MIN(transfer_escrow_sweep.sweep_time) AS min_sweep_time,
                SUM(CASE
                	WHEN transfer_balance.paid THEN transfer_escrow_sweep.payout_byte_count
                	ELSE 0
                END) AS net_payout_byte_count_paid,
                SUM(CASE
                	WHEN NOT transfer_balance.paid THEN transfer_escrow_sweep.payout_byte_count
                	ELSE 0
                END) AS net_payout_byte_count_unpaid

            FROM transfer_escrow_sweep

            INNER JOIN temp_account_payment ON
                temp_account_payment.contract_id = transfer_escrow_sweep.contract_id AND
                temp_account_payment.balance_id = transfer_escrow_sweep.balance_id

            INNER JOIN transfer_balance ON
                transfer_balance.balance_id = transfer_escrow_sweep.balance_id

            GROUP BY
            	transfer_balance.network_id,
            	transfer_escrow_sweep.network_id

        `,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			sweep := &networkSweep{
				payout: NanoCents(0),
			}
			server.Raise(result.Scan(
				&sweep.payerNetworkId,
				&sweep.payeeNetworkId,
				&sweep.minSweepTime,
				&sweep.netPayoutByteCountPaid,
				&sweep.netPayoutByteCountUnpaid,
				// &sweep.payoutWalletId,
			))
			payeeNetworkSweeps, ok := payerPayeeNetworkSweeps[sweep.payerNetworkId]
			if !ok {
				payeeNetworkSweeps = map[server.Id]*networkSweep{}
				payerPayeeNetworkSweeps[sweep.payerNetworkId] = payeeNetworkSweeps
			}
			payeeNetworkSweeps[sweep.payeeNetworkId] = sweep
		}
	})

	result, err = self.tx.Query(
		self.ctx,
		`
			SELECT
	    		transfer_balance.network_id AS payer_network_id,
	    		SUM(transfer_balance.subsidy_net_revenue_nano_cents) AS subsidy_net_revenue_nano_cents
	    	FROM (
	    		SELECT
	    			DISTINCT balance_id
	    		FROM temp_account_payment
	    	) b
	    	INNER JOIN transfer_balance ON
	    		transfer_balance.balance_id = b.balance_id
	    	GROUP BY transfer_balance.network_id
	    `,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var payerNetworkId server.Id
			var subscriptionNetRevenue NanoCents
			server.Raise(result.Scan(&payerNetworkId, &subscriptionNetRevenue))
			payerSubsidyNetRevenues[payerNetworkId] = subscriptionNetRevenue
		}
	})

	netPayoutByteCountPaid := ByteCount(0)
	netPayoutByteCountUnpaid := ByteCount(0)
	activeUserCount := 0
	paidUserCount := 0
	unpaidUserCount := 0
	netRevenue := NanoCents(0)
	for payerNetworkId, payeeNetworkSweeps := range payerPayeeNetworkSweeps {
		payerNetPayoutByteCountPaid := ByteCount(0)
		payerNetPayoutByteCountUnpaid := ByteCount(0)
		for _, sweep := range payeeNetworkSweeps {
			payerNetPayoutByteCountPaid += sweep.netPayoutByteCountPaid
			payerNetPayoutByteCountUnpaid += sweep.netPayoutByteCountUnpaid
		}

		netPayoutByteCountPaid += payerNetPayoutByteCountPaid
		netPayoutByteCountUnpaid += payerNetPayoutByteCountUnpaid

		if self.subsidyConfig.ActiveUserByteCountThreshold() <= payerNetPayoutByteCountPaid+payerNetPayoutByteCountUnpaid {
			activeUserCount += 1
		}
		if 0 < payerNetPayoutByteCountPaid {
			paidUserCount += 1
		}
		if 0 < payerNetPayoutByteCountUnpaid {
			unpaidUserCount += 1
		}
		netRevenue += payerSubsidyNetRevenues[payerNetworkId]
	}

	var subsidyStartTime time.Time
	var subsidyEndTime time.Time
	result, err = self.tx.Query(
		self.ctx,
		`
    	SELECT
            MIN(transfer_contract.create_time) AS subsidy_start_time,
            MAX(transfer_contract.close_time) AS subsidy_end_time

        FROM transfer_escrow_sweep

        INNER JOIN transfer_contract ON
        	transfer_contract.contract_id = transfer_escrow_sweep.contract_id

        `,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&subsidyStartTime, &subsidyEndTime))
		} else {
			subsidyEndTime = server.NowUtc()
			subsidyStartTime = subsidyStartTime.Add(-time.Duration(self.subsidyConfig.Days) * 24 * time.Hour)
		}
	})

	if !subsidyStartTime.Before(subsidyEndTime) {
		// empty time range
		glog.Infof("[plan]subsidy empty\n")
		return
	}

	if subsidyEndTime.Sub(subsidyStartTime) < self.subsidyConfig.MinDurationPerPayout() {
		// does not meet minimum time range
		glog.Infof("[plan]subsidy short\n")
		return
	}

	// if the end time is contained in a subsidy, end
	// move the start time forward to the max end time that contains the start time,
	// and the end time backward to the min start time that contains the end time
	result, err = self.tx.Query(
		self.ctx,
		`
		SELECT
			start_time,
			end_time
		FROM subsidy_payment
		WHERE start_time < $2 AND $1 < end_time
		`,
		subsidyStartTime,
		subsidyEndTime,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var existingSubsidyStartTime time.Time
			var existingSubsidyEndTime time.Time
			server.Raise(result.Scan(&existingSubsidyStartTime, &existingSubsidyEndTime))

			if !subsidyStartTime.After(existingSubsidyStartTime) && subsidyStartTime.Before(existingSubsidyEndTime) {
				subsidyStartTime = existingSubsidyEndTime
			}
			if !subsidyEndTime.After(existingSubsidyStartTime) && subsidyEndTime.Before(existingSubsidyEndTime) {
				subsidyEndTime = existingSubsidyStartTime
			}
			if existingSubsidyStartTime.Before(subsidyEndTime) && subsidyStartTime.Before(existingSubsidyEndTime) {
				// existing is contained (split)
				// use the most recent time region for this subsidy
				subsidyStartTime = existingSubsidyEndTime
			}
		}
	})

	if !subsidyStartTime.Before(subsidyEndTime) {
		// the subsidy is contained in existing subsidies
		// returnErr = fmt.Errorf("Planned subsidy overlaps with an existing subsdidy. Cannot double pay subsidies.")
		glog.Infof("[plan]subsidy overlap\n")
		return
	}

	if subsidyEndTime.Sub(subsidyStartTime) < self.subsidyConfig.MinDurationPerPayout() {
		// does not meet minimum time range
		glog.Infof("[plan]subsidy adjusted short\n")
		return
	}

	subsidyPayoutUsd := max(
		self.subsidyConfig.MinPayoutUsd,
		self.subsidyConfig.UsdPerActiveUser*float64(activeUserCount),
		self.subsidyConfig.SubscriptionNetRevenueFraction*NanoCentsToUsd(netRevenue),
	)
	// the fraction of a `days` for this subsidy payout
	// restrict the time range to a single subsidy epoch
	subsidyScale := min(
		float64(subsidyEndTime.Sub(subsidyStartTime)/time.Minute)/float64(self.subsidyConfig.Duration()/time.Minute),
		1.0,
	)
	subsidyNetPayoutUsd := subsidyScale * subsidyPayoutUsd
	subsidyNetPayoutUsdPaid := min(subsidyNetPayoutUsd, float64(paidUserCount)*self.subsidyConfig.MaxPayoutUsdPerPaidUser)
	subsidyNetPayoutUsdUnpaid := subsidyNetPayoutUsd - subsidyNetPayoutUsdPaid
	glog.Infof("[plan]payout $%.2f ($%.2f/$%.2f)\n", subsidyNetPayoutUsd, subsidyNetPayoutUsdPaid, subsidyNetPayoutUsdUnpaid)

	// this is added up as the exact net payment, which may be rounded from `subsidyNetPayout`
	netPayout := NanoCents(0)

	for _, payeeNetworkSweeps := range payerPayeeNetworkSweeps {
		netPayerPayoutByteCountPaid := ByteCount(0)
		netPayerPayoutByteCountUnpaid := ByteCount(0)
		for _, sweep := range payeeNetworkSweeps {
			netPayerPayoutByteCountPaid += sweep.netPayoutByteCountPaid
			netPayerPayoutByteCountUnpaid += sweep.netPayoutByteCountUnpaid
		}

		for _, sweep := range payeeNetworkSweeps {
			// each user is weighted equally, relative to their own traffic
			if 0 < netPayerPayoutByteCountPaid {
				paidWeight := float64(sweep.netPayoutByteCountPaid) / float64(netPayerPayoutByteCountPaid)
				sweep.payout += UsdToNanoCents(paidWeight * subsidyNetPayoutUsdPaid / float64(paidUserCount))
			}
			if 0 < netPayerPayoutByteCountUnpaid {
				unpaidWeight := float64(sweep.netPayoutByteCountUnpaid) / float64(netPayerPayoutByteCountUnpaid)
				sweep.payout += UsdToNanoCents(unpaidWeight * subsidyNetPayoutUsdUnpaid / float64(unpaidUserCount))
			}
			netPayout += sweep.payout
		}
	}

	// +1 for rounding errors
	if UsdToNanoCents(subsidyNetPayoutUsd+1) < netPayout {
		glog.Infof("[plan]net payment exceeded\n")
		returnErr = fmt.Errorf("Planned subsidy pays out more than the allowed amount: %d <> %d", UsdToNanoCents(subsidyNetPayoutUsd), netPayout)
		return
	}

	for _, payeeNetworkSweeps := range payerPayeeNetworkSweeps {
		for _, sweep := range payeeNetworkSweeps {
			if 0 < sweep.payout {

				payment, ok := self.networkPayments[sweep.payeeNetworkId]
				if !ok {
					paymentId := server.NewId()
					payment = &AccountPayment{
						PaymentId:     paymentId,
						PaymentPlanId: self.paymentPlanId,
						NetworkId:     sweep.payeeNetworkId,
						// WalletId:      sweep.payoutWalletId,
						CreateTime: server.NowUtc(),
					}
					self.networkPayments[sweep.payeeNetworkId] = payment
				}
				payment.Payout += sweep.payout
				payment.SubsidyPayout += sweep.payout

				if payment.MinSweepTime.IsZero() {
					payment.MinSweepTime = sweep.minSweepTime
				} else {
					payment.MinSweepTime = server.MinTime(payment.MinSweepTime, sweep.minSweepTime)
				}
			}
		}
	}

	/**
	 * reliability payout
	 */
	networkReliabilitySubsidies := calculateReliabilityPayoutInTx(
		self.ctx,
		self.tx,
		subsidyStartTime,
		subsidyScale,
	)
	self.networkReliabilitySubsidies = networkReliabilitySubsidies

	/**
	 * Add the reliability subsidies
	 * We do this after calculating totalPayoutAccountNanoPoints and totalPayout so the bonus doesn't skew the points
	 */
	for networkId, reliabilitySubsidy := range networkReliabilitySubsidies {
		payment, ok := self.networkPayments[networkId]
		if !ok {
			glog.Errorf("[plan]no payment found for network %s, but reliability subsidy is %d nano cents\n", networkId, reliabilitySubsidy)
			continue
		}

		// add the reliability subsidy to the payment
		payment.Payout += reliabilitySubsidy.Usdc
	}

	server.RaisePgResult(self.tx.Exec(
		self.ctx,
		`
			INSERT INTO subsidy_payment (
				payment_plan_id,
		        start_time,
		        end_time,
		        active_user_count,
		        paid_user_count,
		        net_payout_byte_count_paid,
		        net_payout_byte_count_unpaid,
		        net_revenue_nano_cents,
		        net_payout_nano_cents
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`,
		self.paymentPlanId,
		subsidyStartTime,
		subsidyEndTime,
		activeUserCount,
		paidUserCount,
		netPayoutByteCountPaid,
		netPayoutByteCountUnpaid,
		netRevenue,
		netPayout,
	))

	glog.Infof(
		"[plan][%s]subsidy %.2fd paid=%du/active=%du paid=%s+unpaid=%s: $%.2f revenue -> $%.2f paid\n",
		self.paymentPlanId,
		float64(subsidyEndTime.Sub(subsidyStartTime))/float64(time.Hour*24),
		paidUserCount,
		activeUserCount,
		ByteCountHumanReadable(netPayoutByteCountPaid),
		ByteCountHumanReadable(netPayoutByteCountUnpaid),
		NanoCentsToUsd(netRevenue),
		NanoCentsToUsd(netPayout),
	)

	self.subsidyPayment = &SubsidyPayment{
		PaymentPlanId:            self.paymentPlanId,
		StartTime:                subsidyStartTime,
		EndTime:                  subsidyEndTime,
		ActiveUserCount:          activeUserCount,
		PaidUserCount:            paidUserCount,
		NetPayoutByteCountPaid:   netPayoutByteCountPaid,
		NetPayoutByteCountUnpaid: netPayoutByteCountUnpaid,
		NetRevenue:               netRevenue,
		NetPayout:                netPayout,
		SubsidyScale:             subsidyScale,
	}
	return
}

func (self *PaymentPlanner) withholdSmallPayments() {

	// apply wallet minimum payout threshold
	// any wallet that does not meet the threshold will not be included in this plan
	self.networkIdsToRemove = []server.Id{}

	payoutExpirationTime := server.NowUtc().Add(-self.subsidyConfig.WalletPayoutTimeout())
	for networkId, payment := range self.networkPayments {
		// cannot remove payments that have `MinSweepTime <= payoutExpirationTime`
		if payment.Payout < UsdToNanoCents(self.subsidyConfig.MinWalletPayoutUsd) && payoutExpirationTime.Before(payment.MinSweepTime) {
			self.networkIdsToRemove = append(self.networkIdsToRemove, networkId)
		}
		// else this payment will be included in the plan
		// note that the `MinSweepTime` is not used for the subsidy payment,
		// but it is used to determine the minimum sweep time for the payment
	}
	removedSubsidyNetPayout := NanoCents(0)
	for _, networkId := range self.networkIdsToRemove {
		// remove the escrow payments for this wallet
		// so that the contracts do not have a dangling payment_id
		for _, escrowId := range self.networkEscrowIds[networkId] {
			delete(self.escrowPaymentIds, *escrowId)
		}

		// note the subsidy payments for this payment plan will be dropped
		// however, since the underlying contracts won't be marked as paid,
		// they will be included in the next subsidy payment plan,
		// although the subsidy payment amounts might not be the same
		payment := self.networkPayments[networkId]
		removedSubsidyNetPayout += payment.SubsidyPayout
		delete(self.networkPayments, networkId)
	}
	if self.subsidyPayment != nil {
		// adjust the subsidy payout to reflect removed payments
		self.subsidyPayment.NetPayout -= removedSubsidyNetPayout
		server.RaisePgResult(self.tx.Exec(
			self.ctx,
			`
				UPDATE subsidy_payment
				SET net_payout_nano_cents = net_payout_nano_cents - $2
				WHERE payment_plan_id = $1
			`,
			self.subsidyPayment.PaymentPlanId,
			removedSubsidyNetPayout,
		))
	}
}

func (self *PaymentPlanner) setWallets() {

	// fill in the payment wallet ids
	server.CreateTempTableInTx(
		self.ctx,
		self.tx,
		"temp_payment_network_ids(network_id uuid)",
		maps.Keys(self.networkPayments)...,
	)

	result, err := self.tx.Query(
		self.ctx,
		`
		SELECT
			temp_payment_network_ids.network_id,
			account_wallet.wallet_id
		FROM temp_payment_network_ids

	    LEFT JOIN payout_wallet ON
            payout_wallet.network_id = temp_payment_network_ids.network_id

        LEFT JOIN account_wallet ON
            account_wallet.wallet_id = payout_wallet.wallet_id AND
            account_wallet.active = true
		`,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			var walletId *server.Id
			server.Raise(result.Scan(
				&networkId,
				&walletId,
			))

			if payment, ok := self.networkPayments[networkId]; ok {
				payment.WalletId = walletId
			}
		}
	})
}

func (self *PaymentPlanner) finalizePayments() {

	server.BatchInTx(self.ctx, self.tx, func(batch server.PgBatch) {
		for _, payment := range self.networkPayments {
			batch.Queue(
				`
				INSERT INTO account_payment (
						payment_id,
						payment_plan_id,
						network_id,
						wallet_id,
						payout_byte_count,
						payout_nano_cents,
						subsidy_payout_nano_cents,
						min_sweep_time,
						create_time,
						reliability_subsidy_nano_cents
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				`,
				payment.PaymentId,
				payment.PaymentPlanId,
				payment.NetworkId,
				payment.WalletId,
				payment.PayoutByteCount,
				payment.Payout,
				payment.SubsidyPayout,
				payment.MinSweepTime,
				payment.CreateTime,
				self.networkReliabilitySubsidies[payment.NetworkId].Usdc,
			)
		}
	})

	server.CreateTempJoinTableInTx(
		self.ctx,
		self.tx,
		"payment_escrow_ids(contract_id uuid, balance_id uuid -> payment_id uuid)",
		self.escrowPaymentIds,
	)

	server.RaisePgResult(self.tx.Exec(
		self.ctx,
		`
            UPDATE transfer_escrow_sweep
            SET
                payment_id = payment_escrow_ids.payment_id
            FROM payment_escrow_ids
            WHERE
                transfer_escrow_sweep.contract_id = payment_escrow_ids.contract_id AND
                transfer_escrow_sweep.balance_id = payment_escrow_ids.balance_id
        `,
	))
}

// points are proportional to payout
func (self *PaymentPlanner) applyPayoutPoints(
	subsidyScale float64,
	totalPayout NanoCents,
) {
	/**
	 * Safeguard against division by zero
	 */
	if totalPayout <= 0 {
		return
	}

	pointsPerPayout := self.subsidyConfig.AccountPointsPerPayout

	// this is the total number of points for this payment plan
	totalPoints := float64(pointsPerPayout) * subsidyScale
	glog.Infof("[plan]total payout %d nano cents, total points %.2f\n",
		totalPayout,
		totalPoints,
	)

	/**
	 * create report
	 * todo - add min data threshold for payout
	 */
	server.RaisePgResult(self.tx.Exec(
		self.ctx,
		`
		INSERT INTO payment_report (
				payment_plan_id,
				total_payout_nano_cents,
				total_nano_points,
				point_scale_factor,
				time_scale_factor,
				payout_points_per_payout

		)
		VALUES ($1, $2, $3, $4, $5, $6)
		`,
		self.paymentPlanId,
		totalPayout,
		PointsToNanoPoints(totalPoints),
		totalPoints,
		subsidyScale,
		pointsPerPayout,
	))

	// TODO the `ApplyAccountPointsInTx` could be done in a batch to speed up the plan

	/**
	 * Apply reliability points
	 */
	for networkId, reliabilitySubsidy := range self.networkReliabilitySubsidies {

		payment, ok := self.networkPayments[networkId]
		if !ok {
			glog.Errorf("[plan]no payment found for network %s, but reliability subsidy is %d nano cents\n", networkId, reliabilitySubsidy)
			continue
		}

		if reliabilitySubsidy.Points > 0 {
			reliabilityPointsArgs := ApplyAccountPointsArgs{
				NetworkId:        payment.NetworkId,
				Event:            AccountPointEventReliability,
				PointValue:       reliabilitySubsidy.Points,
				AccountPaymentId: &payment.PaymentId,
				PaymentPlanId:    &self.paymentPlanId,
			}
			ApplyAccountPointsInTx(self.ctx, self.tx, reliabilityPointsArgs)
		}
	}

	for _, payment := range self.networkPayments {

		/**
		 * Payout account points
		 */
		// accountNanoPoints := PointsToNanoPoints(float64(payment.Payout) / float64(totalPayout))

		// scaledAccountPoints := NanoPoints(pointsScaleFactor * float64(accountNanoPoints))

		scaledAccountPoints := PointsToNanoPoints(totalPoints * (float64(payment.Payout) / float64(totalPayout)))
		glog.Infof("[plan]payout %s with %d nano points (%d nano cents)\n",
			payment.NetworkId,
			scaledAccountPoints,
			payment.Payout,
		)

		if scaledAccountPoints > 0 {

			accountPointsArgs := ApplyAccountPointsArgs{
				NetworkId:        payment.NetworkId,
				Event:            AccountPointEventPayout,
				PointValue:       scaledAccountPoints,
				PaymentPlanId:    &payment.PaymentPlanId,
				AccountPaymentId: &payment.PaymentId,
			}

			ApplyAccountPointsInTx(
				self.ctx,
				self.tx,
				accountPointsArgs,
			)

		}

		/**
		 * Seeker bonus
		 */
		if self.seekerHolderNetworkIds[payment.NetworkId] {
			seekerBonus := NanoPoints(float64(scaledAccountPoints)*self.subsidyConfig.SeekerHolderMultiplier) - scaledAccountPoints
			scaledAccountPoints = NanoPoints(float64(scaledAccountPoints) * self.subsidyConfig.SeekerHolderMultiplier) // apply the multiplier to the account points

			// double reliability points for seeker holders
			seekerBonus += self.networkReliabilitySubsidies[payment.NetworkId].Points
			glog.Infof("[plan]payout seeker holder %s with %d bonus points\n", payment.NetworkId, seekerBonus)

			accountPointsArgs := ApplyAccountPointsArgs{
				NetworkId:        payment.NetworkId,
				Event:            AccountPointEventPayoutMultiplier,
				PointValue:       seekerBonus,
				PaymentPlanId:    &payment.PaymentPlanId,
				AccountPaymentId: &payment.PaymentId,
			}

			ApplyAccountPointsInTx(self.ctx, self.tx, accountPointsArgs)
		}

		/**
		 * Apply bonus points to parent referral network
		 */
		parentReferralNetworkId, ok := self.referralNetworks[payment.NetworkId]

		if ok {
			if parentReferralNetworkId != payment.NetworkId {
				parentReferralPoints := NanoPoints(float64(scaledAccountPoints) * self.subsidyConfig.ReferralParentPayoutFraction)

				glog.Infof("[plan]payout applying referral parent network %s with %d points\n", parentReferralNetworkId, scaledAccountPoints)

				accountPointsArgs := ApplyAccountPointsArgs{
					NetworkId:        parentReferralNetworkId,
					Event:            AccountPointEventPayoutLinkedAccount,
					PointValue:       parentReferralPoints,
					PaymentPlanId:    &payment.PaymentPlanId,
					LinkedNetworkId:  &payment.NetworkId,
					AccountPaymentId: &payment.PaymentId,
				}

				ApplyAccountPointsInTx(self.ctx, self.tx, accountPointsArgs)
			}
		}

		/**
		 * Apply bonus points to child referral networks
		 */
		visited := make(map[server.Id]struct{})
		payoutChildrenReferralNetworksInTx(
			self.ctx,
			self.tx,
			scaledAccountPoints,
			EnvSubsidyConfig().ReferralChildPayoutFraction,
			payment.NetworkId,
			self.networkReferrals,
			payment.PaymentPlanId,
			&payment.PaymentId,
			nil,
			0,
			visited,
		)
	}

}
