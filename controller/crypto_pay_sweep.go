package controller

// The sweep of a per-address coin's issued addresses (ETH through Alchemy,
// native SOL through Helius). No reader of ours sees every invoice address of
// these coins: each scan reads the addresses in play often
// (crypto_pay_watch.go: open invoices, invoices closed within watch_after,
// hinted ones), and the sweep reads every other address an invoice was ever
// given, open or closed, credited, voided or past the retention unlink, so a
// payment to an old or an expired invoice is recorded too. The scanner judges
// it by the late-payment rules (cryptoEvaluateIntent): credited where they
// credit, else held for an operator (late_underpaid, extra, voided, unlinked),
// which payment-received-held reports.
//
// Each scan takes the next ExamineLimit issued addresses in pool index order
// after the coin's rotation position (crypto_address_sweep_position: one
// indexed range read, so a scan's database work is bounded however many
// addresses the coin has issued), wrapping at the end. It reads at most
// ReadsPerScan of them from the chain, each from its own cursor
// (crypto_address_sweep: ETH the last block read through, SOL the newest
// signature examined) to the reader's final head, and only those due: never
// read, last read ReadEvery ago or more (ReadEveryUnwatched for a coin whose
// addresses no provider webhook watches), or whose last read failed. A read
// takes at most RequestsPerScan chain requests a scan in all.
//
// Progress is checkpointed per address: its receipts are recorded and its
// cursor written before the position moves past it. A read that stops early
// (the request budget) keeps its cursor where it stopped and the position
// before it, so the next scan goes on from there. A read that fails records
// its error on the address, moves the position past it and ends the sweep for
// the scan (a chain source that is down costs one request a scan); the
// address is read again when the rotation comes back to it. The sweep never
// fails the scan: the coin stays sellable, and a failing address is counted
// (urnetwork_crypto_pay_sweep_reads_total) and logged once a minute.
//
// Bitcoin and TAO need no sweep: their readers read every block and match
// every issued address in it (crypto_pay_bitcoin.go, crypto_pay_bittensor.go).

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

// The sweep's tunables.
type CryptoAddressSweepSettings struct {
	// A completely read address is due again this long after the read, for a
	// coin whose provider webhook watches every pool address (ETH's Alchemy
	// Address Activity brings an address's read forward when a payment lands,
	// so the sweep is the backstop for a lost delivery). An ETH read is one
	// alchemy_getAssetTransfers (150 compute units): a thousand old addresses
	// a day cost 150 thousand units. Non-positive reads every address each
	// time the rotation comes back to it.
	ReadEvery time.Duration
	// The same for a coin no webhook watches (native SOL: the Helius webhook
	// does not list per-invoice addresses), where the sweep is the only
	// reader once an invoice is out of play. A SOL read with nothing new is
	// one getSignaturesForAddress. Non-positive as for ReadEvery.
	ReadEveryUnwatched time.Duration
	// The most issued addresses one scan looks at in the rotation, due or
	// not: the bound on the scan's database work. Non-positive sweeps
	// nothing.
	ExamineLimit int
	// The most addresses one scan reads from the chain. Non-positive sweeps
	// nothing.
	ReadsPerScan int
	// The most chain requests one scan's sweep makes (signature pages,
	// transactions, transfer reads). A read that would pass it stops and goes
	// on next scan. Non-positive sweeps nothing.
	RequestsPerScan int
	// SOL: the signatures one getSignaturesForAddress page asks for, at most
	// 1000 (the RPC's limit).
	PageSize int
	// SOL: the pages one address's read may list. More history since the
	// cursor is an error for the address, for an operator: nothing of it is
	// acted on, since an older transaction would be passed over for good.
	MaxPages int
}

// A new instance on every call.
func DefaultCryptoAddressSweepSettings() *CryptoAddressSweepSettings {
	return &CryptoAddressSweepSettings{
		// a day: the webhook is the fast path for a late payment
		ReadEvery: 24 * time.Hour,
		// an hour: a late payment to an unwatched address waits at most this
		// long past the next rotation to be recorded
		ReadEveryUnwatched: time.Hour,
		ExamineLimit:       200,
		// ten a scan (ETH every 30 s, SOL every 20 s by default): far more
		// than a coin issues
		ReadsPerScan:    10,
		RequestsPerScan: 40,
		PageSize:        100,
		// a thousand signatures: a deposit address's history is short
		MaxPages: 10,
	}
}

