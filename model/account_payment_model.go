package model

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	// "encoding/json"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type SubsidyConfig struct {
	Days                                      float64 `yaml:"days"`
	MinDaysFraction                           float64 `yaml:"min_days_fraction"`
	UsdPerActiveUser                          float64 `yaml:"usd_per_active_user"`
	SubscriptionNetRevenueFraction            float64 `yaml:"subscription_net_revenue_fraction"`
	MinPayoutUsd                              float64 `yaml:"min_payout_usd"`
	ActiveUserByteCountThresholdHumanReadable string  `yaml:"active_user_byte_count_threshold"`
	MaxPayoutUsdPerPaidUser                   float64 `yaml:"max_payout_usd_per_paid_user"`
	ReferralParentPayoutFraction              float64 `yaml:"referral_parent_payout_fraction"`
	ReferralChildPayoutFraction               float64 `yaml:"referral_child_payout_fraction"`
	AccountPointsPerPayout                    int     `yaml:"account_points_per_payout"`

	MinWalletPayoutUsd float64 `yaml:"min_wallet_payout_usd"`

	// hold onto unpaid amounts for up to this time
	// after this time, even if the value is below the threshold, the payment is created
	WalletPayoutTimeoutHumanReadable string `yaml:"wallet_payout_timeout"`
}

func (self *SubsidyConfig) ActiveUserByteCountThreshold() ByteCount {
	byteCount, err := ParseByteCount(self.ActiveUserByteCountThresholdHumanReadable)
	if err != nil {
		panic(err)
	}
	return byteCount
}

func (self *SubsidyConfig) WalletPayoutTimeout() time.Duration {
	timeout, err := time.ParseDuration(self.WalletPayoutTimeoutHumanReadable)
	if err != nil {
		panic(err)
	}
	return timeout
}

func (self *SubsidyConfig) MinDurationPerPayout() time.Duration {
	return time.Duration(self.Days*24*self.MinDaysFraction) * time.Hour
}

func (self *SubsidyConfig) Duration() time.Duration {
	return time.Duration(self.Days*24) * time.Hour
}

var EnvSubsidyConfig = sync.OnceValue(func() *SubsidyConfig {
	var subsidy SubsidyConfig
	server.Config.RequireSimpleResource("subsidy.yml").UnmarshalYaml(&subsidy)
	return &subsidy
})

type AccountPayment struct {
	PaymentId       server.Id  `json:"payment_id"`
	PaymentPlanId   server.Id  `json:"payment_plan_id"`
	WalletId        *server.Id `json:"wallet_id"`
	NetworkId       server.Id  `json:"network_id"`
	PayoutByteCount ByteCount  `json:"payout_byte_count"`
	Payout          NanoCents  `json:"payout_nano_cents"`
	// AccountPoints   NanoPoints `json:"account_points"`
	SubsidyPayout NanoCents `json:"subsidy_payout_nano_cents"`
	MinSweepTime  time.Time `json:"min_sweep_time"`
	CreateTime    time.Time `json:"create_time"`

	PaymentRecord  *string    `json:"payment_record"`
	TokenType      *string    `json:"token_type"`
	TokenAmount    *float64   `json:"token_amount"`
	PaymentTime    *time.Time `json:"payment_time"`
	PaymentReceipt *string    `json:"payment_receipt"`
	WalletAddress  *string    `json:"wallet_address"`

	Completed    bool       `json:"completed"`
	CompleteTime *time.Time `json:"complete_time"`

	Canceled   bool       `json:"canceled"`
	CancelTime *time.Time `json:"cancel_time"`
}

type EscrowId struct {
	ContractId server.Id
	BalanceId  server.Id
}

// `server.ComplexValue`
func (self *EscrowId) Values() []any {
	return []any{
		self.ContractId,
		self.BalanceId,
	}
}

