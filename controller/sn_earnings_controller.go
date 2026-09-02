// sn_earnings_controller implements the points-first earnings surface:
//
//   - `GET /sn/wallet` — the caller's attached coldkeys (network + per client)
//   - `POST /sn/wallet/validate` — address checks before a wallet is attached
//   - `GET /account/epochs` — points and pool share per finalized epoch
//   - `GET /sn/head` — the server-side head-tier (Top 200) estimate
//   - `POST /sn/head/binding` — store a device's fleet-binding consent and
//     hand back the calldata the operator submits from their own key
//
// Claiming SN25α is deliberately absent: the SDK talks to the settlement
// vault directly (published artifact + on-chain proof). Nothing here signs,
// relays or enqueues a chain transaction.
package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// 1 point = 1_000_000 nano points (model.PointsToNanoPoints)
const snNanoPointsPerPoint = 1_000_000

// -----------------------------------------------------------------------------
// Wallet: read + validate
// -----------------------------------------------------------------------------

// SnWallet is one attached coldkey. ClientId is nil for the network-level
// wallet and set for a provider client's wallet.
type SnWallet struct {
	ColdkeySs58 string     `json:"coldkey_ss58"`
	ClientId    *server.Id `json:"client_id,omitempty"`
	SetAtMillis int64      `json:"set_at_millis"`
}

type SnGetWalletError struct {
	Message string `json:"message"`
}

// SnGetWalletResult lists every wallet attached inside the network. `Wallet`
// is the effective one for the session: the client-scoped wallet for a
// client session when set, else the network wallet; nil when none.
type SnGetWalletResult struct {
	Wallet  *SnWallet         `json:"wallet,omitempty"`
	Wallets []SnWallet        `json:"wallets"`
	Error   *SnGetWalletError `json:"error,omitempty"`
}

func SnGetWallet(clientSession *session.ClientSession) (*SnGetWalletResult, error) {
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	result := &SnGetWalletResult{Wallets: []SnWallet{}}
	if wallet := model.GetStWallet(ctx, networkId); wallet != nil {
		networkWallet := SnWallet{ColdkeySs58: wallet.ColdkeySs58, SetAtMillis: wallet.SetTime.UnixMilli()}
		result.Wallets = append(result.Wallets, networkWallet)
		effective := networkWallet
		result.Wallet = &effective
	}
	for _, providerWallet := range model.GetStProviderWalletsForNetwork(ctx, networkId) {
		clientId := providerWallet.ClientId
		wallet := SnWallet{
			ColdkeySs58: providerWallet.ColdkeySs58,
			ClientId:    &clientId,
			SetAtMillis: providerWallet.SetTime.UnixMilli(),
		}
		result.Wallets = append(result.Wallets, wallet)
		if clientSession.ByJwt.ClientId != nil && *clientSession.ByJwt.ClientId == clientId {
			effective := wallet
			result.Wallet = &effective
		}
	}
	return result, nil
}

// snBannedColdkeys is the operator's wallet ban list keyed by the decoded
// 32-byte public key. It is empty; populate it here (or load it into this map
// from config) to block addresses at validation and attachment time.
var snBannedColdkeys = map[[32]byte]bool{}

// SnWalletBanned reports whether a coldkey is on the operator ban list.
func SnWalletBanned(pubkey [32]byte) bool {
	return snBannedColdkeys[pubkey]
}

type SnValidateWalletArgs struct {
	Address string `json:"address"`
}

type SnValidateWalletResult struct {
	ValidSyntax   bool   `json:"valid_syntax"`
	ExistsOnChain bool   `json:"exists_on_chain"`
	Banned        bool   `json:"banned"`
	Message       string `json:"message,omitempty"`
}

