//go:build linux

// Native composed network diagnostic owns one isolated bridge and a trusted
// sleeping peer. It reads no submission, secret, production mount or job clock.
package competitionagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Invoke the real owned-object reader and pinned namespace inventory, not a
// replacement command or policy waiver. Exact fixture cleanup is unconditional.
func TestHostNetworkNative(t *testing.T) {
	path := os.Getenv("COMPETITION_NETWORK_NATIVE_CONFIG")
	if path == "" || os.Geteuid() != 0 {
		t.Skip("requires explicit root idle private network configuration")
	}
	config, err := LoadDockerProbeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := user.Lookup(config.WorkerUser)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	operator, err := trustedOperatorUid(config.TrustedOperatorUser)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	scope := RuntimeResourceScope{BaseDirectory: config.BaseDirectory, StateDirectory: config.StateDirectory, WorkDirectory: config.WorkDirectory,
		ScratchDirectory: config.DockerRole.ScratchDirectory, WorkerUid: uint32(uid), WorkerGid: uint32(gid), TrustedOperatorUid: operator}
	boundary, err := VerifyRuntimeResourceBoundary(ctx, config.DockerRole.ResourceBoundary, scope, DefaultRuntimeResourceBoundarySettings())
	if err != nil {
		t.Fatal("initial native runtime", err)
	}
	defer boundary.Close()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(nonce[:])
	runId := hex.EncodeToString(sum[:])
	name := "synthetic-net-" + hex.EncodeToString(nonce[:])
	bridge := "unev-" + runId[:10]
	plan := DockerProxyPlan{RunId: runId, JobId: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RoundId: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		BaseDirectory: config.BaseDirectory, ContextRoot: filepath.Join(config.WorkDirectory, runId), ScratchDirectory: filepath.Join(config.DockerRole.ScratchDirectory, runId),
		UpstreamSocketPath: config.DockerSocketPath, HostFiles: &HostFilesPlan{WorkDeviceId: boundary.WorkDeviceId}}
	command := func(args ...string) (string, error) {
		output := &boundedOutput{maximum: 4096}
		cmd := exec.CommandContext(ctx, config.Docker.Path, append([]string{"--host", "unix://" + config.DockerSocketPath}, args...)...)
		cmd.Stdout, cmd.Stderr = output, output
		err := cmd.Run()
		if output.exceeded {
			return "", fmt.Errorf("native fixture command exceeded bound")
		}
		return strings.TrimSpace(string(output.Bytes())), err
	}
	networkId, err := command("network", "create", "--internal", "--driver", "bridge", "--label", cleanupJobLabel+"="+plan.JobId, "--label", cleanupRoundLabel+"="+plan.RoundId,
		"--label", cleanupRunLabel+"="+plan.RunId, "--opt", "com.docker.network.bridge.gateway_mode_ipv4=isolated", "--opt", "com.docker.network.bridge.gateway_mode_ipv6=isolated",
		"--opt", "com.docker.network.bridge.name="+bridge, name)
	if err != nil || !digestPattern.MatchString(networkId) {
		t.Fatalf("native network create: %v: %s", err, networkId)
	}
	var containerIds []string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		for _, containerId := range containerIds {
			if output, err := exec.CommandContext(cleanupCtx, config.Docker.Path, "--host", "unix://"+config.DockerSocketPath, "rm", "--force", containerId).CombinedOutput(); err != nil {
				t.Errorf("remove exact native peer: %v: %s", err, output)
			}
		}
		if output, err := exec.CommandContext(cleanupCtx, config.Docker.Path, "--host", "unix://"+config.DockerSocketPath, "network", "rm", networkId).CombinedOutput(); err != nil {
			t.Errorf("remove exact native network: %v: %s", err, output)
		}
	})
	for i := range 3 {
		containerId, err := command("run", "--detach", "--pull", "never", "--name", fmt.Sprintf("%s-%d", name, i), "--network", networkId, "--read-only", "--cap-drop", "ALL",
			"--health-cmd", "/usr/bin/true", "--health-interval", "1s", "--health-timeout", "1s",
			"--security-opt", "no-new-privileges=true", "--security-opt", "seccomp="+config.DockerRole.CandidateCoreProfile.Path, "--ulimit", "core=1:1", "--user", "65532:65532",
			"--cpuset-cpus", boundary.ManagementCpuSet, "--cgroup-parent", managementPrivateSliceName, "--memory", "128m", "--memory-swap", "128m", "--pids-limit", "32", "--shm-size", "8m",
			"--entrypoint", "/usr/bin/sleep", config.EvaluatorImageDigest, "120")
		if err != nil || !digestPattern.MatchString(containerId) {
			t.Fatalf("native isolated peer create: %v: %s", err, containerId)
		}
		containerIds = append(containerIds, containerId)
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", config.DockerSocketPath)
	}}
	defer transport.CloseIdleConnections()
	proxy := &DockerProxy{plan: plan, settings: DefaultDockerProxySettings(), transport: transport}
	if err := proxy.bindRuntimeBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	proof, err := proxy.proveHostNetwork(ctx, networkId)
	if err != nil {
		for _, family := range []string{"-4", "-6"} {
			content, routeErr := boundary.namespace.Run(ctx, boundary.Ip, []string{"-j", family, "route", "show", "table", "all"})
			t.Logf("owned fixture route family %s: %s (%v)", family, content, routeErr)
		}
		t.Fatal("composed native owned bridge proof", err)
	}
	if err := validateHostNetworkProof(proof, runId, networkId); err != nil {
		t.Fatal(err)
	}
	t.Log("actual owned bridge, trusted isolated peer and retained native proof passed")
}
