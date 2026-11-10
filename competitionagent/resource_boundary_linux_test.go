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
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
	namespace *runtimeBoundaryTestNamespace
}

// Fake receipts own no native namespace, but retain close/recheck state so
// error paths and read-only command custody can be tested without host calls.
type runtimeBoundaryTestNamespace struct {
	closed        bool
	rechecks      int
	failAt        int
	calls         [][]string
	links, routes []byte
}

// Returning a closed or failed generation is never treated as a fresh proof.
func (self *runtimeBoundaryTestNamespace) Recheck(ctx context.Context) error {
	self.rechecks++
	if self.closed || self.failAt > 0 && self.rechecks >= self.failAt {
		return errors.New("synthetic namespace generation failure")
	}
	return ctx.Err()
}

// Fake daemon facts are independently tested by the production pure validator.
func (self *runtimeBoundaryTestNamespace) Peer(int, Pin) error { return nil }

// Output is entirely synthetic; no nsenter or ip process is started.
func (self *runtimeBoundaryTestNamespace) Run(ctx context.Context, _ Pin, arguments []string) ([]byte, error) {
	if err := self.Recheck(ctx); err != nil {
		return nil, err
	}
	self.calls = append(self.calls, append([]string(nil), arguments...))
	if len(arguments) > 2 && arguments[2] == "route" {
		return append([]byte(nil), self.routes...), nil
	}
	return append([]byte(nil), self.links...), nil
}

// The fake implements the same idempotent ownership edge without kernel state.
func (self *runtimeBoundaryTestNamespace) Close() error { self.closed = true; return nil }

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

// Captures are explicit; a later generation is not synthesized on recheck.
func (self *runtimeBoundaryTestHost) Namespace(ctx context.Context, _ RuntimeResourceBoundary, _ RuntimeResourceBoundarySettings) (runtimeBoundaryNamespace, error) {
	self.events = append(self.events, "namespace")
	if self.fail == "namespace" {
		return nil, errors.New("synthetic namespace capture failure")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return self.namespace, nil
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
		RuntimeName: "runc", RuntimeArguments: []string{}, ManagementCpuSet: "2,4"}
	boundary.ContainerdVersion, boundary.ContainerdSourceCommit = "2.2.1", strings.Repeat("c", 40)
	executable := func(name string) Pin {
		return Pin{Path: "/opt/synthetic-private-engine/bin/" + name, Sha256: strings.Repeat("a", 64)}
	}
	boundary.Dockerd, boundary.Containerd, boundary.Runc, boundary.ContainerdShim, boundary.Nftables = executable("dockerd"), executable("containerd"), executable("runc"), executable("containerd-shim-runc-v2"), executable("nft")
	boundary.SocketModeHelper = Pin{Path: "/usr/bin/chmod", Sha256: strings.Repeat("b", 64)}
	boundary.NamespaceKeeper, boundary.Systemctl, boundary.Nsenter, boundary.Ip = executable("sleep"), executable("systemctl"), executable("nsenter"), executable("ip")
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
	for _, name := range []string{runtimeBoundaryKeeperUnitName, runtimeBoundaryEngineUnitName, runtimeBoundaryContainerdUnitName, runtimeBoundaryNetworkUnitName} {
		path := "/etc/systemd/system/" + name
		content := []byte("synthetic protected unit\n")
		digest := sha256.Sum256(content)
		host.documents[path] = content
		pin := Pin{Path: path, Sha256: hex.EncodeToString(digest[:])}
		switch name {
		case runtimeBoundaryKeeperUnitName:
			fixture.boundary.NamespaceKeeperUnit = pin
		case runtimeBoundaryEngineUnitName:
			fixture.boundary.EngineUnit = pin
		case runtimeBoundaryContainerdUnitName:
			fixture.boundary.ContainerdUnit = pin
		case runtimeBoundaryNetworkUnitName:
			fixture.boundary.NetworkUnit = pin
		}
	}
	host.namespace = &runtimeBoundaryTestNamespace{links: []byte(`[{"ifindex":1,"ifname":"lo","link_type":"loopback","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]}]`), routes: []byte("[]")}
	fixture.commit()
	const blockDevice = uint64(7<<8 | 31)
	mountInfo := []byte(fmt.Sprintf("1 0 8:1 / / rw - ext4 /dev/synthetic-root rw\n2 1 7:31 / %s rw,nosuid,nodev - ext4 /dev/loop31 rw\n", candidate))
	host.facts = runtimeBoundaryFacts{MountInfo: mountInfo, MountInfoAfter: append([]byte(nil), mountInfo...),
		CorePattern: []byte("core.%P\n"), CorePatternAfter: []byte("core.%P\n"),
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
		"Containerd": map[string]any{"Address": boundary.ContainerdSocketPath, "Namespaces": map[string]any{"Containers": "urnetwork-evaluation", "Plugins": fmt.Sprintf("plugins.moby-%d.%d", boundary.ContainerRootUid, boundary.ContainerRootGid)}},
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
			objects = append(objects, map[string]any{"rule": map[string]any{"family": "inet", "table": runtimeBoundaryNftTable, "chain": chain, "handle": 3,
				"expr": []any{map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"meta": map[string]any{"key": "iifname"}}, "right": runtimeBoundaryBridgePattern}},
					map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"meta": map[string]any{"key": "oifname"}}, "right": runtimeBoundaryBridgePattern}},
					map[string]any{"accept": nil}}}})
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
	if fixture.host.events[len(fixture.host.events)-4] != "kernel" || fixture.host.events[len(fixture.host.events)-3] != "namespace" ||
		fixture.host.events[len(fixture.host.events)-2] != "daemon" || fixture.host.events[len(fixture.host.events)-1] != "network" {
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
		func(settings *RuntimeResourceBoundarySettings) { settings.MaximumNetworkInterfaces = 0 },
		func(settings *RuntimeResourceBoundarySettings) { settings.MaximumNetworkRoutes = 0 },
		func(settings *RuntimeResourceBoundarySettings) { settings.MaximumRuntimeProcesses = 0 },
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

// Even a root-owned daemon's noisy diagnostics must consume only the private
// quota; a correct unit template is not proof of the actual open descriptors.
func TestRuntimeResourceBoundaryRejectsUnboundedLiveDaemonLogs(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	fixture.boundary.WorkDeviceId = fixture.host.facts.BlockDeviceId
	logs := []runtimeBoundaryLog{
		{Target: filepath.Join(fixture.boundary.CandidateRoot, "daemon-logs", "dockerd.stdout.log"), Stream: "stdout", DeviceId: fixture.boundary.WorkDeviceId, Mode: syscall.S_IFREG | 0600},
		{Target: filepath.Join(fixture.boundary.CandidateRoot, "daemon-logs", "dockerd.stderr.log"), Stream: "stderr", DeviceId: fixture.boundary.WorkDeviceId, Mode: syscall.S_IFREG | 0600},
	}
	if err := validateRuntimeBoundaryLogs(logs, "dockerd", fixture.boundary); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]runtimeBoundaryLog){
		func(logs []runtimeBoundaryLog) { logs[0].Target = "pipe:[123]" },
		func(logs []runtimeBoundaryLog) { logs[0].Target = "/var/log/synthetic-daemon.log" },
		func(logs []runtimeBoundaryLog) { logs[1].DeviceId++ },
		func(logs []runtimeBoundaryLog) { logs[1].Uid = fixture.scope.WorkerUid },
		func(logs []runtimeBoundaryLog) { logs[1].Mode = syscall.S_IFIFO | 0600 },
		func(logs []runtimeBoundaryLog) { logs[1].Mode |= 0020 },
	} {
		current := append([]runtimeBoundaryLog(nil), logs...)
		change(current)
		if err := validateRuntimeBoundaryLogs(current, "dockerd", fixture.boundary); err == nil {
			t.Fatal("unbounded actual daemon diagnostics were admitted")
		}
	}
}

