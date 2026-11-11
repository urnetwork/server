package controller

// Sanctions screening of the crypto payment rail (mmm/BTCPAY.md §17.3,
// §17.5): before a TAO, Bitcoin, ETH or SOL payment is credited, every sender
// the chain names (a Bitcoin payment's inputs, an ETH transfer's sender and
// its transaction's origin, a SOL transfer's source, a TAO transfer's origin)
// is checked against OFAC's sanctioned addresses, and every refund
// destination is checked before a refund is recorded, in every coin.
//
//   - a match: the payment is held for review (sanctions_hold), for the owner
//     and counsel; the scanner never credits it;
//   - a sender that cannot be screened (no list loaded, an address the list
//     cannot read, an input that names no address): held as well
//     (screening_unavailable), never credited unscreened. The scanner screens
//     it again each scan and lets it through once the screen is clean;
//   - senders that cannot be read right now (the chain source is down): the
//     credit waits for the next scan.
//
// The list is the newest config/all/crypto/<release>/ table, which cryptoctl
// refreshes on every release build; the cryptosanctions package reads it. The
// buyer sees only "screening" for either hold. Logs name the invoice and the
// list version, never an address.

import (
	"context"
	"errors"
	"slices"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The review reason a buyer is shown for a payment held by the screen,
// whichever the hold: what the screen found is for the operator.
const CryptoReviewReasonScreening = "screening"

// The list could not be read: no table in this build or deploy.
var errCryptoSanctionsListUnavailable = errors.New("the sanctions list is not loaded")

// The receipts' state write of a hold or a release. Replaceable only by tests.
var cryptoSetReceiptsState = model.SetCryptoPaymentReceiptsStateInConn

// cryptoSanctionsMatch looks one address of a coin up in OFAC's list: matched,
// the list's version, or why the address could not be screened. Replaceable
// only by tests.
var cryptoSanctionsMatch = func(asset string, address string) (matched bool, listVersion string, err error) {
	return false, "", errCryptoSanctionsListUnavailable
}

type cryptoScreenOutcome int

const (
	// screened clean: the credit goes on
	cryptoScreenClean cryptoScreenOutcome = iota
	// held for review: a match, or a sender that cannot be screened
	cryptoScreenHeld
	// the senders could not be read now: the credit waits for the next scan
	cryptoScreenRetry
)

// cryptoScreenSenders screens a payment's senders: a match holds it as
// sanctions_hold; otherwise a sender that cannot be screened (or no sender at
// all) holds it as screening_unavailable.
func cryptoScreenSenders(asset string, senders []string) (outcome cryptoScreenOutcome, reason string, listVersion string) {
	if len(senders) == 0 {
		return cryptoScreenHeld, model.CryptoReviewReasonScreeningUnavailable, ""
	}
	unscreenable := false
	for _, sender := range senders {
		if sender == "" {
			unscreenable = true
			continue
		}
		matched, version, err := cryptoSanctionsMatch(asset, sender)
		if version != "" {
			listVersion = version
		}
		if err != nil {
			unscreenable = true
			continue
		}
		if matched {
			return cryptoScreenHeld, model.CryptoReviewReasonSanctionsHold, listVersion
		}
	}
	if unscreenable {
		return cryptoScreenHeld, model.CryptoReviewReasonScreeningUnavailable, listVersion
	}
	return cryptoScreenClean, "", listVersion
}

// cryptoReceiptSenders reads the senders of the given receipts' transactions
// again from the chain source (Transaction): complete, where a scan's view may
// not be (an ETH contract transfer's origin). ok = false when a transaction
// could not be read now.
func cryptoReceiptSenders(ctx context.Context, wallet cryptoWallet, keys []model.CryptoPaymentReceiptKey) (senders []string, ok bool) {
	senders = []string{}
	byTx := map[string][]model.CryptoPaymentReceiptKey{}
	txIds := []string{}
	for _, key := range keys {
		if _, seen := byTx[key.TxId]; !seen {
			txIds = append(txIds, key.TxId)
		}
		byTx[key.TxId] = append(byTx[key.TxId], key)
	}
	for _, txId := range txIds {
		walletReceipts, found, err := wallet.Transaction(ctx, txId)
		if err != nil || !found {
			return nil, false
		}
		matched := false
		for _, walletReceipt := range walletReceipts {
			for _, key := range byTx[txId] {
				if walletReceipt.OutputKey == key.OutputKey {
					matched = true
					for _, sender := range walletReceipt.Senders {
						if !slices.Contains(senders, sender) {
							senders = append(senders, sender)
						}
					}
				}
			}
		}
		if !matched {
			// the transaction no longer pays the receipt: the scan sorts it out
			return nil, false
		}
	}
	return senders, true
}

// cryptoScreenCredit screens the receipts an invoice is about to be credited
// with. A hold moves them to review with its reason. Clean and retry write
// nothing.
func cryptoScreenCredit(
	ctx context.Context,
	asset string,
	intentId server.Id,
	wallet cryptoWallet,
	keys []model.CryptoPaymentReceiptKey,
) cryptoScreenOutcome {
	senders, ok := cryptoReceiptSenders(ctx, wallet, keys)
	if !ok {
		cryptoPayScreenings.WithLabelValues(asset, "retry").Inc()
		if glog.V(1) {
			glog.Infof("[cryptopay]%s invoice %s: the payment's senders could not be read now; the credit waits\n", asset, intentId)
		}
		return cryptoScreenRetry
	}
	outcome, reason, listVersion := cryptoScreenSenders(asset, senders)
	switch outcome {
	case cryptoScreenHeld:
		cryptoSetReceiptsState(nil, ctx, &intentId, keys, model.CryptoReceiptStateReview, reason)
		if reason == model.CryptoReviewReasonSanctionsHold {
			cryptoPayScreenings.WithLabelValues(asset, "matched").Inc()
			glog.Errorf("[cryptopay]%s invoice %s held for review: a sender is on the sanctions list (list %s); the owner and counsel decide\n", asset, intentId, listVersion)
		} else {
			cryptoPayScreenings.WithLabelValues(asset, "unscreenable").Inc()
			cryptoScanFailureReports.report("screen/"+asset, func() {
				glog.Warningf("[cryptopay]%s invoice %s held for review: its senders could not be screened (list %q); it is screened again each scan\n", asset, intentId, listVersion)
			})
		}
	case cryptoScreenClean:
		cryptoPayScreenings.WithLabelValues(asset, "clean").Inc()
		glog.Infof("[cryptopay]%s invoice %s: senders screened clean (list %s)\n", asset, intentId, listVersion)
	}
	return outcome
}

// The most held invoices one scan screens again.
const cryptoRescreenMaxPerScan = 20

// cryptoRescreenHeld screens again the coin's payments held because their
// senders could not be screened (screening_unavailable): clean ones go back
// to confirmed (or seen, in the mempool) for the invoice's judgment to credit,
// a match becomes sanctions_hold, and the rest stay held. Returns the
// invoices whose receipts moved.
func cryptoRescreenHeld(
	ctx context.Context,
	asset string,
	wallet cryptoWallet,
	open []*model.CryptoPaymentReceipt,
	result *CryptoPaymentScanResult,
) []server.Id {
	held := map[server.Id][]*model.CryptoPaymentReceipt{}
	intentIds := []server.Id{}
	for _, receipt := range open {
		if receipt.IntentId == nil || receipt.State != model.CryptoReceiptStateReview ||
			receipt.ReviewReason != model.CryptoReviewReasonScreeningUnavailable {
			continue
		}
		if _, seen := held[*receipt.IntentId]; !seen {
			if cryptoRescreenMaxPerScan <= len(intentIds) {
				continue
			}
			intentIds = append(intentIds, *receipt.IntentId)
		}
		held[*receipt.IntentId] = append(held[*receipt.IntentId], receipt)
	}
	moved := []server.Id{}
	for _, intentId := range intentIds {
		receipts := held[intentId]
		keys := []model.CryptoPaymentReceiptKey{}
		for _, receipt := range receipts {
			keys = append(keys, receipt.Key())
		}
		senders, ok := cryptoReceiptSenders(ctx, wallet, keys)
		if !ok {
			continue
		}
		outcome, reason, listVersion := cryptoScreenSenders(asset, senders)
		switch {
		case outcome == cryptoScreenClean:
			for _, receipt := range receipts {
				state := model.CryptoReceiptStateConfirmed
				if receipt.BlockHeight == nil {
					state = model.CryptoReceiptStateSeen
				}
				cryptoSetReceiptsState(nil, ctx, &intentId, []model.CryptoPaymentReceiptKey{receipt.Key()}, state, "")
			}
			cryptoPayScreenings.WithLabelValues(asset, "released").Inc()
			result.ScreeningReleased += 1
			glog.Infof("[cryptopay]%s invoice %s: its held payment screened clean (list %s); it is judged again\n", asset, intentId, listVersion)
			moved = append(moved, intentId)
		case reason == model.CryptoReviewReasonSanctionsHold:
			cryptoSetReceiptsState(nil, ctx, &intentId, keys, model.CryptoReceiptStateReview, reason)
			cryptoPayScreenings.WithLabelValues(asset, "matched").Inc()
			glog.Errorf("[cryptopay]%s invoice %s held for review: a sender is on the sanctions list (list %s); the owner and counsel decide\n", asset, intentId, listVersion)
			moved = append(moved, intentId)
		}
	}
	return moved
}

// cryptoScreenRefundDestination screens an address a refund is to be sent to,
// in any coin: nil when it is clear, else why the refund must not be recorded
// (a match, or an address that cannot be screened).
func cryptoScreenRefundDestination(asset string, address string) error {
	matched, listVersion, err := cryptoSanctionsMatch(asset, address)
	if err != nil {
		return errors.New("the refund address could not be screened against the sanctions list (" + err.Error() + "); nothing was recorded")
	}
	if matched {
		glog.Errorf("[cryptopay]a %s refund destination is on the sanctions list (list %s); refused\n", asset, listVersion)
		return errors.New("the refund address is on the sanctions list (list " + listVersion + "): hold this refund for the owner and counsel; nothing was recorded")
	}
	return nil
}
