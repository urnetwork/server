package main

// `bringyourctl crypto`: the operator tools of the crypto payment rail
// (controller/crypto_pay_operator.go). An invoice is named by its id; amounts
// print in coin units. Nothing here prints a status token or an email; the
// receive addresses an operator needs to trace a payment are printed to the
// terminal only.

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// How long a command that credited payments waits for their sales channel
// notices (model.WaitSalesPaymentNotices) before it exits.
const salesPaymentNoticeWait = 2 * time.Minute

// cryptoCommand runs one crypto subcommand and returns the exit code.
func cryptoCommand(opts docopt.Opts, out io.Writer) int {
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()

	intentId := func() (server.Id, bool) {
		text, _ := opts.String("--intent")
		intentId, err := server.ParseId(strings.TrimSpace(text))
		if err != nil {
			fmt.Fprintf(out, "invalid --intent: %s\n", err)
			return server.Id{}, false
		}
		return intentId, true
	}

	if list, _ := opts.Bool("list"); list {
		asset, _ := opts.String("--asset")
		state, _ := opts.String("--state")
		limit := 100
		if limitText, _ := opts.String("--limit"); limitText != "" {
			parsed, err := strconv.Atoi(limitText)
			if err != nil || parsed <= 0 {
				fmt.Fprintf(out, "invalid --limit\n")
				return 1
			}
			limit = parsed
		}
		invoices := controller.CryptoOperatorListInvoices(ctx, strings.TrimSpace(asset), strings.TrimSpace(state), limit)
		for _, invoice := range invoices {
			printCryptoInvoiceLine(out, invoice)
		}
		if state == "" || state == controller.CryptoPaymentStatusReview {
			for _, receipt := range controller.CryptoOperatorUnmatchedReceipts(ctx, strings.TrimSpace(asset), limit) {
				fmt.Fprintf(
					out,
					"unmatched receipt %s %s/%s %s (%s)\n",
					receipt.Asset, receipt.TxId, receipt.OutputKey,
					cryptoFormatAmount(receipt.Asset, receipt.AmountAtomic),
					receipt.ReviewReason,
				)
			}
		}
		return 0
	}

	if show, _ := opts.Bool("show"); show {
		id, ok := intentId()
		if !ok {
			return 1
		}
		invoice := controller.CryptoOperatorShowInvoice(ctx, id)
		if invoice == nil {
			fmt.Fprintf(out, "no such invoice\n")
			return 1
		}
		intent := invoice.Intent
		printCryptoInvoiceLine(out, invoice)
		fmt.Fprintf(out, "  delivery %s, network %v, tier %q, offer %t\n", intent.Delivery, intent.NetworkId, intent.Tier, intent.OfferApplied)
		fmt.Fprintf(out, "  price $%.2f (regular $%.2f), rate $%g\n",
			model.NanoCentsToUsd(intent.PriceUsdNanoCents), model.NanoCentsToUsd(intent.RegularPriceUsdNanoCents), intent.RateUsd)
		fmt.Fprintf(out, "  address %s (index %v)\n", intent.Address, cryptoOptionalInt(intent.AddressIndex))
		fmt.Fprintf(out, "  quoted %s, expires %s", intent.QuotedAt.Format("2006-01-02 15:04:05"), intent.ExpiresAt.Format("2006-01-02 15:04:05"))
		if grace := controller.CryptoAssetConfirmationGrace(intent.Asset); 0 < grace {
			// Bitcoin: a payment sent in time and mined by then is on time
			fmt.Fprintf(out, ", on time if mined by %s (a %s confirmation grace)", intent.ExpiresAt.Add(grace).Format("2006-01-02 15:04:05"), grace)
		}
		fmt.Fprintf(out, "\n")
		if intent.CreditedAt != nil {
			fmt.Fprintf(out, "  credited %s", intent.CreditedAt.Format("2006-01-02 15:04:05"))
			if intent.CreditedUsdNanoCents != nil {
				fmt.Fprintf(out, " at $%.2f", model.NanoCentsToUsd(*intent.CreditedUsdNanoCents))
			}
			fmt.Fprintf(out, "\n")
		}
		if intent.VoidedAt != nil {
			fmt.Fprintf(out, "  voided %s\n", intent.VoidedAt.Format("2006-01-02 15:04:05"))
		}
		if intent.RefundedAt != nil {
			fmt.Fprintf(out, "  refunded %s\n", intent.RefundedAt.Format("2006-01-02 15:04:05"))
		}
		if 0 < intent.CreditFailureCount {
			fmt.Fprintf(out, "  %d failed credits in a row\n", intent.CreditFailureCount)
		}
		if intent.UnlinkedAt != nil {
			fmt.Fprintf(out, "  unlinked from its network %s\n", intent.UnlinkedAt.Format("2006-01-02 15:04:05"))
		}
		for _, receipt := range invoice.Receipts {
			height := "mempool"
			if receipt.BlockHeight != nil {
				height = fmt.Sprintf("height %d", *receipt.BlockHeight)
			}
			fmt.Fprintf(
				out,
				"  receipt %s/%s %s, %s, %d confirmations, %s %s, first seen %s\n",
				receipt.TxId, receipt.OutputKey,
				cryptoFormatAmount(receipt.Asset, receipt.AmountAtomic),
				height, receipt.Confirmations,
				receipt.State, receipt.ReviewReason,
				receipt.FirstSeenAt.Format("2006-01-02 15:04:05"),
			)
		}
		return 0
	}

	if credit, _ := opts.Bool("credit"); credit {
		id, ok := intentId()
		if !ok {
			return 1
		}
		sanctionsCleared, _ := opts.Bool("--sanctions-cleared")
		credited, err := controller.CryptoOperatorCredit(clientSession, id, sanctionsCleared)
		if err != nil {
			fmt.Fprintf(out, "could not credit: %s\n", err)
			return 1
		}
		if !credited {
			fmt.Fprintf(out, "not credited: the invoice was credited or voided concurrently\n")
			return 1
		}
		fmt.Fprintf(out, "credited invoice %s\n", id)
		// the sales channel notice of the payment posts from this process
		if !model.WaitSalesPaymentNotices(salesPaymentNoticeWait) {
			fmt.Fprintf(out, "the sales channel notice was not posted before exit\n")
		}
		return 0
	}

	if void, _ := opts.Bool("void"); void {
		id, ok := intentId()
		if !ok {
			return 1
		}
		voided, err := controller.CryptoOperatorVoid(ctx, id)
		if err != nil {
			fmt.Fprintf(out, "could not void: %s\n", err)
			return 1
		}
		if !voided {
			fmt.Fprintf(out, "not voided: the invoice is credited or already void\n")
			return 1
		}
		fmt.Fprintf(out, "voided invoice %s; a payment that still arrives is held for review\n", id)
		return 0
	}

	if refund, _ := opts.Bool("refund"); refund {
		id, ok := intentId()
		if !ok {
			return 1
		}
		address, _ := opts.String("--address")
		result, err := controller.CryptoOperatorRefund(clientSession, id, address)
		if err != nil {
			fmt.Fprintf(out, "could not refund: %s\n", err)
			return 1
		}
		if gift := result.Gift; gift != nil {
			// a gift order: only its unredeemed codes are refunded
			fmt.Fprintf(
				out,
				"refunded %d of the %d gift codes of invoice %s; %d redeemed and %d already void are not refundable\n",
				gift.Refunded, gift.Quantity, id, gift.Redeemed, gift.AlreadyVoid,
			)
			ticker := strings.ToUpper(cryptoTicker(result.Intent.Asset))
			fmt.Fprintf(
				out,
				"now send the refund by hand from the cold wallet: %s %s, the refunded codes' share of the %s %s received (%.2f USD at the invoice's credited value), to the address given (recorded with the refund event)\n",
				gift.Refund, ticker, result.Received, ticker, gift.RefundUsd,
			)
			return 0
		}
		fmt.Fprintf(out, "ended the grant of invoice %s for %d network(s)\n", id, len(result.EndedNetworkIds))
		fmt.Fprintf(
			out,
			"now send the refund by hand from the cold wallet: up to %s %s received, to the address given (recorded with the refund event)\n",
			result.Received, strings.ToUpper(cryptoTicker(result.Intent.Asset)),
		)
		return 0
	}

	if alchemyWebhook, _ := opts.Bool("alchemy-webhook"); alchemyWebhook {
		signingKeyPath, _ := opts.String("--signing-key-file")
		result, err := controller.CryptoAlchemyEnsureEthWebhook(ctx, strings.TrimSpace(signingKeyPath))
		if err != nil {
			fmt.Fprintf(out, "could not set up the Alchemy webhook: %s\n", err)
			return 1
		}
		if result.Created {
			fmt.Fprintf(out, "created the ETH Address Activity webhook %s for %s\n", result.WebhookId, result.WebhookUrl)
		} else {
			fmt.Fprintf(out, "found the ETH Address Activity webhook %s for %s\n", result.WebhookId, result.WebhookUrl)
		}
		if result.SigningKeyWritten {
			fmt.Fprintf(out, "its signing key is in %s (mode 0600); it was not printed\n", signingKeyPath)
		} else {
			fmt.Fprintf(out, "Alchemy did not return its signing key: copy it from the webhook's page in the Alchemy dashboard\n")
		}
		fmt.Fprintf(out, "store the id as alchemy.eth_webhook_id and the signing key as alchemy.eth_webhook_signing_key in vault/<env>/alchemy.yml\n")
		return 0
	}

	fmt.Fprintf(out, "unknown crypto command\n")
	return 1
}

