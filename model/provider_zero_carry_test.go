// The zero-carry rules without a database (provider_zero_carry.go): the
// evidence rule, the first close time it counts, the clearing rules, and how
// the egress decision, the hard exclusions and the country guard treat a
// verdict.
package model

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The zombie of the investigation is caught; an idle provider, a new one, one
// whose contracts come from too few networks, and an honest provider with
// little traffic are not. The thresholds are inclusive.
func TestProviderZeroCarryEvidenceRule(t *testing.T) {
	for _, c := range []struct {
		name     string
		evidence ProviderZeroCarryEvidence
		want     bool
	}{
		// one sergey837 provider in kz, 2026-11-08: 1,352 contracts from 121
		// networks in a day, every one settled at zero bytes
		{"zombie", ProviderZeroCarryEvidence{RowCount: 1000, ContractCount: 878, NetworkCount: 61, CarriedByteCount: 0}, true},
		// a small country's zombie: few client networks, many contracts
		{"small country zombie", ProviderZeroCarryEvidence{RowCount: 261, ContractCount: 139, NetworkCount: 3, CarriedByteCount: 0}, true},
		// contracts that settle at a few bytes of noise
		{"noise", ProviderZeroCarryEvidence{RowCount: 160, ContractCount: 40, NetworkCount: 14, CarriedByteCount: 3}, true},
		{"at every threshold", ProviderZeroCarryEvidence{ContractCount: ProviderZeroCarryMinContracts, NetworkCount: ProviderZeroCarryMinNetworks, CarriedByteCount: ProviderZeroCarryMaxCarriedByteCount}, true},
		// idle: only the prober's contracts, which are not counted
		{"idle", ProviderZeroCarryEvidence{RowCount: 124, ContractCount: 0, NetworkCount: 0, CarriedByteCount: 0}, false},
		// new: a few contracts so far
		{"new", ProviderZeroCarryEvidence{RowCount: 6, ContractCount: 6, NetworkCount: 2, CarriedByteCount: 0}, false},
		{"one contract short", ProviderZeroCarryEvidence{ContractCount: ProviderZeroCarryMinContracts - 1, NetworkCount: 10, CarriedByteCount: 0}, false},
		// one or two networks cannot tell a broken client from a broken provider
		{"two networks", ProviderZeroCarryEvidence{RowCount: 212, ContractCount: 212, NetworkCount: ProviderZeroCarryMinNetworks - 1, CarriedByteCount: 0}, false},
		// honest, lightly used: most contracts empty, a few carried a page
		{"honest low traffic", ProviderZeroCarryEvidence{RowCount: 25, ContractCount: 25, NetworkCount: 4, CarriedByteCount: 30 * 1024}, false},
		{"one byte over the floor", ProviderZeroCarryEvidence{ContractCount: 100, NetworkCount: 10, CarriedByteCount: ProviderZeroCarryMaxCarriedByteCount + 1}, false},
		{"negative bytes", ProviderZeroCarryEvidence{ContractCount: 100, NetworkCount: 10, CarriedByteCount: -1}, false},
	} {
		if got := c.evidence.ZeroCarry(); got != c.want {
			t.Errorf("%s: zero carry %t, want %t", c.name, got, c.want)
		}
	}
}

// The window's start, or the time the device reported its current app
// version when that is later: a new build is judged only on what it carried.
func TestProviderZeroCarryEvidenceFrom(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-ProviderZeroCarryWindow)
	if got := providerZeroCarryEvidenceFrom(now, nil); !got.Equal(windowStart) {
		t.Fatalf("no version time: %s, want the window's start %s", got, windowStart)
	}
	old := now.Add(-30 * 24 * time.Hour)
	if got := providerZeroCarryEvidenceFrom(now, &old); !got.Equal(windowStart) {
		t.Fatalf("an old version: %s, want the window's start %s", got, windowStart)
	}
	upgraded := now.Add(-2 * time.Hour)
	if got := providerZeroCarryEvidenceFrom(now, &upgraded); !got.Equal(upgraded) {
		t.Fatalf("an upgrade two hours ago: %s, want the upgrade %s", got, upgraded)
	}
}

