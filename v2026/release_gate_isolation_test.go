package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Fake only the Docker boundary, retaining real setup/ownership/endpoint and
// cleanup code. Distinct owner names address distinct state files without a
// shared lock, just as daemon-created immutable IDs do in production.
const releaseGateDockerFixture = `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  info) exit 0 ;;
  create)
    arguments=("$@")
    name='' owner='' service='' restart=''
    for ((i=1; i<${#arguments[@]}; i++)); do
      case "${arguments[i]}" in
        --name) name="${arguments[i+1]}" ;;
        --restart=no) restart=no ;;
        --cidfile) echo 'privileged cidfile is forbidden' >&2; exit 91 ;;
        --label)
          case "${arguments[i+1]}" in
            urnetwork.release-gate.owner=*) owner="${arguments[i+1]#*=}" ;;
            urnetwork.release-gate.service=*) service="${arguments[i+1]#*=}" ;;
          esac ;;
      esac
    done
    [[ "$owner" =~ ^[0-9a-f]{32}$ && "$name" == "urnetwork-gate-$owner-$service" && "$restart" == no ]] || exit 1
    id="$(printf '%s' "$name" | "$FIXTURE_SHA256SUM")"; id="${id%% *}"
    if [[ "$service" == postgres ]]; then port="$FAKE_PG_PORT"; else port="$FAKE_REDIS_PORT"; fi
    printf '%s %s %s %s 127.0.0.1:%s\n' "$name" "$owner" "$service" "$restart" "$port" > "$FAKE_STATE/$id.meta"
    printf '%s\n' "$@" > "$FAKE_STATE/$id.args"
    [[ "${FAKE_FAIL_CREATE:-}" != "$service" ]] || exit 29
    printf '%s\n' "$id" ;;
  inspect)
    id="${!#}"
    [[ -f "$FAKE_STATE/$id.meta" ]] || exit 1
    read -r name owner service restart binding < "$FAKE_STATE/$id.meta"
    if [[ "$*" == *NetworkSettings.Ports* ]]; then printf '%s\n' "$binding"
    elif [[ "$*" == *State.Running* ]]; then
      if [[ "${FAKE_STOPPED_SERVICE:-}" == "$service" ]]; then
        printf 'exited false 1\n'
        if [[ "${FAKE_CHANGE_OWNER_ON_STATE:-}" == 1 ]]; then
          printf '%s %s %s %s %s\n' "$name" 00000000000000000000000000000000 "$service" "$restart" "$binding" > "$FAKE_STATE/$id.meta"
        fi
      else printf 'running true 0\n'; fi
    elif [[ "$*" == *'json .State'* ]]; then
      printf '{"Status":"exited","Running":false,"ExitCode":1}\n'
    else printf '%s /%s %s %s %s\n' "$id" "$name" "$owner" "$service" "$restart"; fi ;;
  container)
    [[ "$2" == ls ]] || exit 1
    filter="${!#}"
    for file in "$FAKE_STATE"/*.meta; do
      [[ -f "$file" ]] || continue
      read -r name owner service restart binding < "$file"
      id="${file##*/}"; id="${id%.meta}"
      if [[ "$filter" == "name=^/${name}$" || "$filter" == "id=$id" ]]; then printf '%s\n' "$id"; fi
    done ;;
  start) [[ -f "$FAKE_STATE/$2.meta" ]] || exit 1 ;;
  exec)
    read -r name owner service restart binding < "$FAKE_STATE/$2.meta"
    printf 'exec\n' >> "$FAKE_STATE/$2.execs"
    if [[ "$service" == postgres ]]; then printf '512:256MB:en_US.UTF-8:t\n'; else printf 'PONG\n'; fi ;;
  logs)
    id="${!#}"
    [[ -f "$FAKE_STATE/$id.meta" ]] || exit 1
    printf '%s\n' "$id" >> "$FAKE_STATE/$id.logs-read"
    printf 'fixture startup failure: permission denied\n'
    if [[ "${FAKE_LARGE_LOG:-}" == 1 ]]; then head -c 2097152 /dev/zero | tr '\000' x; fi ;;
  rm)
    id="${!#}"; [[ "$id" =~ ^[0-9a-f]{64}$ ]] || exit 1
    [[ -f "$FAKE_STATE/$id.meta" ]] || exit 1
    printf '%s\n' "$id" >> "$FAKE_STATE/removed"
    /bin/rm -- "$FAKE_STATE/$id.meta" ;;
  *) printf 'unexpected Docker fixture call: %s\n' "$*" >&2; exit 92 ;;
esac
`

type releaseGateServicesFixture struct {
	root        string
	workspace   string
	state       string
	helper      string
	docker      string
	lock        string
	probe       string
	generator   string
	statCommand string
	hashCommand string
}

// macOS exposes its temporary directory through /var even though the physical
// path is rooted at /private/var. The release gate deliberately rejects path
// aliases, so fixtures must start from the canonical path and reserve symlink
// coverage for the tests that create one explicitly.
func releaseGateCanonicalTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

// GNU spellings differ on Darwin; other hosts retain the existing GNU names.
func releaseGateShellToolNames(goos string) []string {
	if goos == "darwin" {
		return []string{"gstat", "gsha256sum"}
	}
	return []string{"stat", "sha256sum"}
}

