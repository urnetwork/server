// Resource-boundary regressions use synthetic protected documents and injected
// kernel facts. They never format, mount, invoke nft or contact any daemon.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

// The fake preserves pin checking and inventory order without host capabilities.
type runtimeBoundaryTestHost struct {
	documents map[string][]byte
	facts     runtimeBoundaryFacts
	network   []byte
	daemon    []byte
	events    []string
	fail      string
}

// A fake protected read still rejects changed bytes or an excessive document.
func (self *runtimeBoundaryTestHost) Document(pin Pin, maximum int64) ([]byte, error) {
	self.events = append(self.events, "document:"+pin.Path)
	content, present := self.documents[pin.Path]
	digest := sha256.Sum256(content)
	if !present || int64(len(content)) > maximum || hex.EncodeToString(digest[:]) != pin.Sha256 || self.fail == pin.Path {
		return nil, errors.New("synthetic protected document failure")
	}
	return append([]byte(nil), content...), nil
}

// Executable pin failures retain the real verifier's fail-closed ordering.
func (self *runtimeBoundaryTestHost) Executable(pin Pin) error {
	self.events = append(self.events, "executable:"+pin.Path)
	if self.fail == pin.Path {
		return errors.New("synthetic executable pin failure")
	}
	return nil
}

// Paths are recorded so a partial authority check cannot masquerade as success.
func (self *runtimeBoundaryTestHost) Directory(path string, _ RuntimeResourceScope) error {
	self.events = append(self.events, "directory:"+path)
	if self.fail == path {
		return errors.New("synthetic directory authority failure")
	}
	return nil
}

// No descriptor, mount or real block device is opened by the test seam.
func (self *runtimeBoundaryTestHost) Kernel(ctx context.Context, _ RuntimeResourceBoundary, _ RuntimeResourceScope, _ RuntimeResourceBoundarySettings) (runtimeBoundaryFacts, error) {
	self.events = append(self.events, "kernel")
	if self.fail == "kernel" {
		return runtimeBoundaryFacts{}, errors.New("synthetic kernel failure")
	}
	return self.facts, ctx.Err()
}

// Reported engine class is an independent native requirement, not inferred
// from accepted Docker minor versions or a protected configuration document.
func (self *runtimeBoundaryTestHost) Daemon(ctx context.Context, _ RuntimeResourceBoundary, _ RuntimeResourceBoundarySettings) ([]byte, error) {
	self.events = append(self.events, "daemon")
	if self.fail == "daemon" {
		return nil, errors.New("synthetic private daemon failure")
	}
	return append([]byte(nil), self.daemon...), ctx.Err()
}

// Live-policy failures are not replaced by an on-disk policy-file assertion.
func (self *runtimeBoundaryTestHost) Network(ctx context.Context, _ RuntimeResourceBoundary, _ RuntimeResourceBoundarySettings) ([]byte, error) {
	self.events = append(self.events, "network")
	if self.fail == "network" {
		return nil, errors.New("synthetic network inventory failure")
	}
	return append([]byte(nil), self.network...), ctx.Err()
}

// Separate fixtures own all slices and maps; mutations never leak across cases.
type runtimeBoundaryTest struct {
	boundary RuntimeResourceBoundary
	pin      Pin
	scope    RuntimeResourceScope
	settings RuntimeResourceBoundarySettings
	host     *runtimeBoundaryTestHost
}

