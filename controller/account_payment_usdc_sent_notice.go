// `bringyourctl payouts usdc-sent-notice` (owner, 2026-10-28): emails each
// listed payment's network that its USDC payout was sent and that USDC payouts
// are ending. A payment counts as sent when its row is completed, or when
// Circle reports its transfer COMPLETE before the status check records it.
//
// A personal network's notice goes to its admin's login. A Team's admin is an
// owner of record that never signs in (D-TEAM-2), so a Team's notice goes to
// its billing recipients, its Owners and Billing admins at their sign-in email
// addresses (model.GetTeamBillingRecipientsInTx, QD-1222), Owners first and
// within the bound on the people a Team's mail reaches (QD-1218), one message
// per address, and the report has a row for each address.
//
// Each notice goes out once: the send journals a usdc_payout_sent_notice
// event, keyed by an id derived from the payment, or for a Team's address from
// the payment and the address, and a rerun skips a notice that has one.
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

// A Team's notice to one of its addresses: the md5 of the prefix, the payment
// id and the address (QD-1222), so each address is journaled once and an
// address the Team gains after a run still gets its notice in the next. The
// text hashed goes on past a personal network's, which ends at the payment id.
func usdcPayoutSentTeamNoticeEventId(paymentId server.Id, userAuth string) server.Id {
	sum := md5.Sum([]byte(UsdcPayoutSentNoticeEventType + "/" + paymentId.String() + "/" + userAuth))
	return server.RequireIdFromBytes(sum[:])
}

// Tunables of the notice (QD-702), from defaultUsdcSentNoticeSettings.
type usdcSentNoticeSettings struct {
	// The characters of the wallet address the notice shows, from its end: 4,
	// enough for a person to tell its own wallet from another, as the payout
	// wallet's security notice shows. A shorter address is unreadable and
	// holds the notice. Non-positive shows none of it.
	WalletSuffixLength int
}

