//go:build linux

// Shared network and socket directories do not imply shared image mounts.
package competitionagent

import (
	"context"
	"reflect"
	"syscall"
	"testing"
)

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

// Startup observes the actual mounted block device before using its socket
// bind; an absent candidate mount cannot silently consume the data filesystem.
func TestPrivateEngineStartupRefusesUnboundedMountFallback(t *testing.T) {
	candidate := syscall.Stat_t{Dev: 901, Mode: syscall.S_IFDIR | 0755}
	device := syscall.Stat_t{Rdev: 901, Mode: syscall.S_IFBLK | 0600}
	if err := validatePrivateEngineStartupDevice(candidate, device); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*syscall.Stat_t, *syscall.Stat_t){
		func(c, d *syscall.Stat_t) { c.Dev = 902 },
		func(c, d *syscall.Stat_t) { d.Rdev = 0; c.Dev = 0 },
		func(c, d *syscall.Stat_t) { c.Mode = syscall.S_IFLNK | 0755 },
		func(c, d *syscall.Stat_t) { d.Mode = syscall.S_IFREG | 0600 },
		func(c, d *syscall.Stat_t) { c.Uid = 101 },
		func(c, d *syscall.Stat_t) { d.Uid = 101 },
		func(c, d *syscall.Stat_t) { c.Mode |= 0020 },
		func(c, d *syscall.Stat_t) { d.Mode |= 0040 },
	} {
		c, d := candidate, device
		change(&c, &d)
		if err := validatePrivateEngineStartupDevice(c, d); err == nil {
			t.Fatalf("unprotected mount or device accepted: %+v %+v", c, d)
		}
	}
}

// Missing authority cannot reach a namespace mutation or daemon exec.
func TestPrivateEngineStartupRejectsMissingAuthority(t *testing.T) {
	if err := StartPrivateEngine(context.Background(), nil); err == nil {
		t.Fatal("startup accepted absent root role")
	}
}

// The pre-entry --wd can retain an unreachable original systemd mount. Resolve
// cwd inside the target view with --wdns; a correct device alone is not enough.
func TestPrivateEngineStartupKeepsQuotaBackedWorkingDirectory(t *testing.T) {
	boundary := RuntimeResourceBoundary{
		CandidateRoot: "/synthetic/executor/candidate",
		Nsenter:       Pin{Path: "/synthetic/bin/nsenter"},
		Dockerd:       Pin{Path: "/synthetic/bin/dockerd"},
		EngineConfig:  Pin{Path: "/synthetic/config/engine.json"},
	}
	want := []string{"/synthetic/bin/nsenter", "--mount=/proc/self/fd/7", "--wdns=/synthetic/executor/candidate", "--", "/synthetic/bin/dockerd", "--config-file", "/synthetic/config/engine.json"}
	if got := privateEngineStartupArguments(boundary, 7); !reflect.DeepEqual(got, want) {
		t.Fatalf("startup lost held mount identity, bounded cwd or closed daemon arguments: %q", got)
	}
}
