package controller

// sn_claim_relay_api.go — the claim relay's answer for the apps: per coldkey of
// the caller's network, what is pending, why, and whether Claim now can be
// tapped. Apps draw this through the sdk (sdk sn_claim_relay.go), which also
// chooses between the relay and the device's own claim.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
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
	Error              *SnClaimRelayError     `json:"error,omitempty"`
}

type SnClaimRelayError struct {
	Message string `json:"message"`
}

// SnClaimRelayAccount is one coldkey's account in the operator's pool.
type SnClaimRelayAccount struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
	NoId        int64  `json:"no_id"`
	Index       int64  `json:"index"`
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

// One entry's balance as the relay's status judges it. Pure, for the tests. The
// mirror behind the account is the chain's own read here, so a Reason never says
// more than what is true at this read.
func snClaimRelayAccountView(status *SnClaimRelayStatus, coldkeySs58 string, entry *StLedgerV2AccountEntry, requested bool, limitUntil time.Time, now time.Time) (*SnClaimRelayAccount, error) {
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
	account := &SnClaimRelayAccount{
		ColdkeySs58:  coldkeySs58,
		NoId:         int64(entry.NoId),
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
	refusal := status.Refusals[snClaimRelayAccountKey(entry.NoId, entry.Index)]
	refused := refusal != nil && now.Before(refusal.Until)
	class := snClaimRelayClassOf(status.State)
	switch {
	case claimable.IsZero():
		account.Reason = SnClaimRelayReasonNothingPending
	case class == snClaimRelayClassNotRunning:
		account.Reason = SnClaimRelayReasonRelayNotRunning
	case class == snClaimRelayClassPaused:
		account.Reason = SnClaimRelayReasonRelayPaused
	case refused:
		account.Reason = SnClaimRelayReasonRefused
	case !requested && !snClaimRelayOverThreshold(value):
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

// snClaimRelayColdkeyAccounts reads the relay's view of one coldkey's account in
// the operator's pool: none, or one.
func snClaimRelayColdkeyAccounts(ctx context.Context, cfg *StConfig, status *SnClaimRelayStatus, coldkey [32]byte, now time.Time) ([]*SnClaimRelayAccount, error) {
	ledger, err := StLedgerV2Accounts(ctx, coldkey)
	if err != nil {
		return nil, err
	}
	address, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		return nil, err
	}
	requested, limitUntil := model.GetSnClaimRelayRequest(ctx, cfg.DeploymentKey(), coldkey, now)
	accounts := []*SnClaimRelayAccount{}
	for _, entry := range ledger.Accounts {
		// the relay claims in the operator's own pool
		if entry.NoId != cfg.NoId {
			continue
		}
		account, err := snClaimRelayAccountView(status, address, entry, requested, limitUntil, now)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

// snClaimRelayNetworkAccounts reads the relay's view of each of the network's
// accounts in the operator's pool.
func snClaimRelayNetworkAccounts(clientSession *session.ClientSession, cfg *StConfig, status *SnClaimRelayStatus, now time.Time) ([]*SnClaimRelayAccount, error) {
	coldkeys, err := snClaimRelayNetworkColdkeys(clientSession)
	if err != nil {
		return nil, err
	}
	accounts := []*SnClaimRelayAccount{}
	if cfg == nil || !cfg.Enabled || !cfg.LedgerV2Enabled() {
		return accounts, nil
	}
	for _, coldkey := range coldkeys {
		coldkeyAccounts, err := snClaimRelayColdkeyAccounts(clientSession.Ctx, cfg, status, coldkey, now)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, coldkeyAccounts...)
	}
	return accounts, nil
}

// SnClaimRelay answers GET /sn/claim-relay for the caller's network.
func SnClaimRelay(clientSession *session.ClientSession) (*SnClaimRelayResult, error) {
	now := server.NowUtc()
	cfg := stConfig()
	status := snClaimRelayCurrentStatus(clientSession.Ctx, cfg, now)
	var price *big.Int
	if parsed, ok := new(big.Int).SetString(status.AlphaPriceWei, 10); ok {
		price = parsed
	}
	result := &SnClaimRelayResult{
		State:                    status.State,
		AutomaticThresholdTaoRao: int64(snClaimRelayAutomaticTaoRao),
		TransferFloorTaoRao:      int64(snClaimRelayFloorTaoRao),
		AutomaticThresholdRao:    snClaimRelayAlphaOf(snClaimRelayAutomaticTaoRao, price).Int64(),
		TransferFloorRao:         snClaimRelayAlphaOf(snClaimRelayFloorTaoRao, price).Int64(),
		ClaimNowPeriodDays:       int(snClaimRelayClaimNowPeriod / (24 * time.Hour)),
		Accounts:                 []*SnClaimRelayAccount{},
	}
	accounts, err := snClaimRelayNetworkAccounts(clientSession, cfg, status, now)
	if err != nil {
		// Whatever the balances are, they are not zero: the relay's state still
		// reaches the app, and the app says they could not be read.
		glog.Infof("[sn]claim relay accounts of network %s: %s\n", clientSession.ByJwt.NetworkId, err)
		result.Error = &SnClaimRelayError{Message: "The balances could not be read."}
		return result, nil
	}
	result.Accounts = accounts
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
	accounts, err := snClaimRelayColdkeyAccounts(ctx, cfg, status, coldkey, now)
	if err != nil {
		return unreadable(err)
	}
	if len(accounts) == 0 {
		return refuse(SnClaimRelayReasonNothingPending, 0)
	}
	account := accounts[0]
	if !account.ClaimNowAvailable {
		return refuse(account.ClaimNowReason, account.ClaimNowNextAtMillis)
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
		ledger:   &snClaimRelayCoreLedger{cfg: cfg},
		requests: &snClaimRelayCoreRequests{deploymentKey: deploymentKey},
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
	result := &SnClaimRelayPassResult{State: status.State, Delay: snClaimRelayIdleDelay}
	if status.Again && status.State == SnClaimRelayStateRunning {
		result.Delay = snClaimRelayBusyDelay
	}
	return result
}
