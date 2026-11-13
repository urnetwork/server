// `bringyourctl payouts close-parked --paid=<payment_id>:<circle_transaction_id>[,...]`
// also closes keyed USDC payments whose obligation another Circle transfer
// paid (owner decision 2026-11-12: USDC payouts ended). Such a payment is
// pending with a Circle idempotency key and no payment record or tx hash, so
// the parked census leaves it out: Circle may hold a transfer under its key.
//
// Circle cannot be asked about an idempotency key (circleTransactionLister), so
// the evidence is the complete Circle listing of the payment's wallet address,
// in every state, as for the legacy reconciliation:
//   - the named transfer is in the listing: COMPLETE, an outbound developer
//     transfer from the chain's configured payout wallet, of the configured
//     USDC, with a chain hash, of exactly the payout less the sender's fee at
//     USDC precision (six decimals, as the sender formats its amount)
//   - no listed transfer can be the payment's own attempt. A transfer under
//     the key carries the key's request: an outbound transfer of that same
//     amount, created after the payment, that no other payment row holds as
//     its record. Inbound and end-user transfers, failed ones that moved no
//     funds, older ones and those another payment row holds are not it. The
//     named transfer is held to the same rule, so it must be another payment
//     row's record or older than the payment; one that could be the payment's
//     own attempt is refused, to be bound as its record instead.
//
// The dry run only reads, and refuses the whole command when any entry fails a
// check. The apply refuses as the parked close-out does (payments declared off,
// the digest of the census, which binds each paid row's transfer and hash),
// checks each payment again with the database and Circle just before it
// closes it, and closes it as a parked payment closes: canceled, with one
// usdc_payout_closed event naming the reason and the transfer.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The reason a closure of a keyed payment names.
const payoutClosePaidElsewhereReason = "paid_elsewhere"

var ErrPayoutClosePaidRefused = errors.New("payouts close-parked: a --paid entry failed its checks")

// Observes each paid payment of the apply before it is checked again and
// closed. Nil in production; tests use it to change a payment after the census.
var payoutClosePaidBeforeClose func(paymentId server.Id)

// One `--paid` entry: a keyed payment and the Circle transfer that paid its
// obligation.
type PayoutClosePaidEntry struct {
	PaymentId           server.Id `json:"payment_id"`
	CircleTransactionId string    `json:"circle_transaction_id"`
}

// Parses `<payment_id>:<circle_transaction_id>[,...]`; an empty value names
// none. A Circle transaction id is a canonical uuid. A payment or a transfer
// named twice is refused: each transfer paid one obligation.
func ParsePayoutClosePaid(value string) ([]*PayoutClosePaidEntry, error) {
	entries := []*PayoutClosePaidEntry{}
	if value == "" {
		return entries, nil
	}
	paymentIds := map[server.Id]bool{}
	transactionIds := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		paymentText, transactionId, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("--paid entry %q is not <payment_id>:<circle_transaction_id>", part)
		}
		paymentId, err := server.ParseId(paymentText)
		if err != nil {
			return nil, fmt.Errorf("--paid entry %q has an invalid payment id: %w", part, err)
		}
		if parsed, err := uuid.Parse(transactionId); err != nil || parsed.String() != transactionId {
			return nil, fmt.Errorf("--paid entry %q has an invalid Circle transaction id", part)
		}
		if paymentIds[paymentId] {
			return nil, fmt.Errorf("--paid names payment %s twice", paymentId)
		}
		if transactionIds[transactionId] {
			return nil, fmt.Errorf("--paid names Circle transaction %s twice", transactionId)
		}
		paymentIds[paymentId] = true
		transactionIds[transactionId] = true
		entries = append(entries, &PayoutClosePaidEntry{
			PaymentId:           paymentId,
			CircleTransactionId: transactionId,
		})
	}
	return entries, nil
}