// Deployment inputs are rendered only in memory. This exercises the shipped
// assets without installing a unit, formatting a device or executing a helper.
func TestRuntimeResourceBoundaryProvisioningTemplatesMatchVerifier(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	boundary := fixture.boundary
	boundary.Nftables.Path = "/opt/synthetic-system/sbin/nft"
	tokens := map[string]string{
		"BASE_DIRECTORY": boundary.BaseDirectory, "BACKING_FILE": boundary.BackingFile, "LOOP_NUMBER": "31",
		"MAXIMUM_BYTES": strconv.FormatUint(boundary.MaximumBytes, 10), "MINIMUM_FREE_BYTES": strconv.FormatUint(boundary.MinimumFreeBytes, 10),
		"MINIMUM_BACKING_FREE_BYTES": strconv.FormatUint(boundary.MinimumBackingFreeBytes, 10), "REMAP_ACCOUNT": boundary.UserNamespace,
		"CONTAINER_ROOT_UID": strconv.FormatUint(uint64(boundary.ContainerRootUid), 10), "CONTAINER_ROOT_GID": strconv.FormatUint(uint64(boundary.ContainerRootGid), 10),
		"CONTAINER_ID_MAP_LENGTH": strconv.FormatUint(uint64(boundary.ContainerIdMapLength), 10), "ENGINE_BIN": filepath.Dir(boundary.Runc.Path),
		"DOCKERD_SHA256": boundary.Dockerd.Sha256, "CONTAINERD_SHA256": boundary.Containerd.Sha256, "CONTAINERD_SHIM_SHA256": boundary.ContainerdShim.Sha256,
		"CONTAINERD_VERSION": boundary.ContainerdVersion, "CONTAINERD_SOURCE_COMMIT": boundary.ContainerdSourceCommit,
		"RUNC_SHA256": boundary.Runc.Sha256, "CHMOD_SHA256": boundary.SocketModeHelper.Sha256, "NFT_PATH": boundary.Nftables.Path, "NFT_SHA256": boundary.Nftables.Sha256,
		"NAMESPACE_KEEPER_PATH": boundary.NamespaceKeeper.Path, "NAMESPACE_KEEPER_SHA256": boundary.NamespaceKeeper.Sha256,
		"SYSTEMCTL_PATH": boundary.Systemctl.Path, "SYSTEMCTL_SHA256": boundary.Systemctl.Sha256,
		"NSENTER_PATH": boundary.Nsenter.Path, "NSENTER_SHA256": boundary.Nsenter.Sha256, "IP_PATH": boundary.Ip.Path, "IP_SHA256": boundary.Ip.Sha256,
		"NAMESPACE_KEEPER_UNIT_SHA256": boundary.NamespaceKeeperUnit.Sha256, "ENGINE_UNIT_SHA256": boundary.EngineUnit.Sha256,
		"CONTAINERD_UNIT_SHA256": boundary.ContainerdUnit.Sha256, "NETWORK_UNIT_SHA256": boundary.NetworkUnit.Sha256,
		"CONFIG_DIRECTORY": filepath.Dir(boundary.EngineConfig.Path), "CONTAINER_LOG_MAXIMUM_SIZE": fixture.settings.ContainerLogMaximumSize,
		"CONTAINER_LOG_MAXIMUM_FILES": strconv.Itoa(fixture.settings.ContainerLogMaximumFiles), "ENGINE_START_SECONDS": "30", "ENGINE_STOP_SECONDS": "30",
		"ENGINE_MEMORY_MAXIMUM": "1G", "ENGINE_CPU_QUOTA": "100%", "ENGINE_TASKS_MAXIMUM": "4096", "ENGINE_DESCRIPTOR_MAXIMUM": "65536", "MOUNT_START_SECONDS": "30",
		"MANAGEMENT_CPUSET":  boundary.ManagementCpuSet,
		"ENGINE_HELPER_PATH": runtimeBoundaryHelperPath(boundary),
		"PRIVATE_AGENT_PATH": "/usr/local/libexec/synthetic-agent", "PRIVATE_AGENT_CONFIG": "/etc/synthetic-agent.json",
	}
	unresolved := regexp.MustCompile(`@[A-Z0-9_]+@`)
	render := func(name string) []byte {
		content, err := os.ReadFile(filepath.Join("..", "cli", "competitionsubeval", "private-engine", name))
		if err != nil {
			t.Fatal(err)
		}
		document := string(content)
		for name, value := range tokens {
			document = strings.ReplaceAll(document, "@"+name+"@", value)
		}
		if unresolved.MatchString(document) {
			t.Fatalf("deployment template contains an unresolved token: %s", name)
		}
		return []byte(document)
	}
	engine, containerd, policy := render("engine.json.in"), render("containerd.toml.in"), render("network.nft")
	if err := validateRuntimeBoundaryDocuments(boundary, engine, containerd, policy, fixture.settings); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"ENGINE_CONFIG_SHA256": engine, "CONTAINERD_CONFIG_SHA256": containerd, "NETWORK_POLICY_SHA256": policy} {
		digest := sha256.Sum256(content)
		tokens[name] = hex.EncodeToString(digest[:])
	}
	var rendered RuntimeResourceBoundary
	if err := decodeStrict(render("boundary.json.in"), &rendered); err != nil {
		t.Fatal(err)
	}
	if err := rendered.validate(fixture.scope); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"urnetwork-private-engine.service.in", "urnetwork-private-containerd.service.in"} {
		unit := string(render(name))
		for _, required := range []string{"Restart=no", "Delegate=yes", "KillMode=control-group", "LimitCORE=0", "PATH=" + runtimeBoundaryHelperPath(boundary),
			"AllowedCPUs=" + boundary.ManagementCpuSet,
			"PrivateNetwork=yes", "BindsTo=" + runtimeBoundaryKeeperUnitName, "JoinsNamespaceOf=" + runtimeBoundaryKeeperUnitName,
			boundary.CandidateRoot + "/daemon-logs/", "/usr/bin/chmod 0600 " + filepath.Dir(boundary.PrivateSocketPath)} {
			if !strings.Contains(unit, required) {
				t.Fatalf("private service lost an enforced boundary: %s", name)
			}
		}
		if strings.Contains(unit, "/run/docker.sock") || strings.Contains(unit, "docker.service") {
			t.Fatal("private unit targets the user's engine")
		}
		if name == "urnetwork-private-engine.service.in" {
			if strings.Contains(unit, "BindPaths=") || !strings.Contains(unit, "ExecStart="+tokens["PRIVATE_AGENT_PATH"]+" --config "+tokens["PRIVATE_AGENT_CONFIG"]+" --start-private-engine") {
				t.Fatal("Docker must join containerd's image mount namespace through its pinned startup role")
			}
		} else if !strings.Contains(unit, "BindPaths="+boundary.EngineExecRoot+"/containerd-sockets:/run/containerd") {
			t.Fatal("containerd lost its private shim socket mount")
		}
	}
	_ = render("candidate.mount.in")
	if !strings.Contains(string(render("urnetwork-private-engine-network.service.in")), "AllowedCPUs="+boundary.ManagementCpuSet) {
		t.Fatal("network policy helper lost the management processor boundary")
	}
	keeper := string(render(runtimeBoundaryKeeperUnitName + ".in"))
	for _, required := range []string{"PrivateNetwork=yes", "ExecStart=" + boundary.NamespaceKeeper.Path + " infinity", "Restart=no", "KillMode=control-group", "AllowedCPUs=" + boundary.ManagementCpuSet} {
		if !strings.Contains(keeper, required) {
			t.Fatal("keeper template lost its stable private namespace lifetime")
		}
	}
}

