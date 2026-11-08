// FS-55, the rollback harness (§22.8, §23.6's every-Gn-out row; R-OPS-5,
// D-OPS-6, R-PHASE-8): a group's migrations applied while the release before
// them serves, that release restarted on the migrated schema, and then the next
// release started with the group's gate on (plans.go).
//
// It runs only on the private stack, inside the guards server/test-env.sh sets,
// never around them: TestFs55OnServerBinaries runs it through
// .stack/server-test.sh, where server.TestEnv gives it a disposable database and
// Redis db, and the binaries it builds run as WARP_ENV=local processes that the
// same attestation pins (test_env_dial_guard.go). Run directly, the command
// below takes vault and config homes that already name a disposable database:
//
//	go run ./local/fs55 -base <ref> -group MG1 -vault <home> -config <home>
//
// Before any build or dial it refuses an attestation, an authority or a vault
// that names loopback 5432, 6432 or 6379-6427, which can be production data
// tunnels, or any endpoint but the private stack's. It never touches
// ~/urnetwork's containers and runs direct go builds, never the whole-suite
// scripts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// The command line: 0 FS-55 passed, 1 it failed or was refused, 64 usage.
func run(args []string, out io.Writer, errOut io.Writer) int {
	flags := flag.NewFlagSet("fs55", flag.ContinueOnError)
	flags.SetOutput(errOut)
	serverDirectory := flags.String("server", ".", "the server checkout")
	base := flags.String("base", "", "N's git ref")
	groupName := flags.String("group", "", "the group whose Gn-out this checks, MG<n>")
	vaultHome := flags.String("vault", "", "a vault home whose pg.yml names a disposable database")
	configHome := flags.String("config", "", "a config home for WARP_ENV=local")
	extraConfigHome := flags.String("config-extra", "", "a fuller config home whose all/ and missing local/ files fill -config's")
	workDirectory := flags.String("work", "", "a scratch directory; default a new temporary one")
	if err := flags.Parse(args); err != nil || *base == "" || *groupName == "" || *vaultHome == "" || *configHome == "" {
		fmt.Fprintf(errOut, "usage: fs55 -base <ref> -group MG<n> -vault <home> -config <home> [-server <dir>] [-work <dir>]\n")
		return 64
	}
	if err := checkFs55Environment(os.Environ(), *vaultHome); err != nil {
		fmt.Fprintf(errOut, "fs55 refused: %s\n", err)
		return 1
	}
	report, err := runFs55OnServerBinaries(context.Background(), &fs55ServerOptions{
		serverDirectory: *serverDirectory,
		base:            *base,
		groupName:       *groupName,
		vaultHome:       *vaultHome,
		configHome:      *configHome,
		extraConfigHome: *extraConfigHome,
		workDirectory:   *workDirectory,
		settings:        defaultFs55Settings(),
	})
	if err != nil {
		fmt.Fprintf(errOut, "fs55 could not run: %s\n", err)
		return 1
	}
	report.Write(out)
	if !report.Passed() {
		return 1
	}
	return 0
}

// Loopback ports a dial must never reach: they can be production data tunnels
// (server/test-env.sh).
func fs55RefusedLoopbackPort(port int) bool {
	return port == 5432 || port == 6432 || (6379 <= port && port <= 6427)
}

// Why an authority is refused: a refused loopback port, or an endpoint that is
// not one of the private stack's.
func fs55AuthorityRefusal(authority string, private []string) error {
	host, portStr, err := net.SplitHostPort(authority)
	if err != nil {
		return fmt.Errorf("%q is not host:port", authority)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("%q has no port", authority)
	}
	if address, err := netip.ParseAddr(host); (err == nil && address.IsLoopback()) || host == "localhost" {
		if fs55RefusedLoopbackPort(port) {
			return fmt.Errorf("%s is a loopback port that can be a production data tunnel", authority)
		}
	}
	if !slices.Contains(private, authority) {
		return fmt.Errorf("%s is not one of the private stack's endpoints %v", authority, private)
	}
	return nil
}