func dbGetPayment(ctx context.Context, conn server.PgConn, paymentId server.Id) (payment *AccountPayment, returnErr error) {
	result, err := conn.Query(
		ctx,
		`
            SELECT
                account_payment.payment_plan_id,
                account_payment.network_id,
                account_payment.wallet_id,
                account_payment.payout_byte_count,
                account_payment.payout_nano_cents,
                account_payment.subsidy_payout_nano_cents,
                account_payment.min_sweep_time,
                account_payment.create_time,
                account_payment.payment_record,
                account_payment.token_type,
                account_payment.token_amount,
                account_payment.payment_time,
                account_payment.payment_receipt,
                account_payment.completed,
                account_payment.complete_time,
                account_payment.canceled,
                account_payment.cancel_time,
								account_wallet.wallet_address
            FROM account_payment

            LEFT JOIN account_wallet ON
                account_wallet.wallet_id = account_payment.wallet_id

            WHERE
                payment_id = $1
        `,
		paymentId,
	)
	if err != nil {
		returnErr = err
		return
	}

	server.WithPgResult(result, err, func() {

		if err != nil {
			returnErr = err
		}

		if result.Next() {
			payment = &AccountPayment{
				PaymentId: paymentId,
			}
			server.Raise(result.Scan(
				// &payment.PaymentId, // this was returning an empty id
				&payment.PaymentPlanId,
				&payment.NetworkId,
				&payment.WalletId,
				&payment.PayoutByteCount,
				&payment.Payout,
				&payment.SubsidyPayout,
				&payment.MinSweepTime,
				&payment.CreateTime,
				&payment.PaymentRecord,
				&payment.TokenType,
				&payment.TokenAmount,
				&payment.PaymentTime,
				&payment.PaymentReceipt,
				&payment.Completed,
				&payment.CompleteTime,
				&payment.Canceled,
				&payment.CancelTime,

				&payment.WalletAddress,
			))
		}
	})

	return
}

func GetPayment(ctx context.Context, paymentId server.Id) (payment *AccountPayment, err error) {
	server.Db(ctx, func(conn server.PgConn) {
		payment, err = dbGetPayment(ctx, conn, paymentId)
	})
	return
}

func GetPendingPayments(ctx context.Context) []*AccountPayment {
	payments := []*AccountPayment{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    payment_id
                FROM account_payment
                WHERE
                    completed = false AND canceled = false
            `,
		)
		paymentIds := []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				server.Raise(result.Scan(&paymentId))
				paymentIds = append(paymentIds, paymentId)
			}
		})

		for _, paymentId := range paymentIds {
			payment, err := dbGetPayment(ctx, conn, paymentId)
			if err != nil {
				glog.Errorf("[payment]could not load %s\n", paymentId)
			} else {
				payments = append(payments, payment)
			}
		}
	})

	return payments
}

func GetPendingPaymentsInPlan(ctx context.Context, paymentPlanId server.Id) []*AccountPayment {
	payments := []*AccountPayment{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    payment_id
                FROM account_payment
                WHERE
                    payment_plan_id = $1 AND
                    NOT completed AND NOT canceled
            `,
			paymentPlanId,
		)
		paymentIds := []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				server.Raise(result.Scan(&paymentId))
				paymentIds = append(paymentIds, paymentId)
			}
		})

		for _, paymentId := range paymentIds {
			payment, err := dbGetPayment(ctx, conn, paymentId)
			if err != nil {
				glog.Errorf("[payment]could not load %s in plan %s\n", paymentId, paymentPlanId)
			} else {
				payments = append(payments, payment)
			}
		}
	})

	return payments
}

