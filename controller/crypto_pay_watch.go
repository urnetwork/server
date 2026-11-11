package controller

// The invoice addresses a per-address coin's scanner reads (Bitcoin and ETH
// through Alchemy, SOL through Helius, TAO through snow's node;
// mmm/BTCPAY.md §17.3). Monero's and Zcash's daemons see every address of
// their wallet; a Coinbase deposit address is ours only by the pool, so each
// scan names the addresses in play:
//
//   - hot, read every scan: an open invoice, or one with a payment not yet at
//     its final depth;
//   - warm, read once every cryptoWatchWarmEvery scan intervals: an invoice
//     that expired, was credited or was voided within watch_after (24 h), for
//     a late payment, a top-up after the quote, or a second payment;
//   - hinted, read on the next scan whatever its age: an address a verified
//     webhook delivery (Alchemy, Helius) named.
//
// The chain is the source of truth for every credit; a webhook only brings a
// read forward. A payment to an address outside all three is still in our
// Coinbase account, and support finds it there.

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The most invoice addresses one scan of a per-address coin reads, hot first.
const cryptoWatchMaxAddresses = 500

// A closed invoice's address is read once in this many scan intervals.
const cryptoWatchWarmEvery = 10

// The most hinted addresses one scan takes.
const cryptoScanHintMax = 200

// A hint is dropped after this long unread (a scanner that is down).
const cryptoScanHintTtl = 48 * time.Hour

// The watched addresses' read. Replaceable only by tests.
var cryptoListWatchedAddresses = func(ctx context.Context, asset string, now time.Time, watchSince time.Time, limit int) []*model.CryptoPaymentWatchedAddress {
	return model.ListCryptoPaymentWatchedAddressesInConn(nil, ctx, asset, now, watchSince, limit)
}

// The pool's index of each given address of a coin. Replaceable only by
// tests.
var cryptoResolveAddresses = func(ctx context.Context, asset string, addresses []string) map[string]int64 {
	return model.GetCryptoPaymentAddressIndexesInConn(nil, ctx, asset, addresses)
}

// The addresses a coin's stored receipts of one transaction were paid to.
// Replaceable only by tests.
var cryptoReceiptAddressesForTx = func(ctx context.Context, asset string, txId string) []string {
	return model.GetCryptoPaymentReceiptAddressesForTxInConn(nil, ctx, asset, txId)
}

// Which of the given addresses are invoice addresses of the coin. Replaceable
// only by tests.
var cryptoInvoiceAddresses = func(ctx context.Context, asset string, addresses []string) []string {
	return model.GetCryptoPaymentInvoiceAddressesInConn(nil, ctx, asset, addresses)
}

// cryptoWatchedAddresses is what one scan of a per-address coin reads: the hot
// addresses, the warm ones when their read is due, and the hinted ones.
func cryptoWatchedAddresses(
	ctx context.Context,
	config *CryptoPayConfig,
	asset string,
	cursor *model.CryptoScanCursor,
	now time.Time,
) []*model.CryptoPaymentWatchedAddress {
	watchSince := now.Add(-config.WatchAfterFor(asset))
	listed := cryptoListWatchedAddresses(ctx, asset, now, watchSince, cryptoWatchMaxAddresses)
	warmDue := cryptoWarmReadDue(config, asset, cursor, now)
	watched := []*model.CryptoPaymentWatchedAddress{}
	seen := map[string]bool{}
	for _, address := range listed {
		if address.Hot || warmDue {
			watched = append(watched, address)
			seen[address.Address] = true
		}
	}
	if hinted := cryptoTakeScanHints(ctx, asset, cryptoScanHintMax); 0 < len(hinted) {
		for address, index := range cryptoResolveAddresses(ctx, asset, hinted) {
			if !seen[address] {
				watched = append(watched, &model.CryptoPaymentWatchedAddress{Address: address, AddressIndex: index, Hot: true})
				seen[address] = true
			}
		}
	}
	return watched
}

// cryptoWarmReadDue reports whether this scan reads the warm addresses: the
// first scan in each period of cryptoWatchWarmEvery scan intervals, judged
// against the last successful scan, so it holds across taskworker processes.
func cryptoWarmReadDue(config *CryptoPayConfig, asset string, cursor *model.CryptoScanCursor, now time.Time) bool {
	period := config.ScanIntervalFor(asset) * cryptoWatchWarmEvery
	if period <= 0 || cursor == nil || cursor.LastSuccessAt == nil {
		return true
	}
	return !now.Truncate(period).Equal(cursor.LastSuccessAt.Truncate(period))
}

// ----- scan hints: a verified webhook delivery brings a read forward -----

func cryptoScanHintKey(asset string) string {
	return "crypto_scan_hints_" + asset
}

// cryptoAddScanHints asks the coin's next scan to read the addresses, any age.
// Best effort: a hint that is lost costs only the wait for the regular read.
// Replaceable only by tests.
var cryptoAddScanHints = func(ctx context.Context, asset string, addresses []string) {
	if len(addresses) == 0 {
		return
	}
	members := make([]any, 0, len(addresses))
	for _, address := range addresses {
		members = append(members, address)
	}
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			key := cryptoScanHintKey(asset)
			if err := r.SAdd(ctx, key, members...).Err(); err != nil {
				glog.Warningf("[cryptopay]%s scan hint not kept: %s\n", asset, err)
				return
			}
			r.Expire(ctx, key, cryptoScanHintTtl)
		})
	})
}

// cryptoTakeScanHints takes up to limit hinted addresses of the coin.
// Replaceable only by tests.
var cryptoTakeScanHints = func(ctx context.Context, asset string, limit int) (addresses []string) {
	addresses = []string{}
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			taken, err := r.SPopN(ctx, cryptoScanHintKey(asset), int64(limit)).Result()
			if err != nil {
				return
			}
			addresses = taken
		})
	})
	return
}
