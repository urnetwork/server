package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// sidecar.yml's reader (sidecar_config.go). Pure: an override or a config home
// in the test's own directory stands in for the file, and a fake clock moves the
// rereads.

func newTestSidecarConfigReader(load func(previous *SidecarConfig) *SidecarConfig) (*sidecarConfigReader, *time.Time) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	reader := newSidecarConfigReader(DefaultSidecarConfigSettings())
	reader.now = func() time.Time { return now }
	reader.load = load
	return reader, &now
}

// Points Config at a new, empty config home for the rest of the test.
func newTestSidecarConfigHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_CONFIG_HOME", home)
	return home
}

// Holds a read to what the zero value answers, the reader's closed direction: no
// network previewed, so a strict_exp mint is refused, no service gate, format 2
// or switch open, and no reseed or restore hold. Each network given is probed
// beside one no file names.
func requireSidecarConfigOpensNothing(t *testing.T, name string, config *SidecarConfig, networkIds ...Id) {
	t.Helper()
	probeIds := append([]Id{NewId()}, networkIds...)
	if 0 < len(config.previewNetworkIds) {
		t.Errorf("%s: previews %d networks", name, len(config.previewNetworkIds))
	}
	for _, networkId := range probeIds {
		if config.PreviewNetwork(networkId) {
			t.Errorf("%s: previews %s", name, networkId)
		}
	}
	for _, service := range []string{SidecarGateEvs, SidecarGateGkm, SidecarGateSockets, SidecarGateTlsedge, SidecarGateMxedge, SidecarGateSmtpedge, SidecarGateDns} {
		gate := config.Service(service)
		if gate.Enabled() {
			t.Errorf("%s: %s is enabled", name, service)
		}
		for _, networkId := range probeIds {
			if gate.Admits(networkId) {
				t.Errorf("%s: %s admits %s", name, service, networkId)
			}
		}
	}
	format2 := config.Format2()
	if format2.Gate().Enabled() || format2.Purge() != SidecarFormat2Observe || format2.Drop() != SidecarFormat2Observe {
		t.Errorf("%s: format2 is enabled or deletes", name)
	}
	for _, networkId := range probeIds {
		if format2.Admits(networkId) {
			t.Errorf("%s: format2 admits %s", name, networkId)
		}
	}
	for _, switchName := range []string{SidecarSwitchPush, SidecarSwitchWake, SidecarSwitchBridge, SidecarSwitchEscrowDrawOrder, SidecarSwitchAclContractGate, SidecarSwitchCerts} {
		if config.Switch(switchName) {
			t.Errorf("%s: %s is on", name, switchName)
		}
	}
	if config.Reseed() || config.RestoreHold() {
		t.Errorf("%s: reseed %t, restore_hold %t", name, config.Reseed(), config.RestoreHold())
	}
}

// Absent, the file is inert: no network is previewed, so a strict_exp mint is
// refused, and no gate opens. A home that is not there is absent too.
func TestSidecarConfigIsInertWhenAbsent(t *testing.T) {
	networkId := NewId()
	home := newTestSidecarConfigHome(t)
	requireSidecarConfigOpensNothing(t, "an absent file", loadSidecarConfig(nil), networkId)

	// the answer came from this home: a file put there is read
	text := fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)
	if err := os.WriteFile(filepath.Join(home, sidecarConfigResourceName), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if !loadSidecarConfig(nil).PreviewNetwork(networkId) {
		t.Fatal("the test's config home is not the one read")
	}

	t.Setenv("WARP_CONFIG_HOME", filepath.Join(t.TempDir(), "absent"))
	requireSidecarConfigOpensNothing(t, "a file in a home that is not there", loadSidecarConfig(nil), networkId)
}