// SnValidateWallet checks an address a user is about to attach: ss58 syntax
// with the Bittensor prefix, the operator ban list, and whether the account
// exists on the subtensor chain. The chain check fails open (`true` plus a
// message) so an outage never blocks a user. Unauthenticated.
func SnValidateWallet(
	args *SnValidateWalletArgs,
	clientSession *session.ClientSession,
) (*SnValidateWalletResult, error) {
	address := strings.TrimSpace(args.Address)
	pubkey, err := ss58.DecodeWithPrefix(address, ss58.BittensorPrefix)
	if err != nil {
		return &SnValidateWalletResult{
			ValidSyntax: false,
			Message:     "Not a Bittensor ss58 address.",
		}, nil
	}
	if SnWalletBanned(pubkey) {
		return &SnValidateWalletResult{
			ValidSyntax:   true,
			ExistsOnChain: true,
			Banned:        true,
			Message:       "This wallet address cannot be used.",
		}, nil
	}
	if !snWalletValidateAllow(clientSession) {
		return &SnValidateWalletResult{
			ValidSyntax:   true,
			ExistsOnChain: true,
			Message:       "Chain check skipped: too many checks from this address. Try again in a minute.",
		}, nil
	}
	exists, err := SnWalletExistsOnChain(clientSession.Ctx, pubkey)
	if err != nil {
		return &SnValidateWalletResult{
			ValidSyntax:   true,
			ExistsOnChain: true,
			Message:       "Could not reach the Bittensor chain to confirm this address; continuing without the check.",
		}, nil
	}
	result := &SnValidateWalletResult{ValidSyntax: true, ExistsOnChain: exists}
	if !exists {
		result.Message = "This address has no activity on the Bittensor chain yet."
	}
	return result, nil
}

// snUnsignedWalletSetAllowed is the CLI compatibility gate for an unsigned
// `POST /sn/wallet`: only when st.yml sets `wallet_allow_unsigned`.
func snUnsignedWalletSetAllowed() bool {
	cfg := stConfig()
	return cfg != nil && cfg.WalletAllowUnsigned
}

// snNetworkHasWallet reports whether any coldkey is attached inside the
// network (network-level or any provider client).
func snNetworkHasWallet(ctx context.Context, networkId server.Id) bool {
	if model.GetStWallet(ctx, networkId) != nil {
		return true
	}
	return 0 < len(model.GetStProviderWalletsForNetwork(ctx, networkId))
}

// -----------------------------------------------------------------------------
// Chain state helpers shared by the epoch and head reads
// -----------------------------------------------------------------------------

const snChainStateTtl = 60 * time.Second
const snChainCallTimeout = 8 * time.Second

var snChainStateCache = struct {
	lock   sync.Mutex
	state  *StEpochState
	expiry time.Time
}{}

// snChainState returns the st config, client and a recent epoch state. Any of
// them is nil when the subsystem is not configured or the chain is
// unreachable; callers degrade to estimates instead of failing.
func snChainState(ctx context.Context) (*StConfig, StClient, *StEpochState) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, nil, nil
	}
	now := server.NowUtc()
	snChainStateCache.lock.Lock()
	if snChainStateCache.state != nil && now.Before(snChainStateCache.expiry) {
		state := snChainStateCache.state
		snChainStateCache.lock.Unlock()
		return cfg, client, state
	}
	snChainStateCache.lock.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, snChainCallTimeout)
	defer cancel()
	state, err := client.Epoch(callCtx)
	if err != nil {
		return cfg, client, nil
	}
	snChainStateCache.lock.Lock()
	snChainStateCache.state = state
	snChainStateCache.expiry = now.Add(snChainStateTtl)
	snChainStateCache.lock.Unlock()
	return cfg, client, state
}

// snEpochSummaryWithChainSettings overlays the release chain settings that
// direct claims need onto the epoch summary: settlement vault, no_id
// (decimal), netuid and the public rpc url (st.yml `public_rpc_url`). Unset
// values stay empty so the SDK/web defaults win.
func snEpochSummaryWithChainSettings(cfg *StConfig, summary *model.StEpochSummary) *model.StEpochSummary {
	if cfg == nil || summary == nil {
		return summary
	}
	if cfg.SettlementVault != (common.Address{}) {
		summary.SettlementVaultAddress = cfg.SettlementVault.Hex()
	}
	summary.NoId = strconv.FormatUint(cfg.NoId, 10)
	summary.Netuid = cfg.Netuid
	summary.RpcUrl = cfg.PublicRpcUrl
	return summary
}

