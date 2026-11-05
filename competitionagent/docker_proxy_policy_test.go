//go:build linux

// Closed Docker admission is exercised through the real worker HTTP handler
// against fake daemons and private files. No Docker, service or host probe runs.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The shared fixture starts with the real direct-helper create schema and its
// frozen resource/namespace defaults, not a policy-bypassing artificial body.
func dockerPolicyTestCreate(t *testing.T, fixture *dockerProxyTest, mutate func(map[string]any)) *httptest.ResponseRecorder {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(fixture.create, &object); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(object)
	}
	content, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/v1.56/containers/create", bytes.NewReader(content)))
	return response
}

// A counter proves that rejected policy never reached even the fake daemon.
func dockerPolicyTestDaemon(t *testing.T, fixture *dockerProxyTest) *atomic.Int64 {
	t.Helper()
	calls := &atomic.Int64{}
	fixture.daemon(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusCreated) })
	return calls
}

// Directory creation is confined to the synthetic run or explicitly pinned
// local-fixture tree; the production policy never creates a mount source.
func dockerPolicyTestDirectory(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// Real direct metadata/source probes have readonly root, cap-drop all, nnp,
// pinned core-suppression profile and finite management CPU/memory/pid ceilings.
func TestDockerPolicyAcceptsActualBoundedDirectHelperSchema(t *testing.T) {
	fixture := newDockerProxyTest(t)
	calls := dockerPolicyTestDaemon(t, fixture)
	if response := dockerPolicyTestCreate(t, fixture, nil); response.Code != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("bounded direct helper was rejected: %d", response.Code)
	}
	for _, group := range fixture.plan.CgroupNames {
		if group == fixture.plan.BuildCgroupName {
			continue // Tracked check/compile stages have their own mandatory source/cache/output contract.
		}
		response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) { object["HostConfig"].(map[string]any)["CgroupParent"] = group })
		if response.Code != http.StatusCreated {
			t.Fatalf("exact precommitted full-R9 cgroup was rejected: %s", group)
		}
	}
}

// Missing labels and every privilege/namespace bypass fail before any daemon
// call, including explicit empty masks that disable Docker's default guards.
func TestDockerPolicyRejectsPrivilegeAndNamespaceExpansions(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(object map[string]any) { delete(object["Labels"].(map[string]any), dockerProxyRunLabel) },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["CgroupParent"] = "synthetic-foreign.slice"
		},
		func(object map[string]any) { object["HostConfig"].(map[string]any)["Privileged"] = true },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["ReadonlyRootfs"] = false },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["CapDrop"] = []string{} },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["CapAdd"] = []string{"SYS_ADMIN"} },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["PidMode"] = "host" },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["IpcMode"] = "host" },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["UTSMode"] = "host" },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["UsernsMode"] = "host" },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["CgroupnsMode"] = "host" },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["NetworkMode"] = "host" },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["NetworkMode"] = "container:synthetic"
		},
		func(object map[string]any) { object["HostConfig"].(map[string]any)["Runtime"] = "runsc" },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=unconfined"}
		},
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["SecurityOpt"] = []string{"no-new-privileges:false"}
		},
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Devices"] = []map[string]any{{"PathOnHost": "/dev/synthetic", "PathInContainer": "/dev/synthetic", "CgroupPermissions": "rwm"}}
		},
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["DeviceRequests"] = []map[string]any{{"Driver": "synthetic", "Count": -1}}
		},
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["VolumeDriver"] = "synthetic-external"
		},
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["VolumesFrom"] = []string{"synthetic-foreign"}
		},
		func(object map[string]any) { object["HostConfig"].(map[string]any)["PublishAllPorts"] = true },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["MaskedPaths"] = []string{} },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["ReadonlyPaths"] = []string{} },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Annotations"] = map[string]string{"synthetic.host.capability": "true"}
		},
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["ContainerIDFile"] = "/etc/synthetic-host-file"
		},
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["RestartPolicy"] = map[string]any{"Name": "always", "MaximumRetryCount": 0}
		},
		func(object map[string]any) { object["Volumes"] = map[string]any{"/synthetic-volume": map[string]any{}} },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["PortBindings"] = map[string]any{"12345/tcp": []any{}}
		},
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Annotations"] = map[string]string{"synthetic-capability": ""}
		},
	} {
		fixture := newDockerProxyTest(t)
		calls := dockerPolicyTestDaemon(t, fixture)
		if response := dockerPolicyTestCreate(t, fixture, mutate); response.Code != http.StatusForbidden || calls.Load() != 0 {
			t.Fatalf("privilege expansion reached fake daemon: status=%d calls=%d", response.Code, calls.Load())
		}
	}
}

