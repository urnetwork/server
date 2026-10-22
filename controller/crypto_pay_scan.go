package controller

// The scanner, the address refill and the retention unlink of the crypto
// payment rail (mmm/ZCASH-CONTRACT.md §3.8, mmm/ZCASH.md §4.9, §4.10).
// The taskworker runs them (taskworker/work/crypto_pay_work.go): one scan
// chain and one refill chain per scanned coin, each a RunOnce key, so two
// scans of one coin never overlap, and a daily unlink.
//
// A scan reads the wallet daemon's incoming transfers since its cursor less a
// reorg window, records each as a receipt (idempotent on the transaction and
// the receiving address), matches it to its invoice by the address's index,
// judges every invoice with an open receipt (cryptoEvaluateIntent: paid,
// short, late, extra, voided), credits through the one credit at the coin's
// credit depth, stamps receipts final at the final depth, and records the
// heights and its success, which the quote path reads to stop selling a coin
// whose payments would not be seen in time.
//
// A receipt is never counted when it is time-locked or a transparent Zcash
// output (decision 4): it is recorded as ignored, with the reason, and
// counted in a metric. A mempool transaction the wallet flags as a double
// spend is recorded as seen and counts only once it is mined.
//
// The scanner runs whenever the coin's wallet daemon is configured, whether
// or not the coin is sold (the kill switch gates quoting only, mmm/ZCASH-CONTRACT.md
// §3.13.1), and it takes a daemon's "not found" as proof that a payment left
// the chain only from a wallet that has caught up (§3.13.2).

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The most stored receipts one scan re-checks with the daemon after they
// stopped being reported.
const cryptoScanMaxVanishedChecks = 50

// The most open receipts one scan judges.
const cryptoScanMaxOpenReceipts = 5000

// The most addresses one refill run derives.
const cryptoRefillMaxPerRun = 200

// The pool label a derived address carries in the daemon (never user data).
const cryptoPoolAddressLabel = "pool"

// CryptoPaymentScanResult is what one scan did.
type CryptoPaymentScanResult struct {
	Asset string `json:"asset"`
	// false: the coin's wallet daemon is not configured (or its vault file
	// is unreadable), and the chain backs off before it tries again
	Configured    bool   `json:"configured"`
	Error         string `json:"error,omitempty"`
	WalletHeight  uint64 `json:"wallet_height"`
	Receipts      int    `json:"receipts"`
	NewReceipts   int    `json:"new_receipts"`
	Credited      int    `json:"credited"`
	Reviewed      int    `json:"reviewed"`
	Orphaned      int    `json:"orphaned"`
	MarkedFinal   int64  `json:"marked_final"`
	ScanInterval  int64  `json:"scan_interval_seconds"`
	OpenInvoices  int    `json:"open_invoices"`
	MinHeight     uint64 `json:"min_height"`
	CreditErrored int    `json:"credit_errored"`
	// vanished receipts left alone because the wallet has not caught up
	OrphanChecksSkipped int `json:"orphan_checks_skipped"`
}

// One default-level report a minute per coin of a failing scan or refill.
var cryptoScanFailureReports = newCryptoPayKeyedReport(time.Minute)

// RunCryptoPaymentScan is one scan of a coin. A failure of the daemon is
// recorded on the cursor (the quote path then stops selling the coin once the
// last success is three intervals old) and in the result, not returned: the
// chain keeps its cadence. A coin whose daemon is not configured is not
// scanned (Configured = false), and its chain backs off.
func RunCryptoPaymentScan(clientSession *session.ClientSession, asset string) (*CryptoPaymentScanResult, error) {
	config := cryptoPayConfigFunc()
	result := &CryptoPaymentScanResult{
		Asset:        asset,
		ScanInterval: int64(config.ScanInterval / time.Second),
	}
	if err := cryptoAssetScannable(config, asset); err != nil {
		if glog.V(1) {
			glog.Infof("[cryptopay]%s is not scanned: %s\n", asset, err)
		}
		return result, nil
	}
	result.Configured = true
	wallet, _, err := cryptoWalletForAsset(config, asset)
	if err == nil {
		err = cryptoScan(clientSession, config, asset, wallet, result)
	}
	ctx := clientSession.Ctx
	if err != nil {
		result.Error = err.Error()
		cryptoPayScans.WithLabelValues(asset, "error").Inc()
		// the error names the method and the status, never an address or a
		// credential (crypto_pay_wallet.go)
		model.SetCryptoScanCursorErrorInConn(nil, ctx, asset, truncateCryptoError(err.Error()), server.NowUtc())
		cryptoScanFailureReports.report("scan/"+asset, func() {
			glog.Warningf("[cryptopay]%s scan failed: %s\n", asset, err)
		})
		return result, nil
	}
	cryptoPayScans.WithLabelValues(asset, "ok").Inc()
	return result, nil
}