// snCurrentEpoch is the contract epoch as mirrored by the sync task, else the
// newest mirrored epoch row, else 0.
func snCurrentEpoch(ctx context.Context) uint64 {
	if summary := model.GetStEpochSummaryCache(ctx); summary != nil {
		return summary.Epoch
	}
	if latest := model.GetLatestStEpoch(ctx); latest != nil {
		return latest.Epoch
	}
	return 0
}

var snEpochWindowCache = struct {
	lock    sync.Mutex
	windows map[uint64][2]time.Time
}{windows: map[uint64][2]time.Time{}}

// snEpochWindow resolves an epoch's wall-clock [start, end) window: from block
// times when the chain is reachable (cached — a closed epoch's window never
// changes), else estimated from the mirrored row's finalized time and the
// block period.
func snEpochWindow(ctx context.Context, row *model.StEpoch) (time.Time, time.Time) {
	snEpochWindowCache.lock.Lock()
	window, ok := snEpochWindowCache.windows[row.Epoch]
	snEpochWindowCache.lock.Unlock()
	if ok {
		return window[0], window[1]
	}
	cfg, client, state := snChainState(ctx)
	if client != nil && state != nil {
		callCtx, cancel := context.WithTimeout(ctx, snChainCallTimeout)
		start, end, _, _, err := stEpochWindow(callCtx, client, state, row.Epoch, cfg.BlockSeconds)
		cancel()
		if err == nil {
			snEpochWindowCache.lock.Lock()
			snEpochWindowCache.windows[row.Epoch] = [2]time.Time{start, end}
			snEpochWindowCache.lock.Unlock()
			return start, end
		}
	}
	return snEstimateEpochWindow(ctx, cfg, row)
}

// snEstimateEpochWindow anchors block times on the row's finalized time (the
// finalize tx lands at or just after finalize_block) and the configured block
// period. The close block is start + tEpoch when the epoch clock is mirrored,
// else the commit deadline (an upper bound).
func snEstimateEpochWindow(ctx context.Context, cfg *StConfig, row *model.StEpoch) (time.Time, time.Time) {
	blockSeconds := int64(stDefaultBlockSeconds)
	if cfg != nil && 0 < cfg.BlockSeconds {
		blockSeconds = cfg.BlockSeconds
	}
	anchorTime := server.NowUtc()
	if row.FinalizedTime != nil {
		anchorTime = *row.FinalizedTime
	}
	anchorBlock := row.FinalizeBlock
	closeBlock := row.CommitDeadlineBlock
	if summary := model.GetStEpochSummaryCache(ctx); summary != nil && 0 < summary.TEpochBlocks {
		closeBlock = row.StartBlock + summary.TEpochBlocks
	}
	blockTime := func(block uint64) time.Time {
		if block <= anchorBlock {
			return anchorTime.Add(-time.Duration(anchorBlock-block) * time.Duration(blockSeconds) * time.Second)
		}
		return anchorTime.Add(time.Duration(block-anchorBlock) * time.Duration(blockSeconds) * time.Second)
	}
	return blockTime(row.StartBlock), blockTime(closeBlock)
}

// -----------------------------------------------------------------------------
// Account epochs
// -----------------------------------------------------------------------------

const accountEpochsDefaultLimit = 26
const accountEpochsMaxLimit = 104

// AccountEpochsArgs comes from the `limit` query parameter; 0 means default.
type AccountEpochsArgs struct {
	Limit int
}

// AccountEpoch is one finalized epoch as the app shows it: the points the
// network earned inside the epoch window and its share of the operator pool
// in basis points (0 when no coldkey was attached, i.e. points only).
type AccountEpoch struct {
	Epoch       uint64  `json:"epoch"`
	StartMillis int64   `json:"start_millis"`
	EndMillis   int64   `json:"end_millis"`
	Points      float64 `json:"points"`
	ShareBps    int     `json:"share_bps"`
}

