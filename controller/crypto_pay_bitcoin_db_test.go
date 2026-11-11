package controller

// The Bitcoin block scanner end to end, through RunCryptoPaymentScan and the
// database, over the recorded mainnet blocks of crypto_pay_bitcoin_test.go's
// fake node: every scan builds its reader afresh, as each scan on any
// taskworker does, and starts from the chain the last complete scan kept with
// the cursor. Needs the test database (server.DefaultTestEnv) and pro.yml.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// cryptoDbBitcoinScanner is Bitcoin on, credited at depth 2 and final at 6,
// read from the fake node, with clean sanctions screens and rates.
func cryptoDbBitcoinScanner(t testing.TB, node *cryptoTestBitcoinNode) {
	endpoint := node.serve()
	config := cryptoCoinFlowConfig(model.CryptoAssetBtc)
	config.Assets[model.CryptoAssetBtc].CreditConfirmations = 2
	config.Assets[model.CryptoAssetBtc].FinalConfirmations = 6
	config.Assets[model.CryptoAssetBtc].ReorgWindowBlocks = 6
	restoreConfig := setCryptoPayConfigForTest(config)
	previousSettings := cryptoWalletSettingsFunc
	cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
		if asset != model.CryptoAssetBtc {
			return nil, fmt.Errorf("%s has no chain source in this test", asset)
		}
		return &cryptoWalletSettings{Asset: model.CryptoAssetBtc, ChainRpcUrl: endpoint}, nil
	}
	cryptoTestRates(t, map[string]map[string]float64{
		cryptoRateSourceKraken:   {model.CryptoAssetBtc: 1_000_000},
		cryptoRateSourceCoinbase: {model.CryptoAssetBtc: 1_000_000},
	})
	cryptoTestSanctions(t, map[string]bool{}, nil)
	resetCryptoAvailability()
	t.Cleanup(func() {
		restoreConfig()
		cryptoWalletSettingsFunc = previousSettings
		resetCryptoAvailability()
	})
}

