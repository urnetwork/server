package controller

// The server's end of demand deposits (sn/demand). vault demand.yml holds the
// operator's two wallets. Its alpha wallet is the coordinator's deposit signer,
// the one account whose stake on the deposit hotkey a deposit can draw on and
// whose gas the operator gas policy already covers, so all its transactions go
// through the same durable sender as the deposit itself. Without demand.yml
// nothing converts: a deposit is paid from what the wallet already holds, or
// fails, as before. A broken demand.yml converts nothing either, and refuses no
// deposit the wallet can already pay.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urfoundation/sn/demand"
	"github.com/urfoundation/sn/demand/bridge"
	"github.com/urfoundation/sn/demand/pricesheet"
	"github.com/urfoundation/sn/demand/solana"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// stDemandVaultBytes reads vault demand.yml; tests replace it.
var stDemandVaultBytes = func() ([]byte, error) {
	resource, err := server.Vault.SimpleResource(demand.ResourceName)
	if err != nil {
		return nil, err
	}
	return resource.BytesE()
}

var stDemandConfigLock sync.Mutex
var stDemandConfigKnown bool
var stDemandConfigValue *demand.Config

// stDemandConfigOnce keeps one answer per process, so a change takes effect at
// restart: the parsed file, or that the vault proved it absent. A lookup that
// could not prove its absence, a failed read and a file that does not parse are
// each an error, and are read again next time.
func stDemandConfigOnce() (*demand.Config, error) {
	stDemandConfigLock.Lock()
	defer stDemandConfigLock.Unlock()
	if stDemandConfigKnown {
		return stDemandConfigValue, nil
	}
	raw, err := stDemandVaultBytes()
	var wallets *demand.Config
	if err != nil {
		if !errors.Is(err, server.ErrResourceNotFound) || errors.Is(err, server.ErrResourceUnavailable) {
			return nil, fmt.Errorf("st: read %s: %w", demand.ResourceName, err)
		}
	} else if wallets, err = demand.ParseConfig(raw); err != nil {
		return nil, err
	}
	stDemandConfigKnown, stDemandConfigValue = true, wallets
	return wallets, nil
}

// stDemandConfig is the parsed demand.yml; nil when the vault has none. Tests
// replace it.
var stDemandConfig = stDemandConfigOnce

// stDemandBinding requires the alpha wallet to be the deposit signer on the
// deposit hotkey st.yml names: any other account's stake cannot pay a deposit.
func stDemandBinding(cfg *StConfig, wallets *demand.Config) error {
	if cfg == nil || wallets == nil || cfg.DepositKey == nil {
		return errors.New("st: demand deposit wallets are not bound to a deposit signer")
	}
	if wallets.AlphaAddress != crypto.PubkeyToAddress(cfg.DepositKey.PublicKey) {
		return fmt.Errorf("st: %s alpha wallet %s is not the deposit signer", demand.ResourceName, wallets.AlphaAddress)
	}
	if wallets.Hotkey != cfg.DepositHotkey {
		return fmt.Errorf("st: %s hotkey is not the deposit hotkey", demand.ResourceName)
	}
	return nil
}

// stDemandSolanaRpcUrl is the operator's Solana endpoint, which carries its key
// and so never reaches a log. A helius.yml that does not parse is an error, not a
// panic in the deposit task, and its parse error, which may quote the file, is not
// repeated.
func stDemandSolanaRpcUrl() (string, error) {
	resource, err := server.Vault.SimpleResource("helius.yml")
	if err != nil {
		return "", fmt.Errorf("st: the demand deposit funder needs helius.yml: %w", err)
	}
	if _, err := resource.ParseE(); err != nil {
		return "", errors.New("st: helius.yml does not parse")
	}
	keys := resource.String("helius", "api_key")
	if len(keys) != 1 || keys[0] == "" {
		return "", errors.New("st: helius.yml has no api_key")
	}
	return "https://mainnet.helius-rpc.com/?api-key=" + keys[0], nil
}

// The text form of a URL, which a read error names and which may carry a
// provider's key in its path or query, and a key without its URL.
var (
	stDemandUrlText = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>\\]+`)
	stDemandKeyText = regexp.MustCompile(`(?i)\b(api[-_]?key|apikey|access[-_]?token|token|secret|password)=[^\s&"'<>]+`)
)

// stScrubEndpoints leaves only each URL's scheme and host, as solana.Redact does
// for its own endpoint. An RPC read's error names the endpoint it tried
// (st_read_owner.go), so a keyed endpoint would otherwise reach a publish row, a
// log or the status command.
func stScrubEndpoints(text string) string {
	text = stDemandUrlText.ReplaceAllStringFunc(text, func(found string) string {
		return solana.Redact(strings.TrimRight(found, ".,;:)"))
	})
	return stDemandKeyText.ReplaceAllString(text, "${1}=(redacted)")
}

