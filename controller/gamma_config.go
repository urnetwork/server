package controller

// Pay with Gamma: the vault file and the deposit hotkeys (mmm/BTCPAY.md §18).
//
// Everything Gamma needs is in one vault resource, read on each use (cached
// for a few seconds) so a vault fix takes effect without a restart:
//
//	vault/<env>/gamma.yml
//	  gamma:
//	    billing_key_seed: 0x...       # SECRET: the GammaSpend proxy key, an sr25519 mini-secret
//	    billing_address: 5...         # its SS58, checked against the seed
//	    deposit_hotkey_master: 5...   # the public key every deposit hotkey derives from
//	    spender: 5...                 # the stable spender every SN25 deposit names (seed offline)
//	    netuid: 25
//	    service_hotkey: 5CyQ...     # the SN25 owner hotkey the service is published under
//	    services_url: https://api.bringyour.com/pay/gamma/services.json
//	    rpc_urls: []                # Finney JSON-RPC; empty = st.yml's subtensor gateway
//	    max_fee_rao: 5000000        # the most one spend batch may pay in fees
//	    min_float_rao: 1000000      # the least the billing key must hold to bill
//
// A missing or incomplete file is Gamma off ("not_configured"), never a boot
// failure. Errors name the resource and the key, never a value.
//
// Deposit hotkeys: the n-th hotkey we assign (n counted over every account,
// from 0) is the deposit master key soft-derived with the junction /n: the
// public key of <seed>/n, which any Substrate tool (subkey, btcli
// regen-hotkey --uri, the Python SDK's Keypair.create_from_uri) reproduces
// from the seed. A soft junction derives from the public key alone, so no
// host holds the seed: it stays in vault/<env>/gamma_keys.yml, which no
// service reads, and only recovers TAO sent to a deposit hotkey by mistake.
// The keys have no power over gamma: a deposit belongs to the coldkey that
// mints into it, and only the spender spends. An account gets a second
// hotkey when a payer's coldkey cannot open a deposit at its first (another
// coldkey owns that deposit or the hotkey).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/vedhavyas/go-subkey/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	stconn "github.com/urnetwork/server/st"
)

const gammaVaultResource = "gamma.yml"

// The defaults for the policy keys gamma.yml may leave out.
const (
	gammaDefaultNetuid      = 25
	gammaDefaultMaxFeeRao   = 5_000_000
	gammaDefaultMinFloatRao = 1_000_000
	// an intent nothing was charged for ends after this
	gammaDefaultIntentTtl = 24 * time.Hour
	// a spend batch's era: 64 blocks, about 13 minutes
	gammaDefaultEraPeriod = 64
	// the most finalized blocks one biller run reads
	gammaDefaultMaxBlocksPerRun = 200
	// the biller runs this often
	gammaDefaultRunInterval = 12 * time.Second
	// the automatic renewal, top-up and seat checks run this often
	gammaDefaultAutoInterval = 5 * time.Minute
	// the most deposit hotkeys one account is given
	gammaMaxDepositHotkeysPerNetwork = 8
)

type gammaYaml struct {
	Gamma struct {
		BillingKeySeed      string   `yaml:"billing_key_seed"`
		BillingAddress      string   `yaml:"billing_address"`
		DepositHotkeyMaster string   `yaml:"deposit_hotkey_master"`
		Spender             string   `yaml:"spender"`
		Netuid              int      `yaml:"netuid"`
		ServiceHotkey       string   `yaml:"service_hotkey"`
		ServicesUrl         string   `yaml:"services_url"`
		RpcUrls             []string `yaml:"rpc_urls"`
		MaxFeeRao           uint64   `yaml:"max_fee_rao"`
		MinFloatRao         uint64   `yaml:"min_float_rao"`
		IntentTtl           string   `yaml:"intent_ttl"`
		EraPeriod           uint64   `yaml:"era_period"`
		MaxBlocksPerRun     uint64   `yaml:"max_blocks_per_run"`
		RunInterval         string   `yaml:"run_interval"`
	} `yaml:"gamma"`
}

// gammaSettings is gamma.yml, checked.
type gammaSettings struct {
	billingKey       signature.KeyringPair
	BillingPublicKey [32]byte
	BillingAddress   string
	// the public key every deposit hotkey derives from
	DepositHotkeyMaster [32]byte
	Spender             [32]byte
	SpenderAddress      string
	Netuid              uint16
	ServiceHotkey       [32]byte
	ServiceAddress      string
	ServicesUrl         string
	RpcUrls             []string
	MaxFeeRao           uint64
	MinFloatRao         uint64
	IntentTtl           time.Duration
	EraPeriod           uint64
	MaxBlocksPerRun     uint64
	RunInterval         time.Duration
}