// A verdict clears on expiry, a URL success, a new app version or a gone
// client, expiry first; it holds otherwise, whitespace and a missing version
// read alike.
func TestProviderZeroCarryVerdictClearing(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	text := func(s string) *string { return &s }
	verdict := ProviderZeroCarryVerdict{
		ClientId:      server.NewId(),
		CreateTime:    now.Add(-time.Hour),
		ExpireTime:    now.Add(time.Hour),
		ClientVersion: text("2026.4.6+906996900"),
	}
	for _, c := range []struct {
		name  string
		state providerZeroCarryVerdictState
		want  string
	}{
		{"holds", providerZeroCarryVerdictState{verdict: verdict, active: true, clientVersion: text("2026.4.6+906996900")}, ""},
		{"same version with whitespace", providerZeroCarryVerdictState{verdict: verdict, active: true, clientVersion: text(" 2026.4.6+906996900 ")}, ""},
		{"upgraded", providerZeroCarryVerdictState{verdict: verdict, active: true, clientVersion: text("2026.11.6-1092062290")}, ProviderZeroCarryClearUpgrade},
		{"version gone", providerZeroCarryVerdictState{verdict: verdict, active: true}, ProviderZeroCarryClearUpgrade},
		{"url success", providerZeroCarryVerdictState{verdict: verdict, active: true, clientVersion: text("2026.4.6+906996900"), urlSuccess: true}, ProviderZeroCarryClearUrlSuccess},
		{"client gone", providerZeroCarryVerdictState{verdict: verdict, clientVersion: text("2026.4.6+906996900")}, ProviderZeroCarryClearGone},
		{"expired first", providerZeroCarryVerdictState{verdict: ProviderZeroCarryVerdict{ExpireTime: now, ClientVersion: verdict.ClientVersion}, urlSuccess: true}, ProviderZeroCarryClearExpired},
	} {
		if got := c.state.clearReason(now); got != c.want {
			t.Errorf("%s: clears %q, want %q", c.name, got, c.want)
		}
	}
	// no version at the verdict and none now is no upgrade
	unknown := providerZeroCarryVerdictState{verdict: ProviderZeroCarryVerdict{ExpireTime: now.Add(time.Hour)}, active: true}
	if got := unknown.clearReason(now); got != "" {
		t.Fatalf("an unknown version both times cleared as %q", got)
	}
}

// A verdict is a hard exclusion: out of every bucket and count under its own
// reason, after the common gates and before the URL-success rule, so a zombie
// in a country the guard exempts leaves Online as well.
func TestProviderZeroCarryDecisionIsAHardExclusion(t *testing.T) {
	decision := decideProviderEgress(&providerEgressFacts{zeroCarry: true, noUrlSuccess: true}, false)
	if !decision.hardExcluded || decision.reason != ProviderExcludedZeroCarry || decision.online || decision.counted || decision.quality || decision.speed {
		t.Fatalf("a zero-carry provider decided %+v", decision)
	}
	// the exempt country's zombie: no URL success, but the rule does not
	// apply there, so before the verdict it was online and counted
	exempt := decideProviderEgress(&providerEgressFacts{egressQuality: new(bool)}, false)
	if !exempt.online || !exempt.counted {
		t.Fatalf("the exempt country's provider without a verdict decided %+v", exempt)
	}
	exempt = decideProviderEgress(&providerEgressFacts{egressQuality: new(bool), zeroCarry: true}, false)
	if !exempt.hardExcluded || exempt.online || exempt.counted {
		t.Fatalf("the exempt country's zombie decided %+v", exempt)
	}
	// the common gates keep their own reasons
	tls := decideProviderEgress(&providerEgressFacts{tlsAuthenticationFailed: true, zeroCarry: true}, false)
	if tls.reason != ProviderExcludedTls {
		t.Fatalf("a TLS quarantine with a verdict counts as %q", tls.reason)
	}
	found := false
	for _, reason := range ProviderExcludedReasons {
		found = found || reason == ProviderExcludedZeroCarry
	}
	if !found {
		t.Fatal("the zero-carry reason is not reported")
	}
}