// stScrubEventData is an event's data with no endpoint in any of its text.
func stScrubEventData(data map[string]any) map[string]any {
	scrubbed := make(map[string]any, len(data))
	for key, value := range data {
		if text, ok := value.(string); ok {
			value = stScrubEndpoints(text)
		}
		scrubbed[key] = value
	}
	return scrubbed
}

// stDemandEvm reads Bittensor EVM as the alpha wallet, over the server's verified
// endpoints.
type stDemandEvm struct {
	core *CoreStClient
	from common.Address
}

func (self *stDemandEvm) Call(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
	var out []byte
	err := self.core.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		value, err := client.CallContract(callCtx, ethereum.CallMsg{From: self.from, To: &to, Data: data}, nil)
		if err != nil {
			return err
		}
		out = value
		return nil
	})
	return out, err
}

func (self *stDemandEvm) Balance(ctx context.Context, account common.Address) (*big.Int, error) {
	var out *big.Int
	err := self.core.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		value, err := client.BalanceAt(callCtx, account, nil)
		if err != nil {
			return err
		}
		out = value
		return nil
	})
	return out, err
}

func (self *stDemandEvm) PendingNonce(ctx context.Context, account common.Address) (uint64, error) {
	var out uint64
	err := self.core.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		value, err := client.PendingNonceAt(callCtx, account)
		if err != nil {
			return err
		}
		out = value
		return nil
	})
	return out, err
}

// errStDemandUnsettled marks a durable send that stopped before its prepare ran:
// reconciling the wallet's earlier transactions, or the admissions after them, did
// not finish. Ensure settles first on every step, so this reaches it for any wallet,
// and it says nothing about whether the wallet can be funded.
var errStDemandUnsettled = errors.New("st: the deposit key's earlier transactions have not settled")

// stDemandSender sends the alpha wallet's transactions through the durable
// sender. Its reconciliation finishes before prepare runs, so prepare sizes from
// balances that earlier sends have already changed.
type stDemandSender struct {
	core *CoreStClient
	key  *ecdsa.PrivateKey
}

// stDemandSendContextKey carries the attempt's own context into the durable sender. The
// conversion runs under a deadline at the staging window's close (fundDemandDeposit),
// which bounds the funder's decisions and its calls out. A send's database work must
// outlive it: the engine's database calls panic on a context that has ended, and an
// intent half recorded would stop the deposit key.
type stDemandSendContextKey struct{}

func (self *stDemandSender) SendPrepared(ctx context.Context, prepare demand.Prepare) (hash string, err error) {
	sendCtx := ctx
	if attempt, ok := ctx.Value(stDemandSendContextKey{}).(context.Context); ok {
		sendCtx = attempt
	}
	prepared := false
	// A panic out of the engine, as from a database that fails mid-send, is a wallet
	// whose transactions have not settled: the next attempt reconciles them.
	defer func() {
		if r := recover(); r != nil {
			hash, err = "", fmt.Errorf("%w: %s", errStDemandUnsettled, stScrubEndpoints(fmt.Sprint(r)))
		}
	}()
	hash, err = self.core.sendPrepared(sendCtx, self.key, func(ctx context.Context, _ *ethclient.Client) (string, common.Address, []byte, error) {
		prepared = true
		return prepare(ctx)
	})
	if err != nil && !prepared {
		return hash, fmt.Errorf("%w: %w", errStDemandUnsettled, err)
	}
	return hash, err
}

// The demand deposit's alerts. Each is a log line like the other settlement alerts
// (stLedgerV2AlertSink) and a durable event, which is what the monitor pages on
// (subnet-demand-event), since a log line by itself reaches nobody. The kinds fit
// st_demand_event.kind, and the monitor names them too.
const (
	// The funder's own event: USDC left the wallet and no TAO ever showed for it.
	stDemandEventSwapNeedsPerson = "swap_needs_person"
	// A line the funder marks ALERT: USDC has left the wallet and no TAO has come yet.
	stDemandEventFunderAlert = "funder_alert"
	// A dry run says the deposit hotkey cannot take stake, which nothing expects of
	// an associated hotkey.
	stDemandEventHotkeyNotReady = "hotkey_not_ready"
	// stDemandAlertRepeat spaces one alert's repeats: the funder's line recurs on every
	// poll of its wait. A refusal retries slower than this, so its event recurs for as
	// long as the refusal lasts, and the monitor's page lasts as long.
	stDemandAlertRepeat = 10 * time.Minute
)

