package monitor

import (
	"context"
	"testing"
)

type syntheticSettingsSshAdmission struct{ t *testing.T }

func (self *syntheticSettingsSshAdmission) acquire(context.Context, string) (func() error, error) {
	self.t.Fatal("settings generation comparison attempted SSH admission")
	return nil, nil
}

func TestSharedSshAdmissionPreservesSettingsGeneration(t *testing.T) {
	settings, current := settingsFreshnessFixture(t)
	// The CLI arms admission after loading settings and installing its loader.
	// Fresh loads therefore have no process-owned backend, while startup does.
	settings.sharedSshAdmission = &syntheticSettingsSshAdmission{t: t}
	check := settings.SettingsGenerationCheck
	if same, err := check(context.Background(), settings); err != nil || !same {
		t.Fatal("shared SSH runtime state made unchanged effective settings stale")
	}
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("shared SSH runtime state emitted a settings-generation finding")
	}

	original := *current
	for _, change := range []func(*SignalSettings){
		func(next *SignalSettings) { next.PostgreSQL.Password = "synthetic-reloaded-secret" },
		func(next *SignalSettings) { next.routerDesiredGeneration[0]++ },
	} {
		*current = original
		change(current)
		if same, err := check(context.Background(), settings); err != nil || same {
			t.Fatal("ignoring shared SSH runtime state hid an effective settings change")
		}
		alerts, err = NewSettingsFreshnessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "settings-generation-stale")
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "synthetic-startup-secret", "synthetic-reloaded-secret")
		}
	}
}
