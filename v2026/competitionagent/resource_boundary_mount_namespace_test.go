// Namespace handles are valid foreign mount roots, never candidate quota proof.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Docker derives its plugin namespace from the exact mapped root identity.
func TestRuntimeResourceBoundaryBindsRemappedPluginNamespace(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	var info map[string]any
	if err := json.Unmarshal(fixture.host.daemon, &info); err != nil {
		t.Fatal(err)
	}
	namespaces := info["Containerd"].(map[string]any)["Namespaces"].(map[string]any)
	namespaces["Plugins"] = fmt.Sprintf("plugins.moby-%d.%d", fixture.boundary.ContainerRootUid, fixture.boundary.ContainerRootGid)
	content, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeBoundaryDaemon(content, fixture.boundary, fixture.settings); err != nil {
		t.Fatal("actual mapped plugin namespace rejected", err)
	}
	for _, changed := range []string{"urnetwork-evaluation-plugins", "plugins.moby-1.1", "synthetic-other"} {
		namespaces["Plugins"] = changed
		content, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateRuntimeBoundaryDaemon(content, fixture.boundary, fixture.settings); err == nil {
			t.Fatal("unbound plugin namespace accepted", changed)
		}
	}
}

// Exercise full admission with Docker-style namespace mounts outside the quota.
func TestRuntimeResourceBoundaryAcceptsForeignNamespaceHandleMounts(t *testing.T) {
	for _, kind := range []string{"net", "mnt", "pid", "user", "uts", "ipc", "cgroup", "time"} {
		fixture := newRuntimeBoundaryTest(t)
		line := []byte("3 1 0:42 " + kind + ":[123456789] /run/synthetic-runtime/netns/handle rw - nsfs nsfs rw\n")
		fixture.host.facts.MountInfo = append(fixture.host.facts.MountInfo, line...)
		fixture.host.facts.MountInfoAfter = bytes.Clone(fixture.host.facts.MountInfo)
		proof, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host)
		if err != nil {
			t.Fatalf("valid foreign %s namespace mount rejected: %v", kind, err)
		}
		if err := proof.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Distribution branding does not change the separately pinned version/commit.
func TestRuntimeBoundaryContainerdAcceptsPinnedDistributionIdentity(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	version := "containerd containerd v" + fixture.boundary.ContainerdVersion + " " + fixture.boundary.ContainerdSourceCommit + "\n"
	if err := validateRuntimeBoundaryContainerdVersion([]byte(version), fixture.boundary, fixture.settings); err != nil {
		t.Fatal("pinned distribution version was rejected", err)
	}
	for _, changed := range []string{
		strings.Replace(version, fixture.boundary.ContainerdVersion, "9.9.9", 1),
		strings.Replace(version, fixture.boundary.ContainerdSourceCommit, strings.Repeat("d", 40), 1),
		strings.Replace(version, "containerd containerd", "containerd synthetic-vendor", 1),
		version + "extra",
	} {
		if err := validateRuntimeBoundaryContainerdVersion([]byte(changed), fixture.boundary, fixture.settings); err == nil {
			t.Fatalf("changed source identity accepted: %q", changed)
		}
	}
}

// A valid handle mounted inside writable roots must still fail the quota gate.
func TestRuntimeResourceBoundaryRejectsNestedNamespaceHandleMount(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	line := []byte("3 2 0:42 net:[123456789] " + fixture.boundary.WorkRoot + " rw - nsfs nsfs rw\n")
	fixture.host.facts.MountInfo = append(fixture.host.facts.MountInfo, line...)
	fixture.host.facts.MountInfoAfter = bytes.Clone(fixture.host.facts.MountInfo)
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("namespace mount bypassed the nested-mount refusal")
	}
}

// Namespace spelling cannot excuse noncanonical ordinary roots or targets.
func TestRuntimeBoundaryMountsRejectMalformedNamespaceIdentity(t *testing.T) {
	for _, row := range []string{
		"3 1 0:42 net:[123456789] /run/synthetic-handle rw - ext4 synthetic rw\n",
		"3 1 0:42 net:[0] /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 net:[01] /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 net:[18446744073709551616] /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 net:[123 /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 unknown:[123] /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 net:[123] /run/../synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 relative /run/synthetic-handle rw - ext4 synthetic rw\n",
	} {
		if _, err := parseRuntimeBoundaryMounts([]byte(row), DefaultRuntimeResourceBoundarySettings()); err == nil {
			t.Fatalf("malformed mount identity accepted: %q", row)
		}
	}
}
