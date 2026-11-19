package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreservedBaselineUsesGoModuleBoundary(t *testing.T) {
	baselineRoot, err := filepath.Abs("baseline")
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command("go", "env", "GOMOD")
	command.Dir = baselineRoot
	output, err := command.Output()
	if err != nil {
		t.Fatalf("resolve baseline module: %v", err)
	}

	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatalf("resolve reported go.mod: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(baselineRoot, "go.mod"))
	if err != nil {
		t.Fatalf("resolve expected go.mod: %v", err)
	}
	if got != want {
		t.Fatalf("baseline GOMOD = %q, want %q", got, want)
	}
}
