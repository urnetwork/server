// Hotkey wallet mapping through the actual authenticated HTTP boundary: only
// the network owner's session retains a global chain, idempotently and within
// its network's hotkey links and the new generation bound, and a fork is
// refused; only that session issues and submits a delegation; neither changes
// a wallet projection; both histories serve the independent reader; GET
// /sn/wallet lists the delegation in settlement's precedence; and epoch
// settlement applies provider > network > hotkey without falling back on bad
// evidence. Keys, networks and clients are synthetic. The refusals before any
// read and the coded bounds are also tested without a database.
package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/validator"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// The wallet routes with the hotkey routes, using the same authentication and
// decoding as api/handlers.
func snHotkeyWalletControllerHttp(t testing.TB) *httptest.Server {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sn/wallet":
			if r.Method == http.MethodGet {
				router.WrapRequireAuth(SnGetWallet, w, r)
			} else {
				router.WrapWithInputRequireAuth(SnSetWallet, w, r)
			}
		case "/sn/wallet/consent":
			router.WrapWithInputRequireAuth(SnWalletMappingChallenge, w, r)
		case "/sn/wallet/network-consent":
			router.WrapWithInputRequireAuth(SnNetworkWalletMappingChallenge, w, r)
		case "/sn/wallet/network-consent/history":
			router.WrapWithInputNoAuth(SnNetworkWalletMappingHistory, w, r)
		case "/sn/wallet/hotkey-consent":
			router.WrapWithInputRequireAuth(SnHotkeyWalletMappingConsent, w, r)
		case "/sn/wallet/hotkey-consent/history":
			router.WrapWithInputNoAuth(SnHotkeyWalletMappingHistory, w, r)
		case "/sn/wallet/hotkey-delegation":
			router.WrapWithInputRequireAuth(SnHotkeyNetworkDelegationChallenge, w, r)
		case "/sn/wallet/hotkey-delegation/history":
			router.WrapWithInputNoAuth(SnHotkeyNetworkDelegationHistory, w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

// The network consent fixture, served with the hotkey routes too.
func newSnHotkeyWalletFixture(t testing.TB) *snWalletProviderScopeFixture {
	t.Helper()
	f := newSnWalletProviderScopeFixture(t)
	f.endpoint = snHotkeyWalletControllerHttp(t)
	return f
}

// The substrate-context sr25519 signature over the exact message.
func snHotkeyWalletSign(t testing.TB, key *schnorrkel.MiniSecretKey, message string) [64]byte {
	t.Helper()
	signature, err := key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	return signature.Encode()
}

// The chain with one more generation that both keys sign, effective for 101
// epochs from fromEpoch. The nonce seed tells apart two otherwise equal
// generations, which is how a test builds a fork.
func snHotkeyWalletConsentAppend(t testing.TB, chain []protocol.HotkeyWalletMappingConsent, subnet protocol.HotkeyWalletMappingSubnet, hotkey, coldkey *schnorrkel.MiniSecretKey, fromEpoch uint64, nonceSeed string) []protocol.HotkeyWalletMappingConsent {
	t.Helper()
	statement := protocol.HotkeyWalletMappingStatement{
		Schema: protocol.HotkeyWalletMappingConsentSchema, Scope: protocol.HotkeyWalletMappingScope, Subnet: subnet,
		Hotkey: hotkey.Public().Encode(), Coldkey: coldkey.Public().Encode(), Generation: uint64(len(chain)) + 1,
		Nonce: sha256.Sum256([]byte("synthetic-hotkey-consent-nonce/" + nonceSeed)), IssuedAt: server.NowUtc().Unix(),
		FromEpoch: fromEpoch, ThroughEpoch: fromEpoch + 100,
	}
	if 0 < len(chain) {
		_, previous, err := protocol.VerifyHotkeyWalletMappingConsent(t.Context(), chain[len(chain)-1])
		if err != nil {
			t.Fatal(err)
		}
		statement.PreviousHash = previous
	}
	message, err := statement.Message()
	if err != nil {
		t.Fatal(err)
	}
	original := protocol.HotkeyWalletMappingConsent{Message: message, ColdkeySignature: snHotkeyWalletSign(t, coldkey, message), HotkeySignature: snHotkeyWalletSign(t, hotkey, message)}
	return append(append([]protocol.HotkeyWalletMappingConsent(nil), chain...), original)
}

// The content hash of each original, which is each generation's head.
func snHotkeyWalletConsentHashes(t testing.TB, chain []protocol.HotkeyWalletMappingConsent) [][32]byte {
	t.Helper()
	hashes := make([][32]byte, len(chain))
	for index, original := range chain {
		_, hash, err := protocol.VerifyHotkeyWalletMappingConsent(t.Context(), original)
		if err != nil {
			t.Fatal(err)
		}
		hashes[index] = hash
	}
	return hashes
}

// POST /sn/wallet/hotkey-consent, unauthenticated for a nil credential.
func (self *snWalletProviderScopeFixture) submitHotkeyChain(t testing.TB, credential *jwt.ByJwt, chain []protocol.HotkeyWalletMappingConsent) (int, []byte) {
	t.Helper()
	token := ""
	if credential != nil {
		token = credential.Sign()
	}
	return walletMappingControllerPost(t, self.endpoint.URL, "/sn/wallet/hotkey-consent", token, &SnHotkeyWalletMappingConsentArgs{Originals: chain})
}

// A submission the operator retains.
func (self *snWalletProviderScopeFixture) retainHotkeyChain(t testing.TB, credential *jwt.ByJwt, chain []protocol.HotkeyWalletMappingConsent) SnHotkeyWalletMappingConsentResult {
	t.Helper()
	status, raw := self.submitHotkeyChain(t, credential, chain)
	var result SnHotkeyWalletMappingConsentResult
	if status != http.StatusOK || json.Unmarshal(raw, &result) != nil {
		t.Fatal("hotkey consent chain was not retained", status, string(raw))
	}
	return result
}

// Issue a delegation of the chain's head through the authenticated route, then
// sign the exact bytes with the hotkey and decode them independently.
func (self *snWalletProviderScopeFixture) hotkeyDelegation(t testing.TB, credential *jwt.ByJwt, hotkey *schnorrkel.MiniSecretKey, hotkeySs58 string, chain []protocol.HotkeyWalletMappingConsent, fromEpoch uint64) (*SnSetWalletArgs, protocol.HotkeyNetworkDelegationStatement, [32]byte) {
	t.Helper()
	hashes := snHotkeyWalletConsentHashes(t, chain)
	args := &SnHotkeyNetworkDelegationChallengeArgs{HotkeySs58: hotkeySs58, ConsentHeadHash: hashes[len(hashes)-1], ConsentGeneration: uint64(len(chain)), FromEpoch: fromEpoch, ThroughEpoch: fromEpoch + 100}
	status, raw := walletMappingControllerPost(t, self.endpoint.URL, "/sn/wallet/hotkey-delegation", credential.Sign(), args)
	var challenge SnWalletMappingChallengeResult
	if status != http.StatusOK || json.Unmarshal(raw, &challenge) != nil || challenge.Message == "" {
		t.Fatal("authorized hotkey delegation issuance failed", status, string(raw))
	}
	signature := snHotkeyWalletSign(t, hotkey, challenge.Message)
	statement, hash, err := protocol.VerifyHotkeyNetworkDelegation(t.Context(), protocol.WalletMappingConsent{Message: challenge.Message, Signature: signature})
	if err != nil {
		t.Fatal("issued hotkey delegation does not verify", err)
	}
	return &SnSetWalletArgs{ColdkeySs58: hotkeySs58, Message: challenge.Message, Signature: "0x" + hex.EncodeToString(signature[:])}, *statement, hash
}

// The hotkey chain, link and delegation tables and the wallet projections of
// the network. Each test database is fresh, so the global chains count whole.
type snHotkeyWalletRows struct {
	consents        int
	submitters      int
	challenges      int
	delegations     int
	providerWallets int
	networkWallets  int
}

func (self *snWalletProviderScopeFixture) hotkeyRows(t testing.TB) snHotkeyWalletRows {
	t.Helper()
	var rows snHotkeyWalletRows
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM hotkey_wallet_mapping_consent),
			(SELECT count(*) FROM hotkey_wallet_mapping_submitter WHERE network_id=$1),
			(SELECT count(*) FROM hotkey_network_delegation_challenge WHERE network_id=$1),
			(SELECT count(*) FROM hotkey_network_delegation WHERE network_id=$1),
			(SELECT count(*) FROM st_provider_wallet_history WHERE network_id=$1),
			(SELECT count(*) FROM st_wallet WHERE network_id=$1)`, self.provider.NetworkId).Scan(&rows.consents, &rows.submitters, &rows.challenges, &rows.delegations, &rows.providerWallets, &rows.networkWallets))
	})
	return rows
}

// A refusal that is not a 200 and carries the sentinel's text.
func snHotkeyWalletRefused(status int, raw []byte, sentinel error) bool {
	return status != http.StatusOK && strings.Contains(string(raw), sentinel.Error())
}

// The network owner's session retains the complete chain and links the
// network to its hotkey, a replay of it or of a prefix is idempotent, and a
// client session, a fork, another subnet or a partial chain is refused without
// changing what is retained. The history serves the independent reader only
// at a pinned head.
func TestSnHotkeyWalletConsentRetainsChainIdempotentlyAndRefusesForks(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnHotkeyWalletFixture(t)
		hotkey, hotkeySs58 := snNetworkWalletTestKey(t, 161)
		coldkey, _ := snNetworkWalletTestKey(t, 162)
		replacement, _ := snNetworkWalletTestKey(t, 163)
		subnet := f.rpc.domain.HotkeySubnet()
		first := snHotkeyWalletConsentAppend(t, nil, subnet, hotkey, coldkey, f.scope.Epoch, "first")
		chain := snHotkeyWalletConsentAppend(t, first, subnet, hotkey, replacement, f.scope.Epoch+5, "second")
		heads := snHotkeyWalletConsentHashes(t, chain)
		if status, _ := f.submitHotkeyChain(t, nil, first); status == http.StatusOK {
			t.Fatal("an unauthenticated caller retained a hotkey consent chain")
		}
		for _, credential := range []*jwt.ByJwt{f.provider, f.sibling} {
			if status, raw := f.submitHotkeyChain(t, credential, first); status == http.StatusOK || strings.TrimSpace(string(raw)) != protocol.ErrWalletMappingIntegrity.Error() {
				t.Fatal("a client session retained a hotkey consent chain", status, string(raw))
			}
		}
		if rows := f.hotkeyRows(t); rows != (snHotkeyWalletRows{}) {
			t.Fatal("refused submissions retained a chain or a link", rows)
		}
		for range 2 {
			if result := f.retainHotkeyChain(t, f.owner, first); result.HotkeySs58 != hotkeySs58 || result.HeadHash != heads[0] || result.Generation != 1 {
				t.Fatal("the retained chain lost its head", result)
			}
		}
		if rows := f.hotkeyRows(t); rows != (snHotkeyWalletRows{consents: 1, submitters: 1}) {
			t.Fatal("replays appended history, links or a wallet projection", rows)
		}
		if result := f.retainHotkeyChain(t, f.owner, chain); result.HeadHash != heads[1] || result.Generation != 2 {
			t.Fatal("the replacement generation was not appended", result)
		}
		if result := f.retainHotkeyChain(t, f.owner, first); result.HeadHash != heads[0] || result.Generation != 1 {
			t.Fatal("a prefix replay did not name its own head", result)
		}
		forkedSecond := snHotkeyWalletConsentAppend(t, first, subnet, hotkey, coldkey, f.scope.Epoch+5, "forked second")
		forkedFirst := snHotkeyWalletConsentAppend(t, nil, subnet, hotkey, coldkey, f.scope.Epoch, "forked first")
		otherSubnet := subnet
		otherSubnet.Netuid += 1
		foreign := snHotkeyWalletConsentAppend(t, nil, otherSubnet, hotkey, coldkey, f.scope.Epoch, "foreign")
		for _, refused := range [][]protocol.HotkeyWalletMappingConsent{forkedSecond, forkedFirst, foreign, chain[1:]} {
			if status, raw := f.submitHotkeyChain(t, f.owner, refused); !snHotkeyWalletRefused(status, raw, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("a fork, another subnet or a partial chain was retained", status, string(raw))
			}
		}
		if rows := f.hotkeyRows(t); rows != (snHotkeyWalletRows{consents: 2, submitters: 1}) {
			t.Fatal("refused chains changed the retained history or links", rows)
		}
		reader, err := validator.NewHttpWalletMappingReader(f.endpoint.URL)
		if err != nil {
			t.Fatal(err)
		}
		expectation := protocol.HotkeyWalletMappingHistoryExpectation{Subnet: subnet, Hotkey: hotkey.Public().Encode(), HeadHash: heads[1], Generation: 2, Epoch: f.scope.Epoch}
		originals, verified, err := reader.ReadHotkeyConsentBounded(t.Context(), expectation, protocol.MaxWalletMappingConsentBytes*4)
		if err != nil || verified == nil || len(originals) != 2 || originals[0] != chain[0] || originals[1] != chain[1] || verified.Statement.Coldkey != coldkey.Public().Encode() {
			t.Fatal("the independent reader lost the retained chain", verified, err)
		}
		expectation.HeadHash, expectation.Generation = heads[0], 1
		if originals, _, err := reader.ReadHotkeyConsentBounded(t.Context(), expectation, protocol.MaxWalletMappingConsentBytes*4); err != nil || len(originals) != 1 || originals[0] != chain[0] {
			t.Fatal("the history did not stop at the pinned head", err)
		}
		expectation.HeadHash, expectation.Generation = sha256.Sum256([]byte("another head")), 2
		if _, _, err := reader.ReadHotkeyConsentBounded(t.Context(), expectation, protocol.MaxWalletMappingConsentBytes*4); err == nil {
			t.Fatal("a history read without the pinned head succeeded")
		}
	})
}

// A network links at most model.MaxHotkeyWalletMappingNetworkHotkeys hotkeys:
// the next new hotkey is a 403 with its message, while a linked hotkey is
// resubmitted and extended at the bound and another network still links the
// refused one. One submission adds at most
// model.MaxHotkeyWalletMappingNewGenerations generations, a 400 otherwise, and
// a longer chain arrives after its prefix. A refused submission links nothing.
func TestSnHotkeyWalletConsentBoundsNetworkHotkeysAndNewGenerations(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnHotkeyWalletFixture(t)
		subnet := f.rpc.domain.HotkeySubnet()
		coldkey, _ := snNetworkWalletTestKey(t, 210)
		keys := make([]*schnorrkel.MiniSecretKey, model.MaxHotkeyWalletMappingNetworkHotkeys+1)
		chains := make([][]protocol.HotkeyWalletMappingConsent, len(keys))
		for index := range keys {
			keys[index], _ = snNetworkWalletTestKey(t, byte(211+index))
			chains[index] = snHotkeyWalletConsentAppend(t, nil, subnet, keys[index], coldkey, f.scope.Epoch, fmt.Sprintf("bounded %d", index))
		}
		for _, chain := range chains[:model.MaxHotkeyWalletMappingNetworkHotkeys] {
			f.retainHotkeyChain(t, f.owner, chain)
		}
		refused := chains[model.MaxHotkeyWalletMappingNetworkHotkeys]
		if status, raw := f.submitHotkeyChain(t, f.owner, refused); status != http.StatusForbidden || strings.TrimSpace(string(raw)) != model.ErrHotkeyWalletMappingNetworkHotkeys.Error() {
			t.Fatal("a new hotkey past the network's links was not the coded refusal", status, string(raw))
		}
		bound := model.MaxHotkeyWalletMappingNetworkHotkeys
		if rows := f.hotkeyRows(t); rows != (snHotkeyWalletRows{consents: bound, submitters: bound}) {
			t.Fatal("the refused hotkey was retained or linked", rows)
		}
		f.retainHotkeyChain(t, f.owner, chains[0])
		extended := snHotkeyWalletConsentAppend(t, chains[0], subnet, keys[0], coldkey, f.scope.Epoch+5, "bounded extension")
		if result := f.retainHotkeyChain(t, f.owner, extended); result.Generation != 2 {
			t.Fatal("a linked hotkey was not extended at the bound", result)
		}
		otherNetworkId, otherUserId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(t.Context(), otherNetworkId, "synthetic-hotkey-bound-"+otherNetworkId.String(), otherUserId)
		other := jwt.NewByJwt(otherNetworkId, otherUserId, "synthetic-hotkey-bound", false, false)
		f.retainHotkeyChain(t, other, refused)
		// a chain with one generation more than one submission may add
		longKey, _ := snNetworkWalletTestKey(t, 230)
		var long []protocol.HotkeyWalletMappingConsent
		for index := range model.MaxHotkeyWalletMappingNewGenerations + 1 {
			long = snHotkeyWalletConsentAppend(t, long, subnet, longKey, coldkey, f.scope.Epoch+uint64(index), fmt.Sprintf("long %d", index))
		}
		if status, raw := f.submitHotkeyChain(t, other, long); status != http.StatusBadRequest || strings.TrimSpace(string(raw)) != model.ErrHotkeyWalletMappingNewGenerations.Error() {
			t.Fatal("a submission of too many new generations was not the coded refusal", status, string(raw))
		}
		f.retainHotkeyChain(t, other, long[:model.MaxHotkeyWalletMappingNewGenerations])
		if result := f.retainHotkeyChain(t, other, long); result.Generation != uint64(len(long)) {
			t.Fatal("a long chain did not arrive after its prefix", result)
		}
		var otherLinks int
		server.Db(t.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(t.Context(), `SELECT count(*) FROM hotkey_wallet_mapping_submitter WHERE network_id=$1`, otherNetworkId).Scan(&otherLinks))
		})
		if rows := f.hotkeyRows(t); otherLinks != 2 || rows.submitters != bound || rows.consents != bound+2+len(long) {
			t.Fatal("the links or retained chains differ from the accepted submissions", otherLinks, rows)
		}
	})
}

// Only the network owner's session issues and submits a delegation, for a
// retained head of the named hotkey. An exact replay is idempotent, a second
// original for the same challenge is refused, and nothing changes a wallet
// projection. The history serves the independent reader at a pinned head.
func TestSnHotkeyNetworkDelegationOnlyNetworkOwnerAndNoProjection(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnHotkeyWalletFixture(t)
		hotkey, hotkeySs58 := snNetworkWalletTestKey(t, 171)
		coldkey, coldkeySs58 := snNetworkWalletTestKey(t, 172)
		chain := snHotkeyWalletConsentAppend(t, nil, f.rpc.domain.HotkeySubnet(), hotkey, coldkey, f.scope.Epoch, "delegated")
		head := snHotkeyWalletConsentHashes(t, chain)[0]
		args := &SnHotkeyNetworkDelegationChallengeArgs{HotkeySs58: hotkeySs58, ConsentHeadHash: head, ConsentGeneration: 1, FromEpoch: f.scope.Epoch, ThroughEpoch: f.scope.Epoch + 100}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/hotkey-delegation", f.owner.Sign(), args); !snHotkeyWalletRefused(status, raw, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("a delegation was issued before its chain was retained", status, string(raw))
		}
		f.retainHotkeyChain(t, f.owner, chain)
		for _, credential := range []*jwt.ByJwt{f.provider, f.sibling} {
			if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/hotkey-delegation", credential.Sign(), args); status == http.StatusOK || strings.TrimSpace(string(raw)) != protocol.ErrWalletMappingIntegrity.Error() {
				t.Fatal("a client session requested a hotkey delegation", status, string(raw))
			}
		}
		if status, _ := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/hotkey-delegation", "", args); status == http.StatusOK {
			t.Fatal("an unauthenticated caller requested a hotkey delegation")
		}
		// a head the operator does not retain for the named hotkey
		for _, change := range []func(*SnHotkeyNetworkDelegationChallengeArgs){
			func(other *SnHotkeyNetworkDelegationChallengeArgs) { other.HotkeySs58 = coldkeySs58 },
			func(other *SnHotkeyNetworkDelegationChallengeArgs) { other.ConsentGeneration = 2 },
			func(other *SnHotkeyNetworkDelegationChallengeArgs) { other.ConsentHeadHash[0] ^= 1 },
		} {
			other := *args
			change(&other)
			if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/hotkey-delegation", f.owner.Sign(), &other); !snHotkeyWalletRefused(status, raw, protocol.ErrWalletMappingUnavailable) {
				t.Fatal("a delegation was issued for a head that is not retained", status, string(raw))
			}
		}
		if rows := f.hotkeyRows(t); rows != (snHotkeyWalletRows{consents: 1, submitters: 1}) {
			t.Fatal("refused delegation issuance wrote state", rows)
		}
		set, statement, hash := f.hotkeyDelegation(t, f.owner, hotkey, hotkeySs58, chain, f.scope.Epoch)
		if statement.Schema != protocol.HotkeyNetworkDelegationSchema || statement.Scope != protocol.HotkeyNetworkDelegationScope || statement.Domain != f.rpc.domain || statement.UserId != [16]byte(f.owner.UserId) || statement.NetworkId != [16]byte(f.owner.NetworkId) || statement.Hotkey != hotkey.Public().Encode() || statement.ConsentHeadHash != head || statement.ConsentGeneration != 1 || statement.Generation != 1 || statement.FromEpoch != f.scope.Epoch || statement.ThroughEpoch != f.scope.Epoch+100 || statement.Prospective.Boundary != f.rpc.boundary || statement.Prospective.Signer != f.rpc.root || statement.ExpiresAt > f.scope.StartTime.Unix() {
			t.Fatal("the delegation differs from the authenticated owner, head or window", statement)
		}
		// a client session cannot submit it, a request naming a client is
		// refused, and coldkey_ss58 must name the signing hotkey
		named := *set
		named.ClientId = f.provider.ClientId
		otherSigner := *set
		otherSigner.ColdkeySs58 = coldkeySs58
		for _, refused := range []struct {
			credential *jwt.ByJwt
			set        *SnSetWalletArgs
		}{{f.provider, set}, {f.owner, &named}, {f.owner, &otherSigner}} {
			if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", refused.credential.Sign(), refused.set); !snHotkeyWalletRefused(status, raw, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("a delegation was accepted from a client, for a client or for another signer", status, string(raw))
			}
		}
		if rows := f.hotkeyRows(t); rows != (snHotkeyWalletRows{consents: 1, submitters: 1, challenges: 1}) {
			t.Fatal("refused delegation acceptance consumed the challenge", rows)
		}
		for range 2 {
			if accepted := f.accept(t, f.owner, set); accepted.MappingHash != hex.EncodeToString(hash[:]) || accepted.MappingGeneration != 1 {
				t.Fatal("delegation acceptance lost the original", accepted)
			}
		}
		// sr25519 signatures are randomized: a second signature over the same
		// challenge is another original for a consumed nonce
		resigned := *set
		signature := snHotkeyWalletSign(t, hotkey, set.Message)
		resigned.Signature = "0x" + hex.EncodeToString(signature[:])
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.owner.Sign(), &resigned); !snHotkeyWalletRefused(status, raw, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("a second original was accepted for one challenge", status, string(raw))
		}
		if rows := f.hotkeyRows(t); rows != (snHotkeyWalletRows{consents: 1, submitters: 1, challenges: 1, delegations: 1}) {
			t.Fatal("the delegation changed a wallet projection or appended twice", rows)
		}
		reader, err := validator.NewHttpWalletMappingReader(f.endpoint.URL)
		if err != nil {
			t.Fatal(err)
		}
		expectation := protocol.HotkeyNetworkDelegationHistoryExpectation{Domain: f.rpc.domain, NetworkId: [16]byte(f.owner.NetworkId), HeadHash: hash, Generation: 1, Epoch: f.scope.Epoch}
		originals, verified, err := reader.ReadHotkeyDelegationBounded(t.Context(), expectation, protocol.MaxWalletMappingConsentBytes*4)
		if err != nil || verified == nil || len(originals) != 1 || originals[0].Message != set.Message || verified.Statement.Hotkey != hotkey.Public().Encode() || verified.Statement.ConsentHeadHash != head {
			t.Fatal("the independent reader lost the accepted delegation", verified, err)
		}
		expectation.HeadHash = sha256.Sum256([]byte("another head"))
		if _, _, err := reader.ReadHotkeyDelegationBounded(t.Context(), expectation, protocol.MaxWalletMappingConsentBytes*4); err == nil {
			t.Fatal("a delegation history read without the pinned head succeeded")
		}
	})
}

// Another key's signature is the coded mismatch; a user who does not
// administer the network, an expired challenge and a seventeenth open
// challenge are refused; and no kind is accepted as another.
func TestSnHotkeyNetworkDelegationRefusals(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnHotkeyWalletFixture(t)
		hotkey, hotkeySs58 := snNetworkWalletTestKey(t, 191)
		coldkey, _ := snNetworkWalletTestKey(t, 192)
		chain := snHotkeyWalletConsentAppend(t, nil, f.rpc.domain.HotkeySubnet(), hotkey, coldkey, f.scope.Epoch, "refusals")
		head := snHotkeyWalletConsentHashes(t, chain)[0]
		f.retainHotkeyChain(t, f.owner, chain)
		set, _, _ := f.hotkeyDelegation(t, f.owner, hotkey, hotkeySs58, chain, f.scope.Epoch)
		mismatch := *set
		other := snHotkeyWalletSign(t, coldkey, set.Message)
		mismatch.Signature = "0x" + hex.EncodeToString(other[:])
		status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.owner.Sign(), &mismatch)
		var refused SnSetWalletResult
		if status != http.StatusOK || json.Unmarshal(raw, &refused) != nil || refused.Error == nil || refused.Error.Code != SnSetWalletErrorCodeSignatureMismatch {
			t.Fatal("another key's signature was not the coded mismatch", status, string(raw))
		}
		// a session of a user who does not administer the network does not
		// authenticate; the model refuses that owner even so
		foreign := jwt.NewByJwt(f.owner.NetworkId, server.NewId(), "synthetic-foreign-user", false, false)
		args := &SnHotkeyNetworkDelegationChallengeArgs{HotkeySs58: hotkeySs58, ConsentHeadHash: head, ConsentGeneration: 1, FromEpoch: f.scope.Epoch, ThroughEpoch: f.scope.Epoch + 100}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/hotkey-delegation", foreign.Sign(), args); status == http.StatusOK {
			t.Fatal("a user who does not administer the network requested its delegation", string(raw))
		}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", foreign.Sign(), set); status == http.StatusOK {
			t.Fatal("a user who does not administer the network submitted its delegation", string(raw))
		}
		cfg := stConfig()
		if cfg == nil || cfg.RootKey == nil {
			t.Fatal("fixture operator owner absent")
		}
		prospective := &model.WalletMappingProspectiveOwner{Boundary: f.rpc.boundary, RootKey: cfg.RootKey}
		owner := model.NetworkWalletMappingOwner{Domain: f.rpc.domain, UserId: server.NewId(), NetworkId: f.owner.NetworkId, Prospective: prospective}
		if message, err := model.CreateHotkeyNetworkDelegationChallenge(t.Context(), owner, hotkey.Public().Encode(), head, 1, f.scope.Epoch, f.scope.Epoch+100); message != "" || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("a user who does not administer the network was issued its delegation", err)
		}
		owner.UserId, owner.NetworkId = f.owner.UserId, server.NewId()
		if message, err := model.CreateHotkeyNetworkDelegationChallenge(t.Context(), owner, hotkey.Public().Encode(), head, 1, f.scope.Epoch, f.scope.Epoch+100); message != "" || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("a missing network was issued a delegation", err)
		}
		owner.NetworkId, owner.Prospective = f.owner.NetworkId, nil
		if message, err := model.CreateHotkeyNetworkDelegationChallenge(t.Context(), owner, hotkey.Public().Encode(), head, 1, f.scope.Epoch, f.scope.Epoch+100); message != "" || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("a delegation was issued without the operator boundary", err)
		}
		// an issued challenge whose acceptance window has passed
		domainHash, err := f.rpc.domain.Digest()
		if err != nil {
			t.Fatal(err)
		}
		now := server.NowUtc().Unix()
		expired := protocol.HotkeyNetworkDelegationStatement{Domain: f.rpc.domain, UserId: [16]byte(f.owner.UserId), NetworkId: [16]byte(f.owner.NetworkId), Hotkey: hotkey.Public().Encode(), ConsentHeadHash: head, ConsentGeneration: 1, Generation: 1, Nonce: sha256.Sum256([]byte("synthetic-expired-delegation")), IssuedAt: now - 900, ExpiresAt: now - 600, FromEpoch: f.scope.Epoch, ThroughEpoch: f.scope.Epoch + 100}
		if err := protocol.SignProspectiveHotkeyNetworkDelegation(&expired, f.rpc.boundary, cfg.RootKey); err != nil {
			t.Fatal(err)
		}
		expiredMessage, err := expired.Message()
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO hotkey_network_delegation_challenge(nonce,domain_hash,network_id,generation,expires_at,message) VALUES($1,$2,$3,1,$4,$5)`, expired.Nonce[:], domainHash[:], f.owner.NetworkId, expired.ExpiresAt, expiredMessage))
		})
		expiredSignature := snHotkeyWalletSign(t, hotkey, expiredMessage)
		expiredSet := &SnSetWalletArgs{ColdkeySs58: hotkeySs58, Message: expiredMessage, Signature: "0x" + hex.EncodeToString(expiredSignature[:])}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.owner.Sign(), expiredSet); status == http.StatusOK || !strings.Contains(string(raw), "expired") {
			t.Fatal("an expired delegation challenge was accepted", status, string(raw))
		}
		// no kind is accepted as another: a network consent or a delegation as
		// a global chain, a global consent at the accept route
		networkKey, networkAddress := snNetworkWalletTestKey(t, 193)
		networkSet, _, networkHead := f.networkConsent(t, f.owner, networkKey, networkAddress, f.scope.Epoch)
		for _, message := range []string{networkSet.Message, set.Message} {
			impostor := []protocol.HotkeyWalletMappingConsent{{Message: message, ColdkeySignature: snHotkeyWalletSign(t, coldkey, message), HotkeySignature: snHotkeyWalletSign(t, hotkey, message)}}
			if status, raw := f.submitHotkeyChain(t, f.owner, impostor); !snHotkeyWalletRefused(status, raw, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("another kind was retained as a global hotkey consent", status, string(raw))
			}
		}
		global := &SnSetWalletArgs{ColdkeySs58: hotkeySs58, Message: chain[0].Message, Signature: "0x" + hex.EncodeToString(chain[0].HotkeySignature[:])}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet", f.owner.Sign(), global); !snHotkeyWalletRefused(status, raw, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("a global hotkey consent was accepted at the accept route", status, string(raw))
		}
		// the delegation history never serves a network consent chain
		f.accept(t, f.owner, networkSet)
		history := &SnHotkeyNetworkDelegationHistoryArgs{Domain: f.rpc.domain, NetworkId: f.owner.NetworkId, HeadHash: networkHead, Generation: 1}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/hotkey-delegation/history", "", history); !snHotkeyWalletRefused(status, raw, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("the delegation history served a network consent", status, string(raw))
		}
		// one challenge is open (the mismatch was never accepted); fifteen more
		// fill the bound and the next is refused
		for range 15 {
			if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/hotkey-delegation", f.owner.Sign(), args); status != http.StatusOK {
				t.Fatal("an open challenge below the bound was refused", status, string(raw))
			}
		}
		if status, raw := walletMappingControllerPost(t, f.endpoint.URL, "/sn/wallet/hotkey-delegation", f.owner.Sign(), args); status == http.StatusOK || !strings.Contains(string(raw), "capacity") {
			t.Fatal("a seventeenth open delegation challenge was issued", status, string(raw))
		}
		if rows := f.hotkeyRows(t); rows.delegations != 0 || rows.challenges != 17 {
			t.Fatal("refusals retained a delegation or lost a challenge", rows)
		}
	})
}

