package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

const attemptedCloseTestPayoutWallet = "synthetic-payout-wallet"

// A keyed payment of the payout, created at `createTime`, to `address`.
func attemptedCloseTestRow(payout model.NanoCents, createTime time.Time, address string) *UsdcAttemptedRow {
	blockchain := "SOL"
	return newUsdcAttemptedRow(&model.UsdcAttemptedPayment{
		PaymentId:            server.NewId(),
		NetworkId:            server.NewId(),
		Payout:               payout,
		CircleIdempotencyKey: attemptedCloseTestPointer(server.NewId()),
		CreateTime:           createTime,
		WalletAddress:        &address,
		Blockchain:           &blockchain,
	})
}

func attemptedCloseTestPointer[T any](value T) *T {
	return &value
}

// A verified USDC payout transfer of `amount` to `address`, from the payout
// wallet, at `createTime`.
func attemptedCloseTestTransfer(amount string, state string, txHash string, createTime time.Time, address string) *circleAttemptedTransfer {
	return &circleAttemptedTransfer{
		Id:                 server.NewId().String(),
		State:              state,
		Blockchain:         "SOL",
		TransactionType:    "OUTBOUND",
		Operation:          "TRANSFER",
		CustodyType:        "DEVELOPER",
		WalletId:           attemptedCloseTestPayoutWallet,
		DestinationAddress: address,
		Amounts:            []string{amount},
		TokenId:            "synthetic-usdc",
		TxHash:             txHash,
		CreateDate:         createTime,
		usdcAssetVerified:  true,
	}
}

func attemptedCloseTestDecisions(rows []*UsdcAttemptedRow) []string {
	decisions := []string{}
	for _, row := range rows {
		decisions = append(decisions, row.Class+"/"+row.Reason)
	}
	return decisions
}

// A destination's keyed payments are decided from its complete history, as
// the legacy reconciliation decided them: a payment with no live transfer
// attributed to it by its exact amount created none and is forfeited; one
// with a COMPLETE transfer is paid, and one with a transfer in progress is in
// flight. A failed transfer moved no funds, a transfer another payment owns or
// created before the payments less a day is not theirs, and anything that
// cannot be attributed to exactly one payment holds the whole destination.
func TestUsdcKeyedDestinationDecisions(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	address := "synthetic-destination"
	// 1.01 USD and 2.01 USD payouts send 1 and 2 USDC
	one, two := model.UsdToNanoCents(1.01), model.UsdToNanoCents(2.01)
	for _, c := range []struct {
		name      string
		payouts   []model.NanoCents
		transfers func(owned *circleAttemptedTransfer) []*circleAttemptedTransfer
		want      []string
	}{
		{name: "no transfer", payouts: []model.NanoCents{one, two},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer { return nil },
			want:      []string{"closed/circle_absent", "closed/circle_absent"}},
		{name: "one paid", payouts: []model.NanoCents{one, two},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{attemptedCloseTestTransfer("2.000000", "COMPLETE", "synthetic-hash", created.Add(time.Hour), address)}
			},
			want: []string{"closed/circle_absent", "paid/circle_complete"}},
		{name: "in flight", payouts: []model.NanoCents{one},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{attemptedCloseTestTransfer("1", "SENT", "", created.Add(time.Hour), address)}
			},
			want: []string{"in_flight/circle_sent"}},
		{name: "failed moved nothing", payouts: []model.NanoCents{one},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{attemptedCloseTestTransfer("1", "FAILED", "", created.Add(time.Hour), address)}
			},
			want: []string{"closed/circle_absent"}},
		{name: "owned by another payment", payouts: []model.NanoCents{one},
			transfers: func(owned *circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{owned}
			},
			want: []string{"closed/circle_absent"}},
		{name: "before the window", payouts: []model.NanoCents{one},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-hash", created.Add(-48*time.Hour), address)}
			},
			want: []string{"closed/circle_absent"}},
		{name: "foreign wallet", payouts: []model.NanoCents{one, two},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				transfer := attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-hash", created.Add(time.Hour), address)
				transfer.WalletId = "synthetic-other-wallet"
				return []*circleAttemptedTransfer{transfer}
			},
			want: []string{"held/unattributable_transfer", "held/unattributable_transfer"}},
		{name: "no payment's amount", payouts: []model.NanoCents{one},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{attemptedCloseTestTransfer("3", "COMPLETE", "synthetic-hash", created.Add(time.Hour), address)}
			},
			want: []string{"held/unattributable_transfer"}},
		{name: "two payments' amount", payouts: []model.NanoCents{one, one},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-hash", created.Add(time.Hour), address)}
			},
			want: []string{"held/unattributable_transfer", "held/unattributable_transfer"}},
		{name: "two transfers for one payment", payouts: []model.NanoCents{one},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{
					attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-hash", created.Add(time.Hour), address),
					attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-hash-2", created.Add(2*time.Hour), address),
				}
			},
			want: []string{"held/several_transfers"}},
		{name: "unverified asset", payouts: []model.NanoCents{one},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				transfer := attemptedCloseTestTransfer("1", "FAILED", "", created.Add(time.Hour), address)
				transfer.usdcAssetVerified = false
				return []*circleAttemptedTransfer{transfer}
			},
			want: []string{"held/asset_unverified"}},
		{name: "complete without a chain hash", payouts: []model.NanoCents{one},
			transfers: func(*circleAttemptedTransfer) []*circleAttemptedTransfer {
				return []*circleAttemptedTransfer{attemptedCloseTestTransfer("1", "COMPLETE", "", created.Add(time.Hour), address)}
			},
			want: []string{"held/complete_without_hash"}},
	} {
		rows := []*UsdcAttemptedRow{}
		for _, payout := range c.payouts {
			rows = append(rows, attemptedCloseTestRow(payout, created, address))
		}
		owned := attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-owned-hash", created.Add(time.Hour), address)
		owners := map[string][]server.Id{owned.Id: {server.NewId()}}
		decideUsdcKeyedDestination(rows, c.transfers(owned), owners, attemptedCloseTestPayoutWallet)
		if got := attemptedCloseTestDecisions(rows); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: decisions %v, want %v", c.name, got, c.want)
		}
	}
}

