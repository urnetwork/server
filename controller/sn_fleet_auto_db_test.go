package controller

// The Top 200 claim's server side against a real database, with the real sdk device on
// the other side of real HTTP and real keys. The chain, the relay's sending and the
// Redis-backed evidence are fakes. Run through dbtest.sh.

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	sdkprotocol "github.com/urnetwork/sdk/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The chain, as far as the server can tell.
type testingFleetAutoChain struct {
	mirroredHash  [32]byte
	mirroredBlock uint64
	mirrorErr     error
	burn          uint64
	fee           uint64
	free          uint64
	readErr       error
}

func (self *testingFleetAutoChain) MirroredCommitment(context.Context, [32]byte) ([32]byte, uint64, error) {
	return self.mirroredHash, self.mirroredBlock, self.mirrorErr
}

func (self *testingFleetAutoChain) RegistrationBurnRao(context.Context, uint16) (uint64, error) {
	return self.burn, self.readErr
}

func (self *testingFleetAutoChain) RegistrationFeeRao(context.Context, uint16) (uint64, error) {
	return self.fee, self.readErr
}

func (self *testingFleetAutoChain) FreeBalanceRao(context.Context, [32]byte) (uint64, error) {
	return self.free, self.readErr
}

// The relay's sending, with every transaction recorded.
type testingFleetAutoRelayer struct {
	available map[string]bool
	sent      []*model.SnFleetAutoRelay
	settled   []*model.SnFleetAutoRelay
	sendErr   error
	// what the next Send answers, Done when empty
	status string
}

func (self *testingFleetAutoRelayer) Available(_ context.Context, fromKey string) bool {
	return self.available[fromKey]
}

func (self *testingFleetAutoRelayer) Send(_ context.Context, relay *model.SnFleetAutoRelay) (string, string, error) {
	self.sent = append(self.sent, relay)
	if self.sendErr != nil {
		return model.SnFleetAutoRelayQueued, "", self.sendErr
	}
	if self.status != "" {
		return self.status, "", nil
	}
	return model.SnFleetAutoRelayDone, "", nil
}

func (self *testingFleetAutoRelayer) Settle(_ context.Context, relay *model.SnFleetAutoRelay) (string, string, error) {
	self.settled = append(self.settled, relay)
	return model.SnFleetAutoRelayDone, "", nil
}

// Everything a claim's server side reads from outside the database.
type testingFleetAuto struct {
	t        testing.TB
	f        *snWalletProviderScopeFixture
	chain    *testingFleetAutoChain
	relayer  *testingFleetAutoRelayer
	epoch    *StEpochState
	evidence *snFleetAutoEvidence
	holders  *model.SnUidHolders
	prompt   bool
	coldkey  *schnorrkel.MiniSecretKey
	address  string
	endpoint *httptest.Server
}

func newTestingFleetAuto(t testing.TB) *testingFleetAuto {
	t.Helper()
	f := newSnWalletProviderScopeFixture(t)
	env := &testingFleetAuto{
		t:       t,
		f:       f,
		chain:   &testingFleetAutoChain{burn: 500_000, fee: 1_065_869},
		relayer: &testingFleetAutoRelayer{available: map[string]bool{model.SnFleetAutoRelayKeyBinding: true, model.SnFleetAutoRelayKeyTopUp: true}},
		epoch:   &StEpochState{Epoch: 10, EpochStartBlock: 500_000, TEpochBlocks: 50_400, HeadBlock: 530_000},
		evidence: &snFleetAutoEvidence{
			ClientKeys: map[server.Id][32]byte{},
			Eligible:   map[server.Id]bool{},
			Prefixes:   map[server.Id][32]byte{},
			Measured:   map[server.Id]bool{},
			PerFleet:   7,
		},
		holders: &model.SnUidHolders{Holders: map[uint16]model.SnUidHolder{}},
		prompt:  true,
	}
	env.coldkey, env.address = snNetworkWalletTestKey(t, 151)
	oldOpen, oldChain, oldEpoch, oldEvidence, oldHolders, oldPrompt, oldRelayer := snFleetAutoOpen, snFleetAutoChainFn, snFleetAutoEpochFn, snFleetAutoEvidenceFn, snFleetAutoHoldersFn, snFleetAutoClaimPromptFn, snFleetRelayerFn
	snFleetAutoOpen = func() bool { return true }
	snFleetAutoChainFn = func() snFleetAutoChain { return env.chain }
	snFleetAutoEpochFn = func(context.Context) *StEpochState { return env.epoch }
	snFleetAutoEvidenceFn = func(context.Context, []server.Id, time.Time) *snFleetAutoEvidence { return env.evidence }
	snFleetAutoHoldersFn = func(context.Context, model.StDeploymentKey) *model.SnUidHolders { return env.holders }
	snFleetAutoClaimPromptFn = func(context.Context, server.Id) bool { return env.prompt }
	snFleetRelayerFn = func() snFleetRelayer { return env.relayer }
	snFleetAutoResetNetworks()
	t.Cleanup(func() {
		snFleetAutoOpen, snFleetAutoChainFn, snFleetAutoEpochFn, snFleetAutoEvidenceFn, snFleetAutoHoldersFn, snFleetAutoClaimPromptFn, snFleetRelayerFn = oldOpen, oldChain, oldEpoch, oldEvidence, oldHolders, oldPrompt, oldRelayer
		snFleetAutoResetNetworks()
	})
	// the device's real routes
	env.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sn/fleet/device":
			args := &SnFleetDeviceArgs{NeedConsent: r.URL.Query().Get("need_consent") == "1"}
			router.WrapRequireClient(func(clientSession *session.ClientSession) (*SnFleetDeviceResult, error) {
				return GetSnFleetDevice(args, clientSession)
			}, w, r)
		case "/sn/fleet/device/signature":
			router.WrapWithInputRequireClient(SnFleetDeviceSignature, w, r)
		case "/sn/fleet/device/binding":
			router.WrapRequireClient(GetSnFleetDeviceBinding, w, r)
		case "/sn/fleet/device/leave":
			router.WrapWithInputRequireClient(SnFleetDeviceLeave, w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(env.endpoint.Close)
	return env
}

func (self *testingFleetAuto) ctx() context.Context {
	return self.t.(*snWalletProviderScopeTest).ctx
}

func (self *testingFleetAuto) session(byJwt *session.ByJwt) *session.ClientSession {
	return session.Testing_CreateClientSession(self.ctx(), byJwt)
}

// The network's payout consent, signed by its coldkey, through the real routes.
func (self *testingFleetAuto) consent() {
	self.t.Helper()
	set, _, _ := self.f.networkConsent(self.t, self.f.owner, self.coldkey, self.address, self.f.scope.Epoch)
	self.f.accept(self.t, self.f.owner, set)
}

func (self *testingFleetAuto) hotkey(seed [32]byte) [32]byte {
	self.t.Helper()
	public, err := sdkprotocol.FleetHotkeyPublicKey(seed)
	if err != nil {
		self.t.Fatal(err)
	}
	return public
}

func (self *testingFleetAuto) authorizationMessage(networkId server.Id, coldkey [32]byte, hotkey [32]byte) string {
	self.t.Helper()
	cfg := stConfig()
	message, err := sdkprotocol.FleetHotkeyAuthorization{ChainID: cfg.ChainId, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, NetworkID: [16]byte(networkId), Coldkey: coldkey, Hotkey: hotkey}.Message()
	if err != nil {
		self.t.Fatal(err)
	}
	return message
}

func testingFleetAutoSign(t testing.TB, key *schnorrkel.MiniSecretKey, message string) string {
	t.Helper()
	signature, err := key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte("<Bytes>"+message+"</Bytes>")))
	if err != nil {
		t.Fatal(err)
	}
	encoded := signature.Encode()
	return "0x" + hex.EncodeToString(encoded[:])
}