// Positive namespace identities must agree across three distinct live roots;
// unit metadata alone cannot qualify host identity or a missing participant.
func TestRuntimeResourceBoundaryNamespacePeersRequireNonHostIdentity(t *testing.T) {
	private := runtimeBoundaryNamespaceId{Device: 4, Inode: 200}
	host := runtimeBoundaryNamespaceId{Device: 4, Inode: 100}
	makeUnits := func() map[string]runtimeBoundaryUnit {
		return map[string]runtimeBoundaryUnit{
			runtimeBoundaryKeeperUnitName:     {Pid: 101, ProcessStart: 1, Namespace: private},
			runtimeBoundaryEngineUnitName:     {Pid: 102, ProcessStart: 2, Namespace: private, MountNamespace: private},
			runtimeBoundaryContainerdUnitName: {Pid: 103, ProcessStart: 3, Namespace: private, MountNamespace: private},
		}
	}
	if err := validateRuntimeBoundaryNamespacePeers(makeUnits(), private, host); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]runtimeBoundaryUnit){
		func(units map[string]runtimeBoundaryUnit) { delete(units, runtimeBoundaryKeeperUnitName) },
		func(units map[string]runtimeBoundaryUnit) {
			unit := units[runtimeBoundaryEngineUnitName]
			unit.Namespace = host
			units[runtimeBoundaryEngineUnitName] = unit
		},
		func(units map[string]runtimeBoundaryUnit) {
			unit := units[runtimeBoundaryContainerdUnitName]
			unit.ProcessStart = 0
			units[runtimeBoundaryContainerdUnitName] = unit
		},
		func(units map[string]runtimeBoundaryUnit) {
			unit := units[runtimeBoundaryEngineUnitName]
			unit.Pid = 101
			units[runtimeBoundaryEngineUnitName] = unit
		},
	} {
		units := makeUnits()
		change(units)
		if err := validateRuntimeBoundaryNamespacePeers(units, private, host); err == nil {
			t.Fatal("missing, reused or foreign namespace participant was admitted")
		}
	}
	if err := validateRuntimeBoundaryNamespacePeers(makeUnits(), host, host); err == nil {
		t.Fatal("host namespace was accepted as private")
	}
}

