package controller

// The capability cohorts (connect/EXTENDER-UPDATE2.md §3.2): which records
// require a capability, the matcher a release checks it with, and the release
// metrics' fixed labels. Pure: no database, no vault.

import (
	"bytes"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

func testCapabilityExtender(keyByte byte, directoryTier int, canaryChannel string) *model.NetworkExtender {
	return &model.NetworkExtender{
		PublicKey:     bytes.Repeat([]byte{keyByte}, 32),
		DirectoryTier: directoryTier,
		CanaryChannel: canaryChannel,
	}
}

// The shape operations write, read through the same yaml the vault resource
// is decoded with.
func TestExtenderCapabilityCohortConfigShape(t *testing.T) {
	config := &ExtenderConfig{}
	body := strings.Join([]string{
		"directory:",
		"  durable_network_ids:",
		"    - 00000000-0000-0000-0000-000000000001",
		"  capability_cohorts:",
		"    - capability: tcp-camouflage-v1",
		"      extender_public_keys_hex:",
		"        - " + strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))),
	}, "\n")
	if err := yaml.Unmarshal([]byte(body), config); err != nil {
		t.Fatal(err)
	}
	if len(config.Directory.CapabilityCohorts) != 1 ||
		config.Directory.CapabilityCohorts[0].Capability != extenderCapabilityTcpCamouflageV1 ||
		len(config.Directory.CapabilityCohorts[0].ExtenderPublicKeysHex) != 1 {
		t.Fatalf("cohorts = %+v", config.Directory.CapabilityCohorts)
	}
	extender := testCapabilityExtender(0xab, connect.ExtenderDirectoryTierGated, "")
	if required := config.RecordRequiredCapabilities(extender, bytes.Repeat([]byte{1}, 32)); !slices.Equal(required, []string{"tcp-camouflage-v1"}) {
		t.Fatalf("an upper case configured key did not match: %v", required)
	}
}

// Root cause guarded (§3.2): no client may be left holding a record it cannot
// use. A requirement is set only where a release alone hands the record out
// and the record carries what the capability needs; a capability the server
// does not know is kept, so no client satisfies it.
func TestExtenderRecordRequiredCapabilitiesRule(t *testing.T) {
	cohortKey := byte(0xa1)
	otherKey := byte(0xb2)
	realityKey := bytes.Repeat([]byte{0x5a}, extenderRealityPublicKeySize)
	config := &ExtenderConfig{
		Directory: ExtenderDirectoryConfig{
			CapabilityCohorts: []ExtenderCapabilityCohortConfig{
				{Capability: " tcp-camouflage-v1 ", ExtenderPublicKeysHex: []string{hex.EncodeToString(bytes.Repeat([]byte{cohortKey}, 32)), "not a key"}},
				// listed twice: required once
				{Capability: "tcp-camouflage-v1", ExtenderPublicKeysHex: []string{hex.EncodeToString(bytes.Repeat([]byte{cohortKey}, 32))}},
				{Capability: "tcp-camoflage-v1", ExtenderPublicKeysHex: []string{hex.EncodeToString(bytes.Repeat([]byte{otherKey}, 32))}},
				{Capability: "", ExtenderPublicKeysHex: []string{hex.EncodeToString(bytes.Repeat([]byte{otherKey}, 32))}},
			},
		},
	}
	cases := []struct {
		name       string
		extender   *model.NetworkExtender
		realityKey []byte
		want       []string
	}{
		{"gated, listed, with a key", testCapabilityExtender(cohortKey, connect.ExtenderDirectoryTierGated, ""), realityKey, []string{"tcp-camouflage-v1"}},
		{"a gated canary, listed", testCapabilityExtender(cohortKey, connect.ExtenderDirectoryTierGated, connect.ExtenderChannelGated), realityKey, []string{"tcp-camouflage-v1"}},
		{"no camouflage key", testCapabilityExtender(cohortKey, connect.ExtenderDirectoryTierGated, ""), nil, []string{}},
		{"a malformed camouflage key", testCapabilityExtender(cohortKey, connect.ExtenderDirectoryTierGated, ""), realityKey[:16], []string{}},
		{"the open tier", testCapabilityExtender(cohortKey, connect.ExtenderDirectoryTierOpen, ""), realityKey, []string{}},
		{"a dns canary", testCapabilityExtender(cohortKey, connect.ExtenderDirectoryTierGated, connect.ExtenderChannelDns), realityKey, []string{}},
		{"an open tier gated canary", testCapabilityExtender(cohortKey, connect.ExtenderDirectoryTierOpen, connect.ExtenderChannelGated), realityKey, []string{}},
		{"not listed", testCapabilityExtender(0xc3, connect.ExtenderDirectoryTierGated, ""), realityKey, []string{}},
		// an unknown capability is required as written, key or no key
		{"a misspelled capability", testCapabilityExtender(otherKey, connect.ExtenderDirectoryTierGated, ""), nil, []string{"tcp-camoflage-v1"}},
	}
	for _, c := range cases {
		if required := config.RecordRequiredCapabilities(c.extender, c.realityKey); !slices.Equal(required, c.want) {
			t.Errorf("%s: required %v, want %v", c.name, required, c.want)
		}
	}
	if required := (&ExtenderConfig{}).RecordRequiredCapabilities(cases[0].extender, realityKey); len(required) != 0 {
		t.Fatalf("no cohorts required %v", required)
	}
}