// A file that is there but cannot be read is not an absent one, though a first
// read answers alike: it previews nothing and opens no gate, and after a good
// read it keeps that read. The same holds when what cannot be read is the home
// the file would be in. Only the first read says it failed (QD-1079).
func TestSidecarConfigFailsClosedWhenUnavailable(t *testing.T) {
	networkId := NewId()
	for _, c := range []struct {
		name  string
		place func(home string) error
	}{
		{name: "a dangling link", place: func(home string) error {
			return os.Symlink(filepath.Join(home, "missing.yml"), filepath.Join(home, sidecarConfigResourceName))
		}},
		{name: "a directory", place: func(home string) error {
			return os.Mkdir(filepath.Join(home, sidecarConfigResourceName), 0o700)
		}},
		{name: "a home that is a dangling link", place: func(home string) error {
			if err := os.Remove(home); err != nil {
				return err
			}
			return os.Symlink(home+".missing", home)
		}},
		{name: "a home that is a file", place: func(home string) error {
			if err := os.Remove(home); err != nil {
				return err
			}
			return os.WriteFile(home, []byte("synthetic: true\n"), 0o600)
		}},
	} {
		home := newTestSidecarConfigHome(t)
		if err := c.place(home); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// the case is what it says: the resolver cannot prove the file absent
		if _, err := Config.SimpleResource(sidecarConfigResourceName); !errors.Is(err, ErrResourceUnavailable) {
			t.Fatalf("%s: the resolver answers %v, want an unavailable resource", c.name, err)
		}

		firstRead := loadSidecarConfig(nil)
		requireSidecarConfigOpensNothing(t, "a first read of "+c.name, firstRead, networkId)
		if !firstRead.FailedFirstRead() {
			t.Errorf("%s: the first read does not say it failed", c.name)
		}
		// a good read, as newSidecarConfig makes one
		previous := &SidecarConfig{
			parsed:            true,
			previewNetworkIds: map[Id]bool{networkId: true},
		}
		if config := loadSidecarConfig(previous); config != previous || config.FailedFirstRead() {
			t.Errorf("%s: a later read replaced the latest good read", c.name)
		}
	}
}

// §19.11's shape, with keys this reader does not read beside the ones it does,
// the retired p0_enforce among them.
func TestSidecarConfigReadsItsGates(t *testing.T) {
	networkId := NewId()
	defer Testing_PushSidecarConfig(fmt.Sprintf(`
sidecar:
  evs: {enabled: false, writes: true, networks: [], percent: 0}
  reseed: false
  place_new: meta-1
  p0_enforce: {enabled: true}
  revocation_record_enabled: false
  preview_networks: [%q, "not-an-id"]
`, networkId))()

	config := CurrentSidecarConfig()
	if !config.PreviewNetwork(networkId) {
		t.Fatal("the listed network was not read")
	}
	if config.PreviewNetwork(NewId()) {
		t.Fatal("an unlisted network previews")
	}
	if config.PlaceNew() != "meta-1" {
		t.Fatalf("place_new = %q, want meta-1", config.PlaceNew())
	}
}

