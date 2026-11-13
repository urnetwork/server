// The shipped preparation launcher is exercised with a synthetic immutable
// owner/root and harmless child; no privileged program or real configuration runs.
package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every authority substitution changes a private copy's fixed definitions,
// never an environment-controlled escape in the installed root-only launcher.
type competitionPreparationLauncherFixture struct {
	root          string
	launcher      string
	native        string
	config        string
	qualification string
	observed      string
	version       string
	image         string
}

// The exact launcher body retains its ancestry, link, mode and hash checks;
// only its fixed synthetic owner/root and deployment identities are substituted.
func newCompetitionPreparationLauncherFixture(t *testing.T) competitionPreparationLauncherFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := competitionPreparationLauncherFixture{root: root, launcher: filepath.Join(root, "launcher.sh"), native: filepath.Join(root, "native.sh"),
		config: filepath.Join(root, "preparation.json"), qualification: filepath.Join(root, "qualification.json"), observed: filepath.Join(root, "observed"),
		version: strings.Repeat("a", 40), image: "sha256:" + strings.Repeat("b", 64)}
	if err := os.Mkdir(fixture.observed, 0700); err != nil {
		t.Fatal(err)
	}
	child := []byte("#!/bin/sh\nprintf '%s\\n' \"$WARP_ENV\" \"$WARP_HOST\" \"$WARP_HOME\" \"$WARP_VERSION\" \"$WARP_IMAGE_DIGEST\" \"$SIM_LATENCY_PREPARATION_CONFIG\" \"$SIM_LATENCY_QUALIFICATION_CONFIG\" \"${URNETWORK_TEST_REAL_VAULT-unset}\" >\"$SYNTHETIC_OBSERVED/environment\"\nprintf '%s\\n' \"$@\" >\"$SYNTHETIC_OBSERVED/arguments\"\n/bin/cat >\"$SYNTHETIC_OBSERVED/input\"\n")
	for path, content := range map[string][]byte{fixture.native: child, fixture.config: []byte("{}\n"), fixture.qualification: []byte("{}\n")} {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0400)
		if path == fixture.native {
			mode = 0555
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "launch", "competitionprepare-fixed-worker-context.sh.template"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(child)
	definitions := map[string]string{"REQUIRED_UID": strconv.Itoa(os.Geteuid()), "TRUSTED_ROOT": root, "NATIVE": fixture.native,
		"NATIVE_SHA": hex.EncodeToString(digest[:]), "CONFIG": fixture.config, "QUALIFICATION_CONFIG": fixture.qualification,
		"CONTROL_SOURCE": fixture.version, "WORKER_IMAGE": fixture.image, "RUNTIME_ENVIRONMENT": "synthetic", "RUNTIME_HOST": "synthetic-host", "RUNTIME_HOME": root}
	lines := strings.Split(string(script), "\n")
	for key, value := range definitions {
		prefix, replaced := "readonly "+key+"=", 0
		for index, line := range lines {
			if strings.HasPrefix(line, prefix) {
				lines[index] = prefix + "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
				replaced++
			}
		}
		if replaced != 1 {
			t.Fatalf("fixed synthetic definition %s occurs %d times", key, replaced)
		}
	}
	if err := os.WriteFile(fixture.launcher, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.launcher, 0555); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// Parent provenance is intentionally wrong: fixed worker context must replace
// it only in the preparation child, without altering the parent's environment.
func (self competitionPreparationLauncherFixture) command(arguments ...string) *exec.Cmd {
	command := exec.Command("/usr/bin/bash", append([]string{self.launcher}, arguments...)...)
	command.Env = []string{"PATH=/usr/bin:/bin", "SYNTHETIC_OBSERVED=" + self.observed, "WARP_ENV=wrong", "WARP_HOST=wrong", "WARP_HOME=/synthetic/wrong",
		"WARP_VERSION=wrong-source-supervisor-version", "WARP_IMAGE_DIGEST=sha256:" + strings.Repeat("f", 64), "SIM_LATENCY_PREPARATION_CONFIG=/synthetic/wrong",
		"SIM_LATENCY_QUALIFICATION_CONFIG=/synthetic/wrong", "URNETWORK_TEST_REAL_VAULT=1"}
	return command
}

// Exact stdin and the single fixed --config argument survive exec; target
// worker release metadata cannot leak backwards into source-supervisor context.
func TestCompetitionPreparationLauncherPinsChildWorkerContext(t *testing.T) {
	fixture := newCompetitionPreparationLauncherFixture(t)
	input := []byte("{\"operation\":\"synthetic-observation\"}")
	command := fixture.command()
	command.Stdin = bytes.NewReader(input)
	before := append([]string(nil), command.Env...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("trusted context launcher failed: %v %s", err, output)
	}
	expected := strings.Join([]string{"synthetic", "synthetic-host", fixture.root, fixture.version, fixture.image, fixture.config, fixture.qualification, "unset", ""}, "\n")
	observed, err := os.ReadFile(filepath.Join(fixture.observed, "environment"))
	if err != nil || string(observed) != expected {
		t.Fatalf("child inherited another release context: %q %v", observed, err)
	}
	observed, err = os.ReadFile(filepath.Join(fixture.observed, "arguments"))
	if err != nil || string(observed) != "--config="+fixture.config+"\n" {
		t.Fatal("launcher did not select its fixed native config", err)
	}
	observed, err = os.ReadFile(filepath.Join(fixture.observed, "input"))
	if err != nil || !bytes.Equal(observed, input) || strings.Join(command.Env, "\n") != strings.Join(before, "\n") {
		t.Fatal("request pipe or parent context changed", err)
	}
}

// Supervisor input cannot select a command/config path or override fixed flags.
func TestCompetitionPreparationLauncherRejectsArguments(t *testing.T) {
	fixture := newCompetitionPreparationLauncherFixture(t)
	output, err := fixture.command("--config=/synthetic/override").CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 2 || len(output) != 0 {
		t.Fatalf("caller config override accepted: %v %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(fixture.observed, "environment")); !os.IsNotExist(err) {
		t.Fatal("refused launcher still executed native child", err)
	}
}

// The same fixed body refuses unsafe modes, wrong bytes and linked inventory
// before the child can read main credentials or perform any external action.
func TestCompetitionPreparationLauncherRejectsUnsafeInputs(t *testing.T) {
	for _, failure := range []string{"native-mode", "config-mode", "qualification-link", "native-hash"} {
		fixture := newCompetitionPreparationLauncherFixture(t)
		var err error
		switch failure {
		case "native-mode":
			err = os.Chmod(fixture.native, 0755)
		case "config-mode":
			err = os.Chmod(fixture.config, 0600)
		case "qualification-link":
			target := filepath.Join(fixture.root, "other.json")
			if err = os.Rename(fixture.qualification, target); err == nil {
				err = os.Symlink(target, fixture.qualification)
			}
		case "native-hash":
			if err = os.Chmod(fixture.native, 0600); err == nil {
				err = os.WriteFile(fixture.native, []byte("#!/bin/sh\nexit 0\n"), 0600)
			}
			if err == nil {
				err = os.Chmod(fixture.native, 0555)
			}
		}
		if err != nil {
			t.Fatal(failure, err)
		}
		output, err := fixture.command().CombinedOutput()
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 2 || len(output) != 0 {
			t.Fatalf("%s: unsafe input accepted: %v %s", failure, err, output)
		}
		if _, err := os.Stat(filepath.Join(fixture.observed, "environment")); !os.IsNotExist(err) {
			t.Fatal(failure, "executed native child", err)
		}
	}
}