// Synthetic systemd properties preserve the exact requested units and dependencies.
func runtimeBoundaryTestUnitProperties(boundary RuntimeResourceBoundary, pin Pin) map[string]string {
	name := filepath.Base(pin.Path)
	properties := map[string]string{"Id": name, "FragmentPath": pin.Path, "DropInPaths": "", "LoadState": "loaded", "ActiveState": "active", "SubState": "running",
		"UMask":   "0077",
		"MainPID": "101", "InvocationID": strings.Repeat("a", 32), "ExecMainStartTimestampMonotonic": "123",
		"ControlGroup": managementPrivateCgroupPath + "/" + name, "PrivateNetwork": "yes", "NetworkNamespacePath": "", "User": "root", "Group": "root",
		"Slice": managementPrivateSliceName, "OnFailure": managementRecoveryServiceName, "OnFailureJobMode": "replace",
		"Restart": "no", "KillMode": "control-group", "RemainAfterExit": "no", "JoinsNamespaceOf": runtimeBoundaryKeeperUnitName,
		"AllowedCPUs": strings.ReplaceAll(boundary.ManagementCpuSet, ",", " "),
		"Requires":    runtimeBoundaryKeeperUnitName, "BindsTo": runtimeBoundaryKeeperUnitName, "After": runtimeBoundaryKeeperUnitName}
	if name == runtimeBoundaryEngineUnitName || name == runtimeBoundaryContainerdUnitName {
		properties["UMask"] = "0022"
	}
	if name == runtimeBoundaryKeeperUnitName {
		properties["JoinsNamespaceOf"] = runtimeBoundaryEngineUnitName + " " + runtimeBoundaryContainerdUnitName + " " + runtimeBoundaryNetworkUnitName
	}
	if name == runtimeBoundaryNetworkUnitName {
		properties["MainPID"], properties["SubState"], properties["RemainAfterExit"] = "0", "exited", "yes"
	}
	return properties
}