func defaultUsdcSentNoticeSettings() *usdcSentNoticeSettings {
	return &usdcSentNoticeSettings{
		WalletSuffixLength: 4,
	}
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

// The login the notice goes to.
const (
	usdcSentNoticeRecipientEmail = "email"
	usdcSentNoticeRecipientPhone = "phone"
	usdcSentNoticeRecipientNone  = "none"
)

// One listed payment's notice to one recipient, as the notice read it, and
// what the send did. A Team's payment that is due has a row for each of its
// billing recipients' addresses, in their order; any other payment has one.
type PayoutUsdcSentNoticeRow struct {
	PaymentId         server.Id       `json:"payment_id"`
	NetworkId         server.Id       `json:"network_id"`
	TransferNanoCents model.NanoCents `json:"transfer_nano_cents"`
	SentBy            string          `json:"sent_by,omitempty"`
	// the payment's network is a Team, whose notice goes to its billing
	// recipients
	Team bool `json:"team,omitempty"`
	// a Team's address: its place among the Team's billing recipients'
	// addresses, from 1, Owners first, and how many there are; both zero for
	// any other row
	RecipientNumber int    `json:"recipient_number,omitempty"`
	RecipientCount  int    `json:"recipient_count,omitempty"`
	Recipient       string `json:"recipient"`
	Problem         string `json:"problem,omitempty"`
	AlreadyNotified bool   `json:"already_notified,omitempty"`
	Notified        bool   `json:"notified,omitempty"`

	userAuth string
	// the key the send is journaled by
	eventId  server.Id
	template *UsdcPayoutSentTemplate
}

type PayoutUsdcSentNoticeReport struct {
	// dry_run or send
	Mode string                     `json:"mode"`
	Rows []*PayoutUsdcSentNoticeRow `json:"rows"`
}

// Reads each payment's notices and, with `send`, sends each one that is due.
// A notice is due when its payment was sent, its recipient signs in by email,
// and no earlier run journaled it.
func PayoutsUsdcSentNotice(clientSession *session.ClientSession, paymentIds []server.Id, send bool) (*PayoutUsdcSentNoticeReport, error) {
	ctx := clientSession.Ctx
	report := &PayoutUsdcSentNoticeReport{
		Mode: "dry_run",
		Rows: []*PayoutUsdcSentNoticeRow{},
	}
	if send {
		report.Mode = "send"
	}
	reader := &usdcSentNoticeReader{
		ctx:      ctx,
		settings: defaultUsdcSentNoticeSettings(),
	}
	for _, paymentId := range paymentIds {
		report.Rows = append(report.Rows, reader.read(paymentId)...)
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
	settings  *usdcSentNoticeSettings
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

// The payment's rows: for a Team's payment that is due, one for each of its
// billing recipients' addresses; else one, which names what holds a held
// notice.
func (self *usdcSentNoticeReader) read(paymentId server.Id) []*PayoutUsdcSentNoticeRow {
	row := &PayoutUsdcSentNoticeRow{
		PaymentId: paymentId,
		Recipient: usdcSentNoticeRecipientNone,
	}
	rows := []*PayoutUsdcSentNoticeRow{row}
	payment, err := model.GetPayment(self.ctx, paymentId)
	if err != nil || payment == nil {
		row.Problem = "no such payment"
		return rows
	}
	row.NetworkId = payment.NetworkId
	row.TransferNanoCents = payment.Payout - model.UsdToNanoCents(providerPaymentFeeUsd)
	template := &UsdcPayoutSentTemplate{Transfer: row.TransferNanoCents}

	switch {
	case payment.Canceled:
		row.Problem = "the payment is canceled"
		return rows
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
			return rows
		}
		row.SentBy = usdcSentNoticeByCircle
		template.SentTime = transaction.sentTime
		template.TxHash = transaction.txHash
	default:
		row.Problem = "not sent: Circle holds no transfer for it"
		return rows
	}

	if payment.WalletId == nil {
		row.Problem = "the payment has no wallet"
		return rows
	}
	wallet := model.GetAccountWallet(self.ctx, *payment.WalletId)
	walletSuffixLength := max(self.settings.WalletSuffixLength, 0)
	if wallet == nil || wallet.WalletAddress == "" || len(wallet.WalletAddress) < walletSuffixLength {
		row.Problem = "the payment's wallet is unreadable"
		return rows
	}
	template.WalletSuffix = wallet.WalletAddress[len(wallet.WalletAddress)-walletSuffixLength:]

	// a Team's billing recipients, or none for a personal network (QD-1222)
	var teamUserAuths []string
	var team bool
	server.Tx(self.ctx, func(tx server.PgTx) {
		teamUserAuths, team = model.GetTeamBillingRecipientsInTx(self.ctx, tx, []server.Id{payment.NetworkId})[payment.NetworkId]
	})
	if team {
		row.Team = true
		if len(teamUserAuths) == 0 {
			row.Problem = "the Team has no Owner or Billing admin with an email login"
			return rows
		}
		rows = []*PayoutUsdcSentNoticeRow{}
		for i, userAuth := range teamUserAuths {
			addressRow := *row
			addressRow.RecipientNumber = i + 1
			addressRow.RecipientCount = len(teamUserAuths)
			addressRow.Recipient = usdcSentNoticeRecipientEmail
			addressRow.userAuth = userAuth
			addressRow.eventId = usdcPayoutSentTeamNoticeEventId(paymentId, userAuth)
			addressRow.template = template
			addressRow.AlreadyNotified = usdcPayoutSentNoticeJournaled(self.ctx, addressRow.eventId, paymentId)
			rows = append(rows, &addressRow)
		}
		return rows
	}

	_, userAuth, ok := model.GetNetworkAdminUserAuth(self.ctx, payment.NetworkId)
	switch {
	case !ok || userAuth == "":
		row.Problem = "the network's admin has no email login"
		return rows
	case !strings.Contains(userAuth, "@"):
		// SMS sends are blocked, and this notice has no SMS body
		row.Recipient = usdcSentNoticeRecipientPhone
		row.Problem = "the network's admin signs in by phone"
		return rows
	}
	row.Recipient = usdcSentNoticeRecipientEmail
	row.userAuth = userAuth
	row.eventId = usdcPayoutSentNoticeEventId(paymentId)
	row.template = template
	row.AlreadyNotified = usdcPayoutSentNoticeJournaled(self.ctx, row.eventId, paymentId)
	return rows
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

// Whether a notice of the payment is journaled under the event id.
func usdcPayoutSentNoticeJournaled(ctx context.Context, eventId server.Id, paymentId server.Id) (journaled bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM audit_account_payment WHERE event_id = $1 AND payment_id = $2 AND event_type = $3)`,
			eventId,
			paymentId,
			UsdcPayoutSentNoticeEventType,
		).Scan(&journaled))
	})
	return
}

// Journals a sent notice under its row's key. A notice sent twice, because its
// journal write failed after the send, is the only duplicate this allows. A
// Team's notice says so, and the journal holds no address.
func journalUsdcPayoutSentNotice(ctx context.Context, row *PayoutUsdcSentNoticeRow) {
	eventDetails := map[string]any{
		"transfer_nano_cents": row.TransferNanoCents,
		"sent_by":             row.SentBy,
	}
	if row.Team {
		eventDetails["team"] = true
	}
	details, err := json.Marshal(eventDetails)
	server.Raise(err)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO audit_account_payment (event_id, event_time, payment_id, event_type, event_details)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING
			`,
			row.eventId,
			server.NowUtc(),
			row.PaymentId,
			UsdcPayoutSentNoticeEventType,
			string(details),
		))
	})
}

// The report without ids, wallet addresses or recipients: a line for each row,
// numbered by its payment's place in the list, and a Team's rows by their
// address's place among the Team's.
func (self *PayoutUsdcSentNoticeReport) Text() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "payouts usdc-sent-notice, mode %s\n", self.Mode)
	due := 0
	paymentNumber := -1
	for _, row := range self.Rows {
		// a payment's first row; a Team's later addresses follow it
		if row.RecipientNumber <= 1 {
			paymentNumber += 1
		}
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
		payment := fmt.Sprintf("payment %d", paymentNumber)
		if row.Team {
			if 0 < row.RecipientCount {
				payment += fmt.Sprintf(", Team address %d of %d", row.RecipientNumber, row.RecipientCount)
			} else {
				payment += ", Team"
			}
		}
		fmt.Fprintf(b, "  %s: %.2f USDC, sent by %s, recipient %s, %s\n",
			payment, model.NanoCentsToUsd(row.TransferNanoCents), usdcSentNoticeOrUnknown(row.SentBy), row.Recipient, status)
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