func UpdatePaymentWallet(ctx context.Context, paymentId server.Id) {
	// note the wallet cannot be updated once there is a payment record
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE account_payment
			SET
				wallet_id = t.wallet_id
			FROM (
				SELECT
					account_payment.payment_id AS payment_id,
					account_wallet.wallet_id AS wallet_id
				FROM account_payment

				INNER JOIN payout_wallet ON
					payout_wallet.network_id = account_payment.network_id

			    INNER JOIN account_wallet ON
			        account_wallet.wallet_id = payout_wallet.wallet_id AND
			        account_wallet.active = true

		        WHERE
		        	account_payment.payment_id = $1 AND
		        	account_payment.payment_record IS NULL AND
		        	account_payment.completed = false AND
		        	account_payment.canceled = false

		    ) t
		    WHERE account_payment.payment_id = t.payment_id
			`,
			paymentId,
		))
	})
}

type PaymentPlan struct {
	PaymentPlanId server.Id
	// network_id -> payment
	NetworkPayments map[server.Id]*AccountPayment
	SubsidyPayment  *SubsidyPayment
	// these networks have pending payouts but were not paid due to any of
	// - thresholds
	// - missing wallets
	// - or other rules
	WithheldNetworkIds []server.Id
}

type SubsidyPayment struct {
	PaymentPlanId            server.Id
	StartTime                time.Time
	EndTime                  time.Time
	ActiveUserCount          int
	PaidUserCount            int
	NetPayoutByteCountPaid   ByteCount
	NetPayoutByteCountUnpaid ByteCount
	NetRevenue               NanoCents
	NetPayout                NanoCents
}

const SeekerHolderMultiplier = 2.0

// plan, manually check out and add balance to funding account, then complete
// minimum net_revenue_nano_cents to include in a payout
// all of the returned payments are tagged with the same payment_plan_id
func PlanPayments(ctx context.Context) (paymentPlan *PaymentPlan, returnErr error) {
	subsidyConfig := EnvSubsidyConfig()
	return PlanPaymentsWithConfig(ctx, subsidyConfig)
}

func PlanPaymentsWithConfig(ctx context.Context, subsidyConfig *SubsidyConfig) (paymentPlan *PaymentPlan, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {

		paymentPlanId := server.NewId()
		// networkId -> AccountPayment
		networkPayments := map[server.Id]*AccountPayment{}
		networkEscrowIds := map[server.Id][]*EscrowId{}

		// escrow ids -> payment id
		escrowPaymentIds := map[EscrowId]server.Id{}

		// networkId -> parent referralNetworkId
		referralNetworks := map[server.Id]server.Id{}

		// network_id -> list of child referral networks
		networkReferrals := GetNetworkReferralsMap(ctx)

		seekerHolderNetworkIds := GetAllSeekerHolders(ctx)

		server.RaisePgResult(tx.Exec(
			ctx,
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

		result, err := tx.Query(
			ctx,
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

				FOR UPDATE OF transfer_escrow_sweep
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
					referralNetworks[networkId] = *referralNetworkId
				}

				payment, ok := networkPayments[networkId]
				if !ok {
					paymentId := server.NewId()
					payment = &AccountPayment{
						PaymentId:     paymentId,
						PaymentPlanId: paymentPlanId,
						NetworkId:     networkId,
						// WalletId:      walletId,
						CreateTime: server.NowUtc(),
					}
					networkPayments[networkId] = payment
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
				escrowPaymentIds[escrowId] = payment.PaymentId
				networkEscrowIds[networkId] = append(networkEscrowIds[networkId], &escrowId)
			}
		})

		// log the network referral map
		glog.Infof("[plan]network referrals count: %d\n", len(referralNetworks))
		for networkId, referralNetworkId := range referralNetworks {
			glog.Infof("[plan]network %s referred by %s\n", networkId, referralNetworkId)
		}

		subsidyPayment, err := planSubsidyPaymentInTx(ctx, tx, subsidyConfig, paymentPlanId, networkPayments)
		if err != nil {
			returnErr = err
			return
		}
		// note `subsidyPayment` will be nil if the subsidy cannot be paid
		// this will be the case if there was already a subsidy payment to overlap this time range

		// fill in the payment wallet ids
		server.CreateTempTableInTx(
			ctx,
			tx,
			"temp_payment_network_ids(network_id uuid)",
			maps.Keys(networkPayments)...,
		)

		result, err = tx.Query(
			ctx,
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

				if payment, ok := networkPayments[networkId]; ok {
					payment.WalletId = walletId
				}
			}
		})

		// apply wallet minimum payout threshold
		// any wallet that does not meet the threshold will not be included in this plan
		networkIdsToRemove := []server.Id{}

		totalPayout := NanoCents(0)

		payoutExpirationTime := server.NowUtc().Add(-subsidyConfig.WalletPayoutTimeout())
		for networkId, payment := range networkPayments {
			// cannot remove payments that have `MinSweepTime <= payoutExpirationTime`
			if payment.Payout < UsdToNanoCents(subsidyConfig.MinWalletPayoutUsd) && payoutExpirationTime.Before(payment.MinSweepTime) {
				networkIdsToRemove = append(networkIdsToRemove, networkId)
			} else {
				// this payment will be included in the plan
				// note that the `MinSweepTime` is not used for the subsidy payment,
				// but it is used to determine the minimum sweep time for the payment
				totalPayout += payment.Payout
			}
		}
		removedSubsidyNetPayout := NanoCents(0)
		for _, networkId := range networkIdsToRemove {
			// remove the escrow payments for this wallet
			// so that the contracts do not have a dangling payment_id
			for _, escrowId := range networkEscrowIds[networkId] {
				delete(escrowPaymentIds, *escrowId)
			}

			// note the subsidy payments for this payment plan will be dropped
			// however, since the underlying contracts won't be marked as paid,
			// they will be included in the next subsidy payment plan,
			// although the subsidy payment amounts might not be the same
			payment := networkPayments[networkId]
			removedSubsidyNetPayout += payment.SubsidyPayout
			delete(networkPayments, networkId)
		}
		if subsidyPayment != nil {
			// adjust the subsidy payout to reflect removed payments
			subsidyPayment.NetPayout -= removedSubsidyNetPayout
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE subsidy_payment
					SET net_payout_nano_cents = net_payout_nano_cents - $2
					WHERE payment_plan_id = $1
				`,
				subsidyPayment.PaymentPlanId,
				removedSubsidyNetPayout,
			))
		}

		totalPayoutAccountNanoPoints := NanoPoints(0)

		// get total points
		for _, payment := range networkPayments {
			totalPayoutAccountNanoPoints += PointsToNanoPoints(float64(payment.Payout) / float64(totalPayout))
		}

		// // this is the total number of points allocated per payout
		// // should this factor in how frequent the payouts are?
		// // todo: should be moved into config
		nanoPointsPerPayout := PointsToNanoPoints(float64(EnvSubsidyConfig().AccountPointsPerPayout))

		// pointsScaleFactor := (float64(nanoPointsPerPayout) / float64(totalPayoutAccountNanoPoints)) / 1_000_000
		pointsScaleFactor := (float64(nanoPointsPerPayout) / float64(totalPayoutAccountNanoPoints))
		glog.Infof("[plan]total payout %d nano cents, total account points %d nano points, points scale factor %f\n",
			totalPayout,
			totalPayoutAccountNanoPoints,
			pointsScaleFactor,
		)

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, payment := range networkPayments {

				accountNanoPoints := PointsToNanoPoints(float64(payment.Payout) / float64(totalPayout))

				scaledAccountPoints := NanoPoints(pointsScaleFactor * float64(accountNanoPoints))
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

				ApplyAccountPoints(
					ctx,
					accountPointsArgs,
				)

				if seekerHolderNetworkIds[payment.NetworkId] {
					seekerBonus := scaledAccountPoints*SeekerHolderMultiplier - scaledAccountPoints
					scaledAccountPoints *= SeekerHolderMultiplier // apply the multiplier to the account points
					glog.Infof("[plan]payout seeker holder %s with %d bonus points\n", payment.NetworkId, seekerBonus)

					accountPointsArgs = ApplyAccountPointsArgs{
						NetworkId:        payment.NetworkId,
						Event:            AccountPointEventPayoutMultiplier,
						PointValue:       seekerBonus,
						PaymentPlanId:    &payment.PaymentPlanId,
						AccountPaymentId: &payment.PaymentId,
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
							NetworkId:        parentReferralNetworkId,
							Event:            AccountPointEventPayoutLinkedAccount,
							PointValue:       parentReferralPoints,
							PaymentPlanId:    &payment.PaymentPlanId,
							LinkedNetworkId:  &payment.NetworkId,
							AccountPaymentId: &payment.PaymentId,
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
					nil,
					0,
				)

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
									create_time
							)
							VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
					`,
					payment.PaymentId,
					payment.PaymentPlanId,
					payment.NetworkId,
					payment.WalletId,
					payment.PayoutByteCount,
					payment.Payout,
					// payment.AccountPoints,
					payment.SubsidyPayout,
					payment.MinSweepTime,
					payment.CreateTime,
				)
			}
		})

		server.CreateTempJoinTableInTx(
			ctx,
			tx,
			"payment_escrow_ids(contract_id uuid, balance_id uuid -> payment_id uuid)",
			escrowPaymentIds,
		)

		server.RaisePgResult(tx.Exec(
			ctx,
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

		paymentPlan = &PaymentPlan{
			PaymentPlanId:      paymentPlanId,
			NetworkPayments:    networkPayments,
			SubsidyPayment:     subsidyPayment,
			WithheldNetworkIds: networkIdsToRemove,
		}
	}, server.TxReadCommitted)

	return
}

func payoutChildrenReferralNetworks(
	ctx context.Context,
	basePayoutAmount NanoPoints,
	payoutFraction float64,
	networkId server.Id,
	networkReferrals map[server.Id][]server.Id,
	paymentPlanId server.Id,
	accountPaymentId server.Id,
	accountPointsReports []AccountPointReport,
	depth int,
) []AccountPointReport {

	glog.Infof("payout recursion depth is: %d", depth)
	glog.Infof("report count is: %d", len(accountPointsReports))

	glog.Infof("[plan]payout referral network %s has %d referrals with fraction %f\n",
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
			AccountPaymentId: &accountPaymentId,
		}

		err := ApplyAccountPoints(ctx, args)
		if err != nil {
			glog.Errorf("[plan]could not apply referral points to %s: %v\n", childNetworkId, err)
		}

		accountPointsReports = append(accountPointsReports, AccountPointReport{
			NetworkId:         childNetworkId,
			Event:             string(AccountPointEventPayoutLinkedAccount),
			ScaledNanoPoints:  childPayoutAmount,
			ReferralNetworkId: &networkId,
			// PaymentPlanId:   &paymentPlanId,
		})

		// recursively payout to child referral networks
		reports := payoutChildrenReferralNetworks(
			ctx,
			basePayoutAmount,
			payoutFraction*0.5, // reduce the payout fraction for each level of referral
			childNetworkId,
			networkReferrals,
			paymentPlanId,
			accountPaymentId,
			[]AccountPointReport{},
			depth+1,
		)

		accountPointsReports = append(accountPointsReports, reports...)

	}

	return accountPointsReports

}

// this assumes the table `temp_account_payment` exists in the transaction
func planSubsidyPaymentInTx(
	ctx context.Context,
	tx server.PgTx,
	subsidyConfig *SubsidyConfig,
	paymentPlanId server.Id,
	networkPayments map[server.Id]*AccountPayment,
) (subsidyPayment *SubsidyPayment, returnErr error) {
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

	result, err := tx.Query(
		ctx,
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

	result, err = tx.Query(
		ctx,
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

		if subsidyConfig.ActiveUserByteCountThreshold() <= payerNetPayoutByteCountPaid+payerNetPayoutByteCountUnpaid {
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
	result, err = tx.Query(
		ctx,
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
			subsidyStartTime = subsidyStartTime.Add(-time.Duration(subsidyConfig.Days) * 24 * time.Hour)
		}
	})

	if !subsidyStartTime.Before(subsidyEndTime) {
		// empty time range
		glog.Infof("[plan]subsidy empty\n")
		return
	}

	if subsidyEndTime.Sub(subsidyStartTime) < subsidyConfig.MinDurationPerPayout() {
		// does not meet minimum time range
		glog.Infof("[plan]subsidy short\n")
		return
	}

	// if the end time is contained in a subsidy, end
	// move the start time forward to the max end time that contains the start time,
	// and the end time backward to the min start time that contains the end time
	result, err = tx.Query(
		ctx,
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

	if subsidyEndTime.Sub(subsidyStartTime) < subsidyConfig.MinDurationPerPayout() {
		// does not meet minimum time range
		glog.Infof("[plan]subsidy adjusted short\n")
		return
	}

	subsidyPayoutUsd := max(
		subsidyConfig.MinPayoutUsd,
		subsidyConfig.UsdPerActiveUser*float64(activeUserCount),
		subsidyConfig.SubscriptionNetRevenueFraction*NanoCentsToUsd(netRevenue),
	)
	// the fraction of a `days` for this subsidy payout
	// restrict the time range to a single subsidy epoch
	subsidyScale := min(
		float64(subsidyEndTime.Sub(subsidyStartTime)/time.Minute)/float64(subsidyConfig.Duration()/time.Minute),
		1.0,
	)
	subsidyNetPayoutUsd := subsidyScale * subsidyPayoutUsd
	subsidyNetPayoutUsdPaid := min(subsidyNetPayoutUsd, float64(paidUserCount)*subsidyConfig.MaxPayoutUsdPerPaidUser)
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

				payment, ok := networkPayments[sweep.payeeNetworkId]
				if !ok {
					paymentId := server.NewId()
					payment = &AccountPayment{
						PaymentId:     paymentId,
						PaymentPlanId: paymentPlanId,
						NetworkId:     sweep.payeeNetworkId,
						// WalletId:      sweep.payoutWalletId,
						CreateTime: server.NowUtc(),
					}
					networkPayments[sweep.payeeNetworkId] = payment
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

	server.RaisePgResult(tx.Exec(
		ctx,
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
		paymentPlanId,
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
		paymentPlanId,
		float64(subsidyEndTime.Sub(subsidyStartTime))/float64(time.Hour*24),
		paidUserCount,
		activeUserCount,
		ByteCountHumanReadable(netPayoutByteCountPaid),
		ByteCountHumanReadable(netPayoutByteCountUnpaid),
		NanoCentsToUsd(netRevenue),
		NanoCentsToUsd(netPayout),
	)

	subsidyPayment = &SubsidyPayment{
		PaymentPlanId:            paymentPlanId,
		StartTime:                subsidyStartTime,
		EndTime:                  subsidyEndTime,
		ActiveUserCount:          activeUserCount,
		PaidUserCount:            paidUserCount,
		NetPayoutByteCountPaid:   netPayoutByteCountPaid,
		NetPayoutByteCountUnpaid: netPayoutByteCountUnpaid,
		NetRevenue:               netRevenue,
		NetPayout:                netPayout,
	}
	return
}

func GetSubsidyPayment(ctx context.Context, paymentPlanId server.Id) (paymentPlan *SubsidyPayment) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
			        start_time,
			        end_time,
			        active_user_count,
			        paid_user_count,
			        net_payout_byte_count_paid,
			        net_payout_byte_count_unpaid,
			        net_revenue_nano_cents,
			        net_payout_nano_cents
				FROM subsidy_payment
				WHERE payment_plan_id = $1
			`,
			paymentPlanId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				paymentPlan = &SubsidyPayment{
					PaymentPlanId: paymentPlanId,
				}
				server.Raise(result.Scan(
					&paymentPlan.StartTime,
					&paymentPlan.EndTime,
					&paymentPlan.ActiveUserCount,
					&paymentPlan.PaidUserCount,
					&paymentPlan.NetPayoutByteCountPaid,
					&paymentPlan.NetPayoutByteCountUnpaid,
					&paymentPlan.NetRevenue,
					&paymentPlan.NetPayout,
				))
			}
		})
	})
	return
}

