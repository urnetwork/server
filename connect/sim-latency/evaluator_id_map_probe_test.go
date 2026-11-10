// Real helper argv is observed through inert test-private Docker clients. These
// tests never contact a daemon, run sudo, or claim native qualification.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Both scoped and legacy routing terminate at the same inert argument recorder.
// A deliberate create failure stops before any namespace/PID operation.
func idMapProbeFixture(t *testing.T, scoped bool, uid, gid string) ([]string, []byte, error) {
	t.Helper()
	root := t.TempDir()
	image := "sha256:" + strings.Repeat("3", 64)
	client := filepath.Join(root, "client")
	body := `#!/bin/bash
set -eu
printf 'called\n' >> "$PROBE_TEST_CALLS"
case "$4" in
image) printf '%s\n' "$PROBE_TEST_IMAGE" ;;
run) printf '%s\000' "$@" > "$PROBE_TEST_ARGUMENTS"; exit 73 ;;
rm) exit 0 ;;
*) exit 99 ;;
esac
`
	if err := os.WriteFile(client, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"docker": "#!/bin/bash\nexit 99\n",
		"sudo":   "#!/bin/bash\nset -eu\n[[ $1 == -n && $2 == docker ]]\nshift 2\nexec \"$PROBE_TEST_CLIENT\" --docker-client synthetic -- \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", filepath.Join("evaluator", "container", "docker-id-map.sh"), "--image", image, "--uid", uid, "--gid", gid)
	command.Env = []string{"PATH=" + root + ":/usr/bin:/bin", "LANG=C", "LC_ALL=C", "PROBE_TEST_CLIENT=" + client, "PROBE_TEST_IMAGE=" + image, "PROBE_TEST_ARGUMENTS=" + filepath.Join(root, "args"), "PROBE_TEST_CALLS=" + filepath.Join(root, "calls")}
	if scoped {
		command.Env = append(command.Env, subevaluationOwnershipEnvironment()...)
		command.Env = append(command.Env, "URNETWORK_SUBEVALUATION_CLIENT="+client, "URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET=6,8")
	}
	output, err := command.CombinedOutput()
	arguments, _ := os.ReadFile(filepath.Join(root, "args"))
	if len(arguments) == 0 {
		if calls, _ := os.ReadFile(filepath.Join(root, "calls")); len(calls) != 0 {
			t.Fatal("invalid private identity reached a Docker operation", string(calls))
		}
		return nil, output, err
	}
	return strings.Split(strings.TrimSuffix(string(arguments), "\x00"), "\x00"), output, err
}

// The unchanged proxy refuses arbitrary root containers. The actual private
// namespace probe must select its existing permitted UID, not seek an exception.
func TestPrivateIdMapProbeUsesPermittedNonRootIdentity(t *testing.T) {
	arguments, output, err := idMapProbeFixture(t, true, "65532", "65532")
	if err == nil || !strings.Contains(string(output), "could not start the namespace-map probe") {
		t.Fatal("inert create counterfactual did not terminate the actual helper", err, string(output))
	}
	for index, argument := range arguments {
		if argument == "--user" {
			if index+1 >= len(arguments) || arguments[index+1] != "65532:65532" {
				t.Fatal("private namespace probe violates its own proxy role", arguments)
			}
			return
		}
	}
	t.Fatal("private namespace probe omitted its explicit user", arguments)
}

// Legacy baseline-producer metadata retains its previous argument bytes.
func TestLegacyIdMapProbeRetainsRootIdentity(t *testing.T) {
	arguments, _, err := idMapProbeFixture(t, false, "65532", "65532")
	if err == nil {
		t.Fatal("inert create unexpectedly succeeded")
	}
	for index, argument := range arguments {
		if argument == "--user" {
			if index+1 >= len(arguments) || arguments[index+1] != "0:0" {
				t.Fatal("legacy probe identity changed", arguments)
			}
			return
		}
	}
	t.Fatal("legacy probe user missing", arguments)
}

// Reject unsupported translation identities before inspection or create, not
// after allocating a privileged-helper container with unusable requested maps.
func TestPrivateIdMapProbeRejectsDifferentIdentityBeforeDocker(t *testing.T) {
	for _, identity := range []struct{ uid, gid string }{{uid: "0", gid: "0"}, {uid: "65532", gid: "999"}} {
		arguments, output, err := idMapProbeFixture(t, true, identity.uid, identity.gid)
		if err == nil || arguments != nil || !strings.Contains(string(output), "private role only translates its frozen container identity") {
			t.Fatal("unsupported private identity reached Docker", arguments, err, string(output))
		}
	}
}
