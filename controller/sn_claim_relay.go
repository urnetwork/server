package controller

// sn_claim_relay.go — the settlement v2 claim relay (apple BTEASY.md, "Claim
// relay"). vault2's claim is open to anyone and pays the leaf's coldkey
// whoever sends it, so URnetwork can claim for every consented network, wallet
// or local key, and pay the gas, with no tap and no permission. The relayer can
// redirect nothing.
//
// Policy (owner, question 1). Everything is in TAO value, since gas is paid in
// TAO:
//   - a coldkey's balance is claimed automatically once it is worth 0.0125 TAO;
//   - "Claim now" claims it once it clears the transfer floor of 0.0001 TAO, at
//     most once per coldkey per 30 days;
//   - claims spend at most 0.01 TAO a day. Past that they wait for the next UTC
//     day, which costs nothing: v2 credit never expires.
//
// Stops, all of them fail closed:
//   - no relayer key: nothing is read or sent, and the status says so. This is
//     the only gate; there is no other setting;
//   - a gas price over the ceiling;
//   - a claim's gas jumping, which is what griefing looks like: third parties
//     filling vault2's staking-hotkey list take a claim from about 150,000 gas
//     to about 1.26M. A preflight estimate over the limit stops the pass before
//     anything is sent, and a finalized claim that used over the limit or burned
//     its whole gas limit stops the rest of its UTC day;
//   - a balance that cannot cover the next transaction;
//   - any chain read failing.
//
// The relayer signs each claim at its estimate plus 20%, not at the 2,600,000
// other senders use (sn VERSION2-FINAL.md §8.1). That floor exists so that a
// claim survives a list lengthened after its estimate, and only the gas used is
// paid, but 2,600,000 gas at 5 gwei needs 0.013 TAO on hand and would burn it if
// the runtime refused the claim, which alone is over the day's cap. At the
// estimate plus 20% the most a transaction can cost is known before it is sent,
// so the cap holds; a list lengthened after the estimate costs one reverted claim
// and trips the stop.
//
// The chain, the clock and the stores are injected. Nothing here keeps state of
// its own: what was spent comes from the durable transaction intents of the
// relayer's account, what was claimed from the chain, and a pass that dies is
// repeated by the next one (sn_claim_relay_chain.go, the task in
// taskworker/work/sn_claim_relay_work.go).

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
)

const (
	// 0.0125 TAO, in rao of TAO: 1.9 α at sn's planning price, about 3 α today.
	snClaimRelayAutomaticTaoRao uint64 = 12_500_000
	// 0.0001 TAO, in rao of TAO: the runtime refuses a smaller transfer, and the
	// claim reverts (sn VERSION2-FINAL.md §7.3).
	snClaimRelayFloorTaoRao uint64 = 100_000
	// The floor is compared with a price read before the claim is included, so
	// a claim has to clear it by this much to be sent.
	snClaimRelayFloorMarginBps uint64 = 500

	// The gas a preflight estimate or a finalized claim may reach. A claim is
	// about 150,000 to 207,000 gas and about 1.26M with the list full; this is
	// the list at about 25 entries.
	snClaimRelayGriefingGas uint64 = 400_000

	snClaimRelayClaimNowPeriod = 30 * 24 * time.Hour

	// A claim's gas limit at sn's measured 155,000 gas, for judging the funds when
	// nothing is due to estimate.
	snClaimRelayPlanningGasLimit uint64 = 186_000

	// The most claims in one transaction.
	snClaimRelayBatchMaximum = onchain.ClaimBatchMaxClaims
	// Estimates in one pass, so that many refusals cannot make a pass long.
	snClaimRelayPreflightsPerPass = 64
	// Requests read in one pass. Past this they wait for the next pass.
	snClaimRelayRequestsPerPass = 256
	// A claim whose preflight was refused is tried again after this. It cost
	// nothing, and most refusals clear.
	snClaimRelayRefusalRetry = 1 * time.Hour
	// Refusals carried in the status, the oldest dropped first.
	snClaimRelayRefusalsLimit = 1024

	// One pass sends at most one transaction. With more due the next pass comes
	// after the busy delay, otherwise after the idle delay.
	snClaimRelayIdleDelay = 5 * time.Minute
	snClaimRelayBusyDelay = 15 * time.Second
	// A status older than this means the relay is not running. It covers the
	// task's own limit, during which a pass may wait for one claim's finality.
	snClaimRelayStatusStale = 40 * time.Minute
)

