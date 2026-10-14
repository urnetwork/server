// Admits to a test binary, which otherwise reads only a fixture vault (see
// refuseNonFixtureVault), the local PostgreSQL and Redis resources of the
// launcher that test-env.sh attested.
//
// local/run-local.sh starts the local PostgreSQL and Redis services with the
// credentials in pg.yml and redis.yml of its selected vault's local directory.
// That vault is $WARP_VAULT_HOME, $WARP_HOME/vault or a sibling vault checkout,
// any of which can also hold live secrets, or else the checked-in fixture.
// test-env.sh selects the same vault, checks those resources against the
// launcher's readiness attestation, and in managed-local mode names the vault
// in WARP_TEST_ENV_LAUNCHER_VAULT_HOME. While WARP_ENV is local and the vault
// home is that vault, a test binary reads pg.yml, pg_maintenance.yml and
// redis.yml only from the vault's local directory, each only while the
// attestation is valid and names the endpoint that the selected file names. It
// reads every other resource from the checked-in fixture vault. Nothing else in
// that vault is read: not its root or all directory, no other environment such
// as main, and no other resource of its local directory.
//
// The attestation is read again on every lookup of those resources, so a
// launcher that withdraws readiness stops the reads at once. A valid attestation
// here is the record itself: its owner matches the lock in the v2 form, its
// dedicated address is neither loopback nor unspecified, and its endpoints are
// complete. test-env.sh has also checked the launcher's live process and its
// hosts mappings, and the dial guard pins every connection to the attested
// addresses (test_env_dial_guard.go).
package server

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Names the vault that test-env.sh selected for the launcher it attested.
const testLauncherVaultHomeEnvName = "WARP_TEST_ENV_LAUNCHER_VAULT_HOME"

// run-local.sh's lock directory, which holds its owner and readiness record.
const testLauncherLockDir = "/tmp/urnetwork-server-run-local.lock"

// The first line of the launcher's readiness record (run-local-state.sh).
const testLauncherAttestationFormat = "format=urnetwork-server-run-local-ready-v1"

// The owner form whose pid and nonce test-env.sh checks against the live
// launcher process.
var testLauncherOwnerTokenPattern = regexp.MustCompile(`^v2:[1-9][0-9]{0,9}:[0-9a-f]{32}$`)

// The service whose attested endpoint each launcher resource must name.
var testLauncherResourceNameServices = map[string]string{
	DefaultPgVaultResourceName:     "postgres",
	MaintenancePgVaultResourceName: "postgres",
	"redis.yml":                    "redis",
}

// The homes one vault lookup of a test binary searches. A launcher resource
// also carries the attested host:port that its selected file must name.
type testVaultScope struct {
	root             string
	env              string
	launcherEndpoint string
}

// Where a test binary's vault lookup of relPath may read, or why it may not.
// Refusals read as not found, as refuseNonFixtureVault's do.
func selectTestVaultScope(root string, env string, relPath string) (testVaultScope, error) {
	fixtureErr := refuseNonFixtureVault(root, true)
	if fixtureErr == nil {
		return testVaultScope{root: root, env: env}, nil
	}
	launcherVaultHome := os.Getenv(testLauncherVaultHomeEnvName)
	if launcherVaultHome == "" || !filepath.IsAbs(launcherVaultHome) ||
		!pathWithin(root, launcherVaultHome) || !pathWithin(launcherVaultHome, root) {
		return testVaultScope{}, fixtureErr
	}
	if env != "local" {
		return testVaultScope{}, fmt.Errorf(
			"%w: a test reads the launcher's vault %s only with WARP_ENV=local, not %q",
			ErrResourceNotFound,
			root,
			env,
		)
	}
	service, ok := testLauncherResourceNameServices[relPath]
	if !ok {
		if syntheticTestResourceRoot == "" {
			return testVaultScope{}, fmt.Errorf(
				"%w: a test reads %s from the checked-in fixture vault beside the launcher's, which a -trimpath build cannot locate",
				ErrResourceNotFound,
				relPath,
			)
		}
		return testVaultScope{root: filepath.Join(syntheticTestResourceRoot, "vault"), env: env}, nil
	}
	attestation, err := readTestLauncherAttestation()
	if err != nil {
		return testVaultScope{}, fmt.Errorf(
			"%w: a test reads the launcher's %s from %s only while its readiness attestation is valid, and %v; "+
				"start ./local/run-local.sh, wait for 'Local environment is up', and source ./test-env.sh again",
			ErrResourceNotFound,
			relPath,
			root,
			err,
		)
	}
	launcherEndpoint := attestation.postgresEndpoint
	if service == "redis" {
		launcherEndpoint = attestation.redisEndpoint
	}
	// only the local directory, which the launcher reads
	return testVaultScope{root: filepath.Join(root, "local"), launcherEndpoint: launcherEndpoint}, nil
}

// The endpoints of a valid launcher readiness record, each as host:port.
type testLauncherAttestation struct {
	postgresEndpoint string
	redisEndpoint    string
}

