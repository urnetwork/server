// Network wallet consent through the actual authenticated HTTP boundary: only
// the network owner's session issues and submits it, it changes no wallet
// projection, its history serves the independent reader, GET /sn/wallet shows
// it in settlement's precedence, and epoch settlement applies the precedence.
// Keys, networks and clients are synthetic.
package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/ss58"
	"github.com/urfoundation/sn/v2026/validator"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A second synthetic coldkey, distinct from the fixture's provider coldkey.
func snNetworkWalletTestKey(t testing.TB, seed byte) (*schnorrkel.MiniSecretKey, string) {
	t.Helper()
	key, err := schnorrkel.NewMiniSecretKeyFromRaw([32]byte{seed})
	if err != nil {
		t.Fatal(err)
	}
	address, err := ss58.Encode(key.Public().Encode(), ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return key, address
}

// Issue through the authenticated network route, then sign the exact bytes
// and decode them independently.
func (self *snWalletProviderScopeFixture) networkConsent(t testing.TB, credential *session.ByJwt, key *schnorrkel.MiniSecretKey, address string, fromEpoch uint64) (*SnSetWalletArgs, protocol.NetworkWalletMappingStatement, [32]byte) {
	t.Helper()
	args := &SnNetworkWalletMappingChallengeArgs{ColdkeySs58: address, FromEpoch: fromEpoch, ThroughEpoch: fromEpoch + 100}
	status, raw := walletMappingControllerPost(t, self.endpoint.URL, "/sn/wallet/network-consent", credential.Testing_Sign(), args)
	var challenge SnWalletMappingChallengeResult
	if status != http.StatusOK || json.Unmarshal(raw, &challenge) != nil || challenge.Message == "" {
		t.Fatal("authorized network consent issuance failed", status, string(raw))
	}
	signature, err := key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(challenge.Message)))
	if err != nil {
		t.Fatal(err)
	}
	original := protocol.WalletMappingConsent{Message: challenge.Message, Signature: signature.Encode()}
	statement, head, err := protocol.VerifyNetworkWalletMappingConsent(t.Context(), original)
	if err != nil {
		t.Fatal("issued network consent does not verify", err)
	}
	return &SnSetWalletArgs{ColdkeySs58: address, Message: original.Message, Signature: "0x" + hex.EncodeToString(original.Signature[:])}, *statement, head
}

// Count the network chain tables and the wallet projections of the network.
type snNetworkWalletRows struct {
	challenges      int
	consents        int
	providerWallets int
	networkWallets  int
}

