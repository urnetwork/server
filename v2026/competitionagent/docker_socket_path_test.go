//go:build linux

// Full run digests must fit Unix sockets under ordinary base paths; lossless
// compact encoding preserves all identity bits instead of truncating a fence.
package competitionagent

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// All client fixture listeners use the same production derivation as the
// owner, including typed builds and host-file calls on its worker endpoint.
func dockerClientTestSocket(t *testing.T, fixture *dockerClientTestFixture) string {
	t.Helper()
	worker, _, err := dockerRunSocketPaths(fixture.config.SocketDirectory, fixture.runId)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestDockerRunSocketsFitNormalBaseAndRetainEveryIdentityBit(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "ds-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	padding := 33 - len(root) - 1
	if padding <= 0 {
		t.Fatal("synthetic native boundary fixture is too long")
	}
	base := filepath.Join(root, strings.Repeat("x", padding))
	runId := strings.Repeat("a1", 32)
	if len(filepath.Join(base, "clients", runId, "docker.sock")) < len(syscall.RawSockaddrUnix{}.Path) {
		t.Fatal("counterfactual no longer exceeds native socket limit")
	}
	oldSocket := filepath.Join(base, "clients", runId, "docker.sock")
	if err := os.MkdirAll(filepath.Dir(oldSocket), 0700); err != nil {
		t.Fatal(err)
	}
	oldListener, oldErr := net.Listen("unix", oldSocket)
	if oldErr == nil {
		_ = oldListener.Close()
		t.Fatal("counterfactual unexpectedly bound an oversized native Unix address")
	}
	if !errors.Is(oldErr, syscall.EINVAL) {
		t.Fatal("counterfactual missed native address-length rejection", oldErr)
	}
	worker, build, err := dockerRunSocketPaths(filepath.Join(base, "clients"), runId)
	if err != nil || worker == build {
		t.Fatal("valid base cannot address both fixed roles", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(filepath.Base(filepath.Dir(worker)))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := hex.DecodeString(runId)
	if err != nil || !bytes.Equal(expected, decoded) {
		t.Fatal("socket identity lost or truncated digest bits", err)
	}
	if err := os.MkdirAll(filepath.Dir(worker), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{worker, build} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal("derived native socket cannot bind", err)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDockerRunSocketsRejectInvalidIdentitiesAndOversizedRoots(t *testing.T) {
	for _, value := range []struct{ directory, runId string }{
		{directory: "/" + strings.Repeat("x", 120), runId: strings.Repeat("a", 64)},
		{directory: "relative", runId: strings.Repeat("a", 64)},
		{directory: "/tmp/../escape", runId: strings.Repeat("a", 64)},
		{directory: "/tmp/synthetic", runId: strings.Repeat("A", 64)},
		{directory: "/tmp/synthetic", runId: strings.Repeat("a", 63)},
	} {
		if _, _, err := dockerRunSocketPaths(value.directory, value.runId); err == nil {
			t.Fatal("unusable or aliased socket identity admitted", value)
		}
	}
}

// Refuse unusable addresses at protected role installation, before allocating
// a start manifest or waiting on a launch that can never create its listener.
func TestDockerRoleRejectsOversizedRunSocketsBeforeLaunch(t *testing.T) {
	fixture := newAgentTestFixture(t)
	config := *fixture.config
	config.BaseDirectory = "/" + strings.Repeat("x", 120)
	if err := config.DockerRole.validate(&config, false); err == nil || !strings.Contains(err.Error(), "Unix address") {
		t.Fatal("role missed native socket bound before other setup", err)
	}
}
