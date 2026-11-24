// The source graph selector keeps every round of the ten repositories at its
// exact frozen bytes and names the twelve only by an explicit version.
package model

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Omission is the ten, sidecar/1 is the ten with sidecar and operator-core last,
// and no other spelling selects a graph.
func TestCompetitionSourceGraphPolicyNamesItsRepositories(t *testing.T) {
	ten := "server connect sdk proxy glog goidenticons userwireguard sn warp gvisor"
	for _, c := range []struct {
		policy CompetitionSourceGraphPolicy
		names  string
	}{
		{policy: CompetitionSourceGraphPolicyLegacy, names: ten},
		{policy: CompetitionSourceGraphPolicySidecar, names: ten + " sidecar operator-core"},
	} {
		names, err := CompetitionSourceRepositoryNamesForGraphPolicy(c.policy)
		if err != nil || strings.Join(names, " ") != c.names {
			t.Errorf("%q: names=%v error=%v", c.policy, names, err)
		}
		if err := ValidateCompetitionSourceGraphPolicy(c.policy); err != nil {
			t.Errorf("%q refused: %v", c.policy, err)
		}
	}
	if string(CompetitionSourceGraphPolicySidecar) != "sidecar/1" {
		t.Fatal("the explicit source graph version changed")
	}
	for _, policy := range []CompetitionSourceGraphPolicy{"sidecar", "sidecar/2", "legacy", " ", "ten/1", "Sidecar/1"} {
		if _, err := CompetitionSourceRepositoryNamesForGraphPolicy(policy); err == nil {
			t.Errorf("unknown source graph %q named repositories", policy)
		}
		if ValidateCompetitionSourceGraphPolicy(policy) == nil {
			t.Errorf("unknown source graph %q accepted", policy)
		}
	}
}

// Neither JSON nor YAML writes an empty new field into a policy of the ten,
// and an explicit value survives a round trip only as sidecar/1.
func TestCompetitionSourceGraphPolicyLegacyCodecOmission(t *testing.T) {
	legacy := CompetitionEvaluationPolicy{HardwareId: "synthetic-hardware"}
	jsonBytes, err := json.Marshal(legacy)
	if err != nil || bytes.Contains(jsonBytes, []byte("source_graph_policy")) {
		t.Fatal("legacy JSON acquired a source graph field", err)
	}
	yamlBytes, err := yaml.Marshal(legacy)
	if err != nil || bytes.Contains(yamlBytes, []byte("source_graph_policy")) {
		t.Fatal("legacy YAML acquired a source graph field", err)
	}
	var replay CompetitionEvaluationPolicy
	if err := json.Unmarshal(jsonBytes, &replay); err != nil {
		t.Fatal(err)
	}
	replayed, err := json.Marshal(replay)
	if err != nil || !bytes.Equal(replayed, jsonBytes) || replay.SourceGraphPolicy != CompetitionSourceGraphPolicyLegacy {
		t.Fatal("legacy JSON did not round-trip byte-identically", err)
	}
	legacy.SourceGraphPolicy = CompetitionSourceGraphPolicySidecar
	explicit, err := json.Marshal(legacy)
	if err != nil || !bytes.Contains(explicit, []byte(`"source_graph_policy":"sidecar/1"`)) {
		t.Fatal("the explicit source graph is not serialized", err)
	}
	var decoded CompetitionEvaluationPolicy
	if err := json.Unmarshal(explicit, &decoded); err != nil || decoded.SourceGraphPolicy != CompetitionSourceGraphPolicySidecar || !decoded.Equal(legacy) {
		t.Fatal("the explicit source graph did not round-trip", err)
	}
	if legacy.Equal(CompetitionEvaluationPolicy{HardwareId: "synthetic-hardware"}) {
		t.Fatal("policies of different source graphs compare equal")
	}
	for _, value := range []string{`""`, `null`, `"sidecar/2"`, `"legacy"`, `1`} {
		var policy CompetitionEvaluationPolicy
		content := []byte(`{"source_graph_policy":` + value + `}`)
		if err := json.Unmarshal(content, &policy); err == nil && value != `null` {
			t.Errorf("explicit source graph %s accepted", value)
		}
		if policy.SourceGraphPolicy != CompetitionSourceGraphPolicyLegacy {
			t.Errorf("explicit source graph %s selected %q", value, policy.SourceGraphPolicy)
		}
	}
}

// A lock stands under a policy only when it holds exactly that policy's graph.
func TestCompetitionSourceLockMatchesGraphPolicy(t *testing.T) {
	ten := map[string]string{}
	for _, repository := range CompetitionSourceRepositoryNamesBeforeSidecar() {
		ten[repository] = strings.Repeat("a", 40)
	}
	twelve := map[string]string{"sidecar": strings.Repeat("b", 40), "operator-core": strings.Repeat("c", 40)}
	for repository, commit := range ten {
		twelve[repository] = commit
	}
	if !CompetitionSourceLockMatchesGraphPolicy(ten, CompetitionSourceGraphPolicyLegacy) || !CompetitionSourceLockMatchesGraphPolicy(twelve, CompetitionSourceGraphPolicySidecar) {
		t.Fatal("a lock was refused under its own source graph")
	}
	if CompetitionSourceLockMatchesGraphPolicy(twelve, CompetitionSourceGraphPolicyLegacy) {
		t.Fatal("a lock naming sidecar and operator-core stood under the graph of the ten")
	}
	if CompetitionSourceLockMatchesGraphPolicy(ten, CompetitionSourceGraphPolicySidecar) {
		t.Fatal("a lock of the ten stood under sidecar/1")
	}
	// the eleven sidecar/1 named before operator-core, and its mirror
	for _, name := range []string{"operator-core", "sidecar"} {
		eleven := map[string]string{}
		for repository, commit := range twelve {
			if repository != name {
				eleven[repository] = commit
			}
		}
		for _, policy := range []CompetitionSourceGraphPolicy{CompetitionSourceGraphPolicyLegacy, CompetitionSourceGraphPolicySidecar} {
			if CompetitionSourceLockMatchesGraphPolicy(eleven, policy) {
				t.Fatalf("a lock lacking %s stood under %q", name, policy)
			}
		}
	}
	if CompetitionSourceLockMatchesGraphPolicy(twelve, "sidecar/2") || CompetitionSourceLockMatchesGraphPolicy(nil, CompetitionSourceGraphPolicyLegacy) {
		t.Fatal("an unknown graph or an absent lock was accepted")
	}
	replaced := map[string]string{"sidecar": strings.Repeat("b", 40)}
	for repository, commit := range ten {
		if repository != "warp" {
			replaced[repository] = commit
		}
	}
	if CompetitionSourceLockMatchesGraphPolicy(replaced, CompetitionSourceGraphPolicyLegacy) {
		t.Fatal("sidecar replaced a repository of the ten")
	}
	replaced = map[string]string{"sidecar": strings.Repeat("b", 40), "synthetic-unknown": strings.Repeat("c", 40)}
	for repository, commit := range ten {
		replaced[repository] = commit
	}
	if CompetitionSourceLockMatchesGraphPolicy(replaced, CompetitionSourceGraphPolicySidecar) {
		t.Fatal("an unknown repository stood in for operator-core")
	}
	for _, name := range []string{"sidecar", "operator-core"} {
		malformed := map[string]string{}
		for repository, commit := range twelve {
			malformed[repository] = commit
		}
		malformed[name] = strings.Repeat("0", 64)
		if CompetitionSourceLockMatchesGraphPolicy(malformed, CompetitionSourceGraphPolicySidecar) {
			t.Fatalf("a malformed %s commit was accepted", name)
		}
	}
}