func newReleaseGateServicesFixture(t *testing.T) *releaseGateServicesFixture {
	t.Helper()
	// Resolve before allocating fixture state. Absolute physical paths remain
	// valid across Go exec and the fake Docker's separate Bash interpreter.
	toolPaths := []string{}
	for _, name := range releaseGateShellToolNames(runtime.GOOS) {
		commandPath, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("required release-gate shell tool %s: %v", name, err)
		}
		commandPath, err = filepath.Abs(commandPath)
		if err != nil {
			t.Fatalf("absolute release-gate shell tool %s: %v", name, err)
		}
		commandPath, err = filepath.EvalSymlinks(commandPath)
		if err != nil {
			t.Fatalf("physical release-gate shell tool %s: %v", name, err)
		}
		toolPaths = append(toolPaths, commandPath)
	}
	self := &releaseGateServicesFixture{
		root:        filepath.Join(releaseGateCanonicalTempDir(t), "gate"),
		workspace:   releaseGateCanonicalTempDir(t),
		state:       releaseGateCanonicalTempDir(t),
		statCommand: toolPaths[0],
		hashCommand: toolPaths[1],
	}
	if err := os.Mkdir(self.root, 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	self.helper = filepath.Join(server, "local", "release-gate-services.sh")
	// Process setup compiled the actual generator once. Each service owner
	// still receives fresh resources from that executable, never a test double.
	self.generator = releaseGateSuiteGenerator
	if err := os.Symlink(server, filepath.Join(self.workspace, "server")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vault", "config", "config/local", "vault/local"} {
		if err := os.Mkdir(filepath.Join(self.workspace, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{
		"config/local/settings.yml":      "all:\n  env_vars:\n    BRINGYOUR_POSTGRES_HOSTNAME: shared.invalid\n",
		"vault/local/pg_maintenance.yml": "authority: shared.invalid:5432\n",
		"config/db_maintenance.yml":      "min_connections: 1000\nmax_connections: 1000\n",
		"vault/local/nonservice.yml":     "public-fixture: true\n",
	} {
		if err := os.WriteFile(filepath.Join(self.workspace, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	self.docker = filepath.Join(releaseGateCanonicalTempDir(t), "docker")
	if err := os.WriteFile(self.docker, []byte(releaseGateDockerFixture), 0o700); err != nil {
		t.Fatal(err)
	}
	self.lock = filepath.Join(releaseGateCanonicalTempDir(t), "release.lock.yml")
	lock := "    postgres: postgres:18@sha256:" + strings.Repeat("a", 64) + "\n    redis: redis:8-alpine@sha256:" + strings.Repeat("b", 64) + "\n"
	if err := os.WriteFile(self.lock, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	self.probe = filepath.Join(releaseGateCanonicalTempDir(t), "probe")
	if err := os.WriteFile(self.probe, []byte("#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> \"$GATE_ROOT/probes\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return self
}

func (self *releaseGateServicesFixture) run(t *testing.T, body string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "-c", `set -euo pipefail
trap 'exit 143' TERM
stat() { "$FIXTURE_STAT" "$@"; }
source "$HELPER"
release_gate_service_docker() { "$FAKE_DOCKER" "$@"; }
release_gate_service_fixture() { shift 2; "$FIXTURE_GENERATOR" "$@"; }
`+body)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 10 * time.Second
	command.Env = testCommandEnvironment(map[string]string{
		"HELPER": self.helper, "FAKE_DOCKER": self.docker, "FAKE_STATE": self.state,
		"FIXTURE_GENERATOR": self.generator,
		"FIXTURE_STAT":      self.statCommand, "FIXTURE_SHA256SUM": self.hashCommand,
		"GATE_ROOT": self.root, "FIXTURE_WORKSPACE": self.workspace, "FIXTURE_LOCK": self.lock,
		"FAKE_PG_PORT": "35431", "FAKE_REDIS_PORT": "36371",
		"APEX_CONTAINER_EVALUATION": "", "FAKE_FAIL_CREATE": "",
		"FAKE_STOPPED_SERVICE": "", "FAKE_CHANGE_OWNER_ON_STATE": "", "FAKE_LARGE_LOG": "",
		"PRIVATE_PROBE": self.probe, "WARP_ENV": "local",
	})
	return command.CombinedOutput()
}

// Tool selection must preserve the host's existing command namespace.
func TestReleaseGateShellToolNamesMatchHost(t *testing.T) {
	for _, test := range []struct {
		goos string
		stat string
		hash string
	}{
		{goos: "darwin", stat: "gstat", hash: "gsha256sum"},
		{goos: "linux", stat: "stat", hash: "sha256sum"},
	} {
		names := releaseGateShellToolNames(test.goos)
		if len(names) != 2 || names[0] != test.stat || names[1] != test.hash {
			t.Errorf("%s shell tools=%v, want %s and %s", test.goos, names, test.stat, test.hash)
		}
	}
}

// The actual helper, generator, fake Docker and sourced environment must use
// bound tools after exec, without changing the parent's ambient command path.
func TestReleaseGateShellToolsIgnoreAmbientNames(t *testing.T) {
	originalPath := os.Getenv("PATH")
	for _, name := range []string{"stat", "sha256sum"} {
		t.Setenv("PATH", originalPath)
		self := newReleaseGateServicesFixture(t)
		directory := releaseGateCanonicalTempDir(t)
		// Keep the other boundary real so each poisoned name is reached even
		// when the control omits both bindings on a BSD-default host.
		otherName, otherPath := "sha256sum", self.hashCommand
		if name == "sha256sum" {
			otherName, otherPath = "stat", self.statCommand
		}
		if err := os.Symlink(otherPath, filepath.Join(directory, otherName)); err != nil {
			t.Fatal(err)
		}
		poison := filepath.Join(directory, name)
		if err := os.WriteFile(poison, []byte("#!/bin/sh\nprintf 'ambient "+name+" invoked\\n' >> \"$GATE_ROOT/tool-poison-hit\"\nexit 89\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		ambientPath := directory + string(os.PathListSeparator) + originalPath
		t.Setenv("PATH", ambientPath)
		output, err := self.run(t, releaseGatePrivateResourcesBody+`
export WARP_TEST_ENV_TCP_PROBE="$PRIVATE_PROBE"
source "$FIXTURE_WORKSPACE/server/test-env.sh"
printf 'release-gate-tools-ok\n'
`)
		if err != nil || !strings.HasSuffix(string(output), "release-gate-tools-ok\n") {
			t.Errorf("%s ambient tool affected bound fixture: %v\n%s", name, err, output)
		}
		if data, err := os.ReadFile(filepath.Join(self.root, "tool-poison-hit")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s ambient tool was invoked: %q %v", name, data, err)
		}
		if os.Getenv("PATH") != ambientPath {
			t.Errorf("%s fixture changed the parent's command path", name)
		}
		if files, err := filepath.Glob(filepath.Join(self.state, "*.meta")); err != nil || len(files) != 0 {
			t.Errorf("%s bound fixture did not clean its pair: %v %v", name, files, err)
		}
	}
}

// Re-execute this test with one missing tool and an unusable temporary parent.
// The exact tool refusal must precede t.TempDir and every service setup call.
func TestReleaseGateMissingShellToolRefusesBeforeSetup(t *testing.T) {
	const childMode = "URNETWORK_RELEASE_GATE_MISSING_TOOL_CHILD"
	if os.Getenv(childMode) == "1" {
		newReleaseGateServicesFixture(t)
		t.Fatal("missing shell tool reached fixture setup")
	}
	self := newReleaseGateServicesFixture(t)
	names := releaseGateShellToolNames(runtime.GOOS)
	toolPaths := []string{self.statCommand, self.hashCommand}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for index, name := range names {
		directory := releaseGateCanonicalTempDir(t)
		binDirectory := filepath.Join(directory, "bin")
		if err := os.Mkdir(binDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		other := 1 - index
		if err := os.Symlink(toolPaths[other], filepath.Join(binDirectory, names[other])); err != nil {
			t.Fatal(err)
		}
		missingTemporary := filepath.Join(directory, "must-not-be-created")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		command := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.count=1")
		command.Env = testCommandEnvironment(map[string]string{
			childMode: "1",
			"PATH":    binDirectory,
			"TMPDIR":  missingTemporary, "GOTMPDIR": missingTemporary,
		})
		output, err := command.CombinedOutput()
		cancel()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 ||
			!strings.Contains(string(output), "required release-gate shell tool "+name+":") {
			t.Errorf("%s did not refuse before fixture allocation: %v\n%s", name, err, output)
		}
		if _, err := os.Lstat(missingTemporary); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s created the forbidden temporary parent: %v", name, err)
		}
	}
}

// Reuse the actual positive resource body in the forced assertion-failure control.
const releaseGatePrivateResourcesBody = `
before_umask="$(umask)"
trap 'release_gate_services_cleanup' EXIT
release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
[[ "$(umask)" == "$before_umask" ]] || exit 1
source "$release_gate_service_root/environment.sh"
[[ "$WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY" == 127.0.0.1:35431 && "$WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY" == 127.0.0.1:36371 ]] || exit 1
[[ "$(stat -c '%a' "$release_gate_service_root/postgres.cid")" == 600 ]] || exit 1
cmp "$WARP_TEST_ENV_PORTABLE_ROOT/vault/pg.yml" "$WARP_TEST_ENV_PORTABLE_ROOT/vault/pg_maintenance.yml"
cmp "$WARP_TEST_ENV_PORTABLE_ROOT/config/db.yml" "$WARP_TEST_ENV_PORTABLE_ROOT/config/db_maintenance.yml"
cat "$WARP_TEST_ENV_PORTABLE_ROOT/config/settings.yml"
[[ -d "$WARP_SITE_HOME" && ! -e "$WARP_TEST_ENV_PORTABLE_ROOT/vault/local" ]] || exit 1
[[ -f "$WARP_TEST_ENV_PORTABLE_ROOT/vault/auth.yml" && ! -L "$WARP_TEST_ENV_PORTABLE_ROOT/vault/auth.yml" ]] || exit 1
[[ ! -e "$WARP_TEST_ENV_PORTABLE_ROOT/vault/nonservice.yml" ]] || exit 1
for service in postgres redis; do
  id="$(< "$release_gate_service_root/$service.cid")"
  [[ "$(< "$FAKE_STATE/$id.args")" == *--restart=no* ]] || exit 1
  [[ "$(< "$FAKE_STATE/$id.args")" == *--tmpfs* ]] || exit 1
  [[ "$(< "$FAKE_STATE/$id.args")" == *127.0.0.1::* ]] || exit 1
done
`

func TestReleaseGateServicesPrivateResourcesAndNoRestart(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, releaseGatePrivateResourcesBody)
	if err != nil {
		t.Fatalf("private resource setup: %v\n%s", err, output)
	}
	// The real private generator owns only these documentation, loopback and
	// connect's default local TUN allocator locations (sn f4f1fd452). Reject
	// extra settings, especially ambient env_vars that could redirect the
	// isolated database or Redis transports.
	var settings struct {
		All struct {
			IPOverrides []struct {
				Subnet      string `yaml:"subnet"`
				CountryCode string `yaml:"country_code"`
				Country     string `yaml:"country"`
				Region      string `yaml:"region"`
				City        string `yaml:"city"`
			} `yaml:"ip_overrides"`
		} `yaml:"all"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(output))
	decoder.KnownFields(true)
	if err := decoder.Decode(&settings); err != nil {
		t.Fatalf("private settings census: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("private settings have trailing content: %v", err)
	}
	expectedSubnets := []string{"192.0.2.0/24", "2001:db8::/32", "127.0.0.0/8", "::1/128", "169.254.0.0/16", "fd75:726e:6574::/96"}
	if len(settings.All.IPOverrides) != len(expectedSubnets) {
		t.Fatalf("private settings override count=%d, want %d", len(settings.All.IPOverrides), len(expectedSubnets))
	}
	for index, subnet := range expectedSubnets {
		override := settings.All.IPOverrides[index]
		if override.Subnet != subnet || override.CountryCode != "zz" || override.Country != "Fixture Country" || override.Region != "Fixture Region" || override.City != "Fixture City" {
			t.Fatalf("private settings override %d differs: %+v", index, override)
		}
	}
	if files, err := filepath.Glob(filepath.Join(self.state, "*.meta")); err != nil || len(files) != 0 {
		t.Fatalf("owned resources survived cleanup: %v %v", files, err)
	}
}

// A false intermediate assertion must stop the actual fixture body before
// later work; a positive entry marker distinguishes setup refusal from proof.
func TestReleaseGateShellAssertionStopsBeforeFollowingCommands(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	const predicate = `[[ "$(umask)" == "$before_umask" ]]`
	if strings.Count(releaseGatePrivateResourcesBody, predicate) != 1 {
		t.Fatal("private resource body lost its unique assertion boundary")
	}
	body := strings.Replace(releaseGatePrivateResourcesBody, predicate, `printf 'predicate reached\n' > "$GATE_ROOT/assertion-reached"
[[ fixture == rejected ]]`, 1)
	output, err := self.run(t, body+`
printf 'unexpected continuation\n' > "$GATE_ROOT/assertion-continued"
`)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("false fixture assertion did not exit 1: %v\n%s", err, output)
	}
	reached, err := os.ReadFile(filepath.Join(self.root, "assertion-reached"))
	if err != nil || string(reached) != "predicate reached\n" {
		t.Fatalf("assertion boundary was not reached: %q %v", reached, err)
	}
	if _, err := os.Lstat(filepath.Join(self.root, "assertion-continued")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("false assertion reached following work: %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(self.state, "*.meta"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("assertion refusal did not clean its owned fake pair: %v %v", entries, err)
	}
}

// Invoke the same executable written by the fixture, with invalid authority at
// each mutation/observation boundary; failure cannot leave a write or removal.
func TestReleaseGateDockerFixtureRejectsMalformedAuthority(t *testing.T) {
	missingId := strings.Repeat("a", 64)
	for _, test := range []struct {
		name      string
		arguments string
		existing  string
	}{
		{name: "create", arguments: "create --name invalid --restart=no --label urnetwork.release-gate.owner=" + strings.Repeat("a", 32) + " --label urnetwork.release-gate.service=postgres"},
		{name: "container-command", arguments: "container inspect --filter id=" + missingId},
		{name: "logs-missing-state", arguments: "logs " + missingId},
		{name: "remove-invalid-id", arguments: "rm invalid-id", existing: "invalid-id.meta"},
		{name: "remove-missing-state", arguments: "rm " + missingId},
		{name: "start-missing-state", arguments: "start " + missingId},
	} {
		self := newReleaseGateServicesFixture(t)
		const originalState = "synthetic pre-existing state\n"
		if test.existing != "" {
			if err := os.WriteFile(filepath.Join(self.state, test.existing), []byte(originalState), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		output, err := self.run(t, `exec "$FAKE_DOCKER" `+test.arguments)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Errorf("%s malformed fake authority did not exit 1: %v\n%s", test.name, err, output)
		}
		entries, err := os.ReadDir(self.state)
		if err != nil {
			t.Fatal(err)
		}
		if test.existing == "" {
			if len(entries) != 0 {
				t.Errorf("%s refusal wrote fake state: %v", test.name, entries)
			}
		} else {
			data, err := os.ReadFile(filepath.Join(self.state, test.existing))
			if err != nil || string(data) != originalState || len(entries) != 1 || entries[0].Name() != test.existing {
				t.Errorf("%s refusal mutated existing fake state: %q %v entries=%v", test.name, data, err, entries)
			}
		}
	}
}

func TestReleaseGateServicesStoppedOwnerRetainsBoundedDiagnostics(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
export FAKE_STOPPED_SERVICE=postgres FAKE_LARGE_LOG=1
if release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"; then exit 90; fi
id="$(< "$release_gate_service_root/postgres.cid")"
[[ ! -e "$release_gate_service_root/redis.cid" && ! -e "$release_gate_service_root/environment.sh" ]] || exit 1
[[ ! -e "$FAKE_STATE/$id.execs" ]] || exit 1
[[ "$(< "$FAKE_STATE/$id.logs-read")" == "$id" ]] || exit 1
[[ "$(< "$release_gate_service_root/postgres.failure.txt")" == *"container_id=$id"* ]] || exit 1
[[ "$(< "$release_gate_service_root/postgres.failure.txt")" == *'stage=stopped'* ]] || exit 1
[[ "$(< "$release_gate_service_root/postgres.failure.txt")" == *'observation=exited false 1'* ]] || exit 1
[[ "$(< "$release_gate_service_root/postgres.failure-state.txt")" == '{"Status":"exited","Running":false,"ExitCode":1}' ]] || exit 1
[[ "$(stat -c '%s' "$release_gate_service_root/postgres.failure-log.txt")" == 1048576 ]] || exit 1
for suffix in failure.txt failure-state.txt failure-log.txt readiness.stderr; do
  [[ "$(stat -c '%a' "$release_gate_service_root/postgres.$suffix")" == 600 ]] || exit 1
done
release_gate_services_cleanup
[[ "$(< "$FAKE_STATE/removed")" == "$id" && ! -e "$FAKE_STATE/$id.meta" ]] || exit 1
`)
	if err != nil {
		t.Fatalf("stopped owned service diagnostics: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "is not running: exited false 1") {
		t.Fatalf("stopped owned service lost its concrete refusal: %s", output)
	}
}

func TestReleaseGateServicesStoppedOwnerDoesNotReadForeignDiagnostics(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
export FAKE_STOPPED_SERVICE=postgres FAKE_CHANGE_OWNER_ON_STATE=1
if release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"; then exit 90; fi
id="$(< "$release_gate_service_root/postgres.cid")"
[[ ! -e "$FAKE_STATE/$id.execs" && ! -e "$FAKE_STATE/$id.logs-read" ]] || exit 1
[[ ! -e "$release_gate_service_root/postgres.failure.txt" && ! -e "$release_gate_service_root/postgres.failure-log.txt" ]] || exit 1
[[ ! -e "$release_gate_service_root/redis.cid" && ! -e "$release_gate_service_root/environment.sh" ]] || exit 1
if release_gate_services_cleanup; then exit 91; fi
[[ -f "$FAKE_STATE/$id.meta" && ! -e "$FAKE_STATE/removed" ]] || exit 1
`)
	if err != nil {
		t.Fatalf("changed owner diagnostic refusal: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "refusing changed postgres container ownership") {
		t.Fatalf("changed owner diagnostic refusal was not retained: %s", output)
	}
}

func TestReleaseGateServicesLostCreateAcknowledgementCleansPartialPair(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
export FAKE_FAIL_CREATE=redis
if release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"; then exit 90; fi
[[ ! -e "$release_gate_service_root/redis.cid" ]] || exit 1
release_gate_services_cleanup
release_gate_services_cleanup
`)
	if err != nil {
		t.Fatalf("lost create acknowledgement: %v\n%s", err, output)
	}
	removed, err := os.ReadFile(filepath.Join(self.state, "removed"))
	if err != nil || len(strings.Fields(string(removed))) != 2 {
		t.Fatalf("partial setup cleanup did not remove exactly its pair: %q %v", removed, err)
	}
}

func TestReleaseGateServicesConcurrentOwnersKeepIndependentCleanup(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
mkdir -m 700 "$GATE_ROOT/a" "$GATE_ROOT/b"
mkfifo "$GATE_ROOT/ready" "$GATE_ROOT/release-a" "$GATE_ROOT/release-b"
# Bash 3.2, which remains the system shell on macOS, predates dynamic
# descriptor allocation. Fixed high descriptors are private to this fixture.
exec 7<>"$GATE_ROOT/ready"
exec 8<>"$GATE_ROOT/release-a"
exec 9<>"$GATE_ROOT/release-b"
a_pid='' b_pid=''
join_fixture_owners() {
  printf 'release\n' >&8
  printf 'release\n' >&9
  if [[ -n "$a_pid" ]]; then wait "$a_pid" || :; fi
  if [[ -n "$b_pid" ]]; then wait "$b_pid" || :; fi
}
trap join_fixture_owners EXIT
owner_a() (
  trap 'release_gate_services_cleanup' EXIT
  export FAKE_PG_PORT=45431 FAKE_REDIS_PORT=46371
  release_gate_services_start "$GATE_ROOT/a" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
  printf 'a\n' >&7
  read -r release < "$GATE_ROOT/release-a"
  exit 23
)
owner_b() (
  trap 'release_gate_services_cleanup' EXIT
  export FAKE_PG_PORT=55431 FAKE_REDIS_PORT=56371
  release_gate_services_start "$GATE_ROOT/b" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
  printf 'b\n' >&7
  read -r release < "$GATE_ROOT/release-b"
  release_gate_service_find postgres
  [[ -n "$RELEASE_GATE_SERVICE_ID" ]] || exit 1
  release_gate_service_find redis
  [[ -n "$RELEASE_GATE_SERVICE_ID" ]] || exit 1
)
owner_a & a_pid=$!
owner_b & b_pid=$!
read -r first <&7
read -r second <&7
[[ "$first:$second" == a:b || "$first:$second" == b:a ]] || exit 1
[[ "$(< "$GATE_ROOT/a/services/owner")" != "$(< "$GATE_ROOT/b/services/owner")" ]] || exit 1
printf 'release\n' >&8
status=0; wait "$a_pid" || status=$?
[[ "$status" == 23 ]] || exit 1
kill -0 "$b_pid"
for service in postgres redis; do
  a_id="$(< "$GATE_ROOT/a/services/$service.cid")"
  b_id="$(< "$GATE_ROOT/b/services/$service.cid")"
  [[ "$a_id" != "$b_id" && ! -e "$FAKE_STATE/$a_id.meta" && -f "$FAKE_STATE/$b_id.meta" ]] || exit 1
done
printf 'release\n' >&9
wait "$b_pid"
`)
	if err != nil {
		t.Fatalf("concurrent service owners: %v\n%s", err, output)
	}
	removed, err := os.ReadFile(filepath.Join(self.state, "removed"))
	if err != nil || len(strings.Fields(string(removed))) != 4 {
		t.Fatalf("concurrent cleanup removed %q: %v", removed, err)
	}
}

func TestReleaseGateServicesChangedRootIdentityRefusesCleanup(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
mv "$release_gate_service_root" "$GATE_ROOT/original-services"
mkdir -m 700 "$release_gate_service_root"
cp "$GATE_ROOT/original-services/owner" "$release_gate_service_root/owner"
if release_gate_services_cleanup; then exit 90; fi
[[ ! -e "$FAKE_STATE/removed" ]] || exit 1
mv "$release_gate_service_root" "$GATE_ROOT/replacement-services"
mv "$GATE_ROOT/original-services" "$release_gate_service_root"
release_gate_services_cleanup
`)
	if err != nil {
		t.Fatalf("changed root identity: %v\n%s", err, output)
	}
}

func TestReleaseGateServicesExpiredAdmissionDeadlineDoesNotInvokeDocker(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
source "$HELPER"
release_gate_docker_command=("$FAKE_DOCKER")
release_gate_service_deadline=$((SECONDS - 1))
# SECONDS begins at zero in a fresh shell: use a strictly positive expired
# deadline, without sleeping or executing a Docker boundary.
SECONDS=10
release_gate_service_deadline=9
status=0; release_gate_service_docker create || status=$?
[[ "$status" == 124 ]] || exit 1
release_gate_service_fixture() { printf 'unexpected generator\n' > "$FAKE_STATE/fixture-called"; return 93; }
status=0; release_gate_service_resources "$FIXTURE_WORKSPACE" || status=$?
[[ "$status" == 124 && ! -e "$FAKE_STATE/fixture-called" ]] || exit 1
`)
	if err != nil {
		t.Fatalf("expired service admission: %v\n%s", err, output)
	}
	entries, err := os.ReadDir(self.state)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired admission touched Docker: %v %v", entries, err)
	}
}

func TestReleaseGateServicesWrongOwnerLabelRefusesMutation(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
id="$(< "$release_gate_service_root/postgres.cid")"
original="$(< "$FAKE_STATE/$id.meta")"
read -r name owner service restart binding <<< "$original"
printf '%s %s %s %s %s\n' "$name" 00000000000000000000000000000000 "$service" "$restart" "$binding" > "$FAKE_STATE/$id.meta"
if release_gate_services_cleanup; then exit 90; fi
[[ -f "$FAKE_STATE/$id.meta" ]] || exit 1
printf '%s\n' "$original" > "$FAKE_STATE/$id.meta"
release_gate_services_cleanup
`)
	if err != nil {
		t.Fatalf("changed owner label: %v\n%s", err, output)
	}
}

func TestReleaseGateServicesWrongRecordedIDRefusesForeignContainer(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
pg_id="$(< "$release_gate_service_root/postgres.cid")"
redis_id="$(< "$release_gate_service_root/redis.cid")"
printf '%s\n' "$redis_id" > "$release_gate_service_root/postgres.cid"
if release_gate_service_find postgres; then exit 90; fi
[[ -f "$FAKE_STATE/$redis_id.meta" && -f "$FAKE_STATE/$pg_id.meta" ]] || exit 1
printf '%s\n' "$pg_id" > "$release_gate_service_root/postgres.cid"
release_gate_services_cleanup
`)
	if err != nil {
		t.Fatalf("changed recorded ID: %v\n%s", err, output)
	}
}

func TestReleaseGateServicesRejectNonLoopbackDaemonBinding(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
trap 'release_gate_services_cleanup' EXIT
id="$(< "$release_gate_service_root/postgres.cid")"
read -r name owner service restart binding < "$FAKE_STATE/$id.meta"
printf '%s %s %s %s %s\n' "$name" "$owner" "$service" "$restart" 0.0.0.0:35431 > "$FAKE_STATE/$id.meta"
if release_gate_service_endpoint postgres 5432 "$id"; then exit 90; fi
`)
	if err != nil {
		t.Fatalf("non-loopback binding: %v\n%s", err, output)
	}
}

func TestReleaseGateServicesRejectAPEXBeforeCreation(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
export APEX_CONTAINER_EVALUATION=1
if release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"; then exit 90; fi
release_gate_services_cleanup
[[ ! -e "$GATE_ROOT/services" ]] || exit 1
`)
	if err != nil || !strings.Contains(string(output), "APEX credential override") {
		t.Fatalf("incompatible credential override: %v\n%s", err, output)
	}
}

const releaseGatePrivateShellProfile = `
release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
trap 'release_gate_services_cleanup' EXIT
source "$release_gate_service_root/environment.sh"
export WARP_TEST_ENV_TCP_PROBE="$PRIVATE_PROBE"
`

// The private portable root names its own loopback endpoints, but never one on
// a production data tunnel port, in either service: the refusal comes before
// any probe and leaves no attestation.
func TestTestEnvironmentPrivatePortableShellRefusesTunnelPorts(t *testing.T) {
	for _, c := range []struct {
		ports     string
		errorText string
	}{
		{ports: "FAKE_PG_PORT=5432", errorText: "postgres endpoint 127.0.0.1:5432 is a loopback production data tunnel port"},
		{ports: "FAKE_REDIS_PORT=6400", errorText: "redis endpoint 127.0.0.1:6400 is a loopback production data tunnel port"},
	} {
		self := newReleaseGateServicesFixture(t)
		output, err := self.run(t, "export "+c.ports+"\n"+releaseGatePrivateShellProfile+`
set +e
source "$FIXTURE_WORKSPACE/server/test-env.sh"
source_status=$?
set -e
[[ "$source_status" != 0 ]] || exit 1
[[ ! -e "$GATE_ROOT/probes" ]] || exit 1
[[ -z "${WARP_TEST_ENV_ATTESTED_ADDRS:-}" ]] || exit 1
`)
		if err != nil || !strings.Contains(string(output), c.errorText) {
			t.Errorf("%s: private tunnel-port refusal: %v\n%s", c.ports, err, output)
		}
	}
}

func TestTestEnvironmentPrivatePortableShellProfileProbesOnlyOwnedEndpoints(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, releaseGatePrivateShellProfile+`
source "$FIXTURE_WORKSPACE/server/test-env.sh"
test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"
[[ "$BRINGYOUR_POSTGRES_HOSTNAME" == 127.0.0.1 && "$BRINGYOUR_REDIS_HOSTNAME" == 127.0.0.1 ]] || exit 1
[[ "$WARP_SITE_HOME" == "$WARP_TEST_ENV_PORTABLE_ROOT/site" ]] || exit 1
# the explicit portable mode attests exactly its own loopback endpoints
[[ "$WARP_TEST_ENV_ATTESTED_ADDRS" == 127.0.0.1:35431,127.0.0.1:36371 ]] || exit 1
`)
	if err != nil {
		t.Fatalf("private shell profile: %v\n%s", err, output)
	}
	probes, err := os.ReadFile(filepath.Join(self.root, "probes"))
	if err != nil || string(probes) != "postgres 127.0.0.1 35431\nredis 127.0.0.1 36371\n" {
		t.Fatalf("private shell probes=%q error=%v", probes, err)
	}
}

// The adapter must satisfy the complete unchanged guard, not only the four
// service resources. Removing auth reproduces the original pre-body refusal.
func TestReleaseGateServicesSuiteManifestRejectsMissingAuth(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, releaseGatePrivateShellProfile+`
source "$FIXTURE_WORKSPACE/server/test-env.sh"
test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"
mv "$WARP_VAULT_HOME/auth.yml" "$WARP_VAULT_HOME/auth.retained"
if test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"; then exit 90; fi
`)
	if err != nil || !strings.Contains(string(output), "required resource is missing:") || !strings.Contains(string(output), "auth.yml") {
		t.Fatalf("complete private suite guard did not retain missing-auth refusal: %v\n%s", err, output)
	}
}

// A failed real-generator command cannot publish an environment pointing at
// incomplete resources; the existing owner still cleans both private services.
func TestReleaseGateServicesFixtureFailureRetainsRefusalAndCleansPair(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
release_gate_service_fixture() { printf 'fixture command refused\n' >&2; return 37; }
trap 'release_gate_services_cleanup' EXIT
if release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"; then exit 90; fi
[[ ! -e "$release_gate_service_root/environment.sh" ]] || exit 1
`)
	if err != nil || !strings.Contains(string(output), "fixture command refused") {
		t.Fatalf("fixture command refusal lost: %v\n%s", err, output)
	}
	removed, err := os.ReadFile(filepath.Join(self.state, "removed"))
	if err != nil || len(strings.Fields(string(removed))) != 2 {
		t.Fatalf("fixture failure leaked its service pair: %v", err)
	}
}

func TestTestEnvironmentPrivatePortableShellRejectsMaintenanceBeforeProbe(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, releaseGatePrivateShellProfile+`
printf 'authority: shared.invalid:5432\n' > "$WARP_TEST_ENV_PORTABLE_ROOT/vault/pg_maintenance.yml"
set +e
source "$FIXTURE_WORKSPACE/server/test-env.sh"
source_status=$?
set -e
[[ "$source_status" != 0 ]] || exit 1
[[ ! -e "$GATE_ROOT/probes" ]] || exit 1
`)
	if err != nil || !strings.Contains(string(output), "maintenance resource differs") {
		t.Fatalf("maintenance shell refusal: %v\n%s", err, output)
	}
}

func TestTestEnvironmentPrivatePortableShellRequiresBothEscapeFlags(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, releaseGatePrivateShellProfile+`
export WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES=0
set +e
source "$FIXTURE_WORKSPACE/server/test-env.sh"
source_status=$?
set -e
[[ "$source_status" != 0 ]] || exit 1
[[ ! -e "$GATE_ROOT/probes" ]] || exit 1
`)
	if err != nil || !strings.Contains(string(output), "requires both portable service flags") {
		t.Fatalf("private flags shell refusal: %v\n%s", err, output)
	}
}

func TestTestEnvironmentPrivatePortableShellRejectsOutOfRangePort(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, releaseGatePrivateShellProfile+`
export WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY=127.0.0.1:65536
set +e
source "$FIXTURE_WORKSPACE/server/test-env.sh"
source_status=$?
set -e
[[ "$source_status" != 0 ]] || exit 1
[[ ! -e "$GATE_ROOT/probes" ]] || exit 1
`)
	if err != nil || !strings.Contains(string(output), "explicit loopback host and port") {
		t.Fatalf("private port shell refusal: %v\n%s", err, output)
	}
}

func TestTestEnvironmentPrivatePortableShellRejectsAncestorAlias(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, releaseGatePrivateShellProfile+`
ln -s "$GATE_ROOT" "$GATE_ROOT/alias"
export WARP_TEST_ENV_PORTABLE_ROOT="$GATE_ROOT/alias${WARP_TEST_ENV_PORTABLE_ROOT#"$GATE_ROOT"}"
set +e
source "$FIXTURE_WORKSPACE/server/test-env.sh"
source_status=$?
set -e
[[ "$source_status" != 0 ]] || exit 1
[[ ! -e "$GATE_ROOT/probes" ]] || exit 1
`)
	if err != nil || !strings.Contains(string(output), "must not contain a path alias") {
		t.Fatalf("private path shell refusal: %v\n%s", err, output)
	}
}

func pushPrivatePortableTestResources(t *testing.T) {
	t.Helper()
	pushTestEnvironmentPreflightResources(t)
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", releaseGateCanonicalTempDir(t))
	t.Setenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES", "1")
	t.Setenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES", "1")
	t.Setenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY", "127.0.0.1:35431")
	t.Setenv("WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY", "127.0.0.1:36371")
	t.Setenv("BRINGYOUR_POSTGRES_HOSTNAME", "127.0.0.1")
	t.Setenv("BRINGYOUR_REDIS_HOSTNAME", "127.0.0.1")
	pg := []byte("authority: 127.0.0.1:35431\nuser: test\npassword: public\ndb: test\n")
	pops := []func(){Vault.PushSimpleResource(DefaultPgVaultResourceName, pg), Vault.PushSimpleResource(MaintenancePgVaultResourceName, pg), Vault.PushSimpleResource("redis.yml", []byte("authority: 127.0.0.1:36371\npassword: \"\"\ndb: 0\ncluster: false\n"))}
	t.Cleanup(func() {
		for i := len(pops) - 1; i >= 0; i-- {
			pops[i]()
		}
	})
}

func assertPrivatePortableRefusedBeforeProbe(t *testing.T, expected string) {
	t.Helper()
	called := false
	err := preflightTestEnvironment(func(context.Context, string, testEnvironmentConfiguration) error { called = true; return nil })
	if err == nil || !strings.Contains(err.Error(), expected) || called {
		t.Fatalf("private authority preflight: error=%v probed=%t, want %q before any probe", err, called, expected)
	}
}

func TestTestEnvironmentPrivatePortableAcceptsExactAuthorities(t *testing.T) {
	pushPrivatePortableTestResources(t)
	probed := []string{}
	err := preflightTestEnvironment(func(_ context.Context, name string, configuration testEnvironmentConfiguration) error {
		probed = append(probed, testEnvironmentServiceAuthority(name, configuration))
		return nil
	})
	if err != nil || strings.Join(probed, ",") != "127.0.0.1:35431,127.0.0.1:36371" {
		t.Fatalf("private probes=%q error=%v", probed, err)
	}
}

func TestTestEnvironmentPrivatePortableRequiresBothEscapeFlags(t *testing.T) {
	pushPrivatePortableTestResources(t)
	for _, name := range []string{"WARP_TEST_ENV_USE_PORTABLE_RESOURCES", "WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES"} {
		t.Setenv(name, "0")
		assertPrivatePortableRefusedBeforeProbe(t, "requires both portable service flags")
		t.Setenv(name, "1")
	}
}

func TestTestEnvironmentPrivatePortableRejectsSameHostWrongPostgresPort(t *testing.T) {
	pushPrivatePortableTestResources(t)
	t.Setenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY", "127.0.0.1:35432")
	assertPrivatePortableRefusedBeforeProbe(t, "postgres authority differs")
}

func TestTestEnvironmentPrivatePortableRejectsSameHostWrongRedisPort(t *testing.T) {
	pushPrivatePortableTestResources(t)
	t.Setenv("WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY", "127.0.0.1:36372")
	assertPrivatePortableRefusedBeforeProbe(t, "redis authority differs")
	if _, err := loadTestRedisLeaseConfiguration(); err == nil {
		t.Fatal("Redis-only lease path ignored exact private port")
	}
}

func TestTestEnvironmentPrivatePortableRejectsMaintenanceRedirectBeforeProbe(t *testing.T) {
	pushPrivatePortableTestResources(t)
	pop := Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte("authority: shared.invalid:5432\nuser: test\npassword: public\ndb: test\n"))
	defer pop()
	assertPrivatePortableRefusedBeforeProbe(t, "maintenance resource differs")
}

func TestTestEnvironmentPrivatePortableRejectsNoncanonicalPort(t *testing.T) {
	pushPrivatePortableTestResources(t)
	for _, port := range []string{"0", "65536", "035431", "-1"} {
		t.Setenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY", fmt.Sprintf("127.0.0.1:%s", port))
		assertPrivatePortableRefusedBeforeProbe(t, "requires an explicit loopback host and port")
	}
}