// Complete synthetic facts correspond to a one-GiB private loop filesystem.
func newRuntimeBoundaryTest(t *testing.T) *runtimeBoundaryTest {
	t.Helper()
	base := "/srv/synthetic-evaluation"
	candidate := filepath.Join(base, "candidate")
	boundary := RuntimeResourceBoundary{Schema: Schema, BaseDirectory: base, CandidateRoot: candidate,
		BackingFile: "/srv/synthetic-backing/engine.img", BlockDevice: "/dev/loop31",
		MaximumBytes: 1024 * 1024 * 1024, MinimumFreeBytes: 128 * 1024 * 1024, MinimumBackingFreeBytes: 256 * 1024 * 1024,
		PrivateSocketPath: filepath.Join(base, "control", "docker.sock"), ContainerdSocketPath: filepath.Join(base, "control", "containerd.sock"),
		DockerDataRoot: filepath.Join(candidate, "docker"), ContainerdRoot: filepath.Join(candidate, "containerd"), EngineExecRoot: filepath.Join(candidate, "exec"),
		WorkRoot: filepath.Join(candidate, "jobs"), ScratchRoot: filepath.Join(candidate, "scratch"),
		UserNamespace: "synthetic_evaluation", ContainerRootUid: 200000, ContainerRootGid: 300000, ContainerIdMapLength: 65536,
		RuntimeName: "runc", RuntimeArguments: []string{}}
	executable := func(name string) Pin {
		return Pin{Path: "/opt/synthetic-private-engine/bin/" + name, Sha256: strings.Repeat("a", 64)}
	}
	boundary.Dockerd, boundary.Containerd, boundary.Runc, boundary.ContainerdShim, boundary.Nftables = executable("dockerd"), executable("containerd"), executable("runc"), executable("containerd-shim-runc-v2"), executable("nft")
	boundary.SocketModeHelper = Pin{Path: "/usr/bin/chmod", Sha256: strings.Repeat("b", 64)}
	scope := RuntimeResourceScope{BaseDirectory: base, StateDirectory: filepath.Join(base, "state"), WorkDirectory: boundary.WorkRoot,
		ScratchDirectory: boundary.ScratchRoot, TrustedOperatorUid: 12345, WorkerUid: 12346, WorkerGid: 12346}
	host := &runtimeBoundaryTestHost{documents: map[string][]byte{}}
	fixture := &runtimeBoundaryTest{boundary: boundary, scope: scope, host: host, settings: DefaultRuntimeResourceBoundarySettings()}
	engine, err := json.Marshal(runtimeBoundaryEngineDocument(boundary, fixture.settings))
	if err != nil {
		t.Fatal(err)
	}
	fixture.boundary.EngineConfig = fixture.document("engine.json", engine)
	fixture.boundary.ContainerdConfig = fixture.document("containerd.toml", runtimeBoundaryContainerdDocument(boundary))
	fixture.boundary.NetworkPolicy = fixture.document("network.nft", runtimeBoundaryNetworkPolicy())
	fixture.commit()
	const blockDevice = uint64(7<<8 | 31)
	mountInfo := []byte(fmt.Sprintf("1 0 8:1 / / rw - ext4 /dev/synthetic-root rw\n2 1 7:31 / %s rw,nosuid,nodev - ext4 /dev/loop31 rw\n", candidate))
	host.facts = runtimeBoundaryFacts{MountInfo: mountInfo, MountInfoAfter: append([]byte(nil), mountInfo...),
		SubUids: []byte("synthetic_evaluation:200000:65536\n"), SubGids: []byte("synthetic_evaluation:300000:65536\n"),
		BlockDeviceId: blockDevice, BlockBytes: boundary.MaximumBytes, LoopNumber: 31, LoopBackingDevice: 8<<8 | 2, LoopBackingInode: 123,
		BackingDevice: 8<<8 | 2, BackingInode: 123, BackingBytes: boundary.MaximumBytes, BackingAllocatedBytes: boundary.MaximumBytes,
		BackingFilesystemType: runtimeBoundaryExt4, FilesystemType: runtimeBoundaryExt4, FilesystemBytes: boundary.MaximumBytes - 1024*1024,
		FreeBytes: 512 * 1024 * 1024, BackingFreeBytes: 2 * 1024 * 1024 * 1024, PathDeviceIds: map[string]uint64{}}
	for _, path := range []string{candidate, boundary.DockerDataRoot, boundary.ContainerdRoot, boundary.EngineExecRoot, boundary.WorkRoot, boundary.ScratchRoot} {
		host.facts.PathDeviceIds[path] = blockDevice
	}
	host.facts.PathDeviceIds[scope.StateDirectory] = 8<<8 | 2
	host.facts.PathDeviceIds[filepath.Dir(boundary.PrivateSocketPath)] = 8<<8 | 2
	host.network = runtimeBoundaryTestNetwork(t)
	info := map[string]any{"Driver": "overlay2", "DriverStatus": [][]string{{"Backing Filesystem", "extfs"}},
		"DockerRootDir": filepath.Join(boundary.DockerDataRoot, "200000.300000"), "CgroupDriver": "systemd", "CgroupVersion": "2", "DefaultRuntime": "runc",
		"LiveRestoreEnabled": false, "SecurityOptions": []string{"name=userns"},
		"Containerd": map[string]any{"Address": boundary.ContainerdSocketPath, "Namespaces": map[string]any{"Containers": "urnetwork-evaluation", "Plugins": "urnetwork-evaluation-plugins"}},
		"Runtimes":   map[string]any{"runc": map[string]any{"path": "runc"}, "io.containerd.runc.v2": map[string]any{"path": "runc"}}}
	host.daemon, err = json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// Test-private pins always describe the current synthetic document bytes.
func (self *runtimeBoundaryTest) document(name string, content []byte) Pin {
	path := "/etc/synthetic-private-engine/" + name
	digest := sha256.Sum256(content)
	self.host.documents[path] = append([]byte(nil), content...)
	return Pin{Path: path, Sha256: hex.EncodeToString(digest[:])}
}

// Manifest mutations remain authenticated so rejection exercises native policy.
func (self *runtimeBoundaryTest) commit() {
	content, err := json.Marshal(self.boundary)
	if err != nil {
		panic(err)
	}
	self.pin = self.document("boundary.json", content)
}

// Native nft JSON includes runtime handles, but no mutable acceptance rules.
func runtimeBoundaryTestNetwork(t *testing.T) []byte {
	t.Helper()
	objects := []any{map[string]any{"metainfo": map[string]any{"version": "synthetic"}},
		map[string]any{"table": map[string]any{"family": "inet", "name": runtimeBoundaryNftTable, "handle": 1}}}
	for _, chain := range []string{"input", "forward", "output"} {
		objects = append(objects, map[string]any{"chain": map[string]any{"family": "inet", "table": runtimeBoundaryNftTable, "name": chain,
			"handle": 2, "type": "filter", "hook": chain, "prio": -300, "policy": "accept"}})
		keys := []string{"iifname"}
		if chain == "forward" {
			keys = append(keys, "oifname")
		} else if chain == "output" {
			keys = []string{"oifname"}
		}
		for _, key := range keys {
			objects = append(objects, map[string]any{"rule": map[string]any{"family": "inet", "table": runtimeBoundaryNftTable, "chain": chain, "handle": 3,
				"expr": []any{map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"meta": map[string]any{"key": key}}, "right": runtimeBoundaryBridgePattern}},
					map[string]any{"drop": nil}}}})
		}
	}
	content, err := json.Marshal(map[string]any{"nftables": objects})
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// The full admission path requires disk, id delegation and live network proof.
func TestRuntimeResourceBoundaryRequiresAllNativeProofs(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	boundary, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host)
	if err != nil {
		t.Fatal(err)
	}
	if boundary.WorkDeviceId != fixture.host.facts.BlockDeviceId || boundary.ContainerRootUid != 200000 || boundary.ContainerRootGid != 300000 ||
		boundary.ContainerIdMapLength != 65536 || boundary.PrivateSocketPath != fixture.boundary.PrivateSocketPath {
		t.Fatal("admission lost concrete block, engine or remap identity")
	}
	if fixture.host.events[len(fixture.host.events)-3] != "kernel" || fixture.host.events[len(fixture.host.events)-2] != "daemon" || fixture.host.events[len(fixture.host.events)-1] != "network" {
		t.Fatal("live network proof did not follow kernel proof")
	}
}

