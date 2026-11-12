package controller

// Root-cause tests, against the database, of support inbox 7334: a purchase
// must never land on a legacy guest network, which nothing can sign back in
// to. The Pro checkouts and intents refused a guest (dd1e72668); the data and
// agent purchases below still sold to one. Each runs against a real legacy
// guest network (no row in any auth table), then once more after a sign-in
// is added in place, when it gets past the guard. Needs the test database and
// redis (server.DefaultTestEnv).

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type purchaseGuestDbNetwork struct {
	networkId     server.Id
	userId        server.Id
	networkName   string
	clientSession *session.ClientSession
}

func newPurchaseGuestDbNetwork(t testing.TB, ctx context.Context) *purchaseGuestDbNetwork {
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateLegacyGuestNetwork(ctx, networkId, userId)
	networkName := "g" + networkId.String()
	return &purchaseGuestDbNetwork{
		networkId:   networkId,
		userId:      userId,
		networkName: networkName,
		// a refreshed legacy guest: the jwt no longer carries GuestMode
		clientSession: session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: networkName,
		}),
	}
}

// addSignIn converts the guest in place, as the apps do.
func (self *purchaseGuestDbNetwork) addSignIn(t testing.TB) {
	userAuth := fmt.Sprintf("guest-gaps-%s@example.com", self.networkId)
	password := "SomeValidPassword123!"
	result, err := model.AddAuth(model.AddAuthMethod{UserAuth: &userAuth, Password: &password}, self.clientSession)
	if err != nil || result == nil || result.Error != nil {
		t.Fatalf("AddAuth result=%+v err=%v", result, err)
	}
}

// The buy-data pages' purchases for a network named by the buyer, and a data
// pack with crypto for the caller's own network.
func TestGuestNetworkRefusedForDataPurchases(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		guest := newPurchaseGuestDbNetwork(t, ctx)
		// the buy-data page: no sign-in
		anonymous := session.Testing_CreateClientSession(ctx, nil)

		refused := func(entry string, code string, message string, wantMessage string) {
			t.Helper()
			if code != PurchaseErrorCodeGuestSignInRequired || message != wantMessage {
				t.Fatalf("%s: code=%q message=%q, want the guest refusal", entry, code, message)
			}
		}
		notRefused := func(entry string, code string) {
			t.Helper()
			if code == PurchaseErrorCodeGuestSignInRequired {
				t.Fatalf("%s: refused as a guest after the sign-in was added", entry)
			}
		}

		// card, for the guest network by name
		checkout, err := PayDataCheckout(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib, NetworkName: guest.networkName}, anonymous)
		connect.AssertEqual(t, err, nil)
		if checkout.Error == nil {
			t.Fatalf("card data for the guest network: %+v, want a refusal", checkout)
		}
		refused("card data by name", checkout.Error.Code, checkout.Error.Message, purchaseGuestNetworkMessage)

		// USDC by Solana Pay, for the guest network by name
		reference := "guestgaps" + strings.ReplaceAll(server.NewId().String(), "-", "")
		solana, err := PayDataSolanaIntent(&PayDataSolanaIntentArgs{ItemId: StripeItemData1Tib, NetworkName: guest.networkName, Reference: reference}, anonymous)
		connect.AssertEqual(t, err, nil)
		if solana.Error == nil {
			t.Fatalf("USDC data for the guest network: %+v, want a refusal", solana)
		}
		refused("USDC data by name", solana.Error.Code, solana.Error.Message, purchaseGuestNetworkMessage)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, reference), (*model.SolanaPaymentIntent)(nil))

		// any coin, for the guest network by name
		named, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib, NetworkName: guest.networkName}, anonymous)
		connect.AssertEqual(t, err, nil)
		if named.Error == nil {
			t.Fatalf("crypto data for the guest network: %+v, want a refusal", named)
		}
		refused("crypto data by name", named.Error.Code, named.Error.Message, purchaseGuestNetworkMessage)

		// a data pack with crypto, signed in to the guest network
		own, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib}, guest.clientSession)
		connect.AssertEqual(t, err, nil)
		if own.Error == nil {
			t.Fatalf("crypto data for the guest's own network: %+v, want a refusal", own)
		}
		refused("crypto data signed in", own.Error.Code, own.Error.Message, purchaseGuestSignInRequiredMessage)

		// a data code is not refused: it is redeemed where its buyer chooses
		cryptoTestResetLimiters()
		code, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib}, guest.clientSession)
		connect.AssertEqual(t, err, nil)
		if code.Error != nil {
			notRefused("crypto data code", code.Error.Code)
		}

		guest.addSignIn(t)
		cryptoTestResetLimiters()

		checkout, err = PayDataCheckout(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib, NetworkName: guest.networkName}, anonymous)
		connect.AssertEqual(t, err, nil)
		if checkout.Error != nil {
			notRefused("card data by name", checkout.Error.Code)
		}
		solana, err = PayDataSolanaIntent(&PayDataSolanaIntentArgs{ItemId: StripeItemData1Tib, NetworkName: guest.networkName, Reference: reference}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, solana.Error, (*PayDataCheckoutError)(nil))
		named, err = CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib, NetworkName: guest.networkName}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, named.Error, (*CryptoPaymentError)(nil))
		own, err = CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib}, guest.clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, own.Error, (*CryptoPaymentError)(nil))
	})
}