// errGammaNotConfigured: gamma.yml is missing or incomplete.
var errGammaNotConfigured = errors.New("gamma is not configured")

func gammaSettingsFromVault() (*gammaSettings, error) {
	resource, err := server.Vault.SimpleResource(gammaVaultResource)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not present", errGammaNotConfigured, gammaVaultResource)
	}
	var y gammaYaml
	if err := resource.UnmarshalYamlE(&y); err != nil {
		// the parser's message can quote the file; it is not repeated
		return nil, fmt.Errorf("%w: %s does not parse", errGammaNotConfigured, gammaVaultResource)
	}
	return parseGammaSettings(&y)
}

func parseGammaSettings(y *gammaYaml) (*gammaSettings, error) {
	g := y.Gamma
	missing := func(key string) error {
		return fmt.Errorf("%w: %s: gamma.%s is missing", errGammaNotConfigured, gammaVaultResource, key)
	}
	invalid := func(key string) error {
		return fmt.Errorf("%w: %s: gamma.%s is not valid", errGammaNotConfigured, gammaVaultResource, key)
	}
	settings := &gammaSettings{}

	seed := strings.TrimSpace(g.BillingKeySeed)
	if seed == "" {
		return nil, missing("billing_key_seed")
	}
	billingKey, err := signature.KeyringPairFromSecret(seed, model.BittensorSS58Prefix)
	if err != nil || len(billingKey.PublicKey) != 32 {
		return nil, invalid("billing_key_seed")
	}
	settings.billingKey = billingKey
	copy(settings.BillingPublicKey[:], billingKey.PublicKey)
	settings.BillingAddress = billingKey.Address
	if address := strings.TrimSpace(g.BillingAddress); address != "" && address != settings.BillingAddress {
		return nil, fmt.Errorf("%w: %s: gamma.billing_address is not the address of gamma.billing_key_seed", errGammaNotConfigured, gammaVaultResource)
	}

	if strings.TrimSpace(g.DepositHotkeyMaster) == "" {
		return nil, missing("deposit_hotkey_master")
	}
	if settings.DepositHotkeyMaster, err = model.DecodeBittensorAddress(strings.TrimSpace(g.DepositHotkeyMaster)); err != nil {
		return nil, invalid("deposit_hotkey_master")
	}
	if _, _, err := gammaDepositHotkeyAt(settings.DepositHotkeyMaster, 0); err != nil {
		return nil, invalid("deposit_hotkey_master")
	}

	if strings.TrimSpace(g.Spender) == "" {
		return nil, missing("spender")
	}
	if settings.Spender, err = model.DecodeBittensorAddress(strings.TrimSpace(g.Spender)); err != nil {
		return nil, invalid("spender")
	}
	settings.SpenderAddress = strings.TrimSpace(g.Spender)
	if settings.Spender == settings.BillingPublicKey {
		// the spender's seed stays offline; the hot key may only be its proxy
		return nil, fmt.Errorf("%w: %s: gamma.spender must not be the billing key", errGammaNotConfigured, gammaVaultResource)
	}

	netuid := g.Netuid
	if netuid == 0 {
		netuid = gammaDefaultNetuid
	}
	if netuid <= 0 || 0xffff < netuid {
		return nil, invalid("netuid")
	}
	settings.Netuid = uint16(netuid)

	if strings.TrimSpace(g.ServiceHotkey) == "" {
		return nil, missing("service_hotkey")
	}
	if settings.ServiceHotkey, err = model.DecodeBittensorAddress(strings.TrimSpace(g.ServiceHotkey)); err != nil {
		return nil, invalid("service_hotkey")
	}
	settings.ServiceAddress = strings.TrimSpace(g.ServiceHotkey)
	if settings.ServiceHotkey == settings.Spender || settings.ServiceHotkey == settings.BillingPublicKey {
		return nil, invalid("service_hotkey")
	}

	settings.ServicesUrl = strings.TrimSpace(g.ServicesUrl)
	if settings.ServicesUrl == "" {
		return nil, missing("services_url")
	}
	if 256 < len(settings.ServicesUrl) || !strings.HasPrefix(settings.ServicesUrl, "https://") {
		// set_service takes an http(s) url of at most 256 bytes
		return nil, invalid("services_url")
	}

	for _, url := range g.RpcUrls {
		if url = strings.TrimSpace(url); url != "" {
			settings.RpcUrls = append(settings.RpcUrls, url)
		}
	}

	settings.MaxFeeRao = g.MaxFeeRao
	if settings.MaxFeeRao == 0 {
		settings.MaxFeeRao = gammaDefaultMaxFeeRao
	}
	settings.MinFloatRao = g.MinFloatRao
	if settings.MinFloatRao == 0 {
		settings.MinFloatRao = gammaDefaultMinFloatRao
	}
	settings.IntentTtl = gammaDefaultIntentTtl
	if g.IntentTtl != "" {
		ttl, err := time.ParseDuration(g.IntentTtl)
		if err != nil || ttl < time.Hour {
			return nil, invalid("intent_ttl")
		}
		settings.IntentTtl = ttl
	}
	settings.EraPeriod = g.EraPeriod
	if settings.EraPeriod == 0 {
		settings.EraPeriod = gammaDefaultEraPeriod
	}
	if _, err := gammaMortalEra(0, settings.EraPeriod); err != nil {
		return nil, invalid("era_period")
	}
	settings.MaxBlocksPerRun = g.MaxBlocksPerRun
	if settings.MaxBlocksPerRun == 0 {
		settings.MaxBlocksPerRun = gammaDefaultMaxBlocksPerRun
	}
	settings.RunInterval = gammaDefaultRunInterval
	if g.RunInterval != "" {
		interval, err := time.ParseDuration(g.RunInterval)
		if err != nil || interval < time.Second {
			return nil, invalid("run_interval")
		}
		settings.RunInterval = interval
	}
	return settings, nil
}

