package controller

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"

	stconn "github.com/urnetwork/server/st"
)

func ctfTestYaml(t *testing.T, text string) yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(text), &node); err != nil {
		t.Fatal(err)
	}
	return *node.Content[0]
}

// §11.2: capture the flag is off unless st.yml's ctf block turns it on, and a
// refused block turns only capture the flag off.
func TestStCtfConfigIsOffByDefault(t *testing.T) {
	alerts := captureLedgerV2Alerts(t)
	cfg := ledgerV2TestConfig()
	cfg.Netuid = 25
	stCtfSelect(cfg, stconn.ProfileTestnet, &yaml.Node{})
	if cfg.Ctf.On() || cfg.Ctf.Mode != StCtfModeOff || cfg.CtfConfigError != "" || cfg.Ctf.PotCaptureAlertRao != stCtfDefaultPotCaptureAlertRao || len(cfg.Ctf.DrandUrls) != len(stCtfDefaultDrandUrls) {
		t.Fatalf("no block: %+v %q", cfg.Ctf, cfg.CtfConfigError)
	}
	node := ctfTestYaml(t, "mode: off\ncarry_address: \"0x00000000000000000000000000000000000000c7\"")
	stCtfSelect(cfg, stconn.ProfileTestnet, &node)
	if cfg.Ctf.On() || cfg.CtfConfigError != "" {
		t.Fatalf("mode off: %+v %q", cfg.Ctf, cfg.CtfConfigError)
	}
	if len(*alerts) != 0 {
		t.Fatalf("alerts %v", *alerts)
	}
}

func TestStCtfConfigSelectsModes(t *testing.T) {
	cfg := ledgerV2TestConfig()
	cfg.Netuid = 25
	node := ctfTestYaml(t, `
mode: shadow
pot_capture_alert_rao: 7
drand_urls: ["https://relay.example/", "http://10.0.0.1:8080"]
amendment_lines: ["`+ctfTestEntryLine(t, 8)+`"]
`)
	stCtfSelect(cfg, stconn.ProfileTestnet, &node)
	if cfg.CtfConfigError != "" || cfg.Ctf.Mode != StCtfModeShadow || cfg.Ctf.PotCaptureAlertRao != 7 || strings.Join(cfg.Ctf.DrandUrls, " ") != "https://relay.example http://10.0.0.1:8080" || len(cfg.Ctf.AmendmentLines) != 1 {
		t.Fatalf("shadow: %+v %q", cfg.Ctf, cfg.CtfConfigError)
	}
	node = ctfTestYaml(t, "mode: live\ncarry_address: \"0x00000000000000000000000000000000000000C7\"")
	stCtfSelect(cfg, stconn.ProfileTestnet, &node)
	if cfg.CtfConfigError != "" || cfg.Ctf.Mode != StCtfModeLive || cfg.Ctf.CarryAddress != ctfTestCarry {
		t.Fatalf("live: %+v %q", cfg.Ctf, cfg.CtfConfigError)
	}
}

func TestStCtfConfigRefusals(t *testing.T) {
	for _, test := range []struct {
		name, text, reason string
		mutate             func(*StConfig)
	}{
		{"mode", "mode: on", `mode "on" is not off, shadow or live`, nil},
		{"carry", "mode: shadow\ncarry_address: \"0x12\"", `carry_address "0x12" is not a nonzero 0x address`, nil},
		{"zero carry", "mode: shadow\ncarry_address: \"0x0000000000000000000000000000000000000000\"", "is not a nonzero 0x address", nil},
		{"live without carry", "mode: live", "live mode needs carry_address", nil},
		{"zero alert", "mode: shadow\npot_capture_alert_rao: 0", "pot_capture_alert_rao must be positive", nil},
		{"no relays", "mode: shadow\ndrand_urls: []", "drand_urls is empty", nil},
		{"relay", "mode: shadow\ndrand_urls: [\"ftp://relay\"]", `drand_urls "ftp://relay" is not an http(s) base url`, nil},
		{"relay query", "mode: shadow\ndrand_urls: [\"https://relay/?x=1\"]", "is not an http(s) base url", nil},
		{"line", "mode: shadow\namendment_lines: [\"UR settlement amendment v2: nonsense\"]", "amendment_lines:", nil},
		{"unknown key", "mode: shadow\nmodes: live", "", nil},
		{"no v2", "mode: shadow", "capture the flag needs settlement_v2_address and vault_v2_address", func(cfg *StConfig) { cfg.SettlementV2 = common.Address{} }},
		{"no pool", "mode: shadow", "no_id 4294967296 has no capture the flag pool", func(cfg *StConfig) { cfg.NoId = 1 << 32 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			alerts := captureLedgerV2Alerts(t)
			cfg := ledgerV2TestConfig()
			cfg.Netuid = 25
			if test.mutate != nil {
				test.mutate(cfg)
			}
			node := ctfTestYaml(t, test.text)
			stCtfSelect(cfg, stconn.ProfileTestnet, &node)
			if test.reason == "" {
				// yaml.v3 decodes unknown keys without complaint; the block stands
				if cfg.CtfConfigError != "" {
					t.Fatalf("refused: %s", cfg.CtfConfigError)
				}
				return
			}
			if cfg.Ctf.On() || !strings.HasPrefix(cfg.CtfConfigError, "st.yml testnet ctf block refused, capture the flag is off: ") || !strings.Contains(cfg.CtfConfigError, test.reason) {
				t.Fatalf("%+v %q", cfg.Ctf, cfg.CtfConfigError)
			}
			if len(*alerts) != 1 || (*alerts)[0] != StCtfAlertConfig+" testnet" {
				t.Fatalf("alerts %v", *alerts)
			}
		})
	}
}

// §13 (server config): st.yml selects each profile's ctf block without
// fallback, and a refused block leaves the profile enabled.
func TestStConfigSelectsTheCtfBlockPerProfile(t *testing.T) {
	captureLedgerV2Alerts(t)
	f := releaseStVaultFile()
	f.TestnetSettlementV2, f.TestnetVaultV2 = testLedgerV2Settlement, testLedgerV2Vault
	f.Ctf = ctfTestYaml(t, "mode: shadow")
	cfg, err := stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Ctf.On() || cfg.CtfConfigError != "" {
		t.Fatalf("testnet selected the mainnet ctf block: %+v %q", cfg.Ctf, cfg.CtfConfigError)
	}
	f.TestnetCtf = ctfTestYaml(t, "mode: shadow")
	if cfg, err = stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Ctf.Mode != StCtfModeShadow || cfg.CtfConfigError != "" {
		t.Fatalf("testnet ctf block: %+v %q", cfg.Ctf, cfg.CtfConfigError)
	}
	f.TestnetCtf = ctfTestYaml(t, "mode: sideways")
	if cfg, err = stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || !cfg.LedgerV2Enabled() || cfg.Ctf.On() || cfg.CtfConfigError == "" {
		t.Fatalf("a refused block: %+v %q", cfg.Ctf, cfg.CtfConfigError)
	}
}
