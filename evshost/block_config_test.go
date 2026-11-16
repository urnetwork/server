package evshost

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A fake ServiceHost for a host a test builds itself.
func newHostTestServiceHost(t testing.TB) *sidecar.ServiceHost {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	return sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil).ServiceHost()
}

// Every seam a host is ready with.
func hostTestDeps(t testing.TB) *HostDeps {
	t.Helper()
	return &HostDeps{
		AuthState:   hostTestAuthState{},
		GateViews:   hostTestGateViews{},
		ReceiptKeys: hostTestReceiptKeys(t, sidecartest.DefaultClockStart()),
	}
}

// The block config's default is this code's own format, EvsSettings' default, which is format 2 (QD-854), and a file
// that names no format reads as it.
func TestBlockConfigDefaultsToThisCodesFormat(t *testing.T) {
	if format := DefaultBlockConfig().InstanceFormat; format != sidecar.EvsRepoFormat2 || format != evsservice.DefaultEvsSettings().InstanceFormat {
		t.Fatalf("the default instance format is %d, the engine's %d", format, evsservice.DefaultEvsSettings().InstanceFormat)
	}
	for _, file := range []string{
		"",
		"\n",
		"# the storage format this block runs as\n",
		"{}\n",
		"---\n",
	} {
		blockConfig, err := ParseBlockConfig([]byte(file))
		if err != nil {
			t.Errorf("%q: %v", file, err)
			continue
		}
		if blockConfig.InstanceFormat != sidecar.EvsRepoFormat2 {
			t.Errorf("%q reads instance format %d", file, blockConfig.InstanceFormat)
		}
	}
}

// A file names format 1, a rollback's, or format 2.
func TestBlockConfigTakesFormats1And2(t *testing.T) {
	for _, c := range []struct {
		file   string
		format uint32
	}{
		{file: "instance_format: 1\n", format: sidecar.EvsRepoFormat1},
		{file: "instance_format: 2\n", format: sidecar.EvsRepoFormat2},
		{file: "# a rollback\ninstance_format: 1 # until the roll forward\n", format: sidecar.EvsRepoFormat1},
		{file: "{instance_format: 2}\n", format: sidecar.EvsRepoFormat2},
		{file: "format: &format 1\ninstance_format: *format\n", format: 0},
	} {
		blockConfig, err := ParseBlockConfig([]byte(c.file))
		if c.format == 0 {
			// an anchor needs a key of its own, which this code does not know
			if err == nil {
				t.Errorf("%q read as %+v", c.file, blockConfig)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.file, err)
			continue
		}
		if blockConfig.InstanceFormat != c.format {
			t.Errorf("%q reads instance format %d, want %d", c.file, blockConfig.InstanceFormat, c.format)
		}
	}
}

// Anything else is refused, never read as the default: a format this code does not know, a value that is not a whole
// number, a key it does not know, and a file that does not parse.
func TestBlockConfigRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, file := range []string{
		"instance_format: 0\n",
		"instance_format: 3\n",
		"instance_format: -1\n",
		"instance_format: 4294967297\n",
		"instance_format: 1.0\n",
		"instance_format: 1.5\n",
		"instance_format: \"1\"\n",
		"instance_format: one\n",
		"instance_format: true\n",
		"instance_format:\n",
		"instance_format: ~\n",
		"instance_format: [1]\n",
		"instance_format: {format: 1}\n",
		"instance-format: 1\n",
		"InstanceFormat: 1\n",
		"instance_format: 2\nformat: 1\n",
		"instance_format: 1\ninstance_format: 2\n",
		"instance_format: 2\n---\ninstance_format: 1\n",
		"instance_format: [1\n",
		"- instance_format: 1\n",
		"1\n",
	} {
		blockConfig, err := ParseBlockConfig([]byte(file))
		if err == nil {
			t.Errorf("%q read as instance format %d", file, blockConfig.InstanceFormat)
			continue
		}
		if !errors.Is(err, ErrBlockConfig) {
			t.Errorf("%q: %v is no ErrBlockConfig", file, err)
		}
	}
}

