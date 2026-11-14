package main

import (
	"maps"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
)

// The usage archive export control command parses to the fields it sets: none
// shows the control, a mode, a floor and a weekly brake set those fields, and
// --remove deletes it. Every set it accepts is one the retention pass reads as
// set, never as off for a field it cannot parse; a mode other than on, dry_run
// or off, a floor that is not a date, a brake outside 1 to the settings' most,
// and --remove with fields are refused.
func TestUsageArchiveExportCommandParses(t *testing.T) {
	settings := server.DefaultProviderUsageArchiveExportSettings()
	for _, test := range []struct {
		args   []string
		fields map[string]string
		remove bool
		mode   server.ProviderUsageArchiveExportMode
		floor  time.Time
		weeks  int
	}{
		{args: []string{}, fields: map[string]string{}, mode: server.ProviderUsageArchiveExportModeOff, floor: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), weeks: 1},
		{args: []string{"--mode=off"}, fields: map[string]string{server.ProviderUsageArchiveExportControlFieldMode: "off"}, mode: server.ProviderUsageArchiveExportModeOff, floor: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), weeks: 1},
		{args: []string{"--mode=dry_run", "--max-weeks-per-pass=4"}, fields: map[string]string{
			server.ProviderUsageArchiveExportControlFieldMode:            "dry_run",
			server.ProviderUsageArchiveExportControlFieldMaxWeeksPerPass: "4",
		}, mode: server.ProviderUsageArchiveExportModeDryRun, floor: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), weeks: 4},
		{args: []string{"--mode=on", "--close-time-floor=2026-09-01", "--max-weeks-per-pass=2"}, fields: map[string]string{
			server.ProviderUsageArchiveExportControlFieldMode:            "on",
			server.ProviderUsageArchiveExportControlFieldCloseTimeFloor:  "2026-09-01",
			server.ProviderUsageArchiveExportControlFieldMaxWeeksPerPass: "2",
		}, mode: server.ProviderUsageArchiveExportModeOn, floor: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), weeks: 2},
		{args: []string{"--close-time-floor=2026-08-17"}, fields: map[string]string{server.ProviderUsageArchiveExportControlFieldCloseTimeFloor: "2026-08-17"}, mode: server.ProviderUsageArchiveExportModeOff, floor: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC), weeks: 1},
		{args: []string{"--remove"}, fields: map[string]string{}, remove: true, mode: server.ProviderUsageArchiveExportModeOff, floor: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), weeks: 1},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"db", "usage-archive-export"}, test.args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if command, _ := opts.Bool("usage-archive-export"); !command {
			t.Fatalf("%v: not the usage archive export command", test.args)
		}
		fields, remove, err := usageArchiveExportControlChange(opts, settings)
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if !maps.Equal(fields, test.fields) || remove != test.remove {
			t.Fatalf("%v: fields %v remove %t, want %v %t", test.args, fields, remove, test.fields, test.remove)
		}
		// the retention pass reads exactly what was set
		control := server.NarrowProviderUsageArchiveExportControl(fields, settings)
		if control.Invalid != "" || control.Mode != test.mode || !control.CloseTimeFloor.Equal(test.floor) || control.MaxWeeksPerPass != test.weeks {
			t.Fatalf("%v: the pass reads mode %s floor %s weeks %d (%q), want %s %s %d", test.args, control.Mode, control.CloseTimeFloor, control.MaxWeeksPerPass, control.Invalid, test.mode, test.floor, test.weeks)
		}
	}
	for _, args := range [][]string{
		{"--mode=yes"},
		{"--mode=ON"},
		{"--close-time-floor=09/01/2026"},
		{"--close-time-floor=2026-09-01T00:00:00Z"},
		{"--max-weeks-per-pass=0"},
		{"--max-weeks-per-pass=5"},
		{"--max-weeks-per-pass=two"},
		{"--remove", "--mode=off"},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"db", "usage-archive-export"}, args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if _, _, err := usageArchiveExportControlChange(opts, settings); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}