// set the record before submitting to the processor
// the controller should check if the payment already has a record before processing -
//
//	these are in a bad state and need to be investigated manually
func SetPaymentRecord(
	ctx context.Context,
	paymentId server.Id,
	tokenType string,
	tokenAmount float64,
	paymentRecord string,
) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE account_payment
                SET
                    token_type = $2,
                    token_amount = $3,
                    payment_record = $4,
                    payment_time = $5
                WHERE
                    payment_id = $1 AND
                    NOT completed AND NOT canceled
            `,
			paymentId,
			tokenType,
			tokenAmount,
			paymentRecord,
			server.NowUtc(),
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Invalid payment.")
			return
		}
	})
	return
}

func RemovePaymentRecord(
	ctx context.Context,
	paymentId server.Id,
) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE account_payment
                SET
                    payment_record = NULL
                WHERE
                    payment_id = $1 AND
                    NOT completed AND NOT canceled
            `,
			paymentId,
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Invalid payment.")
			return
		}
	})
	return
}

func CompletePayment(ctx context.Context, paymentId server.Id, paymentReceipt string) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE account_payment
                SET
                    payment_receipt = $2,
                    completed = true,
                    complete_time = $3
                WHERE
                    payment_id = $1 AND
                    NOT completed AND NOT canceled
            `,
			paymentId,
			paymentReceipt,
			server.NowUtc(),
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Invalid payment.")
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE account_balance
                SET
                    paid_byte_count = paid_byte_count + account_payment.payout_byte_count,
                    paid_net_revenue_nano_cents = paid_net_revenue_nano_cents + account_payment.payout_nano_cents
                FROM account_payment
                WHERE
                    account_payment.payment_id = $1 AND
                    account_balance.network_id = account_payment.network_id
            `,
			paymentId,
		))
	})
	return
}

