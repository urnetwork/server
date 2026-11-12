package controller

// sn_claim_relay_api.go — the claim relay's answer for the apps: per coldkey of
// the caller's network, what is pending, why, whether Claim now can be tapped,
// and the transaction of the relay's claim while it is the account's latest.
// Apps draw this through the sdk (sdk sn_claim_relay.go), which also chooses
// between the relay and the device's own claim.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Why a balance is still pending, for each account on the api. The sdk passes them
// on, and the apps turn each into a sentence.
const (
	SnClaimRelayReasonNothingPending  = "nothing_pending"
	SnClaimRelayReasonUnderThreshold  = "under_threshold"
	SnClaimRelayReasonQueued          = "queued"
	SnClaimRelayReasonCapReached      = "cap_reached"
	SnClaimRelayReasonRefused         = "refused"
	SnClaimRelayReasonRelayNotRunning = "relay_not_running"
	SnClaimRelayReasonRelayPaused     = "relay_paused"
)

// Why Claim now cannot be tapped, beside the reasons above.
const (
	SnClaimRelayClaimNowUnderFloor = "under_floor"
	SnClaimRelayClaimNowRequested  = "requested"
	SnClaimRelayClaimNowLimit      = "limit"
)

// How the apps should read a relay state.
const (
	snClaimRelayClassMoving = iota
	snClaimRelayClassPaused
	snClaimRelayClassNotRunning
)

func snClaimRelayClassOf(state string) int {
	switch state {
	case SnClaimRelayStateRunning, SnClaimRelayStateCapReached, SnClaimRelayStateInFlight:
		return snClaimRelayClassMoving
	case SnClaimRelayStateChainUnavailable, SnClaimRelayStateLedgerUnavailable, SnClaimRelayStateSendFailed,
		SnClaimRelayStateLowBalance, SnClaimRelayStateGasPriceHigh, SnClaimRelayStateGriefingStop:
		return snClaimRelayClassPaused
	default:
		return snClaimRelayClassNotRunning
	}
}

// snClaimRelayStateFor is the relay's state as GET /sn/claim-relay answers it,
// with partial=true or without. A pass that claimed the pools it could read
// says ledger_unavailable for those it could not. Without partial=true, as
// every app that predates the capture the flag pool asks, a pass whose only
// such pools are capture the flag pools reads running, as it would have before
// that pool existed (sn CAPTURETHEFLAG.md §12.1, item 22): those pools are left
// out of the answer, and the operator's pool's rows read as before.
func snClaimRelayStateFor(status *SnClaimRelayStatus, partial bool) string {
	if partial || status.State != SnClaimRelayStateLedgerUnavailable || !status.OthersClaimed || len(status.UnavailablePools) == 0 {
		return status.State
	}
	for _, noId := range status.UnavailablePools {
		if kind, _ := StLedgerV2PoolKind(noId); kind != StLedgerV2PoolKindCtf {
			return status.State
		}
	}
	return SnClaimRelayStateRunning
}

// SnClaimRelayArgs are GET /sn/claim-relay's query: Partial (partial=true) asks
// for the pools that can be read, naming the others in UnavailablePools.
// Without it, as old apps ask, a capture the flag pool that cannot be read is
// left out, unnamed, with the relay's state as before that pool
// (snClaimRelayStateFor), and any other pool that cannot be read is an Error
// for all of them.
type SnClaimRelayArgs struct {
	Partial bool
}