// No real systemctl command runs; serialized native facts exercise the same parser.
func runtimeBoundaryTestUnitBytes(properties map[string]string) []byte {
	var content strings.Builder
	for key, value := range properties {
		content.WriteString(key + "=" + value + "\n")
	}
	return []byte(content.String())
}

// Drop-ins, host fallbacks, ambiguous namespace groups and unit restarts do not
// inherit the authority of an otherwise correctly pinned unit file.
func TestRuntimeResourceBoundaryPrivateUnitContract(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	for _, pin := range fixture.boundary.unitPins() {
		if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(runtimeBoundaryTestUnitProperties(fixture.boundary, pin)), pin, fixture.boundary, fixture.settings); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []func(map[string]string){
		func(properties map[string]string) { properties["PrivateNetwork"] = "no" },
		func(properties map[string]string) { properties["NetworkNamespacePath"] = "/proc/1/ns/net" },
		func(properties map[string]string) {
			properties["DropInPaths"] = "/etc/systemd/system/synthetic-override.conf"
		},
		func(properties map[string]string) { properties["JoinsNamespaceOf"] = "synthetic-foreign.service" },
		func(properties map[string]string) { properties["BindsTo"] = "" },
		func(properties map[string]string) { properties["After"] = "" },
		func(properties map[string]string) { properties["Restart"] = "always" },
		func(properties map[string]string) { properties["InvocationID"] = "" },
		func(properties map[string]string) { properties["SubState"] = "dead" },
	} {
		pin := fixture.boundary.EngineUnit
		properties := runtimeBoundaryTestUnitProperties(fixture.boundary, pin)
		change(properties)
		if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err == nil {
			t.Fatal("unqualified private namespace unit was admitted")
		}
	}
}

// Closing and failed-generation paths never leave a usable namespace receipt.
func TestRuntimeResourceBoundaryNamespaceReceiptClosesOnFailure(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	fixture.host.fail = "daemon"
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil || !fixture.host.namespace.closed {
		t.Fatal("failed admission retained namespace custody")
	}
	fixture = newRuntimeBoundaryTest(t)
	fixture.host.namespace.failAt = 1
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil || !fixture.host.namespace.closed {
		t.Fatal("failed recheck retained namespace custody")
	}
	fixture = newRuntimeBoundaryTest(t)
	boundary, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host)
	if err != nil {
		t.Fatal(err)
	}
	if err := boundary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := boundary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := boundary.Recheck(context.Background()); err == nil {
		t.Fatal("closed namespace receipt was renewed")
	}
	var absent *RuntimeResourceBoundary
	if err := absent.Close(); err != nil {
		t.Fatal(err)
	}
}

// Correct namespace membership cannot hide ordinary management placement or
// an absent/expanded PID1 fallback outside that parent's OOM failure domain.
func TestRuntimeResourceBoundaryPrivateUnitsRequireIndependentRecovery(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	for _, pin := range fixture.boundary.unitPins() {
		properties := runtimeBoundaryTestUnitProperties(fixture.boundary, pin)
		if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err != nil {
			t.Fatal("valid synthetic private unit failed its closed lifetime projection", err)
		}
		for _, change := range []struct{ key, value string }{
			{key: "Slice", value: "system.slice"}, {key: "OnFailure", value: ""},
			{key: "OnFailure", value: managementRecoveryServiceName + " synthetic-other.service"}, {key: "OnFailureJobMode", value: "ignore-dependencies"},
		} {
			mutated := make(map[string]string, len(properties))
			for key, value := range properties {
				mutated[key] = value
			}
			mutated[change.key] = change.value
			if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(mutated), pin, fixture.boundary, fixture.settings); err == nil {
				t.Fatal("private unit lost aggregate placement or fixed independent fallback", change.key)
			}
		}
	}
}

// A legacy system.slice process is not covered by the new eight-GiB aggregate,
// even if its independent CPU and network namespace facts happen to match.
func TestRuntimeResourceBoundaryRejectsLegacyPrivateManagementGroup(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	for _, pin := range []Pin{fixture.boundary.EngineUnit, fixture.boundary.ContainerdUnit, fixture.boundary.NamespaceKeeperUnit} {
		properties := runtimeBoundaryTestUnitProperties(fixture.boundary, pin)
		properties["ControlGroup"] = "/system.slice/" + filepath.Base(pin.Path)
		if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err == nil {
			t.Fatal("private manager accepted a legacy unaggregated process")
		}
		if runtimeBoundaryManagementGroup(runtimeBoundaryUnit{Name: filepath.Base(pin.Path), ControlGroup: properties["ControlGroup"]}) {
			t.Fatal("kernel membership accepted legacy placement outside the aggregate")
		}
	}
}

