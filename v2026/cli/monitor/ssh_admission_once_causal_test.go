package main

import (
	"bytes"
	"errors"
	servermonitor "github.com/urnetwork/server/v2026/monitor"
	"testing"
)

// Reaches the real CLI before any native owner or target operation. The old
// mode guard rejects these valid bounded diagnostic arguments before loading.
func TestSharedSshOnceCausalReachesSettings(t *testing.T) {
	want := errors.New("synthetic settings boundary")
	calls := 0
	err := runWithSettingsLoader([]string{"-once", "-ssh-admission-dir", "/synthetic/private/ssh", "-include-signal", "provider-selection"}, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
		calls++
		return servermonitor.SignalSettings{}, want
	})
	if calls != 1 || !errors.Is(err, want) {
		t.Fatalf("shared-ssh-once-causal: bounded diagnostic rejected before settings: calls=%d err=%v", calls, err)
	}
}
