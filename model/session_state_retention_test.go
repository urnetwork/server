// The session state retention's brake and settings, without a database
// (session_state_retention.go). The DB tests are in
// session_state_retention_db_test.go.
package model

import (
	"testing"
	"time"
)

// The brake narrows to the code's bounds: absent, both passes at the default
// rate; off stops both; a rate is capped at the maximum, and one that does not
// parse stops its table only.
func TestNarrowSessionStateRetentionControl(t *testing.T) {
	settings := DefaultSessionStateRetentionSettings()
	rate := settings.DefaultRowsPerSecond
	for _, test := range []struct {
		fields      map[string]string
		head        float64
		deviceState float64
	}{
		{nil, rate, rate},
		{map[string]string{"mode": "on"}, rate, rate},
		{map[string]string{"mode": "off"}, 0, 0},
		{map[string]string{"mode": "paused", "maxHeadsPerSecond": "500"}, 0, 0},
		{map[string]string{"maxHeadsPerSecond": "500"}, 500, rate},
		{map[string]string{"maxDeviceStatesPerSecond": "1e9"}, rate, settings.MaxRowsPerSecond},
		{map[string]string{"maxHeadsPerSecond": "0"}, 0, rate},
		{map[string]string{"maxDeviceStatesPerSecond": "NaN"}, rate, 0},
		{map[string]string{"maxHeadsPerSecond": "fast"}, 0, rate},
	} {
		control := NarrowSessionStateRetentionControl(test.fields, settings)
		if control.RowsPerSecond(SessionStateRetentionTableHead) != test.head ||
			control.RowsPerSecond(SessionStateRetentionTableDeviceState) != test.deviceState ||
			control.Present != (0 < len(test.fields)) {
			t.Fatalf("fields %v narrowed to %+v, want heads %v device states %v", test.fields, control, test.head, test.deviceState)
		}
	}
}

// The owner's windows and pace (2026-11-16): offline device state a day, at
// most 2,000 rows a second for each table, on by default; the head pass waits
// for the journal's weekly drops, so it runs daily.
func TestSessionStateRetentionDefaults(t *testing.T) {
	settings := DefaultSessionStateRetentionSettings()
	if settings.DeviceStateRetention != 24*time.Hour {
		t.Fatalf("device state retention %s", settings.DeviceStateRetention)
	}
	if settings.MaxRowsPerSecond != 2000 || settings.DefaultRowsPerSecond != settings.MaxRowsPerSecond {
		t.Fatalf("rates default %v max %v", settings.DefaultRowsPerSecond, settings.MaxRowsPerSecond)
	}
	if control := NarrowSessionStateRetentionControl(nil, settings); !(0 < control.RowsPerSecond(SessionStateRetentionTableHead)) || !(0 < control.RowsPerSecond(SessionStateRetentionTableDeviceState)) {
		t.Fatal("the retention is not on without a brake")
	}
	if settings.HeadPassInterval != 24*time.Hour || settings.DeviceStatePassInterval <= 0 || settings.DeviceStatePassInterval > settings.DeviceStateRetention {
		t.Fatalf("pass intervals heads %s device states %s", settings.HeadPassInterval, settings.DeviceStatePassInterval)
	}
	if settings.TaskMaxTime <= settings.RunBudget || settings.MoreInterval <= 0 || settings.RunInterval <= settings.MoreInterval {
		t.Fatalf("schedule %+v", settings)
	}
}
