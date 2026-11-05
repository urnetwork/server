// CLI mode validation happens before protected configuration is accessed.
package main

import (
	"bytes"
	"strings"
	"testing"
)

// The SSH role cannot append a shell command or select both lifecycle owners.
func TestSubevaluationArgumentsRejectExpandedRoles(t *testing.T) {
	cases := [][]string{
		{}, {"--serve"}, {"--config", "/synthetic/agent.json"},
		{"--config", "/synthetic/agent.json", "--serve", "--execute=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--serve", "--cleanup=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--execute=" + strings.Repeat("a", 64), "--cleanup=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--serve", "arbitrary-command"},
		{"--config", "/synthetic/agent.json", "--command=/bin/sh", "--serve"},
		{"--docker-client=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--docker-client=" + strings.Repeat("a", 64), "--", "info"},
		{"--serve", "--docker-client=" + strings.Repeat("a", 64), "--", "info"},
		{"--config", "/synthetic/agent.json", "--serve", "--docker-owner=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--docker-owner=" + strings.Repeat("a", 64), "--freeze=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--execute=" + strings.Repeat("a", 64), "--freeze=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--host-operation=" + strings.Repeat("a", 64), "--", "mount-evidence"},
		{"--host-operation=" + strings.Repeat("a", 64)},
		{"--host-operation=" + strings.Repeat("a", 64), "--docker-client=" + strings.Repeat("a", 64), "--", "info"},
		{"--docker-probe", "--", "info", "--format", "{{.CgroupVersion}}"},
		{"--config", "/synthetic/agent.json", "--docker-probe"},
		{"--config", "/synthetic/agent.json", "--docker-probe", "--serve", "--", "info", "--format", "{{.CgroupVersion}}"},
	}
	for _, args := range cases {
		if _, err := parseArguments(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted expanded or incomplete role: %q", args)
		}
	}
}

func TestSubevaluationArgumentsAcceptRootOnlyDockerProbeRole(t *testing.T) {
	parsed, err := parseArguments([]string{"--config", "/synthetic/agent.json", "--docker-probe", "--", "info", "--format", "{{.CgroupVersion}}"}, &bytes.Buffer{})
	if err != nil || !parsed.dockerProbe || parsed.configPath != "/synthetic/agent.json" || parsed.serve || parsed.dockerClientRunId != "" ||
		len(parsed.dockerArguments) != 3 || parsed.dockerArguments[0] != "info" {
		t.Fatal("closed root host-audit role was not retained", err)
	}
}

// The non-root client cannot name a private role config or share a root role.
func TestSubevaluationArgumentsAcceptPublicDockerClientRole(t *testing.T) {
	runId := strings.Repeat("a", 64)
	client, err := parseArguments([]string{"--docker-client=" + runId, "--", "compose", "ps"}, &bytes.Buffer{})
	if err != nil || client.dockerClientRunId != runId || client.configPath != "" || client.serve ||
		len(client.dockerArguments) != 2 || client.dockerArguments[0] != "compose" || client.dockerArguments[1] != "ps" {
		t.Fatal("fixed non-root client role was not retained without private config", err)
	}
}

func TestSubevaluationArgumentsAcceptFinitePublicHostOperationRole(t *testing.T) {
	runId := strings.Repeat("a", 64)
	parsed, err := parseArguments([]string{"--host-operation=" + runId, "--", "mount-evidence"}, &bytes.Buffer{})
	if err != nil || parsed.hostOperationRunId != runId || parsed.configPath != "" || parsed.dockerClientRunId != "" || parsed.serve ||
		len(parsed.dockerArguments) != 1 || parsed.dockerArguments[0] != "mount-evidence" {
		t.Fatal("host client selected private configuration or expanded its role", err)
	}
}

// All fixed entrypoints remain exact and do not load service configuration.
func TestSubevaluationArgumentsAcceptFixedRoles(t *testing.T) {
	serve, err := parseArguments([]string{"--config", "/synthetic/agent.json", "--serve"}, &bytes.Buffer{})
	if err != nil || !serve.serve || serve.executeRunId != "" || serve.configPath != "/synthetic/agent.json" {
		t.Fatalf("serve role was not retained exactly: %+v %v", serve, err)
	}
	runId := strings.Repeat("a", 64)
	execute, err := parseArguments([]string{"--config", "/synthetic/agent.json", "--execute=" + runId}, &bytes.Buffer{})
	if err != nil || execute.serve || execute.executeRunId != runId {
		t.Fatalf("systemd role was not retained exactly: %+v %v", execute, err)
	}
	cleanup, err := parseArguments([]string{"--config", "/synthetic/agent.json", "--cleanup=" + runId}, &bytes.Buffer{})
	if err != nil || cleanup.serve || cleanup.executeRunId != "" || cleanup.cleanupRunId != runId {
		t.Fatalf("systemd cleanup role was not retained exactly: %+v %v", cleanup, err)
	}
	owner, err := parseArguments([]string{"--config", "/synthetic/agent.json", "--docker-owner=" + runId}, &bytes.Buffer{})
	if err != nil || owner.dockerOwnerRunId != runId || owner.freezeRunId != "" || owner.executeRunId != "" {
		t.Fatal("independent Docker owner role was not retained", err)
	}
	freeze, err := parseArguments([]string{"--config", "/synthetic/agent.json", "--freeze=" + runId}, &bytes.Buffer{})
	if err != nil || freeze.freezeRunId != runId || freeze.dockerOwnerRunId != "" || freeze.cleanupRunId != "" {
		t.Fatal("independent emergency freeze role was not retained", err)
	}
}

// Recovery selects the fixed physical slot; no run id, trailing command or
// second lifecycle role is accepted before any root configuration is loaded.
func TestSubevaluationArgumentsAcceptOnlyClosedRecoveryRole(t *testing.T) {
	parsed, err := parseArguments([]string{"--config", "/synthetic/agent.json", "--recover-active"}, &bytes.Buffer{})
	if err != nil || !parsed.recoverActive || parsed.serve || parsed.cleanupRunId != "" || parsed.executeRunId != "" || parsed.configPath != "/synthetic/agent.json" || len(parsed.dockerArguments) != 0 {
		t.Fatal("fixed independent recovery role was not retained", err)
	}
	for _, args := range [][]string{
		{"--recover-active"},
		{"--config", "/synthetic/agent.json", "--recover-active", "arbitrary-command"},
		{"--config", "/synthetic/agent.json", "--recover-active=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--recover-active", "--serve"},
		{"--config", "/synthetic/agent.json", "--recover-active", "--cleanup=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--recover-active", "--execute=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--recover-active", "--docker-owner=" + strings.Repeat("a", 64)},
		{"--config", "/synthetic/agent.json", "--recover-active", "--docker-probe", "--", "info"},
	} {
		if _, err := parseArguments(args, &bytes.Buffer{}); err == nil {
			t.Fatal("recovery accepted caller-selected execution or authority", args)
		}
	}
}