func (self *snWalletProviderScopeFixture) networkRows(t testing.TB) snNetworkWalletRows {
	t.Helper()
	var rows snNetworkWalletRows
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM network_wallet_mapping_challenge WHERE network_id=$1),
			(SELECT count(*) FROM network_wallet_mapping_consent WHERE network_id=$1),
			(SELECT count(*) FROM st_provider_wallet_history WHERE network_id=$1),
			(SELECT count(*) FROM st_wallet WHERE network_id=$1)`, self.provider.NetworkId).Scan(&rows.challenges, &rows.consents, &rows.providerWallets, &rows.networkWallets))
	})
	return rows
}

// GET /sn/wallet as the credential.
func (self *snWalletProviderScopeFixture) getWallet(t testing.TB, credential *session.ByJwt) SnGetWalletResult {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, self.endpoint.URL+"/sn/wallet", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+credential.Testing_Sign())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result SnGetWalletResult
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&result) != nil {
		t.Fatal("wallet read failed", response.StatusCode)
	}
	return result
}

// A client JWT can neither request nor submit a network consent; the network
// owner's session can, exactly once per original, without any projection.
func TestSnNetworkWalletConsentOnlyNetworkOwnerAndNoProjection(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		key, address := snNetworkWalletTestKey(t, 151)
		args := &SnNetworkWalletMappingChallengeArgs{ColdkeySs58: address, FromEpoch: f.scope.Epoch, ThroughEpoch: f.scope.Epoch + 100}
		for _, credential := range []*session.ByJwt{f.provider, f.sibling} {
			status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/network-consent", credential.Testing_Sign(), args)
			if status == http.StatusOK || strings.TrimSpace(string(raw)) != protocol.ErrWalletMappingIntegrity.Error() {
				t.Fatal("a client session requested a network consent", status, string(raw))
			}
		}
		if status, _ := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/network-consent", "", args); status == http.StatusOK {
			t.Fatal("an unauthenticated caller requested a network consent")
		}
		if rows := f.networkRows(t); rows != (snNetworkWalletRows{}) {
			t.Fatal("refused network issuance wrote state", rows)
		}
		set, statement, head := f.networkConsent(t, f.owner, key, address, f.scope.Epoch)
		if statement.Schema != protocol.NetworkWalletMappingConsentSchema || statement.Scope != protocol.NetworkWalletMappingScope || statement.Domain != f.rpc.domain || statement.UserId != [16]byte(f.owner.UserId) || statement.NetworkId != [16]byte(f.owner.NetworkId) || statement.Coldkey != key.Public().Encode() || statement.Generation != 1 || statement.FromEpoch != f.scope.Epoch || statement.Prospective.Boundary != f.rpc.boundary || statement.Prospective.Signer != f.rpc.root || statement.ExpiresAt > f.scope.StartTime.Unix() {
			t.Fatal("network consent differs from the authenticated owner or window", statement)
		}
		// a client session cannot submit it, and a request naming a client is
		// refused rather than reinterpreted
		status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.provider.Testing_Sign(), set)
		if status == http.StatusOK || strings.TrimSpace(string(raw)) != protocol.ErrWalletMappingIntegrity.Error() {
			t.Fatal("a client session submitted a network consent", status, string(raw))
		}
		named := *set
		named.ClientId = f.provider.ClientId
		status, raw = walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.owner.Testing_Sign(), &named)
		if status == http.StatusOK || strings.TrimSpace(string(raw)) != protocol.ErrWalletMappingIntegrity.Error() {
			t.Fatal("a network consent was accepted for a named client", status, string(raw))
		}
		if rows := f.networkRows(t); rows != (snNetworkWalletRows{challenges: 1}) {
			t.Fatal("refused network acceptance consumed the consent", rows)
		}
		for range 2 {
			accepted := f.accept(t, f.owner, set)
			if accepted.MappingHash != hex.EncodeToString(head[:]) || accepted.MappingGeneration != 1 {
				t.Fatal("network consent acceptance lost the original", accepted)
			}
		}
		if rows := f.networkRows(t); rows != (snNetworkWalletRows{challenges: 1, consents: 1}) {
			t.Fatal("network consent changed a wallet projection or appended twice", rows)
		}
		// the independent reader requires the pinned head
		reader, err := validator.NewHttpWalletMappingReader(f.endpoint.URL)
		if err != nil {
			t.Fatal(err)
		}
		originals, verified, err := reader.ReadNetworkBounded(t.Context(), protocol.NetworkWalletMappingHistoryExpectation{Domain: f.rpc.domain, NetworkId: [16]byte(f.owner.NetworkId), HeadHash: head, Generation: 1, Epoch: f.scope.Epoch}, protocol.MaxWalletMappingConsentBytes*4)
		if err != nil || verified == nil || len(originals) != 1 || originals[0].Message != set.Message || verified.Statement.Coldkey != key.Public().Encode() {
			t.Fatal("independent reader lost the accepted network consent", verified, err)
		}
		if _, _, err := reader.ReadNetworkBounded(t.Context(), protocol.NetworkWalletMappingHistoryExpectation{Domain: f.rpc.domain, NetworkId: [16]byte(f.owner.NetworkId), HeadHash: sha256.Sum256([]byte("another head")), Generation: 1, Epoch: f.scope.Epoch}, protocol.MaxWalletMappingConsentBytes*4); err == nil {
			t.Fatal("a history read without the pinned head succeeded")
		}
	})
}

// Another account's signature is the coded mismatch; a session of a user that
// does not administer the network is refused.
func TestSnNetworkWalletConsentRefusesMismatchAndForeignOwner(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		key, address := snNetworkWalletTestKey(t, 152)
		other, _ := snNetworkWalletTestKey(t, 153)
		set, _, _ := f.networkConsent(t, f.owner, key, address, f.scope.Epoch)
		signature, err := other.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(set.Message)))
		if err != nil {
			t.Fatal(err)
		}
		encoded := signature.Encode()
		mismatch := *set
		mismatch.Signature = "0x" + hex.EncodeToString(encoded[:])
		status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.owner.Testing_Sign(), &mismatch)
		var refused SnSetWalletResult
		if status != http.StatusOK || json.Unmarshal(raw, &refused) != nil || refused.Error == nil || refused.Error.Code != SnSetWalletErrorCodeSignatureMismatch {
			t.Fatal("another account's signature was not the coded mismatch", status, string(raw))
		}
		// a session of a user who does not administer the network does not
		// authenticate; the model refuses that owner even so
		foreign := session.NewByJwt(f.owner.NetworkId, server.NewId(), "synthetic-foreign-user", false, false)
		args := &SnNetworkWalletMappingChallengeArgs{ColdkeySs58: address, FromEpoch: f.scope.Epoch, ThroughEpoch: f.scope.Epoch + 100}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/network-consent", foreign.Testing_Sign(), args); status == http.StatusOK {
			t.Fatal("a user who does not administer the network requested its consent", string(raw))
		}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", foreign.Testing_Sign(), set); status == http.StatusOK {
			t.Fatal("a user who does not administer the network submitted its consent", string(raw))
		}
		cfg := stConfig()
		if cfg == nil || cfg.RootKey == nil {
			t.Fatal("fixture operator owner absent")
		}
		prospective := &model.WalletMappingProspectiveOwner{Boundary: f.rpc.boundary, RootKey: cfg.RootKey}
		owner := model.NetworkWalletMappingOwner{Domain: f.rpc.domain, UserId: server.NewId(), NetworkId: f.owner.NetworkId, Prospective: prospective}
		if message, err := model.CreateNetworkWalletMappingChallenge(t.Context(), owner, key.Public().Encode(), f.scope.Epoch, f.scope.Epoch+100); message != "" || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("a user who does not administer the network was issued its consent", err)
		}
		owner.UserId, owner.NetworkId = f.owner.UserId, server.NewId()
		if message, err := model.CreateNetworkWalletMappingChallenge(t.Context(), owner, key.Public().Encode(), f.scope.Epoch, f.scope.Epoch+100); message != "" || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("a missing network was issued a consent", err)
		}
		owner.NetworkId, owner.Prospective = f.owner.NetworkId, nil
		if message, err := model.CreateNetworkWalletMappingChallenge(t.Context(), owner, key.Public().Encode(), f.scope.Epoch, f.scope.Epoch+100); message != "" || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("a network consent was issued without the operator boundary", err)
		}
		if rows := f.networkRows(t); rows != (snNetworkWalletRows{challenges: 1}) {
			t.Fatal("refused submissions retained a consent", rows)
		}
	})
}

// The effective wallet follows settlement: a client's own provider consent,
// else the network consent, else projections without consent.
func TestSnGetWalletShowsNetworkConsentInPrecedence(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		if result := f.getWallet(t, f.owner); result.Wallet != nil || len(result.Wallets) != 0 {
			t.Fatal("a network without wallets showed one", result)
		}
		networkKey, networkAddress := snNetworkWalletTestKey(t, 154)
		set, _, _ := f.networkConsent(t, f.owner, networkKey, networkAddress, f.scope.Epoch)
		f.accept(t, f.owner, set)
		for _, credential := range []*session.ByJwt{f.owner, f.provider, f.sibling} {
			result := f.getWallet(t, credential)
			if result.Wallet == nil || result.Wallet.ColdkeySs58 != networkAddress || result.Wallet.ConsentScope != SnWalletConsentScopeNetwork || result.Wallet.ClientId != nil || result.Wallet.FromEpoch != f.scope.Epoch || len(result.Wallets) != 1 {
				t.Fatal("the network consent is not every session's effective wallet", result)
			}
		}
		// the provider's own consent wins for the provider only
		proof, _, _ := f.consent(t, f.provider, nil)
		f.accept(t, f.provider, proof)
		result := f.getWallet(t, f.provider)
		if result.Wallet == nil || result.Wallet.ColdkeySs58 != f.address || result.Wallet.ConsentScope != SnWalletConsentScopeProvider || result.Wallet.ClientId == nil || *result.Wallet.ClientId != *f.provider.ClientId {
			t.Fatal("the provider's own consent is not its effective wallet", result)
		}
		// the provider acceptance wrote the side copy; the network consent is
		// listed first and stays effective for the other sessions
		if len(result.Wallets) != 3 || result.Wallets[0].ConsentScope != SnWalletConsentScopeNetwork || result.Wallets[1].ConsentScope != "" || result.Wallets[1].ClientId != nil || result.Wallets[2].ConsentScope != SnWalletConsentScopeProvider {
			t.Fatal("wallet entries lost their scopes or order", result.Wallets)
		}
		for _, credential := range []*session.ByJwt{f.owner, f.sibling} {
			result := f.getWallet(t, credential)
			if result.Wallet == nil || result.Wallet.ColdkeySs58 != networkAddress || result.Wallet.ConsentScope != SnWalletConsentScopeNetwork {
				t.Fatal("a provider consent or the side copy displaced the network consent", result)
			}
		}
		// a login proof projection is not a consent and does not displace it
		login, _ := f.loginProof(t, f.sibling.ClientId)
		f.accept(t, f.sibling, login)
		result = f.getWallet(t, f.sibling)
		if result.Wallet == nil || result.Wallet.ColdkeySs58 != networkAddress || result.Wallet.ConsentScope != SnWalletConsentScopeNetwork {
			t.Fatal("a login proof projection displaced the network consent", result)
		}
	})
}

// A roster pins the provider chain, the network chain or both. The provider's
// own effective consent wins, an absent or not yet effective provider chain
// falls back to the network chain, and a provider chain that cannot be read
// never falls back.
func TestStProviderWalletsForEpochNetworkPrecedence(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		networkKey, networkAddress := snNetworkWalletTestKey(t, 155)
		set, _, networkHead := f.networkConsent(t, f.owner, networkKey, networkAddress, f.scope.Epoch)
		networkAccepted := f.accept(t, f.owner, set)
		providerProof, _, _ := f.consent(t, f.provider, nil)
		providerAccepted := f.accept(t, f.provider, providerProof)
		// the sibling's own consent starts after the epoch
		laterArgs := &SnWalletMappingChallengeArgs{ColdkeySs58: f.address, FromEpoch: f.scope.Epoch + 1, ThroughEpoch: f.scope.Epoch + 100}
		status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/consent", f.sibling.Testing_Sign(), laterArgs)
		var laterChallenge SnWalletMappingChallengeResult
		if status != http.StatusOK || json.Unmarshal(raw, &laterChallenge) != nil {
			t.Fatal("later sibling consent issuance failed", status, string(raw))
		}
		laterSignature, err := f.key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(laterChallenge.Message)))
		if err != nil {
			t.Fatal(err)
		}
		laterEncoded := laterSignature.Encode()
		siblingAccepted := f.accept(t, f.sibling, &SnSetWalletArgs{ColdkeySs58: f.address, Message: laterChallenge.Message, Signature: "0x" + hex.EncodeToString(laterEncoded[:])})
		rosterKey, err := crypto.HexToECDSA(strings.Repeat("46", 32))
		if err != nil {
			t.Fatal(err)
		}
		roster := func(providers []payoutartifact.WholeWorkExpectedProvider, networks []payoutartifact.WholeWorkNetworkWallet) ([]byte, payoutartifact.WholeWorkExpectation) {
			// the roster orders its owners and providers by client
			ordered := append([]payoutartifact.WholeWorkExpectedProvider(nil), providers...)
			sort.Slice(ordered, func(i, j int) bool {
				return bytes.Compare(ordered[i].ClientId[:], ordered[j].ClientId[:]) < 0
			})
			authority := payoutartifact.WholeWorkAuthority{
				Domain: f.scope.Domain, Epoch: f.scope.Epoch, Start: f.scope.Start, End: f.scope.End,
				RequestPublicKey: [32]byte{150}, PriorContracts: []payoutartifact.WholeWorkPriorContract{},
				Owners: []payoutartifact.WholeWorkOwner{}, ExpectedProviders: ordered, NetworkWallets: networks,
			}
			for index, provider := range ordered {
				authority.Owners = append(authority.Owners, payoutartifact.WholeWorkOwner{ClientId: provider.ClientId, NetworkId: provider.NetworkId, Generation: [16]byte{byte(160 + index)}, PublicKey: [32]byte{byte(170 + index)}})
			}
			signed, err := payoutartifact.SignWholeWorkAuthority(t.Context(), authority, rosterKey)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := signed.Bytes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			return raw, payoutartifact.WholeWorkExpectation{AuthoritySigner: crypto.PubkeyToAddress(rosterKey.PublicKey), ClientKeyRootSigner: f.rpc.root, AuthorityHash: "sha256:" + hex.EncodeToString(digest[:])}
		}
		networkId := [16]byte(f.owner.NetworkId)
		pinnedNetwork := []payoutartifact.WholeWorkNetworkWallet{{NetworkId: networkId, WalletHeadHash: networkAccepted.MappingHash, WalletGeneration: networkAccepted.MappingGeneration}}
		provider := payoutartifact.WholeWorkExpectedProvider{ClientId: [16]byte(*f.provider.ClientId), NetworkId: networkId, WalletHeadHash: providerAccepted.MappingHash, WalletGeneration: providerAccepted.MappingGeneration}
		siblingAbsent := payoutartifact.WholeWorkExpectedProvider{ClientId: [16]byte(*f.sibling.ClientId), NetworkId: networkId}
		siblingLater := payoutartifact.WholeWorkExpectedProvider{ClientId: [16]byte(*f.sibling.ClientId), NetworkId: networkId, WalletHeadHash: siblingAccepted.MappingHash, WalletGeneration: siblingAccepted.MappingGeneration}
		for _, sibling := range []payoutartifact.WholeWorkExpectedProvider{siblingAbsent, siblingLater} {
			authority, expected := roster([]payoutartifact.WholeWorkExpectedProvider{provider, sibling}, pinnedNetwork)
			wallets, err := model.GetStProviderWalletsForEpoch(t.Context(), authority, expected, f.scope)
			if err != nil || len(wallets) != 2 {
				t.Fatal("the precedence did not resolve both providers", sibling.WalletHeadHash, wallets, err)
			}
			own := wallets[*f.provider.ClientId]
			if own == nil || own.ColdkeyPubkey != f.key.Public().Encode() || own.Resolution == nil || own.Resolution.Mode != protocol.EarningWalletModeProvider || hex.EncodeToString(own.Resolution.HeadHash[:]) != providerAccepted.MappingHash || own.OriginalMessage == nil || *own.OriginalMessage != providerProof.Message {
				t.Fatal("the provider's own effective consent did not win", own)
			}
			fallback := wallets[*f.sibling.ClientId]
			if fallback == nil || fallback.ColdkeyPubkey != networkKey.Public().Encode() || fallback.ColdkeySs58 != networkAddress || fallback.Resolution == nil || fallback.Resolution.Mode != protocol.EarningWalletModeNetwork || fallback.Resolution.HeadHash != networkHead || fallback.Resolution.ConsentHash != networkHead || fallback.Resolution.ConsentGeneration != 1 || fallback.OriginalMessage == nil || *fallback.OriginalMessage != set.Message {
				t.Fatal("an absent or not yet effective provider chain did not fall back to the network consent", fallback)
			}
		}
		// a pinned provider chain whose originals are missing never falls back
		missing := provider
		missingHead := sha256.Sum256([]byte("synthetic-missing-provider-chain"))
		missing.WalletHeadHash = hex.EncodeToString(missingHead[:])
		authority, expected := roster([]payoutartifact.WholeWorkExpectedProvider{missing, siblingAbsent}, pinnedNetwork)
		if wallets, err := model.GetStProviderWalletsForEpoch(t.Context(), authority, expected, f.scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) || errors.Is(err, protocol.ErrWalletMappingAbsent) {
			t.Fatal("unreadable provider evidence fell back to the network consent", wallets, err)
		}
		// without a pinned network chain an absent provider stays unavailable
		authority, expected = roster([]payoutartifact.WholeWorkExpectedProvider{provider, siblingAbsent}, nil)
		if wallets, err := model.GetStProviderWalletsForEpoch(t.Context(), authority, expected, f.scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("a provider without any pinned consent gained a wallet", wallets, err)
		}
	})
}

// Resolutions are append-only: an exact retry is accepted, a different
// resolution of the same provider is refused and SQL cannot rewrite one.
func TestStPayoutWalletResolutionsAreAppendOnly(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		key := model.StDeploymentKey("synthetic-resolution")
		resolutions := []*model.StPayoutWalletResolution{
			{ClientId: server.Id{2}, NetworkId: server.Id{9}, Mode: protocol.EarningWalletModeNetwork, Coldkey: [32]byte{1}, ConsentHash: [32]byte{2}, ConsentGeneration: 1, HeadHash: [32]byte{2}, HeadGeneration: 1},
			{ClientId: server.Id{1}, NetworkId: server.Id{9}, Mode: protocol.EarningWalletModeProvider, Coldkey: [32]byte{3}, ConsentHash: [32]byte{4}, ConsentGeneration: 2, HeadHash: [32]byte{5}, HeadGeneration: 3},
		}
		for range 2 {
			if err := model.AddStPayoutWalletResolutions(t.Context(), key, 7, 1, resolutions); err != nil {
				t.Fatal("an exact resolution retry was refused", err)
			}
		}
		retained := model.GetStPayoutWalletResolutions(t.Context(), key, 7, 1)
		if len(retained) != 2 || *retained[0] != *resolutions[1] || *retained[1] != *resolutions[0] {
			t.Fatal("resolutions were not retained exactly in client order", retained)
		}
		changed := *resolutions[0]
		changed.Coldkey = [32]byte{6}
		if err := model.AddStPayoutWalletResolutions(t.Context(), key, 7, 1, []*model.StPayoutWalletResolution{&changed}); !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("a different resolution replaced the retained one", err)
		}
		invalid := *resolutions[0]
		invalid.Mode = "side_copy"
		if err := model.AddStPayoutWalletResolutions(t.Context(), key, 8, 1, []*model.StPayoutWalletResolution{&invalid}); !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("an unknown mode was retained", err)
		}
		var rewriteErr error
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					rewriteErr, _ = recovered.(error)
				}
			}()
			server.Tx(t.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(t.Context(), `UPDATE st_payout_wallet_resolution SET coldkey=$1 WHERE deployment_key=$2`, make([]byte, 32), string(key)))
			})
		}()
		if rewriteErr == nil {
			t.Fatal("SQL rewrote a retained resolution")
		}
		if retained := model.GetStPayoutWalletResolutions(t.Context(), key, 7, 1); len(retained) != 2 || retained[1].Coldkey != resolutions[0].Coldkey {
			t.Fatal("a refused rewrite changed a resolution", retained)
		}
	})
}