var stDemandAlerted sync.Map

// stDemandRecordAlert writes an alert's event on a context of its own, since a
// funder's log line carries none. Accounting never stops the money, so a failed write
// is only logged. Tests replace it.
var stDemandRecordAlert = func(deploymentKey model.StDeploymentKey, at time.Time, epoch uint64, kind string, data map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), stPublishResolveTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]demand deposit alert %s has no event: %s\n", kind, stScrubEndpoints(fmt.Sprint(r)))
		}
	}()
	model.AddStDemandEvent(ctx, deploymentKey, at, epoch, kind, data)
}

// stDemandAlert raises one alert, at most once per stDemandAlertRepeat.
func stDemandAlert(cfg *StConfig, epoch uint64, kind string, message string) {
	message = stScrubEndpoints(message)
	now := server.NowUtc()
	key := string(cfg.DeploymentKey()) + "|" + kind + "|" + message
	if last, ok := stDemandAlerted.Load(key); ok && now.Sub(last.(time.Time)) < stDemandAlertRepeat {
		return
	}
	stDemandAlerted.Store(key, now)
	stLedgerV2AlertSink("demand_"+kind, fmt.Sprintf("epoch %d", epoch), message)
	stDemandRecordAlert(cfg.DeploymentKey(), now, epoch, kind, map[string]any{"message": message})
}

// stDemandFunderLog is the funder's log. A line it marks ALERT also raises an alert.
func stDemandFunderLog(cfg *StConfig) func(string, ...any) {
	return func(format string, args ...any) {
		line := stScrubEndpoints(fmt.Sprintf(format, args...))
		glog.Infof("%s\n", line)
		if strings.Contains(format, " ALERT ") {
			var epoch uint64
			fmt.Sscanf(line, "[demand]epoch %d", &epoch)
			stDemandAlert(cfg, epoch, stDemandEventFunderAlert, line)
		}
	}
}

// stDemandStore keeps the funder's state in the database. A database failure is
// an error to the funder, which stops the step before anything is sent, and never a
// panic through a conversion that has journaled a send.
type stDemandStore struct {
	deploymentKey model.StDeploymentKey
}

func stDemandStoreRecover(operation string, err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("st: demand state %s: %v", operation, r)
	}
}

func (self *stDemandStore) Load(ctx context.Context) (state *demand.State, version uint64, err error) {
	defer stDemandStoreRecover("load", &err)
	raw, version := model.GetStDemandState(ctx, self.deploymentKey)
	state, err = demand.UnmarshalState(raw)
	return state, version, err
}

func (self *stDemandStore) Save(ctx context.Context, state *demand.State, version uint64) (next uint64, err error) {
	defer stDemandStoreRecover("save", &err)
	raw, err := demand.MarshalState(state)
	if err != nil {
		return 0, err
	}
	next, ok := model.SaveStDemandState(ctx, self.deploymentKey, raw, version)
	if !ok {
		return 0, fmt.Errorf("%w: version %d", demand.ErrStale, version)
	}
	return next, nil
}

func (self *stDemandStore) Record(ctx context.Context, event demand.Event) (err error) {
	defer stDemandStoreRecover("record", &err)
	data := stScrubEventData(event.Data)
	// Before the write, so a database that refuses it cannot lose the alert as well.
	if event.Kind == stDemandEventSwapNeedsPerson {
		stLedgerV2AlertSink("demand_"+event.Kind, fmt.Sprintf("epoch %d", event.Epoch), fmt.Sprintf("USDC left the wallet and no TAO came for it, and this needs a person; its budget stays spent and the conversion goes on without it: %v", data))
	}
	model.AddStDemandEvent(ctx, self.deploymentKey, event.Time, event.Epoch, event.Kind, data)
	return nil
}

// stDemandPayer is what makes the alpha wallet able to pay. *demand.Funder is it.
type stDemandPayer interface {
	Ensure(ctx context.Context, request demand.Request) (*demand.Result, error)
}

// demandFunder is the funder for this client's deployment; nil, nil when the
// vault has no demand.yml.
func (self *CoreStClient) demandFunder() (*demand.Funder, error) {
	wallets, err := stDemandConfig()
	if err != nil {
		return nil, err
	}
	if wallets == nil {
		return nil, nil
	}
	if err := stDemandBinding(self.cfg, wallets); err != nil {
		return nil, err
	}
	if self.cfg.Netuid == 0 || self.cfg.Netuid > 65535 {
		return nil, errors.New("st: the demand deposit funder needs a netuid")
	}
	endpoint, err := stDemandSolanaRpcUrl()
	if err != nil {
		return nil, err
	}
	return demand.NewFunder(wallets, uint16(self.cfg.Netuid), demand.Deps{
		Store:  &stDemandStore{deploymentKey: self.cfg.DeploymentKey()},
		EVM:    &stDemandEvm{core: self, from: wallets.AlphaAddress},
		Sender: &stDemandSender{core: self, key: wallets.AlphaKey()},
		Solana: solana.NewClient(endpoint),
		Logf:   stDemandFunderLog(self.cfg),
	})
}