func CancelPayment(ctx context.Context, paymentId server.Id) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE account_payment
                SET
                    canceled = true,
                    cancel_time = $2
                WHERE
                    payment_id = $1 AND
                    NOT completed AND NOT canceled
            `,
			paymentId,
			server.NowUtc(),
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Invalid payment.")
			return
		}
	})
	return
}

// used in bringyourctl to apply a bonus to a payment plan
func PayoutPlanApplyBonus(
	ctx context.Context,
	paymentPlanId server.Id,
	bonusNanoCents NanoCents,
) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE account_payment
                SET
                    payout_nano_cents = payout_nano_cents + $2
                WHERE
                    payment_plan_id = $1 AND
                    NOT completed AND NOT canceled
            `,
			paymentPlanId,
			bonusNanoCents,
		))
		if tag.RowsAffected() == 0 {
			returnErr = fmt.Errorf("invalid payment plan")
			return
		}
	})
	return
}

func GetNetworkPayments(session *session.ClientSession) ([]*AccountPayment, error) {

	networkPayments := []*AccountPayment{}

	server.Tx(session.Ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			session.Ctx,
			`
            SELECT
				account_payment.payment_id,
                account_payment.payment_plan_id,
                account_payment.network_id,
                account_payment.wallet_id,
                account_payment.payout_byte_count,
                account_payment.payout_nano_cents,
                account_payment.subsidy_payout_nano_cents,
                account_payment.min_sweep_time,
                account_payment.create_time,
                account_payment.payment_record,
                account_payment.token_type,
                account_payment.token_amount,
                account_payment.payment_time,
                account_payment.payment_receipt,
                account_payment.completed,
                account_payment.complete_time,
                account_payment.canceled,
                account_payment.cancel_time,
				account_wallet.wallet_address
            FROM account_payment

            LEFT JOIN account_wallet ON
                account_wallet.wallet_id = account_payment.wallet_id

            WHERE
                account_payment.network_id = $1 AND
                canceled = false
        `,
			session.ByJwt.NetworkId,
		)

		server.WithPgResult(result, err, func() {

			for result.Next() {
				payment := &AccountPayment{}

				server.Raise(result.Scan(
					&payment.PaymentId,
					&payment.PaymentPlanId,
					&payment.NetworkId,
					&payment.WalletId,
					&payment.PayoutByteCount,
					&payment.Payout,
					&payment.SubsidyPayout,
					&payment.MinSweepTime,
					&payment.CreateTime,
					&payment.PaymentRecord,
					&payment.TokenType,
					&payment.TokenAmount,
					&payment.PaymentTime,
					&payment.PaymentReceipt,
					&payment.Completed,
					&payment.CompleteTime,
					&payment.Canceled,
					&payment.CancelTime,
					&payment.WalletAddress,
				))

				networkPayments = append(networkPayments, payment)

			}
		})
	})

	slices.SortFunc(networkPayments, func(a *AccountPayment, b *AccountPayment) int {
		// descending in create time
		return -a.CreateTime.Compare(b.CreateTime)
	})

	return networkPayments, nil

}

