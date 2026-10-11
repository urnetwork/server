package controller

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Crediting a USDC payment on Solana that arrived WITHOUT its reference.
//
// Some wallets and exchanges cannot add a memo and do not open Solana Pay
// links, so the transfer carries nothing but the amount. Every quote is a
// unique amount (the price plus a reserved sub-cent suffix, see
// model.CreateSolanaPaymentIntentWithUniqueAmount), and the webhook falls back
// to that amount only after the reference search found no open intent and the
// signature was not already credited. Reference matching runs first and is
// unchanged.
//
// The match is credited only when it is unambiguous:
//   - the transaction has no memo (a memo that matched nothing is a conflicting
//     signal, not an absent one)
//   - exactly one USDC transfer to one of our receiving addresses, and the
//     transaction carries its on-chain time
//   - exactly one intent, open or consumed, was quoted that exact micro-USDC
//     amount within the reservation hold of the payment time
//   - that intent is open, its stored quote is the same amount, and the payment
//     time is inside its window [created_at - skew, expires_at]
//
// Anything else is recorded as unfulfilled (no_intent) with the sender and a
// note naming the candidates, so support can credit it by hand. Crediting goes
// through the same intent one-shot as a reference match, so a redelivery or a
// race never credits twice.

// Block times are estimates; a payment a little "before" its quote is clock
// drift. A real payment cannot precede the quote by more than this.
const solanaMemolessClockSkew = 2 * time.Minute

type solanaMemolessTransfer struct {
	amountMicro     int64
	tokenAmountUsd  float64
	senderAccount   string
	transactionTime time.Time
}

// solanaReceivedUsdcTransfers lists the USDC transfers to our receiving
// addresses, the same filter the webhook applies before anything else.
func solanaReceivedUsdcTransfers(transaction *SolanaTransaction) []TokenTransfer {
	transfers := []TokenTransfer{}
	for _, tokenTransfer := range transaction.TokenTransfers {
		if tokenTransfer.Mint == solanaUsdcMint &&
			slices.Contains(solanaReceiverAddresses, tokenTransfer.ToUserAccount) &&
			0 < tokenTransfer.TokenAmount {
			transfers = append(transfers, tokenTransfer)
		}
	}
	return transfers
}

// solanaTransactionHasMemo reports whether any top-level or inner instruction is
// a memo with content.
func solanaTransactionHasMemo(transaction *SolanaTransaction) bool {
	for _, instruction := range transaction.Instructions {
		if 0 < len(solanaMemoTexts(instruction.ProgramId, instruction.Data)) {
			return true
		}
		for _, inner := range instruction.InnerInstructions {
			if 0 < len(solanaMemoTexts(inner.ProgramId, inner.Data)) {
				return true
			}
		}
	}
	return false
}

// solanaMemolessTransferOf returns the one transfer a memo-less payment must
// be, or nil and the reason the transaction cannot be matched by amount.
func solanaMemolessTransferOf(transaction *SolanaTransaction) (*solanaMemolessTransfer, string) {
	if solanaTransactionHasMemo(transaction) {
		return nil, "memo-less: not matched by amount: the transaction carries a memo that matched no open intent"
	}
	transfers := solanaReceivedUsdcTransfers(transaction)
	if len(transfers) != 1 {
		return nil, fmt.Sprintf("memo-less: not matched by amount: %d USDC transfers to our addresses", len(transfers))
	}
	if transaction.Timestamp == 0 {
		return nil, "memo-less: not matched by amount: the transaction has no on-chain time"
	}
	transfer := transfers[0]
	return &solanaMemolessTransfer{
		amountMicro:     model.SolanaUsdToMicro(transfer.TokenAmount),
		tokenAmountUsd:  transfer.TokenAmount,
		senderAccount:   transfer.FromUserAccount,
		transactionTime: time.Unix(transaction.Timestamp, 0).UTC(),
	}, ""
}

