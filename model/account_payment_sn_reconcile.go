// The one-time sn-reconcile census finds legacy USDC rows that neither the
// planner nor the sender pays on its own. It only reads; the revive writes one
// exact row per transaction and the existing sender then pays that row.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

const (
	// RC-1: canceled before the 2026-08-17 fix while carrying a processor marker.
	SnReconcileCanceledWithEvidence = "canceled_with_evidence"
	// RC-2: a key reserved before request journaling, refused by the sender.
	// Reported but never revived (RC-24).
	SnReconcileUnretainedKey = "unretained_key"
)

// Receipts need not be valid JSON. Each pattern captures one quoted string value.
const snReconcileReceiptWalletIdPattern = `"walletId"\s*:\s*"([^"]+)"`
const snReconcileReceiptIdPattern = `"id"\s*:\s*"([^"]+)"`

// A4: addresses compare without surrounding ASCII whitespace, case-insensitively.
// The SQL form strips exactly the same characters.
const snReconcileAddressSpace = " \t\n\r\f\v"
const snReconcileAddressSql = `lower(btrim(%s,E' \t\n\r\f\v'))`

func SnReconcileAddress(address string) string {
	return strings.ToLower(strings.Trim(address, snReconcileAddressSpace))
}

// A8: until the fix of 2026-08-17 the planner moved every canceled row's
// sweeps into the network's next plan.
var SnReconcileReplanFixTime = time.Date(2026, 8, 17, 23, 59, 59, 0, time.UTC)

// A2: a sweep of row `%[1]s` is early when its contract closed at or before
// this bound: the start of the row's own plan epoch, raised to the latest
// earlier row of the network whose plan recorded no epoch. A plan without an
// epoch explains none of its sweeps. An early sweep may have been paid by an
// earlier row.
const snReconcileEarlyBoundSql = `CASE WHEN NOT EXISTS (SELECT 1 FROM subsidy_payment epoch WHERE epoch.payment_plan_id=%[1]s.payment_plan_id) THEN 'infinity'::timestamp
	ELSE GREATEST((SELECT epoch.start_time FROM subsidy_payment epoch WHERE epoch.payment_plan_id=%[1]s.payment_plan_id),
		(SELECT MAX(earlier.create_time) FROM account_payment earlier WHERE earlier.network_id=%[1]s.network_id AND earlier.create_time<%[1]s.create_time
		AND NOT EXISTS (SELECT 1 FROM subsidy_payment earlier_epoch WHERE earlier_epoch.payment_plan_id=earlier.payment_plan_id))) END`

// The sweeps (alias s, contract alias c) of row `%[1]s` that are early against
// the row's bound `%[2]s`. An unknown close time counts as early.
const snReconcileEarlySweepsSql = `transfer_escrow_sweep s LEFT JOIN transfer_contract c ON c.contract_id=s.contract_id
	WHERE s.payment_id=%[1]s.payment_id AND COALESCE(c.close_time<=%[2]s,true)`

// One stuck row as the census read it. The revive compares every column it
// read or clears against the locked row before writing anything.
type SnReconcileRow struct {
	PaymentId                 server.Id
	PaymentPlanId             server.Id
	NetworkId                 server.Id
	WalletId                  *server.Id
	PayoutByteCount           ByteCount
	Payout                    NanoCents
	SubsidyPayout             NanoCents
	ReliabilitySubsidy        NanoCents
	BonusPayout               NanoCents
	AttributionReviewRequired bool
	CreateTime                time.Time
	Canceled                  bool
	CancelTime                *time.Time
	CircleIdempotencyKey      *server.Id
	PaymentRecord             *string
	TxHash                    *string
	TokenType                 *string
	TokenAmount               *float64
	PaymentTime               *time.Time
	PaymentReceipt            *string

	Kind string
	// The address of `WalletId` in any network or state.
	PinnedWalletAddress *string
	// The network's `payout_wallet` row joined to its own `account_wallet`.
	PayoutWalletId         *server.Id
	PayoutWalletAddress    *string
	PayoutWalletBlockchain *string
	PayoutWalletActive     bool
	SweepCount             int64
	// A3: the earliest close time of the row's contracts.
	EarliestClose *time.Time
	// A2: an early sweep, or a bonus that originated in another row.
	EarlyEarnings bool
	// A5, A10: a row the network created after this row's cancellation holds
	// early sweeps that closed before this row was created. Read only for a
	// row without sweeps.
	ReplanEvidence bool
	// A8: the network created a row after this row's cancellation and before
	// the fix, so the planner moved this row's sweeps. Read only for a row
	// without sweeps.
	ReplannedBeforeFix bool
}