// The qualified host uses stock runc with explicitly empty arguments. Docker
// reserves that name, so its daemon config must not redefine runtimes.runc.
func TestRuntimeResourceBoundaryAcceptsOnlyExplicitStockRuncProfile(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	document := runtimeBoundaryEngineDocument(fixture.boundary, fixture.settings)
	if document["default-runtime"] != "runc" || document["runtimes"] != nil {
		t.Fatal("private engine silently changed or redefined the stock runtime")
	}
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err != nil {
		t.Fatalf("explicit empty-argument runc profile was rejected: %v", err)
	}
	document["runtimes"] = map[string]any{"runc": map[string]any{"path": fixture.boundary.Runc.Path}}
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	fixture.boundary.EngineConfig = fixture.document("engine.json", content)
	fixture.commit()
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("reserved runtimes.runc redefinition was admitted")
	}
}

// Every authority failure stops admission; another daemon is never attempted.
func TestRuntimeResourceBoundaryProtectedFailureHasNoFallback(t *testing.T) {
	for _, failure := range []string{"kernel", "daemon", "network", "/opt/synthetic-private-engine/bin/runc", "/srv/synthetic-evaluation/state", "/etc/synthetic-private-engine/network.nft"} {
		fixture := newRuntimeBoundaryTest(t)
		fixture.host.fail = failure
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatalf("native proof failure admitted: %s", failure)
		}
	}
}

