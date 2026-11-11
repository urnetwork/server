//go:build linux

// Only private daemon services may create traversable container mount parents.
// State/key directories keep their explicit private modes and other roles 0077.
package competitionagent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestRuntimeBoundaryDaemonUmaskIsRoleSpecific(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	for _, pin := range fixture.boundary.unitPins() {
		properties := runtimeBoundaryTestUnitProperties(fixture.boundary, pin)
		if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err != nil {
			t.Fatal("correct role umask rejected", err)
		}
		for _, wrong := range []string{"", "0000", "0002", "0777"} {
			properties["UMask"] = wrong
			if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err == nil {
				t.Fatal("unqualified umask accepted", wrong)
			}
		}
		name := filepath.Base(pin.Path)
		want := "0077"
		if name == runtimeBoundaryEngineUnitName || name == runtimeBoundaryContainerdUnitName {
			want = "0022"
		}
		other := "0022"
		if want == "0022" {
			other = "0077"
		}
		properties["UMask"] = other
		if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err == nil {
			t.Fatal("another role's umask acquired authority", name, other)
		}
		content, err := os.ReadFile(filepath.Join("..", "cli", "competitionsubeval", "private-engine", name+".in"))
		if err != nil || strings.Count(string(content), "UMask="+want+"\n") != 1 {
			t.Fatal("template differs from native role admission", name, err)
		}
	}
}

// Process-global umask changes are confined to a dedicated child test process.
func TestRuntimeBoundaryUmaskDirectoryChild(t *testing.T) {
	mode := os.Getenv("SYNTHETIC_RUNTIME_UMASK")
	if mode == "" {
		return
	}
	if mode != "0077" && mode != "0022" {
		os.Exit(2)
	}
	value, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		os.Exit(2)
	}
	syscall.Umask(int(value))
	path := filepath.Join(os.Getenv("SYNTHETIC_RUNTIME_DIRECTORY"), "runtime")
	if err := os.Mkdir(path, 0755); err != nil {
		os.Exit(2)
	}
	info, err := os.Stat(path)
	if err != nil {
		os.Exit(2)
	}
	fmt.Printf("%04o", info.Mode().Perm())
	os.Exit(0)
}

func TestRuntimeBoundaryUmaskPreservesNonrootTraversal(t *testing.T) {
	for _, c := range []struct{ mask, permissions string }{{mask: "0077", permissions: "0700"}, {mask: "0022", permissions: "0755"}} {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRuntimeBoundaryUmaskDirectoryChild$")
		command.Env = []string{"SYNTHETIC_RUNTIME_UMASK=" + c.mask, "SYNTHETIC_RUNTIME_DIRECTORY=" + t.TempDir()}
		output, err := command.CombinedOutput()
		if err != nil || string(output) != c.permissions {
			t.Fatalf("mask %s produced %q (%v), want %s", c.mask, output, err, c.permissions)
		}
	}
}
