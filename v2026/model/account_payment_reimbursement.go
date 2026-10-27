// A manual reimbursement repays one network a listed USDC amount, to one pinned
// wallet of its own, through the ordinary Circle payout path (`bringyourctl
// payouts reimburse`). It has no sweeps. Its authority is its marker, the
// payment's audit_account_payment row keyed by the payment's own id and written
// in the transaction that creates the payment, so every check finds it by
// primary key:
//   - UpdatePaymentWallet never re-points a marked payment;
//   - CancelHungAccountPayments never cancels one;
//   - the USDC admission (RequireProviderUsdcPayment) admits a marked payment
//     by its marker instead of the earning census, and only while the payment
//     matches the marker exactly.
//
// A payment without a marker is unaffected.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

const ManualReimbursementEventType = "manual_reimbursement"

const manualReimbursementMarkerVersion = 1

// A Solana address that received payouts meant for other networks in May 2026.
// No manual reimbursement may pay it.
const ManualReimbursementRefusedAddress = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"

var ErrManualReimbursementChanged = errors.New("manual reimbursement no longer matches its marker; retained without submission")

var ErrManualReimbursementPlanChanged = errors.New("manual reimbursement plan already exists with different payments; nothing was created")

var ErrManualReimbursementWalletChanged = errors.New("manual reimbursement wallet changed since the census; nothing was created")

// Whether `address` is the refused address, compared without surrounding
// whitespace and case.
func ManualReimbursementAddressRefused(address string) bool {
	return strings.EqualFold(strings.TrimSpace(address), ManualReimbursementRefusedAddress)
}

// The account_payment row `%[1]s` is a manual reimbursement.
const manualReimbursementMarkedSql = `EXISTS (SELECT 1 FROM audit_account_payment manual_reimbursement
	WHERE manual_reimbursement.event_id = %[1]s.payment_id AND manual_reimbursement.payment_id = %[1]s.payment_id
	AND manual_reimbursement.event_type = 'manual_reimbursement')`

func manualReimbursementMarked(paymentAlias string) string {
	return fmt.Sprintf(manualReimbursementMarkedSql, paymentAlias)
}

// The marker of one reimbursement payment. Every field is fixed when the plan is
// created, so a rerun of the same plan writes the same bytes.
type ManualReimbursementMarker struct {
	Version       int       `json:"version"`
	Digest        string    `json:"digest"`
	Reason        string    `json:"reason"`
	PaymentPlanId server.Id `json:"payment_plan_id"`
	PaymentId     server.Id `json:"payment_id"`
	NetworkId     server.Id `json:"network_id"`
	WalletId      server.Id `json:"wallet_id"`
	WalletAddress string    `json:"wallet_address"`
	// account_wallet.blockchain as stored
	Blockchain string `json:"blockchain"`
	// the USDC the wallet receives
	AmountNanoCents NanoCents `json:"amount_nano_cents"`
	// the sender's fixed transfer fee, deducted from the payout
	FeeNanoCents NanoCents `json:"fee_nano_cents"`
	// AmountNanoCents + FeeNanoCents
	PayoutNanoCents NanoCents `json:"payout_nano_cents"`
}

func (self *ManualReimbursementMarker) details() string {
	details, err := json.Marshal(self)
	server.Raise(err)
	return string(details)
}

// One plan: one payment per network, all with the plan's id.
type ManualReimbursementPlan struct {
	PaymentPlanId server.Id
	Digest        string
	Reason        string
	Markers       []*ManualReimbursementMarker
}

func (self *ManualReimbursementPlan) check() error {
	if self == nil || self.PaymentPlanId == (server.Id{}) || self.Digest == "" ||
		strings.TrimSpace(self.Reason) == "" || len(self.Reason) > 1024 || len(self.Markers) == 0 {
		return errors.New("manual reimbursement plan requires an id, a digest, a bounded reason and at least one payment")
	}
	networkIds := map[server.Id]bool{}
	paymentIds := map[server.Id]bool{}
	for _, marker := range self.Markers {
		if marker.Version != manualReimbursementMarkerVersion || marker.Digest != self.Digest || marker.Reason != self.Reason ||
			marker.PaymentPlanId != self.PaymentPlanId || marker.PaymentId == (server.Id{}) || marker.NetworkId == (server.Id{}) ||
			marker.WalletId == (server.Id{}) || marker.WalletAddress == "" || marker.Blockchain == "" ||
			marker.AmountNanoCents <= 0 || marker.FeeNanoCents < 0 || marker.PayoutNanoCents != marker.AmountNanoCents+marker.FeeNanoCents {
			return fmt.Errorf("manual reimbursement payment %s is incomplete or inconsistent", marker.PaymentId)
		}
		if ManualReimbursementAddressRefused(marker.WalletAddress) {
			return fmt.Errorf("manual reimbursement payment %s pays the refused address", marker.PaymentId)
		}
		if networkIds[marker.NetworkId] || paymentIds[marker.PaymentId] {
			return fmt.Errorf("manual reimbursement plan repeats network %s or payment %s", marker.NetworkId, marker.PaymentId)
		}
		networkIds[marker.NetworkId] = true
		paymentIds[marker.PaymentId] = true
	}
	return nil
}