// SnClaimRelayResult is GET /sn/claim-relay. An Error means the balances could not
// be read: they are unavailable, never zero.
type SnClaimRelayResult struct {
	// the relay's state, SnClaimRelayState*: not_running when it has no recent pass
	State string `json:"state"`
	// the automatic threshold and the transfer floor, in rao of TAO
	AutomaticThresholdTaoRao int64 `json:"automatic_threshold_tao_rao"`
	TransferFloorTaoRao      int64 `json:"transfer_floor_tao_rao"`
	// the same in rao of α at the last price, 0 without one
	AutomaticThresholdRao int64 `json:"automatic_threshold_rao"`
	TransferFloorRao      int64 `json:"transfer_floor_rao"`
	// days between two Claim now of one coldkey
	ClaimNowPeriodDays int                    `json:"claim_now_period_days"`
	Accounts           []*SnClaimRelayAccount `json:"accounts"`
	// With partial=true, the pools whose ledger could not be read, by no_id:
	// their balances are unavailable, never zero. [] otherwise.
	UnavailablePools []*StLedgerV2UnavailablePool `json:"unavailable_pools"`
	Error            *SnClaimRelayError           `json:"error,omitempty"`
}

type SnClaimRelayError struct {
	Message string `json:"message"`
}

// SnClaimRelayAccount is one coldkey's account in one of the operator's pools:
// its pool or its capture the flag pool.
type SnClaimRelayAccount struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
	NoId        int64  `json:"no_id"`
	// the pool's kind, StLedgerV2PoolKind*, and English display label
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Index int64  `json:"index"`
	// what a claim would pay now, in rao of α
	ClaimableRao int64 `json:"claimable_rao"`
	// its TAO value at the last price, in rao of TAO
	ValueTaoRao int64 `json:"value_tao_rao"`
	// a committed root that is not final yet holds this much more, in rao of α
	PendingRao int64 `json:"pending_rao"`
	// SnClaimRelayReason*
	Reason            string `json:"reason"`
	ClaimNowAvailable bool   `json:"claim_now_available"`
	// why not, when not: SnClaimRelayClaimNow* or a SnClaimRelayReason*
	ClaimNowReason string `json:"claim_now_reason,omitempty"`
	// when the coldkey's limit ends, unix milliseconds; 0 when none holds
	ClaimNowNextAtMillis int64 `json:"claim_now_next_at_millis,omitempty"`
	// the transaction of the account's latest claim, when the relay sent it and
	// still has it on record; "" for a claim anyone else sent, or none
	ClaimTxHash string `json:"claim_tx_hash,omitempty"`
	// what that claim paid, in rao of α
	ClaimAmountRao int64 `json:"claim_amount_rao,omitempty"`
}

// The status the api goes by: the last pass's, or not_running when there is no
// recent one.
func snClaimRelayCurrentStatus(ctx context.Context, cfg *StConfig, now time.Time) *SnClaimRelayStatus {
	if cfg == nil || !cfg.Enabled || !cfg.LedgerV2Enabled() {
		return &SnClaimRelayStatus{State: SnClaimRelayStateNotRunning}
	}
	status := snClaimRelayReadStatus(ctx, cfg.DeploymentKey())
	if status == nil || snClaimRelayStatusStale < now.Sub(status.PassTime) {
		return &SnClaimRelayStatus{State: SnClaimRelayStateNotRunning}
	}
	return status
}

func snClaimRelayReadStatus(ctx context.Context, deploymentKey model.StDeploymentKey) *SnClaimRelayStatus {
	raw := model.GetSnClaimRelayStatus(ctx, deploymentKey)
	if raw == nil {
		return nil
	}
	status := &SnClaimRelayStatus{}
	if err := json.Unmarshal(raw, status); err != nil {
		glog.Errorf("[sn]claim relay status does not decode: %s\n", err)
		return nil
	}
	return status
}

// A transaction hash as the relay records it.
var snClaimRelayTxHashPattern = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

// The rao of an amount, saturated rather than wrapped.
func snClaimRelayRao(amount *uint256.Int) int64 {
	if !amount.IsUint64() || math.MaxInt64 < amount.Uint64() {
		return math.MaxInt64
	}
	return int64(amount.Uint64())
}

