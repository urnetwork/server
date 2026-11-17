package model

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// A one-time crypto renewal names its coin. USDC and native SOL share the
// solana market: a USDC renewal's transaction id is a Solana Pay reference,
// and a SOL year's is its invoice's id, then after the 30-day unlink a
// random id with the SOL prefix, so the term reads SOL for the whole year.
// (A linked SOL year's invoice id is looked up in the database:
// TestCryptoCoinFlowSolana.)
func TestCryptoRenewalAssetReadsTheCoin(t *testing.T) {
	ctx := context.Background()
	for market, asset := range map[string]string{
		SubscriptionMarketMonero:    CryptoAssetXmr,
		SubscriptionMarketZcash:     CryptoAssetZec,
		SubscriptionMarketBittensor: CryptoAssetTao,
		SubscriptionMarketBitcoin:   CryptoAssetBtc,
		SubscriptionMarketEthereum:  CryptoAssetEth,
	} {
		connect.AssertEqual(t, CryptoRenewalAsset(ctx, market, server.NewId().String()), asset)
		connect.AssertEqual(t, cryptoRenewalAssetOf(market, false), asset)
	}
	connect.AssertEqual(t, CryptoRenewalAsset(ctx, SubscriptionMarketSolana, "7B1tVMUE6zGyPFsx6fun6VoX1yCwfeX6D1r1CM3L1fct"), CryptoAssetUsdcSolana)
	connect.AssertEqual(t, CryptoRenewalAsset(ctx, SubscriptionMarketSolana, CryptoUnlinkedSolTransactionPrefix+server.NewId().String()), CryptoAssetSol)
	connect.AssertEqual(t, cryptoRenewalAssetOf(SubscriptionMarketSolana, true), CryptoAssetSol)
	connect.AssertEqual(t, cryptoRenewalAssetOf(SubscriptionMarketSolana, false), CryptoAssetUsdcSolana)
	// a store subscription has no coin
	connect.AssertEqual(t, CryptoRenewalAsset(ctx, SubscriptionMarketStripe, "in_1"), "")
	connect.AssertEqual(t, cryptoRenewalAssetOf(SubscriptionMarketApple, false), "")
	// the queries read the same two marks
	sql := cryptoRenewalSolSql("s")
	connect.AssertEqual(t, strings.Contains(sql, "s.transaction_id LIKE 'sol/%'"), true)
	connect.AssertEqual(t, strings.Contains(sql, "sol_intent.asset = 'sol'"), true)
	connect.AssertEqual(t, strings.Contains(sql, "s.market = 'solana'"), true)
}
