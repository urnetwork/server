package model

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	// "encoding/json"

	// "golang.org/x/exp/maps"

	"github.com/urnetwork/glog"

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
	ReferralParentPayoutFraction              float64 `yaml:"referral_parent_payout_fraction"`
	ReferralChildPayoutFraction               float64 `yaml:"referral_child_payout_fraction"`
	AccountPointsPerPayout                    int     `yaml:"account_points_per_payout"`
	ReliabilityPointsPerPayout                int     `yaml:"reliability_points_per_payout"`
	ReliabilitySubsidyPerPayoutUsd            int     `yaml:"reliability_subsidy_per_payout_usd"`
	CountryReliabilityWeightTarget            float64 `yaml:"country_reliability_weight_target"`
	MaxCountryReliabilityMultiplier           float64 `yaml:"max_country_reliability_multiplier"`

	MinWalletPayoutUsd float64 `yaml:"min_wallet_payout_usd"`

	// hold onto unpaid amounts for up to this time
	// after this time, even if the value is below the threshold, the payment is created
	WalletPayoutTimeoutHumanReadable string `yaml:"wallet_payout_timeout"`

	ForcePoints bool

	SeekerHolderMultiplier float64 `yaml:"seeker_holder_multiplier"`
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
	SubsidyPayout      NanoCents `json:"subsidy_payout_nano_cents"`
	ReliabilitySubsidy NanoCents `json:"reliability_subsidy_nano_cents"`
	MinSweepTime       time.Time `json:"min_sweep_time"`
	CreateTime         time.Time `json:"create_time"`

	PaymentRecord  *string    `json:"payment_record"`
	TokenType      *string    `json:"token_type"`
	TokenAmount    *float64   `json:"token_amount"`
	PaymentTime    *time.Time `json:"payment_time"`
	PaymentReceipt *string    `json:"payment_receipt"`
	WalletAddress  *string    `json:"wallet_address"`
	Blockchain     *string    `json:"blockchain,omitempty"`
	TxHash         *string    `json:"tx_hash,omitempty"`

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
                account_payment.reliability_subsidy_nano_cents,
                account_payment.min_sweep_time,
                account_payment.create_time,
                account_payment.payment_record,
                account_payment.token_type,
                account_payment.token_amount,
                account_payment.tx_hash,
                account_payment.payment_time,
                account_payment.payment_receipt,
                account_payment.completed,
                account_payment.complete_time,
                account_payment.canceled,
                account_payment.cancel_time,
				account_wallet.wallet_address,
				account_wallet.blockchain
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
				&payment.ReliabilitySubsidy,
				&payment.MinSweepTime,
				&payment.CreateTime,
				&payment.PaymentRecord,
				&payment.TokenType,
				&payment.TokenAmount,
				&payment.TxHash,
				&payment.PaymentTime,
				&payment.PaymentReceipt,
				&payment.Completed,
				&payment.CompleteTime,
				&payment.Canceled,
				&payment.CancelTime,
				&payment.WalletAddress,
				&payment.Blockchain,
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
	// note the wallet cannot be updated once there is a payment record or
	// a submit attempt (idempotency key). A retried submit replays the original
	// transaction at the processor, so the wallet on record must stay pinned to
	// the wallet the funds were actually sent to.
	// note `account_wallet.network_id` must match the payment network,
	// so that a payout is never redirected to another network's wallet
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
			        account_wallet.network_id = account_payment.network_id AND
			        account_wallet.active = true

		        WHERE
		        	account_payment.payment_id = $1 AND
		        	account_payment.payment_record IS NULL AND
		        	account_payment.circle_idempotency_key IS NULL AND
		        	account_payment.completed = false AND
		        	account_payment.canceled = false

		    ) t
		    WHERE account_payment.payment_id = t.payment_id
			`,
			paymentId,
		))
	})
}

// returns the stable idempotency key for submitting this payment to the
// payment processor. The key is created on the first submit attempt and reused
// on retries, so a crash between the processor call and `SetPaymentRecord`
// cannot double-send funds. `RemovePaymentRecord` clears the key so that a
// failed transaction is retried with a fresh key (a reused key would replay
// the failed transaction at the processor instead of creating a new one).
func GetOrCreatePaymentIdempotencyKey(ctx context.Context, paymentId server.Id) (idempotencyKey server.Id, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
	            UPDATE account_payment
	            SET
	                circle_idempotency_key = COALESCE(circle_idempotency_key, $2)
	            WHERE
	                payment_id = $1 AND
	                NOT completed AND NOT canceled
	            RETURNING circle_idempotency_key
	        `,
			paymentId,
			server.NewId(),
		)
		set := false
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&idempotencyKey))
				set = true
			}
		})
		if !set {
			returnErr = fmt.Errorf("Invalid payment.")
		}
	})
	return
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
	SubsidyScale             float64
}

