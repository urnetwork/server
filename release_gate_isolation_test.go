package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
    [[ "$owner" =~ ^[0-9a-f]{32}$ && "$name" == "urnetwork-gate-$owner-$service" && "$restart" == no ]]
    id="$(printf '%s' "$name" | sha256sum)"; id="${id%% *}"
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
    else printf '%s /%s %s %s %s\n' "$id" "$name" "$owner" "$service" "$restart"; fi ;;
  container)
    [[ "$2" == ls ]]
    filter="${!#}"
    for file in "$FAKE_STATE"/*.meta; do
      [[ -f "$file" ]] || continue
      read -r name owner service restart binding < "$file"
      id="${file##*/}"; id="${id%.meta}"
      if [[ "$filter" == "name=^/${name}$" || "$filter" == "id=$id" ]]; then printf '%s\n' "$id"; fi
    done ;;
  start) [[ -f "$FAKE_STATE/$2.meta" ]] ;;
  exec)
    read -r name owner service restart binding < "$FAKE_STATE/$2.meta"
    if [[ "$service" == postgres ]]; then printf '512:256MB:en_US.UTF-8:t\n'; else printf 'PONG\n'; fi ;;
  rm)
    id="${!#}"; [[ "$id" =~ ^[0-9a-f]{64}$ ]]
    [[ -f "$FAKE_STATE/$id.meta" ]]
    printf '%s\n' "$id" >> "$FAKE_STATE/removed"
    /bin/rm -- "$FAKE_STATE/$id.meta" ;;
  *) printf 'unexpected Docker fixture call: %s\n' "$*" >&2; exit 92 ;;
esac
`

type releaseGateServicesFixture struct {
	root      string
	workspace string
	state     string
	helper    string
	docker    string
	lock      string
	probe     string
	generator string
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

func newReleaseGateServicesFixture(t *testing.T) *releaseGateServicesFixture {
	t.Helper()
	self := &releaseGateServicesFixture{
		root:      filepath.Join(releaseGateCanonicalTempDir(t), "gate"),
		workspace: releaseGateCanonicalTempDir(t),
		state:     releaseGateCanonicalTempDir(t),
	}
	if err := os.Mkdir(self.root, 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	self.helper = filepath.Join(server, "local", "release-gate-services.sh")
	// Build the actual stdlib-only generator outside each bounded Docker
	// ownership script; no authentication or resource result is doubled.
	self.generator = filepath.Join(releaseGateCanonicalTempDir(t), "server-fixture")
	buildCtx, buildCancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", self.generator, "./scripts/server-fixture")
	build.Dir = filepath.Join(server, "..", "sn")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exact private suite generator: %v\n%s", err, output)
	}
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
source "$HELPER"
release_gate_service_docker() { "$FAKE_DOCKER" "$@"; }
release_gate_service_fixture() { shift 2; "$FIXTURE_GENERATOR" "$@"; }
`+body)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 10 * time.Second
	command.Env = testCommandEnvironment(map[string]string{
		"HELPER": self.helper, "FAKE_DOCKER": self.docker, "FAKE_STATE": self.state,
		"FIXTURE_GENERATOR": self.generator,
		"GATE_ROOT":         self.root, "FIXTURE_WORKSPACE": self.workspace, "FIXTURE_LOCK": self.lock,
		"FAKE_PG_PORT": "35431", "FAKE_REDIS_PORT": "36371",
		"APEX_CONTAINER_EVALUATION": "", "FAKE_FAIL_CREATE": "",
		"PRIVATE_PROBE": self.probe, "WARP_ENV": "local",
	})
	return command.CombinedOutput()
}