type AccountEpochsError struct {
	Message string `json:"message"`
}

type AccountEpochsResult struct {
	Epochs []AccountEpoch      `json:"epochs"`
	Error  *AccountEpochsError `json:"error,omitempty"`
}

// AccountEpochs lists the caller network's finalized epochs, newest first.
func AccountEpochs(
	args *AccountEpochsArgs,
	clientSession *session.ClientSession,
) (*AccountEpochsResult, error) {
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	limit := args.Limit
	if limit <= 0 {
		limit = accountEpochsDefaultLimit
	}
	if accountEpochsMaxLimit < limit {
		limit = accountEpochsMaxLimit
	}
	rows := model.GetFinalizedStEpochs(ctx, limit)
	epochs := make([]uint64, len(rows))
	for i, row := range rows {
		epochs[i] = row.Epoch
	}
	noId := uint64(0)
	if cfg := stConfig(); cfg != nil {
		noId = cfg.NoId
	}
	shares := model.GetStPayoutShareBpsForNetwork(ctx, networkId, noId, epochs)
	result := &AccountEpochsResult{Epochs: []AccountEpoch{}}
	for _, row := range rows {
		start, end := snEpochWindow(ctx, row)
		nanoPoints := model.GetAccountNanoPointsInWindow(ctx, networkId, start, end)
		result.Epochs = append(result.Epochs, AccountEpoch{
			Epoch:       row.Epoch,
			StartMillis: start.UnixMilli(),
			EndMillis:   end.UnixMilli(),
			Points:      float64(nanoPoints) / snNanoPointsPerPoint,
			ShareBps:    shares[row.Epoch],
		})
	}
	return result, nil
}

// -----------------------------------------------------------------------------
// Head tier (Top 200) estimate
// -----------------------------------------------------------------------------

// SnHeadCutoff is the head-tier size (WHITEPAPER §8.4: the top ~200 fleets).
const SnHeadCutoff = 200

const snHeadRankingTtl = 10 * time.Minute
const snHeadMaxNetworkClients = 256
const snHeadMaxChainBindingReads = 8

type SnHeadError struct {
	Message string `json:"message"`
}

// SnHeadResult is the caller network's standing for a head mining spot.
// `Score` is the server's estimate of split-adjusted distinct routable
// egress-IP breadth from the live trail egress index; `Floor` is the score of
// the last network inside the cutoff (0 while fewer than `Cutoff` fleets
// score); `RankEstimate` is 1-based (0 when the network has no score).
// `Bound`/`Hotkey`/`Uid` reflect an active head binding; `Rank` is the bound
// network's estimated rank. `Source` is "server" until validators publish
// consensus scores on chain.
type SnHeadResult struct {
	Eligible     bool         `json:"eligible"`
	Score        float64      `json:"score"`
	Floor        float64      `json:"floor"`
	RankEstimate int          `json:"rank_estimate"`
	Cutoff       int          `json:"cutoff"`
	Bound        bool         `json:"bound"`
	Hotkey       string       `json:"hotkey,omitempty"`
	Uid          uint64       `json:"uid"`
	Rank         int          `json:"rank"`
	Epoch        uint64       `json:"epoch"`
	Netuid       uint64       `json:"netuid"`
	Source       string       `json:"source"`
	Error        *SnHeadError `json:"error,omitempty"`
}

// snHeadRanking is the fleet-wide score table. Scores split each live egress
// hash evenly between the networks currently backing it, so a prefix shared
// by k fleets is worth 1/k to each (split-adjusted breadth).
type snHeadRanking struct {
	scores      map[server.Id]float64
	sorted      []float64 // descending
	liveClients map[server.Id]map[server.Id]bool
	computed    time.Time
}

func (self *snHeadRanking) rankOf(score float64) int {
	if score <= 0 {
		return 0
	}
	// number of networks strictly ahead
	ahead := sort.Search(len(self.sorted), func(i int) bool {
		return self.sorted[i] <= score
	})
	return ahead + 1
}

