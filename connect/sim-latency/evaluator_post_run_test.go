package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	syntheticJobId        = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	syntheticRoundId      = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	syntheticCgroupParent = "urnetwork-evaluation-synthetic.slice"
	syntheticImage        = "sha256:synthetic-evaluator-image"
	syntheticConfigLocal  = "/synthetic/config/local"
	syntheticVaultLocal   = "/synthetic/vault/local"
	syntheticSource       = "/synthetic/source"

	syntheticRunnerMemory   int64 = 77309411328
	syntheticRunnerPids     int64 = 65536
	syntheticPostgresMemory int64 = 17179869184
	syntheticPostgresPids   int64 = 4096
	syntheticRedisMemory    int64 = 8589934592
	syntheticRedisPids      int64 = 4096
)

// evaluatorPostRunInspectPolicy extracts the jq expression directly from the
// evaluator, so this synthetic cannot accidentally replay an approximation.
func evaluatorPostRunInspectPolicy(t *testing.T) string {
	t.Helper()
	scriptPath := filepath.Join("evaluator", "container", "evaluator.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read evaluator helper: %v", err)
	}
	const startMarker = "'length == 3 and\n"
	const endMarker = "] | all)' \\"
	startCount := strings.Count(string(script), startMarker)
	if startCount != 1 {
		t.Fatalf("expected one post-run jq predicate start, found %d", startCount)
	}
	start := strings.Index(string(script), startMarker)
	end := strings.Index(string(script)[start+1:], endMarker)
	if end < 0 {
		t.Fatal("post-run jq predicate end not found")
	}
	end += start + 1 + len("] | all)")
	return string(script)[start+1 : end]
}

func syntheticInspect(statePostgres, stateRedis map[string]any) []map[string]any {
	runner := map[string]any{
		"Id":   "synthetic-runner",
		"Name": "/synthetic-runner-1",
		"Config": map[string]any{
			"Image": syntheticImage,
			"User":  "65532:65532",
		},
		"HostConfig": map[string]any{
			"ReadonlyRootfs": true,
			"Memory":         syntheticRunnerMemory,
			"MemorySwap":     syntheticRunnerMemory,
			"PidsLimit":      syntheticRunnerPids,
			"CgroupParent":   syntheticCgroupParent,
			"CpusetCpus":     "20,22",
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
		},
		"Mounts": []map[string]any{
			{"Type": "bind", "Source": syntheticConfigLocal, "Destination": "/runtime/config/local", "RW": false},
			{"Type": "bind", "Source": syntheticVaultLocal, "Destination": "/runtime/vault/local", "RW": false},
			{"Type": "bind", "Source": syntheticSource, "Destination": "/workspace", "RW": false},
		},
		"State": map[string]any{"Status": "exited", "ExitCode": 0, "OOMKilled": false},
	}
	postgres := map[string]any{
		"Id":   "synthetic-postgres",
		"Name": "/synthetic-postgres-1",
		"Config": map[string]any{
			"User": "999:999",
		},
		"HostConfig": map[string]any{
			"ReadonlyRootfs": true,
			"Memory":         syntheticPostgresMemory,
			"MemorySwap":     syntheticPostgresMemory,
			"PidsLimit":      syntheticPostgresPids,
			"CgroupParent":   syntheticCgroupParent,
			"CpusetCpus":     "20,22",
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
		},
		"State": statePostgres,
	}
	redis := map[string]any{
		"Id":   "synthetic-redis",
		"Name": "/synthetic-redis-1",
		"Config": map[string]any{
			"User": "999:999",
		},
		"HostConfig": map[string]any{
			"ReadonlyRootfs": true,
			"Memory":         syntheticRedisMemory,
			"MemorySwap":     syntheticRedisMemory,
			"PidsLimit":      syntheticRedisPids,
			"CgroupParent":   syntheticCgroupParent,
			"CpusetCpus":     "20,22",
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
		},
		"State": stateRedis,
	}
	for _, container := range []map[string]any{runner, postgres, redis} {
		container["RestartCount"] = 0
		container["HostConfig"].(map[string]any)["RestartPolicy"] = map[string]any{"Name": "no"}
		container["Config"].(map[string]any)["Labels"] = map[string]any{
			"com.docker.compose.project":         "synthetic",
			"com.urnetwork.competition.job-id":   syntheticJobId,
			"com.urnetwork.competition.round-id": syntheticRoundId,
			"com.urnetwork.competition.stage":    "candidate",
		}
	}
	runnerState := healthyServiceState()
	runnerState["Status"] = "exited"
	runnerState["Running"] = false
	runner["State"] = runnerState
	return []map[string]any{runner, postgres, redis}
}