// Inert templates are inspected only. They neither start a daemon nor change a
// host's network, service, cgroup, core policy or currently running evaluator.
func TestRuntimeResourceBoundaryPrivateTemplatesArmFixedRecovery(t *testing.T) {
	for _, name := range []string{runtimeBoundaryEngineUnitName, runtimeBoundaryContainerdUnitName, runtimeBoundaryKeeperUnitName, runtimeBoundaryNetworkUnitName} {
		content, err := os.ReadFile(filepath.Join("..", "cli", "competitionsubeval", "private-engine", name+".in"))
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{"OnFailure=" + managementRecoveryServiceName, "OnFailureJobMode=replace", "Slice=" + managementPrivateSliceName} {
			if bytes.Count(content, []byte(expected+"\n")) != 1 {
				t.Fatal("private template omitted or duplicated fixed recovery ownership", name, expected)
			}
		}
		if bytes.Contains(content, []byte("Slice="+managementCleanupSliceName)) {
			t.Fatal("ordinary daemon entered cleanup's independent failure domain", name)
		}
	}
}

// The only allowed links are loopback and one isolated private cohort
// bridge with container veths enslaved to it; physical/tunnel/extra bridges fail.
func TestRuntimeResourceBoundaryOfflineInterfaceInventory(t *testing.T) {
	settings := DefaultRuntimeResourceBoundarySettings()
	valid := []byte(`[{"ifindex":1,"ifname":"lo","link_type":"loopback","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},{"ifindex":2,"ifname":"unev-0123456789","linkinfo":{"info_kind":"bridge"},"addr_info":[]},{"ifindex":3,"ifname":"veth0123abc","master":"unev-0123456789","link_netnsid":0,"linkinfo":{"info_kind":"veth"},"addr_info":[]}]`)
	if err := validateRuntimeBoundaryInterfaces(valid, settings); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		`[{"ifindex":1,"ifname":"lo","link_type":"loopback"},{"ifindex":2,"ifname":"eth0","link_type":"ether"}]`,
		`[{"ifindex":1,"ifname":"lo","link_type":"loopback"},{"ifindex":2,"ifname":"tun0","linkinfo":{"info_kind":"tun"}}]`,
		`[{"ifindex":1,"ifname":"lo","link_type":"loopback"},{"ifindex":2,"ifname":"veth0123abc","master":"synthetic-foreign","link_netnsid":0,"linkinfo":{"info_kind":"veth"}}]`,
		`[{"ifindex":1,"ifname":"lo","link_type":"loopback"},{"ifindex":2,"ifname":"unev-0123456789","linkinfo":{"info_kind":"bridge"},"addr_info":[{"family":"inet","local":"192.0.2.1","prefixlen":24}]}]`,
		`[{"ifindex":1,"ifname":"lo","link_type":"loopback"},{"ifindex":2,"ifname":"unev-0123456789","linkinfo":{"info_kind":"bridge"}},{"ifindex":3,"ifname":"unev-abcdef0123","linkinfo":{"info_kind":"bridge"}}]`,
	} {
		if err := validateRuntimeBoundaryInterfaces([]byte(content), settings); err == nil {
			t.Fatal("external or unreviewed private-namespace interface was admitted")
		}
	}
	settings.MaximumNetworkInterfaces = 1
	if err := validateRuntimeBoundaryInterfaces(valid, settings); err == nil {
		t.Fatal("interface inventory exceeded its bound")
	}
}

// Bridge and veth IPv6 link-local assignments have identical safety rules;
// IPv4, globally scoped, malformed and non-/64 addresses remain forbidden.
func TestRuntimeResourceBoundaryLinkLocalInterfaceInventory(t *testing.T) {
	settings := DefaultRuntimeResourceBoundarySettings()
	for _, target := range []string{"bridge", "veth"} {
		for _, address := range []string{
			`{"family":"inet6","local":"fe80::1","scope":"link","prefixlen":64}`,
			`{"family":"inet","local":"192.0.2.1","scope":"global","prefixlen":24}`,
			`{"family":"inet6","local":"2001:db8::1","scope":"global","prefixlen":64}`,
			`{"family":"inet6","local":"fe80::1","scope":"global","prefixlen":64}`,
			`{"family":"inet6","local":"fe80::1","scope":"link","prefixlen":32}`,
			`{"family":"inet6","local":"invalid","scope":"link","prefixlen":64}`,
		} {
			bridgeAddresses, vethAddresses := `[]`, `[]`
			if target == "bridge" {
				bridgeAddresses = `[` + address + `]`
			} else {
				vethAddresses = `[` + address + `]`
			}
			content := `[{"ifindex":1,"ifname":"lo","link_type":"loopback","addr_info":[]},{"ifindex":2,"ifname":"unev-0123456789","linkinfo":{"info_kind":"bridge"},"addr_info":` + bridgeAddresses + `},{"ifindex":3,"ifname":"veth0123abc","master":"unev-0123456789","link_netnsid":0,"linkinfo":{"info_kind":"veth"},"addr_info":` + vethAddresses + `}]`
			err := validateRuntimeBoundaryInterfaces([]byte(content), settings)
			wantAllowed := address == `{"family":"inet6","local":"fe80::1","scope":"link","prefixlen":64}`
			if (err == nil) != wantAllowed {
				t.Fatalf("%s address %s: allowed=%v, error=%v", target, address, wantAllowed, err)
			}
		}
	}
}