const (
	// stDepositSoonDelay spaces the attempts of a deposit waiting on something
	// under way: its source totals, or a conversion in flight.
	stDepositSoonDelay = 1 * time.Minute
	// stDepositPriceGuardDelay spaces the attempts of a conversion the price guard
	// refused. That is a passing market condition, not a wallet to refill, and the
	// pool has no weight while the deposit is unpaid.
	stDepositPriceGuardDelay = 5 * time.Minute
	// stDepositPriceGuardAlertAfter is how long the guard may refuse an epoch's
	// conversion before that is worth anyone's attention.
	stDepositPriceGuardAlertAfter = 3 * time.Hour
	// stDemandSweepHoldLeadBlocks is how long before the sweep epoch's end no new
	// conversion starts, in blocks of 12 seconds: Ensure's 12 minutes and one send's
	// 8, so that no conversion started before the hold is still sending at the
	// boundary.
	stDemandSweepHoldLeadBlocks = 100
)

// stDepositSoon is a deposit waiting on something under way: it retries shortly.
func stDepositSoon(reason string) *StPublishOutcome {
	return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: reason, Retry: true, RetryAt: server.NowUtc().Add(stDepositSoonDelay)}
}

// stDepositBelowMinimum is the terminal outcome of a deposit, or of what is left to
// stage of it, below the native transfer minimum. It is the same outcome wherever
// it is found: nothing is sent and nothing retries until the next epoch.
func stDepositBelowMinimum(epoch uint64, err error) *StPublishOutcome {
	reason := stScrubEndpoints(err.Error())
	glog.Errorf("[st]epoch %d DEMAND DEPOSIT REFUSED FOR THE EPOCH, validators will zero the pool: %s\n", epoch, reason)
	return &StPublishOutcome{Status: model.StPublishStatusRefused, Reason: reason}
}

// stDemandPriceGuardWatch is how long the price guard has been refusing one
// epoch's conversion, in this process. A restart only delays the alert.
type stDemandPriceGuardWatch struct {
	stateLock sync.Mutex
	epoch     uint64
	since     time.Time
	alerted   bool
}

// refused notes a refusal and reports how long they have lasted and whether this
// is the one that should raise the alert.
func (self *stDemandPriceGuardWatch) refused(epoch uint64, now time.Time) (time.Duration, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.since.IsZero() || self.epoch != epoch {
		self.epoch, self.since, self.alerted = epoch, now, false
	}
	lasted := now.Sub(self.since)
	alert := !self.alerted && stDepositPriceGuardAlertAfter <= lasted
	self.alerted = self.alerted || alert
	return lasted, alert
}

// passed ends the watch once the guard is no longer what stops the conversion.
func (self *stDemandPriceGuardWatch) passed() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.since = time.Time{}
}

var stDemandPriceGuard = &stDemandPriceGuardWatch{}

// stDepositPriceGuarded is a conversion the price guard refused: it retries in
// minutes and alerts only once the refusal has lasted for hours.
func stDepositPriceGuarded(epoch uint64, err error) *StPublishOutcome {
	reason := stScrubEndpoints(err.Error())
	lasted, alert := stDemandPriceGuard.refused(epoch, server.NowUtc())
	if alert {
		glog.Errorf("[st]epoch %d DEMAND DEPOSIT UNPAID, the price guard has refused its conversion for %s: %s\n", epoch, lasted.Round(time.Minute), reason)
	} else {
		glog.Infof("[st]epoch %d fund demand deposit: the price guard refused the conversion, retrying: %s\n", epoch, reason)
	}
	return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: "fund demand deposit: " + reason, Retry: true, RetryAt: server.NowUtc().Add(stDepositPriceGuardDelay)}
}

// stDemandSweepHeld reports whether funding waits for the sweep's v1 close: from
// shortly before the sweep epoch's end until the close is confirmed or its grace
// has passed. latest is the latest block, which decides imminence as it does for
// the sweep's own quiet rule.
func stDemandSweepHeld(closeConfirmed bool, latest, end, grace uint64) bool {
	if closeConfirmed || latest+stDemandSweepHoldLeadBlocks < end {
		return false
	}
	return latest <= end+grace
}