type TransferStats struct {
	PaidBytesProvided   ByteCount `json:"paid_bytes_provided"`
	UnpaidBytesProvided ByteCount `json:"unpaid_bytes_provided"`
}

/**
 * Total paid and unpaid bytes for a network
 * This is not live data, and depends on transfer_escrow_sweep
 */
func GetTransferStats(
	ctx context.Context,
	networkId server.Id,
) *TransferStats {

	var transferStats *TransferStats

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					coalesce(SUM(CASE
							WHEN account_payment.payment_id IS NOT NULL AND account_payment.canceled = false THEN transfer_escrow_sweep.payout_byte_count
							ELSE 0
						END), 0) as paid_bytes_provided,
					coalesce(SUM(CASE
							WHEN account_payment.payment_id IS NULL OR account_payment.canceled = true THEN transfer_escrow_sweep.payout_byte_count
							ELSE 0
						END), 0) as unpaid_bytes_provided
				FROM
					transfer_escrow_sweep
				LEFT JOIN account_payment
					ON transfer_escrow_sweep.payment_id = account_payment.payment_id
				WHERE
					transfer_escrow_sweep.network_id = $1
			`,
			networkId,
		)

		server.WithPgResult(result, err, func() {

			if result.Next() {

				transferStats = &TransferStats{}

				server.Raise(
					result.Scan(
						&transferStats.PaidBytesProvided,
						&transferStats.UnpaidBytesProvided,
					),
				)
			}
		})
	})

	return transferStats
}
