//go:build linux

// Split system/runtime installation layouts must support real helper lookup
// without inheriting an operator-controlled executable search directory.
package competitionagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Child isolation exercises exec.LookPath without mutating the test process.
func TestRuntimeBoundaryHelperLookupChild(t *testing.T) {
	if os.Getenv("SYNTHETIC_HELPER_LOOKUP") != "1" {
		return
	}
	for _, name := range []string{"runc", "iptables", "ip6tables", "apparmor_parser"} {
		path, err := exec.LookPath(name)
		if err != nil || !filepath.IsAbs(path) {
			os.Exit(3)
		}
	}
	os.Exit(0)
}

// The old runtime-only path deterministically misses DNS/security helpers on
// split installations; the exact derived path finds them with no shell involved.
func TestRuntimeBoundaryHelperPathFindsSplitSystemHelpers(t *testing.T) {
	root := t.TempDir()
	runtimeDirectory, systemDirectory := filepath.Join(root, "bin"), filepath.Join(root, "sbin")
	for _, directory := range []string{runtimeDirectory, systemDirectory} {
		if err := os.Mkdir(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, directory := range map[string]string{"runc": runtimeDirectory, "iptables": systemDirectory, "ip6tables": systemDirectory, "apparmor_parser": systemDirectory} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("synthetic executable lookup fixture\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	boundary := newRuntimeBoundaryTest(t).boundary
	boundary.Runc.Path, boundary.Nftables.Path = filepath.Join(runtimeDirectory, "runc"), filepath.Join(systemDirectory, "nft")
	for _, c := range []struct {
		path    string
		success bool
	}{
		{path: runtimeDirectory, success: false},
		{path: runtimeBoundaryEnvironment(boundary)["PATH"], success: true},
	} {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRuntimeBoundaryHelperLookupChild$")
		command.Env = []string{"SYNTHETIC_HELPER_LOOKUP=1", "PATH=" + c.path}
		if err := command.Run(); (err == nil) != c.success {
			t.Fatalf("split helper lookup success=%v, want %v: %v", err == nil, c.success, err)
		}
	}
}

// Matching installations keep their old exact path. Split ones must not omit
// helpers, search the current directory, reorder authority or append a fallback.
func TestRuntimeBoundaryHelperPathRejectsOverrides(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	if path := runtimeBoundaryHelperPath(fixture.boundary); path != filepath.Dir(fixture.boundary.Runc.Path) {
		t.Fatal("same-directory installation acquired another search root")
	}
	fixture.boundary.Nftables.Path = "/opt/synthetic-system/sbin/nft"
	want := filepath.Dir(fixture.boundary.Runc.Path) + ":/opt/synthetic-system/sbin"
	if runtimeBoundaryHelperPath(fixture.boundary) != want {
		t.Fatal("split system helper directory omitted")
	}
	for _, path := range []string{filepath.Dir(fixture.boundary.Runc.Path), want + ":", ":" + want, want + ":/tmp", "/opt/synthetic-system/sbin:" + filepath.Dir(fixture.boundary.Runc.Path)} {
		environment := runtimeBoundaryEnvironment(fixture.boundary)
		environment["PATH"] = path
		var content []byte
		for name, value := range environment {
			content = append(content, []byte(name+"="+value+"\x00")...)
		}
		if err := validateRuntimeBoundaryEnvironment(content, fixture.boundary, fixture.settings); err == nil {
			t.Fatal("unqualified helper path accepted", strings.Split(path, ":"))
		}
	}
}