// The owner's session stores the coldkey's authorization of the hotkey.
func (self *testingFleetAuto) authorize(hotkeySeed [32]byte) {
	self.t.Helper()
	message := self.authorizationMessage(self.f.owner.NetworkId, self.coldkey.Public().Encode(), self.hotkey(hotkeySeed))
	result, err := SnFleetAuthorize(&SnFleetAuthorizationArgs{Message: message, Signature: testingFleetAutoSign(self.t, self.coldkey, message)}, self.session(self.f.owner))
	if err != nil || result.Error != nil || !result.Accepted {
		self.t.Fatalf("authorize: %v %+v", err, result)
	}
}

// A long enough wait since the authorization for the devices to have asked.
func (self *testingFleetAuto) settle() {
	self.t.Helper()
	server.Tx(self.ctx(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx(), `UPDATE sn_fleet_auto_authorization SET update_time = update_time - interval '13 hours'`))
	})
}

func (self *testingFleetAuto) register(hotkey [32]byte, uid uint16) {
	self.holders.Holders[uid] = model.SnUidHolder{Hotkey: hotkey, BlockAtRegistration: 1}
}

// A device of the network: its evidence, and its seed.
func (self *testingFleetAuto) device(byJwt *session.ByJwt, seedByte byte, prefixByte byte) []byte {
	self.t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = seedByte
	clientId := *byJwt.ClientId
	self.evidence.ClientKeys[clientId] = [32]byte(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	self.evidence.Eligible[clientId] = true
	self.evidence.Prefixes[clientId] = [32]byte{0xd0, prefixByte}
	return seed
}

// An sdk store in memory.
type testingFleetAutoStore struct {
	saved string
}

func (self *testingFleetAutoStore) LoadSnFleetDeviceState() string {
	return self.saved
}

func (self *testingFleetAutoStore) SaveSnFleetDeviceState(stateJson string) bool {
	self.saved = stateJson
	return true
}

// The real sdk device, over real HTTP, as a device of the network.
func (self *testingFleetAuto) sdkDevice(byJwt *session.ByJwt, seed []byte, store *testingFleetAutoStore) *sdk.SnFleetDevice {
	self.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	self.t.Cleanup(cancel)
	strategySettings := connect.DefaultClientStrategySettings()
	strategy := connect.NewClientStrategy(ctx, strategySettings)
	self.t.Cleanup(strategy.Close)
	api := sdk.NewApi(ctx, strategy, self.endpoint.URL)
	self.t.Cleanup(func() { _ = api.CloseAndWait(context.Background()) })
	api.SetByJwt(byJwt.Testing_Sign())
	clientId, err := connect.ParseId(byJwt.ClientId.String())
	if err != nil {
		self.t.Fatal(err)
	}
	return sdk.NewSnFleetDeviceWithClientKey(ctx, api, clientId, seed, store)
}

func (self *testingFleetAuto) deploymentKey() model.StDeploymentKey {
	self.t.Helper()
	key, ok := StDeploymentKey()
	if !ok {
		self.t.Fatal("no deployment")
	}
	return key
}

// The hotkey's signatures on what the claim offers, as the key holder's device sends them.
func (self *testingFleetAuto) hotkeySign(seed [32]byte) int {
	self.t.Helper()
	claim, err := GetSnFleetClaim(self.session(self.f.owner))
	if err != nil {
		self.t.Fatal(err)
	}
	args := &SnFleetHotkeySignaturesArgs{}
	for _, work := range claim.HotkeyWork {
		binding, err := snFleetAutoBindingFromJson(work.BindingJson)
		if err != nil {
			self.t.Fatal(err)
		}
		digest, err := binding.Digest()
		if err != nil {
			self.t.Fatal(err)
		}
		var sdkBinding sdkprotocol.FleetBinding = sdkprotocol.FleetBinding{
			ChainID: binding.ChainID, Netuid: binding.Netuid, Coordinator: binding.Coordinator, FleetID: binding.FleetID, Hotkey: binding.Hotkey,
			ClientID: binding.ClientID, ClientKey: binding.ClientKey, Generation: binding.Generation, ValidFromEpoch: binding.ValidFromEpoch, ValidToEpoch: binding.ValidToEpoch, CommitmentHash: binding.CommitmentHash,
		}
		signature, err := sdkprotocol.SignFleetBindingWithHotkey(seed, sdkBinding)
		if err != nil {
			self.t.Fatal(err)
		}
		args.Signatures = append(args.Signatures, &SnFleetHotkeySignature{Digest: "0x" + hex.EncodeToString(digest[:]), HotkeySignature: "0x" + hex.EncodeToString(signature)})
	}
	result, err := SnFleetHotkeySignatures(args, self.session(self.f.owner))
	if err != nil || result.Error != nil {
		self.t.Fatalf("hotkey signatures: %v %+v", err, result)
	}
	return result.Accepted
}

// The census sees a device's FleetBound log.
func (self *testingFleetAuto) bound(clientId server.Id, generation uint64, fleetId [32]byte, hotkey [32]byte, uid uint16, from uint64, to uint64, block uint64) {
	self.t.Helper()
	data, err := json.Marshal(model.StFleetBoundEventData{
		ClientId:       "0x" + hex.EncodeToString(clientId[:]),
		FleetId:        "0x" + hex.EncodeToString(fleetId[:]),
		Hotkey:         "0x" + hex.EncodeToString(hotkey[:]),
		Uid:            fmt.Sprintf("%d", uid),
		Generation:     fmt.Sprintf("%d", generation),
		ValidFromEpoch: fmt.Sprintf("%d", from),
		ValidToEpoch:   fmt.Sprintf("%d", to),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	blockHash := fmt.Sprintf("0x%064x", block)
	event := &model.StChainEvent{BlockNumber: block, BlockHash: blockHash, LogIndex: 0, TxHash: fmt.Sprintf("0x%064x", block+1), Kind: model.StFleetBoundEventKind, DataJson: string(data)}
	if err := model.ApplySnFleetBindingRange(self.ctx(), self.deploymentKey(), []*model.StChainEvent{event}, model.SnFleetBindingCheckpoint{NextBlock: block + 1, BlockHash: blockHash}); err != nil {
		self.t.Fatal(err)
	}
}

func (self *testingFleetAuto) relayRows() []*model.SnFleetAutoRelay {
	return model.GetSnFleetAutoRelayOpen(self.ctx(), self.deploymentKey(), 100)
}

// The whole server side with a real device on each end: the coldkey's authorization, the
// devices asking, one per prefix chosen and no other, the manifest, the hotkey's and
// each device's signature, the mirror, the relay, and the card's counts at each step.
func TestSnFleetAutoClaimEndToEndDb(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		env := newTestingFleetAuto(t)
		ctx := env.ctx()
		owner, provider, sibling := env.f.owner, env.f.provider, env.f.sibling
		hotkeySeed := [32]byte{0xb1}
		hotkey := env.hotkey(hotkeySeed)
		key := env.deploymentKey()

		// a third device on the same prefix as the first, and a fourth that never asks:
		// an older release, or one with signing off
		deviceId, clientId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, owner.NetworkId, deviceId, clientId, "same-prefix", "test")
		samePrefix := owner.Client(deviceId, clientId)
		deviceId, clientId = server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, owner.NetworkId, deviceId, clientId, "older-release", "test")
		older := owner.Client(deviceId, clientId)

		providerSeed := env.device(provider, 1, 1)
		siblingSeed := env.device(sibling, 2, 2)
		samePrefixSeed := env.device(samePrefix, 3, 1)
		env.device(older, 4, 3)
		providerStore, siblingStore, samePrefixStore := &testingFleetAutoStore{}, &testingFleetAutoStore{}, &testingFleetAutoStore{}
		providerDevice := env.sdkDevice(provider, providerSeed, providerStore)
		siblingDevice := env.sdkDevice(sibling, siblingSeed, siblingStore)
		samePrefixDevice := env.sdkDevice(samePrefix, samePrefixSeed, samePrefixStore)

		// before a consent: nothing to pin, and an authorization has nothing to match
		if result, err := providerDevice.CheckSync(ctx); err != nil || result.Action != sdk.SnFleetDeviceActionNone || !result.BindingOpen {
			t.Fatalf("%+v %v", result, err)
		}
		message := env.authorizationMessage(owner.NetworkId, env.coldkey.Public().Encode(), hotkey)
		if result, err := SnFleetAuthorize(&SnFleetAuthorizationArgs{Message: message, Signature: testingFleetAutoSign(t, env.coldkey, message)}, env.session(owner)); err != nil || result.Error == nil {
			t.Fatalf("authorized with no consent: %+v %v", result, err)
		}

		env.consent()
		// the devices pin on first sight of the consent, before there's anything to attack
		for _, device := range []*sdk.SnFleetDevice{providerDevice, siblingDevice, samePrefixDevice} {
			if result, err := device.CheckSync(ctx); err != nil || result.Action != sdk.SnFleetDeviceActionPinned {
				t.Fatalf("%+v %v", result, err)
			}
		}
		// a network with no authorization isn't on the short list, and stores nothing
		if devices := model.GetSnFleetAutoDevices(ctx, key, owner.NetworkId, server.NowUtc().Add(-time.Hour)); len(devices) != 0 {
			t.Fatalf("devices %+v", devices)
		}

		env.authorize(hotkeySeed)
		// its devices now ask, and so say they have this support. The fourth doesn't
		for _, device := range []*sdk.SnFleetDevice{providerDevice, siblingDevice, samePrefixDevice} {
			if result, err := device.CheckSync(ctx); err != nil || result.Action != sdk.SnFleetDeviceActionNone {
				t.Fatalf("%+v %v", result, err)
			}
		}
		if devices := model.GetSnFleetAutoDevices(ctx, key, owner.NetworkId, server.NowUtc().Add(-time.Hour)); len(devices) != 3 {
			t.Fatalf("devices %+v", devices)
		}

		// not before the hotkey holds a UID, and not before the devices have had time
		SnFleetAutoStep(ctx)
		if model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId) != nil {
			t.Fatal("a manifest with no UID")
		}
		env.register(hotkey, 42)
		SnFleetAutoStep(ctx)
		if model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId) != nil {
			t.Fatal("a manifest before the devices had time to ask")
		}
		env.settle()
		SnFleetAutoStep(ctx)
		generation := model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId)
		if generation == nil || generation.BuiltEpoch != env.epoch.Epoch || generation.Generation != 1 {
			t.Fatalf("generation %+v", generation)
		}

		// one per prefix, and nothing for a device that never asked
		members := model.GetSnFleetAutoMembers(ctx, key, owner.NetworkId, generation.Generation)
		if len(members) != 2 {
			t.Fatalf("members %+v", members)
		}
		chosen := map[server.Id]bool{}
		for _, member := range members {
			chosen[member.ClientId] = true
			if member.ValidFromEpoch != 11 || member.ValidToEpoch != 22 || member.HotkeySignature != nil {
				t.Fatalf("member %+v", member)
			}
		}
		if chosen[*older.ClientId] || !chosen[*sibling.ClientId] || chosen[*provider.ClientId] == chosen[*samePrefix.ClientId] {
			t.Fatalf("chosen %v", chosen)
		}
		chosenPrefixDevice, chosenPrefixStore, otherPrefixDevice := providerDevice, providerStore, samePrefixDevice
		chosenPrefixJwt := provider
		if chosen[*samePrefix.ClientId] {
			chosenPrefixDevice, chosenPrefixStore, otherPrefixDevice = samePrefixDevice, samePrefixStore, providerDevice
			chosenPrefixJwt = samePrefix
		}

		// a device's check before the hotkey signed: nothing to sign, and it asks sooner
		if work, err := GetSnFleetDevice(&SnFleetDeviceArgs{}, env.session(chosenPrefixJwt)); err != nil || work.BindingJson != "" || work.NextCheckSeconds != int64(snFleetAutoCheckChosen/time.Second) {
			t.Fatalf("%+v %v", work, err)
		}

		// the key holder's card, and its work
		claim, err := GetSnFleetClaim(env.session(owner))
		if err != nil {
			t.Fatal(err)
		}
		if claim.ConsentColdkey != env.address || !claim.Registered || claim.Uid != 42 || claim.Manifest == nil || claim.Manifest.Mirrored || len(claim.HotkeyWork) != 2 || claim.Counts.Chosen != 2 || claim.Counts.Ready != 0 || claim.Counts.Bound != 0 {
			t.Fatalf("claim %+v", claim)
		}
		parsed, err := sdkprotocol.ParseFleetManifest([]byte(claim.Manifest.Manifest))
		if err != nil {
			t.Fatal(err)
		}
		if hash, _ := parsed.CommitmentHash(); hash != generation.CommitmentHash || parsed.Hotkey != hotkey || len(parsed.Members) != 2 {
			t.Fatalf("manifest %+v", parsed)
		}
		if claim.AuthorizationMessage != message {
			t.Fatal("authorization")
		}

		// a signature by another hotkey is not stored
		other, err := GetSnFleetClaim(env.session(owner))
		if err != nil {
			t.Fatal(err)
		}
		binding, _ := snFleetAutoBindingFromJson(other.HotkeyWork[0].BindingJson)
		digest, _ := binding.Digest()
		wrong, err := sdkprotocol.SignFleetBindingWithHotkey([32]byte{0xb2}, sdkprotocol.FleetBinding{ChainID: binding.ChainID, Netuid: binding.Netuid, Coordinator: binding.Coordinator, FleetID: binding.FleetID, Hotkey: binding.Hotkey, ClientID: binding.ClientID, ClientKey: binding.ClientKey, Generation: binding.Generation, ValidFromEpoch: binding.ValidFromEpoch, ValidToEpoch: binding.ValidToEpoch, CommitmentHash: binding.CommitmentHash})
		if err != nil {
			t.Fatal(err)
		}
		if result, err := SnFleetHotkeySignatures(&SnFleetHotkeySignaturesArgs{Signatures: []*SnFleetHotkeySignature{{Digest: "0x" + hex.EncodeToString(digest[:]), HotkeySignature: "0x" + hex.EncodeToString(wrong)}}}, env.session(owner)); err != nil || result.Accepted != 0 {
			t.Fatalf("%+v %v", result, err)
		}

		if accepted := env.hotkeySign(hotkeySeed); accepted != 2 {
			t.Fatalf("accepted %d", accepted)
		}

		// each chosen device, through the real sdk checks, signs with no command
		for _, device := range []*sdk.SnFleetDevice{chosenPrefixDevice, siblingDevice} {
			result, err := device.CheckSync(ctx)
			if err != nil || result.Action != sdk.SnFleetDeviceActionSigned || result.SendFailed || result.Generation != 1 {
				t.Fatalf("%+v %v", result, err)
			}
		}
		// only the chosen are offered anything
		if result, err := otherPrefixDevice.CheckSync(ctx); err != nil || result.Action != sdk.SnFleetDeviceActionNone {
			t.Fatalf("not chosen: %+v %v", result, err)
		}
		// and a restarted device isn't offered it again, or asked to sign again
		if !strings.Contains(chosenPrefixStore.saved, "client_signature") {
			t.Fatal("the device saved nothing")
		}
		if work, err := GetSnFleetDevice(&SnFleetDeviceArgs{}, env.session(chosenPrefixJwt)); err != nil || work.BindingJson != "" {
			t.Fatalf("offered again: %+v %v", work, err)
		}

		// no relay before the mirror
		SnFleetAutoStep(ctx)
		if rows := env.relayRows(); len(rows) != 0 {
			t.Fatalf("relayed before the mirror: %+v", rows)
		}
		claim, _ = GetSnFleetClaim(env.session(owner))
		if claim.Counts.Ready != 2 || claim.Counts.Bound != 0 {
			t.Fatalf("counts %+v", claim.Counts)
		}

		// another hotkey's commitment on chain isn't this manifest
		env.chain.mirroredHash, env.chain.mirroredBlock = [32]byte{0xee}, 600
		SnFleetAutoStep(ctx)
		if rows := env.relayRows(); len(rows) != 0 {
			t.Fatalf("relayed on another commitment: %+v", rows)
		}

		env.chain.mirroredHash, env.chain.mirroredBlock = generation.CommitmentHash, 600
		SnFleetAutoStep(ctx)
		rows := env.relayRows()
		if len(rows) != 2 {
			t.Fatalf("rows %+v", rows)
		}
		for _, row := range rows {
			if row.Kind != model.SnFleetAutoRelayBind || row.FromKey != model.SnFleetAutoRelayKeyBinding || !strings.EqualFold(row.ToAddress, stConfig().ContractAddress.Hex()) || len(row.Calldata) < 4 {
				t.Fatalf("row %+v", row)
			}
		}
		// the next pass queues nothing twice
		SnFleetAutoStep(ctx)
		if rows := env.relayRows(); len(rows) != 2 {
			t.Fatalf("queued twice: %+v", rows)
		}

		SnFleetAutoRelayStep(ctx)
		if len(env.relayer.sent) != 2 || len(env.relayRows()) != 0 {
			t.Fatalf("sent %d, open %d", len(env.relayer.sent), len(env.relayRows()))
		}

		// the census sees them, and the card with it
		for _, member := range members {
			env.bound(member.ClientId, 1, generation.FleetId, hotkey, 42, 11, 22, 1000+uint64(member.ClientId[15]))
		}
		claim, _ = GetSnFleetClaim(env.session(owner))
		if claim.Counts.Chosen != 2 || claim.Counts.Bound != 2 || claim.Counts.Ready != 2 || !claim.Manifest.Mirrored {
			t.Fatalf("counts %+v", claim.Counts)
		}

		// manifest changes are batched: in this epoch there's no second one
		SnFleetAutoStep(ctx)
		if again := model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId); again.Generation != generation.Generation {
			t.Fatalf("a second manifest in one epoch: %+v", again)
		}
		// the next epoch has nothing to change while everyone's binding runs on
		env.epoch = &StEpochState{Epoch: 12, EpochStartBlock: 600_800, TEpochBlocks: 50_400, HeadBlock: 610_000}
		for _, device := range []*sdk.SnFleetDevice{chosenPrefixDevice, siblingDevice, otherPrefixDevice} {
			if _, err := device.CheckSync(ctx); err != nil {
				t.Fatal(err)
			}
		}
		SnFleetAutoStep(ctx)
		if again := model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId); again.Generation != generation.Generation {
			t.Fatalf("a manifest with nothing to change: %+v", again)
		}

		// renewing near the end takes no tap: a new generation, a binding starting where the
		// old one ends. Its window and generation are new, and each device signs it
		env.epoch = &StEpochState{Epoch: 21, EpochStartBlock: 1_054_400, TEpochBlocks: 50_400, HeadBlock: 1_060_000}
		for _, device := range []*sdk.SnFleetDevice{chosenPrefixDevice, siblingDevice} {
			if _, err := device.CheckSync(ctx); err != nil {
				t.Fatal(err)
			}
		}
		SnFleetAutoStep(ctx)
		renewal := model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId)
		if renewal.Generation != 2 || renewal.MirroredBlock != nil || renewal.BuiltEpoch != 21 {
			t.Fatalf("renewal %+v", renewal)
		}
		for _, member := range model.GetSnFleetAutoMembers(ctx, key, owner.NetworkId, renewal.Generation) {
			if member.ValidFromEpoch != 23 || member.ValidToEpoch != 34 {
				t.Fatalf("renewal window %+v", member)
			}
		}
		if accepted := env.hotkeySign(hotkeySeed); accepted != 2 {
			t.Fatalf("accepted %d", accepted)
		}
		for _, device := range []*sdk.SnFleetDevice{chosenPrefixDevice, siblingDevice} {
			result, err := device.CheckSync(ctx)
			if err != nil || result.Action != sdk.SnFleetDeviceActionSigned || result.Generation != 2 {
				t.Fatalf("renewal: %+v %v", result, err)
			}
		}
	})
}

