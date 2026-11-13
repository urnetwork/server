// Pure tests of `payouts close-parked --paid` (account_payment_close_paid.go):
// the entry parser, every refusal of the payment's own state, every check of
// the paying transfer and of the payment's own attempt, the digest and the
// text. The ids, addresses and hashes are synthetic.
package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	closePaidTestPayoutWallet = "synthetic-payout-wallet"
	closePaidTestAddress      = "0x00000000000000000000000000000000000000a1"
	// the payout less the fee, 123.446789859 USD, rounds up at USDC precision
	closePaidTestPayout     = model.NanoCents(123_456_789_859)
	closePaidTestSendAmount = "123.446790"
)

var closePaidTestCreated = time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)

// A keyed pending payment on MATIC with no record or tx hash.
func closePaidTestPayment() *model.UsdcPaidElsewherePayment {
	networkId, walletId, key := server.NewId(), server.NewId(), server.NewId()
	address, blockchain := closePaidTestAddress, "MATIC"
	return &model.UsdcPaidElsewherePayment{
		PaymentId:            server.NewId(),
		PaymentPlanId:        server.NewId(),
		NetworkId:            &networkId,
		WalletId:             &walletId,
		Payout:               closePaidTestPayout,
		CircleIdempotencyKey: &key,
		CreateTime:           closePaidTestCreated,
		WalletAddress:        &address,
		Blockchain:           &blockchain,
	}
}

// A COMPLETE outbound developer transfer of the configured USDC from the payout
// wallet to the payment's address, with a chain hash.
func closePaidTestTransfer(created time.Time, amount string) *CircleListedTransaction {
	id := uuid.NewString()
	return &CircleListedTransaction{
		Id:                 id,
		State:              "COMPLETE",
		Blockchain:         "MATIC",
		TransactionType:    "OUTBOUND",
		Operation:          "TRANSFER",
		CustodyType:        "DEVELOPER",
		WalletId:           closePaidTestPayoutWallet,
		DestinationAddress: closePaidTestAddress,
		Amounts:            []string{amount},
		TokenId:            "synthetic-usdc-token",
		TxHash:             "0xsynthetichash" + strings.ReplaceAll(id, "-", ""),
		CreateDate:         created,
		usdcAssetVerified:  true,
	}
}

// The transfer is another payment row's record.
func closePaidTestHeld(transactions ...*CircleListedTransaction) map[string][]*model.UsdcPaymentRecordHolder {
	holders := map[string][]*model.UsdcPaymentRecordHolder{}
	for _, transaction := range transactions {
		holders[transaction.Id] = []*model.UsdcPaymentRecordHolder{{PaymentId: server.NewId(), Completed: true}}
	}
	return holders
}

// The amount is the sender's own: the payout less its fee, formatted as the
// sender and the legacy reconciliation format it.
func TestPayoutClosePaidSendAmountIsTheSendersAmount(t *testing.T) {
	for _, c := range []struct {
		payout model.NanoCents
		want   string
	}{
		{payout: closePaidTestPayout, want: closePaidTestSendAmount},
		{payout: 123_456_789_499, want: "123.446789"},
		{payout: 2_000_000_000, want: "1.990000"},
	} {
		got := payoutClosePaidSendAmount(c.payout)
		_, legacy := legacyProviderPaymentAmount(c.payout)
		if got != c.want || got != legacy {
			t.Errorf("send amount of %d = %q, legacy %q, want %q", c.payout, got, legacy, c.want)
		}
	}
}