// A Gamma data pack delivered to the guest network is refused; as a code it
// is not.
func TestGuestNetworkRefusedForGammaDataPack(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		f.run()
		guest := newPurchaseGuestDbNetwork(t, ctx)

		account := gammaDbIntent(t, guest.clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		if account.Error == nil || account.Error.Code != PurchaseErrorCodeGuestSignInRequired || account.Error.Message != purchaseGuestSignInRequiredMessage {
			t.Fatalf("Gamma data for the guest network: %+v, want the guest refusal", account)
		}
		code := gammaDbIntent(t, guest.clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode})
		if code.Error != nil && code.Error.Code == PurchaseErrorCodeGuestSignInRequired {
			t.Fatalf("a Gamma data code was refused as a guest purchase: %+v", code.Error)
		}

		guest.addSignIn(t)
		account = gammaDbIntent(t, guest.clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		if account.Error != nil && account.Error.Code == PurchaseErrorCodeGuestSignInRequired {
			t.Fatalf("Gamma data refused as a guest after the sign-in was added: %+v", account.Error)
		}
	})
}

// An agent paying with x402 on a guest network's credential is refused with
// 403 before the facilitator verifies or settles anything; with a sign-in it
// pays.
func TestGuestNetworkRefusedForX402(t *testing.T) {
	// the grant after a settlement, observed in memory
	installX402SettledNotGrantedFakes(t)
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		var facilitatorCalls atomic.Int32
		facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			facilitatorCalls.Add(1)
			switch r.URL.Path {
			case "/verify":
				w.Write([]byte(`{"isValid":true}`))
			default:
				w.Write([]byte(`{"success":true,"transaction":"0x6e7e57","network":"base"}`))
			}
		}))
		tb.Cleanup(facilitator.Close)
		x402ErrorTextConfig(tb, facilitator.URL)
		guest := newPurchaseGuestDbNetwork(tb, ctx)

		request := func(target string, body string) *http.Request {
			r := httptest.NewRequest(http.MethodPost, target, bytes.NewReader([]byte(body)))
			r.RemoteAddr = "192.0.2.54:4500"
			r.Header.Set("Authorization", "Bearer "+session.NewByJwt(guest.networkId, guest.userId, guest.networkName, false, false).Testing_Sign())
			payment, _ := x402CdpTestEvmPayment(tb, "base", x402CdpTestPayToBase, "10000000")
			r.Header.Set(X402V1PaymentHeader, payment)
			return r
		}

		w := httptest.NewRecorder()
		X402PurchaseHandler(w, request("/x402/purchase", `{"sku_id":"data_1tib"}`))
		requireX402Answer(tb, w, http.StatusForbidden, purchaseGuestSignInRequiredMessage)

		w = httptest.NewRecorder()
		if X402SettleInlineUpgrade(w, request("/network/auth-client", `{}`)) {
			tb.Fatal("the inline upgrade continued a guest's request")
		}
		requireX402Answer(tb, w, http.StatusForbidden, purchaseGuestSignInRequiredMessage)
		connect.AssertEqual(tb, facilitatorCalls.Load(), int32(0))

		guest.addSignIn(tb)
		w = httptest.NewRecorder()
		X402PurchaseHandler(w, request("/x402/purchase", `{"sku_id":"data_1tib"}`))
		if w.Code == http.StatusForbidden {
			tb.Fatalf("refused after the sign-in was added: %d %q", w.Code, w.Body.String())
		}
		if facilitatorCalls.Load() == 0 {
			tb.Fatal("the facilitator was never asked after the sign-in was added")
		}
	})
}
