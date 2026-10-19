// Canonical source locks distinguish the explicit staging graph without changing historical production bytes.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"strings"
	"testing"
)

// Retiring the unused staging repository changes the graph, not the canonical serialization contract.
func TestCompetitionSourceLockDigestPreservesProductionAndBindsStagingGraph(t *testing.T) {
	production := map[string]string{}
	for _, name := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "operator-proxy", "warp", "gvisor"} {
		production[name] = strings.Repeat("a", 40)
	}
	productionDigest, err := CompetitionSourceLockDigestForMode(production, false)
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"schema":1,"development_snapshot":false,"repositories":{"connect":"` + production["connect"] + `","glog":"` + production["glog"] + `","goidenticons":"` + production["goidenticons"] + `","gvisor":"` + production["gvisor"] + `","operator-proxy":"` + production["operator-proxy"] + `","proxy":"` + production["proxy"] + `","sdk":"` + production["sdk"] + `","server":"` + production["server"] + `","sn":"` + production["sn"] + `","userwireguard":"` + production["userwireguard"] + `","warp":"` + production["warp"] + `"}}`
	expected := sha256.Sum256([]byte(canonical))
	if productionDigest != hex.EncodeToString(expected[:]) {
		t.Fatalf("historical canonical production digest changed: %s", productionDigest)
	}
	if generic, err := CompetitionSourceLockDigest(production); err != nil || generic != productionDigest {
		t.Fatalf("generic production lock differs: %s %v", generic, err)
	}
	staging := maps.Clone(production)
	delete(staging, "operator-proxy")
	stagingDigest, err := CompetitionSourceLockDigestForMode(staging, true)
	if err != nil || stagingDigest == productionDigest {
		t.Fatalf("staging graph did not acquire its own valid digest: %s %v", stagingDigest, err)
	}
	canonical = strings.Replace(canonical, `"operator-proxy":"`+production["operator-proxy"]+`",`, "", 1)
	expected = sha256.Sum256([]byte(canonical))
	if stagingDigest != hex.EncodeToString(expected[:]) {
		t.Fatal("staging changed the canonical format instead of only its repository graph")
	}
	if generic, err := CompetitionSourceLockDigest(staging); err != nil || generic != stagingDigest {
		t.Fatalf("generic staging lock differs: %s %v", generic, err)
	}
	if _, err := CompetitionSourceLockDigestForMode(staging, false); err == nil {
		t.Fatal("production accepted the shortened staging graph")
	}
	if _, err := CompetitionSourceLockDigestForMode(production, true); err == nil {
		t.Fatal("staging retained the retired repository")
	}
}

// Every remaining repository is mandatory; neither count alone nor the deprecated name authorizes a graph.
func TestCompetitionSourceLockDigestRejectsIncompleteAndUnknownGraphs(t *testing.T) {
	staging := map[string]string{}
	for _, name := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"} {
		staging[name] = strings.Repeat("b", 40)
	}
	for _, name := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"} {
		invalid := maps.Clone(staging)
		delete(invalid, name)
		if _, err := CompetitionSourceLockDigest(invalid); err == nil {
			t.Fatalf("accepted missing %s", name)
		}
		invalid["synthetic-unknown"] = strings.Repeat("b", 40)
		if _, err := CompetitionSourceLockDigest(invalid); err == nil {
			t.Fatalf("unknown repository substituted for %s", name)
		}
		invalid = maps.Clone(staging)
		invalid[name] = "invalid-commit"
		if _, err := CompetitionSourceLockDigestForMode(invalid, true); err == nil {
			t.Fatalf("accepted malformed %s pin", name)
		}
	}
	invalid := maps.Clone(staging)
	delete(invalid, "gvisor")
	invalid["operator-proxy"] = strings.Repeat("b", 40)
	if _, err := CompetitionSourceLockDigest(invalid); err == nil {
		t.Fatal("deprecated repository replaced mandatory gvisor")
	}
	invalid = maps.Clone(staging)
	invalid["synthetic-unknown"] = strings.Repeat("b", 40)
	if _, err := CompetitionSourceLockDigest(invalid); err == nil {
		t.Fatal("unknown eleventh repository impersonated the production graph")
	}
}