// GET /sn/wallet lists the delegation after the network consent and before
// the side copy. It shows the coldkey of the global consent effective at the
// mirrored epoch, else of the pinned head, and is effective after the network
// consent for both session kinds and before a client's own wallet without a
// consent.
func TestSnGetWalletShowsHotkeyDelegationInPrecedence(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnHotkeyWalletFixture(t)
		hotkey, hotkeySs58 := snNetworkWalletTestKey(t, 181)
		first, firstSs58 := snNetworkWalletTestKey(t, 182)
		second, secondSs58 := snNetworkWalletTestKey(t, 183)
		subnet := f.rpc.domain.HotkeySubnet()
		chain := snHotkeyWalletConsentAppend(t, nil, subnet, hotkey, first, f.scope.Epoch, "display first")
		chain = snHotkeyWalletConsentAppend(t, chain, subnet, hotkey, second, f.scope.Epoch+5, "display second")
		heads := snHotkeyWalletConsentHashes(t, chain)
		f.retainHotkeyChain(t, f.owner, chain)
		if result := f.getWallet(t, f.owner); result.Wallet != nil || len(result.Wallets) != 0 {
			t.Fatal("a retained global chain showed a wallet without a delegation", result)
		}
		set, _, hash := f.hotkeyDelegation(t, f.owner, hotkey, hotkeySs58, chain, f.scope.Epoch)
		f.accept(t, f.owner, set)
		// without a mirrored epoch the pinned head's coldkey shows
		want := SnWallet{ColdkeySs58: secondSs58, ConsentScope: SnWalletConsentScopeHotkey, FromEpoch: f.scope.Epoch, ThroughEpoch: f.scope.Epoch + 100, HotkeySs58: hotkeySs58, ConsentHeadHash: "0x" + hex.EncodeToString(heads[1][:]), ConsentGeneration: 2, MappingHash: "0x" + hex.EncodeToString(hash[:]), MappingGeneration: 1}
		for _, credential := range []*jwt.ByJwt{f.owner, f.provider, f.sibling} {
			result := f.getWallet(t, credential)
			if result.Wallet == nil || result.Wallet.SetAtMillis == 0 || len(result.Wallets) != 1 {
				t.Fatal("the delegation is not every session's effective wallet", result)
			}
			got := *result.Wallet
			got.SetAtMillis = 0
			if got != want || result.Wallets[0] != *result.Wallet {
				t.Fatal("the delegation entry differs", got, want)
			}
		}
		// the mirrored current epoch selects the consent effective then; none
		// effective shows the pinned head
		cfg := stConfig()
		for _, c := range []struct {
			epoch   uint64
			coldkey string
		}{{f.scope.Epoch, firstSs58}, {f.scope.Epoch + 5, secondSs58}, {0, secondSs58}, {f.scope.Epoch + 200, secondSs58}} {
			model.SetStEpochSummaryCache(t.Context(), cfg.DeploymentKey(), &model.StEpochSummary{Epoch: c.epoch}, time.Minute)
			if result := f.getWallet(t, f.owner); result.Wallet == nil || result.Wallet.ColdkeySs58 != c.coldkey || result.Wallet.ConsentHeadHash != want.ConsentHeadHash {
				t.Fatal("the delegation shows another coldkey than the consent effective at the epoch", c.epoch, result.Wallet)
			}
		}
		model.SetStEpochSummaryCache(t.Context(), cfg.DeploymentKey(), &model.StEpochSummary{Epoch: f.scope.Epoch}, time.Minute)
		// a login proof writes the sibling's own wallet without a consent and the
		// side copy; both rank after the delegation
		login, _ := f.loginProof(t, f.sibling.ClientId)
		f.accept(t, f.sibling, login)
		for _, credential := range []*jwt.ByJwt{f.owner, f.sibling} {
			if result := f.getWallet(t, credential); result.Wallet == nil || result.Wallet.ConsentScope != SnWalletConsentScopeHotkey || result.Wallet.ColdkeySs58 != firstSs58 {
				t.Fatal("a wallet without a consent displaced the delegation", result)
			}
		}
		result := f.getWallet(t, f.owner)
		if len(result.Wallets) != 3 || result.Wallets[0].ConsentScope != SnWalletConsentScopeHotkey || result.Wallets[1].ConsentScope != "" || result.Wallets[1].ClientId != nil || result.Wallets[2].ClientId == nil || *result.Wallets[2].ClientId != *f.sibling.ClientId || result.Wallets[1].HotkeySs58 != "" || result.Wallets[2].MappingHash != "" {
			t.Fatal("the delegation is not listed before the side copy, or other scopes gained hotkey fields", result.Wallets)
		}
		// the provider's own consent wins for the provider only
		proof, _, _ := f.consent(t, f.provider, nil)
		f.accept(t, f.provider, proof)
		if result := f.getWallet(t, f.provider); result.Wallet == nil || result.Wallet.ConsentScope != SnWalletConsentScopeProvider || result.Wallet.ClientId == nil || *result.Wallet.ClientId != *f.provider.ClientId {
			t.Fatal("the provider's own consent is not its effective wallet", result)
		}
		// the network consent is listed first and wins for the other sessions
		networkKey, networkSs58 := snNetworkWalletTestKey(t, 184)
		networkSet, _, _ := f.networkConsent(t, f.owner, networkKey, networkSs58, f.scope.Epoch)
		f.accept(t, f.owner, networkSet)
		result = f.getWallet(t, f.owner)
		if len(result.Wallets) != 5 || result.Wallets[0].ConsentScope != SnWalletConsentScopeNetwork || result.Wallets[1].ConsentScope != SnWalletConsentScopeHotkey || result.Wallets[2].ConsentScope != "" || result.Wallets[2].ClientId != nil {
			t.Fatal("the network consent, the delegation and the side copy lost their order", result.Wallets)
		}
		for _, credential := range []*jwt.ByJwt{f.owner, f.sibling} {
			if result := f.getWallet(t, credential); result.Wallet == nil || result.Wallet.ConsentScope != SnWalletConsentScopeNetwork || result.Wallet.ColdkeySs58 != networkSs58 {
				t.Fatal("the delegation displaced the network consent", result)
			}
		}
		if result := f.getWallet(t, f.provider); result.Wallet == nil || result.Wallet.ConsentScope != SnWalletConsentScopeProvider {
			t.Fatal("the network consent displaced the provider's own consent", result)
		}
	})
}