func runEvaluatorPostRunInspectPolicy(t *testing.T, inspect []map[string]any) error {
	t.Helper()
	input, err := json.Marshal(inspect)
	if err != nil {
		t.Fatalf("marshal synthetic inspect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "jq", "-e",
		"--arg", "parent", syntheticCgroupParent,
		"--arg", "image", syntheticImage,
		"--arg", "cpuset", "20,22",
		"--arg", "runner_id", "synthetic-runner",
		"--arg", "postgres_id", "synthetic-postgres",
		"--arg", "redis_id", "synthetic-redis",
		"--arg", "project", "synthetic",
		"--arg", "role", "candidate",
		"--arg", "job_id", syntheticJobId,
		"--arg", "round_id", syntheticRoundId,
		"--argjson", "exit_code", "0",
		"--arg", "config_local", syntheticConfigLocal,
		"--arg", "vault_local", syntheticVaultLocal,
		"--arg", "source", syntheticSource,
		"--argjson", "runner_memory", strconv.FormatInt(syntheticRunnerMemory, 10),
		"--argjson", "runner_pids", strconv.FormatInt(syntheticRunnerPids, 10),
		"--argjson", "postgres_memory", strconv.FormatInt(syntheticPostgresMemory, 10),
		"--argjson", "postgres_pids", strconv.FormatInt(syntheticPostgresPids, 10),
		"--argjson", "redis_memory", strconv.FormatInt(syntheticRedisMemory, 10),
		"--argjson", "redis_pids", strconv.FormatInt(syntheticRedisPids, 10),
		evaluatorPostRunInspectPolicy(t),
	)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	return fmt.Errorf("jq post-run predicate: %w: %s", err, strings.TrimSpace(string(output)))
}

func healthyServiceState() map[string]any {
	return map[string]any{
		"Status": "running", "Running": true, "ExitCode": 0, "OOMKilled": false,
		"Dead": false, "Paused": false, "Restarting": false, "Error": "",
		"Health":    map[string]any{"Status": "healthy", "FailingStreak": 0},
		"StartedAt": "2026-01-01T00:00:00Z", "FinishedAt": "2026-01-01T00:00:01Z",
	}
}

func oomKilledServiceState() map[string]any {
	state := healthyServiceState()
	state["Status"], state["Running"], state["ExitCode"], state["OOMKilled"] = "exited", false, 137, true
	return state
}

func TestEvaluatorPostRunInspectAcceptsHealthyServices(t *testing.T) {
	if err := runEvaluatorPostRunInspectPolicy(t, syntheticInspect(healthyServiceState(), healthyServiceState())); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluatorPostRunInspectRejectsOomPostgres(t *testing.T) {
	if err := runEvaluatorPostRunInspectPolicy(t, syntheticInspect(oomKilledServiceState(), healthyServiceState())); err == nil {
		t.Fatal("post-run jq predicate accepted OOM-killed postgres")
	}
}

func TestEvaluatorPostRunInspectRejectsOomRedis(t *testing.T) {
	if err := runEvaluatorPostRunInspectPolicy(t, syntheticInspect(healthyServiceState(), oomKilledServiceState())); err == nil {
		t.Fatal("post-run jq predicate accepted OOM-killed redis")
	}
}

// Every backing-service liveness condition is required independently; zero
// runner exit alone cannot establish a valid measured run.
func TestEvaluatorPostRunInspectRejectsUnhealthyBackingServices(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unhealthy", mutate: func(service map[string]any) {
			service["State"].(map[string]any)["Health"] = map[string]any{"Status": "unhealthy", "FailingStreak": 1}
		}},
		{name: "health failure", mutate: func(service map[string]any) {
			service["State"].(map[string]any)["Health"] = map[string]any{"Status": "healthy", "FailingStreak": 1}
		}},
		{name: "restarted", mutate: func(service map[string]any) { service["RestartCount"] = 1 }},
		{name: "restarting", mutate: func(service map[string]any) { service["State"].(map[string]any)["Restarting"] = true }},
		{name: "paused", mutate: func(service map[string]any) { service["State"].(map[string]any)["Paused"] = true }},
		{name: "dead", mutate: func(service map[string]any) { service["State"].(map[string]any)["Dead"] = true }},
		{name: "exited", mutate: func(service map[string]any) { service["State"].(map[string]any)["Status"] = "exited" }},
		{name: "not running", mutate: func(service map[string]any) { service["State"].(map[string]any)["Running"] = false }},
		{name: "nonzero exit", mutate: func(service map[string]any) { service["State"].(map[string]any)["ExitCode"] = 1 }},
		{name: "docker error", mutate: func(service map[string]any) { service["State"].(map[string]any)["Error"] = "synthetic runtime error" }},
		{name: "missing health", mutate: func(service map[string]any) { delete(service["State"].(map[string]any), "Health") }},
	}
	for _, index := range []int{1, 2} {
		for _, c := range cases {
			inspection := syntheticInspect(healthyServiceState(), healthyServiceState())
			c.mutate(inspection[index])
			if err := runEvaluatorPostRunInspectPolicy(t, inspection); err == nil {
				t.Errorf("service %d: accepted %s", index, c.name)
			}
		}
	}
}

// A replaced container or altered containment setting cannot authorize a
// terminal submission failure even when its service reports healthy.
func TestEvaluatorPostRunInspectRejectsIdentityAndContainmentChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "identity", mutate: func(container map[string]any) { container["Id"] = "synthetic-replacement" }},
		{name: "parent", mutate: func(container map[string]any) {
			container["HostConfig"].(map[string]any)["CgroupParent"] = "synthetic-unrelated.slice"
		}},
		{name: "cpu set", mutate: func(container map[string]any) { container["HostConfig"].(map[string]any)["CpusetCpus"] = "99" }},
		{name: "memory", mutate: func(container map[string]any) { container["HostConfig"].(map[string]any)["Memory"] = 0 }},
		{name: "writable root", mutate: func(container map[string]any) { container["HostConfig"].(map[string]any)["ReadonlyRootfs"] = false }},
		{name: "job label", mutate: func(container map[string]any) {
			container["Config"].(map[string]any)["Labels"].(map[string]any)["com.urnetwork.competition.job-id"] = "synthetic-other-job"
		}},
	}
	for index := range 3 {
		for _, c := range cases {
			inspection := syntheticInspect(healthyServiceState(), healthyServiceState())
			c.mutate(inspection[index])
			if err := runEvaluatorPostRunInspectPolicy(t, inspection); err == nil {
				t.Errorf("container %d: accepted %s", index, c.name)
			}
		}
	}
}
