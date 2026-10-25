package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// Each health command parses to its own words and options.
func TestHealthCommandsParse(t *testing.T) {
	for _, test := range []struct {
		args    []string
		command string
		level   string
		reason  string
	}{
		{args: []string{"health", "status"}, command: "status"},
		{args: []string{"health", "recover"}, command: "recover"},
		{args: []string{"health", "degrade", "--reason=Connections are slow."}, command: "degrade", reason: "Connections are slow."},
		{args: []string{"health", "degrade", "--reason", "Most connections fail.", "--level=major"}, command: "degrade", level: "major", reason: "Most connections fail."},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, test.args, "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		if health, _ := opts.Bool("health"); !health {
			t.Fatal(test.args, "did not parse as a health command")
		}
		if command, _ := opts.Bool(test.command); !command {
			t.Fatal(test.args, "did not parse as", test.command)
		}
		level, _ := opts.String("--level")
		reason, _ := opts.String("--reason")
		if level != test.level || reason != test.reason {
			t.Fatal(test.args, level, reason)
		}
	}
	// the default parser exits on a usage error; this one returns it
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	if _, err := parser.ParseArgs(bringyourctlUsage, []string{"health", "degrade"}, "synthetic"); err == nil {
		t.Fatal("degrade parsed without a reason")
	}
}

// A level or reason that cannot be published is refused before Redis is read.
func TestHealthDegradeRefusesBeforeWriting(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"health", "degrade", "--reason=Connections are slow.", "--level=down"}, "invalid --level"},
		{[]string{"health", "degrade", "--reason=Connections are slow.", "--level=healthy"}, "invalid --level"},
		{[]string{"health", "degrade", "--reason=edge-3 is out of disk"}, "invalid --reason: the reason names a host name"},
		{[]string{"health", "degrade", "--reason=   "}, "invalid --reason: the reason is empty"},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, test.args, "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		var out bytes.Buffer
		if code := healthCommand(opts, &out); code != 1 || !strings.HasPrefix(out.String(), test.want) {
			t.Fatalf("%v: exit %d, %q", test.args, code, out.String())
		}
	}
}

func TestFormatHealthAge(t *testing.T) {
	for age, want := range map[time.Duration]string{
		20 * time.Second:                "under a minute",
		time.Minute:                     "1m",
		72*time.Minute + 20*time.Second: "1h12m",
		26 * time.Hour:                  "26h0m",
	} {
		if got := formatHealthAge(age); got != want {
			t.Errorf("%s: %q, want %q", age, got, want)
		}
	}
}