// While binding is closed nothing happens: routes refuse, nothing is stored, and the chain
// and relay aren't touched. The one thing a device can get is its own network's consent.
func TestSnFleetAutoIsDarkWhileBindingIsClosedDb(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		env := newTestingFleetAuto(t)
		ctx := env.ctx()
		snFleetAutoOpen = func() bool { return false }
		env.chain.readErr = errors.New("the chain was read")
		env.chain.mirrorErr = errors.New("the chain was read")
		env.relayer.sendErr = errors.New("the relay was used")
		env.consent()
		hotkeySeed := [32]byte{0xb1}
		hotkey := env.hotkey(hotkeySeed)
		key := env.deploymentKey()

		message := env.authorizationMessage(env.f.owner.NetworkId, env.coldkey.Public().Encode(), hotkey)
		authorized, err := SnFleetAuthorize(&SnFleetAuthorizationArgs{Message: message, Signature: testingFleetAutoSign(t, env.coldkey, message)}, env.session(env.f.owner))
		if err != nil || authorized.Accepted || authorized.Error == nil {
			t.Fatalf("%+v %v", authorized, err)
		}
		if topUp, err := SnFleetTopUp(env.session(env.f.owner)); err != nil || topUp.Error == nil {
			t.Fatalf("%+v %v", topUp, err)
		}
		if signed, err := SnFleetHotkeySignatures(&SnFleetHotkeySignaturesArgs{}, env.session(env.f.owner)); err != nil || signed.Error == nil {
			t.Fatalf("%+v %v", signed, err)
		}
		if signature, err := SnFleetDeviceSignature(&SnFleetDeviceSignatureArgs{Refusal: sdkprotocol.FleetRefusalLeft}, env.session(env.f.provider)); err != nil || signature.Error == nil {
			t.Fatalf("%+v %v", signature, err)
		}
		if leave, err := SnFleetDeviceLeave(&SnFleetDeviceLeaveArgs{}, env.session(env.f.provider)); err != nil || leave.Error == nil {
			t.Fatalf("%+v %v", leave, err)
		}
		if binding, err := GetSnFleetDeviceBinding(env.session(env.f.provider)); err != nil || binding.Bound {
			t.Fatalf("%+v %v", binding, err)
		}
		claim, err := GetSnFleetClaim(env.session(env.f.owner))
		if err != nil || claim.BindingOpen || claim.Manifest != nil || claim.ConsentColdkey != "" {
			t.Fatalf("%+v %v", claim, err)
		}
		work, err := GetSnFleetDevice(&SnFleetDeviceArgs{NeedConsent: true}, env.session(env.f.provider))
		if err != nil || work.BindingOpen || work.NextCheckSeconds != int64(snFleetAutoCheckClosed/time.Second) || work.BindingJson != "" {
			t.Fatalf("%+v %v", work, err)
		}
		if work.Consent == nil || !strings.HasPrefix(work.Consent.Message, "Approve URnetwork network wallet mapping\n") {
			t.Fatalf("consent %+v", work.Consent)
		}
		SnFleetAutoStep(ctx)
		SnFleetAutoRelayStep(ctx)
		if model.GetSnFleetAutoAuthorization(ctx, key, env.f.owner.NetworkId) != nil || len(model.GetSnFleetAutoDevices(ctx, key, env.f.owner.NetworkId, time.Time{})) != 0 || len(env.relayer.sent) != 0 {
			t.Fatal("something was stored or sent while closed")
		}
		if SnFleetAutoOpen() {
			t.Fatal("the taskworker would schedule the steps")
		}
	})
}