// cryptoDbBitcoinIntent is an open Bitcoin invoice of amountAtomic at a
// Coinbase deposit address, for a network of its own.
func cryptoDbBitcoinIntent(t testing.TB, ctx context.Context, name string, item string, index int64, address string, amountAtomic int64) (*model.CryptoPaymentIntent, server.Id) {
	networkId, _, _ := cryptoDbTestNetwork(t, ctx, name)
	intent := cryptoDbCreateCoinbaseIntent(t, ctx, model.CryptoAssetBtc, &networkId, item, 40, index, address)
	cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET amount_atomic = $2 WHERE intent_id = $1`, intent.IntentId, amountAtomic)
	return model.GetCryptoPaymentIntent(ctx, intent.IntentId), networkId
}

func cryptoDbBitcoinScan(t testing.TB, taskSession *session.ClientSession) *CryptoPaymentScanResult {
	scan, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetBtc)
	connect.AssertEqual(t, err, nil)
	if scan.Error != "" {
		t.Fatalf("btc scan: %s", scan.Error)
	}
	return scan
}

func cryptoDbBitcoinStatus(t testing.TB, ctx context.Context, intentId server.Id) *cryptoIntentView {
	intent := model.GetCryptoPaymentIntent(ctx, intentId)
	return cryptoEvaluateIntent(intent, model.GetCryptoPaymentReceiptsForIntent(ctx, intentId), cryptoIntentPolicyFor(cryptoPayConfigFunc(), model.CryptoAssetBtc), server.NowUtc())
}

// A payment in two parts, in two blocks, through the scanner and the
// database: seen while short, confirming once whole, credited at the coin's
// depth (2), final at 6. Each scan reads only the blocks after the chain the
// last one kept with the cursor; a scan with no new block makes one call.
func TestCryptoBitcoinScannerCreditsAtDepth(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		node := newCryptoTestBitcoinNode(t, 975400)
		node.shift = time.Now().Unix() - 1794381078 - 60
		cryptoDbBitcoinScanner(t, node)
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		intent, networkId := cryptoDbBitcoinIntent(t, ctx, "btcdepth", model.SolanaPlanYearly, 701, cryptoTestBtcP2wpkhTwice, 2_640_000)

		// 975400: the first part, alone
		scan := cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, scan.NewReceipts, 1)
		connect.AssertEqual(t, scan.WalletHeight, uint64(975400))
		connect.AssertEqual(t, node.callCount("getblock"), 7)
		view := cryptoDbBitcoinStatus(t, ctx, intent.IntentId)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusSeen)
		connect.AssertEqual(t, view.RemainingAtomic, int64(1_320_000))

		// 975401: no part, the first one deeper
		node.setTip(975401)
		scan = cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, node.callCount("getblock"), 8)
		connect.AssertEqual(t, cryptoDbBitcoinStatus(t, ctx, intent.IntentId).Status, CryptoPaymentStatusSeen)
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, len(receipts), 1)
		connect.AssertEqual(t, receipts[0].Confirmations, 2)
		connect.AssertEqual(t, *receipts[0].BlockHeight, int64(975400))

		// 975402: the second part makes it whole, at 1 confirmation
		node.setTip(975402)
		scan = cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, scan.NewReceipts, 1)
		connect.AssertEqual(t, scan.Credited, 0)
		view = cryptoDbBitcoinStatus(t, ctx, intent.IntentId)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusConfirming)
		connect.AssertEqual(t, view.Confirmations, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		// a scan with no new block: one call, nothing new
		calls := node.totalCalls()
		scan = cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, node.totalCalls(), calls+1)
		connect.AssertEqual(t, scan.NewReceipts, 0)

		// 975403: 2 confirmations, credited
		node.mine()
		scan = cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, scan.Credited, 1)
		connect.AssertEqual(t, cryptoDbBitcoinStatus(t, ctx, intent.IntentId).Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// final at 6: 975400's at 975405, 975402's at 975407
		for range 4 {
			node.mine()
			cryptoDbBitcoinScan(t, taskSession)
		}
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId) {
			connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateCredited)
			connect.AssertEqual(t, receipt.FinalAt != nil, true)
		}

		// the cursor row: the tip read, its hash, the kept chain
		chain := model.GetCryptoScanChainInConn(nil, ctx, model.CryptoAssetBtc)
		connect.AssertEqual(t, chain.Height, int64(975407))
		connect.AssertEqual(t, chain.BlockHash, node.chain[975407])
		var kept bitcoinChain
		connect.AssertEqual(t, json.Unmarshal([]byte(chain.RecentBlocks), &kept), nil)
		connect.AssertEqual(t, len(kept.Blocks), 14)
		connect.AssertEqual(t, kept.tip().Hash, node.chain[975407])
		cursor := model.GetCryptoScanCursor(ctx, model.CryptoAssetBtc)
		connect.AssertEqual(t, cursor.Height, int64(975407))
		connect.AssertEqual(t, *cursor.WalletHeight, int64(975407))
		connect.AssertEqual(t, cursor.LastError, "")
	})
}

// A reorg: a payment credited before it stays credited whatever the new
// chain holds, and the reconciler ends nothing of it (it reports it); a
// payment below the coin's depth is judged again, back in the mempool (0
// confirmations, not credited), and credited once the new chain holds it
// deep enough. The P2SH invoice address is the kind Coinbase issued for BTC
// on 2026-11-11.
func TestCryptoBitcoinReorgKeepsCreditsAndJudgesTheRestAgain(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		node := newCryptoTestBitcoinNode(t, 975402)
		node.shift = time.Now().Unix() - 1794381078 - 60
		cryptoDbBitcoinScanner(t, node)
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		credited, creditedNetworkId := cryptoDbBitcoinIntent(t, ctx, "btcreorgcredited", model.SolanaPlanYearly, 711, cryptoTestBtcP2sh, 700_000)
		pending, pendingNetworkId := cryptoDbBitcoinIntent(t, ctx, "btcreorgpending", model.SolanaPlanYearly, 712, cryptoTestBtcP2wpkhLarge, 157_049_473)

		// 975400 to 975402: the P2SH payment 3 deep, credited; the other 1
		// deep, confirming
		scan := cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, scan.NewReceipts, 2)
		connect.AssertEqual(t, scan.Credited, 1)
		connect.AssertEqual(t, cryptoDbBitcoinStatus(t, ctx, credited.IntentId).Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, cryptoDbBitcoinStatus(t, ctx, pending.IntentId).Status, CryptoPaymentStatusConfirming)
		connect.AssertEqual(t, model.IsProNetwork(ctx, creditedNetworkId), true)

		// a three-block reorg from 975400: the credited payment's transaction
		// is in no block of the new chain and not in the mempool; the other's
		// is back in the mempool
		node.fork(975400, "reorg", cryptoTestBtcTxSplit, cryptoTestBtcTxLarge)
		node.stateLock.Lock()
		node.mempool[cryptoTestBtcTxLarge] = true
		node.stateLock.Unlock()
		scan = cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, scan.WalletHeight, uint64(975403))
		connect.AssertEqual(t, scan.Credited, 0)
		// credited stays credited
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, credited.IntentId).CreditedAt != nil, true)
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, credited.IntentId) {
			connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateCredited)
		}
		connect.AssertEqual(t, cryptoDbBitcoinStatus(t, ctx, credited.IntentId).Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, model.IsProNetwork(ctx, creditedNetworkId), true)
		// below the depth: judged again, in the mempool, not credited
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, pending.IntentId)
		connect.AssertEqual(t, len(receipts), 1)
		connect.AssertEqual(t, receipts[0].State, model.CryptoReceiptStateSeen)
		connect.AssertEqual(t, receipts[0].BlockHeight, (*int64)(nil))
		connect.AssertEqual(t, receipts[0].Confirmations, 0)
		view := cryptoDbBitcoinStatus(t, ctx, pending.IntentId)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusConfirming)
		connect.AssertEqual(t, view.Confirmations, 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, pendingNetworkId), false)

		// mined again in the new chain, then one deeper: credited
		node.mine(cryptoTestBtcTxLarge)
		scan = cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, scan.Credited, 0)
		receipts = model.GetCryptoPaymentReceiptsForIntent(ctx, pending.IntentId)
		connect.AssertEqual(t, receipts[0].State, model.CryptoReceiptStateConfirmed)
		connect.AssertEqual(t, *receipts[0].BlockHeight, int64(975404))
		node.mine()
		scan = cryptoDbBitcoinScan(t, taskSession)
		connect.AssertEqual(t, scan.Credited, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, pendingNetworkId), true)

		// the final depth past the gone payment: the reconciler finds it gone
		// and ends nothing, an error for an operator
		node.mine()
		cryptoDbBitcoinScan(t, taskSession)
		cryptoDbExec(ctx, `UPDATE crypto_payment_receipt SET update_time = $2 WHERE intent_id = $1`, credited.IntentId, server.NowUtc().Add(-3*time.Hour))
		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketBitcoin}},
		)
		connect.AssertEqual(t, err, nil)
		storeResult := result.StoreResults[model.SubscriptionMarketBitcoin]
		connect.AssertEqual(t, storeResult.Ended, 0)
		connect.AssertEqual(t, storeResult.Errors, 1)
		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketBitcoin, model.PaymentReconcileActionEnded), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, creditedNetworkId), true)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, credited.IntentId).CreditedAt != nil, true)
	})
}

// The one indexed lookup a block: which of a block's output addresses are
// invoice addresses in play as of the block's time. Open, or closed within
// the watch window before the block, or with a receipt still in play; never
// unlinked, never made after the block was mined; among many open invoices
// and many addresses, in one query.
func TestCryptoBitcoinInvoiceAddressesInPlay(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoDbBitcoinScanner(t, newCryptoTestBitcoinNode(t, 975402))
		networkId, _, _ := cryptoDbTestNetwork(t, ctx, "btcinplay")
		now := server.NowUtc()
		address := func(i int) string {
			return fmt.Sprintf("bc1qsyntheticinplay%05d", i)
		}
		for i := range 400 {
			cryptoDbCreateCoinbaseIntent(t, ctx, model.CryptoAssetBtc, &networkId, StripeItemData1Tib, 3, int64(2000+i), address(i))
		}
		// open (in play): 0 to 399; then adjust a few
		set := func(i int, column string, value time.Time) {
			cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET `+column+` = $2 WHERE asset = 'btc' AND address = $1`, address(i), value)
		}
		set(1, "expires_at", now.Add(-time.Hour))     // expired an hour ago: in play
		set(2, "expires_at", now.Add(-48*time.Hour))  // expired two days ago: not
		set(3, "credited_at", now.Add(-2*time.Hour))  // credited two hours ago: in play
		set(4, "credited_at", now.Add(-48*time.Hour)) // credited two days ago: not
		set(4, "expires_at", now.Add(-48*time.Hour))  //
		set(5, "unlinked_at", now.Add(-time.Minute))  // unlinked: not
		set(6, "create_time", now.Add(4*time.Hour))   // made after the block: not
		set(7, "voided_at", now.Add(-time.Hour))      // voided an hour ago: in play
		set(8, "expires_at", now.Add(-48*time.Hour))  // expired long ago, but
		inPlayReceipt := model.GetCryptoPaymentIntent(ctx, func() server.Id {
			var intentId server.Id
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT intent_id FROM crypto_payment_intent WHERE address = $1`, address(8)).Scan(&intentId))
			})
			return intentId
		}())
		// ... a receipt of it still unconfirmed: in play
		cryptoDbReceipt(t, ctx, inPlayReceipt, "btc-inplay-unconfirmed", 1, 0)

		block := []string{}
		for i := range 2000 {
			block = append(block, fmt.Sprintf("bc1qsyntheticnotours%05d", i))
		}
		for i := range 10 {
			block = append(block, address(i))
		}
		matched := cryptoBitcoinInvoiceAddresses(ctx, block, now)
		want := map[string]int64{}
		for _, i := range []int{0, 1, 3, 7, 8, 9} {
			want[address(i)] = int64(2000 + i)
		}
		connect.AssertEqual(t, matched, want)
		// a block mined three days ago is matched as of its own time: the
		// invoice that expired two days ago was open then, and those made since
		// are not in play for it
		matched = model.GetCryptoPaymentInvoiceAddressesInPlayInConn(nil, ctx, model.CryptoAssetBtc, []string{address(2), address(0)}, now.Add(-72*time.Hour-24*time.Hour), now.Add(-72*time.Hour+cryptoBitcoinClockTolerance))
		connect.AssertEqual(t, matched, map[string]int64{})
		connect.AssertEqual(t, len(model.GetCryptoPaymentInvoiceAddressesInPlayInConn(nil, ctx, model.CryptoAssetBtc, []string{}, now, now)), 0)
	})
}