// Circle's observation of the row's own attempt, retained with a revival.
type SnReconcileEvidence struct {
	TransactionId string `json:"transaction_id,omitempty"`
	State         string `json:"state,omitempty"`
	TxHash        string `json:"tx_hash,omitempty"`
}

var snReconcileStuckSql = `SELECT p.payment_id,p.payment_plan_id,p.network_id,p.wallet_id,p.payout_byte_count,p.payout_nano_cents,
	p.subsidy_payout_nano_cents,p.reliability_subsidy_nano_cents,p.bonus_payout_nano_cents,p.attribution_review_required,
	p.create_time,p.canceled,p.cancel_time,p.circle_idempotency_key,p.payment_record,p.tx_hash,p.token_type,p.token_amount,
	p.payment_time,p.payment_receipt,pinned.wallet_address,payout.wallet_id,payout.wallet_address,payout.blockchain,
	COALESCE(payout.active,false),(SELECT COUNT(*) FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id),
	(SELECT MIN(c.close_time) FROM transfer_escrow_sweep s JOIN transfer_contract c ON c.contract_id=s.contract_id WHERE s.payment_id=p.payment_id),
	EXISTS (SELECT 1 FROM ` + fmt.Sprintf(snReconcileEarlySweepsSql, "p", "eb.bound") + `)
	OR EXISTS (SELECT 1 FROM account_payment_bonus b WHERE b.payment_id=p.payment_id AND b.origin_payment_id<>p.payment_id),
	CASE WHEN EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id) THEN false ELSE EXISTS (
		SELECT 1 FROM account_payment later CROSS JOIN LATERAL (SELECT ` + fmt.Sprintf(snReconcileEarlyBoundSql, "later") + ` AS bound) later_bound
		JOIN transfer_escrow_sweep s ON s.payment_id=later.payment_id JOIN transfer_contract c ON c.contract_id=s.contract_id
		WHERE later.network_id=p.network_id AND later.create_time>COALESCE(p.cancel_time,p.create_time) AND c.close_time<p.create_time AND c.close_time<=later_bound.bound) END,
	CASE WHEN EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id) THEN false ELSE EXISTS (
		SELECT 1 FROM account_payment later WHERE later.network_id=p.network_id
		AND later.create_time>COALESCE(p.cancel_time,p.create_time) AND later.create_time<=$1) END
	FROM account_payment p
	CROSS JOIN LATERAL (SELECT ` + fmt.Sprintf(snReconcileEarlyBoundSql, "p") + ` AS bound) eb
	LEFT JOIN account_wallet pinned ON pinned.wallet_id=p.wallet_id
	LEFT JOIN payout_wallet pw ON pw.network_id=p.network_id
	LEFT JOIN account_wallet payout ON payout.wallet_id=pw.wallet_id AND payout.network_id=p.network_id
	WHERE NOT p.completed AND NOT ` + legacyProviderPaymentReplacedSql("p.payment_id") + ` AND (
		(p.canceled AND (p.circle_idempotency_key IS NOT NULL OR p.payment_record IS NOT NULL OR p.tx_hash IS NOT NULL)) OR
		(NOT p.canceled AND p.circle_idempotency_key IS NOT NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL
		AND NOT EXISTS (SELECT 1 FROM audit_account_payment a WHERE a.event_id=p.circle_idempotency_key)))
	ORDER BY p.create_time,p.payment_id`

