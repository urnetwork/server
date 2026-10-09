package controller

// The wallet sign-in and network create refusal of a signature made with
// another account than the address entered carries `error.code`
// signature_mismatch only for a client that asks for `result_errors`; an older
// client gets the HTTP 401 it always had. Network create keeps the 401 for
// both (the signup monitor counts every 2xx create as a created network). A
// wallet mapping consent signed by another account than the coldkey it names
// is coded on POST /sn/wallet like a login challenge.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The status and body a refusal error reaches the client as.
func walletSignatureMismatchHttp(err error) (int, string) {
	w := httptest.NewRecorder()
	router.RaiseHttpError(err, w)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// Only a coded login refusal depends on `result_errors`: the code for a client
// that asks, the 401 with the message for one that does not. An uncoded
// refusal and a success are answered as before either way.
func TestAuthLoginCodedRefusalNeedsResultErrors(t *testing.T) {
	coded := &model.AuthLoginResult{Error: &model.AuthLoginResultError{
		Code:    model.WalletAuthErrorCodeSignatureMismatch,
		Message: "Synthetic signature mismatch.",
	}}
	uncoded := &model.AuthLoginResult{Error: &model.AuthLoginResultError{Message: "Invalid login credentials."}}
	signedIn := &model.AuthLoginResult{Network: &model.AuthLoginResultNetwork{ByJwt: "synthetic-jwt"}}
	modelLogin := func(result *model.AuthLoginResult) authLoginFunction {
		return func(model.AuthLoginArgs, *session.ClientSession) (*model.AuthLoginResult, error) {
			return result, nil
		}
	}

	result, err := authLogin(model.AuthLoginArgs{ResultErrors: true}, nil, modelLogin(coded))
	connect.AssertEqual(t, err, nil)
	if result != coded {
		t.Fatalf("result_errors: = %+v, want the coded result", result)
	}

	result, err = authLogin(model.AuthLoginArgs{}, nil, modelLogin(coded))
	if result != nil || err == nil {
		t.Fatalf("an older client: = %+v, %v, want an error", result, err)
	}
	status, body := walletSignatureMismatchHttp(err)
	connect.AssertEqual(t, status, http.StatusUnauthorized)
	connect.AssertEqual(t, body, "Synthetic signature mismatch.")

	cases := []struct {
		args   model.AuthLoginArgs
		result *model.AuthLoginResult
	}{
		{args: model.AuthLoginArgs{}, result: uncoded},
		{args: model.AuthLoginArgs{ResultErrors: true}, result: uncoded},
		{args: model.AuthLoginArgs{}, result: signedIn},
		{args: model.AuthLoginArgs{ResultErrors: true}, result: signedIn},
	}
	for _, test := range cases {
		result, err := authLogin(test.args, nil, modelLogin(test.result))
		if result != test.result || err != nil {
			t.Fatalf("result_errors %v: = %+v, %v, want %+v unchanged", test.args.ResultErrors, result, err, test.result)
		}
	}

	var args model.AuthLoginArgs
	err = json.Unmarshal([]byte(`{"wallet_auth":{"wallet_address":"synthetic"},"result_errors":true}`), &args)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, args.ResultErrors, true)
}

// Network create on a challenge issued for the entered coldkey, signed by
// another account: a 401 either way, with the coded result as its JSON body
// for a client that asks for `result_errors` and the plain message for an
// older one. A refusal without a code keeps its plain status either way.
func TestNetworkCreateCodedRefusalNeedsResultErrors(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		typedAddress, _ := testSnColdkey(t)
		_, otherSign := testSnColdkey(t)
		blockchain := model.TAO.String()
		challenge := model.CreateWalletAuthChallenge(model.WalletAuthChallengeArgs{
			WalletAddress: &typedAddress,
			Blockchain:    &blockchain,
		}, ctx)
		if challenge.Error != nil {
			t.Fatalf("challenge: %s", challenge.Error.Message)
		}
		args := model.NetworkCreateArgs{
			NetworkName: fmt.Sprintf("mismatch-%s", strings.ToLower(server.NewId().String()[:8])),
			Terms:       true,
			WalletAuth: &model.WalletAuthArgs{
				PublicKey:  typedAddress,
				Signature:  otherSign(challenge.MessageTemplate),
				Message:    challenge.MessageTemplate,
				Blockchain: blockchain,
			},
		}

		result, err := NetworkCreate(args, clientSession)
		if result != nil || err == nil {
			t.Fatalf("an older client: = %+v, %v, want an error", result, err)
		}
		status, body := walletSignatureMismatchHttp(err)
		connect.AssertEqual(t, status, http.StatusUnauthorized)
		connect.AssertEqual(t, body, "The signature does not match this wallet address. Sign the challenge with this address.")

		args.ResultErrors = true
		result, err = NetworkCreate(args, clientSession)
		if result != nil || err == nil {
			t.Fatalf("result_errors: = %+v, %v, want an error", result, err)
		}
		w := httptest.NewRecorder()
		router.RaiseHttpError(err, w)
		connect.AssertEqual(t, w.Code, http.StatusUnauthorized)
		connect.AssertEqual(t, w.Header().Get("Content-Type"), "application/json")
		var refused model.NetworkCreateResult
		if err := json.Unmarshal(w.Body.Bytes(), &refused); err != nil {
			t.Fatalf("result_errors: body %q is not the result: %s", w.Body.String(), err)
		}
		if refused.Error == nil || refused.Network != nil {
			t.Fatalf("result_errors: body %q, want only a coded error", w.Body.String())
		}
		connect.AssertEqual(t, refused.Error.Code, model.WalletAuthErrorCodeSignatureMismatch)
		connect.AssertEqual(t, refused.Error.Message, body)

		args.Terms = false
		result, err = NetworkCreate(args, clientSession)
		if result != nil || err == nil {
			t.Fatalf("an uncoded refusal with result_errors: = %+v, %v, want an error", result, err)
		}
		w = httptest.NewRecorder()
		router.RaiseHttpError(err, w)
		connect.AssertEqual(t, w.Code, http.StatusBadRequest)
		connect.AssertEqual(t, strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain"), true)
	})
}

