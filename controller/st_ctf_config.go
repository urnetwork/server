package controller

// st_ctf_config.go — st.yml's capture the flag block (sn CAPTURETHEFLAG.md
// §11.2). The driver is off unless the block turns it on: shadow builds each
// ended epoch's draw and ledger and publishes an unsigned report, and sends
// nothing; live drives the CTF pool from the epoch the signed amendment entry
// starts it (st_ctf_amendment.go). A block that does not decode or check turns
// capture the flag off and is reported, as a refused sweep block is
// (stV1SweepSelect); it never disables settlement.
//
//	ctf:
//	  mode: off | shadow | live
//	  carry_address: "0x…"             # STCtfCarry; live requires it
//	  pot_capture_alert_rao: 5000000000
//	  drand_urls: ["https://api.drand.sh", …]
//	  amendment_lines: ["UR settlement amendment v2: …"]

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/amendment"
)

// StCtfMode is the capture the flag driver's mode.
type StCtfMode string

const (
	// StCtfModeOff plans, reads and sends nothing.
	StCtfModeOff StCtfMode = "off"
	// StCtfModeShadow builds each ended epoch's census draw and ledger
	// without committing anything and publishes an unsigned report (§11.2,
	// step 6). It sends no transaction.
	StCtfModeShadow StCtfMode = "shadow"
	// StCtfModeLive commits the census, draws, builds, commits, finalizes
	// and recycles, from E_c.
	StCtfModeLive StCtfMode = "live"
)

// stCtfDefaultPotCaptureAlertRao is the pot share at or above which a capture
// is alerted when st.yml names no size: 5 α, the /ctf page's big pot (§9.2).
const stCtfDefaultPotCaptureAlertRao = 5_000_000_000

// stCtfDefaultDrandUrls are drand's public HTTP relays that serve quicknet.
// A pulse verifies against quicknet's key whatever relay serves it (§4.4).
var stCtfDefaultDrandUrls = []string{
	"https://api.drand.sh",
	"https://api2.drand.sh",
	"https://api3.drand.sh",
	"https://drand.cloudflare.com",
}

// StCtfConfig is the selected profile's capture the flag block.
type StCtfConfig struct {
	Mode StCtfMode
	// CarryAddress is the carry contract (§4.6). Live mode requires it, and
	// the signed amendment entry's carry must equal it, so two independent
	// sources agree before anything is sent to it.
	CarryAddress common.Address
	// PotCaptureAlertRao raises StCtfAlertPotCapture for every pot share at
	// or above it.
	PotCaptureAlertRao uint64
	// DrandUrls are the drand relays tried in order for a pulse.
	DrandUrls []string
	// AmendmentLines are configured amendment lines, unsigned, the fallback
	// of WHITEPAPER.md §8.6.7: a line stands in only for a kind with no signed
	// entry in force for the epoch.
	AmendmentLines []string
}

// On reports whether capture the flag runs in either mode.
func (self *StCtfConfig) On() bool {
	return self != nil && (self.Mode == StCtfModeShadow || self.Mode == StCtfModeLive)
}

// stCtfFile is the raw block.
type stCtfFile struct {
	Mode               string   `yaml:"mode"`
	CarryAddress       string   `yaml:"carry_address"`
	PotCaptureAlertRao *uint64  `yaml:"pot_capture_alert_rao"`
	DrandUrls          []string `yaml:"drand_urls"`
	AmendmentLines     []string `yaml:"amendment_lines"`
}

// stCtfSelect selects the profile's capture the flag block. A block that is
// refused leaves capture the flag off, says why in CtfConfigError and alerts;
// the rest of st keeps running.
func stCtfSelect(cfg *StConfig, profile string, node *yaml.Node) {
	ctf, err := stCtfParse(cfg, node)
	if err != nil {
		cfg.Ctf = StCtfConfig{Mode: StCtfModeOff}
		cfg.CtfConfigError = fmt.Sprintf("st.yml %s ctf block refused, capture the flag is off: %s", profile, err)
		stLedgerV2AlertSink(StCtfAlertConfig, profile, cfg.CtfConfigError)
		return
	}
	cfg.Ctf = ctf
}

func stCtfParse(cfg *StConfig, node *yaml.Node) (StCtfConfig, error) {
	ctf := StCtfConfig{Mode: StCtfModeOff, PotCaptureAlertRao: stCtfDefaultPotCaptureAlertRao, DrandUrls: append([]string(nil), stCtfDefaultDrandUrls...)}
	if node == nil || node.Kind == 0 {
		return ctf, nil
	}
	var file stCtfFile
	if err := node.Decode(&file); err != nil {
		return ctf, err
	}
	switch StCtfMode(file.Mode) {
	case "", StCtfModeOff:
		ctf.Mode = StCtfModeOff
	case StCtfModeShadow, StCtfModeLive:
		ctf.Mode = StCtfMode(file.Mode)
	default:
		return ctf, fmt.Errorf("mode %q is not off, shadow or live", file.Mode)
	}
	if file.CarryAddress != "" {
		if !common.IsHexAddress(file.CarryAddress) || common.HexToAddress(file.CarryAddress) == (common.Address{}) {
			return ctf, fmt.Errorf("carry_address %q is not a nonzero 0x address", file.CarryAddress)
		}
		ctf.CarryAddress = common.HexToAddress(file.CarryAddress)
	}
	if file.PotCaptureAlertRao != nil {
		if *file.PotCaptureAlertRao == 0 {
			return ctf, errors.New("pot_capture_alert_rao must be positive")
		}
		ctf.PotCaptureAlertRao = *file.PotCaptureAlertRao
	}
	if file.DrandUrls != nil {
		if len(file.DrandUrls) == 0 {
			return ctf, errors.New("drand_urls is empty")
		}
		ctf.DrandUrls = nil
		for _, raw := range file.DrandUrls {
			parsed, err := url.Parse(raw)
			if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				return ctf, fmt.Errorf("drand_urls %q is not an http(s) base url", raw)
			}
			ctf.DrandUrls = append(ctf.DrandUrls, strings.TrimSuffix(raw, "/"))
		}
	}
	ctf.AmendmentLines = append([]string(nil), file.AmendmentLines...)
	if !ctf.On() {
		return ctf, nil
	}
	if !cfg.LedgerV2Enabled() {
		return ctf, errors.New("capture the flag needs settlement_v2_address and vault_v2_address")
	}
	if _, ok := amendment.CtfPoolID(cfg.NoId); !ok {
		return ctf, fmt.Errorf("no_id %d has no capture the flag pool", cfg.NoId)
	}
	if cfg.Netuid == 0 || cfg.Netuid > 65535 {
		return ctf, fmt.Errorf("netuid %d is not a subnet", cfg.Netuid)
	}
	if _, err := amendment.ParseFallback(cfg.ChainId, uint16(cfg.Netuid), ctf.AmendmentLines); err != nil {
		return ctf, fmt.Errorf("amendment_lines: %w", err)
	}
	if ctf.Mode == StCtfModeLive && ctf.CarryAddress == (common.Address{}) {
		return ctf, errors.New("live mode needs carry_address")
	}
	return ctf, nil
}
