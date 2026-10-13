package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/v2026/miner/onchain"
	"github.com/urfoundation/sn/v2026/ss58"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// System.Account's storage prefix is a published constant of every substrate
// chain: twox128("System") ‖ twox128("Account").
func TestSnSubstrateAccountKey(t *testing.T) {
	var pubkey [32]byte
	for i := range pubkey {
		pubkey[i] = byte(i)
	}
	key := snSubstrateAccountKey(pubkey)
	const prefix = "26aa394eea5630e07c48ae0c9558cef7b99d880ec681799c0cf30e8886371da9"
	got := hex.EncodeToString(key)
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("System.Account prefix: got %s", got[:64])
	}
	if len(key) != 16+16+16+32 || !strings.HasSuffix(got, hex.EncodeToString(pubkey[:])) {
		t.Fatalf("key must end with blake2_128(pubkey) ‖ pubkey: %s", got)
	}
}

func TestSnStorageValuePresent(t *testing.T) {
	cases := map[string]bool{
		``:             false,
		`null`:         false,
		`"0x"`:         false,
		`"0x00"`:       true,
		`"0x0102ab"`:   true,
		`{"a": 1}`:     false,
		` "0x0a" `:     true,
		`"not-hex-ok"`: true,
	}
	for raw, want := range cases {
		if got := snStorageValuePresent(json.RawMessage(raw)); got != want {
			t.Errorf("%q: got %v, want %v", raw, got, want)
		}
	}
}