// p0_enforce is retired (owner decision 510, QD-922): §7.4's stage 1 and
// D-TEAM-14 refuse in code since M1. A file an old deployment still carries with
// the line reads exactly as the same file without it, whatever the line's value,
// one the old reader could not decode included, so the line neither opens,
// closes nor breaks anything.
func TestSidecarConfigReadsTheRetiredP0EnforceAsAbsent(t *testing.T) {
	networkId := NewId()
	without := fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n  evs: {enabled: true, networks: [%q]}\n  push: {enabled: true}\n", networkId, networkId)
	want, err := parseSidecarConfig([]byte(without))
	if err != nil {
		t.Fatal(err)
	}
	if !want.PreviewNetwork(networkId) || !want.Service(SidecarGateEvs).Admits(networkId) || !want.Push() {
		t.Fatal("the file without the line did not open what it names")
	}
	for _, line := range []string{
		"  p0_enforce: {enabled: false}\n",
		"  p0_enforce: {enabled: true}\n",
		"  p0_enforce: {enabled: fifty}\n",
		"  p0_enforce: true\n",
	} {
		with := without + line
		got, err := parseSidecarConfig([]byte(with))
		if err != nil {
			t.Errorf("%q: %s", line, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: the file reads otherwise than without the line", line)
		}
		// and as a process reads it, through Config
		func() {
			defer Testing_PushSidecarConfig(with)()
			if !reflect.DeepEqual(CurrentSidecarConfig(), want) {
				t.Errorf("%q: the read file differs from the file without the line", line)
			}
		}()
	}
}

// A file without a gate's key leaves the gate off, and without the block too. The
// retired p0_enforce, on or off, is no gate.
func TestSidecarConfigDefaultsItsGatesOff(t *testing.T) {
	networkId := NewId()
	for _, yaml := range []string{
		"",
		"other: true\n",
		"sidecar: {}\n",
		"sidecar:\n  p0_enforce: {enabled: false}\n",
		"sidecar:\n  p0_enforce: {enabled: true}\n",
	} {
		func() {
			defer Testing_PushSidecarConfig(yaml)()
			requireSidecarConfigOpensNothing(t, fmt.Sprintf("%q", yaml), CurrentSidecarConfig(), networkId)
		}()
	}
}

// The file is read again once a read is older than the interval, and not before:
// a gate moves within §19.11's 60 s of a config push without a read per request.
func TestSidecarConfigRereadsAfterItsInterval(t *testing.T) {
	networkId := NewId()
	reads := 0
	preview := false
	reader, now := newTestSidecarConfigReader(func(previous *SidecarConfig) *SidecarConfig {
		reads += 1
		return &SidecarConfig{previewNetworkIds: map[Id]bool{networkId: preview}}
	})

	if reader.get().PreviewNetwork(networkId) {
		t.Fatal("first read previews")
	}
	preview = true
	*now = now.Add(59 * time.Second)
	if reader.get().PreviewNetwork(networkId) || reads != 1 {
		t.Fatalf("inside the interval: preview = %t, reads = %d", reader.get().PreviewNetwork(networkId), reads)
	}
	*now = now.Add(time.Second)
	if !reader.get().PreviewNetwork(networkId) || reads != 2 {
		t.Fatalf("at the interval: reads = %d", reads)
	}
}

// One caller rereads while the others keep the latest read, so a slow read holds
// up no request.
func TestSidecarConfigRereadBlocksNoOtherCaller(t *testing.T) {
	networkId := NewId()
	entered := make(chan struct{})
	release := make(chan struct{})
	first := true
	reader, now := newTestSidecarConfigReader(func(previous *SidecarConfig) *SidecarConfig {
		if first {
			first = false
			return &SidecarConfig{}
		}
		close(entered)
		<-release
		return &SidecarConfig{previewNetworkIds: map[Id]bool{networkId: true}}
	})
	reader.get()
	*now = now.Add(time.Minute)

	done := make(chan *SidecarConfig)
	go func() {
		done <- reader.get()
	}()
	<-entered
	if reader.get().PreviewNetwork(networkId) {
		t.Fatal("a second caller did not keep the latest read")
	}
	close(release)
	if !(<-done).PreviewNetwork(networkId) {
		t.Fatal("the rereading caller did not get its read")
	}
	if !reader.get().PreviewNetwork(networkId) {
		t.Fatal("the reread was not kept")
	}
}

// A bad push must not preview a network or open a gate: once a read succeeded a
// failed one keeps it, and a first read of a broken file answers the zero value,
// never what it partly parsed. The second broken file fails on its last gate's
// percent, after it names the network and turns push on, which a reader that kept
// its partial decoding would open.
func TestSidecarConfigFailsClosed(t *testing.T) {
	networkId := NewId()
	good := fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)
	newTestSidecarConfigHome(t)

	for _, broken := range []string{
		"sidecar: [unterminated\n",
		fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n  push: {enabled: true}\n  evs: {enabled: true, networks: [%q], percent: fifty}\n", networkId, networkId),
	} {
		if _, err := parseSidecarConfig([]byte(broken)); err == nil {
			t.Fatalf("%q parsed", broken)
		}
		func() {
			defer Testing_PushSidecarConfig(broken)()
			requireSidecarConfigOpensNothing(t, fmt.Sprintf("a first read of %q", broken), loadSidecarConfig(nil), networkId)
		}()

		var previous *SidecarConfig
		func() {
			defer Testing_PushSidecarConfig(good)()
			previous = loadSidecarConfig(nil)
		}()
		if !previous.PreviewNetwork(networkId) {
			t.Fatal("the good file was not read")
		}
		func() {
			defer Testing_PushSidecarConfig(broken)()
			if config := loadSidecarConfig(previous); config != previous {
				t.Fatalf("%q replaced the latest good read", broken)
			}
		}()
		// with no file at all the latest read holds too: only an edit moves a gate
		if config := loadSidecarConfig(previous); config != previous {
			t.Fatal("an absent file replaced the latest good read")
		}
	}
}