// Only one bounded filesystem may contain every candidate-writable engine root.
func TestRuntimeResourceBoundaryRejectsUnboundedOrOverlappingRoots(t *testing.T) {
	for _, change := range []func(*RuntimeResourceBoundary){
		func(boundary *RuntimeResourceBoundary) { boundary.CandidateRoot = boundary.BaseDirectory },
		func(boundary *RuntimeResourceBoundary) { boundary.DockerDataRoot = "/var/lib/docker" },
		func(boundary *RuntimeResourceBoundary) { boundary.ScratchRoot = boundary.DockerDataRoot },
		func(boundary *RuntimeResourceBoundary) { boundary.WorkRoot = boundary.BaseDirectory + "/jobs" },
		func(boundary *RuntimeResourceBoundary) {
			boundary.PrivateSocketPath = boundary.ScratchRoot + "/docker.sock"
		},
		func(boundary *RuntimeResourceBoundary) { boundary.PrivateSocketPath = "/run/docker.sock" },
		func(boundary *RuntimeResourceBoundary) { boundary.ContainerdSocketPath = boundary.PrivateSocketPath },
		func(boundary *RuntimeResourceBoundary) {
			boundary.BackingFile = boundary.CandidateRoot + "/backing.img"
		},
		func(boundary *RuntimeResourceBoundary) {
			boundary.EngineConfig.Path = boundary.CandidateRoot + "/engine.json"
		},
		func(boundary *RuntimeResourceBoundary) { boundary.BlockDevice = "/dev/synthetic-disk" },
		func(boundary *RuntimeResourceBoundary) { boundary.RuntimeArguments = nil },
		func(boundary *RuntimeResourceBoundary) { boundary.RuntimeName = "runsc" },
		func(boundary *RuntimeResourceBoundary) {
			boundary.RuntimeArguments = []string{"--root=/srv/synthetic-host-authority"}
		},
	} {
		fixture := newRuntimeBoundaryTest(t)
		change(&fixture.boundary)
		fixture.commit()
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("unbounded, shared or overlapping runtime authority was admitted")
		}
	}
}

