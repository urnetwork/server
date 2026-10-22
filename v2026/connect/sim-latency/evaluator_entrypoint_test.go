// Runs the evaluator entrypoint's job-service attestation with a synthetic
// vault and a scripted getent. No container, resolver or service is used, and
// the dispatch that would exec the migrator or runner is never reached.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const evaluatorEntrypointPath = "evaluator/container/entrypoint.sh"

// Runs attest_job_services from the real entrypoint against one synthetic job:
// each answer lists the addresses getent returns for a name, and a name without
// an answer fails to resolve.
func runEvaluatorEntrypointAttestation(
	t *testing.T,
	postgresAuthority string,
	redisAuthority string,
	answers map[string][]string,
	environment ...string,
) (string, error) {
	t.Helper()
	content, err := os.ReadFile(evaluatorEntrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	boundary := bytes.Index(content, []byte("# --- dispatch ---"))
	if boundary < 0 {
		t.Fatal("entrypoint dispatch boundary is missing")
	}
	script := string(content[:boundary]) + "attest_job_services\nprintf '%s\\n' \"$WARP_TEST_ENV_ATTESTED_ADDRS\"\n"
	scriptPath := filepath.Join(t.TempDir(), "attest.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	warpHome := t.TempDir()
	vaultDir := filepath.Join(warpHome, "vault", "local")
	if err := os.MkdirAll(vaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, resource := range map[string]string{
		"pg.yml":    "authority: \"" + postgresAuthority + "\"\nuser: \"synthetic\"\npassword: \"synthetic\"\ndb: \"synthetic\"\n",
		"redis.yml": "authority: \"" + redisAuthority + "\"\npassword: \"synthetic\"\ndb: 0\ncluster: false\n",
	} {
		if err := os.WriteFile(filepath.Join(vaultDir, name), []byte(resource), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// getent ahosts prints one line per address and socket type.
	getent := "#!/bin/sh\n[ \"$1\" = ahosts ] || exit 2\ncase \"$2\" in\n"
	for name, addresses := range answers {
		getent += "  '" + name + "')"
		for _, address := range addresses {
			getent += " printf '%s STREAM " + name + "\\n%s DGRAM\\n' '" + address + "' '" + address + "';"
		}
		getent += " ;;\n"
	}
	getent += "  *) exit 2 ;;\nesac\n"
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "getent"), []byte(getent), 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"WARP_HOME=" + warpHome,
	}
	cmd.Env = append(cmd.Env, environment...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// The job's postgres and redis names each resolve once to the one address
// Compose gave the service, and the entrypoint exports those endpoints as the
// attestation the Go processes dial.
func TestEvaluatorEntrypointAttestsJobServices(t *testing.T) {
	cases := []struct {
		answers     map[string][]string
		attestation string
	}{
		{
			answers:     map[string][]string{"postgres": {"192.0.2.21"}, "redis": {"192.0.2.22"}},
			attestation: "192.0.2.21:5432,192.0.2.22:6379",
		},
		{
			answers:     map[string][]string{"postgres": {"192.0.2.21"}, "redis": {"2001:db8::22"}},
			attestation: "192.0.2.21:5432,[2001:db8::22]:6379",
		},
	}
	for _, c := range cases {
		output, err := runEvaluatorEntrypointAttestation(t, "postgres:5432", "redis:6379", c.answers)
		if err != nil || output != c.attestation+"\n" {
			t.Errorf("attestation for %v = %v, %q; want %q", c.answers, err, output, c.attestation)
		}
	}
}

// Direct local vault leaves use these exact resolver placeholders. Only their
// resource-specific Compose hostname is substituted; literal authorities remain
// compatible and the final attestation still contains resolved addresses only.
func TestEvaluatorEntrypointAttestsApprovedEnvironmentAuthorities(t *testing.T) {
	cases := []struct {
		postgresAuthority string
		redisAuthority    string
	}{
		{postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", redisAuthority: "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379"},
		{postgresAuthority: "pg.service.example:5432", redisAuthority: "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379"},
		{postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", redisAuthority: "redis.service.example:6379"},
	}
	answers := map[string][]string{"pg.service.example": {"192.0.2.21"}, "redis.service.example": {"2001:db8::22"}}
	for _, c := range cases {
		output, err := runEvaluatorEntrypointAttestation(t, c.postgresAuthority, c.redisAuthority, answers,
			"BRINGYOUR_POSTGRES_HOSTNAME=pg.service.example", "BRINGYOUR_REDIS_HOSTNAME=redis.service.example")
		if err != nil || output != "192.0.2.21:5432,[2001:db8::22]:6379\n" {
			t.Errorf("approved authorities %q / %q = %v, %q", c.postgresAuthority, c.redisAuthority, err, output)
		}
	}
}

// The compatibility path is deliberately not a general template evaluator:
// absent variables, other variable names, cross-resource references and malformed
// expressions fail before any endpoint can become authoritative.
func TestEvaluatorEntrypointRefusesMalformedEnvironmentAuthorities(t *testing.T) {
	cases := []struct {
		name              string
		postgresAuthority string
		redisAuthority    string
		environment       []string
		resource          string
	}{
		{name: "missing postgres variable", postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", redisAuthority: "redis:6379", resource: "pg.yml"},
		{name: "empty postgres variable", postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME="}, resource: "pg.yml"},
		{name: "missing redis variable", postgresAuthority: "postgres:5432", redisAuthority: "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379", resource: "redis.yml"},
		{name: "empty redis variable", postgresAuthority: "postgres:5432", redisAuthority: "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379", environment: []string{"BRINGYOUR_REDIS_HOSTNAME="}, resource: "redis.yml"},
		{name: "other variable", postgresAuthority: "{{ env:SYNTHETIC_HOSTNAME }}:5432", redisAuthority: "redis:6379", environment: []string{"SYNTHETIC_HOSTNAME=postgres"}, resource: "pg.yml"},
		{name: "postgres references redis", postgresAuthority: "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:5432", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_REDIS_HOSTNAME=postgres"}, resource: "pg.yml"},
		{name: "redis references postgres", postgresAuthority: "postgres:5432", redisAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=redis"}, resource: "redis.yml"},
		{name: "missing expression brace", postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }:5432", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=postgres"}, resource: "pg.yml"},
		{name: "unapproved expression spacing", postgresAuthority: "{{env:BRINGYOUR_POSTGRES_HOSTNAME}}:5432", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=postgres"}, resource: "pg.yml"},
		{name: "expression prefix", postgresAuthority: "prefix-{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=postgres"}, resource: "pg.yml"},
		{name: "expression suffix", postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}.example:5432", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=postgres"}, resource: "pg.yml"},
		{name: "templated port", postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:{{ env:SYNTHETIC_PORT }}", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=postgres", "SYNTHETIC_PORT=5432"}, resource: "pg.yml"},
		{name: "environment contains port", postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=postgres:5432"}, resource: "pg.yml"},
		{name: "environment contains newline", postgresAuthority: "postgres:5432", redisAuthority: "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379", environment: []string{"BRINGYOUR_REDIS_HOSTNAME=redis\npostgres"}, resource: "redis.yml"},
		{name: "port outside range", postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:65536", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=postgres"}, resource: "pg.yml"},
		{name: "unmatched quotation", postgresAuthority: "postgres:5432\"", redisAuthority: "redis:6379", resource: "pg.yml"},
		{name: "duplicate literal authority", postgresAuthority: "postgres:5432\"\nauthority: \"postgres:5432", redisAuthority: "redis:6379", resource: "pg.yml"},
		{name: "duplicate empty authority", postgresAuthority: "postgres:5432\"\nauthority: \"", redisAuthority: "redis:6379", resource: "pg.yml"},
		{name: "duplicate redis authority", postgresAuthority: "postgres:5432", redisAuthority: "redis:6379\"\nauthority: \"redis:6379", resource: "redis.yml"},
	}
	answers := map[string][]string{"postgres": {"192.0.2.21"}, "redis": {"192.0.2.22"}}
	for _, c := range cases {
		output, err := runEvaluatorEntrypointAttestation(t, c.postgresAuthority, c.redisAuthority, answers, c.environment...)
		if err == nil || !strings.Contains(output, c.resource+" needs one host:port authority") {
			t.Errorf("%s: attestation = %v, %q", c.name, err, output)
		}
	}
}

// Hostname environment values and vault text are inert data, even if they
// resemble shell substitutions. A private marker catches accidental evaluation.
func TestEvaluatorEntrypointNeverEvaluatesAuthorityExpressions(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "must-not-exist")
	answers := map[string][]string{"postgres": {"192.0.2.21"}, "redis": {"192.0.2.22"}}
	for _, expression := range []string{"$(touch '" + markerPath + "')", "`touch '" + markerPath + "'`"} {
		for _, c := range []struct {
			postgresAuthority string
			redisAuthority    string
			environment       []string
		}{
			{postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", redisAuthority: "redis:6379", environment: []string{"BRINGYOUR_POSTGRES_HOSTNAME=" + expression}},
			{postgresAuthority: "postgres:5432", redisAuthority: "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379", environment: []string{"BRINGYOUR_REDIS_HOSTNAME=" + expression}},
			{postgresAuthority: expression + ":5432", redisAuthority: "redis:6379"},
		} {
			output, err := runEvaluatorEntrypointAttestation(t, c.postgresAuthority, c.redisAuthority, answers, c.environment...)
			if err == nil || !strings.Contains(output, "needs one host:port authority") {
				t.Errorf("shell-like authority was not refused: %v, %q", err, output)
			}
			if _, err := os.Lstat(markerPath); !os.IsNotExist(err) {
				t.Fatalf("authority expression executed: %v", err)
			}
		}
	}
}

// Environment substitution does not bypass the existing address proof: both
// services must still resolve uniquely to a non-loopback job endpoint.
func TestEvaluatorEntrypointEnvironmentAuthoritiesRetainAddressGuards(t *testing.T) {
	cases := []struct {
		name      string
		answers   map[string][]string
		errorText string
	}{
		{name: "postgres loopback", answers: map[string][]string{"postgres": {"127.0.0.1"}, "redis": {"192.0.2.22"}}, errorText: "postgres resolves to loopback"},
		{name: "redis loopback", answers: map[string][]string{"postgres": {"192.0.2.21"}, "redis": {"::1"}}, errorText: "redis resolves to loopback"},
		{name: "postgres ambiguous", answers: map[string][]string{"postgres": {"192.0.2.21", "192.0.2.23"}, "redis": {"192.0.2.22"}}, errorText: "postgres must resolve to exactly one address"},
		{name: "redis ambiguous", answers: map[string][]string{"postgres": {"192.0.2.21"}, "redis": {"192.0.2.22", "192.0.2.24"}}, errorText: "redis must resolve to exactly one address"},
		{name: "postgres absent", answers: map[string][]string{"redis": {"192.0.2.22"}}, errorText: "postgres did not resolve"},
		{name: "redis absent", answers: map[string][]string{"postgres": {"192.0.2.21"}}, errorText: "redis did not resolve"},
	}
	for _, c := range cases {
		output, err := runEvaluatorEntrypointAttestation(t, "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379", c.answers,
			"BRINGYOUR_POSTGRES_HOSTNAME=postgres", "BRINGYOUR_REDIS_HOSTNAME=redis")
		if err == nil || !strings.Contains(output, c.errorText) {
			t.Errorf("%s: attestation = %v, %q", c.name, err, output)
		}
	}
}

// Anything but one address that is not loopback, or an authority that is not
// one host:port, leaves the job unattested; the Go processes then refuse every
// dial instead of reaching whatever loopback serves.
func TestEvaluatorEntrypointRefusesUnattestableJobServices(t *testing.T) {
	cases := []struct {
		name              string
		postgresAuthority string
		answers           map[string][]string
		errorText         string
	}{
		{
			name:              "loopback",
			postgresAuthority: "postgres:5432",
			answers:           map[string][]string{"postgres": {"127.0.0.1"}, "redis": {"192.0.2.22"}},
			errorText:         "postgres resolves to loopback 127.0.0.1",
		},
		{
			name:              "IPv6 loopback",
			postgresAuthority: "postgres:5432",
			answers:           map[string][]string{"postgres": {"::1"}, "redis": {"192.0.2.22"}},
			errorText:         "postgres resolves to loopback ::1",
		},
		{
			name:              "several addresses",
			postgresAuthority: "postgres:5432",
			answers:           map[string][]string{"postgres": {"192.0.2.21", "192.0.2.23"}, "redis": {"192.0.2.22"}},
			errorText:         "postgres must resolve to exactly one address",
		},
		{
			name:              "unresolved",
			postgresAuthority: "postgres:5432",
			answers:           map[string][]string{"redis": {"192.0.2.22"}},
			errorText:         "postgres did not resolve",
		},
		{
			name:              "unset templated authority",
			postgresAuthority: "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432",
			answers:           map[string][]string{"postgres": {"192.0.2.21"}, "redis": {"192.0.2.22"}},
			errorText:         "pg.yml needs one host:port authority",
		},
		{
			name:              "port out of range",
			postgresAuthority: "postgres:65536",
			answers:           map[string][]string{"postgres": {"192.0.2.21"}, "redis": {"192.0.2.22"}},
			errorText:         "pg.yml needs one host:port authority",
		},
	}
	for _, c := range cases {
		output, err := runEvaluatorEntrypointAttestation(t, c.postgresAuthority, "redis:6379", c.answers)
		if err == nil || !strings.Contains(output, c.errorText) {
			t.Errorf("%s: attestation = %v, %q; want a refusal containing %q", c.name, err, output, c.errorText)
		}
	}
}

// The migrator and the runner reach the job's services, so both attest before
// they exec; the other actions never dial them.
func TestEvaluatorEntrypointAttestsBeforeDatabaseActions(t *testing.T) {
	content, err := os.ReadFile(evaluatorEntrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	boundary := bytes.Index(content, []byte("# --- dispatch ---"))
	if boundary < 0 {
		t.Fatal("entrypoint dispatch boundary is missing")
	}
	dispatch := string(content[boundary:])
	for _, action := range []struct {
		label string
		exec  string
	}{
		{label: "    migrate)\n", exec: "exec /usr/local/libexec/competitiondbinit"},
		{label: "    run)\n", exec: "exec \"$OFFICIAL_RUN\" \"$1\""},
	} {
		labelAt := strings.Index(dispatch, action.label)
		if labelAt < 0 {
			t.Errorf("entrypoint has no %q action", strings.TrimSpace(action.label))
			continue
		}
		branch, _, _ := strings.Cut(dispatch[labelAt:], ";;")
		attestAt := strings.Index(branch, "attest_job_services")
		execAt := strings.Index(branch, action.exec)
		if attestAt < 0 || execAt < attestAt {
			t.Errorf("entrypoint %s does not attest the job services before it execs", strings.TrimSpace(action.label))
		}
	}
}
