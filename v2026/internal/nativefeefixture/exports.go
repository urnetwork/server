// Package nativefeefixture gives the server's public native-fee tests their
// settlement exports. The four TestStNativeFeeSettlementPublic* in ./model and
// TestNativeFeeControllerPublicProofSettlesWithoutSignerConfiguration and
// TestNativeFeeControllerPublicUnknownRefundKeepsOriginalCeiling in
// ./controller settle the fixtures that sn's mainnet.test exports, with that
// same image, which they pin by path and SHA-256. The sn release gate builds
// both (sn/scripts/build-mainnet-exports.sh) and runs the tests with its three
// URNETWORK_NATIVE_FEE_SETTLEMENT_* exports set. A run that sets none of them
// gets the same exports from Require, which builds them the gate's way from the
// sn checkout that the server builds against. It is imported only by tests.
package nativefeefixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The gate's exports that the public tests read: the exporter's output
// directory, its mainnet.test image, and that image's SHA-256.
const (
	fixtureDirectoryVariable = "URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY"
	verifierVariable         = "URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER"
	verifierSha256Variable   = "URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER_SHA256"
)

var gateVariables = []string{fixtureDirectoryVariable, verifierVariable, verifierSha256Variable}

// The module whose mainnet package exports the fixtures; the server's go.mod
// replaces it with ../sn.
const snModulePath = "github.com/urfoundation/sn/v2026"

// Leaves this much of the test binary's deadline for the test to report a
// build that cannot finish.
const deadlineMargin = 30 * time.Second

// Every refusal says what the tests need, why this run builds it, and what
// stopped it.
const refusalPrefix = "the public native-fee tests settle the sn release gate's exports " +
	fixtureDirectoryVariable + ", " + verifierVariable + " and " + verifierSha256Variable +
	". None is set, so they build them from the sibling sn checkout the way the gate does " +
	"(sn/scripts/build-mainnet-exports.sh), but "

// The way around any refusal: the gate's own exports.
const refusalAlternative = " Or set all three from a gate export directory: source its environment.sh."

// The values of the gate's three variables.
type Exports struct {
	FixtureDirectory string
	Verifier         string
	VerifierSha256   string
}

// Require gives the calling test the gate's exports. With any of the three
// variables set, even empty, it changes nothing: the gate selected the exports,
// or a caller reproduces it, and the test checks exactly what was selected. With
// none set it builds the exports once per test process and sets the three
// variables to them, as the gate's environment.sh does. A build that cannot
// happen fails the test with the reason; it never skips.
func Require(tb testing.TB) {
	tb.Helper()
	require(tb, os.LookupEnv, os.Setenv, provisionOnce)
}

func require(tb testing.TB, lookup func(string) (string, bool), set func(string, string) error, provide func(testing.TB) (Exports, error)) {
	tb.Helper()
	for _, name := range gateVariables {
		if _, selected := lookup(name); selected {
			return
		}
	}
	exports, err := provide(tb)
	if err != nil {
		tb.Fatal(err)
	}
	for _, variable := range []struct{ name, value string }{
		{fixtureDirectoryVariable, exports.FixtureDirectory},
		{verifierVariable, exports.Verifier},
		{verifierSha256Variable, exports.VerifierSha256},
	} {
		if err := set(variable.name, variable.value); err != nil {
			tb.Fatal(err)
		}
	}
}

// One build per test process, whose result every later test shares.
var provisioned struct {
	sync.Mutex
	done    bool
	exports Exports
	err     error
}

func provisionOnce(tb testing.TB) (Exports, error) {
	provisioned.Lock()
	defer provisioned.Unlock()
	if !provisioned.done {
		provisioned.exports, provisioned.err = provision(tb, runtime.GOOS, defaultSettings)
		provisioned.done = true
	}
	return provisioned.exports, provisioned.err
}

// What a build depends on outside the sn checkout.
type settings struct {
	goTool    string
	cacheRoot string
	environ   []string
	lock      func(path string, how int) (*os.File, error)
}

func defaultSettings() (settings, error) {
	goTool, err := defaultGoTool()
	if err != nil {
		return settings{}, err
	}
	root, err := defaultCacheRoot()
	if err != nil {
		return settings{}, err
	}
	return settings{goTool: goTool, cacheRoot: root, environ: os.Environ(), lock: lockFile}, nil
}

// The go command on PATH, which the gate's own recipe runs.
func defaultGoTool() (string, error) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("the go command is not on PATH: %w", err)
	}
	return goTool, nil
}

// Builds the exports on a goos host, or says why it cannot.
func provision(tb testing.TB, goos string, configure func() (settings, error)) (Exports, error) {
	if goos != "linux" {
		return Exports{}, nonLinuxRefusal(goos)
	}
	setup, err := configure()
	if err != nil {
		return Exports{}, errors.New(refusalPrefix + err.Error() + "." + refusalAlternative)
	}
	ctx, cancel := provisionContext(tb)
	defer cancel()
	return provide(ctx, setup, tb.Logf)
}

// sn's mainnet tests do not build off Linux, and nativefee.Invoke, which every
// public test settles through, refuses to run there (sn/nativefee/invoke_other.go).
func nonLinuxRefusal(goos string) error {
	return errors.New(refusalPrefix + "this host is " + goos + ": sn's mainnet.test verifier builds and runs only on Linux " +
		"(sn/nativefee needs memfd sealing, /proc/self/exe and an ELF image). Run these tests on Linux, where they build the exports themselves." +
		refusalAlternative)
}

// Bounds a build by the test binary's deadline, less a margin to report it. The
// exports outlive the first test that asks for them, so its own context does
// not bound them.
func provisionContext(tb testing.TB) (context.Context, context.CancelFunc) {
	if deadliner, ok := tb.(interface{ Deadline() (time.Time, bool) }); ok {
		if deadline, ok := deadliner.Deadline(); ok {
			return context.WithDeadline(context.Background(), deadline.Add(-deadlineMargin))
		}
	}
	return context.WithCancel(context.Background())
}

// Under the user's cache directory, or the temporary directory without one, as
// a physical path: the exporter pins the verifier by the path its own process
// sees, which has no symbolic link.
func defaultCacheRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return physicalDirectory(filepath.Join(base, "urnetwork-native-fee-settlement"))
}

func physicalDirectory(path string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("the exports' cache directory %s cannot be created: %w", path, err)
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("the exports' cache directory %s cannot be resolved: %w", path, err)
	}
	if !filepath.IsAbs(physical) {
		if physical, err = filepath.Abs(physical); err != nil {
			return "", err
		}
	}
	return filepath.Clean(physical), nil
}

// The sn module directory this server module builds against, which must hold
// sn's mainnet package. go list reports a replacement's directory whether or
// not it exists.
func resolveSn(ctx context.Context, setup settings) (string, error) {
	command := exec.CommandContext(ctx, setup.goTool, "list", "-m", "-f", "{{.Dir}}", snModulePath)
	command.Env = setup.environ
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	directory := strings.TrimSpace(string(output))
	if err != nil {
		return "", fmt.Errorf("go list -m %s failed: %w: %s", snModulePath, err, strings.TrimSpace(stderr.String()))
	}
	if directory == "" || !filepath.IsAbs(directory) {
		return "", fmt.Errorf("go list -m %s names no checkout directory (%q)", snModulePath, directory)
	}
	for _, required := range []string{"go.mod", "mainnet"} {
		if _, err := os.Stat(filepath.Join(directory, required)); err != nil {
			return "", fmt.Errorf("the %s checkout the server builds against, %s, has no %s: %w", snModulePath, directory, required, err)
		}
	}
	return directory, nil
}
