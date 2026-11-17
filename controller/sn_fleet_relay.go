package controller

// The relay of the Top 200 claim: it pays for fleets within the device limit, so a
// casual miner's claim costs them no gas (BTEASY.md question 15), sends the one top-up
// (question 16) and sends a device's Leave. It holds its own keys, which own nothing
// else. They can't redirect anything: a binding is signed by the hotkey and the client,
// a revocation by the client, and a top-up pays only the coldkey it names.
//
// With no key configured, or too little in it, nothing is sent and nothing is lost: the
// rows wait. Each transaction's signed bytes are saved before it's sent, so a restart
// settles it from its receipt or sends the same bytes again, and nothing new goes out
// from a key until its earlier transaction is settled.

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	// the vault file of the relay's keys. Absent means no relay
	snFleetRelayVaultResource = "sn-fleet-relay.yml"
	// nothing is sent above this gas price: 4 times today's 5 gwei
	snFleetRelayMaxGasPriceWei = 20_000_000_000
	// a key below this is treated as not funded: about 3 binds at the gas price limit
	snFleetRelayMinBalanceWei = 16_000_000_000_000_000
	// how long a funded key's reads hold, so a burst of requests reads once
	snFleetRelayAvailableTtl = time.Minute
	// one pass's work: each transaction waits for finality
	snFleetRelayPassLimit = 16
)

type snFleetRelayKeysFile struct {
	BindingKey string `yaml:"binding_key"`
	TopUpKey   string `yaml:"top_up_key"`
}

// snFleetRelayer sends one queued transaction. What's durable is the model's: Send records
// the signed bytes through the model before they go out.
type snFleetRelayer interface {
	// a funded key stands behind this kind of payment
	Available(ctx context.Context, fromKey string) bool
	// signs and sends a queued row and waits for it. An error leaves the row queued
	// or, once its bytes were saved, sent
	Send(ctx context.Context, relay *model.SnFleetAutoRelay) (status string, relayError string, err error)
	// settles a sent row from its receipt, or sends its saved bytes again
	Settle(ctx context.Context, relay *model.SnFleetAutoRelay) (status string, relayError string, err error)
}

// The relayer: a variable so tests can supply their own.
var snFleetRelayerFn = func() snFleetRelayer {
	return snFleetCoreRelayerInstance()
}

type snFleetCoreRelayer struct {
	keys map[string]*ecdsa.PrivateKey

	stateLock sync.Mutex
	available map[string]bool
	expiry    time.Time
}

var snFleetCoreRelayerInstance = sync.OnceValue(func() *snFleetCoreRelayer {
	return &snFleetCoreRelayer{keys: snFleetRelayKeysFromVault()}
})

func snFleetParseRelayKey(value string) *ecdsa.PrivateKey {
	value = strings.TrimPrefix(strings.TrimSpace(value), "0x")
	if value == "" {
		return nil
	}
	key, err := crypto.HexToECDSA(value)
	if err != nil {
		// the key's text stays out of the log
		glog.Infof("[sn]fleet relay: a key in %s can't be read\n", snFleetRelayVaultResource)
		return nil
	}
	return key
}

// The relay's keys. A missing file or a key it doesn't hold is a relay that sends nothing.
func snFleetRelayKeysFromVault() (keys map[string]*ecdsa.PrivateKey) {
	keys = map[string]*ecdsa.PrivateKey{}
	defer func() {
		if recovered := recover(); recovered != nil {
			keys = map[string]*ecdsa.PrivateKey{}
		}
	}()
	resource, err := server.Vault.SimpleResource(snFleetRelayVaultResource)
	if err != nil {
		return keys
	}
	var file snFleetRelayKeysFile
	if err := yaml.Unmarshal(resource.Bytes(), &file); err != nil {
		return keys
	}
	if key := snFleetParseRelayKey(file.BindingKey); key != nil {
		keys[model.SnFleetAutoRelayKeyBinding] = key
	}
	if key := snFleetParseRelayKey(file.TopUpKey); key != nil {
		keys[model.SnFleetAutoRelayKeyTopUp] = key
	}
	return keys
}