func truncateCryptoError(text string) string {
	if 512 < len(text) {
		return text[:512]
	}
	return text
}

// cryptoWalletNetworkCheck is a daemon that can tell whether it serves the
// configured network and wallet.
type cryptoWalletNetworkCheck interface {
	CheckNetwork(ctx context.Context) error
}

// cryptoWalletAddressCheck is a daemon that can derive an address of a given
// index again, to prove it is the wallet the pool was derived from.
type cryptoWalletAddressCheck interface {
	CheckAddress(ctx context.Context, index uint64, address string) error
}

// cryptoCheckWallet refuses a daemon that is not the coin's wallet: on
// another network, another Monero wallet than the vault's primary address,
// or a Zcash wallet that derives another address than the pool holds at its
// lowest index (mmm/ZCASH-CONTRACT.md §3.13.13). The scanner, the refill and the
// reconcile legs call it before they trust anything the daemon says. Reuse
// the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func cryptoCheckWallet(ctx context.Context, connOwner server.PgConn, asset string, wallet cryptoWallet) error {
	if checker, ok := wallet.(cryptoWalletNetworkCheck); ok {
		if err := checker.CheckNetwork(ctx); err != nil {
			return err
		}
	}
	if checker, ok := wallet.(cryptoWalletAddressCheck); ok {
		if index, address, ok := model.GetCryptoPaymentPoolAddressAtInConn(connOwner, ctx, asset, true); ok && 0 <= index {
			if err := checker.CheckAddress(ctx, uint64(index), address); err != nil {
				return err
			}
		}
	}
	return nil
}

