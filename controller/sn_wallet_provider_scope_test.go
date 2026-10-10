// Authenticated provider scope is checked before either wallet proof is consumed.
// Only original consent admitted by an independent roster enables an epoch wallet.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The existing HTTP and database helpers inherit one bounded test owner.
type snWalletProviderScopeTest struct {
	testing.TB
	ctx context.Context
}

// Preserve the testing lifecycle while bounding every fixture request.
func (self *snWalletProviderScopeTest) Context() context.Context {
	return self.ctx
}

// Run once so a refusal cannot be hidden by a successful retry of the test.
func snWalletProviderScopeRun(t *testing.T, run func(testing.TB)) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		run(&snWalletProviderScopeTest{TB: t, ctx: ctx})
	})
}

// Two real clients share one network and user, isolating the client restriction.
// The future earning clock is fixed before issuing either wallet challenge.
type snWalletProviderScopeFixture struct {
	rpc      *stClientKeyHistoryRPCFixture
	provider *session.ByJwt
	sibling  *session.ByJwt
	owner    *session.ByJwt
	endpoint *httptest.Server
	key      *schnorrkel.MiniSecretKey
	address  string
	scope    model.StProviderWalletEpochScope
}

// All signers, clients and RPC observations are synthetic and owned by this test.
func newSnWalletProviderScopeFixture(t testing.TB) *snWalletProviderScopeFixture {
	t.Helper()
	rpc, provider, _ := newStClientKeyHistoryControllerFixture(t)
	owner := session.NewByJwt(provider.NetworkId, provider.UserId, "synthetic-wallet-owner", false, false)
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(t.Context(), provider.NetworkId, deviceId, clientId, "synthetic-wallet-sibling", "test")
	key, err := schnorrkel.NewMiniSecretKeyFromRaw([32]byte{147})
	if err != nil {
		t.Fatal(err)
	}
	address, err := ss58.Encode(key.Public().Encode(), ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return &snWalletProviderScopeFixture{
		rpc: rpc, provider: provider, sibling: owner.Client(deviceId, clientId), owner: owner,
		endpoint: walletMappingControllerHttp(t), key: key, address: address,
		scope: model.StProviderWalletEpochScope{
			Domain: rpc.domain, Epoch: rpc.boundary.Epoch + 1,
			Start:     payoutartifact.Boundary{Number: rpc.boundary.Block + 1, Hash: common.Hash{148}.Hex()},
			End:       payoutartifact.Boundary{Number: rpc.boundary.Block + 11, Hash: common.Hash{149}.Hex()},
			StartTime: server.NowUtc().Truncate(time.Second).Add(10 * time.Minute),
		},
	}
}

// Issue through the authenticated route, then sign and independently decode the
// exact displayed bytes before a caller can use the resulting history checkpoint.
func (self *snWalletProviderScopeFixture) consent(t testing.TB, credential *session.ByJwt, clientId *server.Id) (*SnSetWalletArgs, protocol.WalletMappingStatement, [32]byte) {
	t.Helper()
	args := &SnWalletMappingChallengeArgs{ClientId: clientId, ColdkeySs58: self.address, FromEpoch: self.scope.Epoch, ThroughEpoch: self.scope.Epoch + 100}
	status, raw := walletMappingControllerPost(t, self.endpoint.URL, "/sn/wallet/consent", credential.Testing_Sign(), args)
	var challenge SnWalletMappingChallengeResult
	if status != http.StatusOK || json.Unmarshal(raw, &challenge) != nil || challenge.Message == "" {
		t.Fatal("authorized HTTP consent issuance failed", status, string(raw))
	}
	signature, err := self.key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(challenge.Message)))
	if err != nil {
		t.Fatal(err)
	}
	original := protocol.WalletMappingConsent{Message: challenge.Message, Signature: signature.Encode()}
	statement, head, err := protocol.VerifyWalletMappingConsent(t.Context(), original)
	selectedClientId := clientId
	if selectedClientId == nil {
		selectedClientId = credential.ClientId
	}
	if err != nil || selectedClientId == nil || statement.Domain != self.rpc.domain || statement.UserId != [16]byte(credential.UserId) || statement.NetworkId != [16]byte(credential.NetworkId) || statement.ClientId != [16]byte(*selectedClientId) || statement.Coldkey != self.key.Public().Encode() || statement.Schema != protocol.WalletMappingProspectiveSchema || statement.Prospective.Boundary != self.rpc.boundary || statement.Prospective.Signer != self.rpc.root || statement.Generation != 1 || statement.FromEpoch != self.scope.Epoch || statement.ExpiresAt > self.scope.StartTime.Unix() {
		t.Fatal("issued original differs from the independently selected owner or earning window", statement, err)
	}
	return &SnSetWalletArgs{ClientId: clientId, ColdkeySs58: self.address, Message: original.Message, Signature: "0x" + hex.EncodeToString(original.Signature[:])}, *statement, head
}