// Reads the launcher's lock owner and readiness record with run-local-state.sh's
// rules: one owner in the v2 form, which the record's owner_token repeats, and
// exactly the record's seven lines in order, no value empty or holding a space.
// The address must be a dedicated one, never loopback or unspecified. Errors
// never include the owner token.
func readTestLauncherAttestation() (testLauncherAttestation, error) {
	// test-env.sh's harness selects a test-only lock with its other test-only
	// local-state paths, and only all together
	lockDir := os.Getenv("WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR")
	hostsFile := os.Getenv("WARP_TEST_ENV_TEST_HOSTS_FILE")
	if lockDir == "" && hostsFile == "" {
		lockDir = testLauncherLockDir
	} else if lockDir == "" || hostsFile == "" || os.Getenv("WARP_TEST_ENV_TCP_PROBE") == "" {
		return testLauncherAttestation{}, errors.New("test-only local-state paths require each other and WARP_TEST_ENV_TCP_PROBE")
	}
	readLines := func(name string) ([]string, error) {
		bytes, err := os.ReadFile(filepath.Join(lockDir, name))
		if err != nil {
			return nil, err
		}
		return strings.Split(strings.TrimSuffix(string(bytes), "\n"), "\n"), nil
	}

	ownerLines, err := readLines("owner")
	if err != nil {
		return testLauncherAttestation{}, fmt.Errorf("the launcher lock has no readable owner: %w", err)
	}
	if len(ownerLines) != 1 || !testLauncherOwnerTokenPattern.MatchString(ownerLines[0]) {
		return testLauncherAttestation{}, fmt.Errorf("the launcher lock %s has no v2 owner", lockDir)
	}
	readyLines, err := readLines("ready")
	if err != nil {
		return testLauncherAttestation{}, fmt.Errorf("the launcher has published no readiness: %w", err)
	}
	fieldNames := []string{"owner_token", "host_ip", "postgres_host", "postgres_port", "redis_host", "redis_port"}
	if len(readyLines) != 1+len(fieldNames) || readyLines[0] != testLauncherAttestationFormat {
		return testLauncherAttestation{}, fmt.Errorf("the launcher readiness in %s is malformed", lockDir)
	}
	fieldNameValues := map[string]string{}
	for i, name := range fieldNames {
		value, ok := strings.CutPrefix(readyLines[1+i], name+"=")
		if !ok || value == "" || strings.ContainsFunc(value, unicode.IsSpace) {
			return testLauncherAttestation{}, fmt.Errorf("the launcher readiness in %s is malformed", lockDir)
		}
		fieldNameValues[name] = value
	}
	if fieldNameValues["owner_token"] != ownerLines[0] {
		return testLauncherAttestation{}, fmt.Errorf("the launcher readiness in %s does not name its lock's owner", lockDir)
	}
	hostIp, err := netip.ParseAddr(fieldNameValues["host_ip"])
	if err != nil || hostIp.Zone() != "" || hostIp.Unmap().IsLoopback() || hostIp.IsUnspecified() || hostIp.IsMulticast() {
		return testLauncherAttestation{}, fmt.Errorf(
			"the launcher readiness in %s names %s, not a dedicated non-loopback address",
			lockDir,
			fieldNameValues["host_ip"],
		)
	}
	for _, name := range []string{"postgres_port", "redis_port"} {
		port, err := strconv.Atoi(fieldNameValues[name])
		if err != nil || port < 1 || 65535 < port || strconv.Itoa(port) != fieldNameValues[name] {
			return testLauncherAttestation{}, fmt.Errorf("the launcher readiness in %s has an invalid %s", lockDir, name)
		}
	}
	return testLauncherAttestation{
		postgresEndpoint: net.JoinHostPort(fieldNameValues["postgres_host"], fieldNameValues["postgres_port"]),
		redisEndpoint:    net.JoinHostPort(fieldNameValues["redis_host"], fieldNameValues["redis_port"]),
	}, nil
}

// Requires the selected launcher resource's authority, interpolated as
// SimpleResource.String interpolates it, to be the attested endpoint. Errors
// name the file and the endpoints, never another value of the file.
func requireTestLauncherResourceEndpoint(path string, launcherEndpoint string) error {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: read the launcher's %s: %w", ErrResourceUnavailable, path, err)
	}
	obj := map[string]any{}
	// a parser error can quote the file, so it is not kept
	if yaml.Unmarshal(bytes, &obj) != nil {
		return fmt.Errorf("%w: the launcher's %s is not a yaml mapping", ErrResourceUnavailable, path)
	}
	authority, ok := obj["authority"].(string)
	if !ok {
		return fmt.Errorf("%w: the launcher's %s has no single authority", ErrResourceUnavailable, path)
	}
	missingNames := []string{}
	authority = templateExpression.ReplaceAllStringFunc(authority, func(match string) string {
		envMatches := templateEnvExpression.FindStringSubmatch(match)
		if envMatches == nil {
			return match
		}
		value := os.Getenv(envMatches[1])
		if value == "" {
			missingNames = append(missingNames, envMatches[1])
		}
		return value
	})
	if len(missingNames) != 0 {
		return fmt.Errorf(
			"%w: the authority of the launcher's %s requires environment variable %s",
			ErrResourceUnavailable,
			path,
			missingNames[0],
		)
	}
	if authority != launcherEndpoint {
		return fmt.Errorf(
			"%w: the launcher's %s names %s, not the attested endpoint %s",
			ErrResourceNotFound,
			path,
			authority,
			launcherEndpoint,
		)
	}
	return nil
}