// A manifest number cannot substitute for kernel capacity or backing allocation.
func TestRuntimeResourceBoundaryRejectsQuotaAndBackingLies(t *testing.T) {
	for _, change := range []func(*runtimeBoundaryFacts){
		func(facts *runtimeBoundaryFacts) { facts.BlockBytes++ },
		func(facts *runtimeBoundaryFacts) { facts.BackingBytes-- },
		func(facts *runtimeBoundaryFacts) { facts.BackingAllocatedBytes-- },
		func(facts *runtimeBoundaryFacts) { facts.LoopBackingInode++ },
		func(facts *runtimeBoundaryFacts) { facts.LoopBackingDevice++ },
		func(facts *runtimeBoundaryFacts) { facts.LoopOffset = 512 },
		func(facts *runtimeBoundaryFacts) { facts.LoopSizeLimit = 1 },
		func(facts *runtimeBoundaryFacts) { facts.LoopNumber++ },
		func(facts *runtimeBoundaryFacts) { facts.LoopFlags = 1 },
		func(facts *runtimeBoundaryFacts) { facts.FilesystemType = 0 },
		func(facts *runtimeBoundaryFacts) { facts.BackingFilesystemType = 0 },
		func(facts *runtimeBoundaryFacts) { facts.FilesystemBytes = facts.BlockBytes + 1 },
		func(facts *runtimeBoundaryFacts) { facts.FreeBytes = 0 },
		func(facts *runtimeBoundaryFacts) { facts.BackingFreeBytes = 0 },
	} {
		fixture := newRuntimeBoundaryTest(t)
		change(&fixture.host.facts)
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("unproved hard quota or backing allocation was admitted")
		}
	}
}

// tmpfs, bind, stacked and shared mount bypasses are rejected before execution.
func TestRuntimeResourceBoundaryRejectsForeignOrChangingMounts(t *testing.T) {
	for _, change := range []func([]byte, RuntimeResourceBoundary) []byte{
		func(content []byte, boundary RuntimeResourceBoundary) []byte {
			return append(content, []byte("3 2 0:42 / "+boundary.WorkRoot+" rw - tmpfs synthetic rw\n")...)
		},
		func(content []byte, _ RuntimeResourceBoundary) []byte {
			return bytes.Replace(content, []byte("7:31 / "), []byte("7:31 /subtree "), 1)
		},
		func(content []byte, _ RuntimeResourceBoundary) []byte {
			return bytes.Replace(content, []byte("rw,nosuid,nodev -"), []byte("rw,nosuid,nodev shared:1 -"), 1)
		},
		func(content []byte, _ RuntimeResourceBoundary) []byte {
			return bytes.Replace(content, []byte("rw,nosuid,nodev"), []byte("rw,nodev"), 1)
		},
		func(content []byte, _ RuntimeResourceBoundary) []byte {
			return bytes.Replace(content, []byte("- ext4 /dev/loop31"), []byte("- xfs /dev/loop31"), 1)
		},
		func(content []byte, _ RuntimeResourceBoundary) []byte {
			return append(content, []byte("3 1 7:31 / /srv/synthetic-alias rw - ext4 /dev/loop31 rw\n")...)
		},
		func(content []byte, boundary RuntimeResourceBoundary) []byte {
			return append(content, []byte("3 2 7:31 / "+boundary.CandidateRoot+" rw,nosuid,nodev - ext4 /dev/loop31 rw\n")...)
		},
		func(content []byte, _ RuntimeResourceBoundary) []byte {
			return bytes.Replace(content, []byte("8:1 / /"), []byte("7:31 / /"), 1)
		},
	} {
		fixture := newRuntimeBoundaryTest(t)
		fixture.host.facts.MountInfo = change(fixture.host.facts.MountInfo, fixture.boundary)
		fixture.host.facts.MountInfoAfter = append([]byte(nil), fixture.host.facts.MountInfo...)
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("mount bypass was admitted")
		}
	}
	fixture := newRuntimeBoundaryTest(t)
	fixture.host.facts.MountInfoAfter = append(fixture.host.facts.MountInfoAfter, '\n')
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("mount generation change was admitted")
	}
}

// The mount record and every already-opened directory must agree on the device.
func TestRuntimeResourceBoundaryRejectsEscapedDirectoryDevices(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	for path := range fixture.host.facts.PathDeviceIds {
		current := newRuntimeBoundaryTest(t)
		if current.host.facts.PathDeviceIds[path] == current.host.facts.BlockDeviceId {
			current.host.facts.PathDeviceIds[path]++
		} else {
			current.host.facts.PathDeviceIds[path] = current.host.facts.BlockDeviceId
		}
		if _, err := verifyRuntimeResourceBoundary(context.Background(), current.pin, current.scope, current.settings, current.host); err == nil {
			t.Fatalf("directory on foreign authority device was admitted: %s", path)
		}
	}
}