// demandSweepHold says why a conversion must wait for the configured sweep's v1
// close, or "" when it need not. The close is sent by this same deposit key and fails
// after stV1SweepMaximumCloseAttempts, each of which reconciling a conversion's
// transaction in flight can use up, and it has no second chance inside its grace. It
// holds only the conversion: staging and the deposit of alpha the wallet already holds
// were sent by this key alongside the close before there were conversions. An unknown
// answer is an error: the conversion waits.
func (self *CoreStClient) demandSweepHold(ctx context.Context) (string, error) {
	epoch, sweep := stLedgerV1SweepEpoch(ctx, self.cfg)
	if !sweep || self.coordinator == nil {
		return "", nil
	}
	publishes := stV1SweepPublishes(ctx, self.cfg, epoch)
	if _, concluded := stV1SweepConcluded(publishes); concluded {
		return "", nil
	}
	confirmed := false
	for _, publish := range publishes {
		confirmed = confirmed || (publish.Kind == StPublishKindV1SweepClose && publish.Status == model.StPublishStatusConfirmed)
	}
	if confirmed {
		return "", nil
	}
	e := new(big.Int).SetUint64(epoch)
	end, err := stViewAt(self, ctx, self.cfg.ContractAddress, self.coordinator.PackEpochEndBlock(e), self.coordinator.UnpackEpochEndBlock)
	if err != nil || end == nil || !end.IsUint64() {
		return "", fmt.Errorf("st: the sweep epoch's end: %v", err)
	}
	policy, err := stViewAt(self, ctx, self.cfg.ContractAddress, self.coordinator.PackPolicyAt(e), self.coordinator.UnpackPolicyAt)
	if err != nil {
		return "", fmt.Errorf("st: the sweep epoch's policy: %w", err)
	}
	latest, err := self.LedgerV2LatestBlock(ctx)
	if err != nil {
		return "", err
	}
	if stDemandSweepHeld(false, latest, end.Uint64(), policy.CloseGraceBlocks) {
		return fmt.Sprintf("held for the sweep's v1 close of epoch %d, until it is confirmed or block %d", epoch, end.Uint64()+policy.CloseGraceBlocks), nil
	}
	return "", nil
}

// settleDeposits finishes the deposit key's earlier transactions and sends nothing.
func (self *CoreStClient) settleDeposits(ctx context.Context) error {
	if self.demandSettle != nil {
		return self.demandSettle(ctx)
	}
	_, err := self.sendPrepared(ctx, self.cfg.DepositKey, func(context.Context, *ethclient.Client) (string, common.Address, []byte, error) {
		return "", common.Address{}, nil, nil
	})
	return err
}

// retainedDepositFunding reports a retained staging intent of this deposit nonce
// that staging will refuse to follow with another transfer (stageDepositPrincipal).
func (self *CoreStClient) retainedDepositFunding(ctx context.Context, epoch uint64, nonce *big.Int) (retained bool, err error) {
	if self.demandRetained != nil {
		return self.demandRetained(ctx, epoch, nonce)
	}
	logicalKey, err := stTransactionLogicalKey(self.cfg, fmt.Sprintf("deposit-fund:%d:%d:%s", epoch, self.cfg.NoId, nonce))
	if err != nil {
		return false, err
	}
	defer func() {
		if r := recover(); r != nil {
			retained, err = false, fmt.Errorf("st: transaction intent read: %v", r)
		}
	}()
	prior := model.GetStTransactionIntent(ctx, logicalKey)
	return prior != nil && prior.Status != model.StTxReverted, nil
}

