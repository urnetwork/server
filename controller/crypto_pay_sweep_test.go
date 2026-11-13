package controller

// The sweep of a per-address coin's issued addresses (crypto_pay_sweep.go)
// against a fake reader with its database in memory, and the ETH and SOL
// readers' reads of one issued address against their recorded chains. No
// database.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The sweep's tables in memory: the issued addresses in pool index order,
// their cursors and the rotation position.
type fakeCryptoSweep struct {
	issued    []*model.CryptoAddressSweepAddress
	cursors   map[string]*model.CryptoAddressSweepCursor
	position  int64
	listCalls int
}

// Issues count addresses at pool indexes 10, 11, ... and replaces the
// sweep's reads and writes with them for the rest of the test.
func newFakeCryptoSweep(t *testing.T, count int) *fakeCryptoSweep {
	sweep := &fakeCryptoSweep{cursors: map[string]*model.CryptoAddressSweepCursor{}, position: -1}
	for i := range count {
		sweep.issued = append(sweep.issued, &model.CryptoAddressSweepAddress{
			Address:      fmt.Sprintf("sweep-%d", 10+i),
			AddressIndex: int64(10 + i),
			IntentId:     server.NewId(),
		})
	}
	previousPosition := cryptoSweepPosition
	previousSetPosition := cryptoSweepSetPosition
	previousAddresses := cryptoSweepAddresses
	previousSetCursor := cryptoSweepSetCursor
	cryptoSweepPosition = func(ctx context.Context, asset string) int64 {
		return sweep.position
	}
	cryptoSweepSetPosition = func(ctx context.Context, asset string, afterAddressIndex int64, now time.Time) {
		sweep.position = afterAddressIndex
	}
	cryptoSweepAddresses = func(ctx context.Context, asset string, afterAddressIndex int64, limit int) []*model.CryptoAddressSweepAddress {
		sweep.listCalls += 1
		listed := []*model.CryptoAddressSweepAddress{}
		for _, address := range sweep.issued {
			if afterAddressIndex < address.AddressIndex && len(listed) < limit {
				copied := *address
				if cursor, ok := sweep.cursors[address.Address]; ok {
					copiedCursor := *cursor
					copied.Cursor = &copiedCursor
				}
				listed = append(listed, &copied)
			}
		}
		return listed
	}
	cryptoSweepSetCursor = func(ctx context.Context, cursor *model.CryptoAddressSweepCursor, now time.Time) {
		copied := *cursor
		sweep.cursors[cursor.Address] = &copied
	}
	t.Cleanup(func() {
		cryptoSweepPosition = previousPosition
		cryptoSweepSetPosition = previousSetPosition
		cryptoSweepAddresses = previousAddresses
		cryptoSweepSetCursor = previousSetCursor
	})
	return sweep
}

// Reads issued addresses: each read spends one request and moves a counter
// cursor on by one, unless the test answers for the address.
type fakeCryptoSweeper struct {
	reads   []string
	answers map[string]func(address *model.CryptoAddressSweepAddress, spend func() bool) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error)
}

func (self *fakeCryptoSweeper) SweepAddress(
	ctx context.Context,
	address *model.CryptoAddressSweepAddress,
	settings *CryptoAddressSweepSettings,
	spend func() bool,
) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error) {
	self.reads = append(self.reads, address.Address)
	if answer, ok := self.answers[address.Address]; ok {
		return answer(address, spend)
	}
	if !spend() {
		return nil, nil, false, nil
	}
	height := int64(1)
	if address.Cursor != nil && address.Cursor.Height != nil {
		height = *address.Cursor.Height + 1
	}
	return []*cryptoWalletReceipt{}, &model.CryptoAddressSweepCursor{Height: &height}, true, nil
}

// Two addresses read a scan, each once a day.
func cryptoTestSweepSettings() *CryptoAddressSweepSettings {
	settings := DefaultCryptoAddressSweepSettings()
	settings.ReadsPerScan = 2
	return settings
}

// Runs one scan's sweep and returns its result and the receipts it
// recorded.
func cryptoTestSweep(sweeper *fakeCryptoSweeper, settings *CryptoAddressSweepSettings, now time.Time) (*CryptoPaymentScanResult, []*cryptoWalletReceipt) {
	result := &CryptoPaymentScanResult{}
	recorded := []*cryptoWalletReceipt{}
	cryptoSweepIssuedAddresses(context.Background(), model.CryptoAssetEth, sweeper, settings, settings.ReadEvery, now, func(receipts []*cryptoWalletReceipt) {
		recorded = append(recorded, receipts...)
	}, result)
	return result, recorded
}