// RC-1 and RC-2 rows, oldest first. A request journaled under the key, valid or
// not, may have been POSTed, so that row is out of scope. So is an original
// that the legacy correction (account_payment_legacy_correction.go) replaced
// (RC-22): it stays canceled with its key, and its replacement carries the
// obligation. A stuck row without a network cannot be attributed to later
// rows; the census refuses it.
func GetSnReconcileStuckRows(ctx context.Context) (rows []*SnReconcileRow, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		rows, returnErr = []*SnReconcileRow{}, nil
		result, err := conn.Query(ctx, snReconcileStuckSql, SnReconcileReplanFixTime)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &SnReconcileRow{}
				var networkId *server.Id
				server.Raise(result.Scan(&row.PaymentId, &row.PaymentPlanId, &networkId, &row.WalletId, &row.PayoutByteCount, &row.Payout,
					&row.SubsidyPayout, &row.ReliabilitySubsidy, &row.BonusPayout, &row.AttributionReviewRequired,
					&row.CreateTime, &row.Canceled, &row.CancelTime, &row.CircleIdempotencyKey, &row.PaymentRecord, &row.TxHash, &row.TokenType, &row.TokenAmount,
					&row.PaymentTime, &row.PaymentReceipt, &row.PinnedWalletAddress, &row.PayoutWalletId, &row.PayoutWalletAddress, &row.PayoutWalletBlockchain,
					&row.PayoutWalletActive, &row.SweepCount, &row.EarliestClose, &row.EarlyEarnings, &row.ReplanEvidence, &row.ReplannedBeforeFix))
				if networkId == nil {
					returnErr = errors.Join(returnErr, fmt.Errorf("stuck payment %s has no network; reconcile it by hand", row.PaymentId))
					continue
				}
				row.NetworkId = *networkId
				row.Kind = SnReconcileUnretainedKey
				if row.Canceled {
					row.Kind = SnReconcileCanceledWithEvidence
				}
				rows = append(rows, row)
			}
		})
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return
}

// RC-5: every Circle wallet id in the receipts of rows paid at or after `from`.
func GetSnReconcileReceiptWalletIds(ctx context.Context, from time.Time) (walletIds []string) {
	server.Db(ctx, func(conn server.PgConn) {
		walletIds = []string{}
		result, err := conn.Query(ctx, `SELECT DISTINCT m[1] FROM account_payment p, regexp_matches(p.payment_receipt,$2,'g') m
			WHERE p.payment_time>=$1 ORDER BY 1`, from, snReconcileReceiptWalletIdPattern)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var walletId string
				server.Raise(result.Scan(&walletId))
				walletIds = append(walletIds, walletId)
			}
		})
	})
	return
}

// RC-6: every payment record stored in [start, end], which the listing must hold.
func GetSnReconcileRecordsPaidBetween(ctx context.Context, start time.Time, end time.Time) (records []string) {
	server.Db(ctx, func(conn server.PgConn) {
		records = []string{}
		result, err := conn.Query(ctx, `SELECT DISTINCT payment_record FROM account_payment
			WHERE payment_record IS NOT NULL AND payment_time>=$1 AND payment_time<=$2 ORDER BY 1`, start, end)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var record string
				server.Raise(result.Scan(&record))
				records = append(records, record)
			}
		})
	})
	return
}

// RC-7: the listed transaction ids that any payment record or receipt id names.
func GetSnReconcileReferencedIds(ctx context.Context, ids []string) (referenced map[string]bool) {
	server.Db(ctx, func(conn server.PgConn) {
		referenced = map[string]bool{}
		result, err := conn.Query(ctx, `SELECT DISTINCT r.id FROM (
			SELECT payment_record id FROM account_payment WHERE payment_record=ANY($1::text[])
			UNION ALL
			SELECT m[1] FROM account_payment p, regexp_matches(p.payment_receipt,$2,'g') m) r
			WHERE r.id=ANY($1::text[])`, ids, snReconcileReceiptIdPattern)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var id string
				server.Raise(result.Scan(&id))
				referenced[id] = true
			}
		})
	})
	return
}

// The wallet address of a row whose payment record names a listed transaction.
type SnReconcileRecordAddress struct {
	Record        string
	WalletAddress *string
}