func TestParsePayoutClosePaid(t *testing.T) {
	a, b := server.NewId(), server.NewId()
	x, y := uuid.NewString(), uuid.NewString()
	if entries, err := ParsePayoutClosePaid(""); err != nil || len(entries) != 0 {
		t.Fatalf("empty: %v %v", entries, err)
	}
	entries, err := ParsePayoutClosePaid(a.String() + ":" + x + "," + b.String() + ":" + y)
	if err != nil || len(entries) != 2 || entries[0].PaymentId != a || entries[0].CircleTransactionId != x ||
		entries[1].PaymentId != b || entries[1].CircleTransactionId != y {
		t.Fatalf("two entries: %v %v", entries, err)
	}
	for _, value := range []string{
		"x",
		a.String(),
		a.String() + ":",
		":" + x,
		"not-an-id:" + x,
		a.String() + ":" + strings.ToUpper(x),
		a.String() + ":" + x + " ",
		" " + a.String() + ":" + x,
		a.String() + ":" + x + ",",
		a.String() + ":" + x + "," + a.String() + ":" + y,
		a.String() + ":" + x + "," + b.String() + ":" + x,
	} {
		if entries, err := ParsePayoutClosePaid(value); err == nil {
			t.Errorf("accepted %q: %v", value, entries)
		}
	}
}