// Every issued address is read in turn, a scan's worth at a time, each from
// its own cursor, and again once its read is ReadEvery old; between reads
// the rotation passes over the addresses that are not due.
func TestCryptoSweepReadsEveryIssuedAddressInTurn(t *testing.T) {
	sweep := newFakeCryptoSweep(t, 5)
	sweeper := &fakeCryptoSweeper{answers: map[string]func(*model.CryptoAddressSweepAddress, func() bool) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error){
		"sweep-12": func(address *model.CryptoAddressSweepAddress, spend func() bool) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error) {
			spend()
			height := int64(7)
			return []*cryptoWalletReceipt{{TxId: "late", AddressIndex: cryptoIndexOf(12), AmountAtomic: 5}}, &model.CryptoAddressSweepCursor{Height: &height}, true, nil
		},
	}}
	settings := cryptoTestSweepSettings()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	result, recorded := cryptoTestSweep(sweeper, settings, now)
	connect.AssertEqual(t, sweeper.reads, []string{"sweep-10", "sweep-11"})
	connect.AssertEqual(t, result.Swept, 2)
	connect.AssertEqual(t, sweep.position, int64(11))
	connect.AssertEqual(t, len(recorded), 0)
	connect.AssertEqual(t, *sweep.cursors["sweep-10"].Height, int64(1))
	connect.AssertEqual(t, sweep.cursors["sweep-10"].ReadAt.Equal(now), true)
	connect.AssertEqual(t, sweep.cursors["sweep-10"].Asset, model.CryptoAssetEth)

	// the next scan goes on from the position; a payment found is recorded
	result, recorded = cryptoTestSweep(sweeper, settings, now.Add(30*time.Second))
	connect.AssertEqual(t, sweeper.reads[2:], []string{"sweep-12", "sweep-13"})
	connect.AssertEqual(t, len(recorded), 1)
	connect.AssertEqual(t, recorded[0].TxId, "late")
	connect.AssertEqual(t, result.SweptReceipts, 1)
	connect.AssertEqual(t, *sweep.cursors["sweep-12"].Height, int64(7))

	cryptoTestSweep(sweeper, settings, now.Add(time.Minute))
	connect.AssertEqual(t, sweeper.reads[4:], []string{"sweep-14"})
	connect.AssertEqual(t, sweep.position, int64(14))

	// the rotation wraps; nothing is due within the day, so nothing is read
	// and the position passes over all of them
	for range 3 {
		result, _ = cryptoTestSweep(sweeper, settings, now.Add(time.Hour))
		connect.AssertEqual(t, result.Swept, 0)
	}
	connect.AssertEqual(t, len(sweeper.reads), 5)

	// a day after its read an address is due again, from its cursor
	cryptoTestSweep(sweeper, settings, now.Add(24*time.Hour+time.Second))
	cryptoTestSweep(sweeper, settings, now.Add(24*time.Hour+time.Second))
	connect.AssertEqual(t, sweeper.reads[5:], []string{"sweep-10", "sweep-11"})
	connect.AssertEqual(t, *sweep.cursors["sweep-10"].Height, int64(2))
}