// The body cannot reset a frozen cap, use swap, mix role cpus, or omit core1.
// The profile/limit source contract is separate from native pipe qualification.
func TestDockerPolicyRejectsUnboundedResources(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(object map[string]any) { object["HostConfig"].(map[string]any)["Memory"] = 0 },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Memory"] = int64(73 * 1024 * 1024 * 1024)
		},
		func(object map[string]any) { object["HostConfig"].(map[string]any)["MemorySwap"] = -1 },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["PidsLimit"] = 0 },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["PidsLimit"] = 65537 },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["CpusetCpus"] = "" },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["CpusetCpus"] = "0,2" },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["CpusetCpus"] = "4096" },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Ulimits"] = []map[string]any{{"Name": "core", "Soft": 0, "Hard": -1}}
		},
		func(object map[string]any) { object["HostConfig"].(map[string]any)["Ulimits"] = []map[string]any{} },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Ulimits"] = []map[string]any{{"Name": "core", "Soft": 1, "Hard": 1}, {"Name": "nofile", "Soft": 1048577, "Hard": 1048577}}
		},
		func(object map[string]any) { object["HostConfig"].(map[string]any)["OomKillDisable"] = true },
		func(object map[string]any) { object["HostConfig"].(map[string]any)["CpuQuota"] = -1 },
		func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Sysctls"] = map[string]string{"net.ipv4.ip_forward": "1"}
		},
	} {
		fixture := newDockerProxyTest(t)
		calls := dockerPolicyTestDaemon(t, fixture)
		if response := dockerPolicyTestCreate(t, fixture, mutate); response.Code != http.StatusForbidden || calls.Load() != 0 {
			t.Fatal("unbounded container resources reached the fake daemon")
		}
	}
	for _, cpuset := range []string{"0-4096", "0,0", "02", "4-2", "1e3", "9223372036854775807"} {
		if _, err := dockerPolicyCpuSet(cpuset, 4095); err == nil {
			t.Fatalf("unbounded or aliased CPU set was accepted: %s", cpuset)
		}
	}
}

// All actual source/input/output families are accepted, with no parent, child,
// arbitrary named volume, symlink or alternate run added to their authority.
func TestDockerPolicyAcceptsOnlyFiniteFullR9BindInventory(t *testing.T) {
	fixture := newDockerProxyTest(t)
	calls := dockerPolicyTestDaemon(t, fixture)
	evidence := filepath.Join(fixture.plan.ContextRoot, ".evidence-runtime")
	for _, role := range []string{"baseline", "candidate"} {
		for _, stage := range []string{"01", "02", "03", "04", "05", "06", "07", "08", "09"} {
			output := dockerPolicyTestDirectory(t, filepath.Join(evidence, "runs", role+"-"+stage, "output"))
			response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) {
				object["HostConfig"].(map[string]any)["Mounts"] = []map[string]any{{"Type": "bind", "Source": output, "Target": "/artifacts"}}
			})
			if response.Code != http.StatusCreated {
				t.Fatal("exact full-R9 output bind was rejected")
			}
		}
	}
	for _, sourceTarget := range []struct {
		source, target string
		readonly       bool
	}{
		{source: "input", target: "/input", readonly: true}, {source: "scorer-input", target: "/artifacts", readonly: true},
		{source: "evaluation-sources.A1b2C3d4/baseline", target: "/workspace", readonly: true},
		{source: "evaluation-sources.A1b2C3d4/candidate", target: "/workspace", readonly: true},
		{source: "baseline-score-output", target: "/score-output"}, {source: "candidate-score-output", target: "/score-output"},
	} {
		source := dockerPolicyTestDirectory(t, filepath.Join(evidence, sourceTarget.source))
		response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Mounts"] = []map[string]any{{"Type": "bind", "Source": source, "Target": sourceTarget.target, "ReadOnly": sourceTarget.readonly}}
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("exact workflow bind was rejected: %s", sourceTarget.source)
		}
	}
	if calls.Load() != 24 {
		t.Fatal("full-R9 fixture did not exercise the complete finite output vocabulary")
	}
}

