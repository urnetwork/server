// Tests for sidecarctl's registry and command line: which command a line chooses, the exit codes a
// caller reads, and how a command's arguments parse.

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// A command that records the arguments it ran with and answers with err.
func recordingSidecarctlCommand(words string, err error, ranArgs *[][]string) *sidecarctlCommand {
	return &sidecarctlCommand{
		words:   words,
		usage:   "<thing>",
		summary: "records its arguments",
		run: func(args []string, out io.Writer) error {
			*ranArgs = append(*ranArgs, args)
			return err
		},
	}
}

// Fails unless running the function panics.
func requireSidecarctlPanic(t *testing.T, label string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", label)
		}
	}()
	f()
}

// The longest registered words win, so a command whose words begin another's is not taken for it,
// and the arguments after the words are the command's own.
func TestSidecarctlChoosesTheLongestCommand(t *testing.T) {
	ranArgs := [][]string{}
	commands := newSidecarctlCommandSet()
	commands.add(recordingSidecarctlCommand("certs", nil, &ranArgs))
	commands.add(recordingSidecarctlCommand("certs swap", nil, &ranArgs))

	command, args := commands.find([]string{"certs", "swap", "main", "2026.11.5"})
	if command == nil || command.words != "certs swap" || !slices.Equal(args, []string{"main", "2026.11.5"}) {
		t.Fatalf("found %v with %v, want certs swap with [main 2026.11.5]", command, args)
	}
	command, args = commands.find([]string{"certs", "renew"})
	if command == nil || command.words != "certs" || !slices.Equal(args, []string{"renew"}) {
		t.Fatalf("found %v with %v, want certs with [renew]", command, args)
	}
	if command, _ := commands.find([]string{"dns", "census"}); command != nil {
		t.Fatalf("found %v for words nothing registered", command)
	}
}

// Two commands under one name would leave one unreachable, and a name that is not single-spaced
// words could never be typed, so registering either is a mistake that stops the binary at start.
func TestSidecarctlRefusesACommandItCouldNotReach(t *testing.T) {
	ranArgs := [][]string{}
	commands := newSidecarctlCommandSet()
	commands.add(recordingSidecarctlCommand("certs swap", nil, &ranArgs))

	requireSidecarctlPanic(t, "a second certs swap", func() {
		commands.add(recordingSidecarctlCommand("certs swap", nil, &ranArgs))
	})
	for _, words := range []string{"", " ", "certs  renew", " certs renew", "certs renew "} {
		requireSidecarctlPanic(t, fmt.Sprintf("words %q", words), func() {
			commands.add(recordingSidecarctlCommand(words, nil, &ranArgs))
		})
	}
}

// A job reads nothing but the exit code: 0 for a command that ran, 1 for one that failed, with its
// error on stderr, and 2 for a line no command takes, with the usage.
func TestSidecarctlExitCodes(t *testing.T) {
	ranArgs := [][]string{}
	commands := newSidecarctlCommandSet()
	commands.add(recordingSidecarctlCommand("works", nil, &ranArgs))
	commands.add(recordingSidecarctlCommand("fails", errors.New("the daemon is down"), &ranArgs))
	commands.add(recordingSidecarctlCommand("misused", sidecarctlUsageErrorf("want a thing"), &ranArgs))
	cases := []struct {
		args       []string
		wantCode   int
		wantStderr string
	}{
		{args: []string{"works", "a"}, wantCode: 0, wantStderr: ""},
		{args: []string{"fails", "a"}, wantCode: 1, wantStderr: "sidecarctl fails: the daemon is down"},
		{args: []string{"misused"}, wantCode: 2, wantStderr: "sidecarctl misused <thing>"},
		{args: []string{"unknown"}, wantCode: 2, wantStderr: "no command is \"unknown\""},
		{args: []string{}, wantCode: 2, wantStderr: "Usage:"},
	}
	for _, c := range cases {
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := commands.run(c.args, stdout, stderr)
		if code != c.wantCode {
			t.Errorf("%v exited %d, want %d", c.args, code, c.wantCode)
		}
		if !strings.Contains(stderr.String(), c.wantStderr) {
			t.Errorf("%v printed %q to stderr, want %q in it", c.args, stderr.String(), c.wantStderr)
		}
	}
}

// Asking for help is no mistake: the usage goes to stdout and the exit is 0.
func TestSidecarctlPrintsUsageWhenAsked(t *testing.T) {
	ranArgs := [][]string{}
	commands := newSidecarctlCommandSet()
	commands.add(recordingSidecarctlCommand("certs swap", nil, &ranArgs))
	for _, arg := range []string{"-h", "--help", "help"} {
		stdout := &bytes.Buffer{}
		if code := commands.run([]string{arg}, stdout, io.Discard); code != 0 {
			t.Errorf("%s exited %d", arg, code)
		}
		if !strings.Contains(stdout.String(), "sidecarctl certs swap <thing>") {
			t.Errorf("%s printed %q", arg, stdout.String())
		}
	}
	if len(ranArgs) != 0 {
		t.Errorf("help ran a command: %v", ranArgs)
	}
}

// A flag may stand anywhere among the positionals, its value after an =, and -- ends the flags. A
// flag the command does not have is a usage mistake.
func TestSidecarctlParsesFlagsAnywhere(t *testing.T) {
	cases := []struct {
		args            []string
		wantPositionals []string
		wantDryRun      bool
		wantSince       string
	}{
		{args: []string{"main", "2026.11.5"}, wantPositionals: []string{"main", "2026.11.5"}},
		{args: []string{"--dry-run", "main", "2026.11.5"}, wantPositionals: []string{"main", "2026.11.5"}, wantDryRun: true},
		{args: []string{"main", "--dry-run", "2026.11.5"}, wantPositionals: []string{"main", "2026.11.5"}, wantDryRun: true},
		{args: []string{"main", "2026.11.5", "--since=1h"}, wantPositionals: []string{"main", "2026.11.5"}, wantSince: "1h"},
		{args: []string{"main", "--", "--dry-run"}, wantPositionals: []string{"main", "--dry-run"}},
		{args: []string{"-", "main"}, wantPositionals: []string{"-", "main"}},
	}
	for _, c := range cases {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		dryRun := flags.Bool("dry-run", false, "")
		since := flags.String("since", "", "")
		positionals, err := parseSidecarctlArgs(flags, c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if !slices.Equal(positionals, c.wantPositionals) || *dryRun != c.wantDryRun || *since != c.wantSince {
			t.Errorf("%v parsed as %v, dry-run %v, since %q", c.args, positionals, *dryRun, *since)
		}
	}

	for _, args := range [][]string{{"main", "--unknown"}, {"main", "--since"}} {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		flags.String("since", "", "")
		_, err := parseSidecarctlArgs(flags, args)
		usageErr := &sidecarctlUsageError{}
		if !errors.As(err, &usageErr) {
			t.Errorf("%v: error = %v, want a usage mistake", args, err)
		}
	}
}

// The binary's own registry holds certs swap, which its cmd file's init registers, under the usage
// its doc gives.
func TestSidecarctlRegistersCertsSwap(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"certs", "swap", "main", "2026.11.5"})
	if command == nil || command.words != "certs swap" {
		t.Fatalf("the binary has no certs swap: %v", command)
	}
	if command.usage != "<env> <version> [--no-reload]" || !slices.Equal(args, []string{"main", "2026.11.5"}) {
		t.Fatalf("certs swap's usage is %q, with %v", command.usage, args)
	}
}