// The refusals of an authorization: it has to be this network's, this deployment's, and
// signed by the coldkey of the newest consent, and nothing else signed can stand in. A
// client token can't, and a hotkey belongs to one network. Any wallet's coldkey serves.
func TestSnFleetAutoAuthorizationRefusalsDb(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		env := newTestingFleetAuto(t)
		ctx := env.ctx()
		env.consent()
		cfg := stConfig()
		owner := env.f.owner
		hotkey := env.hotkey([32]byte{0xb1})
		coldkey := env.coldkey.Public().Encode()
		message := env.authorizationMessage(owner.NetworkId, coldkey, hotkey)
		stranger, _ := snNetworkWalletTestKey(t, 152)

		refused := func(name string, args *SnFleetAuthorizationArgs, byJwt *session.ByJwt) {
			t.Helper()
			result, err := SnFleetAuthorize(args, env.session(byJwt))
			if err != nil || result.Accepted || result.Error == nil {
				t.Fatalf("%s: %+v %v", name, result, err)
			}
			if model.GetSnFleetAutoAuthorization(ctx, env.deploymentKey(), owner.NetworkId) != nil {
				t.Fatalf("%s: stored", name)
			}
		}
		refused("a bad signature", &SnFleetAuthorizationArgs{Message: message, Signature: testingFleetAutoSign(t, stranger, message)}, owner)
		refused("a signature over other text", &SnFleetAuthorizationArgs{Message: message, Signature: testingFleetAutoSign(t, env.coldkey, "other text")}, owner)
		refused("no signature", &SnFleetAuthorizationArgs{Message: message}, owner)
		// the coldkey's own consent, its message and signature exactly, isn't an authorization
		set, _, _ := env.f.networkConsent(t, owner, env.coldkey, env.address, env.f.scope.Epoch+1)
		refused("a consent", &SnFleetAuthorizationArgs{Message: set.Message, Signature: set.Signature}, owner)
		// another coldkey than the consent's, correctly signing its own authorization
		otherMessage := env.authorizationMessage(owner.NetworkId, stranger.Public().Encode(), hotkey)
		refused("another coldkey", &SnFleetAuthorizationArgs{Message: otherMessage, Signature: testingFleetAutoSign(t, stranger, otherMessage)}, owner)
		// a good authorization, for another network
		otherNetwork := env.authorizationMessage(server.NewId(), coldkey, hotkey)
		refused("another network", &SnFleetAuthorizationArgs{Message: otherNetwork, Signature: testingFleetAutoSign(t, env.coldkey, otherNetwork)}, owner)
		// a good authorization, for another deployment
		otherChain, err := sdkprotocol.FleetHotkeyAuthorization{ChainID: cfg.ChainId + 1, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, NetworkID: [16]byte(owner.NetworkId), Coldkey: coldkey, Hotkey: hotkey}.Message()
		if err != nil {
			t.Fatal(err)
		}
		refused("another deployment", &SnFleetAuthorizationArgs{Message: otherChain, Signature: testingFleetAutoSign(t, env.coldkey, otherChain)}, owner)
		// a device's client token, of the same network
		refused("a client token", &SnFleetAuthorizationArgs{Message: message, Signature: testingFleetAutoSign(t, env.coldkey, message)}, env.f.provider)

		// the right one is taken, from any wallet: the message is all that's checked
		result, err := SnFleetAuthorize(&SnFleetAuthorizationArgs{Message: message, Signature: testingFleetAutoSign(t, env.coldkey, message)}, env.session(owner))
		if err != nil || !result.Accepted {
			t.Fatalf("%+v %v", result, err)
		}
		stored := model.GetSnFleetAutoAuthorization(ctx, env.deploymentKey(), owner.NetworkId)
		if stored == nil || stored.Hotkey != hotkey || stored.Coldkey != coldkey || stored.Message != message || stored.FleetId == ([32]byte{}) {
			t.Fatalf("%+v", stored)
		}
		// the admin is told about the hotkey, and told again when it changes
		notices := func() (count int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM account_message_outbox WHERE network_id = $1 AND template_name = $2 AND template_json::text LIKE '%' || $3 || '%'`, owner.NetworkId, model.SecurityPayoutWalletTemplateName, model.SecurityWalletDestinationFleetHotkey).Scan(&count))
			})
			return
		}
		if notices() != 1 {
			t.Fatalf("notices %d", notices())
		}
		again := env.authorizationMessage(owner.NetworkId, coldkey, env.hotkey([32]byte{0xb2}))
		if result, err := SnFleetAuthorize(&SnFleetAuthorizationArgs{Message: again, Signature: testingFleetAutoSign(t, env.coldkey, again)}, env.session(owner)); err != nil || !result.Accepted {
			t.Fatalf("%+v %v", result, err)
		}
		if stored := model.GetSnFleetAutoAuthorization(ctx, env.deploymentKey(), owner.NetworkId); stored.Hotkey != env.hotkey([32]byte{0xb2}) {
			t.Fatal("replacing didn't replace")
		}
	})
}

// Leave works from one device: the client key's signature goes through the relay, the device
// isn't chosen at the next batch, and no one else is touched.
func TestSnFleetAutoLeaveFromOneDeviceDb(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		env := newTestingFleetAuto(t)
		ctx := env.ctx()
		owner, provider, sibling := env.f.owner, env.f.provider, env.f.sibling
		key := env.deploymentKey()
		hotkeySeed := [32]byte{0xb1}
		hotkey := env.hotkey(hotkeySeed)
		env.consent()
		env.authorize(hotkeySeed)
		env.register(hotkey, 42)
		providerSeed, siblingSeed := env.device(provider, 1, 1), env.device(sibling, 2, 2)
		providerStore, siblingStore := &testingFleetAutoStore{}, &testingFleetAutoStore{}
		providerDevice, siblingDevice := env.sdkDevice(provider, providerSeed, providerStore), env.sdkDevice(sibling, siblingSeed, siblingStore)
		for _, device := range []*sdk.SnFleetDevice{providerDevice, siblingDevice} {
			if _, err := device.CheckSync(ctx); err != nil {
				t.Fatal(err)
			}
		}
		env.settle()
		fleetId := snFleetAutoFleetId(owner.NetworkId, hotkey)
		env.bound(*provider.ClientId, 3, fleetId, hotkey, 42, 11, 22, 1000)
		env.epoch = &StEpochState{Epoch: 12, EpochStartBlock: 600_800, TEpochBlocks: 50_400, HeadBlock: 610_000}

		outcome, err := providerDevice.LeaveSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Left || outcome.EffectiveEpoch != 13 {
			t.Fatalf("%+v", outcome)
		}
		rows := env.relayRows()
		if len(rows) != 1 || rows[0].Kind != model.SnFleetAutoRelayRevoke || rows[0].FromKey != model.SnFleetAutoRelayKeyBinding {
			t.Fatalf("rows %+v", rows)
		}
		// asked twice, it's one transaction
		if _, err := providerDevice.LeaveSync(ctx); err != nil {
			t.Fatal(err)
		}
		if rows := env.relayRows(); len(rows) != 1 {
			t.Fatalf("rows %+v", rows)
		}

		// an epoch that has passed gets the current one back
		stale, err := SnFleetDeviceLeave(&SnFleetDeviceLeaveArgs{Generation: 3, EffectiveEpoch: 5, ClientSignature: "0x" + strings.Repeat("00", 64)}, env.session(provider))
		if err != nil || stale.Left || stale.CurrentEpoch != 12 {
			t.Fatalf("%+v %v", stale, err)
		}
		// a device can't revoke what it doesn't have. Asking is leaving, so it isn't
		// chosen either way
		deviceId, clientId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, owner.NetworkId, deviceId, clientId, "third", "test")
		third := owner.Client(deviceId, clientId)
		env.device(third, 5, 5)
		if _, err := GetSnFleetDevice(&SnFleetDeviceArgs{}, env.session(third)); err != nil {
			t.Fatal(err)
		}
		absent, err := SnFleetDeviceLeave(&SnFleetDeviceLeaveArgs{Generation: 9, EffectiveEpoch: 13, ClientSignature: "0x" + strings.Repeat("00", 64)}, env.session(third))
		if err != nil || absent.Left || absent.Error == nil {
			t.Fatalf("%+v %v", absent, err)
		}

		// the next batch leaves it out and chooses the rest
		SnFleetAutoStep(ctx)
		generation := model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId)
		if generation == nil {
			t.Fatal("no manifest for the device that stayed")
		}
		// the sibling, which never asked to leave, and not the two that did
		members := model.GetSnFleetAutoMembers(ctx, key, owner.NetworkId, generation.Generation)
		if len(members) != 1 || members[0].ClientId != *sibling.ClientId {
			t.Fatalf("members %+v", members)
		}

		// the relay sends it
		SnFleetAutoRelayStep(ctx)
		if len(env.relayer.sent) != 1 || env.relayer.sent[0].Kind != model.SnFleetAutoRelayRevoke {
			t.Fatalf("sent %+v", env.relayer.sent)
		}

		// a person resets the device: it pins again, says so, and can be chosen again
		if err := providerDevice.Reset(); err != nil {
			t.Fatal(err)
		}
		if result, err := providerDevice.CheckSync(ctx); err != nil || result.Action != sdk.SnFleetDeviceActionPinned {
			t.Fatalf("%+v %v", result, err)
		}
		devices := model.GetSnFleetAutoDevices(ctx, key, owner.NetworkId, time.Time{})
		for _, device := range devices {
			if device.ClientId == *provider.ClientId && device.LeftTime != nil {
				t.Fatal("still marked as left")
			}
		}
	})
}

// The top-up: the live burn plus its fee, once per network, only for a network the app
// prompts to claim, failing closed with no funded key or in a burn spike.
func TestSnFleetAutoTopUpDb(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		env := newTestingFleetAuto(t)
		ctx := env.ctx()
		owner := env.f.owner
		key := env.deploymentKey()
		refused := func(name string) {
			t.Helper()
			result, err := SnFleetTopUp(env.session(owner))
			if err != nil || result.Error == nil || result.AmountRao != 0 {
				t.Fatalf("%s: %+v %v", name, result, err)
			}
			if model.GetSnFleetAutoTopUp(ctx, key, owner.NetworkId) != nil || len(env.relayRows()) != 0 {
				t.Fatalf("%s: something was queued", name)
			}
		}
		refused("no consent")
		env.consent()

		env.prompt = false
		refused("not prompted")
		claim, err := GetSnFleetClaim(env.session(owner))
		if err != nil || claim.TopUp.Available {
			t.Fatalf("%+v %v", claim.TopUp, err)
		}
		env.prompt = true

		env.relayer.available[model.SnFleetAutoRelayKeyTopUp] = false
		refused("no funded key")
		claim, err = GetSnFleetClaim(env.session(owner))
		if err != nil || claim.TopUp.Available {
			t.Fatalf("%+v %v", claim.TopUp, err)
		}
		env.relayer.available[model.SnFleetAutoRelayKeyTopUp] = true

		env.chain.burn = uint64(sdkprotocol.FleetClaimMaxBurnRao) + 1
		refused("a burn spike")
		env.chain.burn = 500_000

		env.chain.free = 10_000_000
		refused("not needed")
		env.chain.free = 0

		env.chain.readErr = errors.New("the rpc is down")
		refused("a failed read")
		env.chain.readErr = nil

		// a client token can't ask, though it's the network's
		if result, err := SnFleetTopUp(env.session(env.f.provider)); err != nil || result.Error == nil {
			t.Fatalf("%+v %v", result, err)
		}

		claim, err = GetSnFleetClaim(env.session(owner))
		if err != nil || !claim.TopUp.Available {
			t.Fatalf("%+v %v", claim.TopUp, err)
		}
		result, err := SnFleetTopUp(env.session(owner))
		if err != nil || result.Error != nil || result.AmountRao != 1_566_369 {
			t.Fatalf("%+v %v", result, err)
		}
		rows := env.relayRows()
		if len(rows) != 1 || rows[0].Kind != model.SnFleetAutoRelayTopUp || rows[0].FromKey != model.SnFleetAutoRelayKeyTopUp {
			t.Fatalf("rows %+v", rows)
		}
		if rows[0].ValueWei != "1566369000000000" || !strings.EqualFold(rows[0].ToAddress, snFleetTopUpPrecompile.Hex()) {
			t.Fatalf("row %+v", rows[0])
		}
		coldkey := env.coldkey.Public().Encode()
		if !strings.HasSuffix(hex.EncodeToString(rows[0].Calldata), hex.EncodeToString(coldkey[:])) {
			t.Fatalf("it pays someone other than the coldkey: %x", rows[0].Calldata)
		}

		// sent once, and that's all: not again if it lands, fails, or the network asks twice
		again, err := SnFleetTopUp(env.session(owner))
		if err != nil || again.Error == nil || len(env.relayRows()) != 1 {
			t.Fatalf("%+v %v", again, err)
		}
		claim, _ = GetSnFleetClaim(env.session(owner))
		if !claim.TopUp.Used || claim.TopUp.Available || claim.TopUp.Landed || claim.TopUp.AmountRao != 1_566_369 {
			t.Fatalf("%+v", claim.TopUp)
		}
		SnFleetAutoRelayStep(ctx)
		claim, _ = GetSnFleetClaim(env.session(owner))
		if !claim.TopUp.Landed || claim.TopUp.Available {
			t.Fatalf("%+v", claim.TopUp)
		}
		if again, err := SnFleetTopUp(env.session(owner)); err != nil || again.Error == nil {
			t.Fatalf("%+v %v", again, err)
		}
	})
}

// With no funded key nothing goes out and nothing is lost: the rows wait. A sent row is
// settled before anything new from its key, and a key's failure doesn't hold up the other.
func TestSnFleetAutoRelayStepDb(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		env := newTestingFleetAuto(t)
		ctx := env.ctx()
		key := env.deploymentKey()
		networkId := env.f.owner.NetworkId
		enqueue := func(operation string, fromKey string) *model.SnFleetAutoRelay {
			relay := &model.SnFleetAutoRelay{RelayId: server.NewId(), NetworkId: networkId, Kind: model.SnFleetAutoRelayBind, Operation: operation, FromKey: fromKey, ToAddress: "0x01", Calldata: []byte{1}}
			if !model.EnqueueSnFleetAutoRelay(ctx, key, relay) {
				t.Fatalf("%s wasn't queued", operation)
			}
			return relay
		}
		first := enqueue("bind:a:1", model.SnFleetAutoRelayKeyBinding)
		enqueue("bind:b:1", model.SnFleetAutoRelayKeyBinding)
		topUp := enqueue("top_up:a", model.SnFleetAutoRelayKeyTopUp)
		if model.EnqueueSnFleetAutoRelay(ctx, key, &model.SnFleetAutoRelay{RelayId: server.NewId(), NetworkId: networkId, Kind: model.SnFleetAutoRelayBind, Operation: "bind:a:1", FromKey: model.SnFleetAutoRelayKeyBinding, ToAddress: "0x01", Calldata: []byte{1}}) {
			t.Fatal("one operation was queued twice")
		}

		// a sender that fails stops only its own key, and the row waits
		env.relayer.sendErr = errors.New("no funds")
		SnFleetAutoRelayStep(ctx)
		if len(env.relayer.sent) != 2 || env.relayer.sent[0].RelayId != first.RelayId || env.relayer.sent[1].RelayId != topUp.RelayId {
			t.Fatalf("sent %+v", env.relayer.sent)
		}
		if rows := env.relayRows(); len(rows) != 3 {
			t.Fatalf("rows %+v", rows)
		}

		// a transaction that's out and unsettled comes first, and holds its key's next
		env.relayer.sendErr = nil
		env.relayer.sent = nil
		model.SetSnFleetAutoRelaySent(ctx, topUp.RelayId, "0xabc", 7, "0xdef", []byte{9})
		env.relayer.status = model.SnFleetAutoRelaySent
		SnFleetAutoRelayStep(ctx)
		if len(env.relayer.settled) != 1 || env.relayer.settled[0].RelayId != topUp.RelayId {
			t.Fatalf("settled %+v", env.relayer.settled)
		}
		if len(env.relayer.sent) != 1 || env.relayer.sent[0].RelayId != first.RelayId {
			t.Fatalf("sent %+v, want only the first of its key", env.relayer.sent)
		}

		// a key's nonces stay past what it signed
		if floor := model.GetSnFleetAutoRelayNonceFloor(ctx, key, "0xabc"); floor != 8 {
			t.Fatalf("floor %d", floor)
		}
		if floor := model.GetSnFleetAutoRelayNonceFloor(ctx, key, "0xother"); floor != 0 {
			t.Fatalf("floor %d", floor)
		}
		stored := model.GetSnFleetAutoRelay(ctx, topUp.RelayId)
		if stored == nil || stored.Status != model.SnFleetAutoRelayDone || stored.TxHash != "0xdef" || string(stored.RawTransaction) != string([]byte{9}) {
			t.Fatalf("stored %+v", stored)
		}
	})
}

// A device's signature is taken only for its own open binding, and only if it verifies.
// Refusals are bounded to what a device could send, and a refused device isn't chosen next.
func TestSnFleetAutoDeviceSignatureChecksDb(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		env := newTestingFleetAuto(t)
		ctx := env.ctx()
		owner, provider, sibling := env.f.owner, env.f.provider, env.f.sibling
		key := env.deploymentKey()
		hotkeySeed := [32]byte{0xb1}
		hotkey := env.hotkey(hotkeySeed)
		env.consent()
		env.authorize(hotkeySeed)
		env.register(hotkey, 42)
		providerSeed := env.device(provider, 1, 1)
		env.device(sibling, 2, 2)
		for _, byJwt := range []*session.ByJwt{provider, sibling} {
			if _, err := GetSnFleetDevice(&SnFleetDeviceArgs{}, env.session(byJwt)); err != nil {
				t.Fatal(err)
			}
		}
		env.settle()
		SnFleetAutoStep(ctx)
		generation := model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId)
		if generation == nil {
			t.Fatal("no manifest")
		}
		env.hotkeySign(hotkeySeed)
		work, err := GetSnFleetDevice(&SnFleetDeviceArgs{}, env.session(provider))
		if err != nil || work.BindingJson == "" || work.Authorization == nil || work.HotkeySignature == "" {
			t.Fatalf("%+v %v", work, err)
		}
		binding, err := snFleetAutoBindingFromJson(work.BindingJson)
		if err != nil {
			t.Fatal(err)
		}
		digest, _ := binding.Digest()
		digestHex := "0x" + hex.EncodeToString(digest[:])
		good := "0x" + hex.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(providerSeed), digest[:]))

		refused := func(name string, args *SnFleetDeviceSignatureArgs, byJwt *session.ByJwt) {
			t.Helper()
			result, err := SnFleetDeviceSignature(args, env.session(byJwt))
			if err != nil || result.Accepted || result.Error == nil {
				t.Fatalf("%s: %+v %v", name, result, err)
			}
		}
		refused("a signature that doesn't verify", &SnFleetDeviceSignatureArgs{Digest: digestHex, ClientSignature: "0x" + strings.Repeat("11", 64)}, provider)
		refused("another device's binding", &SnFleetDeviceSignatureArgs{Digest: digestHex, ClientSignature: good}, sibling)
		refused("another digest", &SnFleetDeviceSignatureArgs{Digest: "0x" + strings.Repeat("22", 32), ClientSignature: good}, provider)
		refused("an unknown refusal", &SnFleetDeviceSignatureArgs{Digest: digestHex, Refusal: "because"}, provider)
		if result, err := SnFleetDeviceSignature(&SnFleetDeviceSignatureArgs{Digest: digestHex, ClientSignature: good}, env.session(provider)); err != nil || !result.Accepted {
			t.Fatalf("%+v %v", result, err)
		}

		// the sibling refuses. It's offered nothing more, and the next batch leaves it out
		if result, err := SnFleetDeviceSignature(&SnFleetDeviceSignatureArgs{Refusal: sdkprotocol.FleetRefusalHotkeyChanged}, env.session(sibling)); err != nil || !result.Accepted {
			t.Fatalf("%+v %v", result, err)
		}
		if work, err := GetSnFleetDevice(&SnFleetDeviceArgs{}, env.session(sibling)); err != nil || work.BindingJson != "" {
			t.Fatalf("%+v %v", work, err)
		}
		claim, _ := GetSnFleetClaim(env.session(owner))
		if claim.Counts.Refused != 1 || claim.Counts.Chosen != 2 || claim.Counts.Ready != 1 {
			t.Fatalf("%+v", claim.Counts)
		}
		// a device past the epoch of its binding isn't offered it
		env.epoch = &StEpochState{Epoch: 11, EpochStartBlock: 550_400, TEpochBlocks: 50_400, HeadBlock: 560_000}
		// and the next batch chooses the one that signed and not the one that refused
		for _, byJwt := range []*session.ByJwt{provider, sibling} {
			if _, err := GetSnFleetDevice(&SnFleetDeviceArgs{}, env.session(byJwt)); err != nil {
				t.Fatal(err)
			}
		}
		SnFleetAutoStep(ctx)
		next := model.GetSnFleetAutoNewestGeneration(ctx, key, owner.NetworkId)
		if next == nil || next.Generation == generation.Generation {
			t.Fatalf("next %+v", next)
		}
		members := model.GetSnFleetAutoMembers(ctx, key, owner.NetworkId, next.Generation)
		if len(members) != 1 || members[0].ClientId != *provider.ClientId {
			t.Fatalf("members %+v", members)
		}
	})
}

// The network's providers that haven't asked for work show as still to update.
func TestSnFleetAutoStillToUpdateCountDb(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		env := newTestingFleetAuto(t)
		ctx := env.ctx()
		key := env.deploymentKey()
		networkId := env.f.owner.NetworkId
		handlerId := model.CreateNetworkClientHandler(ctx)
		online := func(clientId server.Id) {
			if _, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.1:4000", handlerId); err != nil {
				t.Fatal(err)
			}
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModePublic: make([]byte, 32)})
		}
		provider, sibling := *env.f.provider.ClientId, *env.f.sibling.ClientId
		online(provider)
		online(sibling)
		since := server.NowUtc().Add(-time.Hour)
		if count := model.CountSnFleetAutoStillToUpdate(ctx, key, networkId, since); count != 2 {
			t.Fatalf("count %d", count)
		}
		model.TouchSnFleetAutoDevice(ctx, key, networkId, provider, server.NowUtc())
		if count := model.CountSnFleetAutoStillToUpdate(ctx, key, networkId, since); count != 1 {
			t.Fatalf("count %d", count)
		}
		// a device that asked long ago has gone quiet
		if count := model.CountSnFleetAutoStillToUpdate(ctx, key, networkId, server.NowUtc().Add(time.Hour)); count != 2 {
			t.Fatalf("count %d", count)
		}
	})
}