// Generic proof also starts at its actual public HTTP challenge endpoint. Its
// validity is checked without consuming the nonce before the ownership attack.
func (self *snWalletProviderScopeFixture) loginProof(t testing.TB, clientId *server.Id) (*SnSetWalletArgs, string) {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/wallet-challenge" {
			http.NotFound(w, r)
			return
		}
		router.WrapWithInputNoAuth(AuthWalletChallenge, w, r)
	}))
	t.Cleanup(endpoint.Close)
	blockchain := model.TAO.String()
	status, raw := walletMappingControllerPost(t, endpoint.URL, "/auth/wallet-challenge", "", AuthWalletChallengeArgs{WalletAddress: &self.address, Blockchain: &blockchain})
	var challenge AuthWalletChallengeResult
	if status != http.StatusOK || json.Unmarshal(raw, &challenge) != nil || challenge.Error != nil || challenge.Challenge == "" || challenge.MessageTemplate == "" {
		t.Fatal("actual generic wallet challenge failed", status, string(raw))
	}
	signature, err := self.key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(challenge.MessageTemplate)))
	if err != nil {
		t.Fatal(err)
	}
	encoded := signature.Encode()
	proof := &SnSetWalletArgs{ClientId: clientId, ColdkeySs58: self.address, Message: challenge.MessageTemplate, Signature: "0x" + hex.EncodeToString(encoded[:])}
	if valid, err := model.VerifyBittensorSignature(self.address, proof.Message, proof.Signature); err != nil || !valid {
		t.Fatal("sibling refusal fixture lacks a valid coldkey proof", valid, err)
	}
	return proof, challenge.Challenge
}

// Distinguish unchanged pending consent from both retained originals and the
// mutable provider/network projections, including unauthorized nonce issuance.
type snWalletProviderScopeRows struct {
	challenges      int
	consents        int
	providerWallets int
	networkWallets  int
}

