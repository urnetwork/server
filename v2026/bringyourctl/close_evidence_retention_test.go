package main

import (
	"maps"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
)

// The close evidence brake command parses to the fields it sets: none shows
// the brake, a mode and a rate set those fields, --remove deletes it; a mode
// other than on or off, a rate that is not a number of zero or more, and
// --remove with fields are refused.
func TestCloseEvidenceRetentionCommandParses(t *testing.T) {
	for _, test := range []struct {
		args   []string
		fields map[string]string
		remove bool
	}{
		{args: []string{}, fields: map[string]string{}},
		{args: []string{"--mode=off"}, fields: map[string]string{server.CloseEvidenceRetentionControlFieldMode: "off"}},
		{args: []string{"--mode=on", "--max-rows-per-second=2000"}, fields: map[string]string{
			server.CloseEvidenceRetentionControlFieldMode:             "on",
			server.CloseEvidenceRetentionControlFieldMaxRowsPerSecond: "2000",
		}},
		{args: []string{"--max-rows-per-second=0"}, fields: map[string]string{server.CloseEvidenceRetentionControlFieldMaxRowsPerSecond: "0"}},
		{args: []string{"--remove"}, fields: map[string]string{}, remove: true},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"db", "close-evidence-retention"}, test.args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if command, _ := opts.Bool("close-evidence-retention"); !command {
			t.Fatalf("%v: not the close evidence retention command", test.args)
		}
		fields, remove, err := closeEvidenceRetentionBrakeChange(opts)
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if !maps.Equal(fields, test.fields) || remove != test.remove {
			t.Fatalf("%v: fields %v remove %t, want %v %t", test.args, fields, remove, test.fields, test.remove)
		}
	}
	for _, args := range [][]string{
		{"--mode=dry_run"},
		{"--max-rows-per-second=fast"},
		{"--max-rows-per-second=-1"},
		{"--remove", "--mode=off"},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"db", "close-evidence-retention"}, args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if _, _, err := closeEvidenceRetentionBrakeChange(opts); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}
