// `bringyourctl payouts usdc-sent-notice` (owner, 2026-10-28): emails the admin
// of each listed payment's network that its USDC payout was sent and that USDC
// payouts are ending. A payment counts as sent when its row is completed, or
// when Circle reports its transfer COMPLETE before the status check records it.
// Each payment's notice goes out once: the send journals a
// usdc_payout_sent_notice event, keyed by an id derived from the payment, and a
// rerun skips a payment that has one.
package controller

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const UsdcPayoutSentNoticeEventType = "usdc_payout_sent_notice"

// The notice's event id: the md5 of a fixed prefix and the payment id, as the
// close-out derives its event ids (model.UsdcPayoutClosedEventId). It is never
// the payment id, which a manual reimbursement's marker takes.
func usdcPayoutSentNoticeEventId(paymentId server.Id) server.Id {
	sum := md5.Sum([]byte(UsdcPayoutSentNoticeEventType + "/" + paymentId.String()))
	return server.RequireIdFromBytes(sum[:])
}

// The notice that one of a network's last USDC payouts was sent. Transfer is
// what the wallet received: the payout less the transfer fee.
type UsdcPayoutSentTemplate struct {
	Transfer     model.NanoCents
	WalletSuffix string
	SentTime     time.Time
	TxHash       string
	BaseTemplate
}

func (self *UsdcPayoutSentTemplate) Name() string {
	return "subscription_usdc_payout_sent"
}

// The transfer in USDC with two decimals, as the templates print it.
func (self *UsdcPayoutSentTemplate) AmountUsdc() string {
	return fmt.Sprintf("%.2f", model.NanoCentsToUsd(self.Transfer))
}

func (self *UsdcPayoutSentTemplate) SentDateText() string {
	return self.SentTime.UTC().Format("January 2, 2006")
}

// The Circle read of a payment whose row is not completed yet. Tests replace it.
type usdcSentNoticeCircle interface {
	GetTransaction(ctx context.Context, id string) (*GetTransactionResult, error)
}

var newUsdcSentNoticeCircle = func() (usdcSentNoticeCircle, error) {
	return NewCoreCircleApiClient(PaymentReadSettings{})
}

// How the notice knows the payment was sent.
const (
	usdcSentNoticeByRecord = "record"
	usdcSentNoticeByCircle = "circle"
)

// The admin login the notice goes to.
const (
	usdcSentNoticeRecipientEmail = "email"
	usdcSentNoticeRecipientPhone = "phone"
	usdcSentNoticeRecipientNone  = "none"
)

// One listed payment, as the notice read it, and what the send did.
type PayoutUsdcSentNoticeRow struct {
	PaymentId         server.Id       `json:"payment_id"`
	NetworkId         server.Id       `json:"network_id"`
	TransferNanoCents model.NanoCents `json:"transfer_nano_cents"`
	SentBy            string          `json:"sent_by,omitempty"`
	Recipient         string          `json:"recipient"`
	Problem           string          `json:"problem,omitempty"`
	AlreadyNotified   bool            `json:"already_notified,omitempty"`
	Notified          bool            `json:"notified,omitempty"`

	userAuth string
	template *UsdcPayoutSentTemplate
}

type PayoutUsdcSentNoticeReport struct {
	// dry_run or send
	Mode string                     `json:"mode"`
	Rows []*PayoutUsdcSentNoticeRow `json:"rows"`
}

// Reads each payment's notice and, with `send`, sends each one that is due.
// A notice is due when its payment was sent, its admin signs in by email, and
// no earlier run journaled it.
func PayoutsUsdcSentNotice(clientSession *session.ClientSession, paymentIds []server.Id, send bool) (*PayoutUsdcSentNoticeReport, error) {
	ctx := clientSession.Ctx
	report := &PayoutUsdcSentNoticeReport{
		Mode: "dry_run",
		Rows: []*PayoutUsdcSentNoticeRow{},
	}
	if send {
		report.Mode = "send"
	}
	reader := &usdcSentNoticeReader{ctx: ctx}
	for _, paymentId := range paymentIds {
		report.Rows = append(report.Rows, reader.read(paymentId))
	}
	if !send {
		return report, nil
	}
	for _, row := range report.Rows {
		if row.Problem != "" || row.AlreadyNotified {
			continue
		}
		if err := GetAWSMessageSender().SendAccountMessageTemplate(row.userAuth, row.template); err != nil {
			// the error can carry the recipient; the report never does
			row.Problem = "the send failed; nothing was journaled, so a rerun sends it"
			continue
		}
		journalUsdcPayoutSentNotice(ctx, row)
		row.Notified = true
	}
	return report, nil
}

// Reads the listed payments, opening the Circle client only for a payment
// whose row is not completed.
type usdcSentNoticeReader struct {
	ctx       context.Context
	circle    usdcSentNoticeCircle
	circleErr error
	circleSet bool
}

func (self *usdcSentNoticeReader) circleClient() (usdcSentNoticeCircle, error) {
	if !self.circleSet {
		self.circle, self.circleErr = newUsdcSentNoticeCircle()
		self.circleSet = true
	}
	return self.circle, self.circleErr
}