// A first read of a file that is there but cannot be read or parsed answers the
// zero value and says so (QD-1079), even when the part it could parse names an
// urnetwork_edge, so a reader whose zero value is the destructive direction can
// hold off. A second failed read keeps that read. A first read of an absent file,
// or of a good one, is no failed read.
func TestSidecarConfigMarksAFailedFirstRead(t *testing.T) {
	edgeNetworkId := NewId()
	home := newTestSidecarConfigHome(t)

	if loadSidecarConfig(nil).FailedFirstRead() {
		t.Fatal("a first read of an absent file is marked")
	}
	func() {
		defer Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n", edgeNetworkId))()
		config := loadSidecarConfig(nil)
		if config.FailedFirstRead() {
			t.Fatal("a first good read is marked")
		}
		if networkId, ok := config.UrnetworkEdge(); !ok || networkId != edgeNetworkId {
			t.Fatal("the good file's urnetwork_edge was not read")
		}
	}()

	requireFailedFirstRead := func(name string, config *SidecarConfig) {
		t.Helper()
		if !config.FailedFirstRead() {
			t.Fatalf("%s: not marked", name)
		}
		if _, ok := config.UrnetworkEdge(); ok {
			t.Fatalf("%s: names an urnetwork_edge", name)
		}
		requireSidecarConfigOpensNothing(t, name, config, edgeNetworkId)
		if loadSidecarConfig(config) != config {
			t.Fatalf("%s: a second failed read replaced the first", name)
		}
	}
	for _, broken := range []string{
		"sidecar: [unterminated\n",
		// names the edge before a later key fails, which a reader keeping its
		// partial decoding would answer
		fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n  evs: {enabled: true, percent: fifty}\n", edgeNetworkId),
	} {
		func() {
			defer Testing_PushSidecarConfig(broken)()
			requireFailedFirstRead(fmt.Sprintf("a first read of %q", broken), loadSidecarConfig(nil))
		}()
	}
	if err := os.Mkdir(filepath.Join(home, sidecarConfigResourceName), 0o700); err != nil {
		t.Fatal(err)
	}
	requireFailedFirstRead("a first read of a directory", loadSidecarConfig(nil))
}

// The first good read replaces a failed first read, and with it the mark: the
// file is known now, and its urnetwork_edge is what a holder reads.
func TestSidecarConfigGoodReadClearsAFailedFirstRead(t *testing.T) {
	edgeNetworkId := NewId()
	newTestSidecarConfigHome(t)

	var failed *SidecarConfig
	func() {
		defer Testing_PushSidecarConfig("sidecar: [unterminated\n")()
		failed = loadSidecarConfig(nil)
	}()
	if !failed.FailedFirstRead() {
		t.Fatal("the broken first read is not marked")
	}
	func() {
		defer Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n", edgeNetworkId))()
		config := loadSidecarConfig(failed)
		if config == failed || config.FailedFirstRead() {
			t.Fatal("the good read kept the failed first read")
		}
		if networkId, ok := config.UrnetworkEdge(); !ok || networkId != edgeNetworkId {
			t.Fatal("the good read's urnetwork_edge was not read")
		}
	}()
}