// The matcher has slice F's semantics: an unknown requirement is unsatisfied,
// an unknown declaration is ignored, and nothing required is always satisfied.
func TestExtenderCapabilitiesSatisfied(t *testing.T) {
	cases := []struct {
		required []string
		offered  []string
		want     bool
	}{
		{nil, nil, true},
		{nil, []string{"anything"}, true},
		{[]string{"tcp-camouflage-v1"}, nil, false},
		{[]string{"tcp-camouflage-v1"}, []string{"tcp-camouflage-v1"}, true},
		{[]string{"tcp-camouflage-v1"}, []string{"other", "tcp-camouflage-v1"}, true},
		{[]string{"tcp-camoflage-v1"}, []string{"tcp-camoflage-v1"}, false},
		{[]string{"tcp-camouflage-v1", "future-v2"}, []string{"tcp-camouflage-v1", "future-v2"}, false},
	}
	for _, c := range cases {
		if got := extenderCapabilitiesSatisfied(c.required, c.offered); got != c.want {
			t.Errorf("required %v offered %v = %t, want %t", c.required, c.offered, got, c.want)
		}
	}
	if declared := extenderDeclaredCapabilities([]string{"x", "tcp-camouflage-v1", "tcp-camouflage-v1", " tcp-camouflage-v1"}); !slices.Equal(declared, []string{"tcp-camouflage-v1"}) {
		t.Fatalf("declared = %v, want the known capability once", declared)
	}
	if declared := extenderDeclaredCapabilities(nil); len(declared) != 0 {
		t.Fatalf("nothing declared = %v", declared)
	}
}

// Each cohort member's requirement decision is counted by fixed labels.
func TestExtenderRecordCapabilityRequirementsAreCounted(t *testing.T) {
	previous := defaultExtenderCapabilityMetrics
	defaultExtenderCapabilityMetrics = newExtenderCapabilityMetrics(prometheus.NewRegistry())
	t.Cleanup(func() { defaultExtenderCapabilityMetrics = previous })

	keyHex := hex.EncodeToString(bytes.Repeat([]byte{0xa1}, 32))
	config := &ExtenderConfig{Directory: ExtenderDirectoryConfig{CapabilityCohorts: []ExtenderCapabilityCohortConfig{
		{Capability: "tcp-camouflage-v1", ExtenderPublicKeysHex: []string{keyHex}},
		{Capability: "tcp-camoflage-v1", ExtenderPublicKeysHex: []string{keyHex}},
	}}}
	gated := testCapabilityExtender(0xa1, connect.ExtenderDirectoryTierGated, "")
	config.RecordRequiredCapabilities(gated, bytes.Repeat([]byte{1}, 32))
	config.RecordRequiredCapabilities(gated, nil)
	config.RecordRequiredCapabilities(testCapabilityExtender(0xa1, connect.ExtenderDirectoryTierOpen, ""), nil)
	requirements := defaultExtenderCapabilityMetrics.requirements
	for _, c := range []struct {
		capability string
		result     string
		want       float64
	}{
		{"tcp-camouflage-v1", extenderCapabilityResultRequired, 1},
		{"tcp-camouflage-v1", extenderCapabilityResultNoCamouflageKey, 1},
		{"tcp-camouflage-v1", extenderCapabilityResultNotReleaseOnly, 1},
		{"unknown", extenderCapabilityResultRequired, 2},
		{"unknown", extenderCapabilityResultNotReleaseOnly, 1},
	} {
		if got := testutil.ToFloat64(requirements.WithLabelValues(c.capability, c.result)); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.capability, c.result, got, c.want)
		}
	}
	if count := testutil.CollectAndCount(requirements); count != 6 {
		t.Fatalf("%d series, want the fixed 6", count)
	}
}

