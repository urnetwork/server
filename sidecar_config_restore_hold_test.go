package server

import (
	"testing"
)

// sidecar.yml's restore_hold (plan-OPS-restore-hold, QD-076; OPS-M3-03): read where the file sets it, off where it
// does not, and kept through a broken or absent file once read, as every gate is.
func TestSidecarConfigReadsTheRestoreHold(t *testing.T) {
	func() {
		defer Testing_PushSidecarConfig("sidecar:\n  restore_hold: true\n")()
		if !CurrentSidecarConfig().RestoreHold() {
			t.Fatal("restore_hold: true was not read")
		}
	}()
	for _, yaml := range []string{"", "sidecar: {}\n", "sidecar:\n  restore_hold: false\n"} {
		func() {
			defer Testing_PushSidecarConfig(yaml)()
			if CurrentSidecarConfig().RestoreHold() {
				t.Fatalf("%q holds", yaml)
			}
		}()
	}

	const broken = "sidecar: [unterminated\n"
	var held *SidecarConfig
	func() {
		defer Testing_PushSidecarConfig("sidecar:\n  restore_hold: true\n")()
		held = loadSidecarConfig(nil)
	}()
	func() {
		defer Testing_PushSidecarConfig(broken)()
		if !loadSidecarConfig(held).RestoreHold() {
			t.Fatal("a broken file dropped the hold the latest read held")
		}
		if loadSidecarConfig(nil).RestoreHold() {
			t.Fatal("a first read of a broken file holds every route")
		}
	}()
}