// cryptoScan is the scan itself, against a wallet in hand (the tests' fake).
func cryptoScan(
	clientSession *session.ClientSession,
	config *CryptoPayConfig,
	asset string,
	wallet cryptoWallet,
	result *CryptoPaymentScanResult,
) error {
	ctx := clientSession.Ctx
	assetConfig := config.Asset(asset)
	if err := cryptoCheckWallet(ctx, nil, asset, wallet); err != nil {
		return err
	}

	var cursorHeight uint64
	if cursor := model.GetCryptoScanCursor(ctx, asset); cursor != nil && 0 < cursor.Height {
		cursorHeight = uint64(cursor.Height)
	}
	reorgWindow := uint64(max(0, assetConfig.ReorgWindowBlocksAt(int64(cursorHeight))))
	var minHeight uint64
	if reorgWindow < cursorHeight {
		minHeight = cursorHeight - reorgWindow
	}
	result.MinHeight = minHeight

	walletReceipts, walletHeight, err := wallet.IncomingSince(ctx, minHeight)
	if err != nil {
		return err
	}
	heightWallet, nodeHeight, networkHeight, err := wallet.Heights(ctx)
	if err != nil {
		return err
	}
	walletHeight = max(walletHeight, heightWallet)
	result.WalletHeight = walletHeight
	result.Receipts = len(walletReceipts)
	now := server.NowUtc()

	// the invoices the reported addresses were given
	addressIndexes := []int64{}
	for _, walletReceipt := range walletReceipts {
		if walletReceipt.AddressIndex != nil && *walletReceipt.AddressIndex <= 1<<62 {
			addressIndexes = append(addressIndexes, int64(*walletReceipt.AddressIndex))
		}
	}
	intents := model.GetCryptoPaymentIntentsByAddressIndexesInConn(nil, ctx, asset, addressIndexes)

	// the USD rate a new receipt is first seen at: revenue is booked at fair
	// value on receipt, and a late payment is valued at it. Asked once a scan,
	// and only when there is a new receipt.
	var firstSeenRate *float64
	rateAsked := false
	rateNow := func() *float64 {
		if !rateAsked {
			rateAsked = true
			if quote, _ := cryptoQuoteRate(ctx, config, asset); quote != nil {
				rate := quote.RateUsd
				firstSeenRate = &rate
			}
		}
		return firstSeenRate
	}

	reported := map[model.CryptoPaymentReceiptKey]bool{}
	finalKeys := []model.CryptoPaymentReceiptKey{}
	affected := map[server.Id]bool{}
	firstSeenHeight := int64(walletHeight)
	for _, walletReceipt := range walletReceipts {
		receipt := cryptoReceiptOf(asset, walletReceipt, intents, now)
		receipt.FirstSeenHeight = &firstSeenHeight
		reported[receipt.Key()] = true
		if receipt.IntentId != nil {
			affected[*receipt.IntentId] = true
		}
		inserted, stored := model.UpsertCryptoPaymentReceiptInConn(nil, ctx, receipt)
		if inserted {
			result.NewReceipts += 1
			cryptoPayReceiptsRecorded.WithLabelValues(asset, stored.State, stored.ReviewReason).Inc()
			if stored.State == model.CryptoReceiptStateIgnored {
				glog.Infof("[cryptopay]%s receipt ignored (%s) for invoice %v\n", asset, stored.ReviewReason, stored.IntentId)
			} else if stored.ReviewReason == model.CryptoReviewReasonDoubleSpend {
				glog.Infof("[cryptopay]%s mempool receipt for invoice %v flagged as a double spend; it counts once mined\n", asset, stored.IntentId)
			} else if stored.IntentId == nil {
				glog.Infof("[cryptopay]%s receipt to an address no invoice was given; held for review\n", asset)
			}
			if rate := rateNow(); rate != nil {
				model.SetCryptoPaymentReceiptFirstSeenRateInConn(nil, ctx, stored.Key(), *rate)
			}
		}
		if 0 < walletReceipt.BlockHeight &&
			assetConfig.FinalConfirmationsAt(int64(walletReceipt.BlockHeight)) <= int64(walletReceipt.Confirmations) {
			finalKeys = append(finalKeys, receipt.Key())
		}
	}

	// receipts the daemon stopped reporting: still there (the mined copy of a
	// pool transfer, or below the window), or gone from the chain before
	// they were credited (orphaned; they come back if the chain shows them
	// again). "Not found" is proof only from a wallet that has caught up with
	// a known network height, and only for a receipt the final depth behind
	// the wallet (mined, or first seen in the mempool): a wallet that is
	// resyncing or rebuilt knows nothing yet (mmm/ZCASH-CONTRACT.md §3.13.2).
	caughtUp := cryptoWalletCaughtUp(config, asset, int64(walletHeight), int64(networkHeight))
	open := model.ListCryptoPaymentReceiptsByStateInConn(nil, ctx, asset, []string{
		model.CryptoReceiptStateSeen,
		model.CryptoReceiptStateConfirmed,
		model.CryptoReceiptStateReview,
	}, cryptoScanMaxOpenReceipts)
	checked := 0
	for _, receipt := range open {
		if receipt.State == model.CryptoReceiptStateReview || reported[receipt.Key()] {
			continue
		}
		if receipt.BlockHeight != nil && *receipt.BlockHeight < int64(minHeight) {
			continue
		}
		anchor := receipt.BlockHeight
		if anchor == nil {
			anchor = receipt.FirstSeenHeight
		}
		if !caughtUp || anchor == nil || int64(walletHeight) < *anchor+assetConfig.FinalConfirmationsAt(*anchor) {
			result.OrphanChecksSkipped += 1
			continue
		}
		if cryptoScanMaxVanishedChecks <= checked {
			break
		}
		checked += 1
		transactionReceipts, found, err := wallet.Transaction(ctx, receipt.TxId)
		if err != nil {
			return err
		}
		present := false
		if found {
			for _, transactionReceipt := range transactionReceipts {
				if transactionReceipt.OutputKey == receipt.OutputKey && transactionReceipt.AmountAtomic == uint64(receipt.AmountAtomic) {
					present = true
					again := cryptoReceiptOf(asset, transactionReceipt, intents, now)
					again.IntentId = receipt.IntentId
					if again.State == model.CryptoReceiptStateReview && again.ReviewReason == model.CryptoReviewReasonUnknownAddress {
						// matched by the stored row, not by this scan's index map
						again.State = model.CryptoReceiptStateSeen
						again.ReviewReason = ""
					}
					model.UpsertCryptoPaymentReceiptInConn(nil, ctx, again)
				}
			}
		}
		if !present {
			model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, receipt.IntentId, []model.CryptoPaymentReceiptKey{receipt.Key()}, model.CryptoReceiptStateOrphaned, "")
			result.Orphaned += 1
			glog.Infof("[cryptopay]%s receipt for invoice %v left the chain before it was credited\n", asset, receipt.IntentId)
		}
		if receipt.IntentId != nil {
			affected[*receipt.IntentId] = true
		}
	}

	// every invoice with a receipt still in play is judged again: time alone
	// moves an invoice from short to review at its expiry, and a top-up can
	// make an underpaid one whole. A receipt in review for any other reason
	// waits for an operator.
	for _, receipt := range open {
		if receipt.IntentId != nil && cryptoReceiptCounts(receipt) {
			affected[*receipt.IntentId] = true
		}
	}
	result.OpenInvoices = len(affected)
	policy := cryptoIntentPolicyFor(config, asset)
	for intentId := range affected {
		intent := model.GetCryptoPaymentIntent(ctx, intentId)
		if intent == nil || intent.Asset != asset {
			continue
		}
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intentId)
		view := cryptoEvaluateIntent(intent, receipts, policy, now)
		if view.NeedsRate {
			if rate := rateNow(); rate != nil {
				for _, receipt := range receipts {
					if receipt.FirstSeenRateUsd == nil && receipt.FirstSeenAt.After(intent.ExpiresAt) {
						model.SetCryptoPaymentReceiptFirstSeenRateInConn(nil, ctx, receipt.Key(), *rate)
					}
				}
				receipts = model.GetCryptoPaymentReceiptsForIntent(ctx, intentId)
				view = cryptoEvaluateIntent(intent, receipts, policy, now)
			}
		}
		switch view.Action {
		case cryptoIntentActionCredit:
			credited, err := cryptoCreditIntent(clientSession, intent, &cryptoCredit{
				usdNanoCents: view.CreditUsd,
				receipts:     view.CreditReceipts,
				receiptUsd:   view.ReceiptUsdValue,
				late:         view.Late,
			})
			if err != nil {
				result.CreditErrored += 1
				cryptoCreditFailed(ctx, asset, intentId, view.CreditReceipts, err, result)
				continue
			}
			if credited {
				result.Credited += 1
			}
		case cryptoIntentActionReview:
			model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, &intentId, view.ReviewReceipts, model.CryptoReceiptStateReview, view.ActionReason)
			result.Reviewed += 1
			glog.Infof("[cryptopay]invoice %s held for review: %s\n", intentId, view.ActionReason)
		}
	}

	result.MarkedFinal = model.MarkCryptoPaymentReceiptsFinalInConn(nil, ctx, asset, finalKeys, now)

	model.SetCryptoScanCursorSuccessInConn(nil, ctx, asset, int64(walletHeight), int64(walletHeight), int64(nodeHeight), int64(networkHeight), now)
	cryptoPayScanLastSuccess.WithLabelValues(asset).Set(float64(now.Unix()))
	cryptoPayHeights.WithLabelValues(asset, "wallet").Set(float64(walletHeight))
	cryptoPayHeights.WithLabelValues(asset, "node").Set(float64(nodeHeight))
	cryptoPayHeights.WithLabelValues(asset, "network").Set(float64(networkHeight))
	cryptoRefreshReceiptGauges(ctx, asset)
	return nil
}

