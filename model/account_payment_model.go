package model

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type SubsidyConfig struct {
	Days                                      float64 `yaml:"days"`
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
	SeekerHolderMultiplier                    float64 `yaml:"seeker_holder_multiplier"`
}

func (self *SubsidyConfig) ActiveUserByteCountThreshold() ByteCount {
	byteCount, err := ParseByteCount(self.ActiveUserByteCountThresholdHumanReadable)
	if err != nil {
		panic(err)
	}
	return byteCount
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
	SubsidyPayout             NanoCents `json:"subsidy_payout_nano_cents"`
	ReliabilitySubsidy        NanoCents `json:"reliability_subsidy_nano_cents"`
	BonusPayout               NanoCents `json:"bonus_payout_nano_cents"`
	AttributionReviewRequired bool      `json:"attribution_review_required"`
	// a corrected replacement: the hidden legacy payment it replaces
	ReplacesPaymentId *server.Id `json:"replaces_payment_id,omitempty"`
	MinSweepTime      time.Time  `json:"min_sweep_time"`
	CreateTime        time.Time  `json:"create_time"`

	PaymentRecord        *string    `json:"payment_record"`
	CircleIdempotencyKey *server.Id `json:"-"`
	TokenType            *string    `json:"token_type"`
	TokenAmount          *float64   `json:"token_amount"`
	PaymentTime          *time.Time `json:"payment_time"`
	PaymentReceipt       *string    `json:"payment_receipt"`
	WalletAddress        *string    `json:"wallet_address"`
	Blockchain           *string    `json:"blockchain,omitempty"`
	TxHash               *string    `json:"tx_hash,omitempty"`

	Completed    bool       `json:"completed"`
	CompleteTime *time.Time `json:"complete_time"`

	Canceled   bool       `json:"canceled"`
	CancelTime *time.Time `json:"cancel_time"`
}

// Journaled by the legacy correction on a legacy original and on its
// corrected replacement. Each link event takes the payment id as its event id,
// so listings find it with a primary key probe.
const (
	legacyProviderPaymentReplacedEventType    = "legacy_payment_replaced"
	legacyProviderPaymentReplacementEventType = "legacy_payment_replacement"
)

// The payment of the expression is a legacy original that a corrected
// replacement superseded; provider-facing listings show only the replacement.
func legacyProviderPaymentReplacedSql(paymentIdExpression string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM audit_account_payment replaced
		WHERE replaced.event_id = %s AND replaced.payment_id = %s AND replaced.event_type = '%s')`,
		paymentIdExpression, paymentIdExpression, legacyProviderPaymentReplacedEventType)
}

// The legacy original that the payment of the expression replaces, or null.
func legacyProviderPaymentReplacesSql(paymentIdExpression string) string {
	return fmt.Sprintf(`(SELECT (replacement.event_details::jsonb->>'original_payment_id')::uuid FROM audit_account_payment replacement
		WHERE replacement.event_id = %s AND replacement.payment_id = %s AND replacement.event_type = '%s')`,
		paymentIdExpression, paymentIdExpression, legacyProviderPaymentReplacementEventType)
}

// GetNetworkPayments normally hides canceled payments because their sweeps are
// represented by a replacement payment. A canceled row that still carries a
// retry/on-chain marker is not safely replaceable, however, and remains visible
// so an affected account never loses the only record of a possible transfer.
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
                account_payment.bonus_payout_nano_cents,
                account_payment.reliability_subsidy_nano_cents,
                account_payment.attribution_review_required,
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
				account_wallet.blockchain,
				`+legacyProviderPaymentReplacesSql("account_payment.payment_id")+` AS replaces_payment_id
            FROM account_payment

            LEFT JOIN account_wallet ON
                account_wallet.wallet_id = account_payment.wallet_id

            WHERE
                account_payment.network_id = $1 AND
                -- a corrected legacy payment shows only as its replacement
                NOT `+legacyProviderPaymentReplacedSql("account_payment.payment_id")+` AND
                (
                    NOT account_payment.canceled OR
                    account_payment.payment_record IS NOT NULL OR
                    account_payment.circle_idempotency_key IS NOT NULL OR
                    account_payment.tx_hash IS NOT NULL OR
                    account_payment.subsidy_payout_nano_cents > 0 OR
                    account_payment.reliability_subsidy_nano_cents > 0 OR
                    account_payment.attribution_review_required
                )
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
					&payment.BonusPayout,
					&payment.ReliabilitySubsidy,
					&payment.AttributionReviewRequired,
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
					&payment.ReplacesPaymentId,
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

// GetTransferStats returns the durable paid/unpaid byte totals for a network.
// Unplanned and safely canceled value still comes from transfer_escrow_sweep.
// Once a payment is active, its immutable payout_byte_count is authoritative.
// This keeps the user's unpaid total stable if retention or repair removes some
// of the payment's sweep rows. Canceled rows with processor evidence are
// reconciliation anomalies rather than a second unpaid ledger: counting them
// after their sweeps were reassigned would double-count the replacement
// payment. The totals answer as they did while USDC payouts ran (owner
// decisions 2026-11-12 and 2026-11-13: USDC payouts ended), so apps that read
// them keep working.
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
				FROM transfer_escrow_sweep

				INNER JOIN account_payment ON
					account_payment.payment_id = transfer_escrow_sweep.payment_id

				WHERE
					account_payment.network_id = $1 AND
					NOT account_payment.completed AND
					account_payment.canceled AND
					account_payment.circle_idempotency_key IS NULL AND
					account_payment.payment_record IS NULL AND
					account_payment.tx_hash IS NULL

				UNION ALL

				SELECT
					0 AS paid_bytes_provided,
					COALESCE(SUM(p.payout_byte_count), 0) AS unpaid_bytes_provided
				FROM account_payment p
				WHERE
					p.network_id = $1 AND
					NOT p.completed AND
					NOT p.canceled

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
