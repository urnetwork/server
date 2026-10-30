package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestUsdcPayoutSentTemplateText(t *testing.T) {
	sentTime := time.Date(2026, 10, 28, 6, 30, 0, 0, time.UTC)
	template := &UsdcPayoutSentTemplate{
		Transfer:     model.UsdToNanoCents(428.860284),
		WalletSuffix: "Wx<z",
		SentTime:     sentTime,
		TxHash:       "5synthetictransactionhash",
	}
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Your 428.86 USDC payout was sent" {
		t.Fatalf("subject = %q", subject)
	}
	normalText := strings.Join(strings.Fields(bodyText), " ")
	for _, want := range []string{
		"428.86 USDC sent",
		"Solana wallet ending in Wx<z on October 28, 2026",
		"Solana transaction: 5synthetictransactionhash",
		"USDC payouts are ending",
		"https://ur.io/app/account/claim",
		"support@ur.io",
	} {
		if !strings.Contains(normalText, want) {
			t.Errorf("text missing %q", want)
		}
	}
	for _, want := range []string{"428.86 USDC sent", "ending in Wx&lt;z", "5synthetictransactionhash", "https://ur.io/app/account/claim", "USDC payouts are ending"} {
		if !strings.Contains(bodyHtml, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if strings.Contains(bodyHtml, "Wx<z") {
		t.Fatal("wallet suffix was not HTML-escaped")
	}

	// without a transaction hash the line is left out
	template.TxHash = ""
	_, bodyHtml, bodyText, err = RenderEmailTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodyText, "Solana transaction") || strings.Contains(bodyHtml, "Solana transaction") {
		t.Fatal("an empty transaction hash printed its line")
	}
}

type usdcSentNoticeTestSend struct {
	userAuth string
	template *UsdcPayoutSentTemplate
}

type usdcSentNoticeTestSender struct {
	stateLock sync.Mutex
	sends     []usdcSentNoticeTestSend
}

func (self *usdcSentNoticeTestSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sends = append(self.sends, usdcSentNoticeTestSend{userAuth: userAuth, template: template.(*UsdcPayoutSentTemplate)})
	return nil
}

func (self *usdcSentNoticeTestSender) taken() []usdcSentNoticeTestSend {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sends := self.sends
	self.sends = nil
	return sends
}

type usdcSentNoticeTestCircle struct {
	state  string
	txHash string
}

func (self *usdcSentNoticeTestCircle) GetTransaction(ctx context.Context, id string) (*GetTransactionResult, error) {
	return &GetTransactionResult{Transaction: CircleTransaction{
		Id:         id,
		State:      self.state,
		TxHash:     self.txHash,
		CreateDate: "2026-10-27T07:18:41Z",
	}}, nil
}

type usdcSentNoticeTestFixture struct {
	t       testing.TB
	ctx     context.Context
	sender  *usdcSentNoticeTestSender
	circle  *usdcSentNoticeTestCircle
	session *session.ClientSession
}

func newUsdcSentNoticeTestFixture(t testing.TB) *usdcSentNoticeTestFixture {
	ctx := context.Background()
	f := &usdcSentNoticeTestFixture{
		t:       t,
		ctx:     ctx,
		sender:  &usdcSentNoticeTestSender{},
		circle:  &usdcSentNoticeTestCircle{state: "COMPLETE", txHash: "5circlehash"},
		session: session.NewLocalClientSession(ctx, "0.0.0.0:0", nil),
	}
	previousSender := GetAWSMessageSender()
	SetMessageSender(f.sender)
	previousCircle := newUsdcSentNoticeCircle
	newUsdcSentNoticeCircle = func() (usdcSentNoticeCircle, error) {
		return f.circle, nil
	}
	t.Cleanup(func() {
		SetMessageSender(previousSender)
		newUsdcSentNoticeCircle = previousCircle
		f.session.Cancel()
	})
	return f
}

func (self *usdcSentNoticeTestFixture) exec(sql string, args ...any) {
	self.t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, sql, args...))
	})
}

// A network whose admin signs in with `userAuth` (an email when empty), with
// an active Solana payout wallet, and a pending payment of `payout` to it.
func (self *usdcSentNoticeTestFixture) payment(userAuth string, payout model.NanoCents) (paymentId server.Id, adminUserAuth string) {
	networkId := server.NewId()
	// the id's random tail: ids minted in one millisecond share their head
	adminUserAuth = model.Testing_CreateNetwork(self.ctx, networkId, "usdc-sent-"+strings.ReplaceAll(networkId.String(), "-", "")[16:], server.NewId())
	if userAuth != "" {
		self.exec(`UPDATE network_user SET user_auth = $2 FROM network WHERE network.network_id = $1 AND network_user.user_id = network.admin_user_id`, networkId, userAuth)
		adminUserAuth = userAuth
	}
	walletId := server.NewId()
	self.exec(`INSERT INTO account_wallet(wallet_id,network_id,wallet_type,blockchain,wallet_address,active,default_token_type)
		VALUES($1,$2,'external','SOL',$3,true,'USDC')`, walletId, networkId, "Synthetic"+strings.ReplaceAll(networkId.String(), "-", "")+"AbCd")
	paymentId = server.NewId()
	self.exec(`INSERT INTO account_payment(payment_id,payment_plan_id,network_id,wallet_id,payout_byte_count,payout_nano_cents,min_sweep_time,create_time)
		VALUES($1,$2,$3,$4,0,$5,now(),now())`, paymentId, server.NewId(), networkId, walletId, payout)
	return
}