// The only additional readonly sources are the explicitly frozen local
// config/vault fixtures and the protected PostgreSQL initializer file.
func TestDockerPolicyAcceptsExactPinnedReadonlyFixtures(t *testing.T) {
	fixture := newDockerProxyTest(t)
	dockerPolicyTestDaemon(t, fixture)
	config := dockerPolicyTestDirectory(t, filepath.Join(fixture.plan.BaseDirectory, "fixtures", "config-local"))
	vault := dockerPolicyTestDirectory(t, filepath.Join(fixture.plan.BaseDirectory, "fixtures", "vault-local"))
	initialization := filepath.Join(fixture.plan.BaseDirectory, "fixtures", "postgres-init.sh")
	if err := os.WriteFile(initialization, []byte("# synthetic immutable initializer\n"), 0400); err != nil {
		t.Fatal(err)
	}
	fixture.proxy.plan.ReadOnlyBindPaths = []string{config, vault, initialization}
	response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) {
		object["HostConfig"].(map[string]any)["Mounts"] = []map[string]any{
			{"Type": "bind", "Source": config, "Target": "/runtime/config/local", "ReadOnly": true},
			{"Type": "bind", "Source": vault, "Target": "/runtime/vault/local", "ReadOnly": true},
			{"Type": "bind", "Source": initialization, "Target": "/docker-entrypoint-initdb.d/10-competition-app.sh", "ReadOnly": true},
		}
	})
	if response.Code != http.StatusCreated {
		t.Fatal("exact pinned readonly local fixtures were rejected")
	}
}

// A pinned-looking readonly path cannot expose either ancestor or descendant
// owner authority. The old one-directional parent check missed child exposure.
func TestDockerPolicyRejectsAuthorityChildrenAndSymlinkSources(t *testing.T) {
	for _, sourceKind := range []string{"child", "parent", "control-child", "symlink", "input-writable", "arbitrary-output"} {
		fixture := newDockerProxyTest(t)
		calls := dockerPolicyTestDaemon(t, fixture)
		source := filepath.Join(fixture.plan.BaseDirectory, "fixtures", "local")
		target, readOnly := "/runtime/config/local", true
		switch sourceKind {
		case "child":
			source = filepath.Join(fixture.plan.StateDirectory, "private-child")
		case "parent":
			source = filepath.Dir(fixture.plan.StateDirectory)
		case "control-child":
			source = filepath.Join(filepath.Dir(fixture.plan.UpstreamSocketPath), "private-child")
		case "symlink":
			actual := dockerPolicyTestDirectory(t, source+"-actual")
			if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(actual, source); err != nil {
				t.Fatal(err)
			}
		case "input-writable":
			source, target, readOnly = filepath.Join(fixture.plan.ContextRoot, ".evidence-runtime", "input"), "/artifacts", false
		case "arbitrary-output":
			source, target, readOnly = filepath.Join(fixture.plan.ContextRoot, ".evidence-runtime", "runs", "candidate-10", "output"), "/artifacts", false
		}
		if sourceKind != "symlink" {
			dockerPolicyTestDirectory(t, source)
		}
		fixture.proxy.plan.ReadOnlyBindPaths = append(fixture.proxy.plan.ReadOnlyBindPaths, source)
		response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) {
			object["HostConfig"].(map[string]any)["Mounts"] = []map[string]any{{"Type": "bind", "Source": source, "Target": target, "ReadOnly": readOnly}}
		})
		if response.Code != http.StatusForbidden || calls.Load() != 0 {
			t.Fatalf("authority or path expansion was admitted: %s", sourceKind)
		}
	}
}