// A read the request budget stops is checkpointed where it stopped: what it
// read is recorded, its cursor kept, its last read time unchanged, and the
// position stays before it, so the next scan goes on with the same address.
func TestCryptoSweepCheckpointsAPartialRead(t *testing.T) {
	sweep := newFakeCryptoSweep(t, 3)
	signatures := []string{}
	sweeper := &fakeCryptoSweeper{answers: map[string]func(*model.CryptoAddressSweepAddress, func() bool) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error){
		"sweep-11": func(address *model.CryptoAddressSweepAddress, spend func() bool) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error) {
			from := ""
			if address.Cursor != nil {
				from = address.Cursor.Signature
			}
			signatures = append(signatures, from)
			if from == "" {
				spend()
				return []*cryptoWalletReceipt{{TxId: "first"}}, &model.CryptoAddressSweepCursor{Signature: "first"}, false, nil
			}
			spend()
			return []*cryptoWalletReceipt{{TxId: "second"}}, &model.CryptoAddressSweepCursor{Signature: "second"}, true, nil
		},
	}}
	settings := cryptoTestSweepSettings()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	result, recorded := cryptoTestSweep(sweeper, settings, now)
	connect.AssertEqual(t, sweeper.reads, []string{"sweep-10", "sweep-11"})
	connect.AssertEqual(t, len(recorded), 1)
	connect.AssertEqual(t, result.Swept, 1)
	connect.AssertEqual(t, sweep.position, int64(10))
	connect.AssertEqual(t, sweep.cursors["sweep-11"].Signature, "first")
	connect.AssertEqual(t, sweep.cursors["sweep-11"].ReadAt == nil, true)

	_, recorded = cryptoTestSweep(sweeper, settings, now.Add(30*time.Second))
	connect.AssertEqual(t, sweeper.reads[2:], []string{"sweep-11", "sweep-12"})
	connect.AssertEqual(t, signatures, []string{"", "first"})
	connect.AssertEqual(t, recorded[0].TxId, "second")
	connect.AssertEqual(t, sweep.cursors["sweep-11"].Signature, "second")
	connect.AssertEqual(t, sweep.cursors["sweep-11"].ReadAt != nil, true)
	connect.AssertEqual(t, sweep.position, int64(12))
}

// A failed read keeps what it read before the failure (its receipts are
// recorded and its cursor saved with the error), moves the position past it
// so one failing address never holds the rotation, and ends the sweep for the
// scan. The address is read again when the rotation comes back to it, even
// before ReadEvery.
func TestCryptoSweepFailureMovesOnAndIsReadAgain(t *testing.T) {
	sweep := newFakeCryptoSweep(t, 4)
	fail := true
	sweeper := &fakeCryptoSweeper{answers: map[string]func(*model.CryptoAddressSweepAddress, func() bool) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error){
		"sweep-11": func(address *model.CryptoAddressSweepAddress, spend func() bool) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error) {
			spend()
			if fail {
				return []*cryptoWalletReceipt{{TxId: "before-failure"}}, &model.CryptoAddressSweepCursor{Signature: "before-failure"}, false, errors.New("getTransaction: http 502")
			}
			return []*cryptoWalletReceipt{}, &model.CryptoAddressSweepCursor{Signature: "after"}, true, nil
		},
	}}
	settings := cryptoTestSweepSettings()
	settings.ReadsPerScan = 10
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	result, recorded := cryptoTestSweep(sweeper, settings, now)
	connect.AssertEqual(t, sweeper.reads, []string{"sweep-10", "sweep-11"})
	connect.AssertEqual(t, result.SweepErrors, 1)
	connect.AssertEqual(t, len(recorded), 1)
	connect.AssertEqual(t, recorded[0].TxId, "before-failure")
	connect.AssertEqual(t, sweep.cursors["sweep-11"].Signature, "before-failure")
	connect.AssertEqual(t, sweep.cursors["sweep-11"].LastError, "getTransaction: http 502")
	connect.AssertEqual(t, sweep.cursors["sweep-11"].ReadAt == nil, true)
	connect.AssertEqual(t, sweep.position, int64(11))

	// the next scan goes on past it; the one after wraps and reads it again
	fail = false
	cryptoTestSweep(sweeper, settings, now.Add(30*time.Second))
	connect.AssertEqual(t, sweeper.reads[2:], []string{"sweep-12", "sweep-13"})
	cryptoTestSweep(sweeper, settings, now.Add(time.Minute))
	connect.AssertEqual(t, sweeper.reads[4:], []string{"sweep-11"})
	connect.AssertEqual(t, sweep.cursors["sweep-11"].Signature, "after")
	connect.AssertEqual(t, sweep.cursors["sweep-11"].LastError, "")
	connect.AssertEqual(t, sweep.cursors["sweep-11"].ReadAt != nil, true)
}

