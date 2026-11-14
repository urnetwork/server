// A synchronous harmless filter tripwire proves the configured execution path
// is live, then proves untrusted local source cannot reach it. No sleeps/root
// ownership changes, remote services or candidate script execution are needed.
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
)

// Global drivers are ignored; local drivers are an explicit qualification
// refusal. Neither case executes the candidate-selected attribute driver.
func TestSourceLocalGitCannotExecuteConfiguredAttributeFilters(t *testing.T) {
	globalConfig, hadGlobalConfig := os.LookupEnv("GIT_CONFIG_GLOBAL")
	for _, localDriver := range []bool{false, true} {
		// t.Setenv restores only at whole-test cleanup, not loop boundaries.
		// Fixture commits need the original configured identity before each case.
		t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
		if !hadGlobalConfig {
			if err := os.Unsetenv("GIT_CONFIG_GLOBAL"); err != nil {
				t.Fatal(err)
			}
		}
		root := t.TempDir()
		sourceTestRepository(t, root, "control")
		base := sourceTestRepository(t, root, "connect")
		control, target := filepath.Join(root, "control"), filepath.Join(root, "connect")
		marker := filepath.Join(root, "filter-called")
		program := filepath.Join(root, "filter")
		if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf 'filter-executed\\n' > \"$SOURCE_FILTER_TEST_MARKER\"\ncat\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		global := filepath.Join(root, "global-gitconfig")
		for _, key := range []string{"filter.synthetic.clean", "filter.synthetic.smudge"} {
			sourceTestGit(t, control, "config", "--file", global, key, program)
			if localDriver {
				sourceTestGit(t, target, "config", key, program)
			}
		}
		t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
		t.Setenv("GIT_CONFIG_GLOBAL", global)
		t.Setenv("SOURCE_FILTER_TEST_MARKER", marker)
		if err := os.WriteFile(filepath.Join(control, ".gitattributes"), []byte("source.txt filter=synthetic\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(control, "source.txt"), []byte("control content forces clean-filter conversion\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Native Git must invoke this exact program before its command completes.
		sourceTestGit(t, control, "add", "--", "source.txt")
		if content, err := os.ReadFile(marker); err != nil || string(content) != "filter-executed\n" {
			t.Fatal("configured filter tripwire was not actually armed", err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(target, ".git", "config")
		configBefore, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		raw := "diff --git a/connect/.gitattributes b/connect/.gitattributes\nnew file mode 100644\n--- /dev/null\n+++ b/connect/.gitattributes\n@@ -0,0 +1 @@\n+source.txt filter=synthetic\n" +
			"diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-connect\n+changed\n"
		policy := controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
		patch, patchError := controller.ValidateAndCanonicalizePatch(raw, policy)
		if patchError != nil {
			t.Fatal(patchError)
		}
		err = validateSourcePatchBase(context.Background(), target, base, "connect", patch, policy)
		if localDriver {
			if err == nil || !strings.Contains(err.Error(), "filter configuration") {
				t.Fatal("local filter driver did not stop before source application", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			routed, patchError := controller.SplitCompetitionRepositoryPatch(raw, policy)
			if patchError != nil {
				t.Fatal(patchError)
			}
			patchPath := filepath.Join(root, "canonical.patch")
			if err := os.WriteFile(patchPath, routed["connect"], 0o400); err != nil {
				t.Fatal(err)
			}
			if _, err := sourceGitText(context.Background(), target, "apply", "--index", "--whitespace=error-all", patchPath); err != nil {
				t.Fatal(err)
			}
			if _, err := sourceGitText(context.Background(), target, "-c", "user.name=Synthetic Source", "-c", "user.email=source@account.example", "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic untrusted attributes"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Lstat(marker); !os.IsNotExist(err) {
			t.Fatal("untrusted local attributes executed a configured filter", err)
		}
		configAfter, err := os.ReadFile(configPath)
		if err != nil || !bytes.Equal(configBefore, configAfter) {
			t.Fatal("local execution policy mutated protected repository configuration", err)
		}
	}
}

// Authentication is an explicit transport capability, never an inferred argv
// feature or a side effect of local Git configuration.
func TestSourceGitSeparatesLocalAndTransportConfiguration(t *testing.T) {
	root := t.TempDir()
	sourceTestRepository(t, root, "server")
	repository := filepath.Join(root, "server")
	t.Setenv("GIT_SSH_COMMAND", "synthetic-transport-credential-helper")
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(root, "synthetic-agent"))
	local, err := sourceGitLocalCommand(context.Background(), repository, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	localEnvironment := strings.Join(local.Env, "\n")
	if strings.Contains(localEnvironment, "GIT_SSH_COMMAND=") || strings.Contains(localEnvironment, "SSH_AUTH_SOCK=") || !strings.Contains(localEnvironment, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Fatal("local source command retained transport authority")
	}
	transport, err := sourceGitCommand(context.Background(), repository, "ls-remote", "origin")
	if err != nil || !strings.Contains(strings.Join(transport.Env, "\n"), "GIT_SSH_COMMAND=synthetic-transport-credential-helper") {
		t.Fatal("explicit transport construction lost its configured authentication", err)
	}
}