// Offline routes cannot install a gateway, default, multipath or foreign link.
func TestRuntimeResourceBoundaryOfflineRouteContract(t *testing.T) {
	settings := DefaultRuntimeResourceBoundarySettings()
	if err := validateRuntimeBoundaryRoutes([]byte(`[{"dst":"127.0.0.1","dev":"lo","type":"local"}]`), settings); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		`[{"dst":"default","dev":"unev-0123456789"}]`,
		`[{"dst":"192.0.2.0/24","dev":"eth0"}]`,
		`[{"dst":"192.0.2.0/24","dev":"unev-0123456789","gateway":"192.0.2.1"}]`,
		`[{"dst":"192.0.2.0/24","dev":"unev-0123456789","nexthops":[{"dev":"lo"}]}]`,
	} {
		if err := validateRuntimeBoundaryRoutes([]byte(content), settings); err == nil {
			t.Fatal("external private-namespace route was admitted")
		}
	}
}

// Bringing up an isolated peer creates local and multicast kernel routes, not
// an external gateway. Only these exact route classes may refer to a veth.
func TestRuntimeResourceBoundaryKernelVethRoutes(t *testing.T) {
	settings := DefaultRuntimeResourceBoundarySettings()
	for _, route := range []string{
		`{"dst":"fe80::/64","dev":"veth0123abc","protocol":"kernel"}`,
		`{"dst":"fe80::1","dev":"veth0123abc","protocol":"kernel","type":"local"}`,
		`{"dst":"ff00::/8","dev":"veth0123abc","protocol":"kernel","type":"multicast"}`,
	} {
		if err := validateRuntimeBoundaryRoutes([]byte(`[`+route+`]`), settings); err != nil {
			t.Fatal("isolated peer kernel route rejected", route, err)
		}
	}
	for _, route := range []string{
		`{"dst":"default","dev":"veth0123abc","protocol":"kernel"}`,
		`{"dst":"192.0.2.0/24","dev":"veth0123abc","protocol":"kernel"}`,
		`{"dst":"2001:db8::/64","dev":"veth0123abc","protocol":"kernel"}`,
		`{"dst":"fe80::/64","dev":"veth0123abc","protocol":"static"}`,
		`{"dst":"fe80::/64","dev":"veth0123abc","protocol":"kernel","gateway":"fe80::1"}`,
		`{"dst":"ff00::/8","dev":"veth0123abc","protocol":"kernel","type":"unicast"}`,
		`{"dst":"fe80::1","dev":"veth0123abc","protocol":"kernel","type":"unicast"}`,
		`{"dst":"fe80::/64","dev":"veth0123abc","protocol":"kernel","nexthops":[{"dev":"lo"}]}`,
	} {
		if err := validateRuntimeBoundaryRoutes([]byte(`[`+route+`]`), settings); err == nil {
			t.Fatal("routable or untrusted peer route admitted", route)
		}
	}
}

// A network fact consumer cannot select host namespace paths, PIDs, tools or
// mutating ip/nft commands; even a valid bridge fails after receipt closure.
func TestRuntimeResourceBoundaryNetworkLinkUsesCapturedReadOnlyReceipt(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	boundary, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host)
	if err != nil {
		t.Fatal(err)
	}
	defer boundary.Close()
	if _, err := (runtimeBoundaryNativeHost{}).NetworkLink(context.Background(), *boundary, "unev-0123456789", fixture.settings); err != nil {
		t.Fatal(err)
	}
	if len(fixture.host.namespace.calls) != 4 {
		t.Fatal("network proof skipped its bounded private interface/route inventory")
	}
	for _, arguments := range [][]string{{"link", "add", "synthetic"}, {"-j", "addr", "show", "dev", "lo"}, {"-j", "addr", "show", "dev", "/proc/1/ns/net"}} {
		if runtimeBoundaryNetworkCommandAllowed(*boundary, boundary.Ip, arguments) {
			t.Fatal("network fact consumer accepted a mutating or caller-selected target")
		}
	}
	if runtimeBoundaryNetworkCommandAllowed(*boundary, boundary.Nftables, []string{"flush", "ruleset"}) {
		t.Fatal("network proof accepted a mutating nft command")
	}
	if _, err := (runtimeBoundaryNativeHost{}).NetworkLink(context.Background(), *boundary, "/proc/1/ns/net", fixture.settings); err == nil {
		t.Fatal("network proof accepted a caller namespace path")
	}
	_ = boundary.Close()
	if _, err := (runtimeBoundaryNativeHost{}).NetworkLink(context.Background(), *boundary, "unev-0123456789", fixture.settings); err == nil {
		t.Fatal("network proof renewed a stopped owner")
	}
}