// A4: for each listed transaction a payment record names, the wallet address of
// every row holding that record. A row without a wallet has no address.
func GetSnReconcileRecordAddresses(ctx context.Context, ids []string) (addresses []*SnReconcileRecordAddress) {
	server.Db(ctx, func(conn server.PgConn) {
		addresses = []*SnReconcileRecordAddress{}
		result, err := conn.Query(ctx, `SELECT p.payment_record,w.wallet_address FROM account_payment p
			LEFT JOIN account_wallet w ON w.wallet_id=p.wallet_id WHERE p.payment_record=ANY($1::text[]) ORDER BY 1,2`, ids)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				address := &SnReconcileRecordAddress{}
				server.Raise(result.Scan(&address.Record, &address.WalletAddress))
				addresses = append(addresses, address)
			}
		})
	})
	return
}

// RC-7: the networks whose wallets, active or not, registered each address.
// Addresses are given and keyed in their `SnReconcileAddress` form.
func GetSnReconcileAddressNetworks(ctx context.Context, addresses []string) (networks map[string][]server.Id) {
	address := fmt.Sprintf(snReconcileAddressSql, "wallet_address")
	server.Db(ctx, func(conn server.PgConn) {
		networks = map[string][]server.Id{}
		result, err := conn.Query(ctx, `SELECT `+address+`,network_id FROM account_wallet
			WHERE `+address+`=ANY($1::text[]) GROUP BY 1,2 ORDER BY 1,2`, addresses)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var address string
				var networkId server.Id
				server.Raise(result.Scan(&address, &networkId))
				networks[address] = append(networks[address], networkId)
			}
		})
	})
	return
}

// RC-8: the exact pre-cutoff census `recoverLegacyComponents` uses. A failed
// query is not evidence about the row and raises.
func SnReconcileCensusExact(ctx context.Context, paymentId server.Id, policy *server.ProviderPayoutTransition) (exact bool) {
	server.Db(ctx, func(conn server.PgConn) {
		exact = snReconcileCensusExactIn(ctx, conn, paymentId, policy)
	})
	return
}

func snReconcileCensusExactIn(ctx context.Context, query server.PgCanQuery, paymentId server.Id, policy *server.ProviderPayoutTransition) bool {
	_, _, err := inspectProviderUsdcPaymentAttribution(ctx, query, paymentId, policy, true)
	if errors.Is(err, ErrProviderUsdcAttributionUnresolved) {
		return false
	}
	server.Raise(err)
	return true
}

// A row that is neither completed nor stuck and holds early earnings, which an
// earlier payment may already have paid.
type SnReconcileHolder struct {
	PaymentId          server.Id `json:"payment_id"`
	PaymentPlanId      server.Id `json:"payment_plan_id"`
	NetworkId          server.Id `json:"network_id"`
	State              string    `json:"state"`
	Payout             NanoCents `json:"payout_nano_cents"`
	PayoutByteCount    ByteCount `json:"payout_byte_count"`
	SubsidyPayout      NanoCents `json:"subsidy_payout_nano_cents"`
	ReliabilitySubsidy NanoCents `json:"reliability_subsidy_nano_cents"`
	EarlyRevenue       NanoCents `json:"early_revenue_nano_cents"`
	EarlyByteCount     ByteCount `json:"early_byte_count"`
	EarlyBonus         NanoCents `json:"early_bonus_nano_cents"`
}

var snReconcileHoldersSql = `SELECT p.payment_id,p.payment_plan_id,p.network_id,
	CASE WHEN p.canceled THEN 'safely_canceled' WHEN p.tx_hash IS NOT NULL THEN 'on_chain' WHEN p.payment_record IS NOT NULL THEN 'submitted'
		WHEN p.circle_idempotency_key IS NOT NULL THEN 'reserved' ELSE 'planned' END,
	p.payout_nano_cents,p.payout_byte_count,p.subsidy_payout_nano_cents,p.reliability_subsidy_nano_cents,early.revenue,early.bytes,bonus.amount
	FROM account_payment p
	CROSS JOIN LATERAL (SELECT ` + fmt.Sprintf(snReconcileEarlyBoundSql, "p") + ` AS bound) eb
	CROSS JOIN LATERAL (SELECT COUNT(*) n,COALESCE(SUM(s.payout_net_revenue_nano_cents),0)::bigint revenue,COALESCE(SUM(s.payout_byte_count),0)::bigint bytes
		FROM ` + fmt.Sprintf(snReconcileEarlySweepsSql, "p", "eb.bound") + `) early
	CROSS JOIN LATERAL (SELECT COUNT(*) n,COALESCE(SUM(b.amount_nano_cents),0)::bigint amount FROM account_payment_bonus b
		WHERE b.payment_id=p.payment_id AND b.origin_payment_id<>p.payment_id) bonus
	WHERE NOT p.completed AND (
		(p.canceled AND p.circle_idempotency_key IS NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL) OR
		(NOT p.canceled AND NOT (p.circle_idempotency_key IS NOT NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL
		AND NOT EXISTS (SELECT 1 FROM audit_account_payment a WHERE a.event_id=p.circle_idempotency_key))))
	AND 0<early.n+bonus.n
	ORDER BY p.network_id,p.create_time,p.payment_id`