// A payment with a payment record is decided by that transfer alone, which
// must be the payment's: its destination, chain, asset, payout wallet and the
// legacy submit's amount or its token amount.
func TestUsdcRecordedPaymentDecisions(t *testing.T) {
	created := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	address := "synthetic-destination"
	for _, c := range []struct {
		name     string
		transfer func() *circleAttemptedTransfer
		want     string
	}{
		{name: "complete", want: "paid/circle_complete", transfer: func() *circleAttemptedTransfer {
			return attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-hash", created, address)
		}},
		{name: "token amount", want: "paid/circle_complete", transfer: func() *circleAttemptedTransfer {
			return attemptedCloseTestTransfer("0.9", "COMPLETE", "synthetic-hash", created, address)
		}},
		{name: "failed", want: "closed/circle_failed", transfer: func() *circleAttemptedTransfer {
			return attemptedCloseTestTransfer("1", "DENIED", "", created, address)
		}},
		{name: "in flight", want: "in_flight/circle_queued", transfer: func() *circleAttemptedTransfer {
			return attemptedCloseTestTransfer("1", "QUEUED", "", created, address)
		}},
		{name: "another destination", want: "held/record_mismatch", transfer: func() *circleAttemptedTransfer {
			return attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-hash", created, "synthetic-other-destination")
		}},
		{name: "another amount", want: "held/record_mismatch", transfer: func() *circleAttemptedTransfer {
			return attemptedCloseTestTransfer("2", "COMPLETE", "synthetic-hash", created, address)
		}},
		{name: "unverified asset", want: "held/record_mismatch", transfer: func() *circleAttemptedTransfer {
			transfer := attemptedCloseTestTransfer("1", "COMPLETE", "synthetic-hash", created, address)
			transfer.usdcAssetVerified = false
			return transfer
		}},
	} {
		row := attemptedCloseTestRow(model.UsdToNanoCents(1.01), created, address)
		row.payment.TokenAmount = attemptedCloseTestPointer(0.9)
		transfer := c.transfer()
		row.payment.PaymentRecord = &transfer.Id
		decideUsdcRecordedPayment(row, "SOL", transfer, attemptedCloseTestPayoutWallet)
		if got := row.Class + "/" + row.Reason; got != c.want {
			t.Errorf("%s: decision %s, want %s", c.name, got, c.want)
		}
	}
}

// The digest binds every decision: the same decisions digest alike, and any
// changed field changes it.
func TestUsdcAttemptedDigestBindsEveryDecision(t *testing.T) {
	row := attemptedCloseTestRow(model.UsdToNanoCents(1.01), time.Now(), "synthetic-destination")
	row.decide(UsdcAttemptedClosed, "circle_absent")
	digest := usdcAttemptedDigest([]*UsdcAttemptedRow{row})
	if digest != usdcAttemptedDigest([]*UsdcAttemptedRow{row}) {
		t.Fatal("the same decisions digest differently")
	}
	for _, change := range []func(*UsdcAttemptedRow){
		func(row *UsdcAttemptedRow) { row.Class = UsdcAttemptedPaid },
		func(row *UsdcAttemptedRow) { row.Reason = "circle_failed" },
		func(row *UsdcAttemptedRow) { row.PayoutNanoCents += 1 },
		func(row *UsdcAttemptedRow) { row.Record = "synthetic-record" },
		func(row *UsdcAttemptedRow) { row.TxHash = "synthetic-hash" },
		func(row *UsdcAttemptedRow) { row.Replaced = true },
	} {
		changed := *row
		change(&changed)
		if usdcAttemptedDigest([]*UsdcAttemptedRow{&changed}) == digest {
			t.Errorf("a changed decision kept the digest: %+v", changed)
		}
	}
}