// A v3 roster signed by the separate roster key, with owners and providers in
// client order and the given network wallets and delegations.
func (self *snWalletProviderScopeFixture) hotkeyRoster(t testing.TB, scope model.StProviderWalletEpochScope, providers []payoutartifact.WholeWorkExpectedProvider, networks []payoutartifact.WholeWorkNetworkWallet, delegations []payoutartifact.WholeWorkHotkeyDelegation) ([]byte, payoutartifact.WholeWorkExpectation) {
	t.Helper()
	rosterKey, err := crypto.HexToECDSA(strings.Repeat("46", 32))
	if err != nil {
		t.Fatal(err)
	}
	ordered := append([]payoutartifact.WholeWorkExpectedProvider(nil), providers...)
	sort.Slice(ordered, func(i, j int) bool {
		return bytes.Compare(ordered[i].ClientId[:], ordered[j].ClientId[:]) < 0
	})
	authority := payoutartifact.WholeWorkAuthority{
		Domain: scope.Domain, Epoch: scope.Epoch, Start: scope.Start, End: scope.End,
		RequestPublicKey: [32]byte{150}, PriorContracts: []payoutartifact.WholeWorkPriorContract{},
		Owners: []payoutartifact.WholeWorkOwner{}, ExpectedProviders: ordered, NetworkWallets: networks, HotkeyDelegations: delegations,
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
	return raw, payoutartifact.WholeWorkExpectation{AuthoritySigner: crypto.PubkeyToAddress(rosterKey.PublicKey), ClientKeyRootSigner: self.rpc.root, AuthorityHash: "sha256:" + hex.EncodeToString(digest[:])}
}

// A v3 roster pins the provider chain, the network chain and the delegation.
// The provider's own effective consent wins, then the network's consent
// effective at the epoch, then the delegation; a chain the roster pins but
// cannot be read never falls back. The audit records the hotkey resolution.
func TestStProviderWalletsForEpochHotkeyPrecedence(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnHotkeyWalletFixture(t)
		hotkey, hotkeySs58 := snNetworkWalletTestKey(t, 201)
		coldkey, coldkeySs58 := snNetworkWalletTestKey(t, 202)
		chain := snHotkeyWalletConsentAppend(t, nil, f.rpc.domain.HotkeySubnet(), hotkey, coldkey, f.scope.Epoch, "settled")
		consentHead := snHotkeyWalletConsentHashes(t, chain)[0]
		f.retainHotkeyChain(t, f.owner, chain)
		delegationSet, _, delegationHash := f.hotkeyDelegation(t, f.owner, hotkey, hotkeySs58, chain, f.scope.Epoch)
		delegationAccepted := f.accept(t, f.owner, delegationSet)
		// the network consent is effective one epoch after the delegation
		networkKey, networkSs58 := snNetworkWalletTestKey(t, 203)
		networkSet, _, _ := f.networkConsent(t, f.owner, networkKey, networkSs58, f.scope.Epoch+1)
		networkAccepted := f.accept(t, f.owner, networkSet)
		providerProof, _, _ := f.consent(t, f.provider, nil)
		providerAccepted := f.accept(t, f.provider, providerProof)
		networkId := [16]byte(f.owner.NetworkId)
		provider := payoutartifact.WholeWorkExpectedProvider{ClientId: [16]byte(*f.provider.ClientId), NetworkId: networkId, WalletHeadHash: providerAccepted.MappingHash, WalletGeneration: providerAccepted.MappingGeneration}
		sibling := payoutartifact.WholeWorkExpectedProvider{ClientId: [16]byte(*f.sibling.ClientId), NetworkId: networkId}
		pinnedNetwork := []payoutartifact.WholeWorkNetworkWallet{{NetworkId: networkId, WalletHeadHash: networkAccepted.MappingHash, WalletGeneration: networkAccepted.MappingGeneration}}
		pinnedDelegation := []payoutartifact.WholeWorkHotkeyDelegation{{NetworkId: networkId, DelegationHeadHash: delegationAccepted.MappingHash, DelegationGeneration: delegationAccepted.MappingGeneration}}
		providers := []payoutartifact.WholeWorkExpectedProvider{provider, sibling}
		settle := func(scope model.StProviderWalletEpochScope, providers []payoutartifact.WholeWorkExpectedProvider, networks []payoutartifact.WholeWorkNetworkWallet, delegations []payoutartifact.WholeWorkHotkeyDelegation) (map[server.Id]*model.StProviderWallet, error) {
			authority, expected := f.hotkeyRoster(t, scope, providers, networks, delegations)
			return model.GetStProviderWalletsForEpoch(t.Context(), authority, expected, scope)
		}
		// at the epoch the network consent is not effective yet: the provider
		// keeps its own consent and the sibling earns to the delegation, with
		// or without the network chain pinned
		var settled []*model.StPayoutWalletResolution
		for _, networks := range [][]payoutartifact.WholeWorkNetworkWallet{pinnedNetwork, nil} {
			wallets, err := settle(f.scope, providers, networks, pinnedDelegation)
			if err != nil || len(wallets) != 2 {
				t.Fatal("the precedence did not resolve both providers", wallets, err)
			}
			own := wallets[*f.provider.ClientId]
			if own == nil || own.Resolution == nil || own.Resolution.Mode != protocol.EarningWalletModeProvider || own.ColdkeyPubkey != f.key.Public().Encode() || own.Resolution.Hotkey != ([32]byte{}) {
				t.Fatal("the provider's own effective consent did not win", own)
			}
			delegated := wallets[*f.sibling.ClientId]
			want := model.StPayoutWalletResolution{ClientId: *f.sibling.ClientId, NetworkId: f.owner.NetworkId, Mode: protocol.EarningWalletModeHotkey, Coldkey: coldkey.Public().Encode(), ConsentHash: delegationHash, ConsentGeneration: 1, HeadHash: delegationHash, HeadGeneration: 1, Hotkey: hotkey.Public().Encode(), HotkeyConsentHash: consentHead, HotkeyConsentGeneration: 1, HotkeyConsentHeadHash: consentHead, HotkeyConsentHeadGeneration: 1}
			if delegated == nil || delegated.Resolution == nil || *delegated.Resolution != want || delegated.ColdkeySs58 != coldkeySs58 || delegated.ColdkeyPubkey != coldkey.Public().Encode() || delegated.OriginalMessage == nil || *delegated.OriginalMessage != chain[0].Message || delegated.OriginalSignature == nil || *delegated.OriginalSignature != "0x"+hex.EncodeToString(chain[0].ColdkeySignature[:]) {
				t.Fatal("an absent own chain and a not yet effective network consent did not fall back to the delegation", delegated)
			}
			settled = []*model.StPayoutWalletResolution{own.Resolution, delegated.Resolution}
		}
		// the release payout retains the settled resolutions, idempotently
		auditKey := model.StDeploymentKey("synthetic-hotkey-settlement")
		for range 2 {
			if err := model.AddStPayoutWalletResolutions(t.Context(), auditKey, f.scope.Epoch, 1, settled); err != nil {
				t.Fatal("the settled resolutions were not retained", err)
			}
		}
		sort.Slice(settled, func(i, j int) bool {
			return bytes.Compare(settled[i].ClientId[:], settled[j].ClientId[:]) < 0
		})
		if retained := model.GetStPayoutWalletResolutions(t.Context(), auditKey, f.scope.Epoch, 1); len(retained) != 2 || *retained[0] != *settled[0] || *retained[1] != *settled[1] {
			t.Fatal("the audit lost a settled resolution", retained)
		}
		// one epoch later the network consent is effective and wins over the
		// delegation
		later := f.scope
		later.Epoch += 1
		wallets, err := settle(later, providers, pinnedNetwork, pinnedDelegation)
		if err != nil || len(wallets) != 2 {
			t.Fatal("the later epoch did not resolve both providers", wallets, err)
		}
		if network := wallets[*f.sibling.ClientId]; network == nil || network.Resolution == nil || network.Resolution.Mode != protocol.EarningWalletModeNetwork || network.ColdkeySs58 != networkSs58 || network.Resolution.Hotkey != ([32]byte{}) {
			t.Fatal("the effective network consent did not win over the delegation", network)
		}
		// pinned evidence that cannot be read never falls back
		missingHead := sha256.Sum256([]byte("synthetic-missing-chain"))
		missing := hex.EncodeToString(missingHead[:])
		missingProvider := provider
		missingProvider.WalletHeadHash = missing
		missingNetwork := []payoutartifact.WholeWorkNetworkWallet{{NetworkId: networkId, WalletHeadHash: missing, WalletGeneration: 1}}
		missingDelegation := []payoutartifact.WholeWorkHotkeyDelegation{{NetworkId: networkId, DelegationHeadHash: missing, DelegationGeneration: 1}}
		for _, c := range []struct {
			name        string
			providers   []payoutartifact.WholeWorkExpectedProvider
			networks    []payoutartifact.WholeWorkNetworkWallet
			delegations []payoutartifact.WholeWorkHotkeyDelegation
		}{
			{name: "unreadable provider chain", providers: []payoutartifact.WholeWorkExpectedProvider{missingProvider, sibling}, networks: pinnedNetwork, delegations: pinnedDelegation},
			{name: "unreadable network chain", providers: providers, networks: missingNetwork, delegations: pinnedDelegation},
			{name: "unreadable delegation", providers: providers, networks: pinnedNetwork, delegations: missingDelegation},
		} {
			if wallets, err := settle(f.scope, c.providers, c.networks, c.delegations); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) || errors.Is(err, protocol.ErrWalletMappingAbsent) || errors.Is(err, protocol.ErrWalletMappingNotEffective) {
				t.Fatal("pinned evidence that cannot be read fell back", c.name, wallets, err)
			}
		}
		// with no delegation pinned, the hotkey step is the final outcome: a
		// network consent that is not effective leaves the sibling absent
		if wallets, err := settle(f.scope, providers, pinnedNetwork, nil); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) || !errors.Is(err, protocol.ErrWalletMappingAbsent) {
			t.Fatal("a provider without an effective consent gained a wallet", wallets, err)
		}
	})
}

