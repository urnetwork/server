// The runtime move control (connect_move_control.go): it narrows the code
// settings and never widens them (D14), absent fields leave the code
// defaults, and a field that does not parse narrows as far as it can.
package model

import (
	"math"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The code settings of MOVE.md §5.8 with the mover off by default.
func connectMoveTestCeiling() ConnectMoveControlCeiling {
	return ConnectMoveControlCeiling{
		EnableMove:       true,
		EnableDrainMoves: true,
		DefaultMode:      "off",
		MaxPerMinute:     120,
		Tolerance:        0.10,
	}
}

// Each control the operator might write, and the effective control it gives.
func TestConnectMoveControlNarrows(t *testing.T) {
	ceiling := connectMoveTestCeiling()
	disabled := ceiling
	disabled.EnableMove = false
	noDrainMoves := ceiling
	noDrainMoves.EnableDrainMoves = false
	dryRunDefault := ceiling
	dryRunDefault.DefaultMode = "dry_run"
	badDefault := ceiling
	badDefault.DefaultMode = "fast"

	inf := math.Inf(1)
	cases := []struct {
		name         string
		fields       map[string]string
		ceiling      ConnectMoveControlCeiling
		present      bool
		mode         ConnectMoveMode
		maxPerMinute int
		tolerance    float64
		drainMoves   bool
		evacuate     []string
	}{
		{name: "absent: code defaults", fields: nil, ceiling: ceiling,
			mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "absent with a dry-run default", fields: map[string]string{}, ceiling: dryRunDefault,
			mode: ConnectMoveModeDryRun, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "a default that does not parse is off", fields: nil, ceiling: badDefault,
			mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "the operator turns the mover on", fields: map[string]string{"mode": "on"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOn, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "dry run", fields: map[string]string{"mode": "dry_run"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeDryRun, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "the kill switch", fields: map[string]string{"mode": "off"}, ceiling: dryRunDefault,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "a mode that does not parse is off", fields: map[string]string{"mode": "ON!"}, ceiling: dryRunDefault,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "on cannot widen past EnableMove", fields: map[string]string{"mode": "on", "drainMoves": "true"}, ceiling: disabled,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: false},
		{name: "the first enable at 60/min (D8)", fields: map[string]string{"mode": "on", "maxPerMinute": "60"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOn, maxPerMinute: 60, tolerance: 0.10, drainMoves: true},
		{name: "a rate above the code cap is the code cap", fields: map[string]string{"maxPerMinute": "1000"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "a negative rate allows none", fields: map[string]string{"maxPerMinute": "-5"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: -5, tolerance: 0.10, drainMoves: true},
		{name: "a rate that does not parse allows none", fields: map[string]string{"maxPerMinute": "6O"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 0, tolerance: 0.10, drainMoves: true},
		{name: "a wider band moves fewer", fields: map[string]string{"tolerance": "0.25"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.25, drainMoves: true},
		{name: "a narrower band would move more: the code band", fields: map[string]string{"tolerance": "0.01"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: true},
		{name: "a band that does not parse makes no balance source", fields: map[string]string{"tolerance": "ten"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: inf, drainMoves: true},
		{name: "a band of NaN makes no balance source", fields: map[string]string{"tolerance": "NaN"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: inf, drainMoves: true},
		{name: "drain moves off for the next drain", fields: map[string]string{"drainMoves": "false"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: false},
		{name: "drain moves cannot widen past EnableDrainMoves", fields: map[string]string{"drainMoves": "on"}, ceiling: noDrainMoves,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: false},
		{name: "a drain switch that does not parse is off", fields: map[string]string{"drainMoves": "maybe"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOff, maxPerMinute: 120, tolerance: 0.10, drainMoves: false},
		{name: "evacuate hosts, comma or space separated", fields: map[string]string{"mode": "on", "evacuateHosts": " edge-a.example,edge-b.example  edge-c.example,,"}, ceiling: ceiling,
			present: true, mode: ConnectMoveModeOn, maxPerMinute: 120, tolerance: 0.10, drainMoves: true,
			evacuate: []string{"edge-a.example", "edge-b.example", "edge-c.example"}},
	}
	for _, c := range cases {
		control := NarrowConnectMoveControl(c.fields, c.ceiling)
		if control.Present != c.present || control.Mode != c.mode || control.MaxPerMinute != c.maxPerMinute ||
			control.Tolerance != c.tolerance || control.DrainMoves != c.drainMoves {
			t.Errorf("%s: control = %+v", c.name, control)
		}
		if len(control.EvacuateHosts) != len(c.evacuate) {
			t.Errorf("%s: evacuate hosts = %v, want %v", c.name, control.EvacuateHosts, c.evacuate)
		}
		for _, host := range c.evacuate {
			if !control.EvacuateHosts[host] {
				t.Errorf("%s: %s is not evacuated", c.name, host)
			}
		}
	}
}

// Whatever the control holds, the effective control is never wider than the
// code: no mode past EnableMove, no rate above the code cap, no band narrower
// than the code band, no drain moves past EnableDrainMoves.
func TestConnectMoveControlCannotWiden(t *testing.T) {
	values := map[string][]string{
		"mode":         {"off", "dry_run", "on", "", "garbage"},
		"maxPerMinute": {"0", "1", "119", "120", "121", "100000", "-1", "x"},
		"tolerance":    {"0", "-1", "0.05", "0.1", "0.5", "Inf", "-Inf", "NaN", "x"},
		"drainMoves":   {"true", "false", "on", "off", "1", "0", "x"},
	}
	for _, enableMove := range []bool{true, false} {
		for _, enableDrainMoves := range []bool{true, false} {
			ceiling := connectMoveTestCeiling()
			ceiling.EnableMove = enableMove
			ceiling.EnableDrainMoves = enableDrainMoves
			for _, mode := range values["mode"] {
				for _, maxPerMinute := range values["maxPerMinute"] {
					for _, tolerance := range values["tolerance"] {
						for _, drainMoves := range values["drainMoves"] {
							control := NarrowConnectMoveControl(map[string]string{
								"mode":         mode,
								"maxPerMinute": maxPerMinute,
								"tolerance":    tolerance,
								"drainMoves":   drainMoves,
							}, ceiling)
							if !enableMove && control.Mode != ConnectMoveModeOff {
								t.Fatalf("mode %s past EnableMove off", control.Mode)
							}
							if ceiling.MaxPerMinute < control.MaxPerMinute {
								t.Fatalf("rate %d above the code cap from %q", control.MaxPerMinute, maxPerMinute)
							}
							if !(ceiling.Tolerance <= control.Tolerance) {
								t.Fatalf("band %v narrower than the code band from %q", control.Tolerance, tolerance)
							}
							if control.DrainMoves && !(enableMove && enableDrainMoves) {
								t.Fatalf("drain moves on past the code from %q", drainMoves)
							}
						}
					}
				}
			}
		}
	}
}

// The modes parse from and print to the names the control uses.
func TestConnectMoveModeNames(t *testing.T) {
	for _, mode := range []ConnectMoveMode{ConnectMoveModeOff, ConnectMoveModeDryRun, ConnectMoveModeOn} {
		parsed, ok := ParseConnectMoveMode(mode.String())
		if !ok || parsed != mode {
			t.Errorf("mode %d prints %q, which parses to %d", mode, mode.String(), parsed)
		}
	}
	if ConnectMoveModeOff >= ConnectMoveModeDryRun || ConnectMoveModeDryRun >= ConnectMoveModeOn {
		t.Error("modes are not ordered from narrowest to widest")
	}
	if _, ok := ParseConnectMoveMode("dry-run"); ok {
		t.Error("parsed an unknown mode")
	}
}

// The control round-trips through Redis: absent reads the code defaults, a
// set field narrows, and removing it restores the defaults.
func TestConnectMoveControlInRedis(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		ceiling := connectMoveTestCeiling()

		control, err := GetConnectMoveControl(ctx, ceiling)
		if err != nil {
			t.Fatal(err)
		}
		if control.Present || control.Mode != ConnectMoveModeOff || control.MaxPerMinute != 120 || !control.DrainMoves {
			t.Fatalf("absent control = %+v", control)
		}

		if err := SetConnectMoveControl(ctx, map[string]string{
			ConnectMoveControlFieldMode:          "on",
			ConnectMoveControlFieldMaxPerMinute:  "60",
			ConnectMoveControlFieldEvacuateHosts: "edge-a.example",
		}); err != nil {
			t.Fatal(err)
		}
		if err := SetConnectMoveControl(ctx, map[string]string{
			ConnectMoveControlFieldDrainMoves: "false",
		}); err != nil {
			t.Fatal(err)
		}
		control, err = GetConnectMoveControl(ctx, ceiling)
		if err != nil {
			t.Fatal(err)
		}
		if !control.Present || control.Mode != ConnectMoveModeOn || control.MaxPerMinute != 60 ||
			!control.EvacuateHosts["edge-a.example"] || control.DrainMoves {
			t.Fatalf("set control = %+v", control)
		}

		// the kill switch, without a deploy
		if err := SetConnectMoveControl(ctx, map[string]string{ConnectMoveControlFieldMode: "off"}); err != nil {
			t.Fatal(err)
		}
		control, err = GetConnectMoveControl(ctx, ceiling)
		if err != nil {
			t.Fatal(err)
		}
		if control.Mode != ConnectMoveModeOff {
			t.Fatalf("kill switch left mode %s", control.Mode)
		}

		if err := RemoveConnectMoveControl(ctx); err != nil {
			t.Fatal(err)
		}
		control, err = GetConnectMoveControl(ctx, ceiling)
		if err != nil {
			t.Fatal(err)
		}
		if control.Present || control.MaxPerMinute != 120 || len(control.EvacuateHosts) != 0 || !control.DrainMoves {
			t.Fatalf("removed control = %+v", control)
		}
	})
}