// plan, manually check out and add balance to funding account, then complete
// minimum net_revenue_nano_cents to include in a payout
// all of the returned payments are tagged with the same payment_plan_id
func PlanPayments(ctx context.Context) (paymentPlan *PaymentPlan, returnErr error) {
	subsidyConfig := EnvSubsidyConfig()
	return PlanPaymentsWithConfig(ctx, subsidyConfig)
}

func PlanPaymentsWithConfig(ctx context.Context, subsidyConfig *SubsidyConfig) (paymentPlan *PaymentPlan, returnErr error) {
	return CreatePaymentPlan(ctx, subsidyConfig, false, 0)
}

// PlanPaymentsWithMaxDuration is like PlanPayments but bounds the plan to the
// first maxDuration of contract close time following the most recent subsidy
// epoch, so a large backlog since the last payout can be drained in bounded
// slices instead of one oversized plan that runs out of memory. Each plan
// records a new subsidy epoch whose end advances the frontier, so calling this
// repeatedly walks forward through the backlog one slice at a time. maxDuration
// == 0 is unbounded (identical to PlanPayments).
func PlanPaymentsWithMaxDuration(ctx context.Context, maxDuration time.Duration) (paymentPlan *PaymentPlan, returnErr error) {
	return CreatePaymentPlan(ctx, EnvSubsidyConfig(), false, maxDuration)
}

// PlanPaymentsWithMaxDurationLoop drains the unpaid backlog by planning bounded
// maxDuration slices back to back until the payout frontier reaches now, so a
// single command call catches up instead of requiring one invocation per slice.
//
// Each slice is planned and committed independently, so progress is durable: a
// failure only loses the in-flight slice, and everything already committed stays
// paid (the next call resumes from the advanced frontier). The shared 30-day
// reliability inputs are refreshed once on the first slice rather than per slice.
//
// The loop stops when either:
//   - a slice advances the frontier to at/after now — the backlog is fully
//     drained; or
//   - a slice forms no new subsidy epoch (SubsidyPayment == nil) — nothing left
//     that can advance the frontier: the remaining window is below the minimum
//     payout duration, there are no unpaid sweeps, or the next sweeps sit beyond
//     a gap wider than maxDuration. (The upper bound is capped at now, so the
//     frontier never lands strictly past now; this no-advance condition is what
//     ends the drain as it reaches now.)
//
// maxDuration <= 0 plans a single unbounded plan, matching
// PlanPaymentsWithMaxDuration(ctx, 0).
//
// onSlice, if non-nil, is invoked after each committed slice (for progress
// output). The committed plans are returned in order even when an error is
// returned partway, so callers can report what did complete.
func PlanPaymentsWithMaxDurationLoop(
	ctx context.Context,
	maxDuration time.Duration,
	onSlice func(*PaymentPlan),
) ([]*PaymentPlan, error) {
	if maxDuration <= 0 {
		plan, err := CreatePaymentPlan(ctx, EnvSubsidyConfig(), false, 0)
		if err != nil {
			return nil, err
		}
		if onSlice != nil {
			onSlice(plan)
		}
		return []*PaymentPlan{plan}, nil
	}

	plans := []*PaymentPlan{}
	var lastFrontier time.Time
	for i := 0; ; i += 1 {
		// refresh the shared reliability inputs only on the first slice; the
		// window is the same for every slice in this drain.
		plan, err := createPaymentPlan(ctx, EnvSubsidyConfig(), false, maxDuration, i == 0)
		if err != nil {
			return plans, err
		}
		plans = append(plans, plan)
		if onSlice != nil {
			onSlice(plan)
		}

		// a slice that formed no subsidy epoch cannot advance the frontier, so
		// this drain has gone as far as it can (see the doc comment).
		if plan.SubsidyPayment == nil {
			break
		}
		frontier := plan.SubsidyPayment.EndTime

		// defensive: a recorded epoch always ends after the previous frontier, so
		// this should not trigger — but never replan the same window forever.
		if !lastFrontier.IsZero() && !frontier.After(lastFrontier) {
			glog.Infof("[plan]frontier did not advance past %s; stopping drain\n", frontier)
			break
		}
		lastFrontier = frontier

		// the frontier reached now: the backlog is fully drained.
		if !frontier.Before(server.NowUtc()) {
			break
		}
	}
	return plans, nil
}

