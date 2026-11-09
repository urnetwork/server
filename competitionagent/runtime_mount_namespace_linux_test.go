//go:build linux

// Shared network and socket directories do not imply shared image mounts.
package competitionagent

import "testing"

// Independent systemd BindPaths namespaces passed network admission but left
// containerd looking at an unmounted rootfs. Refuse that exact topology.
func TestRuntimeBoundaryRejectsSplitImageMountNamespaces(t *testing.T) {
	network := runtimeBoundaryNamespaceId{Device: 4, Inode: 201}
	host := runtimeBoundaryNamespaceId{Device: 4, Inode: 101}
	mount := runtimeBoundaryNamespaceId{Device: 4, Inode: 301}
	units := map[string]runtimeBoundaryUnit{
		runtimeBoundaryKeeperUnitName:     {Pid: 11, ProcessStart: 1, Namespace: network},
		runtimeBoundaryEngineUnitName:     {Pid: 12, ProcessStart: 2, Namespace: network, MountNamespace: mount},
		runtimeBoundaryContainerdUnitName: {Pid: 13, ProcessStart: 3, Namespace: network, MountNamespace: mount},
	}
	if err := validateRuntimeBoundaryNamespacePeers(units, network, host); err != nil {
		t.Fatal(err)
	}
	for _, different := range []runtimeBoundaryNamespaceId{{}, {Device: 4, Inode: 302}, {Device: 5, Inode: 301}} {
		containerd := units[runtimeBoundaryContainerdUnitName]
		containerd.MountNamespace = different
		units[runtimeBoundaryContainerdUnitName] = containerd
		if err := validateRuntimeBoundaryNamespacePeers(units, network, host); err == nil {
			t.Fatalf("split or missing image mount namespace accepted: %+v", different)
		}
	}
}