func (self *snHeadRanking) floor() float64 {
	if SnHeadCutoff <= len(self.sorted) {
		return self.sorted[SnHeadCutoff-1]
	}
	return 0
}

var snHeadRankingCache = struct {
	lock        sync.Mutex
	computeLock sync.Mutex
	ranking     *snHeadRanking
}{}

func snHeadRankingGet(ctx context.Context) *snHeadRanking {
	fresh := func() *snHeadRanking {
		snHeadRankingCache.lock.Lock()
		defer snHeadRankingCache.lock.Unlock()
		if snHeadRankingCache.ranking != nil && server.NowUtc().Sub(snHeadRankingCache.ranking.computed) < snHeadRankingTtl {
			return snHeadRankingCache.ranking
		}
		return nil
	}
	if ranking := fresh(); ranking != nil {
		return ranking
	}
	// one computation at a time; late arrivals reuse it
	snHeadRankingCache.computeLock.Lock()
	defer snHeadRankingCache.computeLock.Unlock()
	if ranking := fresh(); ranking != nil {
		return ranking
	}
	ranking := snComputeHeadRanking(ctx)
	snHeadRankingCache.lock.Lock()
	snHeadRankingCache.ranking = ranking
	snHeadRankingCache.lock.Unlock()
	return ranking
}

// snHeadRankingSafe is snHeadRankingGet with the index failures (redis, db)
// turned into a nil ranking, so a head read never becomes an error page.
func snHeadRankingSafe(ctx context.Context) (ranking *snHeadRanking) {
	defer func() {
		if r := recover(); r != nil {
			ranking = nil
		}
	}()
	return snHeadRankingGet(ctx)
}

func snComputeHeadRanking(ctx context.Context) *snHeadRanking {
	clientIds := model.GetVerifyEligibleClientIds(ctx)
	hashesByClient := model.GetVerifyLiveEgressHashes(ctx, clientIds)
	networkByClient := model.GetNetworkIdsForClientIds(ctx, clientIds)
	return snScoreHeadRanking(hashesByClient, networkByClient)
}

// snScoreHeadRanking is the pure scoring step: each live egress hash is
// worth 1/(number of networks currently backing it) to every one of them.
func snScoreHeadRanking(hashesByClient map[server.Id][]string, networkByClient map[server.Id]server.Id) *snHeadRanking {
	claimants := map[string]map[server.Id]bool{}
	networkHashes := map[server.Id]map[string]bool{}
	liveClients := map[server.Id]map[server.Id]bool{}
	for clientId, hashes := range hashesByClient {
		networkId, ok := networkByClient[clientId]
		if !ok || len(hashes) == 0 {
			continue
		}
		if liveClients[networkId] == nil {
			liveClients[networkId] = map[server.Id]bool{}
		}
		liveClients[networkId][clientId] = true
		for _, egressHashHex := range hashes {
			if claimants[egressHashHex] == nil {
				claimants[egressHashHex] = map[server.Id]bool{}
			}
			claimants[egressHashHex][networkId] = true
			if networkHashes[networkId] == nil {
				networkHashes[networkId] = map[string]bool{}
			}
			networkHashes[networkId][egressHashHex] = true
		}
	}
	scores := map[server.Id]float64{}
	sorted := make([]float64, 0, len(networkHashes))
	for networkId, hashes := range networkHashes {
		score := 0.0
		for egressHashHex := range hashes {
			score += 1.0 / float64(len(claimants[egressHashHex]))
		}
		scores[networkId] = score
		sorted = append(sorted, score)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))
	return &snHeadRanking{
		scores:      scores,
		sorted:      sorted,
		liveClients: liveClients,
		computed:    server.NowUtc(),
	}
}

type snHeadBound struct {
	bound  bool
	hotkey [32]byte
	uid    uint64
}