// A2 (§6): every pending or safely canceled row with early earnings, by
// network. Read only.
func GetSnReconcileEarlyHolders(ctx context.Context) (holders []*SnReconcileHolder) {
	server.Db(ctx, func(conn server.PgConn) {
		holders = []*SnReconcileHolder{}
		result, err := conn.Query(ctx, snReconcileHoldersSql)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				holder := &SnReconcileHolder{}
				server.Raise(result.Scan(&holder.PaymentId, &holder.PaymentPlanId, &holder.NetworkId, &holder.State, &holder.Payout, &holder.PayoutByteCount,
					&holder.SubsidyPayout, &holder.ReliabilitySubsidy, &holder.EarlyRevenue, &holder.EarlyByteCount, &holder.EarlyBonus))
				holders = append(holders, holder)
			}
		})
	})
	return
}

// A row an earlier run revived that is not yet completed, with the network's
// current payout wallet.
type SnReconcileRevived struct {
	PaymentId              server.Id
	Payout                 NanoCents
	PayoutWalletBlockchain *string
	PayoutWalletActive     bool
}

// A7, A11: revived rows not yet submitted, which still need funds from the
// payout wallets.
func GetSnReconcileRevivedPending(ctx context.Context) (revived []*SnReconcileRevived) {
	server.Db(ctx, func(conn server.PgConn) {
		revived = []*SnReconcileRevived{}
		result, err := conn.Query(ctx, `SELECT p.payment_id,p.payout_nano_cents,payout.blockchain,COALESCE(payout.active,false)
			FROM account_payment p
			LEFT JOIN payout_wallet pw ON pw.network_id=p.network_id
			LEFT JOIN account_wallet payout ON payout.wallet_id=pw.wallet_id AND payout.network_id=p.network_id
			WHERE NOT p.completed AND NOT p.canceled AND p.payment_record IS NULL AND p.tx_hash IS NULL
			AND EXISTS (SELECT 1 FROM audit_account_payment a WHERE a.payment_id=p.payment_id AND a.event_type='sn_reconcile_revived')
			ORDER BY p.payment_id`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &SnReconcileRevived{}
				server.Raise(result.Scan(&row.PaymentId, &row.Payout, &row.PayoutWalletBlockchain, &row.PayoutWalletActive))
				revived = append(revived, row)
			}
		})
	})
	return
}

// The census snapshot as a predicate on the row: every column the census read
// and every column the revive clears.
const snReconcileSnapshotSql = `payment_id=$1 AND NOT completed AND canceled=$2 AND cancel_time IS NOT DISTINCT FROM $3::timestamp
	AND circle_idempotency_key IS NOT DISTINCT FROM $4::uuid AND payment_record IS NOT DISTINCT FROM $5::text
	AND tx_hash IS NOT DISTINCT FROM $6::text AND wallet_id IS NOT DISTINCT FROM $7::uuid AND network_id=$8
	AND payout_byte_count=$9 AND payout_nano_cents=$10 AND subsidy_payout_nano_cents=$11 AND reliability_subsidy_nano_cents=$12
	AND bonus_payout_nano_cents=$13 AND attribution_review_required=$14 AND payment_plan_id=$15 AND create_time=$16
	AND token_type IS NOT DISTINCT FROM $17::varchar AND token_amount IS NOT DISTINCT FROM $18::double precision
	AND payment_time IS NOT DISTINCT FROM $19::timestamp AND payment_receipt IS NOT DISTINCT FROM $20::text`

