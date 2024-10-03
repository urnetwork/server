package model

import (
	"context"
	"fmt"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
)


type SubsidyConfig struct {
	Days int 					`json:"days"`
	MinDaysFraction float64     `json:"min_days_fraction"`
	UsdPerActiveUser float64 	`json:"usd_per_active_user"`
	SubscriptionNetRevenueFraction float64 	`json:"subscription_net_revenue_fraction"`
	MinPayoutUsd  float64  `json:"min_payout_usd"`
	ActiveUserByteCountThreshold  ByteCount  `json:"active_user_byte_count_threshold"`
}

envSubsidyConfig := sync.OnceValue(func()(*SubsidyConfig) {
	var subsidy SubsidyConfig
	json.Unmarshal(bringyour.ConfigValue("subsidy.yml"), &subsidy)
	return &subsidy
})




type AccountPayment struct {
	PaymentId       bringyour.Id `json:"payment_id"`
	PaymentPlanId   bringyour.Id `json:"payment_plan_id"`
	WalletId        bringyour.Id `json:"wallet_id"`
	NetworkId       bringyour.Id `json:"network_id"`
	PayoutByteCount ByteCount    `json:"payout_byte_count"`
	Payout          NanoCents    `json:"payout_nano_cents"`
	MinSweepTime    time.Time    `json:"min_sweep_time"`
	CreateTime      time.Time    `json:"create_time"`

	PaymentRecord  *string    `json:"payment_record"`
	TokenType      *string    `json:"token_type"`
	TokenAmount    *float64   `json:"token_amount"`
	PaymentTime    *time.Time `json:"payment_time"`
	PaymentReceipt *string    `json:"payment_receipt"`

	Completed    bool       `json:"completed"`
	CompleteTime *time.Time `json:"complete_time"`

	Canceled   bool       `json:"canceled"`
	CancelTime *time.Time `json:"cancel_time"`
}

func dbGetPayment(ctx context.Context, conn bringyour.PgConn, paymentId bringyour.Id) (payment *AccountPayment, returnErr error) {
	result, err := conn.Query(
		ctx,
		`
            SELECT
                account_payment.payment_plan_id,
                account_payment.wallet_id,
                account_payment.payout_byte_count,
                account_payment.payout_nano_cents,
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
                account_wallet.network_id
            FROM account_payment

            INNER JOIN account_wallet ON
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

	bringyour.WithPgResult(result, err, func() {

		if err != nil {
			returnErr = err
		}

		if result.Next() {
			payment = &AccountPayment{
				PaymentId: paymentId,
			}
			bringyour.Raise(result.Scan(
				// &payment.PaymentId, // this was returning an empty id
				&payment.PaymentPlanId,
				&payment.WalletId,
				&payment.PayoutByteCount,
				&payment.Payout,
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
				&payment.NetworkId,
			))
		}
	})

	return
}

func GetPayment(ctx context.Context, paymentId bringyour.Id) (payment *AccountPayment, err error) {
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		payment, err = dbGetPayment(ctx, conn, paymentId)
	})
	return
}

func GetPendingPayments(ctx context.Context) []*AccountPayment {
	payments := []*AccountPayment{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    payment_id
                FROM account_payment
                WHERE
                    NOT completed AND NOT canceled
            `,
		)
		paymentIds := []bringyour.Id{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId bringyour.Id
				bringyour.Raise(result.Scan(&paymentId))
				paymentIds = append(paymentIds, paymentId)
			}
		})

		for _, paymentId := range paymentIds {
			payment, _ := dbGetPayment(ctx, conn, paymentId)
			if payment != nil {
				payments = append(payments, payment)
			}
		}
	})

	return payments
}