// One paid payment of a census, as checked, or one the apply closed.
type PayoutClosePaidRow struct {
	PaymentId            server.Id       `json:"payment_id"`
	Week                 string          `json:"week"`
	Reason               string          `json:"reason"`
	PayoutNanoCents      model.NanoCents `json:"payout_nano_cents"`
	ManualReimbursement  bool            `json:"manual_reimbursement,omitempty"`
	CircleIdempotencyKey server.Id       `json:"circle_idempotency_key"`
	Blockchain           string          `json:"blockchain"`
	WalletAddress        string          `json:"wallet_address"`
	// the payout less the sender's fee at USDC precision: what the payment's
	// own attempt sends
	SendAmountUsdc      string    `json:"send_amount_usdc"`
	CircleTransactionId string    `json:"circle_transaction_id"`
	TransferAmountUsdc  string    `json:"transfer_amount_usdc"`
	TransferTime        time.Time `json:"transfer_time"`
	TxHash              string    `json:"tx_hash"`
	// the payment rows holding the transfer as their payment record
	RecordOfPaymentIds []server.Id `json:"record_of_payment_ids"`
	// the transfers Circle lists to the wallet address, in every state
	ListedCount int `json:"listed_count"`

	payment *model.UsdcPaidElsewherePayment
}

// An entry that failed its checks.
type PayoutClosePaidRefusal struct {
	PaymentId           server.Id `json:"payment_id"`
	CircleTransactionId string    `json:"circle_transaction_id"`
	Reason              string    `json:"reason"`
}

// The Circle reads of the paid check. Tests supply fakes.
type payoutClosePaidCircle struct {
	lister         circleTransactionLister
	payoutWalletId func(blockchain string) (string, error)
}

func newPayoutClosePaidCircle() *payoutClosePaidCircle {
	return &payoutClosePaidCircle{
		lister:         &CoreCircleApiClient{},
		payoutWalletId: getWalletIdByNetwork,
	}
}

// What the payment's own attempt sends: the payout less the sender's fee,
// formatted as the sender formats it (advancePayment, CreateTransferTransaction).
func payoutClosePaidSendAmount(payout model.NanoCents) string {
	return fmt.Sprintf("%f", model.NanoCentsToUsd(payout)-providerPaymentFeeUsd)
}

// Why the payment cannot be closed as paid elsewhere, or "" when its own state
// allows it.
func payoutClosePaidPaymentRefusal(payment *model.UsdcPaidElsewherePayment) string {
	switch {
	case payment == nil:
		return "no such payment"
	case payment.Completed:
		return "the payment is completed"
	case payment.Closed:
		return "the payment was closed before"
	case payment.Canceled:
		return "the payment is canceled"
	case payment.CircleIdempotencyKey == nil:
		return "the payment has no Circle idempotency key; a payment without one is parked and closes without --paid"
	case payment.PaymentRecord != nil:
		return "the payment has a payment record: Circle holds its transfer, which its status check reconciles"
	case payment.TxHash != nil:
		return "the payment has a tx hash"
	case payment.AssignmentPending:
		return "the payment's plan has a pending sweep assignment"
	case payment.NetworkId == nil:
		return "the payment has no network"
	case payment.WalletAddress == nil || payment.Blockchain == nil || *payment.WalletAddress == "":
		return "the payment's wallet is missing or belongs to another network"
	}
	if _, err := formatBlockchain(*payment.Blockchain); err != nil {
		return fmt.Sprintf("the payment's wallet is on an unsupported chain: %s", err)
	}
	return ""
}

