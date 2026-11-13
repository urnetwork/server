// Configuration-only host audit must use the same closed private profile as
// native admission, not the deliberately different legacy global daemon.
package competitionagent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real native document construction and validation stay composed; no Docker
// daemon, root process, namespace, candidate or production identity is used.
func TestDockerProbeEngineProfileMatchesNativeDocuments(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	profile, err := readDockerEngineProfile(fixture.pin, fixture.scope, fixture.host, fixture.settings)
	if err != nil || profile == nil || profile.EngineSha256 != fixture.boundary.EngineConfig.Sha256 || profile.BoundarySha256 != fixture.pin.Sha256 || profile.Kind != "sim-latency-private-engine-profile/v1" {
		t.Fatal("native private documents were mistaken for legacy daemon configuration", profile, err)
	}
	for _, event := range fixture.host.events {
		if !strings.HasPrefix(event, "document:") {
			t.Fatal("configuration observation performed a native operation", event)
		}
	}
}

// Re-authenticating changed document bytes must not authorize an unsafe class.
func TestDockerProbeEngineProfileRejectsAuthenticatedPolicyDrift(t *testing.T) {
	for _, field := range []string{"iptables", "userland-proxy", "log-driver", "unexpected"} {
		fixture := newRuntimeBoundaryTest(t)
		var engine map[string]any
		if err := json.Unmarshal(fixture.host.documents[fixture.boundary.EngineConfig.Path], &engine); err != nil {
			t.Fatal(err)
		}
		if field == "log-driver" {
			engine[field] = "local"
		} else {
			engine[field] = true
		}
		content, err := json.Marshal(engine)
		if err != nil {
			t.Fatal(err)
		}
		fixture.boundary.EngineConfig = fixture.document("changed-engine.json", content)
		fixture.commit()
		if profile, err := readDockerEngineProfile(fixture.pin, fixture.scope, fixture.host, fixture.settings); err == nil || profile != nil {
			t.Fatal("authenticated drift passed the private native profile", field)
		}
	}
}

// Execute the actual shell branch with authentic profile JSON. Only ownership
// checks and the protected profile subprocess are replaced; SHA matching and
// shell dispatch remain real, with no root or fleet access required.
func TestPrivateHostAuditAcceptsPrivateEngineInsteadOfLegacyFlags(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	profile, err := readDockerEngineProfile(fixture.pin, fixture.scope, fixture.host, fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	enginePath := filepath.Join(root, "engine.json")
	boundaryPath := filepath.Join(root, "boundary.json")
	if err = os.WriteFile(enginePath, fixture.host.documents[fixture.boundary.EngineConfig.Path], 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(boundaryPath, fixture.host.documents[fixture.pin.Path], 0600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "host-self-check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	begin := "docker_daemon_config_secure=false\n"
	end := "docker_id_map_json=\"\"\n"
	start := strings.Index(source, begin)
	finish := strings.Index(source, end)
	if start < 0 || finish <= start {
		t.Fatal("host daemon-profile source boundary missing")
	}
	program := "secure_host_path(){ [[ -f \"$1\" && ! -L \"$1\" ]]; }\nstat(){ if [[ $1 == -c && $2 == %u ]]; then printf 0; elif [[ $1 == -c && $2 == %a ]]; then printf 600; else command stat \"$@\"; fi; }\nhost_docker(){ [[ $# == 1 && $1 == engine-profile ]] || return 2; printf '%s' \"$PROFILE\"; }\n" + source[start:finish] + "\n[[ $docker_daemon_config_secure == true ]]\n"
	// The preserved legacy branch is the actual pre-fix predicate. Root
	// ownership is supplied by the same seam, so its refusal is specifically
	// the incompatible engine policy rather than this test user's uid.
	legacyStart := strings.Index(source[start:finish], "if [ -f \"$DOCKER_DAEMON_CONFIG\" ]")
	if legacyStart < 0 {
		t.Fatal("legacy compatibility predicate absent")
	}
	legacyBlock, _, found := strings.Cut(source[start+legacyStart:finish], "\nfi\n")
	if !found {
		t.Fatal("legacy predicate boundary absent")
	}
	prefix, _, _ := strings.Cut(program, begin)
	oldProgram := prefix + begin + "docker_daemon_config_sha256=\"\"\n" + legacyBlock + "\nfi\n[[ $docker_daemon_config_secure == true ]]\n"
	oldCommand := exec.Command("bash", "-eu", "-c", oldProgram)
	oldCommand.Env = append(controlEnvironment(), "private_runtime=true", "DOCKER_DAEMON_CONFIG="+enginePath)
	if output, err := oldCommand.CombinedOutput(); err == nil {
		t.Fatal("pre-fix predicate did not reproduce private engine refusal", string(output))
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-eu", "-c", program)
	command.Env = append(controlEnvironment(), "private_runtime=true", "DOCKER_DAEMON_CONFIG="+enginePath, "private_boundary="+boundaryPath, "PROFILE="+string(encoded))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("private profile refused by legacy daemon predicate", err, string(output))
	}
	profile.EngineSha256 = strings.Repeat("f", 64)
	encoded, err = json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	command = exec.Command("bash", "-eu", "-c", program)
	command.Env = append(controlEnvironment(), "private_runtime=true", "DOCKER_DAEMON_CONFIG="+enginePath, "private_boundary="+boundaryPath, "PROFILE="+string(encoded))
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatal("another engine's authenticated profile was accepted", string(output))
	}
	command = exec.Command("bash", "-eu", "-c", program)
	command.Env = append(controlEnvironment(), "private_runtime=false", "DOCKER_DAEMON_CONFIG="+enginePath, "PROFILE=invalid-unused-private-data")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatal("private engine bypassed unchanged global-daemon policy", string(output))
	}
	legacy := []byte(`{"userns-remap":"synthetic_remap","no-new-privileges":true,"userland-proxy":false,"log-driver":"local","log-opts":{"max-size":"16m","max-file":"2","compress":"true"},"shutdown-timeout":45,"ipv6":false}`)
	if err := os.WriteFile(enginePath, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("bash", "-eu", "-c", program)
	command.Env = append(controlEnvironment(), "private_runtime=false", "DOCKER_DAEMON_CONFIG="+enginePath, "PROFILE=invalid-unused-private-data")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("historical global-daemon policy stopped working", err, string(output))
	}
}