// Hotkey resolutions carry every hotkey field and other modes none. An exact
// retry is accepted, a different resolution is refused, and SQL refuses a
// hotkey row without its columns.
func TestStPayoutWalletResolutionsRecordHotkeyMode(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		key := model.StDeploymentKey("synthetic-hotkey-resolution")
		hotkey := model.StPayoutWalletResolution{ClientId: server.Id{3}, NetworkId: server.Id{9}, Mode: protocol.EarningWalletModeHotkey, Coldkey: [32]byte{1}, ConsentHash: [32]byte{2}, ConsentGeneration: 1, HeadHash: [32]byte{2}, HeadGeneration: 1, Hotkey: [32]byte{7}, HotkeyConsentHash: [32]byte{8}, HotkeyConsentGeneration: 2, HotkeyConsentHeadHash: [32]byte{9}, HotkeyConsentHeadGeneration: 3}
		network := model.StPayoutWalletResolution{ClientId: server.Id{4}, NetworkId: server.Id{9}, Mode: protocol.EarningWalletModeNetwork, Coldkey: [32]byte{5}, ConsentHash: [32]byte{6}, ConsentGeneration: 1, HeadHash: [32]byte{6}, HeadGeneration: 1}
		for range 2 {
			if err := model.AddStPayoutWalletResolutions(t.Context(), key, 7, 1, []*model.StPayoutWalletResolution{&network, &hotkey}); err != nil {
				t.Fatal("an exact hotkey resolution retry was refused", err)
			}
		}
		if retained := model.GetStPayoutWalletResolutions(t.Context(), key, 7, 1); len(retained) != 2 || *retained[0] != hotkey || *retained[1] != network {
			t.Fatal("hotkey resolutions were not retained exactly", retained)
		}
		changed := hotkey
		changed.HotkeyConsentGeneration = 1
		incomplete := []func(*model.StPayoutWalletResolution){
			func(resolution *model.StPayoutWalletResolution) { resolution.Hotkey = [32]byte{} },
			func(resolution *model.StPayoutWalletResolution) { resolution.HotkeyConsentHash = [32]byte{} },
			func(resolution *model.StPayoutWalletResolution) { resolution.HotkeyConsentGeneration = 0 },
			func(resolution *model.StPayoutWalletResolution) { resolution.HotkeyConsentHeadHash = [32]byte{} },
			func(resolution *model.StPayoutWalletResolution) { resolution.HotkeyConsentHeadGeneration = 0 },
		}
		refused := []model.StPayoutWalletResolution{changed}
		for _, clear := range incomplete {
			resolution := hotkey
			resolution.ClientId = server.Id{5}
			clear(&resolution)
			refused = append(refused, resolution)
		}
		leaked := network
		leaked.Hotkey = [32]byte{7}
		refused = append(refused, leaked)
		for _, resolution := range refused {
			if err := model.AddStPayoutWalletResolutions(t.Context(), key, 7, 1, []*model.StPayoutWalletResolution{&resolution}); !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("a different, incomplete or leaked hotkey resolution was retained", resolution, err)
			}
		}
		var insertErr error
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					insertErr, _ = recovered.(error)
				}
			}()
			server.Tx(t.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO st_payout_wallet_resolution(deployment_key,epoch,no_id,client_id,network_id,mode,coldkey,consent_hash,consent_generation,head_hash,head_generation) VALUES($1,8,1,$2,$3,'hotkey',$4,$4,1,$4,1)`, string(key), server.Id{6}, server.Id{9}, make([]byte, 32)))
			})
		}()
		if insertErr == nil {
			t.Fatal("SQL retained a hotkey resolution without its hotkey columns")
		}
		if retained := model.GetStPayoutWalletResolutions(t.Context(), key, 7, 1); len(retained) != 2 || *retained[0] != hotkey {
			t.Fatal("a refused resolution changed the retained ones", retained)
		}
	})
}

// A client JWT is refused before anything is read, like the network consent
// challenge, and so are a session without a JWT and an empty chain. Pure: no
// database.
func TestSnHotkeyWalletMappingConsentRefusesBeforeAnyRead(t *testing.T) {
	owner := jwt.NewByJwt(server.NewId(), server.NewId(), "synthetic-hotkey-owner", false, false)
	args := &SnHotkeyWalletMappingConsentArgs{Originals: []protocol.HotkeyWalletMappingConsent{{Message: protocol.HotkeyWalletMappingConsentPrefix + "{}"}}}
	for _, c := range []struct {
		name    string
		args    *SnHotkeyWalletMappingConsentArgs
		byJwt   *jwt.ByJwt
		refusal error
	}{
		{name: "client JWT", args: args, byJwt: owner.Client(server.NewId(), server.NewId()), refusal: protocol.ErrWalletMappingIntegrity},
		{name: "no JWT", args: args, refusal: protocol.ErrWalletMappingUnavailable},
		{name: "empty chain", args: &SnHotkeyWalletMappingConsentArgs{}, byJwt: owner, refusal: protocol.ErrWalletMappingIntegrity},
	} {
		clientSession := session.NewLocalClientSession(t.Context(), "192.0.2.1:1", c.byJwt)
		if result, err := SnHotkeyWalletMappingConsent(c.args, clientSession); result != nil || !errors.Is(err, c.refusal) {
			t.Errorf("%s: %v, %v; want %v", c.name, result, err, c.refusal)
		}
	}
}

// The submission bounds reach the client as their status and message, and
// every other refusal keeps its own. Pure: no database.
func TestSnHotkeyWalletMappingRefusalsAreCoded(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
	}{
		{err: model.ErrHotkeyWalletMappingNetworkHotkeys, status: http.StatusForbidden},
		{err: model.ErrHotkeyWalletMappingNewGenerations, status: http.StatusBadRequest},
		{err: protocol.ErrWalletMappingIntegrity, status: http.StatusInternalServerError},
	} {
		refusal := snHotkeyWalletMappingRefusal(c.err)
		recorder := httptest.NewRecorder()
		router.RaiseHttpError(refusal, recorder)
		if !errors.Is(refusal, c.err) || recorder.Code != c.status || strings.TrimSpace(recorder.Body.String()) != c.err.Error() {
			t.Errorf("%v: status %d, body %q", c.err, recorder.Code, recorder.Body.String())
		}
	}
}