// The config is the block's own file under config/<env>: absent it is the default, and a file there is read, with
// what cannot be read as a file refused.
func TestLoadBlockConfigReadsTheBlocksFile(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_CONFIG_HOME", config)
	blockConfig, err := LoadBlockConfig()
	if err != nil || blockConfig.InstanceFormat != sidecar.EvsRepoFormat2 {
		t.Fatalf("with no file: %+v, %v", blockConfig, err)
	}
	path := filepath.Join(config, BlockConfigFileName)
	if err := os.WriteFile(path, []byte("instance_format: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blockConfig, err = LoadBlockConfig()
	if err != nil || blockConfig.InstanceFormat != sidecar.EvsRepoFormat1 {
		t.Fatalf("a rollback's file: %+v, %v", blockConfig, err)
	}
	if err := os.WriteFile(path, []byte("instance_format: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if blockConfig, err = LoadBlockConfig(); !errors.Is(err, ErrBlockConfig) {
		t.Fatalf("an unknown format: %+v, %v", blockConfig, err)
	}
	// a directory where the file belongs is no absent file
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if blockConfig, err = LoadBlockConfig(); !errors.Is(err, ErrBlockConfig) {
		t.Fatalf("a directory for the file: %+v, %v", blockConfig, err)
	}
}

// A test's file stands in for the block's config only for the run it wraps.
func TestTestingWithBlockConfigHoldsForItsRun(t *testing.T) {
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	Testing_WithBlockConfig("instance_format: 1\n", func() {
		if blockConfig, err := LoadBlockConfig(); err != nil || blockConfig.InstanceFormat != sidecar.EvsRepoFormat1 {
			t.Fatalf("inside: %+v, %v", blockConfig, err)
		}
	})
	if blockConfig, err := LoadBlockConfig(); err != nil || blockConfig.InstanceFormat != sidecar.EvsRepoFormat2 {
		t.Fatalf("after: %+v, %v", blockConfig, err)
	}
}

// The engine's settings under a config carry its format whatever the caller's held, and leave the caller's alone.
func TestBlockConfigSetsTheEnginesFormat(t *testing.T) {
	blockConfig := &BlockConfig{InstanceFormat: sidecar.EvsRepoFormat1}
	if evsSettings := blockConfig.EvsSettings(nil); evsSettings.InstanceFormat != sidecar.EvsRepoFormat1 || evsSettings.Validate() != nil {
		t.Fatalf("the defaults under format 1: format %d, %v", evsSettings.InstanceFormat, evsSettings.Validate())
	}
	settings := evsservice.DefaultEvsSettings()
	settings.RepoCacheCount = 7
	evsSettings := blockConfig.EvsSettings(settings)
	if evsSettings.InstanceFormat != sidecar.EvsRepoFormat1 || evsSettings.RepoCacheCount != 7 {
		t.Fatalf("format %d, cache %d", evsSettings.InstanceFormat, evsSettings.RepoCacheCount)
	}
	if settings.InstanceFormat != sidecar.EvsRepoFormat2 {
		t.Fatalf("the caller's settings now hold format %d", settings.InstanceFormat)
	}
}

// A host runs as the format its block's config names: 2 with no file, 1 under a rollback's, and never the format its
// caller's settings held.
func TestHostRunsAsItsBlockConfigsFormat(t *testing.T) {
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	evsHost := newHostTestHost(t, nil)
	if evsHost.InstanceFormat() != sidecar.EvsRepoFormat2 || evsHost.Engine().Settings().InstanceFormat != sidecar.EvsRepoFormat2 {
		t.Fatalf("with no file: host %d, engine %d", evsHost.InstanceFormat(), evsHost.Engine().Settings().InstanceFormat)
	}
	Testing_WithBlockConfig("instance_format: 1\n", func() {
		evsHost = newHostTestHost(t, nil)
	})
	if evsHost.InstanceFormat() != sidecar.EvsRepoFormat1 || evsHost.Engine().Settings().InstanceFormat != sidecar.EvsRepoFormat1 {
		t.Fatalf("under a rollback's file: host %d, engine %d", evsHost.InstanceFormat(), evsHost.Engine().Settings().InstanceFormat)
	}
	if !evsHost.Ready() {
		t.Fatal("a format-1 block with every seam is not ready")
	}
	// the caller's settings do not choose the format, either way
	for _, c := range []struct {
		file     string
		settings uint32
		format   uint32
	}{
		{file: "instance_format: 2\n", settings: sidecar.EvsRepoFormat1, format: sidecar.EvsRepoFormat2},
		{file: "instance_format: 1\n", settings: sidecar.EvsRepoFormat2, format: sidecar.EvsRepoFormat1},
		{file: "", settings: sidecar.EvsRepoFormat1, format: sidecar.EvsRepoFormat2},
		{file: "instance_format: 1\n", settings: 0, format: sidecar.EvsRepoFormat1},
	} {
		hostSettings := DefaultHostSettings()
		hostSettings.Evs.InstanceFormat = c.settings
		host := newHostTestServiceHost(t)
		Testing_WithBlockConfig(c.file, func() {
			built, err := NewHost(t.Context(), host, hostSettings, hostTestDeps(t))
			if err != nil {
				t.Errorf("%q over settings' %d: %v", c.file, c.settings, err)
				return
			}
			defer built.Close()
			if built.InstanceFormat() != c.format || built.Engine().Settings().InstanceFormat != c.format {
				t.Errorf("%q over settings' %d: host %d, engine %d", c.file, c.settings, built.InstanceFormat(), built.Engine().Settings().InstanceFormat)
			}
		})
		if hostSettings.Evs.InstanceFormat != c.settings {
			t.Errorf("the caller's settings now hold format %d", hostSettings.Evs.InstanceFormat)
		}
	}
}

// A host whose block config names a format this code does not know, or does not parse, does not start.
func TestHostRefusesABlockConfigItDoesNotKnow(t *testing.T) {
	for _, file := range []string{
		"instance_format: 3\n",
		"instance_format: 0\n",
		"instance_format: one\n",
		"instance-format: 1\n",
		"instance_format: [1\n",
	} {
		host := newHostTestServiceHost(t)
		Testing_WithBlockConfig(file, func() {
			built, err := NewHost(t.Context(), host, nil, hostTestDeps(t))
			if err == nil {
				built.Close()
				t.Errorf("%q: a host started at format %d", file, built.InstanceFormat())
				return
			}
			if !errors.Is(err, ErrBlockConfig) {
				t.Errorf("%q: %v is no ErrBlockConfig", file, err)
			}
		})
	}
}

// A block whose config names a format this code does not know cannot start, whatever else it holds: its engine is
// never ready and serves no route, so /status says not ready and a deploy reverts. Nothing of it is built, so no
// store is read under a format its operator did not deploy.
func TestBlockWithAnUnknownInstanceFormatIsNeverReady(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", filepath.Join(home, "vault"))
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(home, "config"))
	clusters := newHostTestClusters(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()))
	clusters.ready.Store(true)
	deps := hostTestDeps(t)
	deps.Clusters = clusters
	Testing_WithBlockConfig("instance_format: 3\n", func() {
		engine := newBlockEngine(t.Context(), nil, deps)
		defer engine.Close()
		if _, ok := engine.(notReadyEngine); !ok {
			t.Fatalf("a block with an unknown instance format built %T", engine)
		}
		if engine.Ready() {
			t.Fatal("ready")
		}
		options := blockRunOptions(engine, hostTestStatusPort, deps)
		if options.Http != nil {
			t.Error("a block that cannot start serves routes")
		}
		if err := options.Validate(); err != nil {
			t.Error(err)
		}
	})
	if clusters.closed.Load() {
		t.Error("the block's clusters were taken before its config was read")
	}
}