// cryptoWalletSweeper is a per-address coin's reader of one issued address's
// incoming transfers since the address's cursor (ETH, SOL).
type cryptoWalletSweeper interface {
	// SweepAddress reads the transfers to the address after its cursor (its
	// whole history before the first read) up to the reader's final head, as
	// receipts, with the cursor after them. Each chain request is taken from
	// spend first; complete = false when spend refused one: next is where the
	// read stopped (nil: nothing was read), and the next read goes on from
	// it. A read that fails part way returns what it read before the failure
	// with its error, and next where it stopped.
	SweepAddress(
		ctx context.Context,
		address *model.CryptoAddressSweepAddress,
		settings *CryptoAddressSweepSettings,
		spend func() bool,
	) (receipts []*cryptoWalletReceipt, next *model.CryptoAddressSweepCursor, complete bool, returnErr error)
}

// cryptoWalletWebhookWatched is a coin whose provider webhook watches every
// pool address it registered (ETH, when alchemy.yml names the webhook and the
// Notify token): its sweep reads each address less often.
type cryptoWalletWebhookWatched interface {
	WebhookWatched() bool
}

// The sweep's reads and writes. Replaceable only by tests.
var (
	cryptoSweepPosition = func(ctx context.Context, asset string) int64 {
		return model.GetCryptoAddressSweepPositionInConn(nil, ctx, asset)
	}
	cryptoSweepSetPosition = func(ctx context.Context, asset string, afterAddressIndex int64, now time.Time) {
		model.SetCryptoAddressSweepPositionInConn(nil, ctx, asset, afterAddressIndex, now)
	}
	cryptoSweepAddresses = func(ctx context.Context, asset string, afterAddressIndex int64, limit int) []*model.CryptoAddressSweepAddress {
		return model.ListCryptoAddressesToSweepInConn(nil, ctx, asset, afterAddressIndex, limit)
	}
	cryptoSweepSetCursor = func(ctx context.Context, cursor *model.CryptoAddressSweepCursor, now time.Time) {
		model.SetCryptoAddressSweepCursorInConn(nil, ctx, cursor, now)
	}
)

// Issued addresses the sweep read, by asset and outcome: read (complete),
// partial (stopped at the request budget, goes on next scan), error.
var cryptoPaySweepReads = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "sweep_reads_total",
	Help:      "Reads of a per-address coin's issued addresses by the scan's sweep, by asset and outcome (read, partial, error)",
}, []string{"asset", "outcome"})

func init() {
	prometheus.MustRegister(cryptoPaySweepReads)
}

// cryptoSweepReadEvery is how long a completely read address of the wallet's
// coin waits before its next read: ReadEvery when a provider webhook watches
// the coin's addresses, else ReadEveryUnwatched.
func cryptoSweepReadEvery(wallet cryptoWallet, settings *CryptoAddressSweepSettings) time.Duration {
	if watched, ok := wallet.(cryptoWalletWebhookWatched); ok && watched.WebhookWatched() {
		return settings.ReadEvery
	}
	return settings.ReadEveryUnwatched
}