// A key is usable while it holds enough and gas isn't priced above the ceiling. A read
// that fails is not usable.
func snFleetRelayKeyUsable(balanceWei *big.Int, gasPriceWei *big.Int) bool {
	return balanceWei != nil && gasPriceWei != nil &&
		0 <= balanceWei.Cmp(big.NewInt(snFleetRelayMinBalanceWei)) &&
		gasPriceWei.Cmp(big.NewInt(snFleetRelayMaxGasPriceWei)) <= 0
}

func (self *snFleetCoreRelayer) client(ctx context.Context) (*ethclient.Client, error) {
	cfg := stConfig()
	if cfg == nil || len(cfg.RpcUrls) == 0 {
		return nil, errors.New("the st subsystem has no rpc")
	}
	var failures []error
	for _, url := range cfg.RpcUrls {
		client, err := ethclient.DialContext(ctx, url)
		if err == nil {
			return client, nil
		}
		failures = append(failures, err)
	}
	return nil, errors.Join(failures...)
}

func (self *snFleetCoreRelayer) Available(ctx context.Context, fromKey string) bool {
	key := self.keys[fromKey]
	if key == nil {
		return false
	}
	now := server.NowUtc()
	self.stateLock.Lock()
	if now.Before(self.expiry) {
		if available, ok := self.available[fromKey]; ok {
			self.stateLock.Unlock()
			return available
		}
	}
	self.stateLock.Unlock()

	available := false
	callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
	defer cancel()
	if client, err := self.client(callCtx); err == nil {
		defer client.Close()
		balance, balanceErr := client.BalanceAt(callCtx, crypto.PubkeyToAddress(key.PublicKey), nil)
		price, priceErr := client.SuggestGasPrice(callCtx)
		if balanceErr == nil && priceErr == nil {
			available = snFleetRelayKeyUsable(balance, price)
			snFleetRelayBalance.WithLabelValues(fromKey).Set(snFleetWeiToTao(balance))
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.available == nil || !now.Before(self.expiry) {
		self.available = map[string]bool{}
		self.expiry = now.Add(snFleetRelayAvailableTtl)
	}
	self.available[fromKey] = available
	return available
}

func snFleetWeiToTao(wei *big.Int) float64 {
	value, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18)).Float64()
	return value
}

func (self *snFleetCoreRelayer) Send(ctx context.Context, relay *model.SnFleetAutoRelay) (string, string, error) {
	key := self.keys[relay.FromKey]
	cfg := stConfig()
	deploymentKey, ok := StDeploymentKey()
	if key == nil || cfg == nil || !ok {
		return model.SnFleetAutoRelayQueued, "", errors.New("the relay has no key")
	}
	if !self.Available(ctx, relay.FromKey) {
		return model.SnFleetAutoRelayQueued, "", errors.New("the relay's key isn't funded, or gas is over the limit")
	}
	value, parsed := new(big.Int).SetString(relay.ValueWei, 10)
	if !parsed {
		return model.SnFleetAutoRelayFailed, "the value can't be read", nil
	}
	from := strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
	saved := false
	receipt, err := onchain.SubmitWithHooks(ctx, onchain.SubmitParams{
		Contract:   common.HexToAddress(relay.ToAddress),
		Rpcs:       cfg.RpcUrls,
		Key:        key,
		Calldata:   relay.Calldata,
		ChainID:    new(big.Int).SetUint64(cfg.ChainId),
		Value:      value,
		NonceFloor: model.GetSnFleetAutoRelayNonceFloor(ctx, deploymentKey, from),
	}, onchain.SubmitHooks{
		Prepared: func(hash common.Hash, raw []byte) error {
			var signed types.Transaction
			if err := signed.UnmarshalBinary(raw); err != nil {
				return err
			}
			model.SetSnFleetAutoRelaySent(ctx, relay.RelayId, from, signed.Nonce(), strings.ToLower(hash.Hex()), raw)
			saved = true
			return nil
		},
	})
	switch {
	case err == nil && receipt != nil:
		return model.SnFleetAutoRelayDone, "", nil
	case err != nil && !saved && strings.Contains(err.Error(), "preflight eth_call failed"):
		// the coordinator refused it, and no gas was spent: a stale commitment, a binding
		// that overlaps, a lost UID. The next batch starts again from what's on chain
		return model.SnFleetAutoRelayFailed, err.Error(), nil
	case err != nil && saved && strings.Contains(err.Error(), "reverted on-chain"):
		return model.SnFleetAutoRelayFailed, err.Error(), nil
	case saved:
		return model.SnFleetAutoRelaySent, "", err
	default:
		return model.SnFleetAutoRelayQueued, "", err
	}
}