// Frozen ranges must cover both finite output ids and never include host root,
// worker/operator authority, wraparound or a second delegated target range.
func TestRuntimeResourceBoundaryRejectsUnsafeMappedIds(t *testing.T) {
	for _, change := range []func(*RuntimeResourceBoundary, RuntimeResourceScope){
		func(boundary *RuntimeResourceBoundary, _ RuntimeResourceScope) { boundary.ContainerRootUid = 0 },
		func(boundary *RuntimeResourceBoundary, _ RuntimeResourceScope) { boundary.ContainerRootGid = 0 },
		func(boundary *RuntimeResourceBoundary, _ RuntimeResourceScope) { boundary.ContainerIdMapLength = 65532 },
		func(boundary *RuntimeResourceBoundary, _ RuntimeResourceScope) {
			boundary.ContainerRootUid = math.MaxUint32 - 10
		},
		func(boundary *RuntimeResourceBoundary, scope RuntimeResourceScope) {
			boundary.ContainerRootUid = scope.WorkerUid
		},
		func(boundary *RuntimeResourceBoundary, scope RuntimeResourceScope) {
			boundary.ContainerRootGid = scope.WorkerGid
		},
		func(boundary *RuntimeResourceBoundary, scope RuntimeResourceScope) {
			boundary.ContainerRootUid = scope.TrustedOperatorUid
		},
	} {
		fixture := newRuntimeBoundaryTest(t)
		change(&fixture.boundary, fixture.scope)
		fixture.commit()
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("unsafe mapped root allocation was admitted")
		}
	}
	for _, delegation := range []string{"", "synthetic_evaluation:200001:65536\n", "synthetic_evaluation:200000:65536\nsynthetic_evaluation:400000:65536\n",
		"synthetic_evaluation:200000:65536\nsynthetic_other:210000:65536\n", "synthetic_evaluation:0:65536\n", "synthetic_evaluation:4294967290:65536\n"} {
		fixture := newRuntimeBoundaryTest(t)
		fixture.host.facts.SubUids = []byte(delegation)
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("changed native subordinate-id allocation was admitted")
		}
	}
}

// An on-disk pin cannot hide a changed runtime, userns, bridge or storage root.
func TestRuntimeResourceBoundaryRejectsEngineConfigurationExpansion(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(document map[string]any) { document["data-root"] = "/var/lib/docker" },
		func(document map[string]any) { document["hosts"] = []any{"unix:///run/docker.sock"} },
		func(document map[string]any) { document["userns-remap"] = "" },
		func(document map[string]any) { document["default-runtime"] = "runsc" },
		func(document map[string]any) { document["bridge"] = "docker0" },
		func(document map[string]any) { document["iptables"] = true },
		func(document map[string]any) { document["experimental"] = true },
	} {
		fixture := newRuntimeBoundaryTest(t)
		document := runtimeBoundaryEngineDocument(fixture.boundary, fixture.settings)
		change(document)
		content, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		fixture.boundary.EngineConfig = fixture.document("engine.json", content)
		fixture.commit()
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("authenticated but unsafe private-engine configuration was admitted")
		}
	}
	fixture := newRuntimeBoundaryTest(t)
	fixture.boundary.ContainerdConfig = fixture.document("containerd.toml", []byte("version = 3\nroot = \"/var/lib/containerd\"\n"))
	fixture.commit()
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("containerd state escaped the bounded filesystem")
	}
}

