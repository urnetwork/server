//go:build linux

// Host-audit tests exercise a closed metadata grammar only. They do not load
// root configuration, contact a daemon or execute a privileged subprocess.
package competitionagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHostAuditPinsBoundedNonExecutableConfigurationData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-check.json")
	content := []byte(`{"host_id":"executor.example"}`)
	if err := os.WriteFile(path, content, 0400); err != nil {
		t.Fatal(err)
	}
	pin := Pin{Path: path, Sha256: digestDockerBytes(content)}
	actual, err := readHostCheckConfiguration(pin, 1024, false)
	if err != nil || string(actual) != string(content) {
		t.Fatal("non-executable protected data was treated as a host command", err)
	}
	if _, err := readHostCheckConfiguration(pin, int64(len(content)-1), false); err == nil {
		t.Fatal("host configuration escaped its exact read bound")
	}
	pin.Sha256 = strings.Repeat("a", 64)
	if _, err := readHostCheckConfiguration(pin, 1024, false); err == nil {
		t.Fatal("host configuration bytes escaped their installed content pin")
	}
}

func TestDockerProbePinsPrivateEndpointAndReadOnlyFactGrammar(t *testing.T) {
	config := &Config{DockerSocketPath: "/synthetic/private/docker.sock"}
	client := &DockerClientConfig{DockerConfigurationDirectory: "/synthetic/protected/docker-client-config"}
	for _, query := range [][]string{
		{"version", "--format", "{{.Server.Version}}"},
		{"info", "--format", "{{.CgroupVersion}}"},
		{"info", "--format", "{{json .SecurityOptions}}"},
		{"image", "inspect", "--format", "{{.Id}}", "sha256:" + strings.Repeat("a", 64)},
		{"image", "inspect", "--format", "{{.Id}}", "fixture.example/database@sha256:" + strings.Repeat("b", 64)},
	} {
		arguments, err := dockerProbeArguments(config, client, query)
		expected := append([]string{"--host", "unix://" + config.DockerSocketPath, "--config", client.DockerConfigurationDirectory}, query...)
		if err != nil || !reflect.DeepEqual(arguments, expected) {
			t.Fatal("host probe changed its fixed endpoint or closed read-only grammar", query, err)
		}
	}
}

// First observation cannot require the final fingerprint it is computing.
// Neither copying the probe config into execution nor a malformed supplied
// fingerprint may turn this purpose distinction into an admission waiver.
func TestDockerProbeCanObserveBeforeExecutionQualificationExists(t *testing.T) {
	fixture := newAgentTestFixture(t)
	config := *fixture.config
	config.QualificationSha256 = ""
	if err := config.validate(false); err == nil {
		t.Fatal("unqualified role acquired ordinary agent authority")
	}
	if err := config.validateRecoveryConfiguration(DefaultAgentSettings(), false); err == nil {
		t.Fatal("unqualified role acquired recovery authority")
	}
	if err := config.validateForPurpose(false, true); err != nil {
		t.Fatal("first read-only observation required its own future fingerprint", err)
	}
	config.QualificationSha256 = "invalid"
	if err := config.validateForPurpose(false, true); err == nil {
		t.Fatal("metadata role accepted a malformed supplied qualification")
	}
	config.QualificationSha256 = ""
	config.SourceLockSha256 = "invalid"
	if err := config.validateForPurpose(false, true); err == nil {
		t.Fatal("metadata role relaxed another frozen identity")
	}
}

func TestDockerProbeRejectsMutationContainerAndEndpointOverrides(t *testing.T) {
	config := &Config{DockerSocketPath: "/synthetic/private/docker.sock"}
	client := &DockerClientConfig{DockerConfigurationDirectory: "/synthetic/protected/docker-client-config"}
	for _, query := range [][]string{
		nil, {"info"}, {"run", "--rm", "synthetic-image"}, {"version", "--format", "{{json .}}"},
		{"info", "--format", "{{.DockerRootDir}}"}, {"--host", "unix:///synthetic/other.sock", "info"},
		{"container", "inspect", "--format", "{{.Id}}", strings.Repeat("a", 64)},
		{"image", "inspect", "--format", "{{json .}}", "sha256:" + strings.Repeat("a", 64)},
		{"image", "inspect", "--format", "{{.Id}}", "mutable-tag"},
		{"image", "inspect", "--format", "{{.Id}}", "--help@sha256:" + strings.Repeat("a", 64)},
		{"image", "inspect", "--format", "{{.Id}}", "fixture.example/invalid\nname@sha256:" + strings.Repeat("a", 64)},
		{"image", "inspect", "--format", "{{.Id}}", "sha256:" + strings.Repeat("a", 64), "another-image"},
	} {
		if _, err := dockerProbeArguments(config, client, query); err == nil {
			t.Fatal("host audit expanded beyond private immutable local metadata", query)
		}
	}
}

func TestHostAuditConfigurationBindsExactPrivateQualificationTuple(t *testing.T) {
	config := Config{HostId: "executor.example", EvaluatorImageDigest: "sha256:" + strings.Repeat("a", 64),
		QualificationSha256: strings.Repeat("b", 64), DockerRole: DockerRoleConfig{ResourceBoundary: Pin{Sha256: strings.Repeat("c", 64)}}}
	boundary := &RuntimeResourceBoundary{EngineConfig: Pin{Sha256: strings.Repeat("d", 64)}}
	document := map[string]any{"host_id": config.HostId, "image_digest": config.EvaluatorImageDigest, "qualification_sha256": config.QualificationSha256,
		"docker_daemon_config_sha256": boundary.EngineConfig.Sha256, "private_runtime_boundary_sha256": config.DockerRole.ResourceBoundary.Sha256,
		"synthetic_hardware_marker": "additional legacy qualification field"}
	content, err := json.Marshal(document)
	if err != nil || validateHostCheckIdentity(config, boundary, content) != nil {
		t.Fatal("exact private host qualification tuple was refused", err)
	}
	for _, name := range []string{"host_id", "image_digest", "qualification_sha256", "docker_daemon_config_sha256", "private_runtime_boundary_sha256"} {
		old := document[name]
		document[name] = "synthetic-mismatch"
		content, err := json.Marshal(document)
		if err != nil || validateHostCheckIdentity(config, boundary, content) == nil {
			t.Fatal("host audit accepted a mismatched private identity component", name, err)
		}
		document[name] = old
	}
	if validateHostCheckIdentity(config, boundary, []byte(`{"host_id":"executor.example","host_id":"another.example"}`)) == nil {
		t.Fatal("ambiguous host qualification identity was accepted")
	}
}