// snHeadBoundState looks for an active head binding on any of the network's
// clients: first the mirrored `bindHead` registry (one query), then — when
// allowed — the coordinator's fleet bindings for a few clients, live-egress
// clients first.
func snHeadBoundState(ctx context.Context, networkId server.Id, allowChain bool, prefer map[server.Id]bool) snHeadBound {
	clientIds := model.GetActiveNetworkClientIds(ctx, networkId, snHeadMaxNetworkClients)
	if len(clientIds) == 0 {
		return snHeadBound{}
	}
	ckeys := model.GetStContributingClientCkeys(ctx, clientIds)
	ckeyList := make([][32]byte, 0, len(ckeys))
	for _, ckey := range ckeys {
		ckeyList = append(ckeyList, ckey)
	}
	for _, binding := range model.GetActiveStHeadBindingsForCkeys(ctx, ckeyList) {
		return snHeadBound{bound: true, hotkey: binding.Hotkey, uid: binding.Uid}
	}
	if !allowChain {
		return snHeadBound{}
	}
	_, client, state := snChainState(ctx)
	if client == nil || state == nil {
		return snHeadBound{}
	}
	ordered := make([]server.Id, 0, len(clientIds))
	for _, clientId := range clientIds {
		if prefer[clientId] {
			ordered = append(ordered, clientId)
		}
	}
	for _, clientId := range clientIds {
		if !prefer[clientId] {
			ordered = append(ordered, clientId)
		}
	}
	if snHeadMaxChainBindingReads < len(ordered) {
		ordered = ordered[:snHeadMaxChainBindingReads]
	}
	callCtx, cancel := context.WithTimeout(ctx, snChainCallTimeout)
	defer cancel()
	for _, clientId := range ordered {
		binding, err := client.BindingAt(callCtx, [16]byte(clientId), state.Epoch)
		if err != nil {
			break
		}
		if binding != nil && binding.Active {
			return snHeadBound{bound: true, hotkey: binding.Hotkey, uid: uint64(binding.Uid)}
		}
	}
	return snHeadBound{}
}

func snHotkeySs58(hotkey [32]byte) string {
	address, err := ss58.Encode(hotkey, ss58.BittensorPrefix)
	if err != nil {
		return ""
	}
	return address
}

// SnHead reports the caller network's Top 200 standing.
func SnHead(clientSession *session.ClientSession) (*SnHeadResult, error) {
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	result := &SnHeadResult{
		Cutoff: SnHeadCutoff,
		Epoch:  snCurrentEpoch(ctx),
		Source: "server",
	}
	if cfg := stConfig(); cfg != nil {
		result.Netuid = cfg.Netuid
	}
	ranking := snHeadRankingSafe(ctx)
	var prefer map[server.Id]bool
	if ranking == nil {
		result.Error = &SnHeadError{Message: "Head estimate unavailable right now."}
	} else {
		result.Score = ranking.scores[networkId]
		result.Floor = ranking.floor()
		result.RankEstimate = ranking.rankOf(result.Score)
		result.Eligible = 0 < result.Score && result.RankEstimate <= SnHeadCutoff
		prefer = ranking.liveClients[networkId]
	}
	bound := snHeadBoundState(ctx, networkId, true, prefer)
	if bound.bound {
		result.Bound = true
		result.Hotkey = snHotkeySs58(bound.hotkey)
		result.Uid = bound.uid
		result.Rank = result.RankEstimate
	}
	return result, nil
}

// -----------------------------------------------------------------------------
// Fleet binding consent
// -----------------------------------------------------------------------------

// SnFleetBinding is the JSON form of protocol.FleetBinding (WHITEPAPER
// §11.4): 32-byte values as 0x-hex, the hotkey as ss58 or 0x-hex, the
// client id as its uuid.
type SnFleetBinding struct {
	ChainId        uint64    `json:"chain_id"`
	Netuid         uint16    `json:"netuid"`
	Coordinator    string    `json:"coordinator"`
	FleetId        string    `json:"fleet_id"`
	Hotkey         string    `json:"hotkey"`
	ClientId       server.Id `json:"client_id"`
	ClientKey      string    `json:"client_key"`
	Generation     uint64    `json:"generation"`
	ValidFromEpoch uint64    `json:"valid_from_epoch"`
	ValidToEpoch   uint64    `json:"valid_to_epoch"`
	CommitmentHash string    `json:"commitment_hash"`
}