// One entry's balance as the relay's status judges it, and claim the relay's
// record of the account's latest paid claim, nil without one. withDue says
// another account of the coldkey is due, so the relay claims this one with it
// when it batches. Pure, for the tests. The mirror behind the account is the
// chain's own read here, so a Reason never says more than what is true at this
// read.
func snClaimRelayAccountView(status *SnClaimRelayStatus, coldkeySs58 string, entry *StLedgerV2AccountEntry, requested bool, withDue bool, limitUntil time.Time, claim *model.SnClaimRelayClaim, now time.Time) (*SnClaimRelayAccount, error) {
	var cumulative, claimed uint256.Int
	if err := cumulative.SetFromDecimal(entry.Cumulative); err != nil {
		return nil, err
	}
	if err := claimed.SetFromDecimal(entry.Claimed); err != nil {
		return nil, err
	}
	claimable := new(uint256.Int)
	if claimed.Lt(&cumulative) {
		claimable.Sub(&cumulative, &claimed)
	}
	var price *big.Int
	if parsed, ok := new(big.Int).SetString(status.AlphaPriceWei, 10); ok {
		price = parsed
	}
	value := snClaimRelayValueTaoRao(claimable, price)
	kind, label := StLedgerV2PoolKind(entry.NoId)
	account := &SnClaimRelayAccount{
		ColdkeySs58:  coldkeySs58,
		NoId:         int64(entry.NoId),
		Kind:         kind,
		Label:        label,
		Index:        int64(entry.Index),
		ClaimableRao: int64(claimable.Uint64()),
		ValueTaoRao:  value.Int64(),
	}
	if entry.PendingCumulative != nil {
		var pending uint256.Int
		if err := pending.SetFromDecimal(*entry.PendingCumulative); err != nil {
			return nil, err
		}
		if cumulative.Lt(&pending) {
			account.PendingRao = int64(new(uint256.Int).Sub(&pending, &cumulative).Uint64())
		}
	}
	// The record names its claim's transaction while that claim is the account's
	// latest: vault2's claimed, read at the finalized head, is still the cumulative
	// it claimed up to. A later claim, by the relay or anyone, moves claimed on.
	if claim != nil && snClaimRelayTxHashPattern.MatchString(claim.TxHash) {
		var claimedUpTo, amount uint256.Int
		if claimedUpTo.SetFromDecimal(claim.Cumulative) == nil && amount.SetFromDecimal(claim.AmountRao) == nil && !claimed.IsZero() && claimedUpTo.Eq(&claimed) {
			account.ClaimTxHash = claim.TxHash
			account.ClaimAmountRao = snClaimRelayRao(&amount)
		}
	}
	refusal := status.Refusals[snClaimRelayAccountKey(entry.NoId, entry.Index)]
	refused := refusal != nil && now.Before(refusal.Until)
	class := snClaimRelayClassOf(status.State)
	if 0 < len(status.UnavailablePools) {
		switch {
		case slices.Contains(status.UnavailablePools, entry.NoId):
			// a pool the relay could not read this pass waits, as a paused
			// relay does
			if class == snClaimRelayClassMoving || status.State == SnClaimRelayStateLedgerUnavailable {
				class = snClaimRelayClassPaused
			}
		case status.State == SnClaimRelayStateLedgerUnavailable:
			// the pass said the ledger is unavailable for another pool's
			// sake: this pool's accounts are claimed
			class = snClaimRelayClassMoving
		}
	}
	// claimed with a due account of its coldkey in the same batch
	companion := withDue && status.Batching && snClaimRelayClearsFloor(value)
	switch {
	case claimable.IsZero():
		account.Reason = SnClaimRelayReasonNothingPending
	case class == snClaimRelayClassNotRunning:
		account.Reason = SnClaimRelayReasonRelayNotRunning
	case class == snClaimRelayClassPaused:
		account.Reason = SnClaimRelayReasonRelayPaused
	case refused:
		account.Reason = SnClaimRelayReasonRefused
	case !requested && !companion && !snClaimRelayOverThreshold(value):
		account.Reason = SnClaimRelayReasonUnderThreshold
	case status.State == SnClaimRelayStateCapReached:
		account.Reason = SnClaimRelayReasonCapReached
	default:
		account.Reason = SnClaimRelayReasonQueued
	}
	if now.Before(limitUntil) {
		account.ClaimNowNextAtMillis = limitUntil.UnixMilli()
	}
	switch {
	case claimable.IsZero():
		account.ClaimNowReason = SnClaimRelayReasonNothingPending
	case requested:
		account.ClaimNowReason = SnClaimRelayClaimNowRequested
	case now.Before(limitUntil):
		account.ClaimNowReason = SnClaimRelayClaimNowLimit
	case !snClaimRelayClearsFloor(value):
		// a relay that is down has no price: it says so below rather than this
		if class == snClaimRelayClassNotRunning {
			account.ClaimNowReason = SnClaimRelayReasonRelayNotRunning
		} else {
			account.ClaimNowReason = SnClaimRelayClaimNowUnderFloor
		}
	case class == snClaimRelayClassNotRunning:
		account.ClaimNowReason = SnClaimRelayReasonRelayNotRunning
	case class == snClaimRelayClassPaused:
		account.ClaimNowReason = SnClaimRelayReasonRelayPaused
	case refused:
		account.ClaimNowReason = SnClaimRelayReasonRefused
	default:
		account.ClaimNowAvailable = true
	}
	return account, nil
}