// cryptoCreditFailed counts a failed credit of an invoice. The error is
// logged once a minute per coin (the details at verbosity 1); after
// model.CryptoPaymentCreditFailureLimit failures in a row the receipts that
// were to be credited go to review (credit_failed), so the scanner stops
// retrying and an operator finds out why (mmm/ZCASH-CONTRACT.md §3.13.16).
func cryptoCreditFailed(
	ctx context.Context,
	asset string,
	intentId server.Id,
	receipts []model.CryptoPaymentReceiptKey,
	err error,
	result *CryptoPaymentScanResult,
) {
	failures := model.AddCryptoPaymentIntentCreditFailure(ctx, intentId)
	cryptoScanFailureReports.report("credit/"+asset, func() {
		glog.Errorf("[cryptopay]%s credit of invoice %s failed (%d in a row): %s\n", asset, intentId, failures, err)
	})
	if glog.V(1) {
		glog.Infof("[cryptopay]%s credit of invoice %s failed (%d in a row): %s\n", asset, intentId, failures, err)
	}
	if model.CryptoPaymentCreditFailureLimit <= failures {
		model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, &intentId, receipts, model.CryptoReceiptStateReview, model.CryptoReviewReasonCreditFailed)
		result.Reviewed += 1
		glog.Errorf("[cryptopay]invoice %s held for review: its credit failed %d times in a row\n", intentId, failures)
	}
}

