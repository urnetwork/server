package controller

import (
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Pure: every coin of the crypto rail posts a sale notice that names the coin
// and the amount it was quoted in. TAO, Bitcoin, ETH and native SOL posted
// "crypto" and the USD value alone.
func TestCryptoSalesNoticeNamesEveryCoin(t *testing.T) {
	for _, c := range []struct {
		asset        string
		amountAtomic int64
		amount       string
		method       string
	}{
		{model.CryptoAssetTao, 123_456_789, "0.123456789 TAO invoice (credited as 40.00 USD)", "TAO"},
		{model.CryptoAssetBtc, 51_234, "0.00051234 BTC invoice (credited as 40.00 USD)", "Bitcoin"},
		{model.CryptoAssetEth, 12_345_000_000_000_000, "0.012345 ETH invoice (credited as 40.00 USD)", "ETH"},
		{model.CryptoAssetSol, 250_000_000, "0.25 SOL invoice (credited as 40.00 USD)", "native SOL"},
	} {
		networkId := server.NewId()
		intent := &model.CryptoPaymentIntent{
			IntentId:          server.NewId(),
			Asset:             c.asset,
			Delivery:          model.CryptoDeliveryAccount,
			NetworkId:         &networkId,
			Item:              model.SolanaPlanYearly,
			PriceUsdNanoCents: model.UsdToNanoCents(40),
			AmountAtomic:      c.amountAtomic,
		}
		notice := cryptoSalesNotice(intent, &cryptoCredit{usdNanoCents: model.UsdToNanoCents(40)}, true, 0)
		connect.AssertEqual(t, salesNoticeLines(notice, "acme"), salesNoticeText("Pro yearly (one-time)", "acme", c.amount, c.method))
	}

	// the next coin the rail sells needs its name and ticker here too
	for _, asset := range cryptoPayAssets {
		if cryptoSalesMethod(asset) == "crypto" || cryptoSalesSymbol(asset) == "" {
			t.Errorf("a %s sale notice names no coin: method %q, symbol %q", asset, cryptoSalesMethod(asset), cryptoSalesSymbol(asset))
		}
	}
}