// Each answer reads at most this many coldkeys, and at most this many of a
// network's consent generations: each is a chain read. The relay claims every
// account whatever an answer lists.
const (
	snClaimRelayColdkeysPerAnswer = 32
	snClaimRelayConsentsPerAnswer = 8
)

// The coldkeys a network's sessions may ask about, in order: the session's
// effective wallet, the network consent's earlier generations, whose accounts
// keep their balances after a wallet change, and every other wallet SnGetWallet
// lists. A banned coldkey is left out: the relay claims nothing for it.
func snClaimRelayNetworkColdkeys(clientSession *session.ClientSession) ([][32]byte, error) {
	wallets, err := SnGetWallet(clientSession)
	if err != nil {
		return nil, err
	}
	coldkeys := [][32]byte{}
	seen := map[[32]byte]bool{}
	add := func(coldkey [32]byte) {
		if !seen[coldkey] && !SnWalletBanned(coldkey) && len(coldkeys) < snClaimRelayColdkeysPerAnswer {
			seen[coldkey] = true
			coldkeys = append(coldkeys, coldkey)
		}
	}
	addSs58 := func(address string) {
		if coldkey, err := ss58.DecodeWithPrefix(address, ss58.BittensorPrefix); err == nil {
			add(coldkey)
		}
	}
	if wallets.Wallet != nil {
		addSs58(wallets.Wallet.ColdkeySs58)
	}
	if domain, ok := stClientKeyHistoryDomain(); ok {
		earlier, err := model.GetNetworkWalletMappingConsentColdkeys(clientSession.Ctx, domain, clientSession.ByJwt.NetworkId, snClaimRelayConsentsPerAnswer)
		if err != nil {
			return nil, err
		}
		for _, coldkey := range earlier {
			add(coldkey)
		}
	}
	for _, wallet := range wallets.Wallets {
		addSs58(wallet.ColdkeySs58)
	}
	return coldkeys, nil
}