// fundDemandDeposit makes the alpha wallet able to stage amount, converting USDC
// if it must. It returns nil when the deposit can go on, and otherwise the outcome
// that stops it for now. The deposit's own limits are checked first, so USDC is
// never spent on a deposit the caps or the policy would refuse, and every read that
// decides to spend comes after the wallet's earlier transactions have finished, in
// one custody snapshot. Ensure settles them too, but only after its request is sized:
// a staging transfer finishing there would lower alpha under a stale NeedRao, and
// USDC would buy all of it again.
func (self *CoreStClient) fundDemandDeposit(ctx context.Context, epoch uint64, amount *big.Int) *StPublishOutcome {
	refuse := func(reason string) *StPublishOutcome {
		reason = stScrubEndpoints(reason)
		glog.Errorf("[st]epoch %d fund demand deposit: %s\n", epoch, reason)
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: "fund demand deposit: " + reason, Retry: true}
	}
	wait := func(reason string) *StPublishOutcome {
		reason = stScrubEndpoints(reason)
		glog.Infof("[st]epoch %d fund demand deposit: %s\n", epoch, reason)
		return stDepositSoon("fund demand deposit: " + reason)
	}
	payer := self.demandPayer
	if payer == nil {
		funder, err := self.demandFunder()
		if err != nil {
			// Staging decides whether the wallet can pay without a conversion.
			glog.Errorf("[st]epoch %d THE DEMAND DEPOSIT FUNDER IS UNAVAILABLE, nothing can convert until it is fixed: %s\n", epoch, stScrubEndpoints(err.Error()))
			return nil
		}
		if funder == nil {
			return nil
		}
		payer = funder
	}
	if err := self.settleDeposits(ctx); err != nil {
		return wait(fmt.Sprintf("settle earlier transactions: %s", err))
	}
	var custody *stDepositCustody
	err := self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		state, err := self.readDepositCustody(ctx, client, &epoch)
		if err != nil {
			return err
		}
		custody = state
		return nil
	})
	// The read with this epoch is also what keeps every epoch but the chain's current
	// one from reaching Ensure, on the manual path too: a budget moved to a future
	// epoch would stop conversions until the chain got there, and nothing resets it. A
	// failed read refuses nothing, and retries soon, but it can also be a signer or
	// hotkey that differs from the operator's, so it is logged as an error.
	if err != nil {
		reason := stScrubEndpoints(err.Error())
		glog.Errorf("[st]epoch %d fund demand deposit: custody: %s\n", epoch, reason)
		return stDepositSoon("fund demand deposit: " + reason)
	}
	if err := custody.validatePrincipal(self.cfg, amount); err != nil {
		if errors.Is(err, errStDepositAlreadyCredited) {
			return nil // staging reports it
		}
		if errors.Is(err, errStDepositBelowRuntimeMinimum) {
			return stDepositBelowMinimum(epoch, err)
		}
		return refuse(err.Error())
	}
	need, err := stDepositFundingAmount(amount, custody.staged)
	if err != nil {
		return refuse(err.Error())
	}
	// Staging's own admission test, at this one snapshot.
	if need.Sign() == 0 || custody.source.Cmp(need) >= 0 {
		return nil
	}
	if err := custody.validateTransfer(need); err != nil {
		if errors.Is(err, errStDepositBelowRuntimeMinimum) {
			return stDepositBelowMinimum(epoch, err)
		}
		return refuse(err.Error())
	}
	retained, err := self.retainedDepositFunding(ctx, epoch, custody.nonce)
	if err != nil {
		return refuse(err.Error())
	}
	if retained {
		return refuse("a retained deposit funding intent left the staging short, and staging will not send another; nothing converts")
	}
	// What it buys could not be staged in time this epoch, and no conversion step
	// starts after the staging window closes: the funder's journals are safe to stop
	// at any point, and the next epoch's run resumes them.
	closes, err := self.depositStagingCloses(ctx, epoch, custody.deadline)
	if err != nil {
		return wait(err.Error())
	}
	// Only a conversion waits for the sweep's close. Staging and the deposit of alpha
	// the wallet already holds go on as they always did.
	sweepHold := self.demandSweepHold
	if self.demandHold != nil {
		sweepHold = self.demandHold
	}
	if hold, err := sweepHold(ctx); err != nil {
		return wait(err.Error())
	} else if hold != "" {
		return wait(hold)
	}
	ensureCtx, cancelEnsure := context.WithDeadline(context.WithValue(ctx, stDemandSendContextKey{}, ctx), closes)
	defer cancelEnsure()
	result, err := payer.Ensure(ensureCtx, demand.Request{Epoch: epoch, NeedRao: need})
	if errors.Is(err, demand.ErrPriceGuard) {
		return stDepositPriceGuarded(epoch, err)
	}
	if errors.Is(err, demand.ErrStale) || stDemandStillUnderWay(err) {
		return wait(err.Error())
	}
	stDemandPriceGuard.passed()
	switch {
	case err == nil:
	case errors.Is(err, demand.ErrEpochBudget) && self.depositEpochOver(ctx, epoch):
		// A newer epoch's attempt has moved the budget on, and a later attempt of this
		// one could only be skipped.
		reason := stScrubEndpoints(err.Error())
		glog.Infof("[st]epoch %d fund demand deposit: a stale run stops: %s\n", epoch, reason)
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: "fund demand deposit: " + reason}
	case errors.Is(err, demand.ErrHotkeyNotReady):
		stDemandAlert(self.cfg, epoch, stDemandEventHotkeyNotReady, err.Error())
		return refuse(err.Error())
	default:
		return refuse(err.Error())
	}
	if !result.Funded {
		return wait(fmt.Sprintf("waiting on %s", result.Waiting))
	}
	return nil
}