var (
	// 0.01 TAO a day, in wei: 1 TAO is 10^18 wei on the chain's EVM.
	snClaimRelayDailyCapWei = big.NewInt(10_000_000_000_000_000)
	// 10 gwei, twice the chain's usual 5. A claim's gas stays near a tenth of a
	// claim at the automatic threshold.
	snClaimRelayGasPriceCeilingWei = big.NewInt(10_000_000_000)

	snClaimRelayPriceScale = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

// Relay states, in the status and on the api. Only running and cap_reached have
// claims moving; the api groups the rest for the apps (snClaimRelayClassOf).
const (
	SnClaimRelayStateRunning           = "running"
	SnClaimRelayStateCapReached        = "cap_reached"
	SnClaimRelayStateInFlight          = "in_flight"
	SnClaimRelayStateNoKey             = "no_key"
	SnClaimRelayStateBadKey            = "bad_key"
	SnClaimRelayStateNotConfigured     = "not_configured"
	SnClaimRelayStateNotRunning        = "not_running"
	SnClaimRelayStateChainUnavailable  = "chain_unavailable"
	SnClaimRelayStateLedgerUnavailable = "ledger_unavailable"
	SnClaimRelayStateSendFailed        = "send_failed"
	SnClaimRelayStateLowBalance        = "low_balance"
	SnClaimRelayStateGasPriceHigh      = "gas_price_high"
	SnClaimRelayStateGriefingStop      = "griefing_stop"
)

var snClaimRelayStates = []string{
	SnClaimRelayStateRunning, SnClaimRelayStateCapReached, SnClaimRelayStateInFlight, SnClaimRelayStateNoKey,
	SnClaimRelayStateBadKey, SnClaimRelayStateNotConfigured, SnClaimRelayStateChainUnavailable, SnClaimRelayStateLedgerUnavailable,
	SnClaimRelayStateSendFailed, SnClaimRelayStateLowBalance, SnClaimRelayStateGasPriceHigh, SnClaimRelayStateGriefingStop,
}

var (
	errSnClaimRelayNoKey = errors.New("claim relay: no relayer key is configured")
	// A claim whose outcome is not known yet. The next pass finishes it first.
	errSnClaimRelayInFlight = errors.New("claim relay: a claim's outcome is not final yet")
)

// SnClaimRelayStatus is what the last pass found, for the api and the metrics.
// Amounts are decimal strings: wei for gas and balances, wei of TAO per α for
// the price.
type SnClaimRelayStatus struct {
	State           string                          `json:"state"`
	PassTime        time.Time                       `json:"pass_time"`
	Detail          string                          `json:"detail,omitempty"`
	RelayerAddress  string                          `json:"relayer_address,omitempty"`
	BalanceWei      string                          `json:"balance_wei,omitempty"`
	GasPriceWei     string                          `json:"gas_price_wei,omitempty"`
	AlphaPriceWei   string                          `json:"alpha_price_wei,omitempty"`
	SpentTodayWei   string                          `json:"spent_today_wei,omitempty"`
	ClaimsPaidToday int                             `json:"claims_paid_today"`
	ClaimsPending   int                             `json:"claims_pending"`
	OverThreshold   int                             `json:"over_threshold"`
	LastEstimateGas uint64                          `json:"last_estimate_gas,omitempty"`
	Refusals        map[string]*SnClaimRelayRefusal `json:"refusals,omitempty"`
	// More is due now, so the next pass should not wait.
	Again bool `json:"again,omitempty"`
}

// SnClaimRelayRefusal is why one account's claim is waiting: until the next UTC
// day after it reverted, or for a while after its preflight was refused.
type SnClaimRelayRefusal struct {
	Time   time.Time `json:"time"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

func snClaimRelayAccountKey(noId uint64, index uint64) string {
	return fmt.Sprintf("%d/%d", noId, index)
}

// snClaimRelayAccount is one account of the pool's latest finalized root.
// Claimed starts as the mirror of vault2's Claimed events, which can only be
// behind; a claim is sent only after a finalized read of it.
type snClaimRelayAccount struct {
	NoId       uint64
	Index      uint64
	RootIndex  uint64
	Coldkey    [32]byte
	Cumulative uint256.Int
	Claimed    uint256.Int
}

func (self *snClaimRelayAccount) key() string {
	return snClaimRelayAccountKey(self.NoId, self.Index)
}

// The rao of α a claim would pay now.
func (self *snClaimRelayAccount) claimable() *uint256.Int {
	if !self.Claimed.Lt(&self.Cumulative) {
		return new(uint256.Int)
	}
	return new(uint256.Int).Sub(&self.Cumulative, &self.Claimed)
}

// snClaimRelayClaim is one account's claim, ready to send.
type snClaimRelayClaim struct {
	Account *snClaimRelayAccount
	Proof   []merkle.LedgerNode
	// the preflight's gas
	Estimate uint64
	// asked for with Claim now
	Requested bool
}

// snClaimRelayFacts are the live chain values one pass decides with.
type snClaimRelayFacts struct {
	BalanceWei *big.Int
	// what a transaction pays per gas now
	GasPriceWei *big.Int
	// the most a transaction signed now can pay per gas
	FeeCapWei *big.Int
	// wei of TAO per α, the runtime's own price
	AlphaPriceWei *big.Int
}

// snClaimRelaySpend is what the relayer's account spent in one UTC day: what its
// finalized transactions paid, and the most its unfinished ones can.
type snClaimRelaySpend struct {
	SpentWei   *big.Int
	ClaimsPaid int
	// a finalized claim's gas jumped, or a claim burned its whole gas limit
	Griefed bool
	// accounts whose claim reverted, by snClaimRelayAccountKey
	RevertedAccounts map[string]string
}

// snClaimRelayBatch is a batch sized through the claim batcher: the first Count
// claims it was offered.
type snClaimRelayBatch struct {
	Count    int
	To       common.Address
	Calldata []byte
	GasLimit uint64
}

// snClaimRelaySend is the one transaction of a pass.
type snClaimRelaySend struct {
	To       common.Address
	Calldata []byte
	GasLimit uint64
	Claims   []*snClaimRelayClaim
	Batch    bool
}

// snClaimRelayOutcome is one finalized transaction's result per claim, in the
// order of the send's claims.
type snClaimRelayOutcome struct {
	TxHash   string
	GasUsed  uint64
	Griefed  bool
	Reverted bool
	Items    []*snClaimRelayItemOutcome
}

type snClaimRelayItemOutcome struct {
	Paid      bool
	AmountRao *big.Int
	// not attempted: the batch's gas ran out before it; it is simply due again
	NotAttempted bool
	Reason       string
}

// snClaimRelayChain is everything the relay reads from and sends to the chain.
// sn_claim_relay_chain.go implements it over the durable transaction engine.
type snClaimRelayChain interface {
	// Reconcile finishes the relayer's unfinished transactions, in nonce order.
	Reconcile(ctx context.Context) error
	Facts(ctx context.Context) (*snClaimRelayFacts, error)
	// Spend reports the transactions the relayer's account created since the
	// start of a UTC day.
	Spend(ctx context.Context, since time.Time) (*snClaimRelaySpend, error)
	// Claimed reads vault2's claimed at the finalized head.
	Claimed(ctx context.Context, account *snClaimRelayAccount) (*uint256.Int, error)
	// VerifyProof checks an account and its proof against vault2's root.
	VerifyProof(ctx context.Context, account *snClaimRelayAccount, proof []merkle.LedgerNode) (bool, error)
	// Estimate preflights one claim; an error is its refusal.
	Estimate(ctx context.Context, claim *snClaimRelayClaim) (uint64, error)
	// Claim is the single claim's transaction.
	Claim(claim *snClaimRelayClaim) (common.Address, []byte)
	// Batching reports a usable batcher.
	Batching(ctx context.Context) bool
	// SizeBatch sizes a batch of claims, each given gasPerClaim. Nil without a
	// usable batcher.
	SizeBatch(ctx context.Context, claims []*snClaimRelayClaim, gasPerClaim uint64) (*snClaimRelayBatch, error)
	// Send signs and sends the transaction and waits for its finality. A nil
	// outcome with errSnClaimRelayInFlight leaves it to Reconcile.
	Send(ctx context.Context, send *snClaimRelaySend) (*snClaimRelayOutcome, error)
}

// snClaimRelayLedger reads the accounts of the pool's latest finalized root.
type snClaimRelayLedger interface {
	// Accounts is empty until the pool has a finalized, recorded root.
	Accounts(ctx context.Context) ([]*snClaimRelayAccount, error)
	Proof(ctx context.Context, account *snClaimRelayAccount) ([]merkle.LedgerNode, error)
}

// snClaimRelayRequest is one unfinished Claim now.
type snClaimRelayRequest struct {
	Coldkey [32]byte
	Time    time.Time
}

// snClaimRelayRequests holds Claim now's requests and its limit.
type snClaimRelayRequests interface {
	List(ctx context.Context, limit int) []*snClaimRelayRequest
	// Finish removes a request. A refunded one gives the tap back: the coldkey's
	// limit ends, since nothing was sent for it.
	Finish(ctx context.Context, coldkey [32]byte, refund bool)
}

// snClaimRelaySigner is the configured relayer, without its key: the policy never
// needs the key, only that there is one.
type snClaimRelaySigner struct {
	Address common.Address
}

type snClaimRelay struct {
	now          func() time.Time
	signer       func() (*snClaimRelaySigner, error)
	chain        func(signer *snClaimRelaySigner) (snClaimRelayChain, error)
	ledger       snClaimRelayLedger
	requests     snClaimRelayRequests
	lastStatus   func(ctx context.Context) *SnClaimRelayStatus
	publish      func(ctx context.Context, status *SnClaimRelayStatus)
	claimResults func(result string, count int)
	banned       func(coldkey [32]byte) bool
}

func snClaimRelayDayStart(now time.Time) time.Time {
	year, month, day := now.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// The TAO value, in rao of TAO, of an amount of α in rao at a price in wei of
// TAO per α.
func snClaimRelayValueTaoRao(amount *uint256.Int, alphaPriceWei *big.Int) *big.Int {
	if alphaPriceWei == nil {
		return new(big.Int)
	}
	value := new(big.Int).Mul(amount.ToBig(), alphaPriceWei)
	return value.Quo(value, snClaimRelayPriceScale)
}

func snClaimRelayClearsFloor(value *big.Int) bool {
	least := new(big.Int).SetUint64(snClaimRelayFloorTaoRao * (10_000 + snClaimRelayFloorMarginBps) / 10_000)
	return 0 <= value.Cmp(least)
}

func snClaimRelayOverThreshold(value *big.Int) bool {
	return 0 <= value.Cmp(new(big.Int).SetUint64(snClaimRelayAutomaticTaoRao))
}

// The α, in rao, worth a value in rao of TAO, rounded up, for the apps to show.
// Zero without a price.
func snClaimRelayAlphaOf(valueTaoRao uint64, alphaPriceWei *big.Int) *big.Int {
	if alphaPriceWei == nil || alphaPriceWei.Sign() <= 0 {
		return new(big.Int)
	}
	amount := new(big.Int).Mul(new(big.Int).SetUint64(valueTaoRao), snClaimRelayPriceScale)
	amount.Add(amount, new(big.Int).Sub(alphaPriceWei, big.NewInt(1)))
	return amount.Quo(amount, alphaPriceWei)
}

// A gas limit of an estimate: plus 20%, as the engine's.
func snClaimRelayGasLimit(estimate uint64) uint64 {
	return estimate + (estimate+4)/5
}

func snClaimRelayLiability(gasLimit uint64, feeCapWei *big.Int) *big.Int {
	return new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCapWei)
}

func snClaimRelayMin(values ...*big.Int) *big.Int {
	least := values[0]
	for _, value := range values[1:] {
		if value.Cmp(least) < 0 {
			least = value
		}
	}
	return least
}

// pass decides and sends at most one transaction, and reports what it found.
func (self *snClaimRelay) pass(ctx context.Context) *SnClaimRelayStatus {
	now := self.now().UTC()
	status := &SnClaimRelayStatus{State: SnClaimRelayStateRunning, PassTime: now}
	if previous := self.lastStatus(ctx); previous != nil {
		status.Refusals = snClaimRelayLiveRefusals(previous.Refusals, now)
	}
	self.run(ctx, now, status)
	snClaimRelayLimitRefusals(status)
	self.publish(ctx, status)
	return status
}

// Fills the status; every return has set its state.
func (self *snClaimRelay) run(ctx context.Context, now time.Time, status *SnClaimRelayStatus) {
	signer, err := self.signer()
	if errors.Is(err, errSnClaimRelayNoKey) {
		status.State = SnClaimRelayStateNoKey
		return
	}
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateBadKey, err.Error()
		return
	}
	status.RelayerAddress = strings.ToLower(signer.Address.Hex())
	chain, err := self.chain(signer)
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateNotConfigured, err.Error()
		return
	}

	// An unfinished transaction owns the next nonce, and its cost is not known.
	if err := chain.Reconcile(ctx); err != nil {
		status.State, status.Detail = SnClaimRelayStateInFlight, err.Error()
		return
	}
	facts, err := chain.Facts(ctx)
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
		return
	}
	status.BalanceWei = facts.BalanceWei.String()
	status.GasPriceWei = facts.GasPriceWei.String()
	status.AlphaPriceWei = facts.AlphaPriceWei.String()
	spend, err := chain.Spend(ctx, snClaimRelayDayStart(now))
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
		return
	}
	self.applySpend(status, spend, now)

	accounts, err := self.ledger.Accounts(ctx)
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateLedgerUnavailable, err.Error()
		return
	}
	candidates := self.candidates(ctx, accounts, facts, status)

	budget := new(big.Int).Sub(snClaimRelayDailyCapWei, spend.SpentWei)
	planned := snClaimRelayLiability(snClaimRelayPlanningGasLimit, facts.FeeCapWei)
	switch {
	case spend.Griefed:
		status.State = SnClaimRelayStateGriefingStop
		return
	case 0 < facts.GasPriceWei.Cmp(snClaimRelayGasPriceCeilingWei):
		status.State = SnClaimRelayStateGasPriceHigh
		return
	case facts.BalanceWei.Cmp(planned) < 0:
		// Whatever is due, and whatever is tapped next, cannot be sent.
		status.State = SnClaimRelayStateLowBalance
		return
	case budget.Cmp(planned) < 0:
		status.State = SnClaimRelayStateCapReached
		return
	case len(candidates) == 0:
		return
	}

	send, more := self.prepare(ctx, chain, candidates, facts, budget, now, status)
	if send == nil {
		return
	}

	// A pass may wait minutes for finality; the status stays fresh meanwhile.
	self.publish(ctx, status)
	outcome, err := chain.Send(ctx, send)
	if errors.Is(err, errSnClaimRelayInFlight) {
		status.State, status.Detail = SnClaimRelayStateInFlight, err.Error()
		return
	}
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateSendFailed, err.Error()
		return
	}
	self.applyOutcome(ctx, send, outcome, now, status)

	if spend, err = chain.Spend(ctx, snClaimRelayDayStart(now)); err != nil {
		status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
		return
	}
	self.applySpend(status, spend, now)
	if spend.Griefed || outcome.Griefed {
		status.State = SnClaimRelayStateGriefingStop
		return
	}
	status.Again = more || 1 < len(candidates)
}

// Records the day's spend in the status. A claim that reverted waits for the next
// UTC day: it cost real gas, and it passed its preflight.
func (self *snClaimRelay) applySpend(status *SnClaimRelayStatus, spend *snClaimRelaySpend, now time.Time) {
	status.SpentTodayWei = spend.SpentWei.String()
	status.ClaimsPaidToday = spend.ClaimsPaid
	for key, reason := range spend.RevertedAccounts {
		if status.Refusals == nil {
			status.Refusals = map[string]*SnClaimRelayRefusal{}
		}
		status.Refusals[key] = &SnClaimRelayRefusal{Time: now, Until: snClaimRelayDayStart(now).Add(24 * time.Hour), Reason: reason}
	}
}

// Claim now first, oldest first, and then every account over the threshold,
// largest first: a claim moves the most value for the same gas. A request the
// floor no longer clears is given back, so a claim under it is never sent.
func (self *snClaimRelay) candidates(ctx context.Context, accounts []*snClaimRelayAccount, facts *snClaimRelayFacts, status *SnClaimRelayStatus) []*snClaimRelayClaim {
	coldkeyAccounts := make(map[[32]byte]*snClaimRelayAccount, len(accounts))
	over := []*snClaimRelayClaim{}
	requested := map[string]bool{}
	claims := []*snClaimRelayClaim{}
	for _, account := range accounts {
		coldkeyAccounts[account.Coldkey] = account
	}
	for _, request := range self.requests.List(ctx, snClaimRelayRequestsPerPass) {
		account := coldkeyAccounts[request.Coldkey]
		if account == nil {
			self.requests.Finish(ctx, request.Coldkey, true)
			continue
		}
		claimable := account.claimable()
		if claimable.IsZero() {
			// paid, by this relay or anyone else
			self.requests.Finish(ctx, request.Coldkey, false)
			continue
		}
		if !snClaimRelayClearsFloor(snClaimRelayValueTaoRao(claimable, facts.AlphaPriceWei)) || self.banned(account.Coldkey) {
			self.requests.Finish(ctx, request.Coldkey, true)
			continue
		}
		requested[account.key()] = true
		claims = append(claims, &snClaimRelayClaim{Account: account, Requested: true})
	}
	for _, account := range accounts {
		if requested[account.key()] || self.banned(account.Coldkey) {
			continue
		}
		if snClaimRelayOverThreshold(snClaimRelayValueTaoRao(account.claimable(), facts.AlphaPriceWei)) {
			over = append(over, &snClaimRelayClaim{Account: account})
		}
	}
	slices.SortStableFunc(over, func(a, b *snClaimRelayClaim) int {
		if c := b.Account.claimable().Cmp(a.Account.claimable()); c != 0 {
			return c
		}
		if a.Account.Index < b.Account.Index {
			return -1
		}
		return 1
	})
	status.OverThreshold = len(over)
	status.ClaimsPending = len(over) + len(claims)
	return append(claims, over...)
}

// prepare verifies candidates against the chain until it has a transaction. The
// claims it refuses leave the rest alone. A nil send means the status says why
// nothing is sent. more reports claims left over for the next pass.
func (self *snClaimRelay) prepare(ctx context.Context, chain snClaimRelayChain, candidates []*snClaimRelayClaim, facts *snClaimRelayFacts, budget *big.Int, now time.Time, status *SnClaimRelayStatus) (send *snClaimRelaySend, more bool) {
	funds := snClaimRelayMin(budget, facts.BalanceWei)
	claimsPerTransaction := 1
	if chain.Batching(ctx) {
		claimsPerTransaction = snClaimRelayBatchMaximum
	}
	kept := []*snClaimRelayClaim{}
	estimates := uint64(0)
	preflights := 0
	for index, candidate := range candidates {
		if preflights == snClaimRelayPreflightsPerPass {
			more = true
			break
		}
		account := candidate.Account
		if refusal := status.Refusals[account.key()]; refusal != nil && now.Before(refusal.Until) {
			continue
		}
		claimed, err := chain.Claimed(ctx, account)
		if err != nil {
			status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
			return nil, false
		}
		// The mirror can only be behind: this is the claim's real size.
		verified := *account
		verified.Claimed = *claimed
		claim := &snClaimRelayClaim{Account: &verified, Requested: candidate.Requested}
		value := snClaimRelayValueTaoRao(verified.claimable(), facts.AlphaPriceWei)
		switch {
		case verified.claimable().IsZero():
			if candidate.Requested {
				self.requests.Finish(ctx, account.Coldkey, false)
			}
			continue
		case candidate.Requested && !snClaimRelayClearsFloor(value):
			self.requests.Finish(ctx, account.Coldkey, true)
			continue
		case !candidate.Requested && !snClaimRelayOverThreshold(value):
			continue
		}
		claim.Proof, err = self.ledger.Proof(ctx, &verified)
		if err != nil {
			status.State, status.Detail = SnClaimRelayStateLedgerUnavailable, err.Error()
			return nil, false
		}
		valid, err := chain.VerifyProof(ctx, &verified, claim.Proof)
		if err != nil {
			status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
			return nil, false
		}
		if !valid {
			self.refuse(&verified, "the proof does not verify against vault2's root", now, snClaimRelayRefusalRetry, status)
			continue
		}
		preflights++
		estimate, err := chain.Estimate(ctx, claim)
		if err != nil {
			self.refuse(&verified, stLedgerV2RevertName(err), now, snClaimRelayRefusalRetry, status)
			continue
		}
		status.LastEstimateGas = estimate
		if snClaimRelayGriefingGas < estimate {
			status.State = SnClaimRelayStateGriefingStop
			status.Detail = fmt.Sprintf("a claim's preflight estimated %d gas, over %d", estimate, snClaimRelayGriefingGas)
			return nil, false
		}
		claim.Estimate = estimate
		// The first claim has to fit by the most it can cost. The batch is sized
		// from what the others should cost, and checked as a whole below.
		if len(kept) == 0 {
			liability := snClaimRelayLiability(snClaimRelayGasLimit(estimate), facts.FeeCapWei)
			if 0 < liability.Cmp(budget) {
				status.State = SnClaimRelayStateCapReached
				return nil, false
			}
			if 0 < liability.Cmp(facts.BalanceWei) {
				status.State = SnClaimRelayStateLowBalance
				return nil, false
			}
		}
		kept = append(kept, claim)
		estimates += estimate
		if claimsPerTransaction <= len(kept) || 0 < snClaimRelayLiability(estimates, facts.FeeCapWei).Cmp(funds) {
			more = index+1 < len(candidates)
			break
		}
	}
	if len(kept) == 0 {
		return nil, more
	}
	return self.transaction(ctx, chain, kept, facts, funds), more || 1 < len(kept)
}

// transaction is a batch of the kept claims when a batcher is usable and at
// least two fit the funds, or the first alone. A batch names its gas limit: the
// most it can cost is checked against the funds before it is sent.
func (self *snClaimRelay) transaction(ctx context.Context, chain snClaimRelayChain, kept []*snClaimRelayClaim, facts *snClaimRelayFacts, funds *big.Int) *snClaimRelaySend {
	claims := kept
	gasPerClaim := uint64(0)
	for _, claim := range kept {
		gasPerClaim = max(gasPerClaim, snClaimRelayGasLimit(claim.Estimate))
	}
	for 2 <= len(claims) {
		batch, err := chain.SizeBatch(ctx, claims, gasPerClaim)
		if err != nil {
			glog.Infof("[sn]claim relay batch of %d is not ready, claiming alone: %s\n", len(claims), err)
			break
		}
		if batch == nil || batch.Count < 2 {
			break
		}
		liability := snClaimRelayLiability(batch.GasLimit, facts.FeeCapWei)
		if liability.Cmp(funds) <= 0 {
			return &snClaimRelaySend{To: batch.To, Calldata: batch.Calldata, GasLimit: batch.GasLimit, Claims: claims[:batch.Count], Batch: true}
		}
		// As many as fit by this size's cost a claim, and one fewer at least.
		fit := new(big.Int).Quo(new(big.Int).Mul(funds, big.NewInt(int64(batch.Count))), liability).Int64()
		claims = claims[:min(int(fit), batch.Count-1)]
	}
	first := kept[0]
	to, calldata := chain.Claim(first)
	return &snClaimRelaySend{To: to, Calldata: calldata, GasLimit: snClaimRelayGasLimit(first.Estimate), Claims: []*snClaimRelayClaim{first}}
}

func (self *snClaimRelay) refuse(account *snClaimRelayAccount, reason string, now time.Time, wait time.Duration, status *SnClaimRelayStatus) {
	if status.Refusals == nil {
		status.Refusals = map[string]*SnClaimRelayRefusal{}
	}
	status.Refusals[account.key()] = &SnClaimRelayRefusal{Time: now, Until: now.Add(wait), Reason: reason}
	self.claimResults(SnClaimRelayResultRefused, 1)
	glog.Infof("[sn]claim relay account %s refused: %s\n", account.key(), reason)
}

func (self *snClaimRelay) applyOutcome(ctx context.Context, send *snClaimRelaySend, outcome *snClaimRelayOutcome, now time.Time, status *SnClaimRelayStatus) {
	glog.Infof("[sn]claim relay sent %d claims in %s: %d gas, reverted %t\n", len(send.Claims), outcome.TxHash, outcome.GasUsed, outcome.Reverted)
	for index, claim := range send.Claims {
		if len(outcome.Items) <= index {
			break
		}
		item := outcome.Items[index]
		switch {
		case item.Paid:
			self.claimResults(SnClaimRelayResultPaid, 1)
			if claim.Requested {
				self.requests.Finish(ctx, claim.Account.Coldkey, false)
			}
		case item.NotAttempted:
			self.claimResults(SnClaimRelayResultNotAttempted, 1)
		default:
			// A refused claim is due again, but not today: it burned gas.
			self.refuse(claim.Account, item.Reason, now, snClaimRelayDayStart(now).Add(24*time.Hour).Sub(now), status)
		}
	}
}

// Drops refusals that have run out.
func snClaimRelayLiveRefusals(refusals map[string]*SnClaimRelayRefusal, now time.Time) map[string]*SnClaimRelayRefusal {
	var live map[string]*SnClaimRelayRefusal
	for key, refusal := range refusals {
		if refusal != nil && now.Before(refusal.Until) {
			if live == nil {
				live = map[string]*SnClaimRelayRefusal{}
			}
			live[key] = refusal
		}
	}
	return live
}

func snClaimRelayLimitRefusals(status *SnClaimRelayStatus) {
	for snClaimRelayRefusalsLimit < len(status.Refusals) {
		oldestKey := ""
		for key, refusal := range status.Refusals {
			if oldestKey == "" || refusal.Time.Before(status.Refusals[oldestKey].Time) {
				oldestKey = key
			}
		}
		delete(status.Refusals, oldestKey)
	}
}