// Read both siblings so an attack cannot silently redirect the write to itself.
func (self *snWalletProviderScopeFixture) rows(t testing.TB) snWalletProviderScopeRows {
	t.Helper()
	var rows snWalletProviderScopeRows
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM wallet_mapping_challenge WHERE client_id IN ($1,$2)),
			(SELECT count(*) FROM wallet_mapping_consent WHERE client_id IN ($1,$2)),
			(SELECT count(*) FROM st_provider_wallet_history WHERE client_id IN ($1,$2)),
			(SELECT count(*) FROM st_wallet WHERE network_id=$3)`, *self.provider.ClientId, *self.sibling.ClientId, self.provider.NetworkId).Scan(&rows.challenges, &rows.consents, &rows.providerWallets, &rows.networkWallets))
	})
	return rows
}

// Successful route responses retain their original mapping fields for review.
func (self *snWalletProviderScopeFixture) accept(t testing.TB, credential *session.ByJwt, proof *SnSetWalletArgs) SnSetWalletResult {
	t.Helper()
	status, raw := walletMappingControllerPost(t, self.endpoint.URL, "/sn/wallet", credential.Testing_Sign(), proof)
	var accepted SnSetWalletResult
	if status != http.StatusOK || json.Unmarshal(raw, &accepted) != nil || accepted.Error != nil {
		t.Fatal("authorized HTTP wallet acceptance failed", status, string(raw))
	}
	return accepted
}

// This separate roster signer explicitly admits the reviewed provider and
// checkpoint. A retained server response alone never grants this authority.
func (self *snWalletProviderScopeFixture) approve(t testing.TB, credential *session.ByJwt, head string, generation uint64) ([]byte, payoutartifact.WholeWorkExpectation) {
	t.Helper()
	rosterKey, err := crypto.HexToECDSA(strings.Repeat("46", 32))
	if err != nil {
		t.Fatal(err)
	}
	authority := payoutartifact.WholeWorkAuthority{
		Domain: self.scope.Domain, Epoch: self.scope.Epoch, Start: self.scope.Start, End: self.scope.End,
		RequestPublicKey: [32]byte{150}, PriorContracts: []payoutartifact.WholeWorkPriorContract{},
		Owners:            []payoutartifact.WholeWorkOwner{{ClientId: [16]byte(*credential.ClientId), NetworkId: [16]byte(credential.NetworkId), Generation: [16]byte{151}, PublicKey: [32]byte{152}}},
		ExpectedProviders: []payoutartifact.WholeWorkExpectedProvider{{ClientId: [16]byte(*credential.ClientId), NetworkId: [16]byte(credential.NetworkId), WalletHeadHash: head, WalletGeneration: generation}},
	}
	authority, err = payoutartifact.SignWholeWorkAuthority(t.Context(), authority, rosterKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := authority.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	expected := payoutartifact.WholeWorkExpectation{AuthoritySigner: crypto.PubkeyToAddress(rosterKey.PublicKey), ClientKeyRootSigner: self.rpc.root, AuthorityHash: "sha256:" + hex.EncodeToString(digest[:])}
	return raw, expected
}

// A sibling exists in the same network and under the same user; only the JWT
// client differs. Refusal must happen before creating its signed challenge.
func TestSnWalletProviderScopeRefusesSiblingChallenge(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		args := &SnWalletMappingChallengeArgs{ClientId: f.sibling.ClientId, ColdkeySs58: f.address, FromEpoch: f.scope.Epoch, ThroughEpoch: f.scope.Epoch + 100}
		status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/consent", f.provider.Testing_Sign(), args)
		if status == http.StatusOK || strings.TrimSpace(string(raw)) != protocol.ErrWalletMappingIntegrity.Error() {
			t.Fatal("sibling consent issuance was not refused by the provider ownership gate", status, string(raw))
		}
		if rows := f.rows(t); rows != (snWalletProviderScopeRows{}) {
			t.Fatal("refused sibling issuance wrote wallet state", rows)
		}
		f.consent(t, f.sibling, f.sibling.ClientId)
		if rows := f.rows(t); rows != (snWalletProviderScopeRows{challenges: 1}) {
			t.Fatal("legitimate sibling could not issue its own original consent", rows)
		}
	})
}

// A real coldkey signature and owner-issued consent remove every independent
// refusal reason. The sibling can still accept exactly that original afterward.
func TestSnWalletProviderScopeRefusesSiblingConsentAcceptance(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		proof, _, head := f.consent(t, f.owner, f.sibling.ClientId)
		status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.provider.Testing_Sign(), proof)
		if status == http.StatusOK || strings.TrimSpace(string(raw)) != protocol.ErrWalletMappingIntegrity.Error() {
			t.Fatal("sibling consent acceptance was not refused by the provider ownership gate", status, string(raw))
		}
		if rows := f.rows(t); rows != (snWalletProviderScopeRows{challenges: 1}) {
			t.Fatal("refused sibling acceptance consumed consent or changed a wallet", rows)
		}
		accepted := f.accept(t, f.sibling, proof)
		if accepted.MappingHash != hex.EncodeToString(head[:]) || accepted.MappingGeneration != 1 {
			t.Fatal("authorized sibling could not retain the refused original", accepted)
		}
		if rows := f.rows(t); rows != (snWalletProviderScopeRows{challenges: 1, consents: 1, providerWallets: 1, networkWallets: 1}) {
			t.Fatal("authorized sibling acceptance did not commit exactly once", rows)
		}
	})
}

// Possession of a valid login proof cannot authorize a sibling. Refusing before
// nonce consumption lets the actual provider submit that same signed challenge.
func TestSnWalletProviderScopeRefusesSiblingLoginProofWithoutConsumption(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		proof, challenge := f.loginProof(t, f.sibling.ClientId)
		status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.provider.Testing_Sign(), proof)
		var refused SnSetWalletResult
		if status != http.StatusOK || json.Unmarshal(raw, &refused) != nil || refused.Error == nil || refused.Error.Message != "Client does not match the authenticated provider." {
			t.Fatal("valid sibling proof was not refused by the provider ownership gate", status, string(raw))
		}
		var used bool
		server.Db(t.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(t.Context(), `SELECT used FROM wallet_auth_challenge WHERE challenge_value=$1`, challenge).Scan(&used))
		})
		if used || f.rows(t) != (snWalletProviderScopeRows{}) {
			t.Fatal("refused sibling proof consumed the nonce or changed wallet state", used)
		}
		accepted := f.accept(t, f.sibling, proof)
		if accepted.MappingHash != "" || accepted.MappingGeneration != 0 {
			t.Fatal("generic proof invented original mapping history", accepted)
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(t.Context(), `SELECT used FROM wallet_auth_challenge WHERE challenge_value=$1`, challenge).Scan(&used))
		})
		if !used || f.rows(t) != (snWalletProviderScopeRows{providerWallets: 1, networkWallets: 1}) {
			t.Fatal("authorized sibling did not consume its proof exactly once", used)
		}
		wallet := model.GetStProviderWalletsAt(t.Context(), f.scope.StartTime)[*f.sibling.ClientId]
		if wallet == nil || wallet.ClientId != *f.sibling.ClientId || wallet.NetworkId != f.sibling.NetworkId || wallet.ColdkeyPubkey != f.key.Public().Encode() || wallet.OriginalMessage == nil || *wallet.OriginalMessage != proof.Message || wallet.OriginalSignature == nil || *wallet.OriginalSignature != proof.Signature {
			t.Fatal("authorized sibling lost its exact generic proof projection", wallet)
		}
	})
}

// An intentional network owner can still select either actual network client
// through both signed consent and the generic proof compatibility path.
func TestSnWalletProviderScopeNetworkOwnerCanSelectNetworkClient(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		consent, _, head := f.consent(t, f.owner, f.sibling.ClientId)
		accepted := f.accept(t, f.owner, consent)
		if accepted.MappingHash != hex.EncodeToString(head[:]) || accepted.MappingGeneration != 1 {
			t.Fatal("network owner lost explicit client consent compatibility", accepted)
		}
		proof, _ := f.loginProof(t, f.provider.ClientId)
		accepted = f.accept(t, f.owner, proof)
		if accepted.MappingHash != "" || accepted.MappingGeneration != 0 {
			t.Fatal("network owner's generic proof invented mapping consent", accepted)
		}
		wallets := model.GetStProviderWalletsAt(t.Context(), f.scope.StartTime)
		for _, credential := range []*session.ByJwt{f.provider, f.sibling} {
			wallet := wallets[*credential.ClientId]
			if wallet == nil || wallet.NetworkId != credential.NetworkId || wallet.ColdkeyPubkey != f.key.Public().Encode() {
				t.Fatal("network owner could not select its own network client", credential.ClientId, wallet)
			}
		}
		if rows := f.rows(t); rows != (snWalletProviderScopeRows{challenges: 1, consents: 1, providerWallets: 2, networkWallets: 1}) {
			t.Fatal("network owner compatibility wrote unexpected wallet history", rows)
		}
	})
}

// The public producer's actual message is signed, accepted, independently
// admitted into an original roster, and selected by the real epoch consumer.
func TestSnWalletProviderScopeOriginalConsentEnablesEpochWallet(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		proof, statement, head := f.consent(t, f.provider, nil)
		accepted := f.accept(t, f.provider, proof)
		if accepted.MappingHash != hex.EncodeToString(head[:]) || accepted.MappingGeneration != statement.Generation {
			t.Fatal("HTTP acceptance did not retain the independently verified original", accepted)
		}
		authority, expected := f.approve(t, f.provider, accepted.MappingHash, accepted.MappingGeneration)
		if wallets, err := model.GetStProviderWalletsForEpoch(t.Context(), nil, expected, f.scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("accepted consent became epoch authority without an independent original roster", wallets, err)
		}
		wallets, err := model.GetStProviderWalletsForEpoch(t.Context(), authority, expected, f.scope)
		if err != nil || len(wallets) != 1 {
			t.Fatal("HTTP-produced original failed the real epoch wallet consumer", wallets, err)
		}
		wallet := wallets[*f.provider.ClientId]
		if wallet == nil || wallet.ClientId != *f.provider.ClientId || wallet.NetworkId != f.provider.NetworkId || wallet.ColdkeySs58 != f.address || wallet.ColdkeyPubkey != f.key.Public().Encode() || wallet.OriginalMessage == nil || *wallet.OriginalMessage != proof.Message || wallet.OriginalSignature == nil || *wallet.OriginalSignature != proof.Signature || !wallet.SetTime.IsZero() {
			t.Fatal("epoch consumer selected another owner, projection or reconstructed proof", wallet)
		}
	})
}

// Even independently signed roster claims cannot turn a generic login proof
// projection into missing original consent. Unavailable history stays unknown.
func TestSnWalletProviderScopeLoginProofCannotEnableEpochWallet(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		proof, _ := f.loginProof(t, f.provider.ClientId)
		accepted := f.accept(t, f.provider, proof)
		if accepted.MappingHash != "" || accepted.MappingGeneration != 0 {
			t.Fatal("generic login proof gained a consent checkpoint", accepted)
		}
		wallet := model.GetStProviderWalletsAt(t.Context(), f.scope.StartTime)[*f.provider.ClientId]
		if wallet == nil || wallet.ColdkeyPubkey != f.key.Public().Encode() || wallet.OriginalMessage == nil || *wallet.OriginalMessage != proof.Message || wallet.OriginalSignature == nil || *wallet.OriginalSignature != proof.Signature {
			t.Fatal("fixture did not retain the actual generic wallet proof projection", wallet)
		}
		if rows := f.rows(t); rows != (snWalletProviderScopeRows{providerWallets: 1, networkWallets: 1}) {
			t.Fatal("generic proof unexpectedly created mapping history", rows)
		}
		missingHead := sha256.Sum256([]byte("synthetic-missing-original-wallet-consent"))
		authority, expected := f.approve(t, f.provider, hex.EncodeToString(missingHead[:]), 1)
		if wallets, err := model.GetStProviderWalletsForEpoch(t.Context(), authority, expected, f.scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("generic login projection supplied missing epoch consent history", wallets, err)
		}
	})
}
