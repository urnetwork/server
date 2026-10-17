package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func payoutTransitionFixture(t testing.TB) ([]byte, ProviderPayoutMainnet) {
	t.Helper()
	identity := ProviderPayoutMainnet{Profile: "mainnet", ChainId: 964, Netuid: 25,
		GenesisHash: "0x" + strings.Repeat("11", 32), Activation: "reviewed", DeploymentId: "synthetic-transition",
		Coordinator: "0x" + strings.Repeat("22", 20), SettlementVault: "0x" + strings.Repeat("33", 20),
		PolicyHash: "0x" + strings.Repeat("44", 32), ReadinessSha256: strings.Repeat("55", 32)}
	policy := ProviderPayoutTransition{Schema: ProviderPayoutTransitionSchema, CutoffUtc: "2026-10-06T00:00:00Z",
		Attribution: "settled_contract_close_time", LegacyUsdc: "finish_pre_cutoff_obligations", Mainnet: identity}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	// Derived digest is not an input YAML field.
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	delete(object, "config_sha256")
	data, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return data, identity
}

func TestProviderPayoutTransitionUtcBoundary(t *testing.T) {
	data, identity := payoutTransitionFixture(t)
	policy, err := ParseProviderPayoutTransition(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		err := policy.MainnetAdmission(policy.Cutoff.Add(delta), identity)
		if (delta < 0) != (err != nil) {
			t.Fatalf("boundary %s: %v", delta, err)
		}
	}
	start, end := policy.SnWindow(policy.Cutoff.Add(-time.Hour), policy.Cutoff.Add(time.Hour))
	if !start.Equal(policy.Cutoff) || !end.Equal(policy.Cutoff.Add(time.Hour)) {
		t.Fatal("epoch earning clamp differs")
	}
	start, end = policy.SnWindow(policy.Cutoff.Add(-time.Hour), policy.Cutoff)
	if !start.Equal(end) {
		t.Fatal("historical epoch acquired subnet earnings")
	}
}

func TestProviderPayoutTransitionStrictReadinessAndSchema(t *testing.T) {
	data, identity := payoutTransitionFixture(t)
	policy, err := ParseProviderPayoutTransition(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ProviderPayoutMainnet){
		func(v *ProviderPayoutMainnet) { v.ChainId = 945 }, func(v *ProviderPayoutMainnet) { v.GenesisHash = "0x" + strings.Repeat("66", 32) },
		func(v *ProviderPayoutMainnet) { v.Profile = "testnet" }, func(v *ProviderPayoutMainnet) { v.Netuid = 7 },
		func(v *ProviderPayoutMainnet) { v.ReadinessSha256 = strings.Repeat("77", 32) }, func(v *ProviderPayoutMainnet) { v.DeploymentId = "other" },
	} {
		other := identity
		change(&other)
		if policy.MainnetAdmission(policy.Cutoff, other) == nil {
			t.Fatal("mismatched deployment admitted")
		}
	}
	for _, body := range []string{strings.Replace(string(data), "finish_pre_cutoff_obligations", "stop_all_sends", 1), strings.Replace(string(data), "00:00:00Z", "00:00:00+01:00", 1), string(data) + "\n---\n{}", strings.Replace(string(data), "cutoff_utc", "cutoff_utcc", 1)} {
		if _, err := ParseProviderPayoutTransition([]byte(body)); err == nil {
			t.Fatal("invalid schedule admitted")
		}
	}
	policy.Mainnet.Activation = "blocked"
	if policy.MainnetAdmission(policy.Cutoff.Add(time.Hour), identity) == nil {
		t.Fatal("date substituted for launch readiness")
	}
}

func TestProviderPayoutTransitionCanceledAndInvalidDeclaredResource(t *testing.T) {
	data, _ := payoutTransitionFixture(t)
	defer Config.PushSimpleResource("sn.yml", data)()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadProviderPayoutTransition(ctx); err != context.Canceled {
		t.Fatalf("canceled admission: %v", err)
	}
	defer Config.PushSimpleResource("sn.yml", []byte("not: a transition"))()
	if _, err := LoadProviderPayoutTransition(context.Background()); err == nil {
		t.Fatal("declared invalid schedule fell back to legacy")
	}
}
