// The zero-carry rules without a database (provider_zero_carry.go): the
// evidence rule, the first close time it counts, the clearing rules, and how
// the egress decision, the hard exclusions and the country guard treat a
// verdict.
package model

import (
	"fmt"
	"reflect"
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
		// the read stopped at its limit, and a contract earlier in the day
		// carried: a busy honest provider whose newest contracts are empty
		{"older contract carried", ProviderZeroCarryEvidence{RowCount: 1000, ContractCount: 900, NetworkCount: 40, CarriedByteCount: 0, OlderCarried: true}, false},
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

// A verdict clears on expiry, a gone client, a URL success or a new app
// version, in that order; it holds otherwise, whitespace and a missing
// version read alike. A deleted client's device reads as no version, and
// clears as gone, not as an upgrade.
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
		{"client deleted", providerZeroCarryVerdictState{verdict: verdict}, ProviderZeroCarryClearGone},
		{"gone with a success", providerZeroCarryVerdictState{verdict: verdict, urlSuccess: true}, ProviderZeroCarryClearGone},
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
	confirmation := providerZeroCarryConfirmationSql()
	for _, read := range []struct {
		name  string
		query string
	}{{"evidence", evidence}, {"confirming", confirmation}} {
		for _, part := range []string{
			"contract.destination_id = candidate.client_id",
			"contract.close_time >= candidate.evidence_from",
			"contract.close_time <= $3",
			"contract.create_time >= candidate.evidence_from",
			"contract.contract_id >= $4",
			"ORDER BY contract.close_time DESC",
			"LIMIT $5",
			"contract.companion_contract_id IS NULL",
			"contract.source_network_id <> contract.destination_network_id",
			"contract.provider_usage->'excluded_reason' IS NULL",
			"contract.provider_usage->'legacy_exclusion' IS NULL",
			"NOT contract.usage_unverified",
			"WHERE prober.network_id = contract.source_network_id",
			"min(recent.close_time) AS oldest_close_time",
		} {
			if !strings.Contains(read.query, part) {
				t.Errorf("the %s read lacks %q", read.name, part)
			}
		}
	}
	// only the confirming read reads close evidence, per counted contract,
	// and searches past its limit for a contract that carried, below the
	// oldest row it read, over the same window
	for _, part := range []string{
		"CASE WHEN read_contract.counted THEN EXISTS (",
		"close_evidence.contract_id = read_contract.contract_id",
		"close_evidence.party = 'source'",
		"NOT close_evidence.checkpoint",
		"close_evidence.acked_byte_count = 0",
		"close_evidence.unacked_byte_count = 0",
		"count(*) FILTER (WHERE recent.counted AND NOT recent.idle) AS contract_count",
		"CASE WHEN $5 <= confirmed.row_count THEN EXISTS (",
		"older.destination_id = candidate.client_id",
		"older.create_time >= candidate.evidence_from",
		"older.close_time < confirmed.oldest_close_time",
		"(older.provider_usage->>'byte_count')::numeric END > 0",
	} {
		if !strings.Contains(confirmation, part) {
			t.Errorf("the confirming read lacks %q", part)
		}
	}
	if strings.Contains(evidence, "contract_close_report_evidence") {
		t.Error("the evidence read reads close evidence for every candidate")
	}
	page := providerZeroCarryPageSql()
	for _, part := range []string{
		"provider_location.valid AND provider_location.connected AND provider_location.client_id > $1",
		"ORDER BY provider_location.client_id\n\t\t\tLIMIT $2",
		providerOnlineUrlEvidenceSql("success") + " AND success.ok_count = 1",
		providerOnlineUrlEvidenceSql("run"),
		"LIMIT 2",
	} {
		if !strings.Contains(page, part) {
			t.Errorf("the page read lacks %q", part)
		}
	}
	// a success the verdict did not see: within the URL window before it or
	// after it
	cleared := providerZeroCarryUrlSuccessClearedSql("success", "verdict")
	if !strings.Contains(cleared, providerOnlineUrlEvidenceSql("success")+" AND success.ok_count = 1") ||
		!strings.Contains(cleared, fmt.Sprintf("success.measured_at > verdict.create_time - interval '%d seconds'", int(ProviderEgressHealthMaxAge/time.Second))) {
		t.Errorf("the clearing success is %s", cleared)
	}
	// a contract closed at the window's start was created at most a contract
	// lifetime and the id margin before it
	from := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	floor := providerZeroCarryContractIdFloor(from)
	if got := server.ContractIdTime(floor); !got.Equal(from.Add(-DefaultContractExpiration - ContractIdCreateTimeMargin)) {
		t.Fatalf("the contract id floor is at %s", got)
	}
}

// The judge's URL condition is the URL-success rule's own evidence, through
// one shared predicate, so the two cannot drift apart when either changes.
// The window's whole row predicate is the shared one, nothing after it, and
// the rule's evidence is within the window. When the rule's tally gains a
// general-catalog half (D4), the rule decides by that half, and its source
// condition must be in the shared predicate too; this trips until it is.
func TestProviderZeroCarryUrlConditionIsTheOnlineRules(t *testing.T) {
	window := strings.TrimSpace(providerEgressHealthWindowSql())
	rows := strings.TrimSpace(providerEgressHealthWindowRowsSql(""))
	if !strings.HasSuffix(window, rows) {
		t.Fatalf("the URL window's row predicate is not the shared one:\n%s\nwant it to end with\n%s", window, rows)
	}
	for _, alias := range []string{"run", "success"} {
		if !strings.Contains(providerOnlineUrlEvidenceSql(alias), providerEgressHealthWindowRowsSql(alias)) {
			t.Errorf("the URL-success rule's evidence over %s is not within the window", alias)
		}
	}
	if providerZeroCarryUrlRunSql("run") != providerOnlineUrlEvidenceSql("run") {
		t.Error("the judge's URL runs are not the rule's evidence")
	}
	if _, general := reflect.TypeOf(ProviderEgressHealthCounts{}).FieldByName("General"); general &&
		!strings.Contains(providerOnlineUrlEvidenceSql("run"), "url_source") {
		t.Error("the URL-success rule decides by the general catalog's outcomes, and the shared predicate does not")
	}
}