type SnHeadBindingArgs struct {
	Binding SnFleetBinding `json:"binding"`
	// ClientSignature is the device's Ed25519 signature (hex) over the
	// binding digest, produced by the SDK with the client key.
	ClientSignature string `json:"client_signature"`
	// Signature is an accepted alias of ClientSignature.
	Signature string `json:"signature,omitempty"`
	// HotkeySignature is the head hotkey's sr25519 signature (hex) over the
	// same digest; optional, needed for the calldata.
	HotkeySignature string `json:"hotkey_signature,omitempty"`
	// Hotkey optionally overrides the binding's hotkey (ss58 or 0x-hex), for
	// callers that paste the device payload and pick the hotkey separately.
	Hotkey string `json:"hotkey,omitempty"`
}

type SnHeadBindingError struct {
	Message string `json:"message"`
}

// SnHeadBindingResult echoes the digest and which signatures verified. The
// calldata for `bindFleetMember` is present only when both signatures verify;
// the operator submits it from their own EVM key.
type SnHeadBindingResult struct {
	Digest               string `json:"digest"`
	ClientSignatureValid bool   `json:"client_signature_valid"`
	HotkeySignatureValid bool   `json:"hotkey_signature_valid"`
	// Ready mirrors "calldata is present": both signatures verified.
	Ready    bool   `json:"ready"`
	Calldata string `json:"calldata,omitempty"`
	// To/Data duplicate ContractAddress/Calldata as a ready-to-send tx.
	To              string              `json:"to"`
	Data            string              `json:"data,omitempty"`
	ContractAddress string              `json:"contract_address"`
	ChainId         uint64              `json:"chain_id"`
	Error           *SnHeadBindingError `json:"error,omitempty"`
}

func snParseHexBytes(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
	if value == "" {
		return nil, fmt.Errorf("empty")
	}
	return hex.DecodeString(value)
}

func snParseHex32(value string) ([32]byte, error) {
	var out [32]byte
	b, err := snParseHexBytes(value)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// snParseHotkey accepts an ss58 address (any prefix) or 0x-hex public key.
func snParseHotkey(value string) ([32]byte, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X") {
		return snParseHex32(value)
	}
	pubkey, _, err := ss58.Decode(value)
	if err != nil {
		if hexKey, hexErr := snParseHex32(value); hexErr == nil {
			return hexKey, nil
		}
		return [32]byte{}, err
	}
	return pubkey, nil
}

// SnFleetBindingFromJson converts the wire form to the protocol struct.
func SnFleetBindingFromJson(b *SnFleetBinding) (protocol.FleetBinding, error) {
	var binding protocol.FleetBinding
	coordinator, err := snParseHexBytes(b.Coordinator)
	if err != nil || len(coordinator) != 20 {
		return binding, fmt.Errorf("coordinator must be a 20-byte 0x address")
	}
	fleetId, err := snParseHex32(b.FleetId)
	if err != nil {
		return binding, fmt.Errorf("fleet_id: %s", err)
	}
	hotkey, err := snParseHotkey(b.Hotkey)
	if err != nil {
		return binding, fmt.Errorf("hotkey: %s", err)
	}
	clientKey, err := snParseHex32(b.ClientKey)
	if err != nil {
		return binding, fmt.Errorf("client_key: %s", err)
	}
	commitmentHash, err := snParseHex32(b.CommitmentHash)
	if err != nil {
		return binding, fmt.Errorf("commitment_hash: %s", err)
	}
	binding = protocol.FleetBinding{
		ChainID:        b.ChainId,
		Netuid:         b.Netuid,
		FleetID:        fleetId,
		Hotkey:         hotkey,
		ClientID:       [16]byte(b.ClientId),
		ClientKey:      clientKey,
		Generation:     b.Generation,
		ValidFromEpoch: b.ValidFromEpoch,
		ValidToEpoch:   b.ValidToEpoch,
		CommitmentHash: commitmentHash,
	}
	copy(binding.Coordinator[:], coordinator)
	return binding, binding.Validate()
}

