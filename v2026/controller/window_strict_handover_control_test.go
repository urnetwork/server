package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// The api's view of the switch keeps its last good read of the control: none
// until the first, off and on as the control sets them, none again when the
// field goes away or does not parse, and the last value while the control
// cannot be read. A nil owner overrides nothing.
func TestWindowStrictHandoverControlKeepsItsLastGoodRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fields map[string]string
	var readErr error
	readControl := func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
		if readErr != nil {
			return nil, readErr
		}
		return model.NarrowConnectMoveControl(fields, ceiling), nil
	}
	control := newWindowStrictHandoverControlWithStore(ctx, DefaultWindowStrictHandoverControlSettings(), readControl)
	defer control.Close()

	assertOverride := func(name string, wantEnabled bool, wantSet bool) {
		t.Helper()
		if enabled, set := control.Override(); enabled != wantEnabled || set != wantSet {
			t.Fatalf("%s: override (%t, set %t), want (%t, set %t)", name, enabled, set, wantEnabled, wantSet)
		}
	}
	assertOverride("before the first read", false, false)
	for _, step := range []struct {
		name    string
		fields  map[string]string
		err     error
		enabled bool
		set     bool
	}{
		{"no control", nil, nil, false, false},
		{"the operator turns it off", map[string]string{"windowStrictHandover": "off"}, nil, false, true},
		{"the control cannot be read", nil, errors.New("redis unavailable"), false, true},
		{"the operator turns it on", map[string]string{"windowStrictHandover": "on"}, nil, true, true},
		{"a value that does not parse", map[string]string{"windowStrictHandover": "of"}, nil, false, false},
		{"off again", map[string]string{"windowStrictHandover": "false"}, nil, false, true},
		{"the field goes away", map[string]string{"mode": "off"}, nil, false, false},
	} {
		fields, readErr = step.fields, step.err
		control.refresh()
		assertOverride(step.name, step.enabled, step.set)
	}

	var none *WindowStrictHandoverControl
	if enabled, set := none.Override(); enabled || set {
		t.Fatal("a nil owner overrides the handover")
	}
	none.Close()
}

// A read that raises keeps the last value, as one that fails does.
func TestWindowStrictHandoverControlSurvivesARaisingRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raise := false
	readControl := func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
		if raise {
			panic(errors.New("read raised"))
		}
		return model.NarrowConnectMoveControl(map[string]string{"windowStrictHandover": "off"}, ceiling), nil
	}
	control := newWindowStrictHandoverControlWithStore(ctx, DefaultWindowStrictHandoverControlSettings(), readControl)
	defer control.Close()
	control.refresh()
	raise = true
	control.refresh()
	if enabled, set := control.Override(); enabled || !set {
		t.Fatalf("a raising read changed the override to (%t, set %t)", enabled, set)
	}
}
