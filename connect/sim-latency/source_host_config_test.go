// Prospective host qualification must not accept operator-writable aliases or
// replace active configuration. Synthetic stat results test path policy only.
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Extract the actual shell validator before any live host probes can execute.
// Mock ownership/mode reads; real file type, symlink and canonical checks remain.
func sourceHostConfigValidatorFixture(t *testing.T) (string, string) {
	t.Helper()
	content, err := os.ReadFile("evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, function, found := strings.Cut(string(content), "secure_host_path() {")
	if !found {
		t.Fatal("host configuration validator is absent")
	}
	function, _, found = strings.Cut(function, "# --- host configuration path helper ends ---")
	if !found {
		t.Fatal("host configuration validator boundary is absent")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	stat := `#!/bin/bash
set -eu
field="$2"
path="$4"
case "$field" in
  %u)
    if test "$path" = "${SYNTHETIC_BAD_OWNER:-}"; then printf '1000\n'; else printf '0\n'; fi
    ;;
  %a)
    if test "$path" = "${SYNTHETIC_BAD_MODE:-}"; then printf '0777\n'; else printf '0755\n'; fi
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(stat), 0o700); err != nil {
		t.Fatal(err)
	}
	return "secure_host_path() {" + function + "\nsecure_host_config_path \"$1\"", bin
}

// A genuinely canonical regular file is accepted when every ownership and
// mode observation is protected. This is not a hardware/root attestation.
func TestSourceHostConfigValidatorAcceptsProtectedCanonicalRegularFile(t *testing.T) {
	script, bin := sourceHostConfigValidatorFixture(t)
	path := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", script, "fixture", path)
	command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("protected synthetic path rejected", err, string(output))
	}
}

// Changing a single leaf/ancestor owner or mode must revoke path authority.
func TestSourceHostConfigValidatorRejectsWritableOrNonRootAncestors(t *testing.T) {
	script, bin := sourceHostConfigValidatorFixture(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "host.json")
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, variable := range []string{"SYNTHETIC_BAD_OWNER", "SYNTHETIC_BAD_MODE"} {
		for _, unsafe := range []string{path, parent, filepath.Dir(parent)} {
			command := exec.Command("bash", "-c", script, "fixture", path)
			command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", variable + "=" + unsafe}
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatal("unprotected leaf/ancestor accepted", variable, unsafe, string(output))
			}
		}
	}
}

// Aliases cannot redirect a candidate profile into active configuration or a
// different private tree, even when stat would claim protected ownership.
func TestSourceHostConfigValidatorRejectsAliasesDirectoriesAndRelativePaths(t *testing.T) {
	script, bin := sourceHostConfigValidatorFixture(t)
	root := t.TempDir()
	path := filepath.Join(root, "host.json")
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	parentAlias := filepath.Join(t.TempDir(), "parent-alias")
	if err := os.Symlink(root, parentAlias); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"", "relative.json", "/", root, alias, filepath.Join(parentAlias, "host.json"), root + "/../" + filepath.Base(root) + "/host.json", path + "\n"} {
		command := exec.Command("bash", "-c", script, "fixture", unsafe)
		command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatal("unsafe alias/type/path accepted", unsafe, string(output))
		}
	}
}

// Artifact/cache claims require directories, not a protected marker file.
func TestSourceHostPathValidatorRequiresExactDirectoryType(t *testing.T) {
	script, bin := sourceHostConfigValidatorFixture(t)
	script = strings.TrimSuffix(script, "\nsecure_host_config_path \"$1\"") + "\nsecure_host_path \"$1\" directory"
	directory := t.TempDir()
	file := filepath.Join(directory, "not-a-directory")
	if err := os.WriteFile(file, []byte("synthetic-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		Path string
		Pass bool
	}{{Path: directory, Pass: true}, {Path: file, Pass: false}} {
		command := exec.Command("bash", "-c", script, "fixture", c.Path)
		command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		if output, err := command.CombinedOutput(); (err == nil) != c.Pass {
			t.Fatal("host path accepted the wrong resource type", c.Path, err, string(output))
		}
	}
}

// Execute the real marker helper and use a hash-command tripwire to prove
// untrusted ancestors are rejected before opening even a known-digest file.
func TestSourceHostMarkerRejectsUnsafeParentBeforeReadingEvidence(t *testing.T) {
	script, bin := sourceHostConfigValidatorFixture(t)
	content, err := os.ReadFile("evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(content), "hash_marker() {")
	if !ok {
		t.Fatal("host marker helper is absent")
	}
	body, _, ok = strings.Cut(body, "\n}\n")
	if !ok {
		t.Fatal("host marker helper boundary is absent")
	}
	script = strings.TrimSuffix(script, "\nsecure_host_config_path \"$1\"") + "\nhash_marker() {" + body + "\n}\nhash_marker \"$1\" \"$2\""
	root := t.TempDir()
	path, tripwire := filepath.Join(root, "marker.json"), filepath.Join(root, "hash-read")
	marker := []byte(`{"synthetic_evidence":true}`)
	if err := os.WriteFile(path, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	hashProgram := "#!/bin/bash\nprintf 'read\\n' > \"$SYNTHETIC_HASH_TRIPWIRE\"\nexec /usr/bin/sha256sum \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "sha256sum"), []byte(hashProgram), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", script, "fixture", path, fmt.Sprintf("%x", sha256.Sum256(marker)))
	command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "SYNTHETIC_BAD_OWNER=" + root, "SYNTHETIC_HASH_TRIPWIRE=" + tripwire}
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatal("unsafe marker parent passed hash authentication", string(output))
	}
	if _, err := os.Lstat(tripwire); !os.IsNotExist(err) {
		t.Fatal("unsafe marker was read before its path authority was checked", err)
	}
	// A positive control proves the same extracted helper really opens and
	// authenticates protected evidence, rather than rejecting every marker.
	command = exec.Command("bash", "-c", script, "fixture", path, fmt.Sprintf("%x", sha256.Sum256(marker)))
	command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "SYNTHETIC_HASH_TRIPWIRE=" + tripwire}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("protected marker failed hash authentication", err, string(output))
	}
	if content, err := os.ReadFile(tripwire); err != nil || string(content) != "read\n" {
		t.Fatal("positive marker control did not read the evidence", err)
	}
	command = exec.Command("bash", "-c", script, "fixture", path, strings.Repeat("0", 64))
	command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "SYNTHETIC_HASH_TRIPWIRE=" + tripwire}
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatal("protected marker with the wrong digest passed authentication", string(output))
	}
}

// Exercise the actual argument parser without reaching hardware probes. A
// synthetic id command tests authorization branching, not privileged execution.
func TestSourceHostAlternateConfigRequiresRootAndExactArguments(t *testing.T) {
	_, bin := sourceHostConfigValidatorFixture(t)
	content, err := os.ReadFile("evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	parser, _, ok := strings.Cut(string(content), "command -v jq")
	if !ok {
		t.Fatal("host checker argument parser boundary is absent")
	}
	_, parser, ok = strings.Cut(parser, "secure_host_path() {")
	if !ok {
		t.Fatal("host checker protected-path helper is absent")
	}
	parser = "set -uo pipefail\nsecure_host_path() {" + parser
	path := filepath.Join(t.TempDir(), "prospective.json")
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	id := "#!/bin/bash\n[ \"$#\" -eq 1 ] && [ \"$1\" = -u ] || exit 2\nprintf '%s\\n' \"$SYNTHETIC_CALLER_UID\"\n"
	if err := os.WriteFile(filepath.Join(bin, "id"), []byte(id), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		Args []string
		Uid  string
		Pass bool
	}{
		{Args: []string{"--json"}, Uid: "1000", Pass: true},
		{Args: []string{"--json", "--config", path}, Uid: "0", Pass: true},
		{Args: []string{"--json", "--config", path}, Uid: "1000", Pass: false},
		{Args: []string{"--json", "--config"}, Uid: "0", Pass: false},
		{Args: []string{"--config", path, "--json"}, Uid: "0", Pass: false},
		{Args: []string{"--json", "--config", path, "extra"}, Uid: "0", Pass: false},
		{Args: []string{"--json", "--config", "relative.json"}, Uid: "0", Pass: false},
		{Args: nil, Uid: "0", Pass: false},
	} {
		args := append([]string{"-c", parser, "fixture"}, c.Args...)
		command := exec.Command("bash", args...)
		command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "SYNTHETIC_CALLER_UID=" + c.Uid}
		if output, err := command.CombinedOutput(); (err == nil) != c.Pass {
			t.Fatal("host checker argument authorization mismatch", c.Args, c.Uid, err, string(output))
		}
	}
}