// A wallet mapping consent issued for the entered coldkey but signed by another
// account is coded like a login challenge signed by one, before any mapping
// state is read (no database or chain reader here). The entered coldkey's own
// signature, a consent naming another coldkey, an undecodable consent and an
// undecodable signature are not a mismatch and keep the consent's refusals.
func TestSnWalletMappingConsentSignatureMismatchIsCoded(t *testing.T) {
	previous := stConfigInstance
	t.Cleanup(func() {
		SetStConfig(previous)
	})
	SetStConfig(&StConfig{
		Enabled:         true,
		ChainId:         945,
		GenesisHash:     [32]byte{1},
		Netuid:          521,
		ContractAddress: common.Address{2},
		SettlementVault: common.Address{3},
		DeploymentId:    "mapping-mismatch-test",
		PolicyHash:      [32]byte{4},
		NoId:            1,
	})
	domain, ok := stClientKeyHistoryDomain()
	if !ok {
		t.Fatal("the synthetic st config has no key history domain")
	}

	networkId, userId, clientId, deviceId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	clientSession := session.Testing_CreateClientSession(t.Context(), session.NewByJwt(networkId, userId, "mapping-mismatch", false, false).Client(deviceId, clientId))
	typedAddress, typedSign := testSnColdkey(t)
	otherAddress, otherSign := testSnColdkey(t)
	typedColdkey, err := ss58.DecodeWithPrefix(typedAddress, ss58.BittensorPrefix)
	connect.AssertEqual(t, err, nil)
	otherColdkey, err := ss58.DecodeWithPrefix(otherAddress, ss58.BittensorPrefix)
	connect.AssertEqual(t, err, nil)
	issuedAt := server.NowUtc().Unix()
	consent := func(coldkey [32]byte) string {
		message, err := protocol.WalletMappingStatement{
			Schema:       protocol.WalletMappingConsentSchema,
			Domain:       domain,
			UserId:       [16]byte(userId),
			ClientId:     [16]byte(clientId),
			NetworkId:    [16]byte(networkId),
			Coldkey:      coldkey,
			Generation:   1,
			Nonce:        sha256.Sum256([]byte("mapping-mismatch-nonce")),
			IssuedAt:     issuedAt,
			ExpiresAt:    issuedAt + 300,
			FromEpoch:    1,
			ThroughEpoch: 100,
		}.Message()
		connect.AssertEqual(t, err, nil)
		return message
	}
	message := consent(typedColdkey)

	result, err := SnSetWallet(&SnSetWalletArgs{
		ClientId:    &clientId,
		ColdkeySs58: typedAddress,
		Message:     message,
		Signature:   otherSign(message),
	}, clientSession)
	connect.AssertEqual(t, err, nil)
	if result == nil || result.Error == nil || result.MappingHash != "" {
		t.Fatalf("another account's consent signature = %+v, want a coded refusal", result)
	}
	connect.AssertEqual(t, result.Error.Code, SnSetWalletErrorCodeSignatureMismatch)
	connect.AssertEqual(t, result.Error.Message, snSetWalletSignatureMismatchMessage)

	cases := []struct {
		name         string
		message      string
		coldkey      [32]byte
		signature    string
		wantMismatch bool
	}{
		{name: "another account", message: message, coldkey: typedColdkey, signature: otherSign(message), wantMismatch: true},
		{name: "the entered coldkey", message: message, coldkey: typedColdkey, signature: typedSign(message)},
		{name: "a consent naming another coldkey", message: consent(otherColdkey), coldkey: typedColdkey, signature: typedSign(consent(otherColdkey))},
		{name: "not a consent", message: protocol.WalletMappingConsentPrefix + "{}", coldkey: typedColdkey, signature: otherSign(protocol.WalletMappingConsentPrefix + "{}")},
		// no schnorrkel marker: well formed ed25519 bytes that decode and do
		// not verify, so they are a mismatch like any other wrong signature
		{name: "64 bytes that are no signature", message: message, coldkey: typedColdkey, signature: "0x" + strings.Repeat("00", 64), wantMismatch: true},
		// the sr25519 marker on bytes that are no schnorrkel signature: undecodable
		{name: "64 marked bytes that are no signature", message: message, coldkey: typedColdkey, signature: "0x" + strings.Repeat("ff", 64)},
	}
	for _, test := range cases {
		mismatch := snWalletMappingSignatureMismatch(test.message, test.coldkey, typedAddress, test.signature)
		if mismatch != test.wantMismatch {
			t.Fatalf("%s: mismatch = %v, want %v", test.name, mismatch, test.wantMismatch)
		}
	}
}
