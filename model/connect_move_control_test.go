// The runtime move control (connect_move_control.go): it narrows the code
// settings and never widens them (D14), absent fields leave the code
// defaults, and a field that does not parse narrows as far as it can.
package model

import (
	"math"
	"testing"

	"github.com/urnetwork/server"
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

// The control's placement switch (MOVE.md §13): absent keeps the placing
// process's switch, the operator can turn placement off, never on past that
// switch, a value that does not parse is off, and the mover's own switches do
// not touch it.
func TestConnectMoveControlNarrowsPlacement(t *testing.T) {
	placing := connectMoveTestCeiling()
	placing.EnablePlacement = true
	placingWithoutMoves := placing
	placingWithoutMoves.EnableMove = false
	cases := []struct {
		name      string
		fields    map[string]string
		ceiling   ConnectMoveControlCeiling
		placement bool
	}{
		{name: "absent: the placing switch", fields: nil, ceiling: placing, placement: true},
		{name: "absent for the mover's ceiling", fields: nil, ceiling: connectMoveTestCeiling(), placement: false},
		{name: "other fields keep placement", fields: map[string]string{"mode": "on"}, ceiling: placing, placement: true},
		{name: "the operator turns placement off", fields: map[string]string{"placement": "off"}, ceiling: placing, placement: false},
		{name: "false", fields: map[string]string{"placement": "false"}, ceiling: placing, placement: false},
		{name: "on keeps placement on", fields: map[string]string{"placement": "on"}, ceiling: placing, placement: true},
		{name: "on cannot widen past the placing switch", fields: map[string]string{"placement": "on"}, ceiling: connectMoveTestCeiling(), placement: false},
		{name: "a value that does not parse is off", fields: map[string]string{"placement": "yes please"}, ceiling: placing, placement: false},
		{name: "moves off leave placement on", fields: map[string]string{"mode": "on"}, ceiling: placingWithoutMoves, placement: true},
	}
	for _, c := range cases {
		if control := NarrowConnectMoveControl(c.fields, c.ceiling); control.Placement != c.placement {
			t.Errorf("%s: placement = %t, want %t", c.name, control.Placement, c.placement)
		}
	}
}

// The control's transport delay switch (connect DELAY.md §11): absent keeps
// the connect handler's switch, the operator can turn the delay header off,
// never on past that switch, a value that does not parse is off, and neither
// the mover's switches nor the placement's touch it.
func TestConnectMoveControlNarrowsTransportDelay(t *testing.T) {
	handling := ConnectMoveControlCeiling{EnableTransportDelay: true}
	handlingWithMoves := connectMoveTestCeiling()
	handlingWithMoves.EnableTransportDelay = true
	placing := connectMoveTestCeiling()
	placing.EnablePlacement = true
	cases := []struct {
		name           string
		fields         map[string]string
		ceiling        ConnectMoveControlCeiling
		transportDelay bool
	}{
		{name: "absent: the handler's switch", fields: nil, ceiling: handling, transportDelay: true},
		{name: "absent for the mover's ceiling", fields: nil, ceiling: connectMoveTestCeiling(), transportDelay: false},
		{name: "absent for the placement's ceiling", fields: nil, ceiling: placing, transportDelay: false},
		{name: "other fields keep it", fields: map[string]string{"mode": "off", "placement": "off", "drainMoves": "false"}, ceiling: handlingWithMoves, transportDelay: true},
		{name: "the operator turns it off", fields: map[string]string{"transportDelay": "off"}, ceiling: handling, transportDelay: false},
		{name: "false", fields: map[string]string{"transportDelay": "false"}, ceiling: handling, transportDelay: false},
		{name: "0", fields: map[string]string{"transportDelay": "0"}, ceiling: handling, transportDelay: false},
		{name: "on keeps it on", fields: map[string]string{"transportDelay": "on"}, ceiling: handling, transportDelay: true},
		{name: "true, padded", fields: map[string]string{"transportDelay": " true "}, ceiling: handling, transportDelay: true},
		{name: "on cannot widen past the handler's switch", fields: map[string]string{"transportDelay": "on"}, ceiling: ConnectMoveControlCeiling{}, transportDelay: false},
		{name: "a value that does not parse is off", fields: map[string]string{"transportDelay": "enabled"}, ceiling: handling, transportDelay: false},
		{name: "an empty value is off", fields: map[string]string{"transportDelay": ""}, ceiling: handling, transportDelay: false},
		{name: "a field name is exact", fields: map[string]string{"transportdelay": "off"}, ceiling: handling, transportDelay: true},
	}
	for _, c := range cases {
		if control := NarrowConnectMoveControl(c.fields, c.ceiling); control.TransportDelay != c.transportDelay {
			t.Errorf("%s: transport delay = %t, want %t", c.name, control.TransportDelay, c.transportDelay)
		}
	}
	// the switch changes nothing the mover reads
	moves := NarrowConnectMoveControl(map[string]string{"mode": "on", "transportDelay": "off"}, handlingWithMoves)
	if moves.Mode != ConnectMoveModeOn || moves.MaxPerMinute != 120 || moves.Tolerance != 0.10 || !moves.DrainMoves {
		t.Errorf("transport delay off changed the mover's control: %+v", moves)
	}
}

// The exchange's in-process delivery (connect resident_local_transport.go):
// the control chooses the blocks, never past the exchange's switch, the code
// default decides while the field is absent, and only the exchange's ceiling
// reads it.
func TestConnectMoveControlNarrowsLocalTransport(t *testing.T) {
	dark := ConnectMoveControlCeiling{EnableLocalTransport: true, LocalTransportDefault: "off"}
	shipped := ConnectMoveControlCeiling{EnableLocalTransport: true, LocalTransportDefault: "all"}
	ceilingOff := ConnectMoveControlCeiling{EnableLocalTransport: false, LocalTransportDefault: "all"}
	handling := ConnectMoveControlCeiling{EnableTransportDelay: true}
	cases := []struct {
		name    string
		fields  map[string]string
		ceiling ConnectMoveControlCeiling
		allowed []string
		refused []string
	}{
		{name: "absent: dark by default", fields: nil, ceiling: dark, refused: []string{"beta", "g1", "g2"}},
		{name: "absent: a default of all", fields: nil, ceiling: shipped, allowed: []string{"beta", "g1", "g2"}},
		{name: "absent: the ceiling off", fields: nil, ceiling: ceilingOff, refused: []string{"beta", "g1"}},
		{name: "the beta stage", fields: map[string]string{"localTransport": "beta"}, ceiling: dark, allowed: []string{"beta"}, refused: []string{"g1", "g2"}},
		{name: "g1 against the other blocks", fields: map[string]string{"localTransport": "g1"}, ceiling: dark, allowed: []string{"g1"}, refused: []string{"beta", "g2", "g3", "g4"}},
		{name: "a list, padded and mixed separators", fields: map[string]string{"localTransport": " beta, g1 g2\tg3 "}, ceiling: dark, allowed: []string{"beta", "g1", "g2", "g3"}, refused: []string{"g4"}},
		{name: "all", fields: map[string]string{"localTransport": "all"}, ceiling: dark, allowed: []string{"beta", "g1", "g4"}},
		{name: "the kill switch", fields: map[string]string{"localTransport": "off"}, ceiling: shipped, refused: []string{"beta", "g1"}},
		{name: "all cannot widen past the ceiling", fields: map[string]string{"localTransport": "all"}, ceiling: ceilingOff, refused: []string{"beta", "g1"}},
		{name: "a block cannot widen past the ceiling", fields: map[string]string{"localTransport": "g1"}, ceiling: ceilingOff, refused: []string{"g1"}},
		{name: "an empty value names no block", fields: map[string]string{"localTransport": ""}, ceiling: shipped, refused: []string{"beta", "g1"}},
		{name: "on is not all: it names a block called on", fields: map[string]string{"localTransport": "on"}, ceiling: dark, refused: []string{"beta", "g1"}},
		{name: "a keyword is exact", fields: map[string]string{"localTransport": "All"}, ceiling: dark, refused: []string{"beta", "g1"}},
		{name: "a field name is exact", fields: map[string]string{"localtransport": "all"}, ceiling: dark, refused: []string{"beta", "g1"}},
		{name: "other ceilings ignore it", fields: map[string]string{"localTransport": "all"}, ceiling: handling, refused: []string{"beta", "g1"}},
		{name: "other ceilings ignore it with moves", fields: map[string]string{"localTransport": "all"}, ceiling: connectMoveTestCeiling(), refused: []string{"beta", "g1"}},
	}
	for _, c := range cases {
		control := NarrowConnectMoveControl(c.fields, c.ceiling)
		for _, block := range c.allowed {
			if !control.LocalTransportAllows(block) {
				t.Errorf("%s: block %s refused, want allowed", c.name, block)
			}
		}
		for _, block := range c.refused {
			if control.LocalTransportAllows(block) {
				t.Errorf("%s: block %s allowed, want refused", c.name, block)
			}
		}
	}
	// the switch changes nothing the mover or the handlers read
	both := connectMoveTestCeiling()
	both.EnableTransportDelay = true
	both.EnableLocalTransport = true
	moves := NarrowConnectMoveControl(map[string]string{"mode": "on", "localTransport": "off"}, both)
	if moves.Mode != ConnectMoveModeOn || moves.MaxPerMinute != 120 || moves.Tolerance != 0.10 || !moves.DrainMoves || !moves.TransportDelay {
		t.Errorf("local transport off changed another control: %+v", moves)
	}
}

// The stuck provider switches choose blocks the way localTransport does, each
// under its own ceiling and from its own field: the api's signal and the
// exchanges' recycle never read each other's field, and every other reader
// ignores both.
func TestConnectMoveControlNarrowsStuckProviderSwitches(t *testing.T) {
	type stuckSwitch struct {
		name    string
		field   string
		dark    ConnectMoveControlCeiling
		shipped ConnectMoveControlCeiling
		allows  func(*ConnectMoveControl, string) bool
	}
	switches := []stuckSwitch{
		{
			name:    "signal",
			field:   ConnectMoveControlFieldStuckProviderSignal,
			dark:    ConnectMoveControlCeiling{EnableStuckProviderSignal: true, StuckProviderSignalDefault: "off"},
			shipped: ConnectMoveControlCeiling{EnableStuckProviderSignal: true, StuckProviderSignalDefault: "all"},
			allows:  (*ConnectMoveControl).StuckProviderSignalAllows,
		},
		{
			name:    "recycle",
			field:   ConnectMoveControlFieldStuckProviderRecycle,
			dark:    ConnectMoveControlCeiling{EnableStuckProviderRecycle: true, StuckProviderRecycleDefault: "off"},
			shipped: ConnectMoveControlCeiling{EnableStuckProviderRecycle: true, StuckProviderRecycleDefault: "all"},
			allows:  (*ConnectMoveControl).StuckProviderRecycleAllows,
		},
	}
	for i, s := range switches {
		other := switches[1-i]
		ceilingOff := s.shipped
		ceilingOff.EnableStuckProviderSignal = false
		ceilingOff.EnableStuckProviderRecycle = false
		cases := []struct {
			name    string
			fields  map[string]string
			ceiling ConnectMoveControlCeiling
			allowed []string
			refused []string
		}{
			{name: "absent: dark by default", fields: nil, ceiling: s.dark, refused: []string{"beta", "g1", "g2"}},
			{name: "absent: a default of all", fields: nil, ceiling: s.shipped, allowed: []string{"beta", "g1", "g2"}},
			{name: "absent: the ceiling off", fields: nil, ceiling: ceilingOff, refused: []string{"beta", "g1"}},
			{name: "one block for the trial", fields: map[string]string{s.field: "g1"}, ceiling: s.dark, allowed: []string{"g1"}, refused: []string{"beta", "g2", "g3", "g4"}},
			{name: "a list, padded and mixed separators", fields: map[string]string{s.field: " beta, g1 g2\tg3 "}, ceiling: s.dark, allowed: []string{"beta", "g1", "g2", "g3"}, refused: []string{"g4"}},
			{name: "all", fields: map[string]string{s.field: "all"}, ceiling: s.dark, allowed: []string{"beta", "g1", "g4"}},
			{name: "the kill switch", fields: map[string]string{s.field: "off"}, ceiling: s.shipped, refused: []string{"beta", "g1"}},
			{name: "all cannot widen past the ceiling", fields: map[string]string{s.field: "all"}, ceiling: ceilingOff, refused: []string{"beta", "g1"}},
			{name: "an empty value names no block", fields: map[string]string{s.field: ""}, ceiling: s.shipped, refused: []string{"beta", "g1"}},
			{name: "on is not all", fields: map[string]string{s.field: "on"}, ceiling: s.dark, refused: []string{"beta", "g1"}},
			{name: "the other switch's field", fields: map[string]string{other.field: "all"}, ceiling: s.dark, refused: []string{"beta", "g1"}},
			{name: "the local transport field", fields: map[string]string{ConnectMoveControlFieldLocalTransport: "all"}, ceiling: s.dark, refused: []string{"beta", "g1"}},
			{name: "other ceilings ignore it", fields: map[string]string{s.field: "all"}, ceiling: other.dark, refused: []string{"beta", "g1"}},
			{name: "other ceilings ignore it with moves", fields: map[string]string{s.field: "all"}, ceiling: connectMoveTestCeiling(), refused: []string{"beta", "g1"}},
		}
		for _, c := range cases {
			control := NarrowConnectMoveControl(c.fields, c.ceiling)
			for _, block := range c.allowed {
				if !s.allows(control, block) {
					t.Errorf("%s %s: block %s refused, want allowed", s.name, c.name, block)
				}
			}
			for _, block := range c.refused {
				if s.allows(control, block) {
					t.Errorf("%s %s: block %s allowed, want refused", s.name, c.name, block)
				}
			}
		}
	}
	// the switches change nothing else the readers read
	both := connectMoveTestCeiling()
	both.EnableLocalTransport = true
	both.LocalTransportDefault = "all"
	both.EnableStuckProviderSignal = true
	both.EnableStuckProviderRecycle = true
	control := NarrowConnectMoveControl(map[string]string{
		"mode":                 "on",
		"stuckProviderSignal":  "g1",
		"stuckProviderRecycle": "g2",
	}, both)
	if control.Mode != ConnectMoveModeOn || control.MaxPerMinute != 120 || !control.DrainMoves || !control.LocalTransportAllows("g3") {
		t.Errorf("the stuck provider switches changed another control: %+v", control)
	}
	if !control.StuckProviderSignalAllows("g1") || control.StuckProviderSignalAllows("g2") ||
		!control.StuckProviderRecycleAllows("g2") || control.StuckProviderRecycleAllows("g1") {
		t.Errorf("the stuck provider switches mixed their blocks: %+v", control)
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

// The platform's override of the apps' strict window handover: true or false,
// 1 or 0, on or off, and a value that does not parse overrides nothing, as an
// absent field does, so the off switch defaults to each app's own setting. It
// reads no ceiling and changes nothing the mover reads.
func TestConnectMoveControlWindowStrictHandover(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name     string
		fields   map[string]string
		override *bool
	}{
		{name: "absent", fields: nil, override: nil},
		{name: "other fields keep none", fields: map[string]string{"mode": "on", "placement": "off"}, override: nil},
		{name: "off", fields: map[string]string{"windowStrictHandover": "off"}, override: &off},
		{name: "false", fields: map[string]string{"windowStrictHandover": "false"}, override: &off},
		{name: "0, padded", fields: map[string]string{"windowStrictHandover": " 0 "}, override: &off},
		{name: "on", fields: map[string]string{"windowStrictHandover": "on"}, override: &on},
		{name: "true", fields: map[string]string{"windowStrictHandover": "true"}, override: &on},
		{name: "1", fields: map[string]string{"windowStrictHandover": "1"}, override: &on},
		{name: "a value that does not parse overrides nothing", fields: map[string]string{"windowStrictHandover": "disabled"}, override: nil},
		{name: "an empty value overrides nothing", fields: map[string]string{"windowStrictHandover": ""}, override: nil},
		{name: "a field name is exact", fields: map[string]string{"windowstricthandover": "off"}, override: nil},
	}
	for _, c := range cases {
		for _, ceiling := range []ConnectMoveControlCeiling{{}, connectMoveTestCeiling()} {
			control := NarrowConnectMoveControl(c.fields, ceiling)
			if (control.WindowStrictHandover == nil) != (c.override == nil) ||
				(c.override != nil && *control.WindowStrictHandover != *c.override) {
				t.Errorf("%s: override = %v, want %v", c.name, control.WindowStrictHandover, c.override)
			}
		}
	}
	moves := NarrowConnectMoveControl(map[string]string{"mode": "on", "windowStrictHandover": "off"}, connectMoveTestCeiling())
	if moves.Mode != ConnectMoveModeOn || moves.MaxPerMinute != 120 || moves.Tolerance != 0.10 || !moves.DrainMoves {
		t.Errorf("the handover override changed the mover's control: %+v", moves)
	}
}