// The decision on one entry from the payment, the complete Circle listing of
// its wallet address, the payment rows holding each listed transfer as their
// record, and the chain's configured payout wallet. Pure.
func payoutClosePaidDecide(
	payment *model.UsdcPaidElsewherePayment,
	entry *PayoutClosePaidEntry,
	transactions []*CircleListedTransaction,
	holders map[string][]*model.UsdcPaymentRecordHolder,
	payoutWalletId string,
) (*PayoutClosePaidRow, string) {
	if refusal := payoutClosePaidPaymentRefusal(payment); refusal != "" {
		return nil, refusal
	}
	chain, _ := formatBlockchain(*payment.Blockchain)
	sendAmount := payoutClosePaidSendAmount(payment.Payout)

	var paying *CircleListedTransaction
	for _, transaction := range transactions {
		if transaction.Id == entry.CircleTransactionId {
			paying = transaction
		}
	}
	switch {
	case paying == nil:
		return nil, fmt.Sprintf("Circle lists no transfer %s to the payment's wallet address on %s", entry.CircleTransactionId, chain)
	case !strings.EqualFold(paying.State, "COMPLETE"):
		return nil, fmt.Sprintf("transfer %s is %s, not COMPLETE", paying.Id, paying.State)
	case !strings.EqualFold(paying.TransactionType, "OUTBOUND") || !strings.EqualFold(paying.Operation, "TRANSFER") || !strings.EqualFold(paying.CustodyType, "DEVELOPER"):
		return nil, fmt.Sprintf("transfer %s is not an outbound developer transfer", paying.Id)
	case payoutWalletId == "" || paying.WalletId != payoutWalletId:
		return nil, fmt.Sprintf("transfer %s is not from the configured %s payout wallet", paying.Id, chain)
	case !paying.usdcAssetVerified:
		return nil, fmt.Sprintf("transfer %s is not of the configured %s USDC", paying.Id, chain)
	case paying.TxHash == "":
		return nil, fmt.Sprintf("transfer %s has no chain hash", paying.Id)
	case len(paying.Amounts) != 1 || !circleDecimalEqual(paying.Amounts[0], sendAmount):
		return nil, fmt.Sprintf("transfer %s pays %v USDC, not the payout less the %s USD fee, %s USDC", paying.Id, paying.Amounts, formatPayoutCloseParkedUsd(model.UsdToNanoCents(providerPaymentFeeUsd)), sendAmount)
	}

	// the transfers that can be the payment's own attempt, the named one included
	windowStart := payment.CreateTime.Add(-legacyProviderPaymentWindowMargin)
	ownIds := []string{}
	for _, transaction := range transactions {
		if strings.EqualFold(transaction.TransactionType, "INBOUND") || strings.EqualFold(transaction.CustodyType, "ENDUSER") ||
			transaction.CreateDate.Before(windowStart) || circleTerminalFailure(transaction) || 0 < len(holders[transaction.Id]) {
			continue
		}
		if len(transaction.Amounts) == 1 && circleDecimalEqual(transaction.Amounts[0], sendAmount) {
			ownIds = append(ownIds, transaction.Id)
		}
	}
	if 0 < len(ownIds) {
		return nil, fmt.Sprintf("Circle may hold the payment's own attempt: transfer(s) %s of %s USDC, created after the payment, are no payment row's record; bind the attempt's transfer as the payment's record instead",
			strings.Join(ownIds, ", "), sendAmount)
	}

	recordOfPaymentIds := []server.Id{}
	for _, holder := range holders[paying.Id] {
		recordOfPaymentIds = append(recordOfPaymentIds, holder.PaymentId)
	}
	return &PayoutClosePaidRow{
		PaymentId:            payment.PaymentId,
		Week:                 payoutCloseParkedWeek(payment.CreateTime),
		Reason:               payoutClosePaidElsewhereReason,
		PayoutNanoCents:      payment.Payout,
		ManualReimbursement:  payment.ManualReimbursement,
		CircleIdempotencyKey: *payment.CircleIdempotencyKey,
		Blockchain:           chain,
		WalletAddress:        *payment.WalletAddress,
		SendAmountUsdc:       sendAmount,
		CircleTransactionId:  paying.Id,
		TransferAmountUsdc:   paying.Amounts[0],
		TransferTime:         paying.CreateDate.UTC(),
		TxHash:               paying.TxHash,
		RecordOfPaymentIds:   recordOfPaymentIds,
		ListedCount:          len(transactions),
		payment:              payment,
	}, ""
}

// Checks one entry with the database and Circle, read only.
func payoutClosePaidCheck(ctx context.Context, circle *payoutClosePaidCircle, entry *PayoutClosePaidEntry) (*PayoutClosePaidRow, string) {
	payment := model.GetUsdcPaidElsewherePayment(ctx, entry.PaymentId)
	if refusal := payoutClosePaidPaymentRefusal(payment); refusal != "" {
		return nil, refusal
	}
	chain, _ := formatBlockchain(*payment.Blockchain)
	payoutWalletId, err := circle.payoutWalletId(chain)
	if err != nil {
		return nil, fmt.Sprintf("no configured %s payout wallet: %s", chain, err)
	}
	transactions, err := circle.lister.ListDestinationTransactions(ctx, chain, *payment.WalletAddress)
	if err != nil {
		return nil, fmt.Sprintf("Circle did not list the transfers to the payment's wallet address: %s", err)
	}
	records := []string{}
	for _, transaction := range transactions {
		records = append(records, transaction.Id)
	}
	return payoutClosePaidDecide(payment, entry, transactions, model.GetUsdcPaymentRecordHolders(ctx, records), payoutWalletId)
}

