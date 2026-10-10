// The actual authenticated HTTP boundary issues, accepts and replays exact
// coldkey consent; the independent SN consumer uses its own original head pin.
package controller

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/validator"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// This uses the same route authentication/decoding as api/handlers without a
// child-package import into its controller parent.
func walletMappingControllerHttp(t testing.TB) *httptest.Server {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sn/wallet/consent":
			router.WrapWithInputRequireAuth(SnWalletMappingChallenge, w, r)
		case "/sn/wallet":
			if r.Method == http.MethodGet {
				router.WrapRequireAuth(SnGetWallet, w, r)
			} else {
				router.WrapWithInputRequireAuth(SnSetWallet, w, r)
			}
		case "/sn/wallet/consent/history":
			router.WrapWithInputNoAuth(SnWalletMappingHistory, w, r)
		case "/sn/wallet/network-consent":
			router.WrapWithInputRequireAuth(SnNetworkWalletMappingChallenge, w, r)
		case "/sn/wallet/network-consent/history":
			router.WrapWithInputNoAuth(SnNetworkWalletMappingHistory, w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

// Actual HTTP response bytes and status are retained for joined assertions.
func walletMappingControllerPost(t testing.TB, endpoint, path, token string, value any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, protocol.MaxWalletMappingConsentBytes+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	return response.StatusCode, body
}

// An independently reconstructed original digest, never a response-selected
// latest SQL head, drives the real consumer-shaped HTTP history request.
func TestWalletMappingPublicAuthenticatedConsentAndIndependentHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(t)
		endpoint := walletMappingControllerHttp(t)
		key, err := schnorrkel.NewMiniSecretKeyFromRaw([32]byte{38})
		if err != nil {
			t.Fatal(err)
		}
		address, err := ss58.Encode(key.Public().Encode(), ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		args := &SnWalletMappingChallengeArgs{ClientId: credential.ClientId, ColdkeySs58: address, FromEpoch: 1, ThroughEpoch: 100}
		if status, _ := walletMappingControllerPost(t, endpoint.URL, "/sn/wallet/consent", "", args); status == http.StatusOK {
			t.Fatal("unauthenticated caller issued a provider mapping")
		}
		status, raw := walletMappingControllerPost(t, endpoint.URL, "/sn/wallet/consent", credential.Testing_Sign(), args)
		var challenge SnWalletMappingChallengeResult
		if status != http.StatusOK || json.Unmarshal(raw, &challenge) != nil || challenge.Message == "" {
			t.Fatal("actual authenticated mapping challenge failed", status, string(raw))
		}
		signature, err := key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(challenge.Message)))
		if err != nil {
			t.Fatal(err)
		}
		original := protocol.WalletMappingConsent{Message: challenge.Message, Signature: signature.Encode()}
		statement, head, err := protocol.VerifyWalletMappingConsent(t.Context(), original)
		if err != nil || statement.Domain != fixture.domain || statement.ClientId != [16]byte(*credential.ClientId) || statement.NetworkId != [16]byte(credential.NetworkId) || statement.UserId != [16]byte(credential.UserId) {
			t.Fatal("issued consent lost its actual authenticated domain", statement, err)
		}
		set := &SnSetWalletArgs{ClientId: credential.ClientId, ColdkeySs58: address, Message: original.Message, Signature: "0x" + hex.EncodeToString(original.Signature[:])}
		for range 2 {
			status, raw = walletMappingControllerPost(t, endpoint.URL, "/sn/wallet", credential.Testing_Sign(), set)
			var result SnSetWalletResult
			if status != http.StatusOK || json.Unmarshal(raw, &result) != nil || result.Error != nil || result.MappingHash != hex.EncodeToString(head[:]) || result.MappingGeneration != 1 {
				t.Fatal("actual wallet set did not retain original mapping", status, string(raw))
			}
		}
		reader, err := validator.NewHttpWalletMappingReader(endpoint.URL)
		if err != nil {
			t.Fatal(err)
		}
		originals, value, err := reader.Read(t.Context(), protocol.WalletMappingHistoryExpectation{Domain: fixture.domain, ClientId: [16]byte(*credential.ClientId), HeadHash: head, Generation: 1, Epoch: 1})
		if err != nil || value == nil || len(originals) != 1 || originals[0] != original || value.Statement.Coldkey != key.Public().Encode() {
			t.Fatal("actual independent reader lost accepted signed mapping", value, err)
		}
	})
}

// The new route cannot silently accept a client from another authenticated
// network or reinterpret a modified mapping as a generic login challenge.
func TestWalletMappingPublicForeignClientCannotAcquireConsent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		_, credential, _ := newStClientKeyHistoryControllerFixture(t)
		endpoint := walletMappingControllerHttp(t)
		key, err := schnorrkel.NewMiniSecretKeyFromRaw([32]byte{39})
		if err != nil {
			t.Fatal(err)
		}
		address, err := ss58.Encode(key.Public().Encode(), ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		foreign := *credential
		foreign.NetworkId = server.NewId()
		args := &SnWalletMappingChallengeArgs{ClientId: credential.ClientId, ColdkeySs58: address, FromEpoch: 0, ThroughEpoch: 100}
		if status, _ := walletMappingControllerPost(t, endpoint.URL, "/sn/wallet/consent", foreign.Testing_Sign(), args); status == http.StatusOK {
			t.Fatal("foreign authenticated network acquired a signed association")
		}
	})
}