func GetPendingPaymentsInPlan(ctx context.Context, paymentPlanId bringyour.Id) []*AccountPayment {
	payments := []*AccountPayment{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
		paymentIds := []bringyour.Id{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId bringyour.Id
				bringyour.Raise(result.Scan(&paymentId))
				paymentIds = append(paymentIds, paymentId)
			}
		})

		for _, paymentId := range paymentIds {
			payment, _ := dbGetPayment(ctx, conn, paymentId)
			if payment != nil {
				payments = append(payments, payment)
			}
		}
	})

	return payments
}

type PaymentPlan struct {
	PaymentPlanId bringyour.Id
	// wallet_id -> payment
	WalletPayments map[bringyour.Id]*AccountPayment
	// these wallets have pending payouts but were not paid due to thresholds or other rules
	WithheldWalletIds []bringyour.Id
}

// plan, manually check out and add balance to funding account, then complete
// minimum net_revenue_nano_cents to include in a payout
// all of the returned payments are tagged with the same payment_plan_id
func PlanPayments(ctx context.Context) (paymentPlan *PaymentPlan, returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {

		

		
		paymentPlanId := bringyour.NewId()
		// walletId -> AccountPayment
		walletPayments := map[bringyour.Id]*AccountPayment{}

		// escrow ids -> payment id
		escrowPaymentIds := map[EscrowId]bringyour.Id{}

		bringyour.RaisePgResult(tx.Exec(
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
                NOT account_payment.completed AND account_payment.canceled
            `,
		))

		result, err := tx.Query(
			ctx,
			`
            SELECT
                transfer_escrow_sweep.contract_id,
                transfer_escrow_sweep.balance_id,
                transfer_escrow_sweep.payout_byte_count,
                transfer_escrow_sweep.payout_net_revenue_nano_cents,
                transfer_escrow_sweep.sweep_time,
                payout_wallet.wallet_id

            FROM transfer_escrow_sweep

            INNER JOIN temp_account_payment ON
                temp_account_payment.contract_id = transfer_escrow_sweep.contract_id AND
                temp_account_payment.balance_id = transfer_escrow_sweep.balance_id

            INNER JOIN payout_wallet ON
                payout_wallet.network_id = transfer_escrow_sweep.network_id

            INNER JOIN account_wallet ON
                account_wallet.wallet_id = payout_wallet.wallet_id AND
                account_wallet.active = true

            FOR UPDATE
            `,
		)

		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId bringyour.Id
				var balanceId bringyour.Id
				var payoutByteCount ByteCount
				var payoutNetRevenue NanoCents
				var sweepTime time.Time
				var walletId bringyour.Id
				bringyour.Raise(result.Scan(
					&contractId,
					&balanceId,
					&payoutByteCount,
					&payoutNetRevenue,
					&sweepTime,
					&walletId,
				))

				payment, ok := walletPayments[walletId]
				if !ok {
					paymentId := bringyour.NewId()
					payment = &AccountPayment{
						PaymentId:     paymentId,
						PaymentPlanId: paymentPlanId,
						WalletId:      walletId,
						CreateTime:    bringyour.NowUtc(),
					}
					walletPayments[walletId] = payment
				}
				payment.PayoutByteCount += payoutByteCount
				payment.Payout += payoutNetRevenue

				if payment.MinSweepTime.IsZero() {
					payment.MinSweepTime = sweepTime
				} else {
					payment.MinSweepTime = bringyour.MinTime(payment.MinSweepTime, sweepTime)
				}

				escrowId := EscrowId{
					ContractId: contractId,
					BalanceId:  balanceId,
				}
				escrowPaymentIds[escrowId] = payment.PaymentId
			}
		})



		subsidyPayment, err := planSubsidyPaymentInTx(ctx, tx, walletPayments)
		if err != nil {
			// in this case, the subsidy is not possible in the current time range
			// most likely because it completely overlaps with an existing subsidy
			// (and we cannot double pay the subsidy)
			// there are two options
			//   1. drop the subsidy, or
			//   2. wait for future payouts and try again, which will expand the subsidy time range
			// we currently choose 2

			returnErr = err
			return
		}




		// apply wallet minimum payout threshold
		// any wallet that does not meet the threshold will not be included in this plan
		walletIdsToRemove := []bringyour.Id{}
		payoutExpirationTime := bringyour.NowUtc().Add(-WalletPayoutTimeout)
		for walletId, payment := range walletPayments {
			// cannot remove payments that have `MinSweepTime <= payoutExpirationTime`
			if payment.Payout < MinWalletPayoutThreshold && payoutExpirationTime.Before(payment.MinSweepTime) {
				walletIdsToRemove = append(walletIdsToRemove, walletId)
			}
		}
		for _, walletId := range walletIdsToRemove {
			delete(walletPayments, walletId)
		}

		bringyour.BatchInTx(ctx, tx, func(batch bringyour.PgBatch) {
			for _, payment := range walletPayments {
				batch.Queue(
					`
                        INSERT INTO account_payment (
                            payment_id,
                            payment_plan_id,
                            wallet_id,
                            payout_byte_count,
                            payout_nano_cents,
                            min_sweep_time,
                            create_time
                        )
                        VALUES ($1, $2, $3, $4, $5, $6, $7)
                    `,
					payment.PaymentId,
					payment.PaymentPlanId,
					payment.WalletId,
					payment.PayoutByteCount,
					payment.Payout,
					payment.MinSweepTime,
					payment.CreateTime,
				)
			}
		})

		bringyour.CreateTempJoinTableInTx(
			ctx,
			tx,
			"payment_escrow_ids(contract_id uuid, balance_id uuid -> payment_id uuid)",
			escrowPaymentIds,
		)

		bringyour.RaisePgResult(tx.Exec(
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
			PaymentPlanId:     paymentPlanId,
			WalletPayments:    walletPayments,
			SubsidyPayment: subsidyPayment,
			WithheldWalletIds: walletIdsToRemove,
		}
	}, bringyour.TxReadCommitted)

	return paymentPlan
}


// this assumes the table `temp_account_payment` exists in the transaction
func planSubsidyPaymentInTx(ctx context.Context, tx bringyour.PgTx, walletPayments map[bringyour.Id]*AccountPayment{}) (*SubsidyPayment, error) {
	// roll up all the sweeps per payee network, payout network
	type networkSweep struct {
		payeeNetworkId Id
		payoutNetworkId Id
		minSweepTime time.Time
		netPayoutByteCountPaid ByteCount
		netPayoutByteCountUnpaid ByteCount
		payoutWalletId Id	
	}

	subsidyConfig := envSubsidyConfig()

	payeePayoutNetworkSweeps := map[Id]map[Id]*networkSweep{}
	payeeSubscriptionNetRevenues := map[Id]NanoCents{}

	result, err := tx.Query(
		ctx,
		`
	        SELECT
	        	t.payee_network_id,
	        	t.payout_network_id,
	        	t.min_sweep_time,
	        	t.net_payout_byte_count_paid,
	        	t.net_payout_byte_count_unpaid,
	        	account_wallet.wallet_id AS payout_wallet_id

	        FROM (
	        	SELECT
	            	transfer_balance.network_id AS payee_network_id,
	                transfer_escrow_sweep.network_id AS payout_network_id,
	                MIN(transfer_escrow_sweep.sweep_time) AS min_sweep_time,
	                SUM(CASE WHEN transfer_balance.paid THEN transfer_escrow_sweep.payout_byte_count ELSE 0) AS net_payout_byte_count_paid,
	                SUM(CASE WHEN NOT transfer_balance.paid THEN transfer_escrow_sweep.payout_byte_count ELSE 0) AS net_payout_byte_count_unpaid

	            FROM transfer_escrow_sweep

	            INNER JOIN temp_account_payment ON
	                temp_account_payment.contract_id = transfer_escrow_sweep.contract_id AND
	                temp_account_payment.balance_id = transfer_escrow_sweep.balance_id

	            INNER JOIN transfer_balance ON
	                transfer_balance.balance_id = transfer_escrow_sweep.balance_id

	            GROUP BY transfer_balance.network_id, transfer_escrow_sweep.network_id
	        ) t

	        INNER JOIN payout_wallet ON
	            payout_wallet.network_id = t.payout_network_id

	        INNER JOIN account_wallet ON
	            account_wallet.wallet_id = payout_wallet.wallet_id AND
	            account_wallet.active = true

	        FOR UPDATE
        `,
	)
	bringyour.WithPgResult(result, err, func() {
		for result.Next() {
			networkSweep := &networkSweep{}
			bringyour.Raise(result.Scan(
				&networkSweep.payeeNetworkId,
				&networkSweep.payoutNetworkId,
				&networkSweep.minSweepTime,
				&networkSweep.netPayoutByteCountPaid,
				&networkSweep.netPayoutByteCountUnpaid,
				&networkSweep.payoutWalletId,
			))
			payoutNetworkSweeps, ok := payeePayoutNetworkSweeps[networkSweep.payeeNetworkId]
			if !ok {
				payoutNetworkSweeps = map[Id]*networkSweep{}
				payeePayoutNetworkSweeps[networkSweep.payeeNetworkId] = payoutNetworkSweeps
			}
			payoutNetworkSweeps[networkSweep.payoutNetworkId] = networkSweep
		}
	})

	result, err := tx.Query(
		ctx,
		`
			SELECT
	    		transfer_balance.network_id AS payee_network_id,
	    		SUM(transfer_balance.subscription_net_revenue_nano_cents) AS subscription_net_revenue_nano_cents
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
	bringyour.WithPgResult(result, err, func() {
		for result.Next() {
			var payeeNetworkId Id
			var subscriptionNetRevenue NanoCents
			bringyour.Raise(result.Scan(&payeeNetworkId, &subscriptionNetRevenue))
			payeeSubscriptionNetRevenues[payeeNetworkId] = subscriptionNetRevenue
		}
	})

	netPayoutByteCountPaid := ByteCount(0)
	netPayoutByteCountUnpaid := ByteCount(0)
	activeUserCount := 0
	paidUserCount := 0
	netRevenue := NanoCents(0)
	for payeeNetworkId, payoutNetworkSweeps := range payeePayoutNetworkSweeps {
		payeeNetPayoutByteCountPaid := ByteCount(0)
		payeeNetPayoutByteCountUnpaid := ByteCount(0)
		for payoutNetworkId, networkSweep := range payoutNetworkSweeps {
			netPaidByteCount += networkSweep.paidByteCount
			netUnpaidByteCount += networkSweep.unpaidByteCount
			payeeNetPaidByteCount += networkSweep.paidByteCount
			payeeNetUnpaidByteCount += networkSweep.unpaidByteCount
		}

		if subsidyConfig.ActiveUserByteCountThreshold <= networkSweep.paidByteCount + networkSweep.unpaidByteCount {
			activeUserCount += 1
		}
		if 0 < payeeNetPayoutByteCountPaid {
			paidUserCount += 1
			netRevenue += payeeSubscriptionNetRevenues[payeeNetworkId]
		}
	}


	var subsidyStartTime time.Time
	var subsidyEndTime time.Time
	result, err := tx.Query(
		ctx,
		`
    	SELECT
            MIN(transfer_contract.create_time) AS subsidy_start_time,
            MIN(transfer_contract.close_time) AS subsidy_end_time,

        FROM transfer_escrow_sweep

        INNER JOIN transfer_contract ON
        	transfer_contract.contract_id = transfer_escrow_sweep.contract_id
    
        `,
	)
	bringyour.WithPgResult(result, err, func() {
		if result.Next() {
			bringyour.Raise(result.Scan(&subsidyStartTime, &subsidyEndTime))
		} else {
			subsidyEndTime = bringyour.NowUtc()
			subsidyStartTime = subsidyStartTime.Add(-subsidyConfig.Days * 24 * time.Hour)
		}
	})


	// if the end time is contained in a subsidy, end
	// move the start time forward to the max end time that contains the start time,
	// and the end time backward to the min start time that contains the end time
	result, err := tx.Query(
		ctx,
		`
		SELECT
			start_time,
			end_time,
		FROM payment_subsidy
		WHERE start_time < $1 AND $2 < end_time
		`,
		subsidyEndTime,
		subsidyStartTime,
	)
	bringyour.WithPgResult(result, err, func() {
		for result.Next() {
			var existingSubsidyStartTime time.Time
			var existingSubsidyEndTime time.Time
			bringyour.Raise(result.Scan(&existingSubsidyStartTime, &existingSubsidyEndTime))

			if existingSubsidyStartTime <= subsidyStartTime && subsidyStartTime < existingSubsidyEndTime {
				subsidyStartTime = existingSubsidyEndTime
			}
			if existingSubsidyStartTime <= subsidyEndTime && subsidyEndTime < existingSubsidyEndTime {
				subsidyEndTime = existingSubsidyStartTime
			}
		}
	})

	if !subsidyStartTime.Before(subsidyEndTime) {
		// the subsidy is contained in existing subsidies
		return fmt.Errorf("Planned subsidy overlaps with an existing subsdidy. Cannot double pay subsidies.")
	}


	subsidyPayoutUsd := max(
		subsidyConfig.MinPayoutUsd,
		max(
			subsidyConfig.UsdPerActionUser * activeUserCount,
			subsidyConfig.SubscriptionNetRevenueFraction * netRevenue,
		),
	)
	// the fraction of a `days` for this subsidy payout
	subsidyScale := float64(subsidyEndTime.Sub(subsidyStartTime) / time.Minute) / float64(subsidyConfig.Days * 24 * time.Hour / time.Minute)
	netPayout := UsdToNanoCents(subsidyScale * subsidyPayoutUsd)

	if subsidyScale <= subsidyConfig.MinDaysFraction {
		// no subsidy
		return nil, nil
	}

	for payeeNetworkId, payoutNetworkSweeps := range payeePayoutNetworkSweeps {
		netPayeePayoutByteCountPaid := ByteCount(0)
		netPayeePayoutByteCountUnpaid := ByteCount(0)
		for payoutNetworkId, networkSweep := range payoutNetworkSweeps {
			netPayeePayoutByteCountPaid += networkSweep.netPayoutByteCountPaid
			netPayeePayoutByteCountUnpaid += networkSweep.netPayoutByteCountUnpaid
		}

		if netPayeePayoutByteCountPaid == 0 {
			continue
		}

		for payoutNetworkId, networkSweep := range payoutNetworkSweeps {
			if networkSweep.netPayoutByteCountPaid == 0 {
				continue
			}

			// each paid user is weighted equally
			weight := float64(networkSweep.netPayoutByteCountPaid) / (float64(netPayeePayoutByteCountPaid) * float64(paidUserCount))
			payout := NanoCents(weight * float64(netPayout))

			payment, ok := walletPayments[networkSweep.payoutWalletId]
			if !ok {
				paymentId := bringyour.NewId()
				payment = &AccountPayment{
					PaymentId:     paymentId,
					PaymentPlanId: paymentPlanId,
					WalletId:      networkSweep.payoutWalletId,
					CreateTime:    bringyour.NowUtc(),
				}
				walletPayments[networkSweep.payoutWalletId] = payment
			}
			payment.Payout += payout

			if payment.MinSweepTime.IsZero() {
				payment.MinSweepTime = networkSweep.minSweepTime
			} else {
				payment.MinSweepTime = bringyour.MinTime(payment.MinSweepTime, networkSweep.minSweepTime)
			}
		}
	}


	bringyour.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO payment_subsidy (
				payment_plan_id,
		        start_time,
		        end_time,
		        active_user_count,
		        paid_user_count,
		        net_paid_byte_count,
		        net_unpaid_byte_count,
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
		"[plan][%s]subsidy %.2fdays %dusers/%dusers %dbytes/%dbytes. $%.2f revenue -> $%.2f paid\n",
		paymentPlanId,
		float64(subsidyEndTime.Sub(subsidyStartTime)) / float64(time.HOUR * 24),
		paidUserCount,
		activeUserCount,
		netPayoutByteCountPaid,
		netPayoutByteCountUnpaid,
		NanoCentsToUsd(netRevenue),
		NanoCentsToUsd(netPayout),
	)

	return nil
}




// set the record before submitting to the processor
// the controller should check if the payment already has a record before processing -
//
//	these are in a bad state and need to be investigated manually
func SetPaymentRecord(
	ctx context.Context,
	paymentId bringyour.Id,
	tokenType string,
	tokenAmount float64,
	paymentRecord string,
) (returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		tag := bringyour.RaisePgResult(tx.Exec(
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
			bringyour.NowUtc(),
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Invalid payment.")
			return
		}
	})
	return
}

func CompletePayment(ctx context.Context, paymentId bringyour.Id, paymentReceipt string) (returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		tag := bringyour.RaisePgResult(tx.Exec(
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
			bringyour.NowUtc(),
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Invalid payment.")
			return
		}

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE account_balance
                SET
                    paid_byte_count = paid_byte_count + account_payment.payout_byte_count,
                    paid_net_revenue_nano_cents = paid_net_revenue_nano_cents + account_payment.payout_nano_cents
                FROM account_payment, account_wallet
                WHERE
                    account_payment.payment_id = $1 AND
                    account_wallet.wallet_id = account_payment.wallet_id AND
                    account_balance.network_id = account_wallet.network_id
            `,
			paymentId,
		))
	})
	return
}

func CancelPayment(ctx context.Context, paymentId bringyour.Id) (returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		tag := bringyour.RaisePgResult(tx.Exec(
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
			bringyour.NowUtc(),
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
	paymentPlanId bringyour.Id,
	bonusNanoCents NanoCents,
) (returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		tag := bringyour.RaisePgResult(tx.Exec(
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

	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {

		result, err := tx.Query(
			session.Ctx,
			`
            SELECT
								account_payment.payment_id,
                account_payment.payment_plan_id,
                account_payment.wallet_id,
                account_payment.payout_byte_count,
                account_payment.payout_nano_cents,
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
                account_wallet.network_id
            FROM account_payment

            INNER JOIN account_wallet ON
                account_wallet.wallet_id = account_payment.wallet_id

            WHERE
                network_id = $1
        `,
			session.ByJwt.NetworkId,
		)

		bringyour.WithPgResult(result, err, func() {

			for result.Next() {
				payment := &AccountPayment{}

				bringyour.Raise(result.Scan(
					&payment.PaymentId,
					&payment.PaymentPlanId,
					&payment.WalletId,
					&payment.PayoutByteCount,
					&payment.Payout,
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
					&payment.NetworkId,
				))

				networkPayments = append(networkPayments, payment)

			}
		})
	})

	return networkPayments, nil

}

type TransferStats struct {
	PaidBytesProvided   int `json:"paid_bytes_provided"`
	UnpaidBytesProvided int `json:"unpaid_bytes_provided"`
}

/**
 * Total paid and unpaid bytes for a network
 * This is not live data, and depends on transfer_escrow_sweep
 */
func GetTransferStats(
	ctx context.Context,
	networkId bringyour.Id,
) *TransferStats {

	var transferStats *TransferStats

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					coalesce(SUM(CASE 
							WHEN account_payment.completed = true THEN transfer_escrow_sweep.payout_byte_count 
							ELSE 0 
						END), 0) as paid_bytes_provided,
					coalesce(SUM(CASE 
							WHEN account_payment.completed IS NULL OR account_payment.completed != true THEN transfer_escrow_sweep.payout_byte_count
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

		bringyour.WithPgResult(result, err, func() {

			if result.Next() {

				transferStats = &TransferStats{}

				bringyour.Raise(
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