// Each state of the payment itself that is not a keyed, unsubmitted, pending
// payment of a supported wallet of its network is refused.
func TestPayoutClosePaidPaymentRefusals(t *testing.T) {
	if refusal := payoutClosePaidPaymentRefusal(closePaidTestPayment()); refusal != "" {
		t.Fatalf("a keyed pending payment was refused: %s", refusal)
	}
	record, hash, address, chain := "synthetic-record", "0xsynthetic", "", "BITCOIN"
	for _, c := range []struct {
		change func(*model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment
		want   string
	}{
		{func(*model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment { return nil }, "no such payment"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment { p.Completed = true; return p }, "completed"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment {
			p.Canceled, p.Closed = true, true
			return p
		}, "closed before"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment { p.Canceled = true; return p }, "canceled"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment {
			p.CircleIdempotencyKey = nil
			return p
		}, "no Circle idempotency key"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment {
			p.PaymentRecord = &record
			return p
		}, "payment record"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment { p.TxHash = &hash; return p }, "tx hash"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment {
			p.AssignmentPending = true
			return p
		}, "sweep assignment"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment { p.NetworkId = nil; return p }, "no network"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment {
			p.WalletAddress = nil
			return p
		}, "wallet is missing"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment {
			p.WalletAddress = &address
			return p
		}, "wallet is missing"},
		{func(p *model.UsdcPaidElsewherePayment) *model.UsdcPaidElsewherePayment {
			p.Blockchain = &chain
			return p
		}, "unsupported chain"},
	} {
		refusal := payoutClosePaidPaymentRefusal(c.change(closePaidTestPayment()))
		if !strings.Contains(refusal, c.want) {
			t.Errorf("refusal %q, want one naming %q", refusal, c.want)
		}
		payment := c.change(closePaidTestPayment())
		paying := closePaidTestTransfer(closePaidTestCreated.Add(time.Hour), closePaidTestSendAmount)
		if row, decided := payoutClosePaidDecide(payment, &PayoutClosePaidEntry{CircleTransactionId: paying.Id},
			[]*CircleListedTransaction{paying}, closePaidTestHeld(paying), closePaidTestPayoutWallet); row != nil || decided != refusal {
			t.Errorf("the decision on a refused payment returned %v, %q", row, decided)
		}
	}
}

// The paying transfer another payment row holds as its record is accepted
// among transfers that cannot be the payment's own attempt: inbound or
// end-user ones, failed ones that moved no funds, older ones, ones of another
// amount, and ones other rows hold.
func TestPayoutClosePaidDecideAccepts(t *testing.T) {
	payment := closePaidTestPayment()
	after := closePaidTestCreated.Add(8 * 24 * time.Hour)
	paying := closePaidTestTransfer(after, closePaidTestSendAmount)
	inbound := closePaidTestTransfer(after, closePaidTestSendAmount)
	inbound.TransactionType = "INBOUND"
	endUser := closePaidTestTransfer(after, closePaidTestSendAmount)
	endUser.CustodyType = "ENDUSER"
	failed := closePaidTestTransfer(after, closePaidTestSendAmount)
	failed.State, failed.TxHash = "FAILED", ""
	older := closePaidTestTransfer(closePaidTestCreated.Add(-legacyProviderPaymentWindowMargin-time.Second), closePaidTestSendAmount)
	otherAmount := closePaidTestTransfer(after, "123.446789")
	otherRow := closePaidTestTransfer(after, closePaidTestSendAmount)
	holders := closePaidTestHeld(paying, otherRow)
	transactions := []*CircleListedTransaction{inbound, endUser, failed, older, otherAmount, otherRow, paying}

	row, refusal := payoutClosePaidDecide(payment, &PayoutClosePaidEntry{PaymentId: payment.PaymentId, CircleTransactionId: paying.Id},
		transactions, holders, closePaidTestPayoutWallet)
	if refusal != "" || row == nil {
		t.Fatalf("refused: %s", refusal)
	}
	if row.PaymentId != payment.PaymentId || row.Reason != "paid_elsewhere" || row.PayoutNanoCents != closePaidTestPayout ||
		row.CircleIdempotencyKey != *payment.CircleIdempotencyKey || row.Blockchain != "MATIC" || row.WalletAddress != closePaidTestAddress ||
		row.SendAmountUsdc != closePaidTestSendAmount || row.CircleTransactionId != paying.Id || row.TransferAmountUsdc != closePaidTestSendAmount ||
		!row.TransferTime.Equal(after) || row.TxHash != paying.TxHash || row.ListedCount != len(transactions) || row.Week != "2026-10-19" ||
		len(row.RecordOfPaymentIds) != 1 || row.RecordOfPaymentIds[0] != holders[paying.Id][0].PaymentId {
		t.Fatalf("row %+v", row)
	}
	// a transfer no row holds is accepted when it is older than the payment
	unheld, refusal := payoutClosePaidDecide(payment, &PayoutClosePaidEntry{PaymentId: payment.PaymentId, CircleTransactionId: older.Id},
		transactions, holders, closePaidTestPayoutWallet)
	if refusal != "" || unheld == nil || len(unheld.RecordOfPaymentIds) != 0 {
		t.Fatalf("an older transfer no row holds: %v %q", unheld, refusal)
	}
}

// Every check of the paying transfer refuses on its own.
func TestPayoutClosePaidDecideChecksThePayingTransfer(t *testing.T) {
	after := closePaidTestCreated.Add(8 * 24 * time.Hour)
	for _, c := range []struct {
		change func(*CircleListedTransaction)
		want   string
	}{
		{func(t *CircleListedTransaction) { t.Id = uuid.NewString() }, "lists no transfer"},
		{func(t *CircleListedTransaction) { t.State = "SENT" }, "not COMPLETE"},
		{func(t *CircleListedTransaction) { t.TransactionType = "INBOUND" }, "not an outbound developer transfer"},
		{func(t *CircleListedTransaction) { t.Operation = "CONTRACT_EXECUTION" }, "not an outbound developer transfer"},
		{func(t *CircleListedTransaction) { t.CustodyType = "ENDUSER" }, "not an outbound developer transfer"},
		{func(t *CircleListedTransaction) { t.WalletId = "synthetic-other-wallet" }, "payout wallet"},
		{func(t *CircleListedTransaction) { t.usdcAssetVerified = false }, "configured MATIC USDC"},
		{func(t *CircleListedTransaction) { t.TxHash = "" }, "no chain hash"},
		{func(t *CircleListedTransaction) { t.Amounts = []string{"123.446789"} }, "not the payout less"},
		{func(t *CircleListedTransaction) { t.Amounts = []string{"123.44679"} }, ""},
		{func(t *CircleListedTransaction) {
			t.Amounts = []string{closePaidTestSendAmount, closePaidTestSendAmount}
		}, "not the payout less"},
		{func(t *CircleListedTransaction) { t.Amounts = []string{} }, "not the payout less"},
	} {
		payment := closePaidTestPayment()
		paying := closePaidTestTransfer(after, closePaidTestSendAmount)
		entry := &PayoutClosePaidEntry{PaymentId: payment.PaymentId, CircleTransactionId: paying.Id}
		holders := closePaidTestHeld(paying)
		c.change(paying)
		row, refusal := payoutClosePaidDecide(payment, entry, []*CircleListedTransaction{paying}, holders, closePaidTestPayoutWallet)
		switch {
		case c.want == "" && (refusal != "" || row == nil):
			t.Errorf("an equal decimal amount was refused: %s", refusal)
		case c.want != "" && (row != nil || !strings.Contains(refusal, c.want)):
			t.Errorf("refusal %q, want one naming %q", refusal, c.want)
		}
	}
	// no configured payout wallet refuses
	payment := closePaidTestPayment()
	paying := closePaidTestTransfer(after, closePaidTestSendAmount)
	if row, refusal := payoutClosePaidDecide(payment, &PayoutClosePaidEntry{CircleTransactionId: paying.Id},
		[]*CircleListedTransaction{paying}, closePaidTestHeld(paying), ""); row != nil || !strings.Contains(refusal, "payout wallet") {
		t.Errorf("without a payout wallet: %v %q", row, refusal)
	}
}

// A listed transfer that can be the payment's own attempt refuses the close:
// an outbound transfer of the attempt's amount after the payment that no row
// holds, live or one that moved funds, and the paying transfer itself when no
// row holds it.
func TestPayoutClosePaidDecideRefusesWhenCircleMayHoldTheOwnAttempt(t *testing.T) {
	after := closePaidTestCreated.Add(8 * 24 * time.Hour)
	for _, c := range []struct {
		name   string
		change func(own *CircleListedTransaction)
	}{
		{"complete", func(*CircleListedTransaction) {}},
		{"in flight", func(own *CircleListedTransaction) { own.State = "QUEUED"; own.TxHash = "" }},
		{"failed after moving funds", func(own *CircleListedTransaction) { own.State = "FAILED" }},
		{"inside the margin before the payment", func(own *CircleListedTransaction) { own.CreateDate = closePaidTestCreated.Add(-time.Hour) }},
		{"of an unverified asset", func(own *CircleListedTransaction) { own.usdcAssetVerified = false }},
	} {
		payment := closePaidTestPayment()
		paying := closePaidTestTransfer(after, closePaidTestSendAmount)
		own := closePaidTestTransfer(after.Add(time.Minute), closePaidTestSendAmount)
		c.change(own)
		row, refusal := payoutClosePaidDecide(payment, &PayoutClosePaidEntry{CircleTransactionId: paying.Id},
			[]*CircleListedTransaction{paying, own}, closePaidTestHeld(paying), closePaidTestPayoutWallet)
		if row != nil || !strings.Contains(refusal, "own attempt") || !strings.Contains(refusal, own.Id) {
			t.Errorf("%s: %v %q", c.name, row, refusal)
		}
	}
	payment := closePaidTestPayment()
	paying := closePaidTestTransfer(after, closePaidTestSendAmount)
	row, refusal := payoutClosePaidDecide(payment, &PayoutClosePaidEntry{CircleTransactionId: paying.Id},
		[]*CircleListedTransaction{paying}, map[string][]*model.UsdcPaymentRecordHolder{}, closePaidTestPayoutWallet)
	if row != nil || !strings.Contains(refusal, "own attempt") || !strings.Contains(refusal, paying.Id) {
		t.Errorf("a paying transfer no row holds, after the payment: %v %q", row, refusal)
	}
}

// The digest binds each paid row's payment, payout, key, transfer and hash, in
// any order, and without paid rows it is the parked digest as before.
func TestPayoutCloseParkedDigestBindsPaidRows(t *testing.T) {
	parked := []*PayoutCloseParkedRow{{PaymentId: server.NewId(), PayoutNanoCents: 10}}
	paidRow := func() *PayoutClosePaidRow {
		return &PayoutClosePaidRow{PaymentId: server.NewId(), PayoutNanoCents: closePaidTestPayout, CircleIdempotencyKey: server.NewId(),
			CircleTransactionId: uuid.NewString(), TxHash: "0xsynthetic"}
	}
	first, second := paidRow(), paidRow()
	digest := payoutCloseParkedDigest(parked, []*PayoutClosePaidRow{first, second})
	if payoutCloseParkedDigest(parked, nil) != payoutCloseParkedDigest(parked, []*PayoutClosePaidRow{}) || payoutCloseParkedDigest(parked, nil) == digest {
		t.Fatal("paid rows do not change the digest, or an empty list does")
	}
	if payoutCloseParkedDigest(parked, []*PayoutClosePaidRow{second, first}) != digest {
		t.Fatal("the digest depends on the order of the paid rows")
	}
	for _, change := range []func(*PayoutClosePaidRow){
		func(row *PayoutClosePaidRow) { row.PayoutNanoCents += 1 },
		func(row *PayoutClosePaidRow) { row.CircleIdempotencyKey = server.NewId() },
		func(row *PayoutClosePaidRow) { row.CircleTransactionId = uuid.NewString() },
		func(row *PayoutClosePaidRow) { row.TxHash = "0xother" },
		func(row *PayoutClosePaidRow) { row.PaymentId = server.NewId() },
	} {
		changed := *second
		change(&changed)
		if payoutCloseParkedDigest(parked, []*PayoutClosePaidRow{first, &changed}) == digest {
			t.Fatal("a changed paid row kept the digest")
		}
	}
}

// The text names each paid payment with its key, the paying transfer, its
// amount, time and hash, and what holds it; the apply command names exactly
// the paid rows.
func TestPayoutClosePaidReportText(t *testing.T) {
	payment := closePaidTestPayment()
	paying := closePaidTestTransfer(closePaidTestCreated.Add(8*24*time.Hour), closePaidTestSendAmount)
	holders := closePaidTestHeld(paying)
	row, refusal := payoutClosePaidDecide(payment, &PayoutClosePaidEntry{PaymentId: payment.PaymentId, CircleTransactionId: paying.Id},
		[]*CircleListedTransaction{paying}, holders, closePaidTestPayoutWallet)
	if refusal != "" {
		t.Fatal(refusal)
	}
	report := &PayoutCloseParkedReport{RunId: server.NewId(), Time: closePaidTestCreated, Mode: "dry_run", UsdcPayments: "off",
		Parked: newPayoutCloseParkedSet(nil), Paid: []*PayoutClosePaidRow{row}}
	report.Digest = payoutCloseParkedDigest(nil, report.Paid)
	text := report.Text(false)
	for _, want := range []string{
		"paid elsewhere (--paid): 1 payment(s), 123.456789859 USD",
		"paid " + payment.PaymentId.String() + " week 2026-10-19 paid_elsewhere 123.456789859 USD",
		payment.CircleIdempotencyKey.String(),
		"paid by Circle transfer " + paying.Id + ": COMPLETE, created 2026-10-28T05:00:00Z, 123.446790 USDC on MATIC to " + closePaidTestAddress,
		"tx hash " + paying.TxHash,
		"is " + closePaidTestSendAmount + " USDC: the transfer pays exactly that",
		"the payment record of " + holders[paying.Id][0].PaymentId.String(),
		"Circle lists 1 transfer(s)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the text lacks %q:\n%s", want, text)
		}
	}
	if command := report.ApplyCommand(); command != "bringyourctl payouts close-parked --paid="+payment.PaymentId.String()+":"+paying.Id+" --apply --expect="+report.Digest {
		t.Errorf("apply command %q", command)
	}
	refused := &PayoutCloseParkedReport{Parked: newPayoutCloseParkedSet(nil), PaidRefused: []*PayoutClosePaidRefusal{
		{PaymentId: payment.PaymentId, CircleTransactionId: paying.Id, Reason: "the payment is completed"}}}
	if text := refused.Text(false); !strings.Contains(text, "refused "+payment.PaymentId.String()+":"+paying.Id+": the payment is completed") {
		t.Errorf("the refusal is not in the text:\n%s", text)
	}
	if command := refused.ApplyCommand(); command != "" {
		t.Errorf("a census with nothing to close has an apply command %q", command)
	}
}
