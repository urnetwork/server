package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

func TestParsePayoutsUsdcSentNoticeIds(t *testing.T) {
	a, b := server.NewId(), server.NewId()
	paymentIds, err := parsePayoutsUsdcSentNoticeIds([]byte("# the two left\n" + a.String() + "\n\n  " + b.String() + "  \n"))
	if err != nil || len(paymentIds) != 2 || paymentIds[0] != a || paymentIds[1] != b {
		t.Fatalf("parsed %d ids, err %v", len(paymentIds), err)
	}
	for name, data := range map[string]string{
		"malformed": a.String() + "\nnot-an-id\n",
		"repeated":  a.String() + "\n" + a.String() + "\n",
		"empty":     "# nothing\n\n",
	} {
		if _, err := parsePayoutsUsdcSentNoticeIds([]byte(data)); err == nil {
			t.Errorf("%s file was accepted", name)
		}
	}
}

func TestRunPayoutsUsdcSentNotice(t *testing.T) {
	a := server.NewId()
	read := func(string) ([]byte, error) { return []byte(a.String() + "\n"), nil }
	var got []server.Id
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := runPayoutsUsdcSentNotice(stdout, stderr, "ids.txt", read, func(paymentIds []server.Id) (*controller.PayoutUsdcSentNoticeReport, error) {
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

	code = runPayoutsUsdcSentNotice(stdout, stderr, "missing.txt", func(string) ([]byte, error) { return nil, errors.New("no file") }, nil)
	if code == 0 {
		t.Fatal("an unreadable file exited zero")
	}
}