func printCryptoInvoiceLine(out io.Writer, invoice *controller.CryptoOperatorInvoice) {
	intent := invoice.Intent
	status := invoice.Status
	if invoice.ReviewReason != "" {
		status += "/" + invoice.ReviewReason
	}
	fmt.Fprintf(
		out,
		"%s %s %s %s %s, created %s\n",
		intent.IntentId,
		intent.Asset,
		intent.Item,
		cryptoFormatAmount(intent.Asset, intent.AmountAtomic),
		status,
		intent.CreateTime.Format("2006-01-02 15:04:05"),
	)
}

func cryptoTicker(asset string) string {
	switch asset {
	case model.CryptoAssetXmr:
		return "xmr"
	case model.CryptoAssetZec:
		return "zec"
	case model.CryptoAssetUsdcSolana:
		return "usdc"
	}
	// tao, btc, eth and sol are their own tickers
	return asset
}

// cryptoFormatAmount prints an atomic amount in coin units with its ticker.
func cryptoFormatAmount(asset string, amountAtomic int64) string {
	decimals := controller.CryptoAssetDecimals(asset)
	text := strconv.FormatInt(amountAtomic, 10)
	if decimals == 0 {
		return text + " " + cryptoTicker(asset)
	}
	for len(text) <= decimals {
		text = "0" + text
	}
	whole, fraction := text[:len(text)-decimals], strings.TrimRight(text[len(text)-decimals:], "0")
	if fraction != "" {
		whole += "." + fraction
	}
	return whole + " " + cryptoTicker(asset)
}

func cryptoOptionalInt(value *int64) string {
	if value == nil {
		return "none"
	}
	return strconv.FormatInt(*value, 10)
}
