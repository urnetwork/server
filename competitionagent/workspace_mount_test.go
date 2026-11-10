//go:build linux

// Admission permits one explicit job self-bind, never general shared mounts.
package competitionagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Keep all positive documents authenticated while changing the declared class.
func workspaceBoundaryFixture(t *testing.T) *runtimeBoundaryTest {
	t.Helper()
	fixture := newRuntimeBoundaryTest(t)
	fixture.boundary.WorkspaceMount = &RuntimeWorkspaceMount{Kind: runtimeWorkspacePropagation,
		Unit:        Pin{Path: "/etc/systemd/system/" + runtimeWorkspaceUnitName(fixture.boundary.WorkRoot), Sha256: strings.Repeat("a", 64)},
		MountHelper: Pin{Path: "/usr/bin/mount", Sha256: strings.Repeat("b", 64)}}
	content, err := RenderRuntimeWorkspaceMount(fixture.boundary, fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	fixture.boundary.WorkspaceMount.Unit.Sha256 = hex.EncodeToString(digest[:])
	fixture.host.documents[fixture.boundary.WorkspaceMount.Unit.Path] = content
	fixture.host.facts.MountInfo = append(fixture.host.facts.MountInfo, []byte(fmt.Sprintf("3 2 7:31 /jobs %s rw,nosuid,nodev shared:27 - ext4 /dev/loop31 rw\n", fixture.boundary.WorkRoot))...)
	fixture.host.facts.MountInfoAfter = bytes.Clone(fixture.host.facts.MountInfo)
	fixture.commit()
	return fixture
}

// Legacy profiles keep the strict no-nested-mount rule; opt-in is mandatory.
func TestRuntimeWorkspaceAdmissionRequiresExplicitClass(t *testing.T) {
	fixture := workspaceBoundaryFixture(t)
	proof, err := verifyRuntimeResourceBoundary(t.Context(), fixture.pin, fixture.scope, fixture.settings, fixture.host)
	if err != nil {
		t.Fatal("declared exact self-bind rejected", err)
	}
	_ = proof.Close()
	fixture.boundary.WorkspaceMount = nil
	fixture.commit()
	if _, err := verifyRuntimeResourceBoundary(t.Context(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("legacy profile silently acquired shared alias authority")
	}
}

// No parent sharing, arbitrary nested mount, stacked alias or wrong device.
func TestRuntimeWorkspaceAdmissionRejectsUnsafeTopology(t *testing.T) {
	for _, change := range []func(*runtimeBoundaryTest){
		func(f *runtimeBoundaryTest) {
			f.host.facts.MountInfo = bytes.ReplaceAll(f.host.facts.MountInfo, []byte(" shared:27"), nil)
		},
		func(f *runtimeBoundaryTest) {
			f.host.facts.MountInfo = bytes.ReplaceAll(f.host.facts.MountInfo, []byte("shared:27"), []byte("master:27"))
		},
		func(f *runtimeBoundaryTest) {
			f.host.facts.MountInfo = bytes.ReplaceAll(f.host.facts.MountInfo, []byte("shared:27"), []byte("shared:27 master:28"))
		},
		func(f *runtimeBoundaryTest) {
			f.host.facts.MountInfo = bytes.ReplaceAll(f.host.facts.MountInfo, []byte("/jobs "), []byte("/other "))
		},
		func(f *runtimeBoundaryTest) {
			f.host.facts.MountInfo = bytes.ReplaceAll(f.host.facts.MountInfo, []byte("3 2 7:31"), []byte("3 2 7:32"))
		},
		func(f *runtimeBoundaryTest) {
			f.host.facts.MountInfo = append(f.host.facts.MountInfo, []byte(fmt.Sprintf("4 2 7:31 /jobs %s/alias rw,nosuid,nodev - ext4 /dev/loop31 rw\n", f.boundary.CandidateRoot))...)
		},
		func(f *runtimeBoundaryTest) {
			f.host.facts.MountInfo = bytes.Replace(f.host.facts.MountInfo, []byte("rw,nosuid,nodev -"), []byte("rw,nosuid,nodev shared:17 -"), 1)
		},
		func(f *runtimeBoundaryTest) {
			f.host.facts.MountInfo = append(f.host.facts.MountInfo, []byte(fmt.Sprintf("4 2 7:31 /jobs %s rw,nosuid,nodev shared:27 - ext4 /dev/loop31 rw\n", f.boundary.WorkRoot))...)
		},
	} {
		fixture := workspaceBoundaryFixture(t)
		change(fixture)
		fixture.host.facts.MountInfoAfter = bytes.Clone(fixture.host.facts.MountInfo)
		if _, err := verifyRuntimeResourceBoundary(t.Context(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("unsafe workspace topology admitted")
		}
	}
}

// A peer may receive mounts but must never propagate them back to the host.
func TestRuntimeWorkspacePeerRequiresExactOneWayMaster(t *testing.T) {
	host := runtimeBoundaryMount{Target: "/srv/synthetic/jobs", Root: "/jobs", Source: "/dev/loop31", Type: "ext4", DeviceId: 123, Shared: true, SharedGroup: 27, Options: map[string]bool{"rw": true, "nosuid": true, "nodev": true}}
	peer := host
	peer.SharedGroup = 0
	peer.MasterGroup = 27
	if err := validateRuntimeWorkspacePeer(host, peer); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*runtimeBoundaryMount){
		func(p *runtimeBoundaryMount) { p.SharedGroup = 28 }, func(p *runtimeBoundaryMount) { p.MasterGroup = 0 }, func(p *runtimeBoundaryMount) { p.MasterGroup = 28 },
		func(p *runtimeBoundaryMount) { p.DeviceId++ }, func(p *runtimeBoundaryMount) { p.Root = "/" }, func(p *runtimeBoundaryMount) { p.Target = "/srv/synthetic/other" },
	} {
		changed := peer
		change(&changed)
		if err := validateRuntimeWorkspacePeer(host, changed); err == nil {
			t.Fatal("foreign or bidirectional workspace peer admitted")
		}
	}
}

// The mount unit belongs to the fixed root bootstrap, not any candidate path.
func TestRuntimeWorkspaceUnitRequiresExactIdentityAndPlacement(t *testing.T) {
	fixture := workspaceBoundaryFixture(t)
	properties := map[string]string{"Id": filepath.Base(fixture.boundary.WorkspaceMount.Unit.Path), "FragmentPath": fixture.boundary.WorkspaceMount.Unit.Path, "DropInPaths": "", "LoadState": "loaded", "ActiveState": "active", "SubState": "mounted", "InvocationID": strings.Repeat("a", 32), "What": fixture.boundary.WorkRoot, "Where": fixture.boundary.WorkRoot, "Slice": managementPrivateSliceName, "AllowedCPUs": "2 4", "ControlPID": "0", "Result": "success"}
	properties["What"] = fixture.boundary.BlockDevice
	if _, err := parseRuntimeWorkspaceUnit(runtimeBoundaryTestUnitBytes(properties), fixture.boundary, fixture.settings); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ key, value string }{
		{key: "Where", value: fixture.boundary.CandidateRoot}, {key: "What", value: "/srv/synthetic-other"}, {key: "Slice", value: "system.slice"},
		{key: "AllowedCPUs", value: "0-7"}, {key: "ControlPID", value: "42"}, {key: "DropInPaths", value: "/synthetic/override.conf"}, {key: "ActiveState", value: "inactive"}, {key: "InvocationID", value: ""},
	} {
		original := properties[change.key]
		properties[change.key] = change.value
		if _, err := parseRuntimeWorkspaceUnit(runtimeBoundaryTestUnitBytes(properties), fixture.boundary, fixture.settings); err == nil {
			t.Fatalf("changed %s admitted", change.key)
		}
		properties[change.key] = original
	}
	for key, value := range properties {
		delete(properties, key)
		if _, err := parseRuntimeWorkspaceUnit(runtimeBoundaryTestUnitBytes(properties), fixture.boundary, fixture.settings); err == nil {
			t.Fatalf("missing mandatory %s admitted", key)
		}
		properties[key] = value
	}
	properties["Unexpected"] = "synthetic"
	if _, err := parseRuntimeWorkspaceUnit(runtimeBoundaryTestUnitBytes(properties), fixture.boundary, fixture.settings); err == nil {
		t.Fatal("unexpected manager property admitted")
	}
}

// Compare the generated dependency name with the actual systemd path encoder.
func TestRuntimeWorkspaceUnitNameMatchesSystemd(t *testing.T) {
	encoder, err := exec.LookPath("systemd-escape")
	if err != nil {
		t.Skip("systemd path encoder is unavailable")
	}
	for _, path := range []string{"/srv/synthetic/jobs", "/srv/synthetic-name/jobs", "/.synthetic/jobs", "/srv/synthetic_1:2/jobs", "/srv/synthetic@name/jobs"} {
		output, err := exec.CommandContext(t.Context(), encoder, "--path", "--suffix=mount", "--", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		if name := runtimeWorkspaceUnitName(path); name != strings.TrimSpace(string(output)) {
			t.Fatalf("path encoding disagrees: %q: %q != %q", path, name, output)
		}
	}
}

// The parent is required as an existing mount, never itself made shared.
func TestRuntimeWorkspaceBootstrapRendererIsJobOnly(t *testing.T) {
	fixture := workspaceBoundaryFixture(t)
	content, err := RenderRuntimeWorkspaceMount(fixture.boundary, fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"What=" + fixture.boundary.WorkRoot + "\n", "Where=" + fixture.boundary.WorkRoot + "\n", "Options=bind,shared,nosuid,nodev\n", "Before=" + runtimeBoundaryContainerdUnitName + " " + runtimeBoundaryEngineUnitName, "Slice=" + managementPrivateSliceName} {
		if !bytes.Contains(content, []byte(expected)) {
			t.Fatalf("bootstrap lost closed directive: %s", expected)
		}
	}
	if bytes.Contains(content, []byte("rshared")) || bytes.Contains(content, []byte("Where="+fixture.boundary.CandidateRoot+"\n")) {
		t.Fatal("bootstrap shares the parent instead of only jobs")
	}
	fixture.boundary.WorkRoot = fixture.boundary.CandidateRoot
	if _, err := RenderRuntimeWorkspaceMount(fixture.boundary, fixture.settings); err == nil {
		t.Fatal("parent sharing renderer admitted")
	}
}

// Duplicate/zero/aliased group identities cannot become a master relationship.
func TestRuntimeWorkspaceMountParserRejectsAmbiguousGroups(t *testing.T) {
	for _, optional := range []string{"shared:0", "shared:01", "master:0", "shared:27 shared:28", "master:27 master:28"} {
		content := []byte("1 0 7:31 /jobs /srv/synthetic/jobs rw,nosuid,nodev " + optional + " - ext4 /dev/loop31 rw\n")
		if _, err := parseRuntimeBoundaryMounts(content, DefaultRuntimeResourceBoundarySettings()); err == nil {
			t.Fatalf("ambiguous propagation accepted: %s", optional)
		}
	}
}

// The fixed mount dependency must be present on both daemon lifetimes.
func TestRuntimeWorkspaceDaemonUnitRequiresBootstrapOrdering(t *testing.T) {
	fixture := workspaceBoundaryFixture(t)
	for _, pin := range []Pin{fixture.boundary.EngineUnit, fixture.boundary.ContainerdUnit} {
		properties := runtimeBoundaryTestUnitProperties(fixture.boundary, pin)
		name := filepath.Base(fixture.boundary.WorkspaceMount.Unit.Path)
		for _, key := range []string{"Requires", "BindsTo", "After"} {
			properties[key] += " " + name
		}
		if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"Requires", "BindsTo", "After"} {
			original := properties[key]
			properties[key] = strings.TrimSuffix(original, " "+name)
			if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err == nil {
				t.Fatalf("daemon lost %s boot/lifetime dependency", key)
			}
			properties[key] = original
		}
	}
}