// An invalid address never reaches the chain; a valid one with no configured
// rpc fails open with a message so an outage cannot block a user.
func TestSnValidateWalletSyntaxAndFailOpen(t *testing.T) {
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	result, err := SnValidateWallet(&SnValidateWalletArgs{Address: "not-an-address"}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	if result.ValidSyntax || result.Banned || result.Message == "" {
		t.Fatalf("invalid address: %+v", result)
	}
	// a Solana-looking address is not ss58 prefix 42 either
	result, _ = SnValidateWallet(&SnValidateWalletArgs{Address: "7Gx4kQ9pT2nVb8sLmR3wYcJ6dHfA5eN1zK2uP9tXq4Wb"}, clientSession)
	if result.ValidSyntax {
		t.Fatalf("base58 that is not ss58 must fail syntax: %+v", result)
	}
	var pubkey [32]byte
	rand.Read(pubkey[:])
	address, err := ss58.Encode(pubkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	result, err = SnValidateWallet(&SnValidateWalletArgs{Address: " " + address + " "}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ValidSyntax || result.Banned {
		t.Fatalf("valid address: %+v", result)
	}
	if !result.ExistsOnChain || result.Message == "" {
		t.Fatalf("without a reachable chain the check must fail open with a message: %+v", result)
	}
	snBannedColdkeys[pubkey] = true
	defer delete(snBannedColdkeys, pubkey)
	result, _ = SnValidateWallet(&SnValidateWalletArgs{Address: address}, clientSession)
	if !result.Banned || !result.ValidSyntax {
		t.Fatalf("banned address: %+v", result)
	}
}

func TestSnHeadRankingSplitAdjusted(t *testing.T) {
	networkA := server.NewId()
	networkB := server.NewId()
	networkC := server.NewId()
	clientA1, clientA2, clientB, clientC := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	hashesByClient := map[server.Id][]string{
		clientA1: {"h1", "h2"},
		clientA2: {"h2", "h3"}, // h2 is shared inside A: counted once for A
		clientB:  {"h3", "h4"}, // h3 is shared between A and B: 1/2 each
		clientC:  {"h4"},       // h4 shared between B and C
	}
	networkByClient := map[server.Id]server.Id{
		clientA1: networkA, clientA2: networkA, clientB: networkB, clientC: networkC,
		server.NewId(): server.NewId(), // a client without live hashes scores nothing
	}
	ranking := snScoreHeadRanking(hashesByClient, networkByClient)
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !near(ranking.scores[networkA], 2.5) || !near(ranking.scores[networkB], 1.0) || !near(ranking.scores[networkC], 0.5) {
		t.Fatalf("scores: %+v", ranking.scores)
	}
	if ranking.rankOf(ranking.scores[networkA]) != 1 || ranking.rankOf(ranking.scores[networkB]) != 2 || ranking.rankOf(ranking.scores[networkC]) != 3 {
		t.Fatalf("ranks: %d %d %d", ranking.rankOf(2.5), ranking.rankOf(1.0), ranking.rankOf(0.5))
	}
	if ranking.rankOf(0) != 0 {
		t.Fatalf("no score, no rank")
	}
	if ranking.floor() != 0 {
		t.Fatalf("fewer than %d fleets: floor must be 0, got %f", SnHeadCutoff, ranking.floor())
	}
	if len(ranking.liveClients[networkA]) != 2 || !ranking.liveClients[networkB][clientB] {
		t.Fatalf("live clients: %+v", ranking.liveClients)
	}
	// with a full tier the floor is the last score inside the cutoff and an
	// equal score ranks inside it
	big := &snHeadRanking{sorted: make([]float64, SnHeadCutoff+5)}
	for i := range big.sorted {
		big.sorted[i] = float64(len(big.sorted) - i)
	}
	if big.floor() != 6 || big.rankOf(6) != SnHeadCutoff || big.rankOf(5) != SnHeadCutoff+1 {
		t.Fatalf("floor %f rank(6) %d rank(5) %d", big.floor(), big.rankOf(6), big.rankOf(5))
	}
}

// The wire form round-trips into protocol.FleetBinding, the client signature
// verifies against the digest, and with a hotkey signature the calldata
// packs — the same path the operator submits from their own key.
func TestSnFleetBindingWireForm(t *testing.T) {
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hotkeySecret, hotkeyPublic, err := schnorrkel.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	hotkeyBytes := hotkeyPublic.Encode()
	hotkeySs58, err := ss58.Encode(hotkeyBytes, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	clientId := server.NewId()
	wire := &SnFleetBinding{
		ChainId:        945,
		Netuid:         25,
		Coordinator:    "0x" + strings.Repeat("ab", 20),
		FleetId:        "0x" + strings.Repeat("01", 32),
		Hotkey:         hotkeySs58,
		ClientId:       clientId,
		ClientKey:      "0x" + hex.EncodeToString(clientPublic),
		Generation:     1,
		ValidFromEpoch: 10,
		ValidToEpoch:   20,
		CommitmentHash: "0x" + strings.Repeat("cd", 32),
	}
	binding, err := SnFleetBindingFromJson(wire)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Hotkey != hotkeyBytes || binding.ClientID != [16]byte(clientId) || binding.Netuid != 25 {
		t.Fatalf("binding fields: %+v", binding)
	}
	clientSignature, err := binding.SignClient(clientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.VerifyClient(clientSignature) {
		t.Fatal("client signature must verify")
	}
	digest, err := binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	transcript := schnorrkel.NewSigningContext([]byte("substrate"), digest[:])
	hotkeySignature, err := hotkeySecret.Sign(transcript)
	if err != nil {
		t.Fatal(err)
	}
	hotkeySignatureBytes := hotkeySignature.Encode()
	if !binding.VerifyHotkey(hotkeySignatureBytes[:]) {
		t.Fatal("hotkey signature must verify")
	}
	calldata, err := onchain.BuildFleetBindingCalldata(binding, clientSignature, hotkeySignatureBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	if len(calldata) < 4 {
		t.Fatalf("calldata: %x", calldata)
	}
	// the hex hotkey form and a wrong-length key
	wire.Hotkey = "0x" + hex.EncodeToString(hotkeyBytes[:])
	if again, err := SnFleetBindingFromJson(wire); err != nil || again.Hotkey != hotkeyBytes {
		t.Fatalf("hex hotkey: %v", err)
	}
	wire.ClientKey = "0x0102"
	if _, err := SnFleetBindingFromJson(wire); err == nil {
		t.Fatal("a short client key must be rejected")
	}
}

func TestEpochEarningsTemplateText(t *testing.T) {
	template := &EpochEarningsTemplate{
		Epoch: 42, Points: 1234.5, ShareBps: 71, Rank: 17, Total: 5210,
		Top200Eligible: true, Top200Rank: 143, HasWallet: true,
		UnclaimedRao: big.NewInt(3_241_000_000),
	}
	if template.PointsText() != "1234.5" || template.SharePercent() != "0.71%" || template.RankText() != "#17 of 5210" {
		t.Fatalf("%s %s %s", template.PointsText(), template.SharePercent(), template.RankText())
	}
	if template.AlphaText() != "3.2410 SN25α" || !template.HasUnclaimed() || template.Top200Text() != "Top 200 · you qualify" {
		t.Fatalf("%s %v %s", template.AlphaText(), template.HasUnclaimed(), template.Top200Text())
	}
	template.Top200Bound = true
	template.Top200Uid = 17
	template.Top200Rank = 9
	if template.Top200Text() != "Top 200 · UID 17 · rank #9" {
		t.Fatalf("%s", template.Top200Text())
	}
	plain := &EpochEarningsTemplate{Points: 2, ShareBps: 0}
	if plain.PointsText() != "2" || plain.RankText() != "unranked" || plain.Top200Text() != "" || plain.AlphaText() != "" || plain.HasUnclaimed() {
		t.Fatalf("%s %s %q %q", plain.PointsText(), plain.RankText(), plain.Top200Text(), plain.AlphaText())
	}
	// points only (no wallet): never a claim line
	plain.ShareBps = 500
	if plain.HasUnclaimed() {
		t.Fatal("no wallet, no unclaimed line")
	}
}

// Both the live answer and the redis summary pass through the overlay: the
// genesis hash is "0x" and 64 lowercase hex digits beside the other chain
// settings, survives the json the summary is cached as, and is omitted while
// unset.
// The epoch-row fallback summary has no chain identity; it must state the same
// chain id and contract as the cached summary, not 0 and "".
func TestSnEpochSummaryWithChainSettingsStatesTheChainOfTheEpochRowFallback(t *testing.T) {
	cfg := &StConfig{ChainId: 964, ContractAddress: common.Address{4}, Netuid: 25}
	cfg.GenesisHash[0] = 1
	fallback := snEpochSummaryWithChainSettings(cfg, &model.StEpochSummary{Epoch: 42, StartBlock: 10})
	if fallback.ChainId != 964 || fallback.ContractAddress != cfg.ContractAddress.Hex() || fallback.Netuid != 25 || fallback.GenesisHash == "" {
		t.Fatalf("fallback summary = %+v", fallback)
	}
	cached := snEpochSummaryWithChainSettings(cfg, &model.StEpochSummary{Epoch: 42, ChainId: 7, ContractAddress: "0x0000000000000000000000000000000000000009"})
	if cached.ChainId != 7 || cached.ContractAddress != "0x0000000000000000000000000000000000000009" {
		t.Fatalf("stated chain settings were overwritten: %+v", cached)
	}
}

func TestSnEpochSummaryWithChainSettingsStatesTheGenesisHash(t *testing.T) {
	cfg := &StConfig{ChainId: 945, Netuid: 25, NoId: 7, SettlementVault: common.Address{5}, PublicRpcUrl: "https://rpc.example"}
	for i := range cfg.GenesisHash {
		cfg.GenesisHash[i] = byte(0xa0 + i)
	}
	const genesisHash = "0xa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
	summary := snEpochSummaryWithChainSettings(cfg, &model.StEpochSummary{Epoch: 42, ChainId: cfg.ChainId, ContractAddress: "0x0000000000000000000000000000000000000004"})
	if summary.GenesisHash != genesisHash || summary.Epoch != 42 || summary.ChainId != 945 || summary.Netuid != 25 || summary.NoId != "7" || summary.RpcUrl != "https://rpc.example" {
		t.Fatalf("summary = %+v", summary)
	}
	summaryJson, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(summaryJson, &fields); err != nil || fields["genesis_hash"] != genesisHash {
		t.Fatalf("summary json = %s, %v", summaryJson, err)
	}
	var cached model.StEpochSummary
	if err := json.Unmarshal(summaryJson, &cached); err != nil || cached != *summary {
		t.Fatalf("cached summary = %+v, %v; want %+v", cached, err, *summary)
	}

	cfg.GenesisHash = [32]byte{}
	unset := snEpochSummaryWithChainSettings(cfg, &model.StEpochSummary{Epoch: 42})
	unsetJson, err := json.Marshal(unset)
	if err != nil {
		t.Fatal(err)
	}
	if unset.GenesisHash != "" || strings.Contains(string(unsetJson), "genesis_hash") {
		t.Fatalf("unset genesis hash = %q in %s", unset.GenesisHash, unsetJson)
	}
}
