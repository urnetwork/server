package controller

// The wallet, Circle transfer and pay-data routes' refusals declare their
// statuses, now that the router answers an undeclared error with a fixed 500.
// The apps show the wallet refusals' text as the reason a wallet was refused.

import (
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestCreateAccountWalletRefusesInvalidWalletsAsBadRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		clientSession := session.Testing_CreateClientSession(tb.Context(), &session.ByJwt{NetworkId: server.NewId()})
		defer clientSession.Cancel()
		_, err := CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{Blockchain: "BTC"}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "invalid blockchain, use SOL, MATIC, or TAO")
		_, err = CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{Blockchain: "SOL", WalletAddress: "1234"}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "invalid wallet address")
	})
}

func TestSetPayoutWalletRefusesUnpayableWalletsAsBadRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		networkId := server.NewId()
		clientSession := session.Testing_CreateClientSession(tb.Context(), &session.ByJwt{NetworkId: networkId})
		defer clientSession.Cancel()
		taoWalletId := model.CreateAccountWalletExternal(clientSession, &model.CreateAccountWalletExternalArgs{NetworkId: networkId, Blockchain: model.TAO.String(), WalletAddress: "synthetic-tao-address"})
		if taoWalletId == nil {
			tb.Fatal("no synthetic wallet")
		}
		_, err := SetPayoutWallet(model.SetPayoutWalletArgs{WalletId: *taoWalletId}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "Bittensor wallets cannot be the payout wallet.")
		_, err = SetPayoutWallet(model.SetPayoutWalletArgs{WalletId: server.NewId()}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "Wallet must be an active wallet owned by the network.")
	})
}

func TestCircleTransferRefusesMalformedRequestsAsBadRequest(t *testing.T) {
	f := newCustomerTransferFixture(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected external call")
	})
	_, err := WalletCircleTransferOut(&WalletCircleTransferOutArgs{Terms: true, ToAddress: "synthetic-destination", AmountUsdcNanoCents: 1000}, f.owner)
	requireRefusalAnswer(t, err, http.StatusBadRequest, "customer transfer requires a caller-retained request_id; reuse it for every retry")
	f.args.Terms = false
	_, err = WalletCircleTransferOut(f.args, f.owner)
	requireRefusalAnswer(t, err, http.StatusBadRequest, "transfer requires accepted terms and an exact bounded destination")
	f.args.Terms = true
	f.args.AmountUsdcNanoCents = 1001
	_, err = WalletCircleTransferOut(f.args, f.owner)
	requireRefusalAnswer(t, err, http.StatusBadRequest, "USDC transfer requires a positive exact six-decimal amount")
}

// A retry names the retained request with other terms.
func TestCircleTransferRefusesChangedTermsAsConflict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		f := newCustomerTransferFixture(tb, func(*http.Request) (*http.Response, error) {
			return nil, errors.New("synthetic-transport-detail")
		})
		// the first call retains the request, with an unknown outcome
		if _, err := WalletCircleTransferOut(f.args, f.owner); !errors.Is(err, ErrCircleTransferUnknown) {
			tb.Fatal(err)
		}
		f.args.ToAddress = "different-destination"
		_, err := WalletCircleTransferOut(f.args, f.owner)
		requireRefusalAnswer(tb, err, http.StatusConflict, "customer transfer request ID already binds different immutable terms")
	})
}

// The customer has no wallet a transfer can come from.
func TestCircleTransferRefusesNoWalletAsConflict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		f := newCustomerTransferFixture(tb, nil)
		f.client.core.readHooks.do = func(*http.Request) (*http.Response, error) {
			return customerTransferResponse(200, map[string]any{"data": map[string]any{"wallets": []any{}}}), nil
		}
		_, err := WalletCircleTransferOut(f.args, f.owner)
		requireRefusalAnswer(tb, err, http.StatusConflict, "no transferable customer wallet")
	})
}

// Circle's answer to the submit is lost. Its transport error was sent after
// the retry instruction, joined to it.
func TestCircleTransferUnknownOutcomeHidesItsCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		f := newCustomerTransferFixture(tb, func(*http.Request) (*http.Response, error) {
			return nil, errors.New("synthetic-transport-detail")
		})
		_, err := WalletCircleTransferOut(f.args, f.owner)
		requireRefusalAnswer(tb, err, http.StatusInternalServerError, ErrCircleTransferUnknown.Error())
	})
}

func TestPayDataLookupsRefuseAnExhaustedAddressAsTooManyRequests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		previous := payDataLookupLimiter
		payDataLookupLimiter = &payDataIpLimiter{limitPerMinute: 0}
		defer func() { payDataLookupLimiter = previous }()
		clientSession := session.Testing_CreateClientSession(tb.Context(), nil)
		defer clientSession.Cancel()
		clientSession.ClientAddress = "192.0.2.61:4500"
		_, err := PayDataNetworkLookup(&PayDataNetworkLookupArgs{NetworkName: "synthetic-network"}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusTooManyRequests, "Too many lookups from this address. Try again in a minute.")
		_, err = PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: "synthetic-reference"}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusTooManyRequests, "Too many status checks from this address. Try again in a minute.")
	})
}
