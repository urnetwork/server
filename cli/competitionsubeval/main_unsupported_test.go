//go:build !linux

// Every public CLI role must fail before protocol I/O or native dispatch.
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/urnetwork/server/competitionagent"
)

// Any attempted read is a deterministic failure, including on a canceled call.
type unsupportedRoleReader struct{}

func (self unsupportedRoleReader) Read(_ []byte) (int, error) {
	panic("unsupported executor read protocol input")
}

// These are valid roles under the unchanged parser, not malformed-input exits.
func TestSubevaluationUnsupportedRolesFailBeforeIo(t *testing.T) {
	runId := strings.Repeat("a", 64)
	config := "/synthetic/unsupported/agent.json"
	for _, arguments := range [][]string{
		{"--config", config, "--serve"},
		{"--config", config, "--serve-managed"},
		{"--config", config, "--recover-active"},
		{"--config", config, "--execute=" + runId},
		{"--config", config, "--cleanup=" + runId},
		{"--config", config, "--docker-owner=" + runId},
		{"--config", config, "--freeze=" + runId},
		{"--config", config, "--docker-probe", "--", "info", "--format", "{{.CgroupVersion}}"},
		{"--docker-client=" + runId, "--", "compose", "ps"},
		{"--host-operation=" + runId, "--", "mount-evidence"},
	} {
		if _, err := parseArguments(arguments, io.Discard); err != nil {
			t.Fatalf("unsupported-role control is not a valid role: %v", err)
		}
		var output, diagnostic bytes.Buffer
		err := run(t.Context(), arguments, unsupportedRoleReader{}, &output, &diagnostic)
		if !errors.Is(err, competitionagent.ErrUnsupportedPlatform) || output.Len() != 0 || diagnostic.Len() != 0 {
			t.Fatalf("unsupported role did not fail closed before I/O: err=%v output=%d diagnostic=%d", err, output.Len(), diagnostic.Len())
		}
	}
}

// A canceled caller still cannot turn the unsupported platform into a success.
func TestSubevaluationUnsupportedCancellationKeepsRefusal(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output, diagnostic bytes.Buffer
	err := run(ctx, []string{"--config", "/synthetic/unsupported/agent.json", "--serve"}, unsupportedRoleReader{}, &output, &diagnostic)
	if !errors.Is(err, competitionagent.ErrUnsupportedPlatform) || output.Len() != 0 || diagnostic.Len() != 0 {
		t.Fatalf("unsupported cancellation lost refusal or published output: %v", err)
	}
}