// Endpoints are the JSON-RPC endpoints to read and submit through: gamma.yml's
// own, else st.yml's subtensor gateway (the archive node).
func (self *gammaSettings) Endpoints() []string {
	if 0 < len(self.RpcUrls) {
		return self.RpcUrls
	}
	return gammaGatewayRpcUrls()
}

// The subtensor gateway's urls; none when st.yml or its host is not
// configured. Replaceable only by tests.
var gammaGatewayRpcUrls = func() (urls []string) {
	defer func() {
		if r := recover(); r != nil {
			urls = nil
		}
	}()
	return stconn.RpcUrls()
}

// The settings, cached for a few seconds so a request does not parse the
// file each time while a vault fix still takes effect quickly.
const gammaSettingsCacheTtl = 10 * time.Second

var gammaSettingsCacheLock sync.Mutex
var gammaSettingsCached *gammaSettings
var gammaSettingsCachedErr error
var gammaSettingsCachedAt time.Time

func gammaLoadSettings() (*gammaSettings, error) {
	gammaSettingsCacheLock.Lock()
	defer gammaSettingsCacheLock.Unlock()
	if !gammaSettingsCachedAt.IsZero() && time.Since(gammaSettingsCachedAt) < gammaSettingsCacheTtl {
		return gammaSettingsCached, gammaSettingsCachedErr
	}
	gammaSettingsCached, gammaSettingsCachedErr = gammaSettingsFunc()
	gammaSettingsCachedAt = time.Now()
	return gammaSettingsCached, gammaSettingsCachedErr
}

// The settings seam. Replaceable only by tests; production reads the vault.
var gammaSettingsFunc = gammaSettingsFromVault

// setGammaSettingsForTest swaps the settings for a test and returns the
// restore.
func setGammaSettingsForTest(settings *gammaSettings, err error) func() {
	previous := gammaSettingsFunc
	gammaSettingsFunc = func() (*gammaSettings, error) {
		return settings, err
	}
	gammaSettingsCacheLock.Lock()
	gammaSettingsCachedAt = time.Time{}
	gammaSettingsCacheLock.Unlock()
	return func() {
		gammaSettingsFunc = previous
		gammaSettingsCacheLock.Lock()
		gammaSettingsCachedAt = time.Time{}
		gammaSettingsCacheLock.Unlock()
	}
}

// gammaDepositHotkeyAt is the deposit hotkey at a derivation index: the
// master public key soft-derived with the junction /index (Substrate's
// DeriveJunction::soft of a number: its u64, little endian, zero padded to
// 32 bytes, as the chain code).
func gammaDepositHotkeyAt(master [32]byte, index int64) ([32]byte, string, error) {
	var publicKey [32]byte
	if index < 0 {
		return publicKey, "", fmt.Errorf("deposit hotkey index %d is negative", index)
	}
	parent, err := schnorrkel.NewPublicKey(master)
	if err != nil {
		return publicKey, "", errors.New("the deposit master is not an sr25519 public key")
	}
	var chainCode [32]byte
	binary.LittleEndian.PutUint64(chainCode[:8], uint64(index))
	extended, err := schnorrkel.DeriveKeySimple(parent, nil, chainCode)
	if err != nil {
		return publicKey, "", err
	}
	child, err := extended.Public()
	if err != nil {
		return publicKey, "", err
	}
	publicKey = child.Encode()
	return publicKey, subkey.SS58Encode(publicKey[:], model.BittensorSS58Prefix), nil
}
