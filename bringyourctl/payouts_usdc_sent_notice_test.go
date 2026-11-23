package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

func TestParsePayoutsUsdcSentNoticeIds(t *testing.T) {
	settings := defaultPayoutsUsdcSentNoticeSettings()
	a, b := server.NewId(), server.NewId()
	paymentIds, err := parsePayoutsUsdcSentNoticeIds([]byte("# the two left\n"+a.String()+"\n\n  "+b.String()+"  \n"), settings)
	if err != nil || len(paymentIds) != 2 || paymentIds[0] != a || paymentIds[1] != b {
		t.Fatalf("parsed %d ids, err %v", len(paymentIds), err)
	}
	for name, data := range map[string]string{
		"malformed": a.String() + "\nnot-an-id\n",
		"repeated":  a.String() + "\n" + a.String() + "\n",
		"empty":     "# nothing\n\n",
	} {
		if _, err := parsePayoutsUsdcSentNoticeIds([]byte(data), settings); err == nil {
			t.Errorf("%s file was accepted", name)
		}
	}
}

// A file with more payment ids than the settings' MaxPaymentCount is refused whole, one at the bound is read, and a
// non-positive bound reads any number.
func TestParsePayoutsUsdcSentNoticeIdsFollowsItsBound(t *testing.T) {
	if defaultPayoutsUsdcSentNoticeSettings().MaxPaymentCount != 1024 {
		t.Fatalf("the default bound is %d, want 1024", defaultPayoutsUsdcSentNoticeSettings().MaxPaymentCount)
	}
	data := []byte(server.NewId().String() + "\n" + server.NewId().String() + "\n" + server.NewId().String() + "\n")
	for _, c := range []struct {
		maxPaymentCount int
		accepted        bool
	}{
		{maxPaymentCount: 2, accepted: false},
		{maxPaymentCount: 3, accepted: true},
		{maxPaymentCount: 0, accepted: true},
		{maxPaymentCount: -1, accepted: true},
	} {
		paymentIds, err := parsePayoutsUsdcSentNoticeIds(data, &payoutsUsdcSentNoticeSettings{MaxPaymentCount: c.maxPaymentCount})
		if c.accepted && (err != nil || len(paymentIds) != 3) {
			t.Errorf("bound %d: read %d ids, err %v; want all 3", c.maxPaymentCount, len(paymentIds), err)
		}
		if !c.accepted && (err == nil || paymentIds != nil) {
			t.Errorf("bound %d: read %d ids; want the file refused", c.maxPaymentCount, len(paymentIds))
		}
	}
}

func TestRunPayoutsUsdcSentNotice(t *testing.T) {
	settings := defaultPayoutsUsdcSentNoticeSettings()
	a := server.NewId()
	read := func(string) ([]byte, error) { return []byte(a.String() + "\n"), nil }
	var got []server.Id
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := runPayoutsUsdcSentNotice(stdout, stderr, "ids.txt", read, settings, func(paymentIds []server.Id) (*controller.PayoutUsdcSentNoticeReport, error) {
		got = paymentIds
		return &controller.PayoutUsdcSentNoticeReport{Mode: "dry_run", Rows: []*controller.PayoutUsdcSentNoticeRow{{PaymentId: a, Recipient: "email", SentBy: "record"}}}, nil
	})
	if code != 0 || len(got) != 1 || got[0] != a {
		t.Fatalf("exit %d, ids %d, stderr %q", code, len(got), stderr.String())
	}
	if strings.Contains(stdout.String(), a.String()) {
		t.Fatal("the report printed a payment id")
	}
	if !strings.Contains(stdout.String(), "1 notice(s) due") {
		t.Fatalf("report: %s", stdout.String())
	}

	code = runPayoutsUsdcSentNotice(stdout, stderr, "missing.txt", func(string) ([]byte, error) { return nil, errors.New("no file") }, settings, nil)
	if code == 0 {
		t.Fatal("an unreadable file exited zero")
	}
}

// A Team's payment prints a line for each of its addresses, numbered among the Team's, under the payment's own
// number, and counts each address's notice that is due; the payment after it keeps its number in the list.
func TestRunPayoutsUsdcSentNoticePrintsATeamsAddresses(t *testing.T) {
	teamPaymentId, personalPaymentId := server.NewId(), server.NewId()
	read := func(string) ([]byte, error) {
		return []byte(teamPaymentId.String() + "\n" + personalPaymentId.String() + "\n"), nil
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := runPayoutsUsdcSentNotice(stdout, stderr, "ids.txt", read, defaultPayoutsUsdcSentNoticeSettings(), func(paymentIds []server.Id) (*controller.PayoutUsdcSentNoticeReport, error) {
		return &controller.PayoutUsdcSentNoticeReport{Mode: "dry_run", Rows: []*controller.PayoutUsdcSentNoticeRow{
			{PaymentId: teamPaymentId, SentBy: "record", Team: true, RecipientNumber: 1, RecipientCount: 3, Recipient: "email"},
			{PaymentId: teamPaymentId, SentBy: "record", Team: true, RecipientNumber: 2, RecipientCount: 3, Recipient: "email", AlreadyNotified: true},
			{PaymentId: teamPaymentId, SentBy: "record", Team: true, RecipientNumber: 3, RecipientCount: 3, Recipient: "email"},
			{PaymentId: personalPaymentId, SentBy: "record", Recipient: "email"},
		}}, nil
	})
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"payment 0, Team address 1 of 3: 0.00 USDC, sent by record, recipient email, due",
		"payment 0, Team address 2 of 3: 0.00 USDC, sent by record, recipient email, already notified",
		"payment 0, Team address 3 of 3: 0.00 USDC, sent by record, recipient email, due",
		"payment 1: 0.00 USDC, sent by record, recipient email, due",
		"3 notice(s) due",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, teamPaymentId.String()) || strings.Contains(text, personalPaymentId.String()) {
		t.Fatal("the report printed a payment id")
	}
}