// cryptoFirstSeenAt is when a receipt counts as first seen: the daemon's time
// for it (the block's, or when the mempool transaction arrived), but never
// after the scan that saw it and never before its invoice's quote. A scanner
// that was down therefore never makes a payment late (mmm/ZCASH-CONTRACT.md §3.13.4).
func cryptoFirstSeenAt(timestamp time.Time, intent *model.CryptoPaymentIntent, now time.Time) time.Time {
	firstSeen := now
	if !timestamp.IsZero() && timestamp.Before(now) {
		firstSeen = timestamp
	}
	if intent != nil && firstSeen.Before(intent.QuotedAt) {
		firstSeen = intent.QuotedAt
	}
	return firstSeen
}

// cryptoReceiptOf is a reported receipt as a row: its invoice (by the
// address's index), when it was first seen, and, for a new row, its state.
// Ignored: time-locked, or a transparent Zcash output. Review: paid to an
// address no invoice was given (the primary address included). Seen with
// the reason double_spend: a mempool transaction the wallet flags, which
// counts once mined. The invoice's judgment does the rest.
func cryptoReceiptOf(
	asset string,
	walletReceipt *cryptoWalletReceipt,
	intents map[int64]*model.CryptoPaymentIntent,
	now time.Time,
) *model.CryptoPaymentReceipt {
	receipt := &model.CryptoPaymentReceipt{
		Asset:         asset,
		TxId:          walletReceipt.TxId,
		OutputKey:     walletReceipt.OutputKey,
		Address:       walletReceipt.Address,
		Pool:          walletReceipt.Pool,
		AmountAtomic:  int64(min(walletReceipt.AmountAtomic, 1<<62)),
		Confirmations: int(min(walletReceipt.Confirmations, 1<<30)),
		FirstSeenAt:   now,
		State:         model.CryptoReceiptStateSeen,
	}
	var intent *model.CryptoPaymentIntent
	if walletReceipt.AddressIndex != nil && *walletReceipt.AddressIndex <= 1<<62 {
		index := int64(*walletReceipt.AddressIndex)
		receipt.AddressIndex = &index
		if matched, ok := intents[index]; ok {
			intent = matched
			intentId := matched.IntentId
			receipt.IntentId = &intentId
		}
	}
	receipt.FirstSeenAt = cryptoFirstSeenAt(walletReceipt.Timestamp, intent, now)
	if 0 < walletReceipt.BlockHeight {
		height := int64(walletReceipt.BlockHeight)
		receipt.BlockHeight = &height
		receipt.State = model.CryptoReceiptStateConfirmed
	}
	switch {
	case walletReceipt.UnlockTime != 0:
		receipt.State = model.CryptoReceiptStateIgnored
		receipt.ReviewReason = model.CryptoReviewReasonLocked
	case asset == model.CryptoAssetZec && slices.Contains(splitCryptoPools(walletReceipt.Pool), cryptoZcashPoolTransparent):
		receipt.State = model.CryptoReceiptStateIgnored
		receipt.ReviewReason = model.CryptoReviewReasonTransparent
	case receipt.IntentId == nil:
		receipt.State = model.CryptoReceiptStateReview
		receipt.ReviewReason = model.CryptoReviewReasonUnknownAddress
	case walletReceipt.DoubleSpend && walletReceipt.BlockHeight == 0:
		// monero-wallet-rpc sets the flag on pool entries only: the mined copy
		// is never flagged, and then counts (mmm/ZCASH-CONTRACT.md §3.13.3)
		receipt.State = model.CryptoReceiptStateSeen
		receipt.ReviewReason = model.CryptoReviewReasonDoubleSpend
	}
	return receipt
}