// An open manual reimbursement of another plan for one of the plan's networks.
type ManualReimbursementConflict struct {
	NetworkId     server.Id
	PaymentId     server.Id
	PaymentPlanId server.Id
	Completed     bool
}

// The open (not canceled) manual reimbursements of `networkIds` outside the plan
// `paymentPlanId`.
func manualReimbursementConflictsInQuery(ctx context.Context, query server.PgCanQuery, networkIds []server.Id, paymentPlanId server.Id) []*ManualReimbursementConflict {
	conflicts := []*ManualReimbursementConflict{}
	result, err := query.Query(
		ctx,
		`
			SELECT p.network_id, p.payment_id, p.payment_plan_id, p.completed
			FROM account_payment p
			WHERE
				p.network_id = ANY($1) AND
				NOT p.canceled AND
				p.payment_plan_id <> $2 AND
				`+manualReimbursementMarked("p")+`
			ORDER BY p.network_id, p.payment_id
		`,
		networkIds,
		paymentPlanId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			conflict := &ManualReimbursementConflict{}
			server.Raise(result.Scan(&conflict.NetworkId, &conflict.PaymentId, &conflict.PaymentPlanId, &conflict.Completed))
			conflicts = append(conflicts, conflict)
		}
	})
	return conflicts
}

// The listed wallets, and the names of the listed networks, by id. A missing id
// has no entry. Read only.
func GetManualReimbursementCensus(ctx context.Context, walletIds []server.Id, networkIds []server.Id) (wallets map[server.Id]*AccountWallet, networkNames map[server.Id]string) {
	wallets = map[server.Id]*AccountWallet{}
	networkNames = map[server.Id]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT wallet_id, network_id, blockchain, wallet_address, active
				FROM account_wallet
				WHERE wallet_id = ANY($1)
			`,
			walletIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				wallet := &AccountWallet{}
				server.Raise(result.Scan(&wallet.WalletId, &wallet.NetworkId, &wallet.Blockchain, &wallet.WalletAddress, &wallet.Active))
				wallets[wallet.WalletId] = wallet
			}
		})
		result, err = conn.Query(
			ctx,
			`
				SELECT network_id, network_name
				FROM network
				WHERE network_id = ANY($1)
			`,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var networkName string
				server.Raise(result.Scan(&networkId, &networkName))
				networkNames[networkId] = networkName
			}
		})
	})
	return
}

// Read only.
func GetManualReimbursementConflicts(ctx context.Context, networkIds []server.Id, paymentPlanId server.Id) (conflicts []*ManualReimbursementConflict) {
	server.Db(ctx, func(conn server.PgConn) {
		conflicts = manualReimbursementConflictsInQuery(ctx, conn, networkIds, paymentPlanId)
	})
	return
}

// The marker of each payment of the plan, keyed by payment id; nil for a
// payment of the plan without a marker. Read only.
func manualReimbursementPlanMarkersInQuery(ctx context.Context, query server.PgCanQuery, paymentPlanId server.Id) map[server.Id]*string {
	markers := map[server.Id]*string{}
	result, err := query.Query(
		ctx,
		`
			SELECT p.payment_id, manual_reimbursement.event_details
			FROM account_payment p
			LEFT JOIN audit_account_payment manual_reimbursement ON
				manual_reimbursement.event_id = p.payment_id AND
				manual_reimbursement.payment_id = p.payment_id AND
				manual_reimbursement.event_type = 'manual_reimbursement'
			WHERE p.payment_plan_id = $1
		`,
		paymentPlanId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var paymentId server.Id
			var details *string
			server.Raise(result.Scan(&paymentId, &details))
			markers[paymentId] = details
		}
	})
	return markers
}

// The number of payments the plan holds. Read only.
func GetManualReimbursementPlanPaymentCount(ctx context.Context, paymentPlanId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		count = len(manualReimbursementPlanMarkersInQuery(ctx, conn, paymentPlanId))
	})
	return
}

// Creates the plan's payments and their markers in one transaction, each payment
// with its wallet pinned. A rerun of the same plan creates nothing and returns
// created false; a plan id that holds other payments, an open reimbursement of a
// network under another plan, or a wallet that changed since the census refuses
// before anything is written.
func CreateManualReimbursementPlan(ctx context.Context, plan *ManualReimbursementPlan, createTime time.Time) (created bool, returnErr error) {
	if err := plan.check(); err != nil {
		return false, err
	}
	createTime = createTime.UTC()
	networkIds := []server.Id{}
	walletIds := []server.Id{}
	for _, marker := range plan.Markers {
		networkIds = append(networkIds, marker.NetworkId)
		walletIds = append(walletIds, marker.WalletId)
	}
	// every creator holds this key, so the checks below and the inserts are one step
	keys := []server.PgOwnershipKey{server.NewPgOwnershipKeyFromString("manual_reimbursement", "create")}
	server.OwnedTx(ctx, keys, func(tx server.PgTx) {
		created, returnErr = false, nil

		existing := manualReimbursementPlanMarkersInQuery(ctx, tx, plan.PaymentPlanId)
		if 0 < len(existing) {
			if len(existing) != len(plan.Markers) {
				returnErr = ErrManualReimbursementPlanChanged
				return
			}
			for _, marker := range plan.Markers {
				details, ok := existing[marker.PaymentId]
				if !ok || details == nil || *details != marker.details() {
					returnErr = ErrManualReimbursementPlanChanged
					return
				}
			}
			return
		}

		if conflicts := manualReimbursementConflictsInQuery(ctx, tx, networkIds, plan.PaymentPlanId); 0 < len(conflicts) {
			returnErr = fmt.Errorf("network %s already has manual reimbursement %s in plan %s; nothing was created", conflicts[0].NetworkId, conflicts[0].PaymentId, conflicts[0].PaymentPlanId)
			return
		}

		type wallet struct {
			networkId  server.Id
			address    string
			blockchain string
			active     bool
		}
		wallets := map[server.Id]*wallet{}
		result, err := tx.Query(
			ctx,
			`
				SELECT wallet_id, network_id, wallet_address, blockchain, active
				FROM account_wallet
				WHERE wallet_id = ANY($1)
				FOR SHARE
			`,
			walletIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var walletId server.Id
				w := &wallet{}
				server.Raise(result.Scan(&walletId, &w.networkId, &w.address, &w.blockchain, &w.active))
				wallets[walletId] = w
			}
		})
		for _, marker := range plan.Markers {
			w := wallets[marker.WalletId]
			if w == nil || !w.active || w.networkId != marker.NetworkId || w.address != marker.WalletAddress || w.blockchain != marker.Blockchain {
				returnErr = fmt.Errorf("%w: wallet %s of network %s", ErrManualReimbursementWalletChanged, marker.WalletId, marker.NetworkId)
				return
			}
		}

		for _, marker := range plan.Markers {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO account_payment (
						payment_id,
						payment_plan_id,
						network_id,
						wallet_id,
						payout_byte_count,
						payout_nano_cents,
						subsidy_payout_nano_cents,
						reliability_subsidy_nano_cents,
						min_sweep_time,
						create_time,
						block_rollup_complete
					)
					VALUES ($1, $2, $3, $4, 0, $5, 0, 0, $6, $6, true)
				`,
				marker.PaymentId,
				plan.PaymentPlanId,
				marker.NetworkId,
				marker.WalletId,
				marker.PayoutNanoCents,
				createTime,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO audit_account_payment (
						event_id,
						event_time,
						payment_id,
						event_type,
						event_details
					)
					VALUES ($1, $2, $1, $3, $4)
				`,
				marker.PaymentId,
				createTime,
				ManualReimbursementEventType,
				marker.details(),
			))
		}
		created = true
	}, server.TxReadCommitted)
	return
}

// The single boolean or text column of the first row of `sql`, as the earning
// census reads: a failed read is an error, never a panic or an admission.
func manualReimbursementScan(ctx context.Context, query server.PgCanQuery, value any, sql string, args ...any) (found bool, returnErr error) {
	result, err := query.Query(ctx, sql, args...)
	if err != nil {
		return false, fmt.Errorf("manual reimbursement marker unavailable: %w", err)
	}
	defer result.Close()
	if result.Next() {
		found = true
		err = result.Scan(value)
	}
	if err = errors.Join(err, result.Err()); err != nil {
		return false, fmt.Errorf("manual reimbursement marker unavailable: %w", err)
	}
	return
}

// The marker of a payment, or nil when the payment is not a manual
// reimbursement. A marker that does not parse is an error.
func manualReimbursementMarkerInQuery(ctx context.Context, query server.PgCanQuery, paymentId server.Id) (*ManualReimbursementMarker, error) {
	var details *string
	found, err := manualReimbursementScan(
		ctx,
		query,
		&details,
		`
			SELECT event_details
			FROM audit_account_payment
			WHERE event_id = $1 AND payment_id = $1 AND event_type = 'manual_reimbursement'
		`,
		paymentId,
	)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	marker := &ManualReimbursementMarker{}
	if details == nil || json.Unmarshal([]byte(*details), marker) != nil || marker.PaymentId != paymentId {
		return nil, ErrManualReimbursementChanged
	}
	return marker, nil
}

// Read only.
func GetManualReimbursementMarker(ctx context.Context, paymentId server.Id) (marker *ManualReimbursementMarker, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		marker, returnErr = manualReimbursementMarkerInQuery(ctx, conn, paymentId)
	})
	return
}

// A marked payment is admitted by its marker alone, never by the earning census.
// Its row must match the marker exactly: the network, the pinned wallet with the
// same owner, address and chain, the payout, and no bytes, subsidy, reliability,
// bonus or sweeps; and a request must carry exactly the listed amount. The
// wallet's active flag is checked where a fresh attempt is reserved, as for any
// payout. `marked` is false for every other payment.
func admitManualReimbursement(ctx context.Context, query server.PgCanQuery, paymentId server.Id, amount *float64) (marked bool, returnErr error) {
	marker, err := manualReimbursementMarkerInQuery(ctx, query, paymentId)
	if err != nil {
		return true, err
	}
	if marker == nil {
		return false, nil
	}
	var matches bool
	_, err = manualReimbursementScan(
		ctx,
		query,
		&matches,
		`
			SELECT COALESCE(
				p.network_id = $2 AND
				p.wallet_id = $3 AND
				p.payout_nano_cents = $4 AND
				p.payout_byte_count = 0 AND
				p.subsidy_payout_nano_cents = 0 AND
				p.reliability_subsidy_nano_cents = 0 AND
				p.bonus_payout_nano_cents = 0 AND
				w.network_id = $2 AND
				w.wallet_address = $5 AND
				w.blockchain = $6 AND
				NOT EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id = p.payment_id) AND
				NOT EXISTS (SELECT 1 FROM account_payment_bonus b WHERE b.payment_id = p.payment_id),
				false
			)
			FROM account_payment p
			INNER JOIN account_wallet w ON w.wallet_id = p.wallet_id
			WHERE p.payment_id = $1
		`,
		paymentId,
		marker.NetworkId,
		marker.WalletId,
		marker.PayoutNanoCents,
		marker.WalletAddress,
		marker.Blockchain,
	)
	if err != nil {
		return true, err
	}
	if !matches || marker.PayoutNanoCents != marker.AmountNanoCents+marker.FeeNanoCents ||
		ManualReimbursementAddressRefused(marker.WalletAddress) {
		return true, ErrManualReimbursementChanged
	}
	if amount != nil && UsdToNanoCents(*amount) != marker.AmountNanoCents {
		return true, ErrManualReimbursementChanged
	}
	return true, nil
}

// Restores a marked payment's pinned wallet while it holds no attempt. Only a
// writer older than the pin can have re-pointed it. Returns whether the wallet
// changed.
func RepinManualReimbursementWallet(ctx context.Context, paymentId server.Id) (repinned bool, returnErr error) {
	marker, err := GetManualReimbursementMarker(ctx, paymentId)
	if err != nil || marker == nil {
		return false, errors.Join(ErrManualReimbursementChanged, err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE account_payment p
				SET wallet_id = $2
				WHERE
					p.payment_id = $1 AND
					p.network_id = $3 AND
					p.wallet_id IS DISTINCT FROM $2 AND
					NOT p.completed AND
					NOT p.canceled AND
					p.circle_idempotency_key IS NULL AND
					p.payment_record IS NULL AND
					p.tx_hash IS NULL AND
					`+manualReimbursementMarked("p")+`
			`,
			paymentId,
			marker.WalletId,
			marker.NetworkId,
		))
		repinned = tag.RowsAffected() == 1
	})
	return
}
