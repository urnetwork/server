// Tests for `sidecarctl dns census` against a fake census: what it prints, its exit codes, and that a failure
// prints no count. cmd_dns_census_server_test.go runs it against Postgres on the private test stack.

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// A command set holding dns census under settings, so a test reads the exit code a person's shell reads.
func dnsCensusTestCommands(settings *dnsCensusSettings) *sidecarctlCommandSet {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{
		words:   "dns census",
		usage:   "",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runDnsCensusWithSettings(args, settings, out)
		},
	})
	return commands
}

// A run prints the env and the four counts under the design's column names, one a line, and nothing else.
func TestDnsCensusPrintsTheFourCounts(t *testing.T) {
	settings := defaultDnsCensusSettings()
	settings.census = func(ctx context.Context) (string, *model.DnsNetworkNameCensus, error) {
		return "test", &model.DnsNetworkNameCensus{PreRule: 11, EdgeHyphen: 7, Dash34: 5, Reserved: 3}, nil
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := dnsCensusTestCommands(settings).run([]string{"dns", "census"}, stdout, stderr); code != 0 {
		t.Fatalf("exited %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	want := []string{"pre_rule 11", "edge_hyphen 7", "dash34 5", "reserved 3"}
	if len(lines) != 5 || !strings.Contains(lines[0], "in test") {
		t.Fatalf("printed %q, want a header naming the env and four counts", stdout.String())
	}
	for index, line := range want {
		if lines[index+1] != line {
			t.Errorf("line %d is %q, want %q", index+1, lines[index+1], line)
		}
	}
}

// A census that fails, answers nothing, or runs past its bound exits 1 and prints no count; an argument exits 2
// before anything is read.
func TestDnsCensusPrintsNoCountWhenItFails(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		census   func(ctx context.Context) (string, *model.DnsNetworkNameCensus, error)
		expired  bool
		wantCode int
		want     string
	}{
		{
			name: "a failed statement",
			census: func(ctx context.Context) (string, *model.DnsNetworkNameCensus, error) {
				return "", nil, errors.New("cannot execute in a read-only transaction")
			},
			wantCode: 1, want: "the census did not run: cannot execute",
		},
		{
			name: "no counts",
			census: func(ctx context.Context) (string, *model.DnsNetworkNameCensus, error) {
				return "test", nil, nil
			},
			wantCode: 1, want: "answered no counts",
		},
		{name: "past its bound", expired: true, wantCode: 1, want: "nothing was counted"},
		{name: "an argument", args: []string{"main"}, wantCode: 2, want: "takes no arguments"},
	}
	for _, c := range cases {
		ran := 0
		settings := defaultDnsCensusSettings()
		if c.expired {
			settings.timeout = 0
		}
		settings.census = func(ctx context.Context) (string, *model.DnsNetworkNameCensus, error) {
			ran += 1
			if c.census == nil {
				return "test", &model.DnsNetworkNameCensus{}, nil
			}
			return c.census(ctx)
		}
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := dnsCensusTestCommands(settings).run(append([]string{"dns", "census"}, c.args...), stdout, stderr)
		if code != c.wantCode || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: exited %d with %q, want %d and %q", c.name, code, stderr.String(), c.wantCode, c.want)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: printed %q", c.name, stdout.String())
		}
		if (c.census == nil) != (ran == 0) {
			t.Errorf("%s: the census ran %d times", c.name, ran)
		}
	}
}

// The binary as built refuses with no WARP_ENV before it opens a session, and its registry holds the command.
func TestDnsCensusRefusesWithNoEnv(t *testing.T) {
	t.Setenv("WARP_ENV", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := sidecarctlCommands.run([]string{"dns", "census"}, stdout, stderr); code != 1 {
		t.Fatalf("exited %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WARP_ENV is not set") || stdout.Len() != 0 {
		t.Errorf("printed %q and %q, want the refusal alone", stdout.String(), stderr.String())
	}
}