// snClaimRelayColdkeyAccounts reads the relay's view of one coldkey's accounts in
// the operator's pools, its pool and, while capture the flag is on
// (stCtfPoolOn), its capture the flag pool, by no_id: none, one or two. partial
// reads each pool on its own and names those that cannot be read; otherwise a
// capture the flag pool that cannot be read is left out, unnamed, and any other
// pool that cannot be read is an error (StLedgerV2Accounts).
func snClaimRelayColdkeyAccounts(ctx context.Context, cfg *StConfig, status *SnClaimRelayStatus, coldkey [32]byte, partial bool, now time.Time) ([]*SnClaimRelayAccount, []*StLedgerV2UnavailablePool, error) {
	var ledger *StLedgerV2AccountsResult
	var err error
	if partial {
		ledger, err = StLedgerV2AccountsPartial(ctx, coldkey)
	} else {
		ledger, err = StLedgerV2Accounts(ctx, coldkey)
	}
	if err != nil {
		return nil, nil, err
	}
	address, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		return nil, nil, err
	}
	ownPools := stLedgerV2OwnPools(cfg, stCtfPoolOn(cfg))
	unavailable := []*StLedgerV2UnavailablePool{}
	for _, pool := range ledger.UnavailablePools {
		if slices.Contains(ownPools, pool.NoId) {
			unavailable = append(unavailable, pool)
		}
	}
	var price *big.Int
	if parsed, ok := new(big.Int).SetString(status.AlphaPriceWei, 10); ok {
		price = parsed
	}
	// the relay claims in the operator's own pools
	entries := []*StLedgerV2AccountEntry{}
	due := map[uint64]bool{}
	for _, entry := range ledger.Accounts {
		if !slices.Contains(ownPools, entry.NoId) {
			continue
		}
		entries = append(entries, entry)
		var cumulative, claimed uint256.Int
		if cumulative.SetFromDecimal(entry.Cumulative) == nil && claimed.SetFromDecimal(entry.Claimed) == nil && claimed.Lt(&cumulative) {
			due[entry.NoId] = snClaimRelayOverThreshold(snClaimRelayValueTaoRao(new(uint256.Int).Sub(&cumulative, &claimed), price))
		}
	}
	requested, limitUntil := model.GetSnClaimRelayRequest(ctx, cfg.DeploymentKey(), coldkey, now)
	accounts := []*SnClaimRelayAccount{}
	for _, entry := range entries {
		withDue := false
		for noId, isDue := range due {
			withDue = withDue || (isDue && noId != entry.NoId)
		}
		claim := model.GetSnClaimRelayClaim(ctx, cfg.DeploymentKey(), entry.NoId, entry.Index)
		account, err := snClaimRelayAccountView(status, address, entry, requested, withDue, limitUntil, claim, now)
		if err != nil {
			return nil, nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, unavailable, nil
}

// snClaimRelayNetworkAccounts reads the relay's view of each of the network's
// accounts in the operator's pools, and with partial the pools that could not
// be read, once each.
func snClaimRelayNetworkAccounts(clientSession *session.ClientSession, cfg *StConfig, status *SnClaimRelayStatus, partial bool, now time.Time) ([]*SnClaimRelayAccount, []*StLedgerV2UnavailablePool, error) {
	coldkeys, err := snClaimRelayNetworkColdkeys(clientSession)
	if err != nil {
		return nil, nil, err
	}
	accounts := []*SnClaimRelayAccount{}
	unavailable := []*StLedgerV2UnavailablePool{}
	if cfg == nil || !cfg.Enabled || !cfg.LedgerV2Enabled() {
		return accounts, unavailable, nil
	}
	for _, coldkey := range coldkeys {
		coldkeyAccounts, coldkeyUnavailable, err := snClaimRelayColdkeyAccounts(clientSession.Ctx, cfg, status, coldkey, partial, now)
		if err != nil {
			return nil, nil, err
		}
		accounts = append(accounts, coldkeyAccounts...)
		for _, pool := range coldkeyUnavailable {
			if !slices.ContainsFunc(unavailable, func(listed *StLedgerV2UnavailablePool) bool { return listed.NoId == pool.NoId }) {
				unavailable = append(unavailable, pool)
			}
		}
	}
	slices.SortFunc(unavailable, func(a, b *StLedgerV2UnavailablePool) int { return cmp.Compare(a.NoId, b.NoId) })
	return accounts, unavailable, nil
}

// SnClaimRelay answers GET /sn/claim-relay for the caller's network as an app
// that predates partial=true asks: a capture the flag pool that cannot be read
// is left out, and any other pool that cannot be read is an Error.
func SnClaimRelay(clientSession *session.ClientSession) (*SnClaimRelayResult, error) {
	return SnClaimRelayWithArgs(&SnClaimRelayArgs{}, clientSession)
}

// SnClaimRelayWithArgs answers GET /sn/claim-relay[?partial=true] for the caller's
// network.
func SnClaimRelayWithArgs(args *SnClaimRelayArgs, clientSession *session.ClientSession) (*SnClaimRelayResult, error) {
	now := server.NowUtc()
	cfg := stConfig()
	status := snClaimRelayCurrentStatus(clientSession.Ctx, cfg, now)
	var price *big.Int
	if parsed, ok := new(big.Int).SetString(status.AlphaPriceWei, 10); ok {
		price = parsed
	}
	result := &SnClaimRelayResult{
		State:                    snClaimRelayStateFor(status, args.Partial),
		AutomaticThresholdTaoRao: int64(snClaimRelayAutomaticTaoRao),
		TransferFloorTaoRao:      int64(snClaimRelayFloorTaoRao),
		AutomaticThresholdRao:    snClaimRelayAlphaOf(snClaimRelayAutomaticTaoRao, price).Int64(),
		TransferFloorRao:         snClaimRelayAlphaOf(snClaimRelayFloorTaoRao, price).Int64(),
		ClaimNowPeriodDays:       int(snClaimRelayClaimNowPeriod / (24 * time.Hour)),
		Accounts:                 []*SnClaimRelayAccount{},
		UnavailablePools:         []*StLedgerV2UnavailablePool{},
	}
	accounts, unavailable, err := snClaimRelayNetworkAccounts(clientSession, cfg, status, args.Partial, now)
	if err != nil {
		// Whatever the balances are, they are not zero: the relay's state still
		// reaches the app, and the app says they could not be read.
		glog.Infof("[sn]claim relay accounts of network %s: %s\n", clientSession.ByJwt.NetworkId, err)
		result.Error = &SnClaimRelayError{Message: "The balances could not be read."}
		return result, nil
	}
	result.Accounts = accounts
	result.UnavailablePools = unavailable
	return result, nil
}

// SnClaimRelayClaimNowArgs names the coldkey to claim for. It must be one of the
// caller's network's.
type SnClaimRelayClaimNowArgs struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
}

