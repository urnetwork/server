package main

import (
	"testing"

	"github.com/docopt/docopt-go"
)

// The extender pool's commands parse with exactly their options: the replay and
// the shadow report name an epoch and an optional output file, and the registry's
// commands a client, a kind and who acts.
func TestStV2ExtenderCommandsParse(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	for _, c := range []struct {
		args    []string
		command string
		options map[string]string
	}{
		{[]string{"st", "v2-extender-replay", "--epoch=4"}, "v2-extender-replay", map[string]string{"--epoch": "4"}},
		{[]string{"st", "v2-extender-replay", "--epoch=4", "--out=replay.json"}, "v2-extender-replay", map[string]string{"--epoch": "4", "--out": "replay.json"}},
		{[]string{"st", "v2-extender-shadow", "--epoch=6"}, "v2-extender-shadow", map[string]string{"--epoch": "6"}},
		{[]string{"st", "v2-platform-endpoints"}, "v2-platform-endpoints", map[string]string{}},
		{[]string{"st", "v2-platform-endpoint-register", "--client_id=018f0000-0000-0000-0000-000000000001", "--kind=tlsedge", "--by=operator"}, "v2-platform-endpoint-register",
			map[string]string{"--client_id": "018f0000-0000-0000-0000-000000000001", "--kind": "tlsedge", "--by": "operator"}},
		{[]string{"st", "v2-platform-endpoint-revoke", "--client_id=018f0000-0000-0000-0000-000000000001", "--by=operator"}, "v2-platform-endpoint-revoke",
			map[string]string{"--client_id": "018f0000-0000-0000-0000-000000000001", "--by": "operator"}},
	} {
		opts, err := parser.ParseArgs(bringyourctlUsage, c.args, "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if run, _ := opts.Bool(c.command); !run {
			t.Fatalf("%v does not select %s", c.args, c.command)
		}
		for name, want := range c.options {
			if got, _ := opts.String(name); got != want {
				t.Fatalf("%v: %s is %q, not %q", c.args, name, got, want)
			}
		}
	}
	for _, args := range [][]string{
		{"st", "v2-extender-replay"},
		{"st", "v2-platform-endpoint-register", "--client_id=x", "--by=operator"},
		{"st", "v2-platform-endpoint-revoke", "--client_id=x"},
	} {
		if _, err := parser.ParseArgs(bringyourctlUsage, args, "synthetic"); err == nil {
			t.Fatalf("%v parsed without its required options", args)
		}
	}
}