// cryptoSweepIssuedAddresses is one scan's sweep (see the file header), with
// readEvery from cryptoSweepReadEvery. record records the receipts of one
// address's read, as the scan records the ones its reader reported.
func cryptoSweepIssuedAddresses(
	ctx context.Context,
	asset string,
	sweeper cryptoWalletSweeper,
	settings *CryptoAddressSweepSettings,
	readEvery time.Duration,
	now time.Time,
	record func(receipts []*cryptoWalletReceipt),
	result *CryptoPaymentScanResult,
) {
	if settings.ExamineLimit <= 0 || settings.ReadsPerScan <= 0 || settings.RequestsPerScan <= 0 {
		return
	}

	position := cryptoSweepPosition(ctx, asset)
	startPosition := position
	addresses := cryptoSweepAddresses(ctx, asset, position, settings.ExamineLimit)
	if len(addresses) == 0 && 0 <= position {
		// the end of the rotation: the next one starts at the lowest index
		position = -1
		addresses = cryptoSweepAddresses(ctx, asset, position, settings.ExamineLimit)
	}
	defer func() {
		if position != startPosition {
			cryptoSweepSetPosition(ctx, asset, position, now)
		}
	}()

	requests := 0
	spend := func() bool {
		if settings.RequestsPerScan <= requests {
			return false
		}
		requests += 1
		return true
	}
	reads := 0
	for _, address := range addresses {
		cursor := address.Cursor
		due := cursor == nil || cursor.ReadAt == nil || cursor.LastError != "" || readEvery <= 0 ||
			!now.Before(cursor.ReadAt.Add(readEvery))
		if !due {
			position = address.AddressIndex
			continue
		}
		if settings.ReadsPerScan <= reads || settings.RequestsPerScan <= requests {
			return
		}
		reads += 1
		receipts, next, complete, err := sweeper.SweepAddress(ctx, address, settings, spend)
		// what was read is recorded before the cursor moves past it
		if 0 < len(receipts) {
			record(receipts)
			result.SweptReceipts += len(receipts)
		}
		if err != nil {
			// the address keeps where it got to on the chain and is read again
			// when the rotation comes back to it; the sweep ends for this scan
			failed := &model.CryptoAddressSweepCursor{}
			if next != nil {
				failed = next
			} else if cursor != nil {
				copied := *cursor
				failed = &copied
			}
			failed.Asset = asset
			failed.Address = address.Address
			failed.ReadAt = nil
			if cursor != nil {
				failed.ReadAt = cursor.ReadAt
			}
			failed.LastError = truncateCryptoError(err.Error())
			cryptoSweepSetCursor(ctx, failed, now)
			position = address.AddressIndex
			result.SweepErrors += 1
			cryptoPaySweepReads.WithLabelValues(asset, "error").Inc()
			cryptoScanFailureReports.report("sweep/"+asset, func() {
				glog.Warningf("[cryptopay]%s sweep of issued address %d failed (it is read again when the rotation comes back): %s\n", asset, address.AddressIndex, err)
			})
			return
		}
		if next != nil {
			next.Asset = asset
			next.Address = address.Address
			next.LastError = ""
			next.ReadAt = nil
			if cursor != nil {
				next.ReadAt = cursor.ReadAt
			}
			if complete {
				readAt := now
				next.ReadAt = &readAt
			}
			cryptoSweepSetCursor(ctx, next, now)
		}
		if !complete {
			// stopped at the request budget: the next scan goes on with this
			// address from its cursor
			cryptoPaySweepReads.WithLabelValues(asset, "partial").Inc()
			return
		}
		result.Swept += 1
		cryptoPaySweepReads.WithLabelValues(asset, "read").Inc()
		position = address.AddressIndex
	}
}

// cryptoReceiptIndexes are the address indexes the receipts name.
func cryptoReceiptIndexes(receipts []*cryptoWalletReceipt) []int64 {
	indexes := []int64{}
	for _, receipt := range receipts {
		if receipt.AddressIndex != nil && *receipt.AddressIndex <= 1<<62 {
			indexes = append(indexes, int64(*receipt.AddressIndex))
		}
	}
	return indexes
}

// cryptoSweepFromHeight is the first block an ETH read of a swept address
// asks for: after its cursor, or the chain's start before its first read.
func cryptoSweepFromHeight(cursor *model.CryptoAddressSweepCursor) uint64 {
	if cursor == nil || cursor.Height == nil || *cursor.Height < 0 {
		return 0
	}
	return uint64(*cursor.Height) + 1
}

// cryptoHexBlock is a block number as a JSON-RPC quantity.
func cryptoHexBlock(number uint64) string {
	return fmt.Sprintf("0x%x", number)
}