// The fields the digest binds, and the wallet address the dry run showed, are
// unchanged since the census.
func payoutClosePaidRowUnchanged(census *PayoutClosePaidRow, again *PayoutClosePaidRow) bool {
	return again != nil && census.PaymentId == again.PaymentId && census.PayoutNanoCents == again.PayoutNanoCents &&
		census.CircleIdempotencyKey == again.CircleIdempotencyKey && census.CircleTransactionId == again.CircleTransactionId &&
		census.TxHash == again.TxHash && census.Blockchain == again.Blockchain && census.WalletAddress == again.WalletAddress
}

// The `--paid` value that names exactly the rows.
func payoutClosePaidArgument(rows []*PayoutClosePaidRow) string {
	parts := []string{}
	for _, row := range rows {
		parts = append(parts, row.PaymentId.String()+":"+row.CircleTransactionId)
	}
	return strings.Join(parts, ",")
}

// The audit record of one paid closure: a parked closure's record, with the
// transfer that paid the obligation.
type payoutClosePaidEvent struct {
	payoutCloseParkedEvent
	CircleIdempotencyKey server.Id   `json:"circle_idempotency_key"`
	CircleTransactionId  string      `json:"circle_transaction_id"`
	TxHash               string      `json:"tx_hash"`
	TransferAmountUsdc   string      `json:"transfer_amount_usdc"`
	TransferTime         time.Time   `json:"transfer_time"`
	RecordOfPaymentIds   []server.Id `json:"record_of_payment_ids"`
	ListedCount          int         `json:"listed_count"`
}

// Closes one paid payment in its own transaction, journaling it with the run.
// A failed transaction rolls back and returns its error.
func closePayoutPaid(
	clientSession *session.ClientSession,
	report *PayoutCloseParkedReport,
	row *PayoutClosePaidRow,
) (closed bool, returnErr error) {
	defer func() {
		if value := recover(); value != nil {
			if err, ok := value.(error); ok {
				closed, returnErr = false, err
				return
			}
			panic(value)
		}
	}()
	closed = model.CloseUsdcPaidElsewherePayment(clientSession.Ctx, row.payment, server.NowUtc(), func(payment *model.UsdcPaidElsewherePayment) string {
		details, err := json.Marshal(&payoutClosePaidEvent{
			payoutCloseParkedEvent: payoutCloseParkedEvent{
				Switch:              payoutCloseParkedSwitch,
				RunId:               report.RunId,
				Digest:              report.Digest,
				Reason:              row.Reason,
				Week:                row.Week,
				PaymentPlanId:       payment.PaymentPlanId,
				NetworkId:           *payment.NetworkId,
				WalletId:            payment.WalletId,
				PayoutNanoCents:     payment.Payout,
				ManualReimbursement: payment.ManualReimbursement,
			},
			CircleIdempotencyKey: row.CircleIdempotencyKey,
			CircleTransactionId:  row.CircleTransactionId,
			TxHash:               row.TxHash,
			TransferAmountUsdc:   row.TransferAmountUsdc,
			TransferTime:         row.TransferTime,
			RecordOfPaymentIds:   row.RecordOfPaymentIds,
			ListedCount:          row.ListedCount,
		})
		server.Raise(err)
		return string(details)
	})
	return
}