// Every release label is fixed: the families, the capable flag, and a country
// reduced to two letters or unknown.
func TestExtenderReleaseMetricLabelsAreFixed(t *testing.T) {
	for _, c := range []struct {
		ipVersions []int
		want       string
	}{
		{nil, "any"},
		{[]int{4, 6}, "any"},
		{[]int{6, 4, 6}, "any"},
		{[]int{4}, "ipv4"},
		{[]int{6, 6}, "ipv6"},
	} {
		if got := extenderReleaseFamily(c.ipVersions); got != c.want {
			t.Errorf("%v = %s, want %s", c.ipVersions, got, c.want)
		}
	}
	for countryCode, want := range map[string]string{
		"us":          "us",
		"DE":          "de",
		"":            "unknown",
		"usa":         "unknown",
		"u1":          "unknown",
		"é":           "unknown",
		"192.0.2.1":   "unknown",
		"a\x00":       "unknown",
		"Zz":          "zz",
		" u":          "unknown",
		"unknown":     "unknown",
		"o'":          "unknown",
		"ab\n":        "unknown",
		"\xff\xfe":    "unknown",
		"aa bb cc dd": "unknown",
	} {
		if got := extenderReleaseCountryLabel(countryCode); got != want {
			t.Errorf("country %q = %q, want %q", countryCode, got, want)
		}
	}

	metrics := newExtenderReleaseMetrics(prometheus.NewRegistry())
	if count := testutil.CollectAndCount(metrics.releases); count != len(extenderReleaseOutcomes)*4*2 {
		t.Fatalf("%d release series at start, want every outcome, family and capable value at zero", count)
	}
	metrics.observe(connect.ExtenderReleaseOutcomeIncompatible, extenderReleaseFamilyIpv6, false, "IR")
	metrics.observe(connect.ExtenderReleaseOutcomeOk, extenderReleaseFamilyAny, true, "not a country")
	if got := testutil.ToFloat64(metrics.releases.WithLabelValues("incompatible", "ipv6", "false")); got != 1 {
		t.Fatalf("incompatible ipv6 = %v", got)
	}
	if got := testutil.ToFloat64(metrics.countryOutcomes.WithLabelValues("incompatible", "ir")); got != 1 {
		t.Fatalf("incompatible ir = %v", got)
	}
	if got := testutil.ToFloat64(metrics.countryOutcomes.WithLabelValues("ok", "unknown")); got != 1 {
		t.Fatalf("ok unknown = %v", got)
	}
	if count := testutil.CollectAndCount(metrics.countryOutcomes); count != 2 {
		t.Fatalf("%d country series, want only those observed", count)
	}
}

// A retry time rounds up to whole seconds, so a client never asks early.
func TestExtenderReleaseRetryAfterSecondsRoundsUp(t *testing.T) {
	for retryAfter, want := range map[time.Duration]int{
		0:                              0,
		-time.Second:                   0,
		time.Nanosecond:                1,
		time.Second:                    1,
		time.Second + time.Millisecond: 2,
		connect.ExtenderReleaseRequestWindow + time.Second: 3601,
	} {
		if got := extenderReleaseRetryAfterSeconds(retryAfter); got != want {
			t.Errorf("%s = %d, want %d", retryAfter, got, want)
		}
	}
}