// A scan's sweep never makes more chain requests than RequestsPerScan, nor
// reads more addresses than ReadsPerScan, nor looks at more issued addresses
// than ExamineLimit; non-positive settings sweep nothing.
func TestCryptoSweepIsBounded(t *testing.T) {
	sweep := newFakeCryptoSweep(t, 20)
	sweeper := &fakeCryptoSweeper{}
	settings := cryptoTestSweepSettings()
	settings.ReadsPerScan = 10
	settings.RequestsPerScan = 3
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	cryptoTestSweep(sweeper, settings, now)
	connect.AssertEqual(t, len(sweeper.reads), 3)
	connect.AssertEqual(t, sweep.position, int64(12))

	settings.RequestsPerScan = 40
	settings.ExamineLimit = 4
	cryptoTestSweep(sweeper, settings, now)
	connect.AssertEqual(t, sweeper.reads[3:], []string{"sweep-13", "sweep-14", "sweep-15", "sweep-16"})
	connect.AssertEqual(t, sweep.position, int64(16))

	listCalls := sweep.listCalls
	for _, off := range []*CryptoAddressSweepSettings{
		{ExamineLimit: 0, ReadsPerScan: 10, RequestsPerScan: 40},
		{ExamineLimit: 200, ReadsPerScan: 0, RequestsPerScan: 40},
		{ExamineLimit: 200, ReadsPerScan: 10, RequestsPerScan: 0},
	} {
		cryptoTestSweep(sweeper, off, now)
	}
	connect.AssertEqual(t, sweep.listCalls, listCalls)
	connect.AssertEqual(t, len(sweeper.reads), 7)
}

// A coin whose provider webhook watches its addresses (ETH with the Address
// Activity webhook) reads each once a day; one no webhook watches (SOL, or
// ETH without the webhook) hourly.
func TestCryptoSweepReadEveryFollowsTheWebhook(t *testing.T) {
	settings := DefaultCryptoAddressSweepSettings()
	watched := newEthereumWallet(&cryptoWalletSettings{Asset: model.CryptoAssetEth, AlchemyWebhookId: "wh_synthetic", AlchemyNotifyToken: "synthetic"}, &CryptoPayAssetConfig{Network: cryptoNetworkMainnet})
	unwatched := newEthereumWallet(&cryptoWalletSettings{Asset: model.CryptoAssetEth}, &CryptoPayAssetConfig{Network: cryptoNetworkMainnet})
	connect.AssertEqual(t, cryptoSweepReadEvery(watched, settings), 24*time.Hour)
	connect.AssertEqual(t, cryptoSweepReadEvery(unwatched, settings), time.Hour)
	connect.AssertEqual(t, cryptoSweepReadEvery(newSolanaTestWallet(t, newFakeSolanaRpc(t)), settings), time.Hour)
	connect.AssertEqual(t, cryptoSweepReadEvery(newFakeCryptoWallet(model.CryptoAssetXmr), settings), time.Hour)
}

// The ETH reader reads an issued address's transfers after its cursor
// through the finalized block in one request, and moves the cursor to that
// block; with nothing finalized since, it asks nothing.
func TestCryptoEthereumSweepReadsAnAddressFromItsCursor(t *testing.T) {
	restore := cryptoTestHermeticSeams()
	t.Cleanup(restore)
	_, wallet := cryptoTestEthereum(t)
	spent := 0
	spend := func() bool {
		spent += 1
		return true
	}
	address := &model.CryptoAddressSweepAddress{Address: "0xd8da6bf26964af9d7eed9e03e53415d37aa96045", AddressIndex: 3}
	receipts, next, complete, err := wallet.SweepAddress(context.Background(), address, DefaultCryptoAddressSweepSettings(), spend)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, spent, 1)
	connect.AssertEqual(t, len(receipts), 2)
	finalized, _, _, err := wallet.heads(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, *next.Height, int64(finalized))
	for _, receipt := range receipts {
		connect.AssertEqual(t, receipt.OutputKey, "3")
		connect.AssertEqual(t, receipt.Confirmations, uint64(1))
	}

	// read again from the finalized block: nothing new, no request
	address.Cursor = next
	receipts, again, complete, err := wallet.SweepAddress(context.Background(), address, DefaultCryptoAddressSweepSettings(), spend)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, spent, 1)
	connect.AssertEqual(t, len(receipts), 0)
	connect.AssertEqual(t, *again.Height, int64(finalized))

	// from a cursor just before one transfer's block: only it
	block, err := cryptoHexUint64(cryptoTestFirstTransferBlock(t, wallet, address))
	connect.AssertEqual(t, err, nil)
	before := int64(block) - 1
	address.Cursor = &model.CryptoAddressSweepCursor{Height: &before}
	receipts, _, _, err = wallet.SweepAddress(context.Background(), address, DefaultCryptoAddressSweepSettings(), spend)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].BlockHeight, block)

	// a budget that refuses reads nothing
	address.Cursor = nil
	receipts, next, complete, err = wallet.SweepAddress(context.Background(), address, DefaultCryptoAddressSweepSettings(), func() bool { return false })
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, false)
	connect.AssertEqual(t, receipts == nil, true)
	connect.AssertEqual(t, next == nil, true)
}