// Source/version and shim daemon arguments are explicit closed contracts, not
// inferred from an executable filename or a separately allowed Docker minor.
func TestRuntimeResourceBoundaryContainerdSourceAndShimContract(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	version := []byte("containerd github.com/containerd/containerd/v2 v" + fixture.boundary.ContainerdVersion + " " + fixture.boundary.ContainerdSourceCommit + "\n")
	if err := validateRuntimeBoundaryContainerdVersion(version, fixture.boundary, fixture.settings); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"containerd github.com/containerd/containerd/v2 v9.9.9 " + fixture.boundary.ContainerdSourceCommit,
		"containerd github.com/containerd/containerd/v2 v" + fixture.boundary.ContainerdVersion + " " + strings.Repeat("d", 40),
		"containerd synthetic-unreviewed-vendor v" + fixture.boundary.ContainerdVersion + " " + fixture.boundary.ContainerdSourceCommit} {
		if err := validateRuntimeBoundaryContainerdVersion([]byte(content), fixture.boundary, fixture.settings); err == nil {
			t.Fatal("unfrozen private containerd source/version was admitted")
		}
	}
	arguments := []string{fixture.boundary.ContainerdShim.Path, "-namespace", "urnetwork-evaluation", "-id", strings.Repeat("a", 64), "-address", fixture.boundary.ContainerdSocketPath}
	encode := func(arguments []string) []byte { return []byte(strings.Join(arguments, "\x00") + "\x00") }
	if err := validateRuntimeBoundaryShimArguments(encode(arguments), fixture.boundary, fixture.settings); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]string){
		func(arguments []string) { arguments[2] = "moby" },
		func(arguments []string) { arguments[4] = "synthetic-short-id" },
		func(arguments []string) { arguments[6] = "/run/containerd/containerd.sock" },
	} {
		current := append([]string(nil), arguments...)
		change(current)
		if err := validateRuntimeBoundaryShimArguments(encode(current), fixture.boundary, fixture.settings); err == nil {
			t.Fatal("private shim escaped its frozen namespace/id/address family")
		}
	}
}

// Process comm may contain spaces and parentheses; the native birth field
// cannot shift to another numeric stat token or accept a missing identity.
func TestRuntimeResourceBoundaryProcessBirthParser(t *testing.T) {
	content := []byte("101 (synthetic (worker)) S " + strings.Repeat("0 ", 18) + "123 0\n")
	if birth, err := runtimeBoundaryProcessStart(content); err != nil || birth != 123 {
		t.Fatal("native process birth parser used a comm token as stat identity")
	}
	for _, content := range []string{"", "101 synthetic 123", "101 (synthetic) S 0", "101 (synthetic) S " + strings.Repeat("0 ", 19)} {
		if _, err := runtimeBoundaryProcessStart([]byte(content)); err == nil {
			t.Fatal("incomplete process birth was admitted")
		}
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

// A zero-limit daemon cannot qualify an unbounded host pipe handler: the kernel
// ignores RLIMIT_CORE for pipes and executes the handler outside the workload.
func TestRuntimeResourceBoundaryRejectsPipedCorePolicyDespiteZeroLimits(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	limits := []byte("Limit                     Soft Limit           Hard Limit           Units\nMax core file size        0                    0                    bytes\n")
	if err := validateRuntimeBoundaryCoreLimits(limits, fixture.settings); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"|/usr/libexec/synthetic-core-handler %P\n", "|/bin/false\n", "|/usr/libexec/synthetic-bounded-handler %P\n"} {
		current := newRuntimeBoundaryTest(t)
		current.host.facts.CorePattern, current.host.facts.CorePatternAfter = []byte(pattern), []byte(pattern)
		if _, err := verifyRuntimeResourceBoundary(context.Background(), current.pin, current.scope, current.settings, current.host); err == nil {
			t.Fatal("zero limits or a handler-looking name waived the host pipe policy")
		}
	}
}

// Only the fixed slash-free filename policy is supported, and actual zero
// soft/hard limits remain mandatory. Neither condition alone is sufficient.
func TestRuntimeResourceBoundaryNonPipedCoreContract(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	if err := validateRuntimeBoundaryCorePattern([]byte("core.%P\n"), fixture.settings); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"", "core\n", "/var/log/synthetic-core.%P\n", "../core.%P\n", "cores/core.%P\n", "core.%e\n", " core.%P\n", "core.%P\n\n"} {
		if err := validateRuntimeBoundaryCorePattern([]byte(pattern), fixture.settings); err == nil {
			t.Fatal("unqualified non-piped core filename was admitted")
		}
	}
	for _, limits := range []string{"", "Max core file size 0 unlimited bytes\n", "Max core file size unlimited unlimited bytes\n", "Max core file size 1 1 bytes\n",
		"Max core file size 0 0 bytes\nMax core file size 0 0 bytes\n", "Max core file size 0 0 blocks\n"} {
		if err := validateRuntimeBoundaryCoreLimits([]byte(limits), fixture.settings); err == nil {
			t.Fatal("nonzero, absent or ambiguous native core limits were admitted")
		}
	}
	fixture.host.facts.CorePatternAfter = []byte("|/usr/libexec/synthetic-core-handler %P\n")
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("host core policy changed during admission without refusal")
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