// Positive Compose scorer/runner/DB-shaped tmpfs options retain their actual
// mapped ids and frozen sizes. No custom filesystem or external driver runs.
func TestDockerPolicyAcceptsFrozenRoleTmpfsAndRejectsUnboundedMounts(t *testing.T) {
	for _, role := range []struct {
		user   string
		memory int64
		tmpfs  map[string]string
	}{
		{user: "65532:65532", memory: 72 * 1024 * 1024 * 1024, tmpfs: map[string]string{"/tmp": "rw,nosuid,nodev,noexec,uid=65532,gid=65532,mode=1770,size=2g", "/work": "rw,nosuid,nodev,uid=65532,gid=65532,mode=0700,size=8g"}},
		{user: "999:999", memory: 16 * 1024 * 1024 * 1024, tmpfs: map[string]string{"/var/lib/postgresql": "rw,nosuid,nodev,noexec,uid=999,gid=999,mode=0700,size=12g", "/var/run/postgresql": "rw,nosuid,nodev,noexec,uid=999,gid=999,mode=0750,size=16m", "/tmp": "rw,nosuid,nodev,noexec,uid=999,gid=999,mode=1770,size=128m"}},
		{user: "999:999", memory: 8 * 1024 * 1024 * 1024, tmpfs: map[string]string{"/data": "rw,nosuid,nodev,noexec,uid=999,gid=999,mode=0700,size=6g", "/tmp": "rw,nosuid,nodev,noexec,uid=999,gid=999,mode=1770,size=64m"}},
		{user: "65532:65532", memory: 4 * 1024 * 1024 * 1024, tmpfs: map[string]string{"/tmp": "rw,nosuid,nodev,noexec,uid=65532,gid=65532,mode=1770,size=1g"}},
	} {
		fixture := newDockerProxyTest(t)
		dockerPolicyTestDaemon(t, fixture)
		response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) {
			object["User"] = role.user
			host := object["HostConfig"].(map[string]any)
			host["Memory"], host["MemorySwap"], host["Tmpfs"], host["CpusetCpus"] = role.memory, role.memory, role.tmpfs, fixture.plan.CpuSet
			host["Ulimits"] = []map[string]any{{"Name": "core", "Soft": 1, "Hard": 1}, {"Name": "nofile", "Soft": 1048576, "Hard": 1048576}}
		})
		if response.Code != http.StatusCreated {
			t.Fatal("actual frozen role tmpfs/resource schema was rejected")
		}
	}
	for _, mutate := range []func(map[string]any){
		func(host map[string]any) {
			host["Tmpfs"] = map[string]string{"/tmp": "rw,nosuid,nodev,uid=65532,gid=65532,mode=1770"}
		},
		func(host map[string]any) {
			host["Tmpfs"] = map[string]string{"/tmp": "rw,nosuid,nodev,uid=65532,gid=65532,mode=1770,size=999999999999999999999g"}
		},
		func(host map[string]any) {
			host["Tmpfs"] = map[string]string{"/tmp": "rw,suid,nodev,uid=65532,gid=65532,mode=1770,size=1g"}
		},
		func(host map[string]any) {
			host["Tmpfs"] = map[string]string{"/var/log": "rw,nosuid,nodev,uid=65532,gid=65532,mode=1770,size=1g"}
		},
		func(host map[string]any) {
			host["Mounts"] = []map[string]any{{"Type": "volume", "Source": "synthetic-external-volume", "Target": "/artifacts"}}
		},
		func(host map[string]any) {
			host["Mounts"] = []map[string]any{{"Type": "image", "Source": "synthetic-image", "Target": "/workspace"}}
		},
		func(host map[string]any) { host["Binds"] = []string{"synthetic-volume:/artifacts:rw"} },
	} {
		fixture := newDockerProxyTest(t)
		calls := dockerPolicyTestDaemon(t, fixture)
		if response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) { mutate(object["HostConfig"].(map[string]any)) }); response.Code != http.StatusForbidden || calls.Load() != 0 {
			t.Fatal("unbounded filesystem request reached the fake daemon")
		}
	}
}

// Compose's internal local bridge keeps only the fixed isolated gateway/name
// options; label ownership alone cannot authorize another network class.
func dockerPolicyTestNetwork(fixture *dockerProxyTest) map[string]any {
	return map[string]any{"Name": "synthetic_stage_evaluation", "Id": strings.Repeat("9", 64), "Driver": "bridge", "Scope": "local",
		"Internal": true, "EnableIPv4": true, "EnableIPv6": false, "Attachable": false, "Ingress": false, "ConfigOnly": false,
		"IPAM":    map[string]any{"Driver": "default", "Options": map[string]string{}, "Config": nil},
		"Options": map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "isolated", "com.docker.network.bridge.gateway_mode_ipv6": "isolated", "com.docker.network.bridge.name": "unev-" + fixture.plan.RunId[:10]},
		"Labels":  map[string]string{cleanupJobLabel: fixture.plan.JobId, cleanupRoundLabel: fixture.plan.RoundId, dockerProxyRunLabel: fixture.plan.RunId}}
}

