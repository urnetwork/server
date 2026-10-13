package controller

// The server-only egress_per_family key: absent keeps the strict one-address
// attribution, true enables one live address per family.

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
)

func TestParseVerifySettingsEgressPerFamily(t *testing.T) {
	policyHash := [32]byte{4, 5, 6}
	coordinator := common.HexToAddress("0xC18925925E2B7bb9059b7d696b8c92762AE86406")
	egressKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	parse := func(extra string) bool {
		SetStConfig(&StConfig{Profile: "mainnet", PolicyHash: policyHash, ChainId: 964, ContractAddress: coordinator})
		defer SetStConfig(nil)
		pop := server.Vault.PushSimpleResource("verify-egress-family-test.yml", []byte(fmt.Sprintf(`profile: mainnet
policy_hash: "0x%x"
egress_hash_key: %s
settings:
  trail_depth: 8
  step_timeout_seconds: 30
  step_timeout_grace_seconds: 5
  trail_ttl_grace_seconds: 60
  egress_ttl_seconds: 600
  egress_refresh_seconds: 120
  reliability_a_min: 8
  stats_period_seconds: 900
  egress_ipv4_prefix: 29
  egress_ipv6_prefix: 48
  egress_hash_key_id: test-v1
  soft_guardrails_enabled: false
  hard_seed_per_minute_per_source: 40
  hard_extend_per_minute_per_source: 240
  hard_active_trails_per_source: 32
%s`, policyHash, egressKey, extra)))
		defer pop()
		return parseVerifySettings(server.Vault.RequireSimpleResource("verify-egress-family-test.yml")).EgressPerFamily
	}
	if parse("") {
		t.Fatal("per-family attribution is on without its key")
	}
	if parse("  egress_per_family: false\n") {
		t.Fatal("egress_per_family: false enabled per-family attribution")
	}
	if !parse("  egress_per_family: true\n") {
		t.Fatal("egress_per_family: true left strict attribution on")
	}
}