// PlanPaymentsDryRun computes a payment plan exactly like PlanPayments but
// persists nothing: no payments are created, no escrow sweeps are marked paid,
// and no points or reliability multipliers are written. Use it to preview the
// plan (the wallets and amounts that would be paid) before committing a real
// plan. Because the reliability inputs are not refreshed for a dry run, the
// reliability portion of the preview reflects the last reliability refresh
// rather than a fresh one.
func PlanPaymentsDryRun(ctx context.Context) (paymentPlan *PaymentPlan, returnErr error) {
	return CreatePaymentPlan(ctx, EnvSubsidyConfig(), true, 0)
}

// PlanPaymentsDryRunWithMaxDuration is PlanPaymentsDryRun with the same
// maxDuration bounding as PlanPaymentsWithMaxDuration.
func PlanPaymentsDryRunWithMaxDuration(ctx context.Context, maxDuration time.Duration) (paymentPlan *PaymentPlan, returnErr error) {
	return CreatePaymentPlan(ctx, EnvSubsidyConfig(), true, maxDuration)
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
                    payment_record = NULL,
                    circle_idempotency_key = NULL
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

func CompletePayment(
	ctx context.Context,
	paymentId server.Id,
	paymentReceipt string,
	txHash string,
) (returnErr error) {

	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                UPDATE account_payment
                SET
                    payment_receipt = $2,
                    completed = true,
                    complete_time = $3,
                    tx_hash = $4
                WHERE
                    payment_id = $1 AND
                    NOT completed AND NOT canceled
            `,
			paymentId,
			paymentReceipt,
			server.NowUtc(),
			txHash,
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Invalid payment.")
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO account_balance (
                	network_id,
                	paid_byte_count,
                	paid_net_revenue_nano_cents
                )
                SELECT
                	account_payment.network_id,
                	account_payment.payout_byte_count AS paid_byte_count,
                	account_payment.payout_nano_cents AS paid_net_revenue_nano_cents
                FROM account_payment
                WHERE
                    account_payment.payment_id = $1
		        ON CONFLICT (network_id) DO UPDATE
                SET
                    paid_byte_count = account_balance.paid_byte_count + EXCLUDED.paid_byte_count,
                    paid_net_revenue_nano_cents = account_balance.paid_net_revenue_nano_cents + EXCLUDED.paid_net_revenue_nano_cents
            `,
			paymentId,
		))
	})
	return
}

