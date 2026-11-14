package monitor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedNulReducerPreservesExactRecordsAndFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty environment", input: "", want: "state=exact records=0"},
		{name: "exact NUL boundaries", input: "fixture\x00one\ntwo\x00synthetic-private-value\x00", want: "state=exact records=3"},
		{name: "empty record", input: "fixture\x00\x00", want: "state=exact records=2"},
		{name: "unterminated record", input: "fixture\x00synthetic-private-value", want: "state=unobservable records=1"},
		{name: "exact byte cap", input: strings.Repeat("a", 65535) + "\x00", want: "state=exact records=1"},
		{name: "overflow byte", input: strings.Repeat("a", 65536) + "\x00", want: "state=unobservable records=0"},
	} {
		inputPath := filepath.Join(t.TempDir(), "fixture")
		if err := os.WriteFile(inputPath, []byte(test.input), 0o600); err != nil {
			t.Fatal(err)
		}
		program := boundedNulBytesReader + `
if ! bytes=$(bounded_nul_bytes "$1"); then printf '%s\n' 'state=unobservable records=0'; exit 0; fi
printf '%s\n' "$bytes" | LC_ALL=C awk '
function observeNulRecord(value) {}
` + boundedNulRecordsAwk + `
END {state=nulInvalid ? "unobservable" : "exact"; printf "state=%s records=%d\n",state,nulRecords}
'
`
		output, err := exec.Command("sh", "-c", program, "fixture", inputPath).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: reducer failed: %v", test.name, err)
		}
		if got := strings.TrimSpace(string(output)); got != test.want {
			t.Errorf("%s: reduced=%q, want %q", test.name, got, test.want)
		}
	}
}

func TestBoundedNulReaderOmittedPathReturnsOnlyFixedVisibility(t *testing.T) {
	privatePath := filepath.Join(t.TempDir(), "synthetic-private-path")
	program := boundedNulBytesReader + `
if bytes=$(bounded_nul_bytes "$1"); then printf '%s\n' unexpected; else printf '%s\n' unobservable; fi
`
	output, err := exec.Command("sh", "-c", program, "fixture", privatePath).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != fmt.Sprintln("unobservable") {
		t.Fatalf("failed read did not retain fixed privacy: %q", output)
	}
}

func TestBoundedNulReaderOptionalSudoIsReadOnlyBoundedAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeCommand := func(name string, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeCommand("od", "printf '%s\\n' 'partial-private-fixture'\nprintf '%s\\n' 'private-read-failure' >&2\nexit 1\n")
	writeCommand("sudo", `case "$*" in
'-n od -An -v -tu1 -N 65537 /proc/123/environ') printf '%s\n' '97 0' ;;
*) printf '%s\n' 'private-denial-detail' >&2; exit 1 ;;
esac
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, test := range []struct {
		name       string
		path       string
		permission string
		want       string
	}{
		{name: "explicit bounded sudo", path: "/proc/123/environ", permission: "allow-sudo", want: "97 0\n"},
		{name: "default denied", path: "/proc/123/environ", want: "unobservable\n"},
		{name: "sudo denied", path: "/proc/124/environ", permission: "allow-sudo", want: "unobservable\n"},
		{name: "arbitrary file denied", path: "/synthetic-private-file", permission: "allow-sudo", want: "unobservable\n"},
		{name: "PID path traversal denied", path: "/proc/../environ", permission: "allow-sudo", want: "unobservable\n"},
		{name: "nested PID path traversal denied", path: "/proc/123/../../environ", permission: "allow-sudo", want: "unobservable\n"},
	} {
		program := boundedNulBytesReader + `
if bytes=$(bounded_nul_bytes "$1" "$2"); then printf '%s\n' "$bytes"; else printf '%s\n' unobservable; fi
`
		output, err := exec.Command("sh", "-c", program, "fixture", test.path, test.permission).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		if string(output) != test.want {
			t.Errorf("%s: output=%q, want %q", test.name, output, test.want)
		}
	}
}
