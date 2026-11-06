package server

import (
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"
)

// The sidecar gates, config/<env>/sidecar.yml (§19.11, D-OPS-10). The file is
// optional and inert when absent, like onboarding.yml, and services reread it,
// so a gate changes with a config push and no deploy:
//
//	sidecar:
//	  p0_enforce: {enabled: false}  # §7.4 stage 1 and D-TEAM-14: off, each counts what it would refuse
//	  preview_networks: []          # §7.4, D-PHASE-3: networks that hold strict_exp clients before stage 2
//
// What this reader parses are the gates server code reads so far; the rest of
// §19.11's file is ignored until its readers land.
//
// A gate that loosens when the file breaks would drop P0's enforcement on a bad
// push. So once a read succeeded, a failed or absent read keeps it, and a file
// that is there but does not parse on a process's first read enforces and
// previews nothing. Taking a gate back off is an edit to the file, not its
// removal: a removal holds until the process restarts.

// The file's name under config/<env>.
const sidecarConfigResourceName = "sidecar.yml"

// Settings of the reader.
type SidecarConfigSettings struct {
	// How long one read answers before the next. §19.11 fixes 60 s for every
	// service, so a gate moves everywhere within a minute of a config push.
	// Non-positive rereads on every call.
	RereadInterval time.Duration
}

// §19.11's interval.
func DefaultSidecarConfigSettings() *SidecarConfigSettings {
	return &SidecarConfigSettings{
		RereadInterval: 60 * time.Second,
	}
}

// The keys this reader reads.
type sidecarConfigYaml struct {
	Sidecar struct {
		P0Enforce struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"p0_enforce"`
		PreviewNetworks []string `yaml:"preview_networks"`
	} `yaml:"sidecar"`
}

// One read of sidecar.yml. Immutable once read.
type SidecarConfig struct {
	// §7.4's stage 1 and D-TEAM-14 refuse what they otherwise only count. M1
	// turns this on.
	P0Enforce bool
	// the networks that may hold strict_exp clients before stage 2 (D-TRUST-3)
	previewNetworkIds map[Id]bool
}

// Whether the network is listed in preview_networks.
func (self *SidecarConfig) PreviewNetwork(networkId Id) bool {
	return self.previewNetworkIds[networkId]
}

// One bad preview entry costs only that entry.
func newSidecarConfig(y *sidecarConfigYaml) *SidecarConfig {
	config := &SidecarConfig{
		P0Enforce:         y.Sidecar.P0Enforce.Enabled,
		previewNetworkIds: map[Id]bool{},
	}
	for _, networkIdStr := range y.Sidecar.PreviewNetworks {
		networkId, err := ParseId(networkIdStr)
		if err != nil {
			// one bad entry admits nothing and costs no other network its entry
			glog.Errorf("[sidecar]%s lists a preview network that is not an id\n", sidecarConfigResourceName)
			continue
		}
		config.previewNetworkIds[networkId] = true
	}
	return config
}

// Reads the file, keeping previous when the read fails. See the file's header
// for what a failure answers.
func loadSidecarConfig(previous *SidecarConfig) (config *SidecarConfig) {
	failed := func(err any) *SidecarConfig {
		glog.Errorf("[sidecar]could not read %s: %v\n", sidecarConfigResourceName, err)
		if previous != nil {
			return previous
		}
		return &SidecarConfig{P0Enforce: true}
	}
	if r := HandleError(func() {
		resource, err := Config.SimpleResource(sidecarConfigResourceName)
		if err != nil {
			config = previous
			if config == nil {
				config = &SidecarConfig{}
			}
			return
		}
		y := &sidecarConfigYaml{}
		if err := resource.UnmarshalYamlE(y); err != nil {
			config = failed(err)
			return
		}
		config = newSidecarConfig(y)
	}); r != nil {
		config = failed(r)
	}
	return
}

// A read and when it was taken.
type sidecarConfigRead struct {
	config   *SidecarConfig
	readTime time.Time
}

// Answers from the latest read and rereads once it is older than the settings
// allow. One caller rereads while the rest keep the latest read, so a reread
// never queues requests. Safe for concurrent use.
type sidecarConfigReader struct {
	settings  *SidecarConfigSettings
	now       func() time.Time
	load      func(previous *SidecarConfig) *SidecarConfig
	rereading atomic.Bool
	latest    atomic.Pointer[sidecarConfigRead]
}

// A reader of the file on the wall clock.
func newSidecarConfigReader(settings *SidecarConfigSettings) *sidecarConfigReader {
	return &sidecarConfigReader{
		settings: settings,
		now:      time.Now,
		load:     loadSidecarConfig,
	}
}

// The latest read, taken again first when it is stale and no other caller is
// taking it.
func (self *sidecarConfigReader) get() *SidecarConfig {
	now := self.now()
	read := self.latest.Load()
	if read != nil && now.Sub(read.readTime) < self.settings.RereadInterval {
		return read.config
	}
	if self.rereading.CompareAndSwap(false, true) {
		defer self.rereading.Store(false)
	} else if read != nil {
		return read.config
	}
	var previous *SidecarConfig
	if read != nil {
		previous = read.config
	}
	config := self.load(previous)
	self.latest.Store(&sidecarConfigRead{config: config, readTime: now})
	return config
}

// The process's reader. The file is the process's, so unlike a budget it has one
// owner per process.
var defaultSidecarConfigReader = newSidecarConfigReader(DefaultSidecarConfigSettings())

// The current sidecar.yml.
func CurrentSidecarConfig() *SidecarConfig {
	return defaultSidecarConfigReader.get()
}

// Stands in for sidecar.yml until the returned pop. The next read answers with it
// at once, and the pop forgets it as quickly.
func Testing_PushSidecarConfig(yaml string) func() {
	pop := Config.PushSimpleResource(sidecarConfigResourceName, []byte(yaml))
	defaultSidecarConfigReader.latest.Store(nil)
	return func() {
		pop()
		defaultSidecarConfigReader.latest.Store(nil)
	}
}