func (self *usdcSentNoticeReader) read(paymentId server.Id) *PayoutUsdcSentNoticeRow {
	row := &PayoutUsdcSentNoticeRow{
		PaymentId: paymentId,
		Recipient: usdcSentNoticeRecipientNone,
	}
	payment, err := model.GetPayment(self.ctx, paymentId)
	if err != nil || payment == nil {
		row.Problem = "no such payment"
		return row
	}
	row.NetworkId = payment.NetworkId
	row.TransferNanoCents = payment.Payout - model.UsdToNanoCents(providerPaymentFeeUsd)
	template := &UsdcPayoutSentTemplate{Transfer: row.TransferNanoCents}

	switch {
	case payment.Canceled:
		row.Problem = "the payment is canceled"
		return row
	case payment.Completed:
		row.SentBy = usdcSentNoticeByRecord
		template.SentTime = server.NowUtc()
		if payment.CompleteTime != nil {
			template.SentTime = *payment.CompleteTime
		}
		if payment.TxHash != nil {
			template.TxHash = *payment.TxHash
		}
	case payment.PaymentRecord != nil:
		transaction, problem := self.circleTransfer(*payment.PaymentRecord)
		if problem != "" {
			row.Problem = problem
			return row
		}
		row.SentBy = usdcSentNoticeByCircle
		template.SentTime = transaction.sentTime
		template.TxHash = transaction.txHash
	default:
		row.Problem = "not sent: Circle holds no transfer for it"
		return row
	}

	if payment.WalletId == nil {
		row.Problem = "the payment has no wallet"
		return row
	}
	wallet := model.GetAccountWallet(self.ctx, *payment.WalletId)
	if wallet == nil || len(wallet.WalletAddress) < 4 {
		row.Problem = "the payment's wallet is unreadable"
		return row
	}
	template.WalletSuffix = wallet.WalletAddress[len(wallet.WalletAddress)-4:]

	_, userAuth, ok := model.GetNetworkAdminUserAuth(self.ctx, payment.NetworkId)
	switch {
	case !ok || userAuth == "":
		row.Problem = "the network's admin has no email login"
		return row
	case !strings.Contains(userAuth, "@"):
		// SMS sends are blocked, and this notice has no SMS body
		row.Recipient = usdcSentNoticeRecipientPhone
		row.Problem = "the network's admin signs in by phone"
		return row
	}
	row.Recipient = usdcSentNoticeRecipientEmail
	row.userAuth = userAuth
	row.template = template
	row.AlreadyNotified = usdcPayoutSentNoticeJournaled(self.ctx, paymentId)
	return row
}

type usdcSentNoticeTransfer struct {
	sentTime time.Time
	txHash   string
}

// A transfer Circle reports COMPLETE, or the problem that keeps the notice.
func (self *usdcSentNoticeReader) circleTransfer(transactionId string) (*usdcSentNoticeTransfer, string) {
	circle, err := self.circleClient()
	if err != nil {
		return nil, "Circle is unavailable"
	}
	result, err := circle.GetTransaction(self.ctx, transactionId)
	if err != nil || result == nil {
		return nil, "Circle did not answer"
	}
	if result.Transaction.State != "COMPLETE" {
		return nil, fmt.Sprintf("not sent: Circle reports %s", result.Transaction.State)
	}
	transfer := &usdcSentNoticeTransfer{
		sentTime: server.NowUtc(),
		txHash:   result.Transaction.TxHash,
	}
	// the transfer completed shortly after Circle created it
	if createTime, err := time.Parse(time.RFC3339, result.Transaction.CreateDate); err == nil {
		transfer.sentTime = createTime
	}
	return transfer, ""
}

func usdcPayoutSentNoticeJournaled(ctx context.Context, paymentId server.Id) (journaled bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM audit_account_payment WHERE event_id = $1 AND payment_id = $2 AND event_type = $3)`,
			usdcPayoutSentNoticeEventId(paymentId),
			paymentId,
			UsdcPayoutSentNoticeEventType,
		).Scan(&journaled))
	})
	return
}

// Journals a sent notice. A notice sent twice, because its journal write
// failed after the send, is the only duplicate this allows.
func journalUsdcPayoutSentNotice(ctx context.Context, row *PayoutUsdcSentNoticeRow) {
	details, err := json.Marshal(map[string]any{
		"transfer_nano_cents": row.TransferNanoCents,
		"sent_by":             row.SentBy,
	})
	server.Raise(err)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO audit_account_payment (event_id, event_time, payment_id, event_type, event_details)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING
			`,
			usdcPayoutSentNoticeEventId(row.PaymentId),
			server.NowUtc(),
			row.PaymentId,
			UsdcPayoutSentNoticeEventType,
			string(details),
		))
	})
}

// The report without ids, wallet addresses or recipients.
func (self *PayoutUsdcSentNoticeReport) Text() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "payouts usdc-sent-notice, mode %s\n", self.Mode)
	due := 0
	for i, row := range self.Rows {
		status := "due"
		switch {
		case row.Problem != "":
			status = "held: " + row.Problem
		case row.AlreadyNotified:
			status = "already notified"
		case row.Notified:
			status = "notified"
		default:
			due += 1
		}
		fmt.Fprintf(b, "  payment %d: %.2f USDC, sent by %s, recipient %s, %s\n",
			i, model.NanoCentsToUsd(row.TransferNanoCents), usdcSentNoticeOrUnknown(row.SentBy), row.Recipient, status)
	}
	if self.Mode == "dry_run" {
		fmt.Fprintf(b, "%d notice(s) due; --send sends them\n", due)
	}
	return b.String()
}

func usdcSentNoticeOrUnknown(sentBy string) string {
	if sentBy == "" {
		return "-"
	}
	return sentBy
}