// SnClaimRelayClaimNowResult is POST /sn/claim-relay/claim-now. Not Requested with
// a Reason is the answer for a coldkey that cannot be claimed for now.
type SnClaimRelayClaimNowResult struct {
	Requested bool `json:"requested"`
	// why not, as SnClaimRelayAccount.ClaimNowReason
	Reason string `json:"reason,omitempty"`
	// when the coldkey's limit ends, unix milliseconds, for a tap the limit refused
	NextAtMillis int64              `json:"next_at_millis,omitempty"`
	Error        *SnClaimRelayError `json:"error,omitempty"`
}

// SnClaimRelayClaimNow queues one coldkey's balance for the relay's next pass,
// once it clears the transfer floor. The relay sends it first, under the same
// daily cap. A coldkey can do this once per 30 days, counted from when the relay
// pays a tapped claim it would not have sent by itself, so a tap that sends
// nothing uses nothing up.
func SnClaimRelayClaimNow(args *SnClaimRelayClaimNowArgs, clientSession *session.ClientSession) (*SnClaimRelayClaimNowResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	refuse := func(reason string, nextAt int64) (*SnClaimRelayClaimNowResult, error) {
		snClaimRelayClaimNowRequests.WithLabelValues(snClaimRelayRequestRefused).Inc()
		return &SnClaimRelayClaimNowResult{Reason: reason, NextAtMillis: nextAt}, nil
	}
	cfg := stConfig()
	status := snClaimRelayCurrentStatus(ctx, cfg, now)
	coldkey, err := ss58.DecodeWithPrefix(args.ColdkeySs58, ss58.BittensorPrefix)
	if err != nil {
		return &SnClaimRelayClaimNowResult{Error: &SnClaimRelayError{Message: "Not a Bittensor ss58 address."}}, nil
	}
	unreadable := func(err error) (*SnClaimRelayClaimNowResult, error) {
		glog.Infof("[sn]claim relay accounts of network %s: %s\n", clientSession.ByJwt.NetworkId, err)
		return &SnClaimRelayClaimNowResult{Error: &SnClaimRelayError{Message: "The balances could not be read."}}, nil
	}
	coldkeys, err := snClaimRelayNetworkColdkeys(clientSession)
	if err != nil {
		return unreadable(err)
	}
	if !slices.Contains(coldkeys, coldkey) || cfg == nil || !cfg.Enabled || !cfg.LedgerV2Enabled() {
		// not this network's, or banned: both read as nothing to claim
		return refuse(SnClaimRelayReasonNothingPending, 0)
	}
	// A tap claims every account of the coldkey the relay can, so it reads
	// each pool on its own: one that cannot be read waits for a later pass.
	accounts, unavailable, err := snClaimRelayColdkeyAccounts(ctx, cfg, status, coldkey, true, now)
	if err != nil {
		return unreadable(err)
	}
	if len(accounts) == 0 {
		// A tap names no partial answer, so it is answered as an app that
		// predates the capture the flag pool asks: that pool, unreadable, is
		// left out, and only another pool that cannot be read leaves the
		// balances unread.
		if slices.ContainsFunc(unavailable, func(pool *StLedgerV2UnavailablePool) bool { return pool.Kind != StLedgerV2PoolKindCtf }) {
			return unreadable(fmt.Errorf("pools %d of %d unavailable", len(unavailable), len(stLedgerV2OwnPools(cfg, stCtfPoolOn(cfg)))))
		}
		return refuse(SnClaimRelayReasonNothingPending, 0)
	}
	if !slices.ContainsFunc(accounts, func(account *SnClaimRelayAccount) bool { return account.ClaimNowAvailable }) {
		// why not: the first account with a balance says it best, the limit
		// and a queued request apply to every account alike
		reason := accounts[0]
		for _, account := range accounts {
			if 0 < account.ClaimableRao {
				reason = account
				break
			}
		}
		return refuse(reason.ClaimNowReason, reason.ClaimNowNextAtMillis)
	}
	model.AddSnClaimRelayRequest(ctx, cfg.DeploymentKey(), coldkey, now)
	snClaimRelayClaimNowRequests.WithLabelValues(snClaimRelayRequestAccepted).Inc()
	return &SnClaimRelayClaimNowResult{Requested: true}, nil
}

