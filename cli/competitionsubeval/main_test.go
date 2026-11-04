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
	}
	for _, args := range cases {
		if _, err := parseArguments(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted expanded or incomplete role: %q", args)
		}
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
}
