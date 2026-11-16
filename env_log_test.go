package server

// A vault string's startup log line names the resource and the key, and never
// carries a character of the value. The log is observed in a child process
// with the default glog setup (stderr, as bringyourctl and every service log),
// so no global logger is replaced; every value is synthetic.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/urnetwork/glog"
)

const envLogTestChildMode = "URNETWORK_TEST_ENV_LOG_CHILD"

// Synthetic credentials whose first and last two characters (what a mask
// shows) appear nowhere else in a glog line.
var envLogTestValues = []struct {
	key   string
	value string
}{
	{key: "user", value: "Jq-synthetic-user-Vx"},
	{key: "password", value: "Zk-synthetic-password-Wy"},
	{key: "authority", value: "Qh-synthetic.example-Xb"},
}

func envLogTestOutput(t *testing.T) []byte {
	t.Helper()
	home := t.TempDir()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRequireStringLogChild$", "-test.count=1", "-test.v")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case envLogTestChildMode, "WARP_HOME", "WARP_VAULT_HOME", "WARP_CONFIG_HOME", "WARP_ENV",
			"BY_LOG_LOGTOSTDERR", "BY_LOG_STDERRTHRESHOLD", "BY_LOG_V":
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env,
		envLogTestChildMode+"=1",
		"WARP_HOME="+home,
		"WARP_VAULT_HOME="+home,
		"WARP_CONFIG_HOME="+home,
		"WARP_ENV=",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("startup log child failed: %v", err)
	}
	start, end := []byte("synthetic-env-log-capture-start\n"), []byte("synthetic-env-log-capture-end\n")
	begin, finish := bytes.Index(output, start), bytes.Index(output, end)
	if begin < 0 || finish < begin+len(start) || bytes.Count(output, start) != 1 || bytes.Count(output, end) != 1 {
		t.Fatal("startup log child did not finish its exact synthetic capture window")
	}
	return output[begin+len(start) : finish]
}

// pg.yml's user, password and authority are logged by name only: neither the
// value nor its masked first and last two characters reach the log.
func TestRequireStringLogsNoValue(t *testing.T) {
	window := string(envLogTestOutput(t))
	for _, entry := range envLogTestValues {
		if !strings.Contains(window, "pg.yml["+entry.key+"]") {
			t.Errorf("the log does not name pg.yml[%s]:\n%s", entry.key, window)
		}
		for _, fragment := range []string{entry.value, entry.value[:2], entry.value[len(entry.value)-2:]} {
			if strings.Contains(window, fragment) {
				t.Errorf("the log carries characters of pg.yml[%s]: %q", entry.key, fragment)
			}
		}
	}
}

func TestRequireStringLogChild(t *testing.T) {
	if os.Getenv(envLogTestChildMode) == "" {
		t.Skip("subprocess-only synthetic startup log observation")
	}
	pg := strings.Builder{}
	for _, entry := range envLogTestValues {
		fmt.Fprintf(&pg, "%s: %q\n", entry.key, entry.value)
	}
	t.Cleanup(Vault.PushSimpleResource("pg.yml", []byte(pg.String())))
	keys := Vault.RequireSimpleResource("pg.yml")
	glog.Flush()
	fmt.Fprintln(os.Stderr, "synthetic-env-log-capture-start")
	for _, entry := range envLogTestValues {
		if value := keys.RequireString(entry.key); value != entry.value {
			t.Fatalf("pg.yml[%s] read a different value", entry.key)
		}
	}
	glog.Flush()
	fmt.Fprintln(os.Stderr, "synthetic-env-log-capture-end")
}
