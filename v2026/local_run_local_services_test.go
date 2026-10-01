package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs the real credential handoff and psql invocation with an isolated local
// process standing in for Docker. No daemon, network, or credential is needed.
func runLocalPostgresAccessProbe(t *testing.T, selectedPassword string, residentPassword string, capability string) ([]byte, error) {
	t.Helper()
	binDir := t.TempDir()
	psqlScript := `#!/usr/bin/env bash
set -euo pipefail
# The official image trusts localhost even with a wrong password. Model that
# path so switching the real probe back to localhost deterministically fails.
arguments=("$@")
for ((i = 0; i + 1 < $#; i++)); do
  if [[ "${arguments[i]}" == --host && "${arguments[i+1]}" == 127.0.0.1 ]]; then
    printf 't\n'
    exit 0
  fi
done
[[ "$PGPASSWORD" == "$RESIDENT_POSTGRES_PASSWORD" ]] || exit 2
[[ "$PGCONNECT_TIMEOUT" == 5 ]] || exit 3
[[ "$PGOPTIONS" == "-c statement_timeout=5000" ]] || exit 4
expected=(-X --no-password -qAt -v ON_ERROR_STOP=1 --host fixture-container --port 5432 --username fixture-user --dbname fixture-database --command "SELECT rolcreatedb FROM pg_roles WHERE rolname = current_user;")
[[ "$#" == "${#expected[@]}" ]] || exit 5
for expected_argument in "${expected[@]}"; do
  [[ "$1" == "$expected_argument" ]] || exit 6
  shift
done
printf '%s\n' "$POSTGRES_ROLE_CAPABILITY"
`
	if err := os.WriteFile(filepath.Join(binDir, "psql"), []byte(psqlScript), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail
source "$1"
fixture_docker() {
  [[ "$1" == exec && "$2" == -i && "$3" == fixture-container ]] || return 2
  shift 3
  "$@"
}
DOCKER=(fixture_docker)
local_postgres_require_application_access fixture-container fixture-user "$SELECTED_POSTGRES_PASSWORD" fixture-database
`, "local-services-test", filepath.Join("local", "run-local-services.sh"))
	cmd.Env = testCommandEnvironment(map[string]string{
		"PATH":                       binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SELECTED_POSTGRES_PASSWORD": selectedPassword,
		"RESIDENT_POSTGRES_PASSWORD": residentPassword,
		"POSTGRES_ROLE_CAPABILITY":   capability,
	})
	return cmd.CombinedOutput()
}

// A listener initialized under another profile must fail readiness even when
// its socket and container health checks succeeded. No password enters output.
func TestRunLocalPostgresRejectsDifferentInitializedProfile(t *testing.T) {
	output, err := runLocalPostgresAccessProbe(t, "new-profile-password", "initialized-profile-password", "t")
	if err == nil || !strings.Contains(string(output), "PostgreSQL application authentication failed") {
		t.Fatalf("mismatched profile readiness = %v, %q; want authentication failure", err, output)
	}
	for _, secret := range []string{"new-profile-password", "initialized-profile-password"} {
		if strings.Contains(string(output), secret) {
			t.Fatal("readiness diagnostic exposed a password")
		}
	}
}

// Shell metacharacters and newlines remain data while the probe checks the
// exact selected role, database, TCP endpoint, and bounded read-only query.
func TestRunLocalPostgresAcceptsMatchingInitializedProfile(t *testing.T) {
	password := "fixture '$name`value`\\\nsecond line"
	output, err := runLocalPostgresAccessProbe(t, password, password, "t")
	if err != nil || len(output) != 0 {
		t.Fatalf("matching profile readiness = %v, %q; want silent success", err, output)
	}
}

// Authentication is insufficient when an old role or unexpected response
// cannot establish the CREATEDB capability the integration setup requires.
func TestRunLocalPostgresRejectsMissingCreateDatabaseCapability(t *testing.T) {
	for _, capability := range []string{"f", "", "t\nt", "true"} {
		output, err := runLocalPostgresAccessProbe(t, "fixture-password", "fixture-password", capability)
		if err == nil || !strings.Contains(string(output), "role must have CREATEDB") {
			t.Errorf("capability %q: readiness = %v, %q; want capability failure", capability, err, output)
		}
	}
}

// The lifecycle cannot advertise a usable stack before authenticating with the
// selected resource; Docker's health check uses a different local socket role.
func TestRunLocalPostgresAuthenticationPrecedesReadyAnnouncement(t *testing.T) {
	contentBytes, err := os.ReadFile(filepath.Join("local", "run-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(contentBytes)
	probeAt := strings.Index(content, `local_postgres_require_application_access "$PG_CONTAINER" "$PG_USER" "$PG_PASSWORD" "$PG_DB" ||`)
	readyAt := strings.Index(content, "  Local environment is up.")
	if probeAt < 0 || readyAt < probeAt {
		t.Fatal("launcher announces readiness before requiring selected-profile authentication")
	}
	if !strings.Contains(content[probeAt:readyAt], `die "postgres does not satisfy the selected local test profile"`) {
		t.Fatal("launcher does not stop after application authentication failure")
	}
}
