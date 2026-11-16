//go:build linux

// The installed Docker CLI parses the real namespace helper against fake HTTP
// daemons. No Docker socket, container, namespace or host mutation is available.
package competitionagent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// The independent inert-argv and handler tests remain unconditional. This
// additional composed control checks the local CLI's actual serialized defaults.
func namespaceProbeCliFixture(t *testing.T, omitSharedMemory bool, apiVersion string) (*dockerProxyTest, int64, string) {
	t.Helper()
	cli, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("supplementary CLI serialization check requires Docker CLI, never a daemon")
	}
	fixture := newDockerProxyTest(t)
	creates := &atomic.Int64{}
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch path := dockerProxyApiPath(r.URL.Path); {
		case path == "/_ping":
			w.Header().Set("API-Version", apiVersion)
			w.Header().Set("OSType", "linux")
			w.WriteHeader(http.StatusOK)
		case path == "/images/"+fixture.plan.BaseImageDigest+"/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": fixture.plan.BaseImageDigest})
		case path == "/containers/create":
			creates.Add(1)
			// Reaching the fake daemon proves policy admission. Refuse here so
			// the real helper stops before start, PID inspection or map reads.
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "synthetic admitted create deliberately stopped"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "no synthetic object"})
		}
	})
	server := httptest.NewServer(http.HandlerFunc(fixture.proxy.workerHandler))
	t.Cleanup(server.Close)
	root := t.TempDir()
	for _, name := range []string{"docker-id-map.sh", "subevaluation-ownership.sh"} {
		content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "docker-id-map.sh" && omitSharedMemory {
			const corrected = "probe_shm_args=(--shm-size 8m)"
			if strings.Count(string(content), corrected) != 1 {
				t.Fatal("counterfactual could not isolate the exact shared-memory correction")
			}
			content = []byte(strings.Replace(string(content), corrected, "probe_shm_args=()", 1))
		}
		if err := os.WriteFile(filepath.Join(root, name), content, 0700); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := os.ReadFile(fixture.plan.CandidateCoreProfile.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "candidate-core.seccomp.json"), profile, 0400); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(root, "fixed-client")
	if err := os.WriteFile(client, []byte(`#!/bin/bash
set -Eeuo pipefail
[[ $1 == --docker-client && $2 == "$PROBE_RUN_ID" && $3 == -- ]]
shift 3
exec "$PROBE_DOCKER_CLI" --host "$PROBE_DOCKER_HOST" "$@"
`), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sudo"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", filepath.Join(root, "docker-id-map.sh"), "--image", fixture.plan.BaseImageDigest, "--uid", "65532", "--gid", "65532")
	command.Env = []string{"PATH=" + root + ":/usr/bin:/bin", "HOME=" + root, "DOCKER_CONFIG=" + root, "LANG=C", "LC_ALL=C", "DOCKER_API_VERSION=" + apiVersion,
		"PROBE_DOCKER_CLI=" + cli, "PROBE_DOCKER_HOST=" + strings.Replace(server.URL, "http://", "tcp://", 1), "PROBE_RUN_ID=" + fixture.plan.RunId,
		"URNETWORK_SUBEVALUATION_CLIENT=" + client, "URNETWORK_SUBEVALUATION_RUN_ID=" + fixture.plan.RunId, "URNETWORK_SUBEVALUATION_JOB_ID=" + fixture.plan.JobId,
		"URNETWORK_SUBEVALUATION_ROUND_ID=" + fixture.plan.RoundId, "URNETWORK_SUBEVALUATION_ATTEMPT=" + strconv.Itoa(fixture.plan.Attempt),
		"URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET=" + fixture.plan.ManagementCpuSet}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "could not start the namespace-map probe") {
		t.Fatal("synthetic create refusal did not stop the actual helper", err, string(output))
	}
	return fixture, creates.Load(), string(output)
}

// Exact helper, CLI defaults and real worker policy compose without weakening
// the policy. The fake daemon's deliberate failure prevents actual execution.
func TestNamespaceProbeCliAdmitsBoundedSharedMemory(t *testing.T) {
	for _, version := range []string{"1.44", "1.56"} {
		fixture, creates, output := namespaceProbeCliFixture(t, false, version)
		if creates != 1 {
			t.Fatal("corrected helper did not reach fake daemon", version, creates, output, namespaceProbeCliRefusal(t, fixture))
		}
	}
}

// Dropping only the fix must fail at the same policy boundary as the native
// incident, not at authentication, CLI parsing or any unrelated resource guard.
func TestNamespaceProbeCliRefusesOversizedImplicitSharedMemory(t *testing.T) {
	for _, version := range []string{"1.44", "1.56"} {
		fixture, creates, output := namespaceProbeCliFixture(t, true, version)
		if creates != 0 || !strings.Contains(output, "Docker scope rejected") || !strings.Contains(namespaceProbeCliRefusal(t, fixture), "tmpfs count or total exceeds its memory budget") {
			t.Fatal("counterfactual missed the shared-memory admission failure", version, creates, output, namespaceProbeCliRefusal(t, fixture))
		}
	}
}

// A refused create remains sealed locally; inspect that exact synthetic body
// to distinguish this resource bug from another CLI or scope incompatibility.
func namespaceProbeCliRefusal(t *testing.T, fixture *dockerProxyTest) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(fixture.proxy.bodyDirectory(), "*.request"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(content, &object) != nil || len(object["HostConfig"]) == 0 {
			continue
		}
		return fmt.Sprint(fixture.proxy.verifyContainerCreate(t.Context(), object))
	}
	return "no container create was received"
}