// SnHeadBinding verifies and stores a device's consent for a fleet binding
// and returns the calldata to submit. The server submits nothing on chain.
func SnHeadBinding(
	args *SnHeadBindingArgs,
	clientSession *session.ClientSession,
) (*SnHeadBindingResult, error) {
	ctx := clientSession.Ctx
	fail := func(message string) (*SnHeadBindingResult, error) {
		return &SnHeadBindingResult{Error: &SnHeadBindingError{Message: message}}, nil
	}
	if strings.TrimSpace(args.Hotkey) != "" {
		args.Binding.Hotkey = args.Hotkey
	}
	if strings.TrimSpace(args.ClientSignature) == "" {
		args.ClientSignature = args.Signature
	}
	binding, err := SnFleetBindingFromJson(&args.Binding)
	if err != nil {
		return fail(fmt.Sprintf("Invalid binding: %s", err))
	}
	clientId := args.Binding.ClientId
	networkId, findErr := model.FindClientNetwork(ctx, clientId)
	if findErr != nil || networkId != clientSession.ByJwt.NetworkId {
		return fail("Client does not belong to this network.")
	}
	if clientSession.ByJwt.ClientId != nil && *clientSession.ByJwt.ClientId != clientId {
		return fail("A client session can only bind its own client id.")
	}
	// the binding's client key must be the key this client actually holds
	if ckeys := model.GetStContributingClientCkeys(ctx, []server.Id{clientId}); len(ckeys) == 1 {
		if ckey, ok := ckeys[clientId]; ok && ckey != binding.ClientKey {
			return fail("client_key does not match this client's key.")
		}
	}
	clientSignature, err := snParseHexBytes(args.ClientSignature)
	if err != nil {
		return fail("client_signature must be hex.")
	}
	digest, err := binding.Digest()
	if err != nil {
		return fail(fmt.Sprintf("Invalid binding: %s", err))
	}
	result := &SnHeadBindingResult{
		Digest:               "0x" + hex.EncodeToString(digest[:]),
		ClientSignatureValid: binding.VerifyClient(clientSignature),
		ContractAddress:      common.Address(binding.Coordinator).Hex(),
		ChainId:              binding.ChainID,
	}
	if cfg := stConfig(); cfg != nil && cfg.ContractAddress != (common.Address{}) {
		result.ContractAddress = cfg.ContractAddress.Hex()
		result.ChainId = cfg.ChainId
	}
	result.To = result.ContractAddress
	if !result.ClientSignatureValid {
		result.Error = &SnHeadBindingError{Message: "client_signature does not verify for this binding."}
		return result, nil
	}
	var hotkeySignature []byte
	if strings.TrimSpace(args.HotkeySignature) != "" {
		hotkeySignature, err = snParseHexBytes(args.HotkeySignature)
		if err != nil {
			return fail("hotkey_signature must be hex.")
		}
		result.HotkeySignatureValid = binding.VerifyHotkey(hotkeySignature)
		if !result.HotkeySignatureValid {
			hotkeySignature = nil
		}
	}
	bindingJson, err := json.Marshal(&args.Binding)
	if err != nil {
		return nil, err
	}
	model.SetStFleetBindingSignature(ctx, &model.StFleetBindingSignature{
		ClientId:        clientId,
		NetworkId:       networkId,
		Generation:      binding.Generation,
		Hotkey:          binding.Hotkey,
		Digest:          digest,
		BindingJson:     string(bindingJson),
		ClientSignature: clientSignature,
		HotkeySignature: hotkeySignature,
		CreateTime:      server.NowUtc(),
	})
	if result.HotkeySignatureValid {
		calldata, err := onchain.BuildFleetBindingCalldata(binding, clientSignature, hotkeySignature)
		if err != nil {
			result.Error = &SnHeadBindingError{Message: fmt.Sprintf("Could not pack calldata: %s", err)}
			return result, nil
		}
		result.Calldata = "0x" + hex.EncodeToString(calldata)
		result.Data = result.Calldata
		result.Ready = true
	}
	return result, nil
}