func (self *snFleetCoreRelayer) Settle(ctx context.Context, relay *model.SnFleetAutoRelay) (string, string, error) {
	callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
	defer cancel()
	client, err := self.client(callCtx)
	if err != nil {
		return model.SnFleetAutoRelaySent, "", err
	}
	defer client.Close()
	receipt, err := client.TransactionReceipt(callCtx, common.HexToHash(relay.TxHash))
	if err == nil {
		return snFleetRelayReceiptStatus(receipt)
	}
	if !errors.Is(err, ethereum.NotFound) {
		return model.SnFleetAutoRelaySent, "", err
	}
	// never seen, or seen and dropped. Past its nonce it can never land
	if relay.Nonce != nil && relay.FromAddress != "" {
		latest, err := client.NonceAt(callCtx, common.HexToAddress(relay.FromAddress), nil)
		if err != nil {
			return model.SnFleetAutoRelaySent, "", err
		}
		if *relay.Nonce < latest {
			return model.SnFleetAutoRelayFailed, "another transaction took its nonce", nil
		}
	}
	var signed types.Transaction
	if err := signed.UnmarshalBinary(relay.RawTransaction); err != nil {
		return model.SnFleetAutoRelayFailed, "its saved bytes can't be read", nil
	}
	if err := client.SendTransaction(callCtx, &signed); err != nil && !strings.Contains(err.Error(), "already known") {
		return model.SnFleetAutoRelaySent, "", err
	}
	return model.SnFleetAutoRelaySent, "", nil
}

func snFleetRelayReceiptStatus(receipt *types.Receipt) (string, string, error) {
	if receipt == nil {
		return model.SnFleetAutoRelaySent, "", errors.New("no receipt")
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return model.SnFleetAutoRelayFailed, "reverted on-chain", nil
	}
	return model.SnFleetAutoRelayDone, "", nil
}

// SnFleetAutoRelayStep works through the relay's open rows, oldest first. A row it can't
// settle stops the pass, so a key's transactions never go out of order.
func SnFleetAutoRelayStep(ctx context.Context) {
	if !snFleetAutoOpen() {
		return
	}
	deploymentKey, ok := StDeploymentKey()
	if !ok {
		return
	}
	relayer := snFleetRelayerFn()
	blocked := map[string]bool{}
	open := model.GetSnFleetAutoRelayOpen(ctx, deploymentKey, snFleetRelayPassLimit)
	snFleetRelayOpen.Set(float64(len(open)))
	for _, relay := range open {
		if blocked[relay.FromKey] {
			continue
		}
		var status, relayError string
		var err error
		if relay.Status == model.SnFleetAutoRelaySent {
			status, relayError, err = relayer.Settle(ctx, relay)
		} else {
			status, relayError, err = relayer.Send(ctx, relay)
		}
		if err != nil {
			// a key with no funds says so once a pass, for its rows to wait
			glog.Infof("[sn]fleet relay %s %s: %s\n", relay.Kind, relay.RelayId, err)
			blocked[relay.FromKey] = true
			continue
		}
		switch status {
		case model.SnFleetAutoRelayDone, model.SnFleetAutoRelayFailed:
			model.SetSnFleetAutoRelaySettled(ctx, relay.RelayId, status, relayError)
			snFleetRelaySettled.WithLabelValues(relay.Kind, status).Inc()
		default:
			// not yet: this key's later rows wait
			blocked[relay.FromKey] = true
		}
	}
}

// The calldata of a top-up: the chain's transfer precompile, transfer(bytes32).
func snFleetTopUpCalldata(coldkey [32]byte) ([]byte, error) {
	if coldkey == ([32]byte{}) {
		return nil, fmt.Errorf("a top-up names no coldkey")
	}
	calldata := crypto.Keccak256([]byte("transfer(bytes32)"))[:4]
	return append(calldata, coldkey[:]...), nil
}
