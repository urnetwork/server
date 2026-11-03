package controller

// Claiming a Top 200 spot from one tap (apple BTEASY.md, "Claiming a Top 200 spot").
// The tap covers the whole network: the server chooses the devices that add score, one
// per IP prefix up to the per-fleet priority share, builds the manifest once per
// settlement epoch, carries each device its binding, takes its client signature and
// relays the result. Renewing is the same with no tap. The rest of the network stays in
// the pool.
//
// The server decides who is asked, and nothing else: the hotkey is on the key holder's
// device, each client key is on its own device, and a device checks everything it's
// handed (sdk sn_fleet_device.go) against what the network's coldkey signed.
//
// Everything here is dark while binding is closed: routes answer closed, nothing is
// scheduled, nothing is stored and nothing is read from the chain. A device's check is
// answered from a short list of networks held in memory, so about 100k providers asking
// a few times a day cost one in-memory lookup each.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/glog"
	sdkprotocol "github.com/urnetwork/sdk/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const (
	// how often an idle device asks. While binding is closed a device asks about once a
	// week, which across 100k providers is about one request every six seconds; it
	// saves the schedule, so a restart doesn't ask sooner. A device learns that
	// binding opened at its next check, so shorten this a week before opening
	snFleetAutoCheckClosed = 7 * 24 * time.Hour
	snFleetAutoCheckOpen   = 6 * time.Hour
	// a chosen device still waiting for its binding asks sooner
	snFleetAutoCheckChosen = time.Hour

	// A device that hasn't asked for this long is taken to be off or gone, and isn't
	// chosen.
	snFleetAutoDeviceFresh = 2 * snFleetAutoCheckOpen

	// How long after an authorization the first manifest waits, so the network's
	// devices have had time to ask. Manifests are built once per epoch, so one built
	// too soon would leave the rest out for a week.
	snFleetAutoSettle = 2 * snFleetAutoCheckOpen

	// A new UID is immune from replacement for this long, and a binding can start up to
	// 7 days away, so a hotkey registers inside this window before its first epoch
	snFleetAutoImmunityBlocks = 21_600
	// and early enough to settle, publish, mirror, sign and bind before the epoch starts
	snFleetAutoRegisterRunwayBlocks = 5_400

	// a binding is renewed from this many epochs before it ends
	snFleetAutoRenewEpochs = 1

	// the short list's age
	snFleetAutoNetworksTtl = time.Minute
)

// Binding opening is the release decision behind snTopBindingOpen. A variable only so
// tests can run the open paths.
var snFleetAutoOpen = func() bool {
	return snTopBindingOpen
}

// SnFleetAutoOpen is whether the claim runs at all, for what the taskworker schedules.
func SnFleetAutoOpen() bool {
	return snFleetAutoOpen()
}

// The chain reads of the flow. CoreStClient implements it; tests supply their own.
type snFleetAutoChain interface {
	// the coordinator's mirror of the hotkey's commitment
	MirroredCommitment(ctx context.Context, hotkey [32]byte) (hash [32]byte, finalizedBlock uint64, err error)
	// the live burn of the deployment's subnet
	RegistrationBurnRao(ctx context.Context, netuid uint16) (uint64, error)
	// the native fee of a registration
	RegistrationFeeRao(ctx context.Context, netuid uint16) (uint64, error)
	FreeBalanceRao(ctx context.Context, account [32]byte) (uint64, error)
}

// The chain client: nil when the st subsystem has no client. A variable so tests can
// supply their own.
var snFleetAutoChainFn = func() snFleetAutoChain {
	client := stClient()
	chain, _ := client.(snFleetAutoChain)
	return chain
}

// snFleetAutoEpoch is what the flow needs of the settlement clock. A variable so tests
// can supply their own.
var snFleetAutoEpochFn = func(ctx context.Context) *StEpochState {
	_, _, state := snChainState(ctx)
	return state
}

// The newest metagraph census's UID holders, nil without one. A variable so tests needn't
// write a whole census.
var snFleetAutoHoldersFn = func(ctx context.Context, deploymentKey model.StDeploymentKey) *model.SnUidHolders {
	if snapshot := model.GetLatestSnIncentiveSnapshot(ctx, deploymentKey); snapshot != nil {
		return snapshot.Holders()
	}
	return nil
}

// Whether the board prompts a network to claim: the same prompt the apps show. A
// variable so tests needn't write a whole snapshot.
var snFleetAutoClaimPromptFn = func(ctx context.Context, networkId server.Id) bool {
	snapshot := snTopLatestSnapshot(ctx)
	if snapshot == nil {
		return false
	}
	row := model.GetSnTopNetworkRow(ctx, snapshot.SnapshotId, networkId)
	return row != nil && row.ClaimPrompt
}

// What a device's fresh prefix and measurement read. A variable so tests can supply
// their own without Redis.
type snFleetAutoEvidence struct {
	ClientKeys map[server.Id][32]byte
	Eligible   map[server.Id]bool
	Prefixes   map[server.Id][32]byte
	Measured   map[server.Id]bool
	PerFleet   int
}

var snFleetAutoEvidenceFn = func(ctx context.Context, clientIds []server.Id, now time.Time) *snFleetAutoEvidence {
	settings := VerifySettings()
	evidence := &snFleetAutoEvidence{
		ClientKeys: model.GetStContributingClientCkeys(ctx, clientIds),
		Eligible:   model.GetVerifyEligibleClients(ctx, clientIds),
		Prefixes:   model.GetSnTopPrefixPins(ctx, clientIds, settings, model.SnTopCompletedTempo(snTopTempoAnchor(ctx), now)),
		PerFleet:   model.VerifyCohortPriorityPerFleet(settings),
	}
	epochMembers, lifetimeMembers := model.GetVerifyCohortMembership(ctx, settings, clientIds)
	evidence.Measured = map[server.Id]bool{}
	for clientId := range epochMembers {
		evidence.Measured[clientId] = true
	}
	for clientId := range lifetimeMembers {
		evidence.Measured[clientId] = true
	}
	return evidence
}

// The short list every device's check is answered from: the networks with an
// authorization. Most devices' networks aren't on it, and those answers touch nothing.
var snFleetAutoNetworks = struct {
	lock       sync.Mutex
	deployment model.StDeploymentKey
	networkIds map[server.Id]bool
	expiry     time.Time
}{}

func snFleetAutoNetworkListed(ctx context.Context, deploymentKey model.StDeploymentKey, networkId server.Id, now time.Time) bool {
	snFleetAutoNetworks.lock.Lock()
	if snFleetAutoNetworks.deployment == deploymentKey && now.Before(snFleetAutoNetworks.expiry) {
		listed := snFleetAutoNetworks.networkIds[networkId]
		snFleetAutoNetworks.lock.Unlock()
		return listed
	}
	snFleetAutoNetworks.lock.Unlock()
	networkIds := model.GetSnFleetAutoNetworkIds(ctx, deploymentKey)
	snFleetAutoNetworks.lock.Lock()
	defer snFleetAutoNetworks.lock.Unlock()
	snFleetAutoNetworks.deployment = deploymentKey
	snFleetAutoNetworks.networkIds = networkIds
	snFleetAutoNetworks.expiry = now.Add(snFleetAutoNetworksTtl)
	return networkIds[networkId]
}

