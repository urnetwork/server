package model

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

func TestNativeReaderSettingsDefaultAndActivation(t *testing.T) {
	if DefaultEgressIndexSettings().NativeReaderEnabled {
		t.Fatal("reader activation must be explicit")
	}
	for _, tc := range []struct {
		name, config string
		enabled      bool
	}{
		{name: "missing"},
		{name: "unrelated", config: "enable_egress_test: true\n"},
		{name: "disabled", config: "egress_index:\n  native_reader_enabled: false\n"},
		{name: "enabled", config: "egress_index:\n  native_reader_enabled: true\n", enabled: true},
		{name: "invalid_scalar", config: "egress_index:\n  native_reader_enabled: invalid\n"},
		{name: "invalid_shape", config: "egress_index:\n  native_reader_enabled: [true]\n"},
		{name: "malformed", config: "egress_index: [unterminated\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(tc.config))
			defer pop()
			requestEgressIndexSettingsSnapshot.Store(nil)
			defer requestEgressIndexSettingsSnapshot.Store(nil)
			if got := requestEgressIndexSettings().NativeReaderEnabled; got != tc.enabled {
				t.Fatalf("enabled=%t want=%t", got, tc.enabled)
			}
			families, err := prometheus.DefaultGatherer.Gather()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, family := range families {
				if family.GetName() != "urnetwork_findproviders2_native_reader_enabled" {
					continue
				}
				found = true
				if len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 {
					t.Fatal("activation gauge must have one unlabeled cell")
				}
				want := float64(0)
				if tc.enabled {
					want = 1
				}
				if got := family.Metric[0].GetGauge().GetValue(); got != want {
					t.Fatalf("quiet effective activation=%g want=%g", got, want)
				}
			}
			if !found {
				t.Fatal("quiet process lacks explicit activation witness")
			}
		})
	}
}

func TestNativeReaderSettingsRefreshAndRollback(t *testing.T) {
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("egress_index:\n  native_reader_enabled: false\n"))
	defer pop()
	requestEgressIndexSettingsSnapshot.Store(nil)
	defer requestEgressIndexSettingsSnapshot.Store(nil)
	initial := requestEgressIndexSettings()
	popEnabled := server.Config.PushSimpleResource(providerConfigResourceName, []byte("egress_index:\n  native_reader_enabled: true\n"))
	defer popEnabled()
	if requestEgressIndexSettings().NativeReaderEnabled {
		t.Fatal("config changed before the bounded snapshot expired")
	}
	requestEgressIndexSettingsSnapshot.Store(&egressIndexSettingsSnapshot{settings: initial, loadTime: time.Now().Add(-2 * time.Minute)})
	enabled := requestEgressIndexSettings()
	if !enabled.NativeReaderEnabled || initial.NativeReaderEnabled {
		t.Fatal("refresh mutated a captured request snapshot or failed to activate")
	}
	popEnabled()
	requestEgressIndexSettingsSnapshot.Store(&egressIndexSettingsSnapshot{settings: enabled, loadTime: time.Now().Add(-2 * time.Minute)})
	if requestEgressIndexSettings().NativeReaderEnabled || !enabled.NativeReaderEnabled {
		t.Fatal("rollback mutated a captured request snapshot or failed to disable")
	}
}
