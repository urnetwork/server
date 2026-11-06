package server

import (
	"fmt"
	"testing"
	"time"
)

// sidecar.yml's reader (sidecar_config.go). Pure: an override stands in for the
// file, and a fake clock moves the rereads.

func newTestSidecarConfigReader(load func(previous *SidecarConfig) *SidecarConfig) (*sidecarConfigReader, *time.Time) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	reader := newSidecarConfigReader(DefaultSidecarConfigSettings())
	reader.now = func() time.Time { return now }
	reader.load = load
	return reader, &now
}

// Absent, the file is inert: nothing enforces and nothing is previewed. Stage 1
// is log-only, and a strict_exp mint is refused.
func TestSidecarConfigIsInertWhenAbsent(t *testing.T) {
	config := loadSidecarConfig(nil)
	if config.P0Enforce {
		t.Fatal("an absent file enforces")
	}
	if config.PreviewNetwork(NewId()) {
		t.Fatal("an absent file previews a network")
	}
}

// §19.11's shape, with keys this reader does not read beside the ones it does.
func TestSidecarConfigReadsItsGates(t *testing.T) {
	networkId := NewId()
	defer Testing_PushSidecarConfig(fmt.Sprintf(`
sidecar:
  evs: {enabled: false, writes: true, networks: [], percent: 0}
  reseed: false
  place_new: meta-1
  p0_enforce: {enabled: true}
  preview_networks: [%q, "not-an-id"]
`, networkId))()

	config := CurrentSidecarConfig()
	if !config.P0Enforce {
		t.Fatal("p0_enforce was not read")
	}
	if !config.PreviewNetwork(networkId) {
		t.Fatal("the listed network was not read")
	}
	if config.PreviewNetwork(NewId()) {
		t.Fatal("an unlisted network previews")
	}
}

// A file without the key leaves the gate off, and without the block too.
func TestSidecarConfigDefaultsItsGatesOff(t *testing.T) {
	for _, yaml := range []string{"", "other: true\n", "sidecar: {}\n", "sidecar:\n  p0_enforce: {enabled: false}\n"} {
		func() {
			defer Testing_PushSidecarConfig(yaml)()
			if CurrentSidecarConfig().P0Enforce {
				t.Fatalf("%q enforces", yaml)
			}
		}()
	}
}

// The file is read again once a read is older than the interval, and not before:
// a gate moves within §19.11's 60 s of a config push without a read per request.
func TestSidecarConfigRereadsAfterItsInterval(t *testing.T) {
	reads := 0
	enforce := false
	reader, now := newTestSidecarConfigReader(func(previous *SidecarConfig) *SidecarConfig {
		reads += 1
		return &SidecarConfig{P0Enforce: enforce}
	})

	if reader.get().P0Enforce {
		t.Fatal("first read enforces")
	}
	enforce = true
	*now = now.Add(59 * time.Second)
	if reader.get().P0Enforce || reads != 1 {
		t.Fatalf("inside the interval: enforce = %t, reads = %d", reader.get().P0Enforce, reads)
	}
	*now = now.Add(time.Second)
	if !reader.get().P0Enforce || reads != 2 {
		t.Fatalf("at the interval: reads = %d", reads)
	}
}

// One caller rereads while the others keep the latest read, so a slow read holds
// up no request.
func TestSidecarConfigRereadBlocksNoOtherCaller(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	first := true
	reader, now := newTestSidecarConfigReader(func(previous *SidecarConfig) *SidecarConfig {
		if first {
			first = false
			return &SidecarConfig{P0Enforce: false}
		}
		close(entered)
		<-release
		return &SidecarConfig{P0Enforce: true}
	})
	reader.get()
	*now = now.Add(time.Minute)

	done := make(chan *SidecarConfig)
	go func() {
		done <- reader.get()
	}()
	<-entered
	if reader.get().P0Enforce {
		t.Fatal("a second caller did not keep the latest read")
	}
	close(release)
	if !(<-done).P0Enforce {
		t.Fatal("the rereading caller did not get its read")
	}
	if !reader.get().P0Enforce {
		t.Fatal("the reread was not kept")
	}
}

// A bad push must not drop enforcement: once a read succeeded a failed one keeps
// it, and a first read of a broken file enforces and previews nothing.
func TestSidecarConfigFailsClosed(t *testing.T) {
	networkId := NewId()
	good := fmt.Sprintf("sidecar:\n  p0_enforce: {enabled: true}\n  preview_networks: [%q]\n", networkId)
	const broken = "sidecar: [unterminated\n"

	func() {
		defer Testing_PushSidecarConfig(broken)()
		config := loadSidecarConfig(nil)
		if !config.P0Enforce {
			t.Fatal("a first read of a broken file does not enforce")
		}
		if config.PreviewNetwork(networkId) {
			t.Fatal("a broken file previews a network")
		}
	}()

	var previous *SidecarConfig
	func() {
		defer Testing_PushSidecarConfig(good)()
		previous = loadSidecarConfig(nil)
	}()
	func() {
		defer Testing_PushSidecarConfig(broken)()
		if config := loadSidecarConfig(previous); config != previous {
			t.Fatal("a broken file replaced the latest good read")
		}
	}()
	// with no file at all the latest read holds too: only an edit moves a gate
	if config := loadSidecarConfig(previous); config != previous {
		t.Fatal("an absent file replaced the latest good read")
	}
}