// stDemandStillUnderWay reports an error from Ensure that refuses nothing: the wallet's
// earlier transactions did not settle, a send is still waiting for its finality, or
// the run was stopped. The next attempt goes on from there, and may find the wallet
// already funded.
func stDemandStillUnderWay(err error) bool {
	return errors.Is(err, errStDemandUnsettled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// depositEpochOver reports whether the chain has moved past epoch. A read that fails
// knows nothing.
func (self *CoreStClient) depositEpochOver(ctx context.Context, epoch uint64) bool {
	over := false
	err := self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		state, err := self.readDepositCustody(ctx, client, nil)
		if err != nil {
			return err
		}
		over = epoch < state.boundary.Epoch
		return nil
	})
	return err == nil && over
}

// demandCallIntent reports whether intent is one of the conversion's own
// transactions from the alpha wallet: an unwrap of its wTAO or a stake of its TAO
// on its deposit hotkey. Neither moves value out of the wallet. Nothing broader
// is the deposit key's to sign. demand.yml gates only a new intent: a retained one
// already holds a nonce of the deposit key, whose later sends, the sweep's close
// among them, stop behind it, so it is admitted on its form against the deployment's
// own deposit hotkey whatever has become of the file since.
func (self *CoreStClient) demandCallIntent(intent *model.StTransactionIntent) bool {
	if intent == nil || self.cfg == nil || self.cfg.DepositKey == nil || self.cfg.Netuid == 0 || self.cfg.Netuid > 65535 {
		return false
	}
	if intent.IntentId == (server.Id{}) {
		wallets, err := stDemandConfig()
		if err != nil || wallets == nil || stDemandBinding(self.cfg, wallets) != nil {
			return false
		}
	}
	return strings.EqualFold(intent.FromAddress, crypto.PubkeyToAddress(self.cfg.DepositKey.PublicKey).Hex()) &&
		strings.EqualFold(intent.CalldataHash, crypto.Keccak256Hash(intent.Calldata).Hex()) &&
		intent.DeploymentKey == self.cfg.DeploymentKey() && intent.ChainId == self.cfg.ChainId &&
		strings.EqualFold(intent.GenesisHash, hexutil.Encode(self.cfg.GenesisHash[:])) &&
		intent.Profile == self.cfg.Profile && intent.DeploymentId == self.cfg.DeploymentId &&
		demand.AllowedAlphaWalletCall(common.HexToAddress(intent.ToAddress), intent.Calldata, bridge.WTAO, self.cfg.DepositHotkey, uint16(self.cfg.Netuid))
}

// stAddStakeLimitSelector is the conversion's stake. Its admission grows with the
// coldkey's StakingHotkeys list as the staging transferStake's does, which third
// parties can lengthen after the estimate, so it takes the same gas floor.
var stAddStakeLimitSelector = crypto.Keccak256([]byte("addStakeLimit(bytes32,uint256,uint256,bool,uint256)"))[:4]

func stDemandStakeCall(to common.Address, calldata []byte) bool {
	return to == stStakingPrecompileAddress && bytes.HasPrefix(calldata, stAddStakeLimitSelector)
}

// StDemandPriceEntry is one signed price of the sheet.
type StDemandPriceEntry struct {
	EffectiveEpoch uint64                 `json:"effective_epoch"`
	EpochCapRao    uint64                 `json:"epoch_cap_rao"`
	ZeroPrice      bool                   `json:"zero_price"`
	Tiers          []protocol.DepositTier `json:"tiers"`
}

// StDemandPriceStatus is the price in force for the current epoch, as the last
// verified sheet says. Reading it fetches nothing.
type StDemandPriceStatus struct {
	Epoch  uint64 `json:"epoch"`
	Signer string `json:"signer,omitempty"`
	// Source is "signed entry", "policy baseline", or "unknown" when the
	// deployment has a pinned signer and no verified sheet can be read.
	Source      string                 `json:"source"`
	ZeroPrice   bool                   `json:"zero_price"`
	EpochCapRao uint64                 `json:"epoch_cap_rao"`
	Tiers       []protocol.DepositTier `json:"tiers"`
	// InForce is the signed entry in force; nil under the policy baseline.
	InForce *StDemandPriceEntry `json:"in_force,omitempty"`
	// NextEntry is the next signed entry to take effect, if one is scheduled.
	NextEntry *StDemandPriceEntry `json:"next_entry,omitempty"`
	Problem   string              `json:"problem,omitempty"`
}