// snClaimRelayCoreRequests is Claim now in Redis.
type snClaimRelayCoreRequests struct {
	deploymentKey model.StDeploymentKey
}

func (self *snClaimRelayCoreRequests) List(ctx context.Context, limit int) []*snClaimRelayRequest {
	requests := []*snClaimRelayRequest{}
	for _, request := range model.GetSnClaimRelayRequests(ctx, self.deploymentKey, limit) {
		requests = append(requests, &snClaimRelayRequest{Coldkey: request.Coldkey, Time: request.Time})
	}
	return requests
}

func (self *snClaimRelayCoreRequests) Finish(ctx context.Context, coldkey [32]byte, startLimit bool) {
	model.FinishSnClaimRelayRequest(ctx, self.deploymentKey, coldkey, startLimit, server.NowUtc(), snClaimRelayClaimNowPeriod)
}

func (self *snClaimRelayCoreRequests) Limit(ctx context.Context, coldkey [32]byte) {
	model.StartSnClaimRelayLimit(ctx, self.deploymentKey, coldkey, server.NowUtc(), snClaimRelayClaimNowPeriod)
}

// snClaimRelayCoreClaimRecords keeps the paid claims in Redis.
type snClaimRelayCoreClaimRecords struct {
	deploymentKey model.StDeploymentKey
}

