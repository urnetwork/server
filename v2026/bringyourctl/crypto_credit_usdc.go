// `bringyourctl crypto credit-usdc` (controller/crypto_pay_operator_usdc.go):
// credits a USDC invoice from one recorded unfulfilled Solana payment that the
// webhook could not match, once the owner approved it. Support inbox 8228,
// 8229 and 8241, owner queue OQ-26.
package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func cryptoCreditUsdcCommand(opts docopt.Opts, stdout io.Writer, stderr io.Writer) int {
	intentIdValue, _ := opts.String("--intent")
	signatureValue, _ := opts.String("--tx")
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	// The dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path.
	if !commit {
		server.RequirePgReadOnly()
	}
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	return runCryptoCreditUsdc(
		stdout,
		stderr,
		intentIdValue,
		signatureValue,
		commit,
		func(intentId server.Id, signature string) (*controller.CryptoUsdcCreditPlan, error) {
			if commit {
				return controller.CryptoOperatorCommitUsdcCredit(clientSession, intentId, signature, expect)
			}
			return controller.CryptoOperatorPlanUsdcCredit(clientSession.Ctx, intentId, signature)
		},
		func() bool {
			return model.WaitSalesPaymentNotices(salesPaymentNoticeWait)
		},
	)
}

// Prints the plan and returns the exit code: 0 for a dry run that can credit
// and for a commit that credited; 1 for a refusal or a failure, which write
// nothing; 2 for an argument that does not parse. A dry run that can credit
// ends with the command that credits exactly what it printed. A commit that
// credited waits for the payment's sales channel notice, as `crypto credit`
// does, and ends with its audit line.
func runCryptoCreditUsdc(
	stdout io.Writer,
	stderr io.Writer,
	intentIdValue string,
	signatureValue string,
	commit bool,
	run func(intentId server.Id, signature string) (*controller.CryptoUsdcCreditPlan, error),
	waitSalesNotices func() bool,
) int {
	intentId, err := server.ParseId(strings.TrimSpace(intentIdValue))
	if err != nil {
		fmt.Fprintf(stderr, "crypto credit-usdc: --intent %q: %s\n", intentIdValue, err)
		return 2
	}
	signature := strings.TrimSpace(signatureValue)
	if signature == "" {
		fmt.Fprintf(stderr, "crypto credit-usdc: --tx is empty\n")
		return 2
	}

	plan, err := run(intentId, signature)
	if plan != nil {
		fmt.Fprint(stdout, cryptoCreditUsdcText(plan, commit))
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	if plan == nil {
		fmt.Fprintf(stderr, "crypto credit-usdc: no plan; nothing was written\n")
		return 1
	}
	switch plan.State {
	case controller.CryptoUsdcCreditStateCredit:
		if !commit {
			fmt.Fprintf(
				stdout,
				"to credit exactly this: bringyourctl crypto credit-usdc --intent=%s --tx=%s --commit --expect=%s\n",
				plan.IntentId,
				plan.Signature,
				plan.Digest,
			)
			return 0
		}
	case controller.CryptoUsdcCreditStateCredited:
		if commit {
			// the sales channel notice of the payment posts from this process
			if !waitSalesNotices() {
				fmt.Fprintf(stdout, "the sales channel notice was not posted before exit\n")
			}
			// the audit line, also in the process log
			line := cryptoCreditUsdcAuditLine(plan)
			fmt.Fprintf(stdout, "%s\n", line)
			glog.Infof("[cryptopay]%s\n", line)
			return 0
		}
	}
	fmt.Fprintf(stderr, "%s\n", controller.ErrCryptoUsdcCreditRefused)
	return 1
}

// One line naming the invoice, the payment, the network, the amounts, the
// credit and the digest.
func cryptoCreditUsdcAuditLine(plan *controller.CryptoUsdcCreditPlan) string {
	networkId := "none"
	quoteAtomic := int64(0)
	if invoice := plan.Invoice; invoice != nil {
		quoteAtomic = invoice.AmountAtomic
		if invoice.NetworkId != nil {
			networkId = invoice.NetworkId.String()
		}
	}
	receivedAtomic := int64(0)
	if plan.Payment != nil {
		receivedAtomic = plan.Payment.AmountAtomic
	}
	return fmt.Sprintf(
		"audit crypto_credit_usdc intent_id=%s tx=%s network_id=%s received_atomic=%d quote_atomic=%d usd_nano_cents=%d late=%t digest=%s",
		plan.IntentId,
		plan.Signature,
		networkId,
		receivedAtomic,
		quoteAtomic,
		plan.UsdNanoCents,
		plan.Late,
		plan.Digest,
	)
}

// The plan as text. It prints no email and no status token: the plan carries
// neither.
func cryptoCreditUsdcText(plan *controller.CryptoUsdcCreditPlan, commit bool) string {
	formatTime := func(t time.Time) string {
		return t.UTC().Format(time.RFC3339)
	}
	usd := func(nanoCents model.NanoCents) string {
		return fmt.Sprintf("$%.2f", model.NanoCentsToUsd(nanoCents))
	}
	usdc := func(amountAtomic int64) string {
		return cryptoFormatAmount(model.CryptoAssetUsdcSolana, amountAtomic)
	}
	b := &strings.Builder{}
	mode := "dry run (read only)"
	if commit {
		mode = "commit"
	}
	fmt.Fprintf(b, "crypto credit-usdc: %s at %s\n", mode, formatTime(plan.Time))

	if invoice := plan.Invoice; invoice != nil {
		fmt.Fprintf(b, "invoice %s: %s %s, tier %q, delivery %s\n", invoice.IntentId, invoice.Asset, invoice.Item, invoice.Tier, invoice.Delivery)
		switch {
		case invoice.NetworkId != nil && invoice.NetworkExists:
			fmt.Fprintf(b, "  linked to network %s\n", *invoice.NetworkId)
		case invoice.NetworkId != nil:
			fmt.Fprintf(b, "  linked to network %s, which no longer exists\n", *invoice.NetworkId)
		case invoice.Delivery == model.CryptoDeliveryCode:
			fmt.Fprintf(b, "  linked to no network: it delivers a balance code\n")
		default:
			fmt.Fprintf(b, "  linked to no network: unlinked from it (retention)\n")
		}
		fmt.Fprintf(
			b,
			"  price %s; quote %s, quoted %s, expires %s\n",
			usd(invoice.PriceUsdNanoCents),
			usdc(invoice.AmountAtomic),
			formatTime(invoice.QuotedAt),
			formatTime(invoice.ExpiresAt),
		)
		if invoice.CreditedAt != nil {
			fmt.Fprintf(b, "  credited %s\n", formatTime(*invoice.CreditedAt))
		}
		if invoice.VoidedAt != nil {
			fmt.Fprintf(b, "  voided %s\n", formatTime(*invoice.VoidedAt))
		}
	} else {
		fmt.Fprintf(b, "invoice %s (no such invoice)\n", plan.IntentId)
	}

	if payment := plan.Payment; payment != nil {
		fmt.Fprintf(b, "payment %s: %s, recorded %s\n", payment.Signature, usdc(payment.AmountAtomic), payment.Reason)
		if payment.TransactionTime != nil {
			fmt.Fprintf(b, "  sent %s (on-chain time)\n", formatTime(*payment.TransactionTime))
		} else {
			fmt.Fprintf(b, "  sent by %s (no on-chain time; recorded then)\n", formatTime(payment.RecordTime))
		}
	} else {
		fmt.Fprintf(b, "payment %s (no recorded unfulfilled payment)\n", plan.Signature)
	}

	if plan.State != controller.CryptoUsdcCreditStateRefused && plan.Payment != nil {
		if 0 < plan.OverQuoteAtomic {
			fmt.Fprintf(b, "  %s over the quote\n", usdc(plan.OverQuoteAtomic))
		} else {
			fmt.Fprintf(b, "  exactly the quote\n")
		}
		if plan.SentBeforeInvoice {
			fmt.Fprintf(b, "  note: sent before the invoice was made\n")
		}
		if plan.Late {
			fmt.Fprintf(b, "credit %s: late, sent after the quote expired: what arrived, capped at the price\n", usd(plan.UsdNanoCents))
		} else {
			fmt.Fprintf(b, "credit %s: on time: the price\n", usd(plan.UsdNanoCents))
		}
		fmt.Fprintf(b, "  the unfulfilled record is resolved (removed) in the credit's transaction\n")
	}

	fmt.Fprintf(b, "state %s\n", plan.State)
	for _, refusal := range plan.Refusals {
		fmt.Fprintf(b, "  refused: %s\n", refusal)
	}
	if plan.Digest != "" {
		fmt.Fprintf(b, "digest %s\n", plan.Digest)
	}
	if plan.State == controller.CryptoUsdcCreditStateCredited {
		fmt.Fprintf(b, "credited: invoice %s is paid by %s; its unfulfilled record is resolved\n", plan.IntentId, plan.Signature)
	}
	return b.String()
}