// A fresh Docker29 installation's overlayfs/containerd store is not equivalent
// to the frozen classic overlay2 remapped class, regardless of minor allowance.
func TestRuntimeResourceBoundaryRejectsActualDaemonClassMismatch(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(info map[string]any) { info["Driver"] = "overlayfs" },
		func(info map[string]any) { info["DockerRootDir"] = "/var/lib/docker" },
		func(info map[string]any) { info["CgroupDriver"] = "cgroupfs" },
		func(info map[string]any) { info["CgroupVersion"] = "1" },
		func(info map[string]any) { info["DefaultRuntime"] = "runsc" },
		func(info map[string]any) { info["SecurityOptions"] = []string{} },
		func(info map[string]any) { info["LiveRestoreEnabled"] = true },
		func(info map[string]any) {
			info["Containerd"].(map[string]any)["Address"] = "/run/containerd/containerd.sock"
		},
		func(info map[string]any) {
			info["Runtimes"].(map[string]any)["runc"].(map[string]any)["path"] = "/usr/bin/synthetic-other-runtime"
		},
		func(info map[string]any) {
			info["Runtimes"].(map[string]any)["runc"].(map[string]any)["runtimeArgs"] = []string{"--network=host"}
		},
	} {
		fixture := newRuntimeBoundaryTest(t)
		var info map[string]any
		if err := json.Unmarshal(fixture.host.daemon, &info); err != nil {
			t.Fatal(err)
		}
		change(info)
		var err error
		fixture.host.daemon, err = json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("actual private-daemon class mismatch was admitted")
		}
	}
}

// No iptables=false assertion replaces actual scoped drop rules in the kernel.
func TestRuntimeResourceBoundaryRejectsMissingOrExpandedNetworkGuard(t *testing.T) {
	for _, change := range []func([]byte) []byte{
		func(content []byte) []byte {
			return bytes.Replace(content, []byte(`"drop":null`), []byte(`"accept":null`), 1)
		},
		func(content []byte) []byte { return bytes.Replace(content, []byte(`"unev-*"`), []byte(`"docker0"`), 1) },
		func(content []byte) []byte {
			return bytes.Replace(content, []byte(`"prio":-300`), []byte(`"prio":0`), 1)
		},
		func(content []byte) []byte {
			return bytes.Replace(content, []byte(`"hook":"input"`), []byte(`"hook":"forward"`), 1)
		},
		func(content []byte) []byte { return []byte(`{"nftables":[]}`) },
		func(content []byte) []byte {
			return bytes.Replace(content, []byte(`"name":"urnetwork_subeval"`), []byte(`"name":"urnetwork_subeval","flags":["dormant"]`), 1)
		},
	} {
		fixture := newRuntimeBoundaryTest(t)
		fixture.host.network = change(fixture.host.network)
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("missing or weakened live network guard was admitted")
		}
	}
	fixture := newRuntimeBoundaryTest(t)
	fixture.boundary.NetworkPolicy = fixture.document("network.nft", []byte("table inet urnetwork_subeval {}\n"))
	fixture.commit()
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("weakened protected network policy was admitted")
	}
}

// Invalid settings and pre-cancellation refuse before any protected read.
func TestRuntimeResourceBoundaryBoundsAndCancellation(t *testing.T) {
	for _, change := range []func(*RuntimeResourceBoundarySettings){
		func(settings *RuntimeResourceBoundarySettings) { settings.DocumentBytes = 0 },
		func(settings *RuntimeResourceBoundarySettings) { settings.NativeBytes = 0 },
		func(settings *RuntimeResourceBoundarySettings) { settings.ExecutableBytes = 0 },
		func(settings *RuntimeResourceBoundarySettings) { settings.MountCount = 0 },
		func(settings *RuntimeResourceBoundarySettings) { settings.DocumentDepth = 0 },
		func(settings *RuntimeResourceBoundarySettings) { settings.CommandTimeout = 0 },
		func(settings *RuntimeResourceBoundarySettings) { settings.ContainerLogMaximumSize = "" },
		func(settings *RuntimeResourceBoundarySettings) { settings.ContainerLogMaximumFiles = 0 },
	} {
		fixture := newRuntimeBoundaryTest(t)
		change(&fixture.settings)
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil || len(fixture.host.events) != 0 {
			t.Fatal("invalid bound reached protected/native work")
		}
	}
	fixture := newRuntimeBoundaryTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := verifyRuntimeResourceBoundary(ctx, fixture.pin, fixture.scope, fixture.settings, fixture.host); !errors.Is(err, context.Canceled) || len(fixture.host.events) != 0 {
		t.Fatal("canceled admission performed protected/native work")
	}
	fixture = newRuntimeBoundaryTest(t)
	fixture.settings.MountCount = 1
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("examined mount count exceeded its bound")
	}
	fixture = newRuntimeBoundaryTest(t)
	fixture.settings.NativeBytes = 1
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("native inventory bytes exceeded their bound")
	}
}

