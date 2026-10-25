package monitor

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestSharedSshOnceSettingsPreserveInventoryAndFailure(t *testing.T) {
	settings := SignalSettings{Hosts: []HostSettings{{Name: "b.example"}, {Name: "a.example"}, {Name: "a.example"}}}
	backend := &syntheticSettingsSshAdmission{t: t}
	called := 0
	configured, err := settings.withSharedSshAdmission(context.Background(), "/synthetic/private/ssh", func(_ context.Context, directory string, hosts []string) (sshAdmissionBackend, error) {
		called++
		if directory != "/synthetic/private/ssh" || !slices.Equal(hosts, []string{"a.example", "b.example"}) {
			t.Fatal("diagnostic attachment changed sorted complete host inventory")
		}
		return backend, nil
	})
	if err != nil || called != 1 || configured.sharedSshAdmission != backend || settings.sharedSshAdmission != nil || len(settings.Hosts) != 3 {
		t.Fatal("admission setup changed caller's inventory or backend selection")
	}
	want := errors.New("synthetic constructor refusal")
	if _, err := settings.withSharedSshAdmission(context.Background(), "/synthetic/private/ssh", func(context.Context, string, []string) (sshAdmissionBackend, error) { return nil, want }); !errors.Is(err, want) {
		t.Fatal("diagnostic constructor failure became configured transport")
	}
}