// After a good read a failed read keeps it, as it did before QD-1079, unmarked:
// the good read's urnetwork_edge stands, so that network stays out of the idle
// marking. An absent read between them changes nothing.
func TestSidecarConfigFailedReadKeepsTheGoodReadUnmarked(t *testing.T) {
	edgeNetworkId := NewId()
	newTestSidecarConfigHome(t)

	var good *SidecarConfig
	func() {
		defer Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n", edgeNetworkId))()
		good = loadSidecarConfig(nil)
	}()
	if loadSidecarConfig(good) != good {
		t.Fatal("an absent file replaced the good read")
	}
	func() {
		defer Testing_PushSidecarConfig("sidecar: [unterminated\n")()
		config := loadSidecarConfig(good)
		if config != good || config.FailedFirstRead() {
			t.Fatal("a failed read after a good one did not keep it unmarked")
		}
		if networkId, ok := config.UrnetworkEdge(); !ok || networkId != edgeNetworkId {
			t.Fatal("the kept read lost its urnetwork_edge")
		}
	}()
}

// An absent file is no failed read (QD-1079): after a failed first read it
// answers the zero value unmarked, since the environment then names no edge
// network to protect. A file that breaks after an absent read has no good read
// before it, so its read is marked.
func TestSidecarConfigAbsentFileIsNoFailedRead(t *testing.T) {
	newTestSidecarConfigHome(t)

	absent := loadSidecarConfig(nil)
	if absent.FailedFirstRead() {
		t.Fatal("an absent file's read is marked")
	}
	if loadSidecarConfig(absent) != absent {
		t.Fatal("a second absent read replaced the first")
	}

	var failed *SidecarConfig
	func() {
		defer Testing_PushSidecarConfig("sidecar: [unterminated\n")()
		failed = loadSidecarConfig(absent)
	}()
	if !failed.FailedFirstRead() {
		t.Fatal("a broken file after an absent one is not marked")
	}
	requireSidecarConfigOpensNothing(t, "a broken file after an absent one", failed)

	config := loadSidecarConfig(failed)
	if config.FailedFirstRead() {
		t.Fatal("an absent file after a failed first read kept the mark")
	}
	requireSidecarConfigOpensNothing(t, "an absent file after a failed first read", config)
}

// The process's reader passes its latest read on as previous, so the mark
// follows the reads a config push makes under a running process, which
// Testing_RereadSidecarConfig stands in for: a broken file keeps a good read,
// a good file clears a failed first read, and the pop forgets every read.
func TestSidecarConfigRereadPassesTheLatestRead(t *testing.T) {
	edgeNetworkId := NewId()
	good := fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n", edgeNetworkId)
	broken := "sidecar: [unterminated\n"
	newTestSidecarConfigHome(t)

	func() {
		defer Testing_PushSidecarConfig(good)()
		goodRead := CurrentSidecarConfig()
		func() {
			defer Testing_RereadSidecarConfig(broken)()
			if CurrentSidecarConfig() != goodRead {
				t.Fatal("a reread of a broken file did not keep the good read")
			}
		}()
		if config := CurrentSidecarConfig(); config == goodRead || config.FailedFirstRead() {
			t.Fatal("the pop did not forget the reads")
		}
	}()

	func() {
		defer Testing_PushSidecarConfig(broken)()
		if !CurrentSidecarConfig().FailedFirstRead() {
			t.Fatal("a broken first read is not marked")
		}
		func() {
			defer Testing_RereadSidecarConfig(good)()
			config := CurrentSidecarConfig()
			if config.FailedFirstRead() {
				t.Fatal("the first good read kept the mark")
			}
			if networkId, ok := config.UrnetworkEdge(); !ok || networkId != edgeNetworkId {
				t.Fatal("the first good read's urnetwork_edge was not read")
			}
		}()
	}()

	Testing_ForgetSidecarConfig()
	if config := CurrentSidecarConfig(); config.FailedFirstRead() {
		t.Fatal("a first read of the absent file is marked")
	}
}