// Actual daemon environment, not a unit-file assertion, must keep helper
// lookup and all candidate-amplified temporary data inside their fixed roots.
func TestRuntimeResourceBoundaryRejectsActualEnvironmentOverrides(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	environment := runtimeBoundaryEnvironment(fixture.boundary)
	encode := func(values map[string]string) []byte {
		var content []byte
		for name, value := range values {
			content = append(content, []byte(name+"="+value+"\x00")...)
		}
		return content
	}
	if err := validateRuntimeBoundaryEnvironment(encode(environment), fixture.boundary, fixture.settings); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]string){
		func(environment map[string]string) { environment["PATH"] = "/usr/bin:/bin" },
		func(environment map[string]string) { environment["TMPDIR"] = "/tmp" },
		func(environment map[string]string) { environment["DOCKER_TMPDIR"] = "/var/lib/docker/tmp" },
		func(environment map[string]string) { environment["HOME"] = "/root" },
		func(environment map[string]string) { environment["DOCKER_HOST"] = "unix:///run/docker.sock" },
		func(environment map[string]string) { environment["DOCKER_RAMDISK"] = "1" },
		func(environment map[string]string) {
			environment["CONTAINERD_ADDRESS"] = "/run/containerd/containerd.sock"
		},
		func(environment map[string]string) { environment["HTTP_PROXY"] = "http://synthetic-proxy.example" },
		func(environment map[string]string) { delete(environment, "TMPDIR") },
	} {
		current := runtimeBoundaryEnvironment(fixture.boundary)
		change(current)
		if err := validateRuntimeBoundaryEnvironment(encode(current), fixture.boundary, fixture.settings); err == nil {
			t.Fatal("private daemon environment escaped frozen helper/temp authority")
		}
	}
	duplicate := append(encode(environment), []byte("TMPDIR=/tmp\x00")...)
	if err := validateRuntimeBoundaryEnvironment(duplicate, fixture.boundary, fixture.settings); err == nil {
		t.Fatal("duplicate daemon environment override was admitted")
	}
}

// Kernel arithmetic is checked without issuing a filesystem operation.
func TestRuntimeResourceBoundaryCapacityArithmeticRejectsOverflow(t *testing.T) {
	for _, entry := range []struct {
		count uint64
		size  int64
	}{{count: math.MaxUint64, size: 512}, {count: 1, size: -1}, {count: 1, size: 0}} {
		if _, err := runtimeBoundaryBytes(entry.count, entry.size); err == nil {
			t.Fatal("invalid capacity arithmetic was accepted")
		}
	}
	if value, err := runtimeBoundaryBytes(4, 512); err != nil || value != 2048 {
		t.Fatal("valid kernel capacity arithmetic changed")
	}
}

// Duplicate/case-alias or unknown policy keys cannot select a second quota.
func TestRuntimeResourceBoundaryStrictManifestKeys(t *testing.T) {
	for _, suffix := range []string{`,"Maximum_Bytes":1}`, `,"maximum_bytes":1}`, `,"unexpected":true}`} {
		fixture := newRuntimeBoundaryTest(t)
		content := fixture.host.documents[fixture.pin.Path]
		content = append(append([]byte(nil), content[:len(content)-1]...), []byte(suffix)...)
		fixture.pin = fixture.document("boundary.json", content)
		if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
			t.Fatal("ambiguous resource-boundary manifest was admitted")
		}
	}
}