// StDemandUsageStatement is an epoch's recorded usage statement, or its captured
// users before the statement exists.
type StDemandUsageStatement struct {
	Epoch           uint64     `json:"epoch"`
	TotalUsageBytes uint64     `json:"total_usage_bytes"`
	TotalUsers      uint64     `json:"total_users"`
	ProviderRows    uint64     `json:"provider_rows"`
	ContentHash     string     `json:"content_hash"`
	StatementTime   *time.Time `json:"statement_time,omitempty"`
}

// StDemandStatusReport is the status command's report: the wallets as
// Funder.Status reads them, the price in force and the last usage statement.
type StDemandStatusReport struct {
	*demand.WalletStatus
	WalletsError       string                  `json:"wallets_error,omitempty"`
	Price              *StDemandPriceStatus    `json:"price"`
	LastUsageStatement *StDemandUsageStatement `json:"last_usage_statement,omitempty"`
}

func stDemandPriceEntry(entry *pricesheet.Entry) *StDemandPriceEntry {
	if entry == nil {
		return nil
	}
	zeroPrice, _ := protocol.ValidateDepositTiers(entry.Tiers)
	return &StDemandPriceEntry{EffectiveEpoch: entry.EffectiveEpoch, EpochCapRao: entry.EpochCapRao, ZeroPrice: zeroPrice, Tiers: entry.Tiers}
}

// stDemandPriceStatusAt reports the price in force for an epoch from a sheet read
// by the caller: nil with a problem when the price is unknown.
func stDemandPriceStatusAt(cfg *StConfig, epoch uint64, signer string, sheet *pricesheet.Sheet, problem error) *StDemandPriceStatus {
	status := &StDemandPriceStatus{Epoch: epoch, Signer: signer}
	if problem != nil {
		status.Problem = stScrubEndpoints(problem.Error())
		if sheet == nil && signer != "" {
			status.Source = "unknown"
			return status
		}
	}
	policy := pricesheet.Effective(sheet, epoch, stDepositPolicy(cfg))
	status.ZeroPrice, status.EpochCapRao, status.Tiers = policy.IsZeroPrice(), policy.EpochCapRaoPerOperator, policy.Tiers
	status.Source = "policy baseline"
	if entry := sheet.At(epoch); entry != nil {
		status.Source = "signed entry"
		status.InForce = stDemandPriceEntry(entry)
	}
	if sheet != nil {
		for i := range sheet.Entries {
			if epoch < sheet.Entries[i].EffectiveEpoch {
				status.NextEntry = stDemandPriceEntry(&sheet.Entries[i])
				break
			}
		}
	}
	return status
}

// StDemandStatus reads the demand deposit wallets, what is under way, the price in
// force for this epoch and the last usage statement. It signs nothing and fetches
// nothing: the price is the last verified sheet's, which the deposit task refreshes.
func StDemandStatus(ctx context.Context) (*StDemandStatusReport, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	core, ok := client.(*CoreStClient)
	if !ok {
		return nil, errors.New("st: demand deposits need the chain client")
	}
	state, err := client.Epoch(ctx)
	if err != nil {
		return nil, errors.New(stScrubEndpoints(err.Error()))
	}
	report := &StDemandStatusReport{}
	signer := ""
	sheet, problem := (*pricesheet.Sheet)(nil), error(nil)
	if address, pinned := stDemandSheetSignerFor(cfg); pinned {
		signer = address.Hex()
		sheet, problem = stDemandStoredSheet(ctx, cfg)
	}
	report.Price = stDemandPriceStatusAt(cfg, state.PendingEpoch, signer, sheet, problem)
	if cfg.LedgerV2Enabled() {
		latest, err := stDemandReadLatestStatement(ctx, cfg)
		if err != nil {
			return nil, errors.New(stScrubEndpoints(err.Error()))
		}
		if latest != nil {
			report.LastUsageStatement = &StDemandUsageStatement{Epoch: latest.Epoch, TotalUsageBytes: latest.TotalUsageBytes, TotalUsers: latest.TotalUsers, ProviderRows: latest.ProviderRows, ContentHash: latest.ContentHash, StatementTime: latest.StatementTime}
		}
	}
	funder, err := core.demandFunder()
	switch {
	case err != nil:
		report.WalletsError = stScrubEndpoints(err.Error())
	case funder == nil:
		report.WalletsError = fmt.Sprintf("st: the vault has no %s", demand.ResourceName)
	default:
		wallets, err := funder.Status(ctx)
		if err != nil {
			report.WalletsError = stScrubEndpoints(err.Error())
			break
		}
		wallets.HotkeyError = stScrubEndpoints(wallets.HotkeyError)
		report.WalletStatus = wallets
	}
	return report, nil
}