// So the process that wrote an authorization sees it right away.
func snFleetAutoResetNetworks() {
	snFleetAutoNetworks.lock.Lock()
	defer snFleetAutoNetworks.lock.Unlock()
	snFleetAutoNetworks.expiry = time.Time{}
}

func snFleetAutoHex(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

// The fleet id of a network's hotkey. It only has to be stable and nonzero, and the
// same for every generation of the fleet.
func snFleetAutoFleetId(networkId server.Id, hotkey [32]byte) [32]byte {
	hash := sha256.New()
	hash.Write([]byte("urnetwork/one-tap-fleet/v1"))
	hash.Write(networkId[:])
	hash.Write(hotkey[:])
	return [32]byte(hash.Sum(nil))
}

/**
 * A device's routes
 */

type SnFleetSignedMessage struct {
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

type SnFleetDeviceArgs struct {
	// this device has no pinned coldkey yet, so it wants the network's consent
	NeedConsent bool
}

// SnFleetDeviceResult answers a device's check (`GET /sn/fleet/device`). Almost always
// it says only when to ask again.
type SnFleetDeviceResult struct {
	BindingOpen      bool                  `json:"binding_open"`
	NextCheckSeconds int64                 `json:"next_check_seconds"`
	Consent          *SnFleetSignedMessage `json:"consent,omitempty"`
	Authorization    *SnFleetSignedMessage `json:"authorization,omitempty"`
	BindingJson      string                `json:"binding_json,omitempty"`
	HotkeySignature  string                `json:"hotkey_signature,omitempty"`
}

// The network's newest consent, for a device to pin its coldkey the first time it
// sees one. A failed read is no consent: the device asks again next time.
func snFleetAutoConsent(ctx context.Context, networkId server.Id) (consent *SnFleetSignedMessage) {
	domain, ok := stClientKeyHistoryDomain()
	if !ok {
		return nil
	}
	digest, err := domain.Digest()
	if err != nil {
		return nil
	}
	server.HandleError(func() {
		raw := model.GetSnFleetAutoConsentOriginal(ctx, digest, networkId)
		if raw == nil {
			return
		}
		var original protocol.WalletMappingConsent
		if err := json.Unmarshal(raw, &original); err != nil {
			return
		}
		consent = &SnFleetSignedMessage{Message: original.Message, Signature: snFleetAutoHex(original.Signature[:])}
	}, func(err error) {
		glog.Infof("[sn]fleet consent read failed: %s\n", err)
	})
	return consent
}

// GetSnFleetDevice answers one device's check.
func GetSnFleetDevice(args *SnFleetDeviceArgs, clientSession *session.ClientSession) (*SnFleetDeviceResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	networkId := clientSession.ByJwt.NetworkId
	clientId := *clientSession.ByJwt.ClientId
	result := &SnFleetDeviceResult{NextCheckSeconds: int64(snFleetAutoCheckClosed / time.Second)}
	// a device pins before there's anything to attack, so this is the one thing a
	// closed answer carries
	if args.NeedConsent {
		result.Consent = snFleetAutoConsent(ctx, networkId)
	}
	if !snFleetAutoOpen() {
		return result, nil
	}
	deploymentKey, ok := StDeploymentKey()
	if !ok {
		return result, nil
	}
	result.BindingOpen = true
	result.NextCheckSeconds = int64(snFleetAutoCheckOpen / time.Second)
	if !snFleetAutoNetworkListed(ctx, deploymentKey, networkId, now) {
		return result, nil
	}
	model.TouchSnFleetAutoDevice(ctx, deploymentKey, networkId, clientId, now)
	// a device that left asks without a pin only after a person reset it
	if args.NeedConsent {
		model.ClearSnFleetAutoDeviceLeft(ctx, deploymentKey, networkId, clientId)
	}
	member := model.GetSnFleetAutoDeviceMember(ctx, deploymentKey, networkId, clientId)
	if member == nil || member.Refusal != "" || member.ClientSignature != nil {
		return result, nil
	}
	state := snFleetAutoEpochFn(ctx)
	if state == nil || member.ValidFromEpoch <= state.Epoch {
		// its epoch has started: the next batch makes a new one
		return result, nil
	}
	if member.HotkeySignature == nil {
		result.NextCheckSeconds = int64(snFleetAutoCheckChosen / time.Second)
		return result, nil
	}
	authorization := model.GetSnFleetAutoAuthorization(ctx, deploymentKey, networkId)
	if authorization == nil {
		return result, nil
	}
	result.Authorization = &SnFleetSignedMessage{Message: authorization.Message, Signature: snFleetAutoHex(authorization.Signature)}
	result.BindingJson = member.BindingJson
	result.HotkeySignature = snFleetAutoHex(member.HotkeySignature)
	return result, nil
}

type SnFleetAutoError struct {
	Message string `json:"message"`
}

func snFleetAutoFail(format string, args ...any) *SnFleetAutoError {
	return &SnFleetAutoError{Message: fmt.Sprintf(format, args...)}
}

const snFleetAutoClosedMessage = "Top 200 binding isn't open yet."

type SnFleetDeviceSignatureArgs struct {
	Digest          string `json:"digest"`
	ClientSignature string `json:"client_signature,omitempty"`
	Refusal         string `json:"refusal,omitempty"`
}

type SnFleetDeviceSignatureResult struct {
	Accepted bool              `json:"accepted"`
	Error    *SnFleetAutoError `json:"error,omitempty"`
}

// SnFleetDeviceSignature takes a device's signature on its own binding, or why it
// refused (`POST /sn/fleet/device/signature`). A signature that doesn't verify under
// the key in the binding isn't stored.
func SnFleetDeviceSignature(args *SnFleetDeviceSignatureArgs, clientSession *session.ClientSession) (*SnFleetDeviceSignatureResult, error) {
	fail := func(format string, values ...any) (*SnFleetDeviceSignatureResult, error) {
		return &SnFleetDeviceSignatureResult{Error: snFleetAutoFail(format, values...)}, nil
	}
	if !snFleetAutoOpen() {
		return fail(snFleetAutoClosedMessage)
	}
	deploymentKey, ok := StDeploymentKey()
	if !ok {
		return fail(snFleetAutoClosedMessage)
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	clientId := *clientSession.ByJwt.ClientId
	if args.Refusal != "" {
		if !sdkprotocol.FleetRefusalKnown(args.Refusal) {
			return fail("Unknown refusal.")
		}
		model.SetSnFleetAutoRefusal(ctx, deploymentKey, networkId, clientId, args.Refusal)
		snFleetAutoRefusals.WithLabelValues(args.Refusal).Inc()
		return &SnFleetDeviceSignatureResult{Accepted: true}, nil
	}
	digest, err := snParseHexBytes(args.Digest)
	if err != nil || len(digest) != 32 {
		return fail("digest must be 32 bytes of hex.")
	}
	signature, err := snParseHexBytes(args.ClientSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fail("client_signature must be 64 bytes of hex.")
	}
	member := model.GetSnFleetAutoDeviceMember(ctx, deploymentKey, networkId, clientId)
	if member == nil || string(member.Digest) != string(digest) {
		return fail("That isn't this device's binding.")
	}
	if !ed25519.Verify(ed25519.PublicKey(member.ClientKey[:]), digest, signature) {
		return fail("client_signature doesn't verify for this binding.")
	}
	if !model.SetSnFleetAutoClientSignature(ctx, deploymentKey, networkId, clientId, digest, signature) {
		return fail("That binding is no longer open.")
	}
	return &SnFleetDeviceSignatureResult{Accepted: true}, nil
}

// SnFleetDeviceBindingResult is a device's newest binding on chain, for Leave, whatever
// made it: this flow or `provider fleet bind`.
type SnFleetDeviceBindingResult struct {
	Bound          bool   `json:"bound"`
	ChainId        uint64 `json:"chain_id,omitempty"`
	Netuid         uint64 `json:"netuid,omitempty"`
	Coordinator    string `json:"coordinator,omitempty"`
	Generation     uint64 `json:"generation,omitempty"`
	ValidFromEpoch uint64 `json:"valid_from_epoch,omitempty"`
	ValidToEpoch   uint64 `json:"valid_to_epoch,omitempty"`
	CurrentEpoch   uint64 `json:"current_epoch,omitempty"`
}

// The newest binding of a client that hasn't ended by the epoch, from the census.
func snFleetAutoOpenVersion(versions []*model.SnFleetBindingVersion, clientId server.Id, epoch uint64) *model.SnFleetBindingVersion {
	var newest *model.SnFleetBindingVersion
	for _, version := range versions {
		if version.ClientId != clientId || version.Cleaned || version.ValidToEpoch < epoch {
			continue
		}
		if newest == nil || newest.Generation < version.Generation {
			newest = version
		}
	}
	return newest
}

// GetSnFleetDeviceBinding is `GET /sn/fleet/device/binding`.
func GetSnFleetDeviceBinding(clientSession *session.ClientSession) (*SnFleetDeviceBindingResult, error) {
	result := &SnFleetDeviceBindingResult{}
	if !snFleetAutoOpen() {
		return result, nil
	}
	cfg := stConfig()
	deploymentKey, ok := StDeploymentKey()
	if cfg == nil || !ok {
		return result, nil
	}
	ctx := clientSession.Ctx
	state := snFleetAutoEpochFn(ctx)
	if state == nil {
		return nil, errors.New("503 The chain can't be read right now.")
	}
	clientId := *clientSession.ByJwt.ClientId
	version := snFleetAutoOpenVersion(model.GetSnFleetBindingVersionsForClients(ctx, deploymentKey, []server.Id{clientId}), clientId, state.Epoch)
	if version == nil {
		return result, nil
	}
	return &SnFleetDeviceBindingResult{
		Bound:          true,
		ChainId:        cfg.ChainId,
		Netuid:         cfg.Netuid,
		Coordinator:    strings.ToLower(cfg.ContractAddress.Hex()),
		Generation:     version.Generation,
		ValidFromEpoch: version.ValidFromEpoch,
		ValidToEpoch:   version.ValidToEpoch,
		CurrentEpoch:   state.Epoch,
	}, nil
}

type SnFleetDeviceLeaveArgs struct {
	Generation      uint64 `json:"generation"`
	EffectiveEpoch  uint64 `json:"effective_epoch"`
	ClientSignature string `json:"client_signature,omitempty"`
}

type SnFleetDeviceLeaveResult struct {
	Left           bool              `json:"left"`
	EffectiveEpoch uint64            `json:"effective_epoch,omitempty"`
	CurrentEpoch   uint64            `json:"current_epoch,omitempty"`
	Error          *SnFleetAutoError `json:"error,omitempty"`
}

// SnFleetDeviceLeave takes a device out of its fleet from the next epoch
// (`POST /sn/fleet/device/leave`). revokeFleetBinding needs only the client key, so a
// device leaves by itself, and the relay pays. The device isn't chosen again either
// way. A leave for an epoch that has passed gets the current one back, for the device
// to sign again.
func SnFleetDeviceLeave(args *SnFleetDeviceLeaveArgs, clientSession *session.ClientSession) (*SnFleetDeviceLeaveResult, error) {
	fail := func(format string, values ...any) (*SnFleetDeviceLeaveResult, error) {
		return &SnFleetDeviceLeaveResult{Error: snFleetAutoFail(format, values...)}, nil
	}
	if !snFleetAutoOpen() {
		return fail(snFleetAutoClosedMessage)
	}
	cfg := stConfig()
	deploymentKey, ok := StDeploymentKey()
	if cfg == nil || !ok {
		return fail(snFleetAutoClosedMessage)
	}
	ctx := clientSession.Ctx
	now := server.NowUtc()
	networkId := clientSession.ByJwt.NetworkId
	clientId := *clientSession.ByJwt.ClientId
	model.SetSnFleetAutoDeviceLeft(ctx, deploymentKey, networkId, clientId, now)
	if args.ClientSignature == "" {
		// nothing on chain to end
		return &SnFleetDeviceLeaveResult{Left: true}, nil
	}
	state := snFleetAutoEpochFn(ctx)
	if state == nil {
		return fail("The chain can't be read right now. Try again.")
	}
	if args.EffectiveEpoch != state.Epoch+1 {
		return &SnFleetDeviceLeaveResult{CurrentEpoch: state.Epoch}, nil
	}
	signature, err := snParseHexBytes(args.ClientSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fail("client_signature must be 64 bytes of hex.")
	}
	version := snFleetAutoOpenVersion(model.GetSnFleetBindingVersionsForClients(ctx, deploymentKey, []server.Id{clientId}), clientId, state.Epoch)
	if version == nil || version.Generation != args.Generation {
		return fail("This device has no such binding on chain.")
	}
	if version.ValidToEpoch < args.EffectiveEpoch {
		// it ends by itself this epoch
		return &SnFleetDeviceLeaveResult{Left: true}, nil
	}
	revoke := protocol.FleetRevoke{ChainID: cfg.ChainId, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, ClientID: [16]byte(clientId), Generation: args.Generation, EffectiveEpoch: args.EffectiveEpoch}
	digest, err := revoke.Digest()
	if err != nil {
		return fail("Invalid revocation: %s", err)
	}
	// the coordinator checks the key it holds, and the relay's preflight finds out
	// before spending anything. This refuses what's plainly wrong without a chain read
	if key, known := model.GetStContributingClientCkeys(ctx, []server.Id{clientId})[clientId]; known && !ed25519.Verify(ed25519.PublicKey(key[:]), digest[:], signature) {
		return fail("client_signature doesn't verify for this revocation.")
	}
	calldata, err := stabi.NewSTCoordinator().TryPackRevokeFleetBinding([16]byte(clientId), args.Generation, args.EffectiveEpoch, signature)
	if err != nil {
		return fail("Invalid revocation: %s", err)
	}
	model.EnqueueSnFleetAutoRelay(ctx, deploymentKey, &model.SnFleetAutoRelay{
		RelayId:   server.NewId(),
		NetworkId: networkId,
		Kind:      model.SnFleetAutoRelayRevoke,
		Operation: fmt.Sprintf("revoke:%s:%d:%d", clientId, args.Generation, args.EffectiveEpoch),
		FromKey:   model.SnFleetAutoRelayKeyBinding,
		ToAddress: cfg.ContractAddress.Hex(),
		Calldata:  calldata,
	})
	return &SnFleetDeviceLeaveResult{Left: true, EffectiveEpoch: args.EffectiveEpoch}, nil
}

/**
 * The key holder's routes
 */

type SnFleetHotkeyWork struct {
	BindingJson string `json:"binding_json"`
}

type SnFleetManifestState struct {
	Generation uint64 `json:"generation"`
	Manifest   string `json:"manifest"`
	Mirrored   bool   `json:"mirrored"`
}

// How many of the network's devices are where, for its card.
type SnFleetDeviceCounts struct {
	Chosen        int64 `json:"chosen"`
	Ready         int64 `json:"ready"`
	Bound         int64 `json:"bound"`
	Refused       int64 `json:"refused"`
	StillToUpdate int64 `json:"still_to_update"`
}

type SnFleetTopUpState struct {
	Used      bool   `json:"used"`
	Available bool   `json:"available"`
	AmountRao uint64 `json:"amount_rao,omitempty"`
	Landed    bool   `json:"landed"`
}

// SnFleetClaimResult is `GET /sn/fleet/claim`: the network's claim, what the key
// holder's device has to do next, and what its card shows.
type SnFleetClaimResult struct {
	BindingOpen          bool                  `json:"binding_open"`
	ClaimPrompt          bool                  `json:"claim_prompt"`
	ConsentColdkey       string                `json:"consent_coldkey"`
	ChainId              uint64                `json:"chain_id"`
	Netuid               uint64                `json:"netuid"`
	Coordinator          string                `json:"coordinator"`
	AuthorizationMessage string                `json:"authorization_message"`
	Registered           bool                  `json:"registered"`
	Uid                  uint64                `json:"uid"`
	CurrentEpoch         uint64                `json:"current_epoch"`
	CurrentBlock         uint64                `json:"current_block"`
	NextEpochStartBlock  uint64                `json:"next_epoch_start_block"`
	RegisterAfterBlock   uint64                `json:"register_after_block"`
	RegisterBeforeBlock  uint64                `json:"register_before_block"`
	Manifest             *SnFleetManifestState `json:"manifest,omitempty"`
	HotkeyWork           []*SnFleetHotkeyWork  `json:"hotkey_work,omitempty"`
	Counts               SnFleetDeviceCounts   `json:"counts"`
	TopUp                SnFleetTopUpState     `json:"top_up"`
}

// A registration here has its UID still immune when its binding starts, and leaves
// time to bind first. Past the runway the window moves to the next epoch.
func snFleetAutoRegisterWindow(state *StEpochState) (after uint64, before uint64, nextEpochStart uint64) {
	nextEpochStart = state.EpochStartBlock + state.TEpochBlocks
	before = nextEpochStart - min(nextEpochStart, snFleetAutoRegisterRunwayBlocks)
	after = nextEpochStart - min(nextEpochStart, snFleetAutoImmunityBlocks)
	if before < state.HeadBlock {
		after += state.TEpochBlocks
		before += state.TEpochBlocks
	}
	return after, before, nextEpochStart
}

// The UID a hotkey holds at the newest metagraph census.
func snFleetAutoUid(holders *model.SnUidHolders, hotkey [32]byte) (uid uint16, registered bool) {
	if holders == nil {
		return 0, false
	}
	for candidate, holder := range holders.Holders {
		if holder.Hotkey == hotkey {
			return candidate, true
		}
	}
	return 0, false
}

// GetSnFleetClaim is `GET /sn/fleet/claim`. Read by the key holder's device, whenever
// it's running, and by any device of the network for the counts.
func GetSnFleetClaim(clientSession *session.ClientSession) (*SnFleetClaimResult, error) {
	result := &SnFleetClaimResult{}
	if !snFleetAutoOpen() {
		return result, nil
	}
	result.BindingOpen = true
	cfg := stConfig()
	deploymentKey, ok := StDeploymentKey()
	if cfg == nil || !ok {
		result.BindingOpen = false
		return result, nil
	}
	ctx := clientSession.Ctx
	now := server.NowUtc()
	networkId := clientSession.ByJwt.NetworkId
	result.ChainId, result.Netuid = cfg.ChainId, cfg.Netuid
	result.Coordinator = strings.ToLower(cfg.ContractAddress.Hex())
	if domain, ok := stClientKeyHistoryDomain(); ok {
		consent, err := model.GetNetworkWalletMappingConsent(ctx, domain, networkId)
		if err != nil {
			return nil, err
		}
		if consent != nil {
			result.ConsentColdkey = consent.ColdkeySs58
		}
	}
	result.ClaimPrompt = snFleetAutoClaimPromptFn(ctx, networkId)
	state := snFleetAutoEpochFn(ctx)
	if state != nil {
		result.CurrentEpoch, result.CurrentBlock = state.Epoch, state.HeadBlock
		result.RegisterAfterBlock, result.RegisterBeforeBlock, result.NextEpochStartBlock = snFleetAutoRegisterWindow(state)
	}
	topUp := model.GetSnFleetAutoTopUp(ctx, deploymentKey, networkId)
	if topUp != nil {
		result.TopUp.Used = true
		result.TopUp.AmountRao = topUp.AmountRao
		result.TopUp.Landed = topUp.Status == model.SnFleetAutoRelayDone
	} else {
		result.TopUp.Available = result.ClaimPrompt && snFleetRelayerFn().Available(ctx, model.SnFleetAutoRelayKeyTopUp)
	}

	authorization := model.GetSnFleetAutoAuthorization(ctx, deploymentKey, networkId)
	if authorization == nil {
		result.Counts.StillToUpdate = 0
		return result, nil
	}
	result.AuthorizationMessage = authorization.Message
	uid, registered := snFleetAutoUid(snFleetAutoHoldersFn(ctx, deploymentKey), authorization.Hotkey)
	result.Registered, result.Uid = registered, uint64(uid)
	result.Counts.StillToUpdate = model.CountSnFleetAutoStillToUpdate(ctx, deploymentKey, networkId, now.Add(-snFleetAutoDeviceFresh))

	generation := model.GetSnFleetAutoNewestGeneration(ctx, deploymentKey, networkId)
	if generation == nil {
		return result, nil
	}
	result.Manifest = &SnFleetManifestState{Generation: generation.Generation, Manifest: generation.Manifest, Mirrored: generation.MirroredBlock != nil}
	members := model.GetSnFleetAutoMembers(ctx, deploymentKey, networkId, generation.Generation)
	clientIds := make([]server.Id, 0, len(members))
	for _, member := range members {
		clientIds = append(clientIds, member.ClientId)
	}
	versions := model.GetSnFleetBindingVersionsForClients(ctx, deploymentKey, clientIds)
	for _, member := range members {
		result.Counts.Chosen += 1
		if member.Refusal != "" {
			result.Counts.Refused += 1
			continue
		}
		version := (*model.SnFleetBindingVersion)(nil)
		if state != nil {
			version = snFleetAutoOpenVersion(versions, member.ClientId, state.Epoch)
		}
		bound := version != nil && version.FleetId == generation.FleetId && version.Hotkey == generation.Hotkey
		if bound {
			result.Counts.Bound += 1
		}
		if bound || member.ClientSignature != nil {
			result.Counts.Ready += 1
		}
		// a binding still awaiting the hotkey, and whose epoch hasn't begun
		if member.BindingJson != "" && member.HotkeySignature == nil && state != nil && state.Epoch < member.ValidFromEpoch {
			result.HotkeyWork = append(result.HotkeyWork, &SnFleetHotkeyWork{BindingJson: member.BindingJson})
		}
	}
	return result, nil
}

type SnFleetAuthorizationArgs struct {
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

type SnFleetAuthorizationResult struct {
	Accepted bool              `json:"accepted"`
	Error    *SnFleetAutoError `json:"error,omitempty"`
}

// SnFleetAuthorize stores the coldkey's signed authorization of a hotkey
// (`POST /sn/fleet/authorization`). Any wallet's signature serves: a key on this device,
// Talisman, btcli. It must name this network and deployment, and be signed by the coldkey
// of the network's newest consent. The owner's session only, as for the consent.
func SnFleetAuthorize(args *SnFleetAuthorizationArgs, clientSession *session.ClientSession) (*SnFleetAuthorizationResult, error) {
	fail := func(format string, values ...any) (*SnFleetAuthorizationResult, error) {
		return &SnFleetAuthorizationResult{Error: snFleetAutoFail(format, values...)}, nil
	}
	if !snFleetAutoOpen() {
		return fail(snFleetAutoClosedMessage)
	}
	cfg := stConfig()
	deploymentKey, ok := StDeploymentKey()
	if cfg == nil || !ok {
		return fail(snFleetAutoClosedMessage)
	}
	owner, err := snNetworkWalletMappingOwner(clientSession)
	if err != nil {
		return fail("Only the network's owner can authorize its hotkey.")
	}
	ctx := clientSession.Ctx
	signature, err := snParseHexBytes(args.Signature)
	if err != nil || len(signature) != 64 {
		return fail("signature must be 64 bytes of hex.")
	}
	// a consent or any other signed text fails here, whatever its signature
	authorization, err := sdkprotocol.VerifyFleetHotkeyAuthorization(args.Message, signature)
	if err != nil {
		return fail("The authorization doesn't verify: %s.", err)
	}
	if authorization.NetworkID != [16]byte(owner.NetworkId) {
		return fail("The authorization names another network.")
	}
	if authorization.ChainID != cfg.ChainId || uint64(authorization.Netuid) != cfg.Netuid || common.Address(authorization.Coordinator) != cfg.ContractAddress {
		return fail("The authorization names another deployment.")
	}
	consent, err := model.GetNetworkWalletMappingConsent(ctx, owner.Domain, owner.NetworkId)
	if err != nil {
		return nil, err
	}
	if consent == nil {
		return fail("The network has no payout consent yet.")
	}
	if consent.Coldkey != authorization.Coldkey {
		return fail("The authorization isn't signed by the coldkey of the network's payout consent.")
	}
	if SnWalletBanned(authorization.Hotkey) {
		return fail("This hotkey can't be used.")
	}
	err = model.SetSnFleetAutoAuthorization(ctx, deploymentKey, owner.UserId, &model.SnFleetAutoAuthorization{
		NetworkId: owner.NetworkId,
		Coldkey:   authorization.Coldkey,
		Hotkey:    authorization.Hotkey,
		FleetId:   snFleetAutoFleetId(owner.NetworkId, authorization.Hotkey),
		Message:   args.Message,
		Signature: signature,
	})
	if err != nil {
		return fail("%s.", err)
	}
	snFleetAutoResetNetworks()
	snFleetAutoAuthorizations.Inc()
	return &SnFleetAuthorizationResult{Accepted: true}, nil
}

type SnFleetHotkeySignature struct {
	Digest          string `json:"digest"`
	HotkeySignature string `json:"hotkey_signature"`
}

type SnFleetHotkeySignaturesArgs struct {
	Signatures []*SnFleetHotkeySignature `json:"signatures"`
}

type SnFleetHotkeySignaturesResult struct {
	Accepted int               `json:"accepted"`
	Error    *SnFleetAutoError `json:"error,omitempty"`
}

// SnFleetHotkeySignatures takes the hotkey's signatures on the newest generation's
// bindings (`POST /sn/fleet/hotkey-signatures`). Only one that verifies under the
// network's authorized hotkey is stored.
func SnFleetHotkeySignatures(args *SnFleetHotkeySignaturesArgs, clientSession *session.ClientSession) (*SnFleetHotkeySignaturesResult, error) {
	fail := func(format string, values ...any) (*SnFleetHotkeySignaturesResult, error) {
		return &SnFleetHotkeySignaturesResult{Error: snFleetAutoFail(format, values...)}, nil
	}
	if !snFleetAutoOpen() {
		return fail(snFleetAutoClosedMessage)
	}
	deploymentKey, ok := StDeploymentKey()
	if !ok {
		return fail(snFleetAutoClosedMessage)
	}
	owner, err := snNetworkWalletMappingOwner(clientSession)
	if err != nil {
		return fail("Only the network's owner can do this.")
	}
	ctx := clientSession.Ctx
	generation := model.GetSnFleetAutoNewestGeneration(ctx, deploymentKey, owner.NetworkId)
	if generation == nil {
		return fail("There's nothing to sign.")
	}
	members := model.GetSnFleetAutoMembers(ctx, deploymentKey, owner.NetworkId, generation.Generation)
	bindings := map[string]*protocol.FleetBinding{}
	for _, member := range members {
		if member.BindingJson == "" {
			continue
		}
		binding, err := snFleetAutoBindingFromJson(member.BindingJson)
		if err != nil {
			return nil, err
		}
		if binding.Hotkey != generation.Hotkey {
			return nil, errors.New("a stored binding names another hotkey")
		}
		bindings[string(member.Digest)] = binding
	}
	accepted := 0
	for _, item := range args.Signatures {
		if item == nil {
			continue
		}
		digest, err := snParseHexBytes(item.Digest)
		if err != nil {
			continue
		}
		binding := bindings[string(digest)]
		if binding == nil {
			continue
		}
		signature, err := snParseHexBytes(item.HotkeySignature)
		if err != nil || !binding.VerifyHotkey(signature) {
			continue
		}
		if model.SetSnFleetAutoHotkeySignature(ctx, deploymentKey, owner.NetworkId, digest, signature) {
			accepted += 1
		}
	}
	if accepted == 0 && 0 < len(args.Signatures) {
		return fail("None of those signatures fit an open binding.")
	}
	return &SnFleetHotkeySignaturesResult{Accepted: accepted}, nil
}

func snFleetAutoBindingFromJson(bindingJson string) (*protocol.FleetBinding, error) {
	var wire SnFleetBinding
	if err := json.Unmarshal([]byte(bindingJson), &wire); err != nil {
		return nil, err
	}
	binding, err := SnFleetBindingFromJson(&wire)
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

type SnFleetTopUpResult struct {
	AmountRao uint64            `json:"amount_rao"`
	Error     *SnFleetAutoError `json:"error,omitempty"`
}

// What a registration leaves in the account. The chain's existential deposit: a burn and
// fee that took the last of it couldn't land.
const snFleetTopUpDepositRao = 500

// The runtime refuses a transfer below this.
const snFleetTopUpMinimumRao = 100_000

// EVM value to native rao: 1 rao is 1e9 wei.
const snFleetTopUpWeiPerRao = 1_000_000_000

// The chain's transfer precompile (sn evm/src/interfaces/balanceTransfer.sol).
var snFleetTopUpPrecompile = common.HexToAddress("0x0000000000000000000000000000000000000800")

// snFleetTopUpAmount is what a top-up pays: the live burn plus its fee, what the key is
// short of it and no more, and never into a burn spike. ok is false when none is owed.
func snFleetTopUpAmount(burnRao uint64, feeRao uint64, freeRao uint64) (amountRao uint64, ok bool) {
	if uint64(sdkprotocol.FleetClaimMaxBurnRao) < burnRao {
		return 0, false
	}
	needed := burnRao + feeRao + snFleetTopUpDepositRao
	if needed <= freeRao {
		return 0, false
	}
	return max(needed-freeRao, snFleetTopUpMinimumRao), true
}

// SnFleetTopUp sends a network what registering costs (`POST /sn/fleet/top-up`): the
// live burn plus its fee, once per network, only for a network the app prompts to claim,
// and only with a funded key. Everything else fails closed: no transaction is queued.
func SnFleetTopUp(clientSession *session.ClientSession) (*SnFleetTopUpResult, error) {
	fail := func(format string, values ...any) (*SnFleetTopUpResult, error) {
		return &SnFleetTopUpResult{Error: snFleetAutoFail(format, values...)}, nil
	}
	if !snFleetAutoOpen() {
		return fail(snFleetAutoClosedMessage)
	}
	cfg := stConfig()
	deploymentKey, ok := StDeploymentKey()
	if cfg == nil || !ok {
		return fail(snFleetAutoClosedMessage)
	}
	owner, err := snNetworkWalletMappingOwner(clientSession)
	if err != nil {
		return fail("Only the network's owner can ask for this.")
	}
	ctx := clientSession.Ctx
	if model.GetSnFleetAutoTopUp(ctx, deploymentKey, owner.NetworkId) != nil {
		return fail("This network has had its top-up.")
	}
	if !snFleetAutoClaimPromptFn(ctx, owner.NetworkId) {
		return fail("This network isn't being prompted to claim a spot.")
	}
	consent, err := model.GetNetworkWalletMappingConsent(ctx, owner.Domain, owner.NetworkId)
	if err != nil {
		return nil, err
	}
	if consent == nil {
		return fail("The network has no payout consent yet.")
	}
	relayer := snFleetRelayerFn()
	if !relayer.Available(ctx, model.SnFleetAutoRelayKeyTopUp) {
		return fail("Top-ups aren't available right now.")
	}
	chain := snFleetAutoChainFn()
	if chain == nil {
		return fail("Top-ups aren't available right now.")
	}
	netuid := uint16(cfg.Netuid)
	burn, err := chain.RegistrationBurnRao(ctx, netuid)
	if err != nil {
		return fail("The chain can't be read right now.")
	}
	fee, err := chain.RegistrationFeeRao(ctx, netuid)
	if err != nil {
		return fail("The chain can't be read right now.")
	}
	free, err := chain.FreeBalanceRao(ctx, consent.Coldkey)
	if err != nil {
		return fail("The chain can't be read right now.")
	}
	amount, owed := snFleetTopUpAmount(burn, fee, free)
	if !owed {
		return fail("A top-up isn't needed, or the burn is too high right now. Try again later.")
	}
	calldata, err := snFleetTopUpCalldata(consent.Coldkey)
	if err != nil {
		return nil, err
	}
	relayId := server.NewId()
	claimed := model.ClaimSnFleetAutoTopUp(
		ctx,
		deploymentKey,
		&model.SnFleetAutoTopUp{NetworkId: owner.NetworkId, Coldkey: consent.Coldkey, BurnRao: burn, FeeRao: fee, AmountRao: amount, RelayId: relayId},
		&model.SnFleetAutoRelay{
			RelayId:   relayId,
			NetworkId: owner.NetworkId,
			Kind:      model.SnFleetAutoRelayTopUp,
			Operation: fmt.Sprintf("top_up:%s", owner.NetworkId),
			FromKey:   model.SnFleetAutoRelayKeyTopUp,
			ToAddress: snFleetTopUpPrecompile.Hex(),
			Calldata:  calldata,
			ValueWei:  fmt.Sprintf("%d%09d", amount, 0),
		},
	)
	if !claimed {
		return fail("This network has had its top-up.")
	}
	coldkey, _ := ss58.Encode(consent.Coldkey, ss58.BittensorPrefix)
	glog.Infof("[sn]fleet top-up of %d rao queued for %s\n", amount, coldkey)
	snFleetAutoTopUps.Inc()
	return &SnFleetTopUpResult{AmountRao: amount}, nil
}

/**
 * Choosing the devices
 */

// One device the plan can choose from. Only devices with a client key, a prefix and a
// fresh check get this far.
type snFleetAutoCandidate struct {
	ClientId  server.Id
	ClientKey [32]byte
	Prefix    [32]byte
	Measured  bool
	LastCheck time.Time
	// its newest binding that hasn't ended, nil without one
	Binding *model.SnFleetBindingVersion
}

// One chosen device and what its binding needs.
type snFleetAutoPlanMember struct {
	Candidate *snFleetAutoCandidate
	// false when its binding runs on: it's in the manifest and needs no transaction
	Bind      bool
	ValidFrom uint64
	ValidTo   uint64
}

func snFleetAutoInFleet(version *model.SnFleetBindingVersion, fleetId [32]byte, hotkey [32]byte) bool {
	return version != nil && version.FleetId == fleetId && version.Hotkey == hotkey
}

// The better of two devices for one prefix, and the same for prefixes against each
// other: one that's bound already costs no transaction and keeps its score, then one the
// validator measures, then one that asked most recently, then by client id so the choice
// doesn't depend on order.
func snFleetAutoBetter(a *snFleetAutoCandidate, b *snFleetAutoCandidate, fleetId [32]byte, hotkey [32]byte) int {
	aBound, bBound := snFleetAutoInFleet(a.Binding, fleetId, hotkey), snFleetAutoInFleet(b.Binding, fleetId, hotkey)
	if aBound != bBound {
		if aBound {
			return -1
		}
		return 1
	}
	if a.Measured != b.Measured {
		if a.Measured {
			return -1
		}
		return 1
	}
	if !a.LastCheck.Equal(b.LastCheck) {
		if a.LastCheck.After(b.LastCheck) {
			return -1
		}
		return 1
	}
	return a.ClientId.Cmp(b.ClientId)
}

// snFleetAutoPlan chooses the devices that add score: score counts distinct prefixes, so
// a second device on a prefix adds nothing and would lose its pool share. One per prefix,
// up to the limit, and one per client key, since a manifest can't name a key twice and
// slots on one host share theirs. A device bound to someone else's fleet into the next
// epoch can't be bound here until that ends.
func snFleetAutoPlan(candidates []*snFleetAutoCandidate, limit int, epoch uint64, fleetId [32]byte, hotkey [32]byte) []*snFleetAutoPlanMember {
	eligible := make([]*snFleetAutoCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Binding != nil && !snFleetAutoInFleet(candidate.Binding, fleetId, hotkey) && epoch < candidate.Binding.ValidToEpoch {
			continue
		}
		eligible = append(eligible, candidate)
	}
	slices.SortFunc(eligible, func(a *snFleetAutoCandidate, b *snFleetAutoCandidate) int {
		return snFleetAutoBetter(a, b, fleetId, hotkey)
	})
	plan := []*snFleetAutoPlanMember{}
	prefixes := map[[32]byte]bool{}
	keys := map[[32]byte]bool{}
	for _, candidate := range eligible {
		if limit <= len(plan) {
			break
		}
		if prefixes[candidate.Prefix] || keys[candidate.ClientKey] {
			continue
		}
		prefixes[candidate.Prefix] = true
		keys[candidate.ClientKey] = true
		member := &snFleetAutoPlanMember{Candidate: candidate}
		switch {
		case !snFleetAutoInFleet(candidate.Binding, fleetId, hotkey):
			member.Bind = true
			member.ValidFrom = epoch + 1
		case epoch+snFleetAutoRenewEpochs < candidate.Binding.ValidToEpoch:
			// it runs on
		default:
			// a new binding starts where the old one ends, with no gap
			member.Bind = true
			member.ValidFrom = candidate.Binding.ValidToEpoch + 1
		}
		if member.Bind {
			member.ValidTo = member.ValidFrom + sdkprotocol.FleetBindingMaxEpochs - 1
		}
		plan = append(plan, member)
	}
	return plan
}

// The next generation: past the fleet's and past every generation a member's client has
// ever bound, since the coordinator requires each client's to go up.
func snFleetAutoNextGeneration(previous *model.SnFleetAutoGeneration, versions []*model.SnFleetBindingVersion) uint64 {
	next := uint64(1)
	if previous != nil {
		next = previous.Generation + 1
	}
	for _, version := range versions {
		next = max(next, version.Generation+1)
	}
	return next
}

// snFleetAutoBuild makes the epoch's manifest for a network when its plan binds anything.
func snFleetAutoBuild(ctx context.Context, cfg *StConfig, deploymentKey model.StDeploymentKey, authorization *model.SnFleetAutoAuthorization, previous *model.SnFleetAutoGeneration, epoch uint64, now time.Time) (*model.SnFleetAutoGeneration, error) {
	devices := model.GetSnFleetAutoDevices(ctx, deploymentKey, authorization.NetworkId, now.Add(-snFleetAutoDeviceFresh))
	clientIds := []server.Id{}
	lastCheck := map[server.Id]time.Time{}
	for _, device := range devices {
		if device.LeftTime == nil {
			clientIds = append(clientIds, device.ClientId)
			lastCheck[device.ClientId] = device.LastCheckTime
		}
	}
	if len(clientIds) == 0 {
		return nil, nil
	}
	refused := map[server.Id]bool{}
	if previous != nil {
		for _, member := range model.GetSnFleetAutoMembers(ctx, deploymentKey, authorization.NetworkId, previous.Generation) {
			if member.Refusal != "" {
				refused[member.ClientId] = true
			}
		}
	}
	evidence := snFleetAutoEvidenceFn(ctx, clientIds, now)
	versions := model.GetSnFleetBindingVersionsForClients(ctx, deploymentKey, clientIds)
	candidates := []*snFleetAutoCandidate{}
	for _, clientId := range clientIds {
		clientKey, hasKey := evidence.ClientKeys[clientId]
		prefix, hasPrefix := evidence.Prefixes[clientId]
		if !hasKey || !hasPrefix || !evidence.Eligible[clientId] || refused[clientId] {
			continue
		}
		candidates = append(candidates, &snFleetAutoCandidate{
			ClientId:  clientId,
			ClientKey: clientKey,
			Prefix:    prefix,
			Measured:  evidence.Measured[clientId],
			LastCheck: lastCheck[clientId],
			Binding:   snFleetAutoOpenVersion(versions, clientId, epoch),
		})
	}
	limit := min(evidence.PerFleet, sdkprotocol.FleetRelayDeviceLimit)
	plan := snFleetAutoPlan(candidates, limit, epoch, authorization.FleetId, authorization.Hotkey)
	binds := false
	for _, member := range plan {
		binds = binds || member.Bind
	}
	if !binds {
		return nil, nil
	}
	return snFleetAutoStoreGeneration(ctx, cfg, deploymentKey, authorization, previous, versions, plan, epoch)
}

func snFleetAutoStoreGeneration(ctx context.Context, cfg *StConfig, deploymentKey model.StDeploymentKey, authorization *model.SnFleetAutoAuthorization, previous *model.SnFleetAutoGeneration, versions []*model.SnFleetBindingVersion, plan []*snFleetAutoPlanMember, epoch uint64) (*model.SnFleetAutoGeneration, error) {
	manifest := protocol.FleetManifest{
		Schema:      protocol.FleetManifestSchema,
		ChainID:     cfg.ChainId,
		Netuid:      uint16(cfg.Netuid),
		Coordinator: cfg.ContractAddress,
		FleetID:     authorization.FleetId,
		Hotkey:      authorization.Hotkey,
		Generation:  snFleetAutoNextGeneration(previous, versions),
	}
	for _, member := range plan {
		manifest.Members = append(manifest.Members, protocol.FleetMember{ClientID: [16]byte(member.Candidate.ClientId), ClientKey: member.Candidate.ClientKey})
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		return nil, err
	}
	hash, err := manifest.CommitmentHash()
	if err != nil {
		return nil, err
	}
	generation := &model.SnFleetAutoGeneration{
		NetworkId:      authorization.NetworkId,
		Generation:     manifest.Generation,
		BuiltEpoch:     epoch,
		FleetId:        authorization.FleetId,
		Hotkey:         authorization.Hotkey,
		Manifest:       string(canonical),
		CommitmentHash: hash,
	}
	members := []*model.SnFleetAutoMember{}
	for _, planned := range plan {
		member := &model.SnFleetAutoMember{
			NetworkId:  authorization.NetworkId,
			Generation: manifest.Generation,
			ClientId:   planned.Candidate.ClientId,
			ClientKey:  planned.Candidate.ClientKey,
			PrefixHash: planned.Candidate.Prefix,
		}
		if planned.Bind {
			binding, err := manifest.Binding(protocol.FleetMember{ClientID: [16]byte(planned.Candidate.ClientId), ClientKey: planned.Candidate.ClientKey}, planned.ValidFrom, planned.ValidTo)
			if err != nil {
				return nil, err
			}
			digest, err := binding.Digest()
			if err != nil {
				return nil, err
			}
			bindingJson, err := json.Marshal(&SnFleetBinding{
				ChainId:        binding.ChainID,
				Netuid:         binding.Netuid,
				Coordinator:    strings.ToLower(common.Address(binding.Coordinator).Hex()),
				FleetId:        snFleetAutoHex(binding.FleetID[:]),
				Hotkey:         snFleetAutoHex(binding.Hotkey[:]),
				ClientId:       planned.Candidate.ClientId,
				ClientKey:      snFleetAutoHex(binding.ClientKey[:]),
				Generation:     binding.Generation,
				ValidFromEpoch: binding.ValidFromEpoch,
				ValidToEpoch:   binding.ValidToEpoch,
				CommitmentHash: snFleetAutoHex(binding.CommitmentHash[:]),
			})
			if err != nil {
				return nil, err
			}
			member.ValidFromEpoch, member.ValidToEpoch = binding.ValidFromEpoch, binding.ValidToEpoch
			member.Digest, member.BindingJson = digest[:], string(bindingJson)
		}
		members = append(members, member)
	}
	if !model.InsertSnFleetAutoGeneration(ctx, deploymentKey, generation, members) {
		return nil, nil
	}
	snFleetAutoGenerations.Inc()
	return generation, nil
}

/**
 * The periodic step
 */

// SnFleetAutoStep moves every claim forward by what the server can do: reads the mirror,
// builds the epoch's manifest, and queues signed bindings for the relay. Each network's
// failure is its own.
func SnFleetAutoStep(ctx context.Context) {
	if !snFleetAutoOpen() {
		return
	}
	cfg := stConfig()
	deploymentKey, ok := StDeploymentKey()
	if cfg == nil || !ok {
		return
	}
	state := snFleetAutoEpochFn(ctx)
	if state == nil {
		return
	}
	now := server.NowUtc()
	holders := snFleetAutoHoldersFn(ctx, deploymentKey)
	for _, authorization := range model.GetSnFleetAutoAuthorizations(ctx, deploymentKey) {
		server.HandleError(func() {
			snFleetAutoStepNetwork(ctx, cfg, deploymentKey, authorization, holders, state.Epoch, now)
		}, func(err error) {
			glog.Infof("[sn]fleet step for network %s failed: %s\n", authorization.NetworkId, err)
		})
	}
}

func snFleetAutoStepNetwork(ctx context.Context, cfg *StConfig, deploymentKey model.StDeploymentKey, authorization *model.SnFleetAutoAuthorization, holders *model.SnUidHolders, epoch uint64, now time.Time) {
	generation := model.GetSnFleetAutoNewestGeneration(ctx, deploymentKey, authorization.NetworkId)

	// once per epoch, once the hotkey holds a UID and the devices have had time to ask
	_, registered := snFleetAutoUid(holders, authorization.Hotkey)
	settled := snFleetAutoSettle <= now.Sub(authorization.UpdateTime)
	if registered && settled && (generation == nil || generation.BuiltEpoch < epoch) {
		built, err := snFleetAutoBuild(ctx, cfg, deploymentKey, authorization, generation, epoch, now)
		server.Raise(err)
		if built != nil {
			generation = built
		}
	}
	if generation == nil {
		return
	}

	if generation.MirroredBlock == nil {
		chain := snFleetAutoChainFn()
		if chain == nil {
			return
		}
		hash, block, err := chain.MirroredCommitment(ctx, generation.Hotkey)
		if err != nil {
			glog.Infof("[sn]fleet mirror read for network %s failed: %s\n", authorization.NetworkId, err)
			return
		}
		if hash != generation.CommitmentHash {
			return
		}
		model.SetSnFleetAutoGenerationMirrored(ctx, deploymentKey, authorization.NetworkId, generation.Generation, block)
		generation.MirroredBlock = &block
	}

	// signed by both, and able to start: the relay's preflight checks the rest
	for _, member := range model.GetSnFleetAutoSignedMembers(ctx, deploymentKey, authorization.NetworkId, generation.Generation) {
		if member.ValidFromEpoch <= epoch {
			continue
		}
		binding, err := snFleetAutoBindingFromJson(member.BindingJson)
		server.Raise(err)
		calldata, err := onchain.BuildFleetBindingCalldata(*binding, member.ClientSignature, member.HotkeySignature)
		if err != nil {
			glog.Infof("[sn]fleet binding of client %s can't be packed: %s\n", member.ClientId, err)
			continue
		}
		model.EnqueueSnFleetAutoRelayForMember(ctx, deploymentKey, &model.SnFleetAutoRelay{
			RelayId:   server.NewId(),
			NetworkId: authorization.NetworkId,
			Kind:      model.SnFleetAutoRelayBind,
			Operation: fmt.Sprintf("bind:%s:%d", member.ClientId, member.Generation),
			FromKey:   model.SnFleetAutoRelayKeyBinding,
			ToAddress: cfg.ContractAddress.Hex(),
			Calldata:  calldata,
		}, member)
	}
}