func (self *SnReconcileRow) snapshot() []any {
	return []any{self.PaymentId, self.Canceled, self.CancelTime, self.CircleIdempotencyKey, self.PaymentRecord,
		self.TxHash, self.WalletId, self.NetworkId, self.PayoutByteCount, self.Payout, self.SubsidyPayout, self.ReliabilitySubsidy,
		self.BonusPayout, self.AttributionReviewRequired, self.PaymentPlanId, self.CreateTime,
		self.TokenType, self.TokenAmount, self.PaymentTime, self.PaymentReceipt}
}

// The audit record of a revival keeps every marker and every cleared value.
type snReconcileRevival struct {
	RunId                server.Id            `json:"run_id"`
	PolicySha256         string               `json:"policy_sha256"`
	Evidence             *SnReconcileEvidence `json:"circle_evidence"`
	Canceled             bool                 `json:"canceled"`
	CancelTime           *time.Time           `json:"cancel_time"`
	CircleIdempotencyKey *server.Id           `json:"circle_idempotency_key"`
	PaymentRecord        *string              `json:"payment_record"`
	TxHash               *string              `json:"tx_hash"`
	TokenType            *string              `json:"token_type"`
	TokenAmount          *float64             `json:"token_amount"`
	PaymentTime          *time.Time           `json:"payment_time"`
	PaymentReceipt       *string              `json:"payment_receipt"`
}

// RC-14: revive one payable row in its own transaction, then run `schedule`
// in that transaction. A row that differs from its snapshot, whose key now has
// a journal row (A1), or whose census is no longer exact, is left untouched
// (RC-15). One UPDATE clears the markers and the cancellation together, so the
// row never reads as safely canceled; amounts, plan, points, sweeps and
// `cancel_time` are unchanged. The transaction is READ COMMITTED, so every
// statement after FOR UPDATE reads what committed while it waited for the row
// lock: a request the lock holder journaled under the key is seen (A1). Under
// REPEATABLE READ the snapshot would date from before that wait.
func ReviveSnReconcileRow(ctx context.Context, row *SnReconcileRow, policy *server.ProviderPayoutTransition, runId server.Id, evidence *SnReconcileEvidence, schedule func(server.PgTx)) (revived bool) {
	revival := &snReconcileRevival{RunId: runId, Evidence: evidence, Canceled: row.Canceled, CancelTime: row.CancelTime,
		CircleIdempotencyKey: row.CircleIdempotencyKey, PaymentRecord: row.PaymentRecord, TxHash: row.TxHash, TokenType: row.TokenType,
		TokenAmount: row.TokenAmount, PaymentTime: row.PaymentTime, PaymentReceipt: row.PaymentReceipt}
	if policy != nil {
		revival.PolicySha256 = policy.ConfigSha256
	}
	details, err := json.Marshal(revival)
	server.Raise(err)
	server.Tx(ctx, func(tx server.PgTx) {
		revived = false
		result, err := tx.Query(ctx, `SELECT payment_id FROM account_payment WHERE `+snReconcileSnapshotSql+` FOR UPDATE`, row.snapshot()...)
		locked := false
		server.WithPgResult(result, err, func() {
			locked = result.Next()
		})
		if !locked {
			return
		}
		// A1: a request journaled under the key since the census may have been POSTed.
		journaled := false
		if row.CircleIdempotencyKey != nil {
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_account_payment WHERE event_id=$1)`, *row.CircleIdempotencyKey).Scan(&journaled))
		}
		if journaled || !snReconcileCensusExactIn(ctx, tx, row.PaymentId, policy) {
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details)
			VALUES($1,$2,'sn_reconcile_revived',$3)`, server.NewId(), row.PaymentId, string(details)))
		tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET canceled=false,circle_idempotency_key=NULL,payment_record=NULL,
			token_type=NULL,token_amount=NULL,payment_time=NULL,payment_receipt=NULL WHERE `+snReconcileSnapshotSql, row.snapshot()...))
		if tag.RowsAffected() != 1 {
			panic(fmt.Errorf("sn-reconcile revive of %s did not update its locked row", row.PaymentId))
		}
		schedule(tx)
		revived = true
	}, server.TxReadCommitted)
	return
}