// What a vault's pg.yml and redis.yml say, as far as the guard reads them.
type fs55VaultEndpoints struct {
	Authority string `yaml:"authority"`
	Host      string `yaml:"host"`
	Port      int    `yaml:"port"`
}

// The authorities a vault home's local resources name, read without keeping a
// credential.
func fs55VaultAuthorities(vaultHome string) ([]string, error) {
	authorities := []string{}
	for _, name := range []string{"pg.yml", "pg_maintenance.yml", "redis.yml"} {
		data, err := os.ReadFile(filepath.Join(vaultHome, "local", name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		endpoints := fs55VaultEndpoints{}
		if err := yaml.Unmarshal(data, &endpoints); err != nil {
			return nil, fmt.Errorf("%s does not parse", name)
		}
		switch {
		case endpoints.Authority != "":
			authorities = append(authorities, endpoints.Authority)
		case endpoints.Host != "" && endpoints.Port != 0:
			authorities = append(authorities, net.JoinHostPort(endpoints.Host, strconv.Itoa(endpoints.Port)))
		default:
			return nil, fmt.Errorf("%s names no endpoint the guard can read", name)
		}
	}
	if len(authorities) == 0 {
		return nil, errors.New("the vault home names no database or Redis")
	}
	return authorities, nil
}

// The guard FS-55 runs before any build or dial: test-env.sh's attestation is
// there, names only private endpoints and no refused port, and the vault the
// binaries will read names only attested endpoints.
func checkFs55Environment(environ []string, vaultHome string) error {
	lookup := func(name string) string {
		for _, variable := range environ {
			if value, ok := strings.CutPrefix(variable, name+"="); ok {
				return value
			}
		}
		return ""
	}
	private := []string{}
	for _, name := range []string{"WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY", "WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY"} {
		if value := lookup(name); value != "" {
			private = append(private, value)
		}
	}
	if len(private) != 2 {
		return errors.New("the private stack's authorities are not set: source .stack/env.sh, or run through .stack/server-test.sh")
	}
	attested := strings.FieldsFunc(lookup("WARP_TEST_ENV_ATTESTED_ADDRS"), func(r rune) bool { return r == ',' || r == ' ' })
	if len(attested) == 0 {
		return errors.New("no attestation: source server/test-env.sh first, as .stack/server-test.sh does")
	}
	for _, authority := range attested {
		if err := fs55AuthorityRefusal(authority, private); err != nil {
			return fmt.Errorf("the attestation: %w", err)
		}
	}
	if vaultHome == "" {
		return nil
	}
	authorities, err := fs55VaultAuthorities(vaultHome)
	if err != nil {
		return fmt.Errorf("the vault home: %w", err)
	}
	for _, authority := range authorities {
		if err := fs55AuthorityRefusal(authority, private); err != nil {
			return fmt.Errorf("the vault home: %w", err)
		}
	}
	return nil
}

// What a run on built server binaries needs.
type fs55ServerOptions struct {
	serverDirectory string
	base            string
	groupName       string
	vaultHome       string
	configHome      string
	extraConfigHome string
	workDirectory   string
	settings        *fs55Settings
	// what plans the query shapes on the scratch database; nil plans none
	explain fs55Explainer
}

// The pending groups a tree holds, in order, read from its pending file.
func fs55PendingGroups(serverDirectory string, ref string) ([]string, error) {
	output, err := gitShow(serverDirectory, ref, "db_migrations_sidecar_pending.go")
	if err != nil {
		return nil, err
	}
	groups := []string{}
	for _, match := range regexp.MustCompile(`(?m)^func sidecarMg([0-9])Migrations\(\) \[\]any`).FindAllStringSubmatch(output, -1) {
		groups = append(groups, "MG"+match[1])
	}
	slices.Sort(groups)
	return groups, nil
}

// One file at a ref.
func gitShow(serverDirectory string, ref string, path string) (string, error) {
	command := exec.Command("git", "show", ref+":"+path)
	command.Dir = serverDirectory
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git show %s:%s: %w", ref, path, err)
	}
	return string(output), nil
}

// The gate N+1 runs with in phase 4, and the group's own routes, for each group.
// A group none of whose routes a smoke can ask without a credential yet has
// none here, which phase 4's report says.
func fs55GroupPlan(groupName string) (gateYaml string, groupRoutes []fs55Route) {
	switch groupName {
	case "MG0", "MG1":
		// §7.4's stage 1 and D-TEAM-14, which MG0's and MG1's code serves
		return "sidecar:\n  p0_enforce: {enabled: true}\n", nil
	case "MG2":
		return "sidecar:\n  p0_enforce: {enabled: true}\n  evs: {enabled: true, percent: 100}\n  gkm: {enabled: true, percent: 100}\n", nil
	case "MG5":
		return "sidecar:\n  p0_enforce: {enabled: true}\n  dns: {enabled: true}\n  sockets: {enabled: true, percent: 100}\n  tlsedge: {enabled: true, percent: 100}\n", nil
	case "MG6":
		return "sidecar:\n  p0_enforce: {enabled: true}\n  bridge: {enabled: true}\n", nil
	case "MG7":
		return "sidecar:\n  p0_enforce: {enabled: true}\n  mxedge: {enabled: true, percent: 100}\n", nil
	case "MG8":
		return "sidecar:\n  p0_enforce: {enabled: true}\n  push: {enabled: true}\n  wake: {enabled: true}\n", nil
	case "MG9":
		return "sidecar:\n  p0_enforce: {enabled: true}\n  smtpedge: {enabled: true, percent: 100}\n", nil
	}
	return "", nil
}

// Builds N and N+1 from options.base and runs the four phases on them.
func runFs55OnServerBinaries(ctx context.Context, options *fs55ServerOptions) (*fs55Report, error) {
	workDirectory := options.workDirectory
	if workDirectory == "" {
		temporary, err := os.MkdirTemp("", "fs55-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(temporary)
		workDirectory = temporary
	}
	serverDirectory, err := filepath.Abs(options.serverDirectory)
	if err != nil {
		return nil, err
	}
	// the homes go into links and child processes, so each is absolute
	homes := []*string{&options.vaultHome, &options.configHome, &options.extraConfigHome}
	for _, home := range homes {
		if *home == "" {
			continue
		}
		if *home, err = filepath.Abs(*home); err != nil {
			return nil, err
		}
	}
	pendingGroups, err := fs55PendingGroups(serverDirectory, options.base)
	if err != nil {
		return nil, err
	}
	promoteGroups, err := fs55GroupsThrough(options.groupName, pendingGroups)
	if err != nil {
		return nil, err
	}
	nBinaries, err := buildFs55Binaries(ctx, serverDirectory, workDirectory, "N", options.base, nil)
	if err != nil {
		return nil, err
	}
	n1Binaries, err := buildFs55Binaries(ctx, serverDirectory, workDirectory, "N+1", options.base, promoteGroups)
	if err != nil {
		return nil, err
	}
	explain := options.explain
	if explain == nil {
		explain = func(ctx context.Context, query string) error { return nil }
	}
	gateYaml, groupRoutes := fs55GroupPlan(options.groupName)
	plan := &fs55Plan{
		groupName:   options.groupName,
		n:           &fs55BuiltRelease{name: "N", binaryDirectory: nBinaries, vaultHome: options.vaultHome, configHome: options.configHome, extraConfigHome: options.extraConfigHome, workDirectory: workDirectory, settings: options.settings},
		n1:          &fs55BuiltRelease{name: "N+1", binaryDirectory: n1Binaries, vaultHome: options.vaultHome, configHome: options.configHome, extraConfigHome: options.extraConfigHome, workDirectory: workDirectory, settings: options.settings},
		routes:      apiSmokeRoutes(),
		groupRoutes: groupRoutes,
		gateYaml:    gateYaml,
		explain:     explain,
		settings:    options.settings,
	}
	return runFs55(ctx, plan), nil
}