// The filter's hard failure includes a verdict; its common gates do not, and
// the URL-success rule's country guard reads only the common gates, so a
// verdict changes no country's exemption.
func TestProviderZeroCarryLeavesTheCountryGuardAlone(t *testing.T) {
	now := server.NowUtc()
	working := server.NewId()
	zombies := []server.Id{server.NewId(), server.NewId(), server.NewId()}
	filter := providerCountFilter{
		now: now,
		healthCounts: map[server.Id]ProviderEgressHealthCounts{
			working: {MeasuredAt: now, FirstMeasuredAt: now.Add(-time.Hour), OKCount: 5, Total: 5},
		},
		zeroCarry: map[server.Id]bool{},
	}
	countryCodes := map[server.Id]string{working: "kz"}
	for _, zombie := range zombies {
		filter.healthCounts[zombie] = ProviderEgressHealthCounts{MeasuredAt: now, FirstMeasuredAt: now.Add(-time.Hour), OKCount: 0, Total: 5}
		filter.zeroCarry[zombie] = true
		countryCodes[zombie] = "kz"
	}
	if !filter.hasHardEgressFailure(zombies[0]) || filter.hasCommonGateFailure(zombies[0]) {
		t.Fatal("a verdict is not a hard failure apart from the common gates")
	}
	settings := DefaultEgressIndexSettings()
	baseline := newProviderOnlineSuccessBaseline(filter, countryCodes, settings, nil)
	count := baseline.countryCodeCounts["kz"]
	// one of four measured providers succeeded: exempt, as without verdicts
	if count.Measured != 4 || count.Succeeded != 1 || !count.Exempt {
		t.Fatalf("the guard counted the country as %+v", count)
	}
}

// What an operator is told of a verdict: the URL-success rule's reason, every
// network check failing, with its fix and way back, under the egress code
// apps know. The reasons the API spec lists are unchanged.
func TestProviderZeroCarryStatusOutcome(t *testing.T) {
	outcome := providerStatusOutcomeOf(ProviderExcludedZeroCarry)
	if outcome.reason != ProviderStatusReasonEgressNoSuccess || outcome.code != ProviderStatusCodeEgressFailing {
		t.Fatalf("a verdict is told as %+v", outcome)
	}
	if outcome == providerStatusWithheld {
		t.Fatal("a verdict is told as an anti-abuse signal")
	}
}

// The read-through is bounded by its candidates like every other input of the
// hard exclusions, and the judge's statements read only what their indexes
// bound.
func TestProviderZeroCarrySqlIsBounded(t *testing.T) {
	hard := providerHardExclusionsSql()
	for _, part := range []string{
		"provider_zero_carry AS MATERIALIZED (",
		"WHERE client_id = ANY($1) AND expire_time > (now() AT TIME ZONE 'UTC')",
		"SELECT client_id FROM provider_zero_carry\n",
	} {
		if !strings.Contains(hard, part) {
			t.Errorf("the hard exclusions lack %q", part)
		}
	}
	evidence := providerZeroCarryEvidenceSql()
	for _, part := range []string{
		"contract.destination_id = candidate.client_id",
		"contract.close_time >= candidate.evidence_from",
		"contract.close_time <= $3",
		"contract.contract_id >= $4",
		"ORDER BY contract.close_time DESC",
		"LIMIT $5",
		"contract.companion_contract_id IS NULL",
		"contract.source_network_id <> contract.destination_network_id",
		"contract.provider_usage->'excluded_reason' IS NULL",
		"NOT contract.usage_unverified",
		"WHERE prober.network_id = contract.source_network_id",
	} {
		if !strings.Contains(evidence, part) {
			t.Errorf("the evidence read lacks %q", part)
		}
	}
	page := providerZeroCarryPageSql()
	for _, part := range []string{
		"provider_location.valid AND provider_location.connected AND provider_location.client_id > $1",
		"ORDER BY provider_location.client_id\n\t\t\tLIMIT $2",
		"success.total_count = 1 AND success.url_probe_policy_version = ",
		"run.total_count = 1 AND run.url_probe_policy_version = ",
		"LIMIT 2",
	} {
		if !strings.Contains(page, part) {
			t.Errorf("the page read lacks %q", part)
		}
	}
	// a contract closed at the window's start was created at most a contract
	// lifetime and the id margin before it
	from := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	floor := providerZeroCarryContractIdFloor(from)
	if got := server.ContractIdTime(floor); !got.Equal(from.Add(-DefaultContractExpiration - ContractIdCreateTimeMargin)) {
		t.Fatalf("the contract id floor is at %s", got)
	}
}