func TestReleaseGateServicesPrivateResourcesAndNoRestart(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
before_umask="$(umask)"
trap 'release_gate_services_cleanup' EXIT
release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
[[ "$(umask)" == "$before_umask" ]]
source "$release_gate_service_root/environment.sh"
[[ "$WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY" == 127.0.0.1:35431 && "$WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY" == 127.0.0.1:36371 ]]
[[ "$(stat -c '%a' "$release_gate_service_root/postgres.cid")" == 600 ]]
cmp "$WARP_TEST_ENV_PORTABLE_ROOT/vault/pg.yml" "$WARP_TEST_ENV_PORTABLE_ROOT/vault/pg_maintenance.yml"
cmp "$WARP_TEST_ENV_PORTABLE_ROOT/config/db.yml" "$WARP_TEST_ENV_PORTABLE_ROOT/config/db_maintenance.yml"
[[ "$(< "$WARP_TEST_ENV_PORTABLE_ROOT/config/settings.yml")" == 'all: {}' ]]
[[ -d "$WARP_SITE_HOME" && ! -e "$WARP_TEST_ENV_PORTABLE_ROOT/vault/local" ]]
[[ -f "$WARP_TEST_ENV_PORTABLE_ROOT/vault/auth.yml" && ! -L "$WARP_TEST_ENV_PORTABLE_ROOT/vault/auth.yml" ]]
[[ ! -e "$WARP_TEST_ENV_PORTABLE_ROOT/vault/nonservice.yml" ]]
for service in postgres redis; do
  id="$(< "$release_gate_service_root/$service.cid")"
  [[ "$(< "$FAKE_STATE/$id.args")" == *--restart=no* ]]
  [[ "$(< "$FAKE_STATE/$id.args")" == *--tmpfs* ]]
  [[ "$(< "$FAKE_STATE/$id.args")" == *127.0.0.1::* ]]
done
`)
	if err != nil {
		t.Fatalf("private resource setup: %v\n%s", err, output)
	}
	if files, err := filepath.Glob(filepath.Join(self.state, "*.meta")); err != nil || len(files) != 0 {
		t.Fatalf("owned resources survived cleanup: %v %v", files, err)
	}
}

func TestReleaseGateServicesLostCreateAcknowledgementCleansPartialPair(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
export FAKE_FAIL_CREATE=redis
if release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"; then exit 90; fi
[[ ! -e "$release_gate_service_root/redis.cid" ]]
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
  [[ -n "$RELEASE_GATE_SERVICE_ID" ]]
  release_gate_service_find redis
  [[ -n "$RELEASE_GATE_SERVICE_ID" ]]
)
owner_a & a_pid=$!
owner_b & b_pid=$!
read -r first <&7
read -r second <&7
[[ "$first:$second" == a:b || "$first:$second" == b:a ]]
[[ "$(< "$GATE_ROOT/a/services/owner")" != "$(< "$GATE_ROOT/b/services/owner")" ]]
printf 'release\n' >&8
status=0; wait "$a_pid" || status=$?
[[ "$status" == 23 ]]
kill -0 "$b_pid"
for service in postgres redis; do
  a_id="$(< "$GATE_ROOT/a/services/$service.cid")"
  b_id="$(< "$GATE_ROOT/b/services/$service.cid")"
  [[ "$a_id" != "$b_id" && ! -e "$FAKE_STATE/$a_id.meta" && -f "$FAKE_STATE/$b_id.meta" ]]
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
[[ ! -e "$FAKE_STATE/removed" ]]
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
[[ "$status" == 124 ]]
release_gate_service_fixture() { printf 'unexpected generator\n' > "$FAKE_STATE/fixture-called"; return 93; }
status=0; release_gate_service_resources "$FIXTURE_WORKSPACE" || status=$?
[[ "$status" == 124 && ! -e "$FAKE_STATE/fixture-called" ]]
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
[[ -f "$FAKE_STATE/$id.meta" ]]
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
[[ -f "$FAKE_STATE/$redis_id.meta" && -f "$FAKE_STATE/$pg_id.meta" ]]
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
[[ ! -e "$GATE_ROOT/services" ]]
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

func TestTestEnvironmentPrivatePortableShellProfileProbesOnlyOwnedEndpoints(t *testing.T) {
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, releaseGatePrivateShellProfile+`
source "$FIXTURE_WORKSPACE/server/test-env.sh"
test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"
[[ "$BRINGYOUR_POSTGRES_HOSTNAME" == 127.0.0.1 && "$BRINGYOUR_REDIS_HOSTNAME" == 127.0.0.1 ]]
[[ "$WARP_SITE_HOME" == "$WARP_TEST_ENV_PORTABLE_ROOT/site" ]]
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
[[ ! -e "$release_gate_service_root/environment.sh" ]]
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
[[ "$source_status" != 0 ]]
[[ ! -e "$GATE_ROOT/probes" ]]
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
[[ "$source_status" != 0 ]]
[[ ! -e "$GATE_ROOT/probes" ]]
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
[[ "$source_status" != 0 ]]
[[ ! -e "$GATE_ROOT/probes" ]]
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
[[ "$source_status" != 0 ]]
[[ ! -e "$GATE_ROOT/probes" ]]
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