// Request-shaped network bodies omit inspect identity and preserve the real
// Compose IPAM/driver schema; all denied requests stop before mutation.
func TestDockerPolicyNetworkCreateAndLiveEndpointAreStrict(t *testing.T) {
	fixture := newDockerProxyTest(t)
	network := dockerPolicyTestNetwork(fixture)
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(network)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) {
		object["HostConfig"].(map[string]any)["NetworkMode"] = network["Name"]
		object["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{network["Name"].(string): map[string]any{"NetworkID": network["Id"], "Aliases": []string{"runner"}, "IPAMConfig": nil, "Links": nil, "DriverOpts": nil}}}
	})
	if response.Code != http.StatusCreated {
		t.Fatal("actual owned Compose endpoint schema was rejected")
	}
	for _, change := range []func(map[string]any){
		func(network map[string]any) { network["Internal"] = false },
		func(network map[string]any) { network["Driver"] = "overlay" },
		func(network map[string]any) { network["Scope"] = "swarm" },
		func(network map[string]any) { network["EnableIPv6"] = true },
		func(network map[string]any) { network["Attachable"] = true },
		func(network map[string]any) { network["Ingress"] = true },
		func(network map[string]any) {
			network["Options"].(map[string]string)["com.docker.network.bridge.name"] = "synthetic-other"
		},
		func(network map[string]any) { network["IPAM"].(map[string]any)["Driver"] = "synthetic-external" },
	} {
		current := newDockerProxyTest(t)
		calls := dockerPolicyTestDaemon(t, current)
		document := dockerPolicyTestNetwork(current)
		delete(document, "Id")
		change(document)
		content, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		current.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/networks/create", bytes.NewReader(content)))
		if response.Code != http.StatusForbidden || calls.Load() != 0 {
			t.Fatal("network class expansion reached fake daemon")
		}
	}
	requestNetwork := dockerPolicyTestNetwork(fixture)
	delete(requestNetwork, "Id")
	content, err := json.Marshal(requestNetwork)
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/networks/create", bytes.NewReader(content)))
	if response.Code != http.StatusCreated {
		t.Fatal("actual isolated network-create schema was rejected")
	}
}

// Owning a target does not authorize privileged exec or switching its intended
// user. Empty User inherits the already admitted container's frozen identity.
func TestDockerPolicyExecCannotOverridePrivilegeOrUser(t *testing.T) {
	fixture := newDockerProxyTest(t)
	id := strings.Repeat("8", 64)
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Labels": map[string]string{cleanupJobLabel: fixture.plan.JobId, cleanupRoundLabel: fixture.plan.RoundId, dockerProxyRunLabel: fixture.plan.RunId}},
				"HostConfig": map[string]any{"CgroupParent": fixture.plan.CgroupNames[1]}})
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	for _, config := range []map[string]any{
		{"AttachStdout": true, "AttachStderr": true, "Cmd": []string{"/usr/bin/jq", "-c", ".", "/opt/urnetwork/identity.json"}},
		{"Cmd": []string{"synthetic"}, "Privileged": true},
		{"Cmd": []string{"synthetic"}, "User": "0:0"},
		{"Cmd": []string{"synthetic"}, "User": "65532:65532"},
	} {
		content, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/containers/"+id+"/exec", bytes.NewReader(content)))
		expected := http.StatusForbidden
		if config["Privileged"] == nil && config["User"] == nil {
			expected = http.StatusCreated
		}
		if response.Code != expected {
			t.Fatal("Docker exec privilege/user override policy changed")
		}
	}
}

// Marshaled zero fields are compatible, but an unknown capability and tiny
// nonzero exponent may not masquerade as zero through floating-point underflow.
func TestDockerPolicyRealClientZerosDoNotWaiveCapabilities(t *testing.T) {
	fixture := newDockerProxyTest(t)
	dockerPolicyTestDaemon(t, fixture)
	response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) {
		object["HostConfig"].(map[string]any)["ConsoleSize"] = []int{0, 0}
		object["HostConfig"].(map[string]any)["MemoryReservation"] = 0
		object["HostConfig"].(map[string]any)["CpuPeriod"] = 0
		object["HostConfig"].(map[string]any)["OomKillDisable"] = false
	})
	if response.Code != http.StatusCreated {
		t.Fatal("real Docker client default-zero schema was rejected")
	}
	for _, raw := range []string{"1e-99999", "true", `{"unreviewed":true}`, "null false"} {
		if dockerPolicyZero(json.RawMessage(raw)) {
			t.Fatalf("unreviewed nonzero capability was treated as zero: %s", raw)
		}
	}
}