// Checks each paid payment of the census again and closes it, in census order.
// A payment that changed since the census, or no longer passes its checks, is
// skipped.
func closePayoutPaidRows(
	clientSession *session.ClientSession,
	circle *payoutClosePaidCircle,
	report *PayoutCloseParkedReport,
) (closed []*PayoutClosePaidRow, skipped []*PayoutClosePaidRow, returnErr error) {
	closed, skipped = []*PayoutClosePaidRow{}, []*PayoutClosePaidRow{}
	for _, row := range report.Paid {
		if err := clientSession.Ctx.Err(); err != nil {
			return closed, skipped, err
		}
		// the switch may have turned on since the run began
		if err := requireUsdcPaymentsOff(); err != nil {
			return closed, skipped, err
		}
		if payoutClosePaidBeforeClose != nil {
			payoutClosePaidBeforeClose(row.PaymentId)
		}
		again, _ := payoutClosePaidCheck(clientSession.Ctx, circle, &PayoutClosePaidEntry{
			PaymentId:           row.PaymentId,
			CircleTransactionId: row.CircleTransactionId,
		})
		if !payoutClosePaidRowUnchanged(row, again) {
			skipped = append(skipped, row)
			continue
		}
		ok, err := closePayoutPaid(clientSession, report, again)
		if err != nil {
			return closed, skipped, err
		}
		if !ok {
			skipped = append(skipped, row)
			continue
		}
		closed = append(closed, again)
	}
	return closed, skipped, nil
}

func payoutClosePaidRowText(b *strings.Builder, name string, row *PayoutClosePaidRow) {
	fmt.Fprintf(b, "  %s %s week %s %s %s USD", name, row.PaymentId, row.Week, row.Reason, formatPayoutCloseParkedUsd(row.PayoutNanoCents))
	if row.ManualReimbursement {
		fmt.Fprintf(b, " manual_reimbursement")
	}
	fmt.Fprintf(b, ", Circle idempotency key %s, no payment record or tx hash\n", row.CircleIdempotencyKey)
	fmt.Fprintf(b, "    paid by Circle transfer %s: COMPLETE, created %s, %s USDC on %s to %s\n",
		row.CircleTransactionId, row.TransferTime.Format(time.RFC3339), row.TransferAmountUsdc, row.Blockchain, row.WalletAddress)
	fmt.Fprintf(b, "    tx hash %s\n", row.TxHash)
	fmt.Fprintf(b, "    the payout less the fee at USDC precision is %s USDC: the transfer pays exactly that\n", row.SendAmountUsdc)
	if len(row.RecordOfPaymentIds) == 0 {
		fmt.Fprintf(b, "    the transfer is no payment row's record, and was created before the payment\n")
	} else {
		recordOf := []string{}
		for _, paymentId := range row.RecordOfPaymentIds {
			recordOf = append(recordOf, paymentId.String())
		}
		fmt.Fprintf(b, "    the transfer is the payment record of %s\n", strings.Join(recordOf, ", "))
	}
	fmt.Fprintf(b, "    Circle lists %d transfer(s) to the wallet address, in every state; none can be the payment's own attempt\n", row.ListedCount)
}

// The paid section of the report text: the census, the refused entries and,
// after an apply, what it closed and skipped.
func (self *PayoutCloseParkedReport) paidText(b *strings.Builder) {
	if len(self.Paid) == 0 && len(self.PaidRefused) == 0 {
		return
	}
	total := model.NanoCents(0)
	for _, row := range self.Paid {
		total += row.PayoutNanoCents
	}
	fmt.Fprintf(b, "paid elsewhere (--paid): %d payment(s), %s USD; each is keyed, and closing it records the Circle transfer that paid its obligation\n",
		len(self.Paid), formatPayoutCloseParkedUsd(total))
	for _, row := range self.Paid {
		payoutClosePaidRowText(b, "paid", row)
	}
	for _, refusal := range self.PaidRefused {
		fmt.Fprintf(b, "  refused %s:%s: %s\n", refusal.PaymentId, refusal.CircleTransactionId, refusal.Reason)
	}
}

// The apply's paid closures, after the parked ones.
func (self *PayoutCloseParkedReport) closedPaidText(b *strings.Builder) {
	if self.ClosedPaid == nil && self.SkippedPaid == nil {
		return
	}
	fmt.Fprintf(b, "closed as paid elsewhere: %d payment(s)\n", len(self.ClosedPaid))
	for _, row := range self.ClosedPaid {
		fmt.Fprintf(b, "  closed %s %s, paid by Circle transfer %s, tx hash %s\n", row.PaymentId, row.Reason, row.CircleTransactionId, row.TxHash)
	}
	fmt.Fprintf(b, "skipped as paid elsewhere, changed since the census or no longer passing its checks: %d\n", len(self.SkippedPaid))
	for _, row := range self.SkippedPaid {
		fmt.Fprintf(b, "  skipped %s\n", row.PaymentId)
	}
}