func splitCryptoPools(pools string) []string {
	out := []string{}
	start := 0
	for i := 0; i <= len(pools); i++ {
		if i == len(pools) || pools[i] == ',' {
			if start < i {
				out = append(out, pools[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// cryptoRefreshReceiptGauges sets the receipts gauge and the pool gauge from
// the database. Best effort: a failed count leaves the last values.
func cryptoRefreshReceiptGauges(ctx context.Context, asset string) {
	server.HandleError(func() {
		counts := model.CountCryptoPaymentReceiptsByState(ctx, asset)
		cryptoPayReceipts.DeletePartialMatch(map[string]string{"asset": asset})
		for key, count := range counts {
			cryptoPayReceipts.WithLabelValues(asset, key[0], key[1]).Set(float64(count))
		}
		cryptoPayAddressPoolFree.WithLabelValues(asset).Set(float64(model.CountFreeCryptoPaymentAddresses(ctx, asset)))
	})
}

// ----- the address pool -----

// cryptoWalletHighWater is a daemon that must not issue an index at or below
// the pool's highest (monero-wallet-rpc forgets uncommitted subaddresses).
type cryptoWalletHighWater interface {
	SetAddressHighWater(index uint64)
}

// cryptoWalletLookahead is a daemon whose scan window must cover every pool
// address (a monero wallet rebuilt from keys scans only its default
// lookahead): given the pool's highest address, it widens the window when the
// wallet does not own it.
type cryptoWalletLookahead interface {
	EnsureLookahead(ctx context.Context, highestIndex uint64, highestAddress string) error
}

// CryptoAddressRefillResult is what one refill did.
type CryptoAddressRefillResult struct {
	Asset string `json:"asset"`
	// false: the coin's wallet daemon is not configured; the chain backs off
	Configured bool   `json:"configured"`
	Free       int    `json:"free"`
	Added      int    `json:"added"`
	Error      string `json:"error,omitempty"`
}

// RunCryptoAddressRefill derives addresses until the coin's pool holds
// address_pool_target free ones (at most cryptoRefillMaxPerRun a run). Each
// address is stored as soon as it is derived, so a failure keeps the progress.
// It runs whenever the coin's daemon is configured, so a coin switched on
// again has its pool ready.
func RunCryptoAddressRefill(clientSession *session.ClientSession, asset string) (*CryptoAddressRefillResult, error) {
	config := cryptoPayConfigFunc()
	result := &CryptoAddressRefillResult{Asset: asset}
	if err := cryptoAssetScannable(config, asset); err != nil {
		return result, nil
	}
	result.Configured = true
	wallet, _, err := cryptoWalletForAsset(config, asset)
	if err == nil {
		err = cryptoRefill(clientSession.Ctx, config, asset, wallet, result)
	}
	if err != nil {
		result.Error = err.Error()
		cryptoScanFailureReports.report("refill/"+asset, func() {
			glog.Warningf("[cryptopay]%s address refill failed: %s\n", asset, err)
		})
	}
	return result, nil
}

func cryptoRefill(
	ctx context.Context,
	config *CryptoPayConfig,
	asset string,
	wallet cryptoWallet,
	result *CryptoAddressRefillResult,
) error {
	if err := cryptoCheckWallet(ctx, nil, asset, wallet); err != nil {
		return err
	}
	// every refill: the wallet must still own the pool's highest address, or
	// it does not see payments to it (mmm/ZCASH-CONTRACT.md §3.13.9)
	if lookaheadWallet, ok := wallet.(cryptoWalletLookahead); ok {
		if index, address, ok := model.GetCryptoPaymentPoolAddressAt(ctx, asset, false); ok && 0 <= index {
			if err := lookaheadWallet.EnsureLookahead(ctx, uint64(index), address); err != nil {
				return err
			}
		}
	}
	free := model.CountFreeCryptoPaymentAddresses(ctx, asset)
	result.Free = free
	cryptoPayAddressPoolFree.WithLabelValues(asset).Set(float64(free))
	highWater := model.MaxCryptoPaymentAddressIndex(ctx, asset)
	if highWaterWallet, ok := wallet.(cryptoWalletHighWater); ok && 0 <= highWater {
		highWaterWallet.SetAddressHighWater(uint64(highWater))
	}
	for i := 0; free+result.Added < config.AddressPoolTarget && i < cryptoRefillMaxPerRun; i += 1 {
		address, index, err := wallet.NewAddress(ctx, cryptoPoolAddressLabel)
		if err != nil {
			return err
		}
		if 1<<62 < index {
			return fmt.Errorf("the %s wallet issued an index out of range", asset)
		}
		added := model.AddCryptoPaymentAddresses(ctx, []*model.CryptoPaymentAddress{{
			Asset:        asset,
			AddressIndex: int64(index),
			Address:      address,
		}})
		result.Added += int(added)
	}
	if 0 < result.Added {
		glog.Infof("[cryptopay]%s address pool: %d free, %d added\n", asset, free, result.Added)
	}
	cryptoPayAddressPoolFree.WithLabelValues(asset).Set(float64(free + result.Added))
	return nil
}

// ----- the retention unlink (decision 9) -----

// One unlink batch.
const cryptoUnlinkBatchSize = 500

// The most batches one unlink run takes.
const cryptoUnlinkMaxBatches = 100

// CryptoPaymentUnlinkResult is what one unlink run did.
type CryptoPaymentUnlinkResult struct {
	Unlinked int `json:"unlinked"`
	// paid intents of the old Solana Pay endpoints, which no invoice links
	SolanaUnlinked int `json:"solana_unlinked"`
	// unfulfilled Solana payments recorded before the cutoff
	SolanaUnfulfilledUnlinked int `json:"solana_unfulfilled_unlinked"`
}

// RunCryptoPaymentUnlink drops the link between invoices and their networks
// once unlink_after has passed since the credit, or since the invoice was
// made when it was never credited (decision 9, mmm/ZCASH-CONTRACT.md §3.13.6;
// model.UnlinkCryptoPaymentIntents has the steps). Transaction ids, amounts
// and days stay.
//
// The USDC paid through the old Solana Pay endpoints (POST
// /solana/payment-intent, POST /pay/data/solana-intent), which older apps and
// the SDK still use, is unlinked after the same unlink_after
// (model.UnlinkSolanaPaymentIntents), and so is every unfulfilled Solana
// payment recorded before then (model.UnlinkSolanaUnfulfilledPayments).
func RunCryptoPaymentUnlink(clientSession *session.ClientSession) (*CryptoPaymentUnlinkResult, error) {
	config := cryptoPayConfigFunc()
	now := server.NowUtc()
	cutoff := now.Add(-config.UnlinkAfter)
	result := &CryptoPaymentUnlinkResult{}
	for range cryptoUnlinkMaxBatches {
		unlinked := model.UnlinkCryptoPaymentIntents(clientSession.Ctx, cutoff, cryptoUnlinkBatchSize, now)
		result.Unlinked += unlinked
		if unlinked < cryptoUnlinkBatchSize {
			break
		}
	}
	for range cryptoUnlinkMaxBatches {
		unlinked := model.UnlinkSolanaPaymentIntents(clientSession.Ctx, cutoff, cryptoUnlinkBatchSize)
		result.SolanaUnlinked += unlinked
		if unlinked < cryptoUnlinkBatchSize {
			break
		}
	}
	for range cryptoUnlinkMaxBatches {
		unlinked := model.UnlinkSolanaUnfulfilledPayments(clientSession.Ctx, cutoff, cryptoUnlinkBatchSize)
		result.SolanaUnfulfilledUnlinked += unlinked
		if unlinked < cryptoUnlinkBatchSize {
			break
		}
	}
	if 0 < result.Unlinked {
		glog.Infof("[cryptopay]unlinked %d paid invoices from their networks\n", result.Unlinked)
	}
	if 0 < result.SolanaUnlinked || 0 < result.SolanaUnfulfilledUnlinked {
		glog.Infof(
			"[cryptopay]unlinked %d paid Solana Pay intents and %d unfulfilled Solana payments from their networks\n",
			result.SolanaUnlinked,
			result.SolanaUnfulfilledUnlinked,
		)
	}
	return result, nil
}

// CryptoPaymentScanInterval is how often a coin is scanned.
func CryptoPaymentScanInterval() time.Duration {
	return cryptoPayConfigFunc().ScanInterval
}

// CryptoScannedAssets lists the scanned coins: each has a scan chain and a
// refill chain, which back off while its daemon is not configured.
func CryptoScannedAssets() []string {
	return slices.Clone(cryptoPayScannedAssets)
}