func (self *usdcSentNoticeTestFixture) completed(paymentId server.Id) {
	self.exec(`UPDATE account_payment SET completed = true, complete_time = now(), payment_record = 'synthetic-record', tx_hash = '5recordhash' WHERE payment_id = $1`, paymentId)
}

func (self *usdcSentNoticeTestFixture) atCircle(paymentId server.Id) {
	self.exec(`UPDATE account_payment SET payment_record = 'synthetic-record', circle_idempotency_key = $2 WHERE payment_id = $1`, paymentId, server.NewId())
}

func (self *usdcSentNoticeTestFixture) run(send bool, paymentIds ...server.Id) *PayoutUsdcSentNoticeReport {
	self.t.Helper()
	report, err := PayoutsUsdcSentNotice(self.session, paymentIds, send)
	if err != nil {
		self.t.Fatal(err)
	}
	return report
}

// A completed payment's admin is emailed once; the dry run sends nothing, and
// a rerun finds the journaled notice and sends nothing.
func TestPayoutsUsdcSentNoticeSendsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newUsdcSentNoticeTestFixture(t)
		payout := model.UsdToNanoCents(29.951303)
		paymentId, userAuth := f.payment("", payout)
		f.completed(paymentId)

		dryRun := f.run(false, paymentId)
		if row := dryRun.Rows[0]; row.Problem != "" || row.AlreadyNotified || row.Notified || row.Recipient != "email" || row.SentBy != "record" {
			t.Fatalf("dry run row: problem=%q already=%t notified=%t recipient=%s sent_by=%s", row.Problem, row.AlreadyNotified, row.Notified, row.Recipient, row.SentBy)
		}
		if sends := f.sender.taken(); len(sends) != 0 {
			t.Fatalf("the dry run sent %d notices", len(sends))
		}
		if !strings.Contains(dryRun.Text(), "1 notice(s) due") {
			t.Fatalf("dry run text: %s", dryRun.Text())
		}

		sent := f.run(true, paymentId)
		if !sent.Rows[0].Notified {
			t.Fatalf("the send did not notify: %q", sent.Rows[0].Problem)
		}
		sends := f.sender.taken()
		if len(sends) != 1 || sends[0].userAuth != userAuth {
			t.Fatalf("sent %d notices, want 1 to the admin", len(sends))
		}
		wantTransfer := payout - model.UsdToNanoCents(providerPaymentFeeUsd)
		if sends[0].template.Transfer != wantTransfer || sends[0].template.TxHash != "5recordhash" || sends[0].template.WalletSuffix != "AbCd" {
			t.Fatalf("notice transfer=%d tx=%q suffix=%q", sends[0].template.Transfer, sends[0].template.TxHash, sends[0].template.WalletSuffix)
		}

		rerun := f.run(true, paymentId)
		if !rerun.Rows[0].AlreadyNotified || rerun.Rows[0].Notified {
			t.Fatal("a rerun did not find the journaled notice")
		}
		if sends := f.sender.taken(); len(sends) != 0 {
			t.Fatalf("a rerun sent %d notices", len(sends))
		}
	})
}

// A payment Circle reports COMPLETE is sent before its row records it; one
// Circle has not finished, one Circle never held, and one whose admin signs in
// by phone are held.
func TestPayoutsUsdcSentNoticeHoldsUnsent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newUsdcSentNoticeTestFixture(t)
		atCircle, _ := f.payment("", model.UsdToNanoCents(428.870284))
		f.atCircle(atCircle)
		unsent, _ := f.payment("", model.UsdToNanoCents(5))
		phone, _ := f.payment("+15555550123", model.UsdToNanoCents(7))
		f.completed(phone)

		f.circle.state = "PENDING"
		pending := f.run(true, atCircle)
		if pending.Rows[0].Problem == "" || pending.Rows[0].Notified {
			t.Fatal("a transfer Circle has not finished was notified")
		}

		f.circle.state = "COMPLETE"
		report := f.run(true, atCircle, unsent, phone)
		if row := report.Rows[0]; !row.Notified || row.SentBy != "circle" {
			t.Fatalf("Circle-complete payment: notified=%t sent_by=%s problem=%q", row.Notified, row.SentBy, row.Problem)
		}
		if row := report.Rows[1]; row.Problem == "" || row.Notified {
			t.Fatal("a payment Circle never held was notified")
		}
		if row := report.Rows[2]; row.Recipient != "phone" || row.Problem == "" || row.Notified {
			t.Fatal("a phone admin was notified")
		}
		sends := f.sender.taken()
		if len(sends) != 1 || sends[0].template.TxHash != "5circlehash" || sends[0].template.SentTime.Format(time.RFC3339) != "2026-10-27T07:18:41Z" {
			t.Fatalf("sent %d notices, want only the Circle-complete one with Circle's hash and time", len(sends))
		}
	})
}