// a planned payment that is neither completed nor canceled after this long is
// hung and will not complete on its own; see `CancelHungAccountPayments`
const HungPaymentExpiration = 30 * 24 * time.Hour

// CancelHungAccountPayments cancels payments stuck pending for longer than
// `HungPaymentExpiration`, which releases their sweeps back to the payout
// planner for a fresh payment (the planner re-selects sweeps whose payment is
// canceled). This is the first layer of straggler recovery; contracts whose
// payments keep hanging are hard deleted at `StragglerContractExpiration` as
// the final backstop. A canceled payment can no longer be completed
// (CompletePayment guards NOT canceled), so a payment whose external transfer
// was already initiated (payment_record set) is logged loudly for audit: if
// that transfer did land out of band, the re-planned sweeps would pay again.
func CancelHungAccountPayments(ctx context.Context, maxTime time.Time) (canceledCount int64) {
	minTime := maxTime.Add(-HungPaymentExpiration)

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
			UPDATE account_payment
			SET
				canceled = true,
				cancel_time = $2
			WHERE
				NOT completed AND
				NOT canceled AND
				create_time < $1
			RETURNING payment_id, payment_record IS NOT NULL
			`,
			minTime,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				var hasPaymentRecord bool
				server.Raise(result.Scan(&paymentId, &hasPaymentRecord))
				canceledCount += 1
				if hasPaymentRecord {
					glog.Infof("[pay]canceled hung payment %s WITH an initiated payment record; audit the external transfer for double payout\n", paymentId)
				} else {
					glog.Infof("[pay]canceled hung payment %s\n", paymentId)
				}
			}
		})
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
                account_payment.tx_hash,
                account_payment.token_type,
                account_payment.token_amount,
                account_payment.payment_time,
                account_payment.payment_receipt,
                account_payment.completed,
                account_payment.complete_time,
                account_payment.canceled,
                account_payment.cancel_time,
				account_wallet.wallet_address,
				account_wallet.blockchain
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
					&payment.TxHash,
					&payment.TokenType,
					&payment.TokenAmount,
					&payment.PaymentTime,
					&payment.PaymentReceipt,
					&payment.Completed,
					&payment.CompleteTime,
					&payment.Canceled,
					&payment.CancelTime,
					&payment.WalletAddress,
					&payment.Blockchain,
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

	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					0 AS paid_bytes_provided,
					COALESCE(SUM(transfer_escrow_sweep.payout_byte_count), 0) AS unpaid_bytes_provided
				FROM transfer_escrow_sweep

				WHERE
					transfer_escrow_sweep.network_id = $1 AND
					transfer_escrow_sweep.payment_id IS NULL

				UNION ALL

				SELECT
					0 AS paid_bytes_provided,
					COALESCE(SUM(transfer_escrow_sweep.payout_byte_count), 0) AS unpaid_bytes_provided
				FROM account_payment

				INNER JOIN transfer_escrow_sweep ON
                        transfer_escrow_sweep.payment_id = account_payment.payment_id

				WHERE
					account_payment.network_id = $1 AND
					account_payment.completed = false

				UNION ALL

				SELECT
					COALESCE(SUM(account_payment.payout_byte_count), 0) AS paid_bytes_provided,
					0 AS unpaid_bytes_provided
				FROM account_payment
				WHERE
					account_payment.network_id = $1 AND
					account_payment.completed = true
			`,
			networkId,
		)

		server.WithPgResult(result, err, func() {
			transferStats = &TransferStats{}

			for result.Next() {

				var paidBytesProvided ByteCount
				var unpaidBytesProvided ByteCount

				server.Raise(
					result.Scan(
						&paidBytesProvided,
						&unpaidBytesProvided,
					),
				)

				transferStats.PaidBytesProvided += paidBytesProvided
				transferStats.UnpaidBytesProvided += unpaidBytesProvided
			}
		})
	})

	return transferStats
}