func (self *snClaimRelayCoreClaimRecords) Record(ctx context.Context, paid *snClaimRelayPaid) {
	model.SetSnClaimRelayClaim(ctx, self.deploymentKey, paid.NoId, paid.Index, &model.SnClaimRelayClaim{
		TxHash:     strings.ToLower(paid.TxHash),
		Cumulative: paid.Cumulative.String(),
		AmountRao:  paid.AmountRao.String(),
	})
}

// SnClaimRelayPassResult tells the task when to run the next pass.
type SnClaimRelayPassResult struct {
	State string
	Delay time.Duration
}

func (self *SnClaimRelayPassResult) String() string {
	return fmt.Sprintf("state %s, next pass in %s", self.State, self.Delay)
}

func newSnClaimRelayCore(cfg *StConfig) *snClaimRelay {
	deploymentKey := cfg.DeploymentKey()
	var relayer *snClaimRelayKey
	return &snClaimRelay{
		now: server.NowUtc,
		signer: func() (*snClaimRelaySigner, error) {
			var err error
			if relayer, err = loadSnClaimRelayKey(cfg); err != nil {
				return nil, err
			}
			return &snClaimRelaySigner{Address: relayer.address}, nil
		},
		chain: func(*snClaimRelaySigner) (snClaimRelayChain, error) {
			if !cfg.Enabled || !cfg.LedgerV2Enabled() {
				return nil, errStLedgerV2NotConfigured
			}
			return newSnClaimRelayCoreChain(cfg, relayer), nil
		},
		ledger:       &snClaimRelayCoreLedger{cfg: cfg},
		requests:     &snClaimRelayCoreRequests{deploymentKey: deploymentKey},
		claimRecords: &snClaimRelayCoreClaimRecords{deploymentKey: deploymentKey},
		lastStatus: func(ctx context.Context) *SnClaimRelayStatus {
			return snClaimRelayReadStatus(ctx, deploymentKey)
		},
		publish: func(ctx context.Context, status *SnClaimRelayStatus) {
			raw, err := json.Marshal(status)
			server.Raise(err)
			model.SetSnClaimRelayStatus(ctx, deploymentKey, raw)
		},
		claimResults: observeSnClaimRelayClaims,
		banned:       SnWalletBanned,
	}
}

// SnClaimRelayPass runs one pass of the relay on this process. The task holds one
// instance at a time.
func SnClaimRelayPass(ctx context.Context) *SnClaimRelayPassResult {
	cfg := stConfig()
	if cfg == nil {
		return &SnClaimRelayPassResult{State: SnClaimRelayStateNotConfigured, Delay: snClaimRelayIdleDelay}
	}
	status := newSnClaimRelayCore(cfg).pass(ctx)
	observeSnClaimRelayPass(status)
	if status.Detail != "" {
		glog.Infof("[sn]claim relay state %s: %s\n", status.State, status.Detail)
	}
	return &SnClaimRelayPassResult{State: status.State, Delay: snClaimRelayNextDelay(status)}
}

// snClaimRelayNextDelay is when the next pass runs: soon while claims move and
// more are due, the readable pools' too while another pool cannot be read; else
// after the idle delay.
func snClaimRelayNextDelay(status *SnClaimRelayStatus) time.Duration {
	moving := status.State == SnClaimRelayStateRunning || (status.State == SnClaimRelayStateLedgerUnavailable && 0 < len(status.UnavailablePools))
	if status.Again && moving {
		return snClaimRelayBusyDelay
	}
	return snClaimRelayIdleDelay
}
