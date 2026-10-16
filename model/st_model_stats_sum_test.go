package model

import (
	"math/big"
	"testing"
)

// The emission sum reads a release-1.0 capture (mirrored as PoolSwept, its
// amount in `measured`) and a settlement v2 capture (V2EmissionCaptured, its
// amount in `amount`; `observed` is what the pool hotkey held, not what was
// captured), and nothing else.
func TestParseEmissionCapturedEventReadsBothVaults(t *testing.T) {
	for _, c := range []struct {
		kind     string
		dataJson string
		want     int64
		ok       bool
	}{
		{"PoolSwept", `{"e":"1","no_id":"1","pool_hotkey":"0x01","measured":"700","swept":"700","move_ok":true}`, 700, true},
		{"V2EmissionCaptured", `{"e":"3","no_id":"1","observed":"900","amount":"800","funded_through":"3"}`, 800, true},
		{"V2EmissionDeferred", `{"e":"3","no_id":"1","funded_through":"2"}`, 0, false},
		{"MinerClaimed", `{"measured":"5","amount":"5"}`, 0, false},
		{"PoolSwept", `{"measured":"not-a-number"}`, 0, false},
		{"V2EmissionCaptured", `not json`, 0, false},
	} {
		captured, ok := parseEmissionCapturedEvent(c.kind, c.dataJson)
		if ok != c.ok || (ok && captured.Cmp(big.NewInt(c.want)) != 0) {
			t.Fatalf("%s %s = %v, %t; want %d, %t", c.kind, c.dataJson, captured, ok, c.want, c.ok)
		}
	}
}

// A release-1.0 claim and a settlement v2 claim parse alike, and a coldkey is
// compared in one case so a miner who claims from both vaults counts once.
func TestParseMinerClaimedEventReadsBothVaults(t *testing.T) {
	coldkey, amount, ok := parseMinerClaimedEvent(`{"e":"1","no_id":"1","coldkey":"0xAB","share_bps":"5000","amount":"100","caller":"0x0"}`)
	if !ok || coldkey != "0xab" || amount.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("v1 claim = %s, %v, %t", coldkey, amount, ok)
	}
	coldkey, amount, ok = parseMinerClaimedEvent(`{"no_id":"1","root_index":"0","index":"4","coldkey":"0xab","cumulative":"300","amount":"200","caller":"0x0"}`)
	if !ok || coldkey != "0xab" || amount.Cmp(big.NewInt(200)) != 0 {
		t.Fatalf("v2 claim = %s, %v, %t", coldkey, amount, ok)
	}
	if _, _, ok := parseMinerClaimedEvent(`{"amount":"5"}`); ok {
		t.Fatal("a claim without a coldkey must not parse")
	}
}
