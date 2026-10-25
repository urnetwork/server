package work

// The crypto payment rail's tasks (mmm/ZCASH-CONTRACT.md §3.8): a
// scan chain and an address refill chain per scanned coin (Zcash, Monero),
// and the daily retention unlink. Thin shells: the work is in
// controller/crypto_pay_scan.go.
//
// A coin's chains always run: the kill switch gates selling only, and payments
// to invoices already handed out must still be seen and credited (mmm/ZCASH-CONTRACT.md
// §3.13.1). Startup schedules both chains of every scanned coin, and every run
// schedules its successor: after the scan interval (the refill's minute) when
// the coin's daemon is configured, and with a doubling backoff up to
// cryptoPayIdleMaxDelay while it is not (no vault file, or an unreadable
// one), so a vault fix is picked up within minutes without a restart. A
// coin's RunOnce key holds one row per chain, so two scans of one coin never
// overlap.

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A scan or a refill is abandoned after this long.
const cryptoPayTaskMaxTime = 10 * time.Minute

// The pool is topped up this often.
const cryptoAddressRefillInterval = 1 * time.Minute

// The retention unlink runs this often.
const cryptoPaymentUnlinkInterval = 24 * time.Hour

// The longest a chain waits while its coin's daemon is not configured.
const cryptoPayIdleMaxDelay = 10 * time.Minute

// cryptoPayIdleDelay is the wait after the idle-th run in a row that found no
// daemon configured: the chain's interval, doubled per idle run, at most
// cryptoPayIdleMaxDelay.
func cryptoPayIdleDelay(interval time.Duration, idle int) time.Duration {
	delay := interval
	for i := 0; i < idle && delay < cryptoPayIdleMaxDelay; i += 1 {
		delay *= 2
	}
	return min(delay, cryptoPayIdleMaxDelay)
}

type CryptoPaymentScanArgs struct {
	Asset string `json:"asset"`
	// the runs in a row that found the coin's daemon not configured
	Idle int `json:"idle,omitempty"`
}

type CryptoPaymentScanResult = controller.CryptoPaymentScanResult

// ScheduleCryptoPaymentScans starts the scan and refill chains of every
// scanned coin.
func ScheduleCryptoPaymentScans(clientSession *session.ClientSession, tx server.PgTx) {
	now := server.NowUtc()
	for _, asset := range controller.CryptoScannedAssets() {
		scheduleCryptoPaymentScan(clientSession, tx, &CryptoPaymentScanArgs{Asset: asset}, now)
		scheduleCryptoAddressRefill(clientSession, tx, &CryptoAddressRefillArgs{Asset: asset}, now)
	}
}

func scheduleCryptoPaymentScan(clientSession *session.ClientSession, tx server.PgTx, args *CryptoPaymentScanArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		CryptoPaymentScan,
		args,
		clientSession,
		task.RunOnce("crypto_scan_"+args.Asset),
		task.RunAt(runAt),
		task.MaxTime(cryptoPayTaskMaxTime),
	)
}

// CryptoPaymentScan is one scan of a coin.
func CryptoPaymentScan(
	args *CryptoPaymentScanArgs,
	clientSession *session.ClientSession,
) (*CryptoPaymentScanResult, error) {
	return controller.RunCryptoPaymentScan(clientSession, args.Asset)
}

// CryptoPaymentScanPost schedules the next scan: after the scan interval, or
// later while the coin's daemon is not configured.
func CryptoPaymentScanPost(
	args *CryptoPaymentScanArgs,
	result *CryptoPaymentScanResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	next := &CryptoPaymentScanArgs{Asset: args.Asset}
	delay := controller.CryptoPaymentScanInterval()
	if result == nil || !result.Configured {
		next.Idle = args.Idle + 1
		delay = cryptoPayIdleDelay(delay, next.Idle)
	}
	scheduleCryptoPaymentScan(clientSession, tx, next, server.NowUtc().Add(delay))
	return nil
}

type CryptoAddressRefillArgs struct {
	Asset string `json:"asset"`
	// the runs in a row that found the coin's daemon not configured
	Idle int `json:"idle,omitempty"`
}

type CryptoAddressRefillResult = controller.CryptoAddressRefillResult

func scheduleCryptoAddressRefill(clientSession *session.ClientSession, tx server.PgTx, args *CryptoAddressRefillArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		CryptoAddressRefill,
		args,
		clientSession,
		task.RunOnce("crypto_address_refill_"+args.Asset),
		task.RunAt(runAt),
		task.MaxTime(cryptoPayTaskMaxTime),
	)
}

// CryptoAddressRefill keeps a coin's address pool at its target.
func CryptoAddressRefill(
	args *CryptoAddressRefillArgs,
	clientSession *session.ClientSession,
) (*CryptoAddressRefillResult, error) {
	return controller.RunCryptoAddressRefill(clientSession, args.Asset)
}

// CryptoAddressRefillPost schedules the next refill: after a minute, or later
// while the coin's daemon is not configured.
func CryptoAddressRefillPost(
	args *CryptoAddressRefillArgs,
	result *CryptoAddressRefillResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	next := &CryptoAddressRefillArgs{Asset: args.Asset}
	delay := cryptoAddressRefillInterval
	if result == nil || !result.Configured {
		next.Idle = args.Idle + 1
		delay = cryptoPayIdleDelay(delay, next.Idle)
	}
	scheduleCryptoAddressRefill(clientSession, tx, next, server.NowUtc().Add(delay))
	return nil
}

type CryptoPaymentUnlinkArgs struct{}

type CryptoPaymentUnlinkResult = controller.CryptoPaymentUnlinkResult

// ScheduleCryptoPaymentUnlink starts the daily retention unlink. It runs
// whatever coins are on: paid invoices outlive a kill switch.
func ScheduleCryptoPaymentUnlink(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		CryptoPaymentUnlink,
		&CryptoPaymentUnlinkArgs{},
		clientSession,
		task.RunOnce("crypto_payment_unlink"),
		task.RunAt(server.NowUtc().Add(cryptoPaymentUnlinkInterval)),
		task.MaxTime(cryptoPayTaskMaxTime),
	)
}

// CryptoPaymentUnlink drops the link between invoices paid more than
// unlink_after ago and their networks (decision 9), and between the old
// Solana Pay endpoints' payments and theirs.
func CryptoPaymentUnlink(
	args *CryptoPaymentUnlinkArgs,
	clientSession *session.ClientSession,
) (*CryptoPaymentUnlinkResult, error) {
	return controller.RunCryptoPaymentUnlink(clientSession)
}

func CryptoPaymentUnlinkPost(
	args *CryptoPaymentUnlinkArgs,
	result *CryptoPaymentUnlinkResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleCryptoPaymentUnlink(clientSession, tx)
	return nil
}
