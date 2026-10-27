// Pure tests of `payouts reimburse`: the file, the amounts, the fee math, the
// digest and the ids. No database, Circle or network. Every id, address and
// amount is synthetic.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestPayoutReimbursementAmountParsing(t *testing.T) {
	for text, want := range map[string]int64{
		"123.456789":       123_456_789,
		"0.000001":         1,
		"1":                1_000_000,
		"1.5":              1_500_000,
		"0.654321":         654_321,
		"007.000100":       7_000_100,
		"999999999.999999": 999_999_999_999_999,
	} {
		got, err := parsePayoutReimbursementAmount(text)
		if err != nil || got != want {
			t.Fatalf("%q parsed as %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{
		"", "0", "0.0", "0.000000", "-1", "+1", "1.0000001", "0.0000001", ".5", "5.", "1e3", " 1", "1 ",
		"1,000", "1000000000", "0x10", "NaN", "Inf", "1.5 USDC", "１",
	} {
		if got, err := parsePayoutReimbursementAmount(text); err == nil {
			t.Fatalf("%q was accepted as %d micro USDC", text, got)
		}
	}
	for micro, want := range map[int64]string{1: "0.000001", 654_321: "0.654321", 123_456_789: "123.456789", 7_000_000: "7.000000"} {
		if got := formatPayoutReimbursementMicroUsdc(micro); got != want {
			t.Fatalf("%d formatted as %s, want %s", micro, got, want)
		}
	}
}

func TestPayoutReimbursementCsvRefusals(t *testing.T) {
	networkA, walletA := server.NewId(), server.NewId()
	networkB, walletB := server.NewId(), server.NewId()
	row := func(networkId server.Id, walletId server.Id, amount string) string {
		return fmt.Sprintf("%s,%s,%s\n", networkId, walletId, amount)
	}
	header := payoutReimbursementCsvHeader + "\n"

	inputs, problems := parsePayoutReimbursementCsv([]byte(header + row(networkA, walletA, "1.000001") + row(networkB, walletB, "0.5")))
	if len(problems) != 0 || len(inputs) != 2 || inputs[0].networkId != networkA || inputs[0].walletId != walletA ||
		inputs[0].microUsdc != 1_000_001 || inputs[0].line != 2 || inputs[1].microUsdc != 500_000 || inputs[1].line != 3 {
		t.Fatal("a valid file was not parsed", problems, inputs)
	}

	for name, test := range map[string]struct {
		data string
		want string
	}{
		"empty file":        {"", "no network_id,wallet_id,amount_usdc header"},
		"no rows":           {header, "the file lists no rows"},
		"wrong header":      {"network,solana_address,usdc\n" + row(networkA, walletA, "1"), `the header is "network,solana_address,usdc"`},
		"reordered header":  {"wallet_id,network_id,amount_usdc\n" + row(networkA, walletA, "1"), "not network_id,wallet_id,amount_usdc"},
		"byte order mark":   {"\ufeff" + header + row(networkA, walletA, "1"), "the header is"},
		"too few fields":    {header + fmt.Sprintf("%s,%s\n", networkA, walletA), "wrong number of fields"},
		"too many fields":   {header + fmt.Sprintf("%s,%s,1,2\n", networkA, walletA), "wrong number of fields"},
		"network name":      {header + "synthetic-network-name," + walletA.String() + ",1\n", `network_id "synthetic-network-name" is not an id`},
		"wallet address":    {header + networkA.String() + ",SyntheticSolanaAddress1111111111111111111111,1\n", "is not an id"},
		"zero amount":       {header + row(networkA, walletA, "0.000000"), "is not positive"},
		"negative amount":   {header + row(networkA, walletA, "-1.5"), "is not a decimal USDC amount"},
		"seven decimals":    {header + row(networkA, walletA, "0.1234567"), "at most 6 decimals"},
		"duplicate network": {header + row(networkA, walletA, "1") + row(networkA, walletB, "2"), "repeats line 2"},
	} {
		_, problems := parsePayoutReimbursementCsv([]byte(test.data))
		if !strings.Contains(strings.Join(problems, "\n"), test.want) {
			t.Fatalf("%s: problems %q do not say %q", name, problems, test.want)
		}
	}

	var many strings.Builder
	many.WriteString(header)
	for range payoutReimbursementMaxRows + 1 {
		many.WriteString(row(server.NewId(), server.NewId(), "1"))
	}
	if _, problems := parsePayoutReimbursementCsv([]byte(many.String())); !strings.Contains(strings.Join(problems, "\n"), "more than 1000") {
		t.Fatal("an oversized file was accepted", problems)
	}
}

// The sender deducts its fixed fee from a payout and asks Circle for the rest,
// formatted with %f (advancePayment, CoreCircleApiClient.CreateTransferTransaction).
// Each payout is the listed amount plus that fee, so Circle is asked for exactly
// the listed amount: every micro USDC up to 0.1 USDC, then a dense stride of
// amounts up to 1,000,000 USDC, and the edges.
func TestPayoutReimbursementFeeMath(t *testing.T) {
	fee := model.UsdToNanoCents(providerPaymentFeeUsd)
	if fee != 10_000_000 {
		t.Fatalf("the sender's fee is %d nano cents, not 0.01 USD", fee)
	}
	check := func(micro int64) {
		text := formatPayoutReimbursementMicroUsdc(micro)
		parsed, err := parsePayoutReimbursementAmount(text)
		if err != nil || parsed != micro {
			t.Fatalf("%s parsed as %d, %v", text, parsed, err)
		}
		amount, rowFee, payout, err := payoutReimbursementPayout(micro)
		if err != nil {
			t.Fatal(text, err)
		}
		if amount != micro*1000 || rowFee != fee || payout != amount+fee {
			t.Fatalf("%s: amount %d, fee %d, payout %d", text, amount, rowFee, payout)
		}
		// the sender's own arithmetic, written out independently
		sent := model.NanoCentsToUsd(payout) - 0.01
		if got := fmt.Sprintf("%f", sent); got != text {
			t.Fatalf("%s: the sender would ask Circle for %s", text, got)
		}
		if circleAmount, circleText := payoutReimbursementCircleAmount(payout); circleAmount != sent || circleText != text {
			t.Fatalf("%s: the reported Circle amount %s differs from the sender's", text, circleText)
		}
		if model.UsdToNanoCents(sent) != amount {
			t.Fatalf("%s: the request amount maps to %d nano cents, not %d", text, model.UsdToNanoCents(sent), amount)
		}
	}
	count := 0
	for micro := int64(1); micro <= 100_000; micro += 1 {
		check(micro)
		count += 1
	}
	for micro := int64(100_000); micro <= 1_000_000_000_000; micro += micro/6000 + 7 {
		check(micro)
		count += 1
	}
	for _, micro := range []int64{9_999, 10_000, 10_001, 999_999_999_999, 1_000_000_000_000, 123_456_789_123_456} {
		check(micro)
		count += 1
	}
	if count < 190_000 {
		t.Fatalf("only %d amounts were checked", count)
	}
}

func payoutReimbursementTestRows() []*PayoutReimbursementRow {
	return []*PayoutReimbursementRow{
		{
			NetworkId: server.RequireParseId("00000000-0000-8000-8000-00000000000a"), WalletId: server.RequireParseId("00000000-0000-8000-8000-0000000000a1"),
			Blockchain: "SOL", WalletAddress: "SyntheticSolanaAddress1111111111111111111111", Amount: "12.345678", PayoutNanoCents: 12_355_678_000,
		},
		{
			NetworkId: server.RequireParseId("00000000-0000-8000-8000-00000000000b"), WalletId: server.RequireParseId("00000000-0000-8000-8000-0000000000b1"),
			Blockchain: "MATIC", WalletAddress: "0x00000000000000000000000000000000000000B1", Amount: "0.000123", PayoutNanoCents: 10_123_000,
		},
	}
}

// The digest covers the exact rows and reason: the canonical text below, in
// network order, whatever order the file lists.
func TestPayoutReimbursementDigest(t *testing.T) {
	reason := "synthetic reimbursement to each network's own wallet"
	rows := payoutReimbursementTestRows()
	digest := payoutReimbursementDigest(reason, 10_000_000, rows)

	canonical := strings.Join([]string{
		"urnetwork payouts reimburse v1",
		"reason " + strconv.Quote(reason),
		"fee_nano_cents 10000000",
		`row 00000000-0000-8000-8000-00000000000a 00000000-0000-8000-8000-0000000000a1 "SOL" "SyntheticSolanaAddress1111111111111111111111" 12.345678 12355678000`,
		`row 00000000-0000-8000-8000-00000000000b 00000000-0000-8000-8000-0000000000b1 "MATIC" "0x00000000000000000000000000000000000000B1" 0.000123 10123000`,
	}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("the digest is not the sha256 of the canonical rows and reason", digest)
	}
	if payoutReimbursementDigest(reason, 10_000_000, []*PayoutReimbursementRow{rows[1], rows[0]}) != digest {
		t.Fatal("the digest depends on the file's row order")
	}

	changed := map[string]func(rows []*PayoutReimbursementRow) (string, model.NanoCents){
		"reason":         func(rows []*PayoutReimbursementRow) (string, model.NanoCents) { return reason + ".", 10_000_000 },
		"reason spacing": func(rows []*PayoutReimbursementRow) (string, model.NanoCents) { return reason + " ", 10_000_000 },
		"fee":            func(rows []*PayoutReimbursementRow) (string, model.NanoCents) { return reason, 20_000_000 },
		"amount": func(rows []*PayoutReimbursementRow) (string, model.NanoCents) {
			rows[0].Amount = "12.345679"
			return reason, 10_000_000
		},
		"payout": func(rows []*PayoutReimbursementRow) (string, model.NanoCents) {
			rows[0].PayoutNanoCents += 1
			return reason, 10_000_000
		},
		"wallet": func(rows []*PayoutReimbursementRow) (string, model.NanoCents) {
			rows[0].WalletId = server.RequireParseId("00000000-0000-8000-8000-0000000000a2")
			return reason, 10_000_000
		},
		"address": func(rows []*PayoutReimbursementRow) (string, model.NanoCents) {
			rows[1].WalletAddress = strings.ToLower(rows[1].WalletAddress)
			return reason, 10_000_000
		},
		"chain": func(rows []*PayoutReimbursementRow) (string, model.NanoCents) {
			rows[1].Blockchain = "POLYGON"
			return reason, 10_000_000
		},
		"network": func(rows []*PayoutReimbursementRow) (string, model.NanoCents) {
			rows[1].NetworkId = server.RequireParseId("00000000-0000-8000-8000-00000000000c")
			return reason, 10_000_000
		},
	}
	for name, change := range changed {
		rows := payoutReimbursementTestRows()
		changedReason, fee := change(rows)
		if payoutReimbursementDigest(changedReason, fee, rows) == digest {
			t.Fatalf("a changed %s keeps the digest", name)
		}
	}
	if payoutReimbursementDigest(reason, 10_000_000, rows[:1]) == digest {
		t.Fatal("a dropped row keeps the digest")
	}
}

// The plan and payment ids derive from the digest, so the same rows and reason
// always name the same payments.
func TestPayoutReimbursementIds(t *testing.T) {
	digest := payoutReimbursementDigest("reason", 10_000_000, payoutReimbursementTestRows())
	other := payoutReimbursementDigest("other reason", 10_000_000, payoutReimbursementTestRows())
	networkA, networkB := server.NewId(), server.NewId()
	ids := []server.Id{
		payoutReimbursementPlanId(digest), payoutReimbursementPlanId(other),
		payoutReimbursementPaymentId(digest, networkA), payoutReimbursementPaymentId(digest, networkB),
		payoutReimbursementPaymentId(other, networkA),
	}
	seen := map[server.Id]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatal("two ids collide", ids)
		}
		seen[id] = true
		if id[6]>>4 != 8 || id[8]>>6 != 2 {
			t.Fatal("not a version 8 RFC 9562 id", id)
		}
	}
	if payoutReimbursementPlanId(digest) != ids[0] || payoutReimbursementPaymentId(digest, networkA) != ids[2] {
		t.Fatal("the ids are not deterministic")
	}
}
