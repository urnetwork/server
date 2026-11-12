// Canonical source locks bind the same complete live graph in either mode.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"strings"
	"testing"
)

// The exact independently serialized bytes detect census or format drift.
func TestCompetitionSourceLockDigestBindsTenRepositoriesInBothModes(t *testing.T) {
	production := map[string]string{}
	for _, name := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"} {
		production[name] = strings.Repeat("a", 40)
	}
	productionDigest, err := CompetitionSourceLockDigestForMode(production, false)
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"schema":1,"development_snapshot":false,"repositories":{"connect":"` + production["connect"] + `","glog":"` + production["glog"] + `","goidenticons":"` + production["goidenticons"] + `","gvisor":"` + production["gvisor"] + `","proxy":"` + production["proxy"] + `","sdk":"` + production["sdk"] + `","server":"` + production["server"] + `","sn":"` + production["sn"] + `","userwireguard":"` + production["userwireguard"] + `","warp":"` + production["warp"] + `"}}`
	expected := sha256.Sum256([]byte(canonical))
	if productionDigest != hex.EncodeToString(expected[:]) {
		t.Fatalf("canonical production digest changed: %s", productionDigest)
	}
	if generic, err := CompetitionSourceLockDigest(production); err != nil || generic != productionDigest {
		t.Fatalf("generic production lock differs: %s %v", generic, err)
	}
	staging := maps.Clone(production)
	stagingDigest, err := CompetitionSourceLockDigestForMode(staging, true)
	if err != nil || stagingDigest != productionDigest {
		t.Fatalf("equal graphs must have equal source digests: %s %v", stagingDigest, err)
	}
	if generic, err := CompetitionSourceLockDigest(staging); err != nil || generic != stagingDigest {
		t.Fatalf("generic staging lock differs: %s %v", generic, err)
	}
	for _, staging := range []bool{false, true} {
		retired := maps.Clone(production)
		retired["operator-proxy"] = strings.Repeat("a", 40)
		if _, err := CompetitionSourceLockDigestForMode(retired, staging); err == nil {
			t.Fatalf("retired repository accepted, staging=%t", staging)
		}
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

// The active graph names sidecar after the ten, and a lock of the eleven has its
// own canonical bytes: the ten's with sidecar in key order. The ten alone keep
// the digest they had.
func TestCompetitionSourceLockDigestBindsTheTenWithSidecar(t *testing.T) {
	ten := []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"}
	if names := CompetitionSourceRepositoryNamesBeforeSidecar(); strings.Join(names, " ") != strings.Join(ten, " ") {
		t.Fatalf("the graph before sidecar = %v", names)
	}
	if names := CompetitionSourceRepositoryNames(); strings.Join(names, " ") != strings.Join(ten, " ")+" sidecar" {
		t.Fatalf("the active graph = %v", names)
	}
	CompetitionSourceRepositoryNames()[0] = "changed"
	CompetitionSourceRepositoryNamesBeforeSidecar()[0] = "changed"
	if CompetitionSourceRepositoryNames()[0] != "server" || CompetitionSourceRepositoryNamesBeforeSidecar()[0] != "server" {
		t.Fatal("a caller changed the graph's names")
	}

	commits := map[string]string{}
	for _, name := range CompetitionSourceRepositoryNames() {
		commits[name] = strings.Repeat("c", 40)
	}
	canonical := `{"schema":1,"development_snapshot":false,"repositories":{"connect":"` + commits["connect"] + `","glog":"` + commits["glog"] + `","goidenticons":"` + commits["goidenticons"] + `","gvisor":"` + commits["gvisor"] + `","proxy":"` + commits["proxy"] + `","sdk":"` + commits["sdk"] + `","server":"` + commits["server"] + `","sidecar":"` + commits["sidecar"] + `","sn":"` + commits["sn"] + `","userwireguard":"` + commits["userwireguard"] + `","warp":"` + commits["warp"] + `"}}`
	expected := sha256.Sum256([]byte(canonical))
	for _, staging := range []bool{false, true} {
		digest, err := CompetitionSourceLockDigestForMode(commits, staging)
		if err != nil || digest != hex.EncodeToString(expected[:]) {
			t.Fatalf("the eleven's digest, staging=%t: %s %v", staging, digest, err)
		}
	}
	before := maps.Clone(commits)
	delete(before, "sidecar")
	beforeDigest, err := CompetitionSourceLockDigest(before)
	if err != nil || beforeDigest == hex.EncodeToString(expected[:]) {
		t.Fatalf("the ten alone: %s %v", beforeDigest, err)
	}
}

// Sidecar is admitted only beside all ten, with a well-formed commit, and with
// nothing else: it stands in for no live repository and brings no other name.
func TestCompetitionSourceLockDigestRejectsSidecarInAnIncompleteOrUnknownGraph(t *testing.T) {
	eleven := map[string]string{}
	for _, name := range CompetitionSourceRepositoryNames() {
		eleven[name] = strings.Repeat("d", 40)
	}
	for _, name := range CompetitionSourceRepositoryNamesBeforeSidecar() {
		invalid := maps.Clone(eleven)
		delete(invalid, name)
		if _, err := CompetitionSourceLockDigest(invalid); err == nil {
			t.Fatalf("sidecar replaced mandatory %s", name)
		}
		invalid["synthetic-unknown"] = strings.Repeat("d", 40)
		if _, err := CompetitionSourceLockDigest(invalid); err == nil {
			t.Fatalf("sidecar and an unknown repository replaced %s", name)
		}
	}
	for _, commit := range []string{"", "invalid-commit", strings.Repeat("d", 39), strings.Repeat("D", 40)} {
		invalid := maps.Clone(eleven)
		invalid["sidecar"] = commit
		if _, err := CompetitionSourceLockDigestForMode(invalid, true); err == nil {
			t.Fatalf("accepted the sidecar pin %q", commit)
		}
	}
	for _, name := range []string{"synthetic-unknown", "operator-proxy"} {
		invalid := maps.Clone(eleven)
		invalid[name] = strings.Repeat("d", 40)
		if _, err := CompetitionSourceLockDigest(invalid); err == nil {
			t.Fatalf("accepted %s as a twelfth repository", name)
		}
	}
}