// solanaMatchMemolessIntent picks the intent a memo-less transfer pays, from
// every intent quoted the same amount within the reservation hold. nil and a
// note for support unless the match is unambiguous (see the file header).
func solanaMatchMemolessIntent(
	transfer *solanaMemolessTransfer,
	candidates []*model.SolanaPaymentIntent,
) (*model.SolanaPaymentIntent, string) {
	amount := solanaFormatMicro(transfer.amountMicro)
	switch len(candidates) {
	case 0:
		return nil, fmt.Sprintf("memo-less: no intent was quoted %s USDC", amount)
	case 1:
	default:
		references := []string{}
		for _, candidate := range candidates {
			references = append(references, candidate.PaymentReference)
		}
		return nil, fmt.Sprintf(
			"memo-less: ambiguous: %d intents were quoted %s USDC: %s",
			len(candidates), amount, strings.Join(references, ", "),
		)
	}

	intent := candidates[0]
	describe := fmt.Sprintf(
		"intent %s (network %s, plan %s, quoted %s USDC)",
		intent.PaymentReference, intent.NetworkId, intent.SubscriptionPlan, amount,
	)
	if intent.TxSignature != nil {
		return nil, fmt.Sprintf("memo-less: %s was already paid by %s", describe, *intent.TxSignature)
	}
	if model.SolanaUsdToMicro(intent.ExpectedAmountUsd) != transfer.amountMicro {
		return nil, fmt.Sprintf(
			"memo-less: %s has a stored quote of %v USDC, not the amount received",
			describe, intent.ExpectedAmountUsd,
		)
	}
	if intent.NetworkId == (server.Id{}) {
		return nil, fmt.Sprintf("memo-less: %s has no network", describe)
	}
	if intent.ExpiresAt == nil {
		return nil, fmt.Sprintf("memo-less: %s has no expiry", describe)
	}
	if transfer.transactionTime.Before(intent.CreatedAt.Add(-solanaMemolessClockSkew)) {
		return nil, fmt.Sprintf(
			"memo-less: %s: sent at %s, before the quote was made at %s",
			describe, transfer.transactionTime.Format(time.RFC3339), intent.CreatedAt.Format(time.RFC3339),
		)
	}
	if intent.ExpiresAt.Before(transfer.transactionTime) {
		return nil, fmt.Sprintf(
			"memo-less: %s: sent at %s, after the quote expired at %s",
			describe, transfer.transactionTime.Format(time.RFC3339), intent.ExpiresAt.Format(time.RFC3339),
		)
	}
	return intent, ""
}

// solanaFormatMicro prints a micro-USDC amount with all 6 decimals.
func solanaFormatMicro(amountMicro int64) string {
	return fmt.Sprintf("%d.%06d", amountMicro/model.SolanaUsdcMicroPerUsd, amountMicro%model.SolanaUsdcMicroPerUsd)
}

// solanaDecideMemolessPayment is the webhook's fallback for a payment whose
// reference matched no open intent and whose signature was not credited:
// credit the unambiguous amount match, or record the payment with what support
// needs. unfulfilled is the record solanaDecidePayment would otherwise write.
func solanaDecideMemolessPayment(
	transaction *SolanaTransaction,
	lookups solanaPaymentLookups,
	unfulfilled *model.UnfulfilledSolanaPayment,
) *solanaPaymentDecision {
	record := func(note string) *solanaPaymentDecision {
		if transfers := solanaReceivedUsdcTransfers(transaction); 0 < len(transfers) {
			senderAccount := transfers[0].FromUserAccount
			unfulfilled.SenderAccount = &senderAccount
		}
		unfulfilled.MatchNote = &note
		return &solanaPaymentDecision{
			action:      solanaPaymentActionRecord,
			skipMessage: "No payment intent found for this network ID",
			unfulfilled: unfulfilled,
		}
	}

	transfer, note := solanaMemolessTransferOf(transaction)
	if transfer == nil {
		return record(note)
	}

	candidates, err := lookups.intentsByAmountMicro(
		transfer.amountMicro,
		transfer.transactionTime.Add(-model.SolanaUniqueAmountHold),
	)
	if err != nil {
		return &solanaPaymentDecision{
			action: solanaPaymentActionError,
			err:    err,
		}
	}

	intent, note := solanaMatchMemolessIntent(transfer, candidates)
	if intent == nil {
		return record(note)
	}

	networkId := intent.NetworkId
	return &solanaPaymentDecision{
		action: solanaPaymentActionCredit,
		intent: &model.PaymentIntentSearchResult{
			NetworkId:         &networkId,
			PaymentReference:  intent.PaymentReference,
			ExpectedAmountUsd: intent.ExpectedAmountUsd,
			SubscriptionPlan:  intent.SubscriptionPlan,
		},
		tokenAmountUsd: transfer.tokenAmountUsd,
	}
}