// The block of the first of the address's recorded transfers, as a JSON-RPC
// quantity.
func cryptoTestFirstTransferBlock(t *testing.T, wallet *ethereumWallet, address *model.CryptoAddressSweepAddress) string {
	t.Helper()
	transfers, err := wallet.transfersTo(context.Background(), address.Address, "0x0", "latest")
	connect.AssertEqual(t, err, nil)
	if len(transfers) == 0 {
		t.Fatal("the recorded address has no transfers")
	}
	return transfers[0].BlockNum
}

// The SOL reader reads an issued address's finalized transactions newer than
// its cursor, oldest first, and reports its native SOL payments; the cursor is
// the newest examined. A budget that runs out stops it at the last
// transaction examined, and the next read goes on from there. A history
// longer than the page limit is an error, acted on not at all.
func TestSolanaNativeSweepReadsAnAddressFromItsCursor(t *testing.T) {
	rpc := newFakeSolanaRpc(t)
	rpc.signatures[solanaFixtureWallet] = "sol_signatures.json"
	rpc.transactions[solanaFixtureNativeSignature] = "sol_tx_native.json"
	rpc.transactions[solanaFixtureUsdcSignature] = "sol_tx_usdc.json"
	wallet := newSolanaTestWallet(t, rpc)
	settings := DefaultCryptoAddressSweepSettings()
	address := &model.CryptoAddressSweepAddress{Address: solanaFixtureWallet, AddressIndex: 7}
	requests := 0
	budget := 100
	spend := func() bool {
		if budget <= requests {
			return false
		}
		requests += 1
		return true
	}

	// the first read: the whole history, one page; the failed transaction is
	// never fetched, the USDC one pays no SOL
	receipts, next, complete, err := wallet.SweepAddress(context.Background(), address, settings, spend)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, requests, 3)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].TxId, solanaFixtureNativeSignature)
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(108_600))
	connect.AssertEqual(t, receipts[0].OutputKey, "7")
	connect.AssertEqual(t, next.Signature, solanaFixtureNativeSignature)
	connect.AssertEqual(t, *next.Height, int64(467670518))

	// read again from the newest: one listing, nothing new
	address.Cursor = next
	receipts, again, complete, err := wallet.SweepAddress(context.Background(), address, settings, spend)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, requests, 4)
	connect.AssertEqual(t, len(receipts), 0)
	connect.AssertEqual(t, again.Signature, solanaFixtureNativeSignature)

	// a budget of two: the listing and the oldest transaction, then a stop
	// at the failed one (examined without a request); the next read lists
	// from there and reads the payment
	solanaParsedTransactions.reset()
	address.Cursor = nil
	requests, budget = 0, 2
	receipts, next, complete, err = wallet.SweepAddress(context.Background(), address, settings, spend)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, false)
	connect.AssertEqual(t, len(receipts), 0)
	connect.AssertEqual(t, next.Signature != solanaFixtureNativeSignature, true)
	connect.AssertEqual(t, next.Signature != solanaFixtureUsdcSignature, true)
	address.Cursor = next
	requests, budget = 0, 100
	receipts, next, complete, err = wallet.SweepAddress(context.Background(), address, settings, spend)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].TxId, solanaFixtureNativeSignature)
	connect.AssertEqual(t, next.Signature, solanaFixtureNativeSignature)

	// more history than the pages: an error, nothing acted on
	address.Cursor = nil
	settings.PageSize = 1
	settings.MaxPages = 2
	receipts, next, complete, err = wallet.SweepAddress(context.Background(), address, settings, spend)
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, complete, false)
	connect.AssertEqual(t, receipts == nil, true)
	connect.AssertEqual(t, next == nil, true)
}
