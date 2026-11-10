package server

import (
	"errors"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"
)

// The sidecar gates, config/<env>/sidecar.yml (§19.11, D-OPS-10). The file is
// optional and inert when absent, like onboarding.yml, and services reread it,
// so a gate changes with a config push and no deploy:
//
//	sidecar:
//	  evs: {enabled: false, writes: true, networks: [], percent: 0}
//	  format2: {enabled: false, networks: [], percent: 0, purge: observe, drop: observe}
//	  gkm: {enabled: false, networks: [], percent: 0}  # sockets, tlsedge, mxedge, smtpedge alike
//	  dns: {enabled: false, writes: true}
//	  push: {enabled: false}
//	  wake: {enabled: false}
//	  bridge: {enabled: false}
//	  escrow_draw_order: {enabled: false}
//	  reseed: false
//	  place_new: meta-1
//	  drained: []
//	  preview_networks: []          # §7.4, D-PHASE-3: networks that hold strict_exp clients before stage 2
//	  sockets_control_qps: {rate: 50, burst: 500}
//	  urnetwork_edge: <network_id>
//	  p0_enforce: {enabled: false}  # M0's log-only switch for §7.4 stage 1 and D-TEAM-14; inert since M1
//	  file_clusters: {<cluster id>: {settle_passed_build: <commit>, hdd_capacity_ops: <n>}}
//
// p0_enforce is not in §19.11's list, which picks it up at the next records pass
// (plan-OPS-small-readings #3), and file_clusters is plan-EVSB-settle-gate's.
// sidecar_gates.go holds what each gate answers.
//
// A gate that loosens when the file breaks would drop P0's enforcement, or open
// a service, on a bad push. So once a read succeeded, a failed or absent read
// keeps it, and on a process's first read a file that is there but cannot be
// read or parsed enforces P0, previews nothing and opens no gate. Only a file
// the resolver finds absent from every home is inert (ErrResourceNotFound): a
// dangling link, a directory or an inaccessible home is unavailable, not absent.
// Taking a gate back off is an edit to the file, not its removal: a removal
// holds until the process restarts. Within a file that parses, one bad entry
// costs only itself: an id that does not parse is skipped, a percent out of
// range reads 0, and a purge or drop that is not exactly on reads observe.

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

// One gate as the file spells it. writes is a pointer, since it defaults true
// inside a present gate.
type sidecarGateYaml struct {
	Enabled  bool     `yaml:"enabled"`
	Writes   *bool    `yaml:"writes"`
	Networks []string `yaml:"networks"`
	Percent  int      `yaml:"percent"`
	// format2's alone
	Purge string `yaml:"purge"`
	Drop  string `yaml:"drop"`
}

// A key that is only `enabled`.
type sidecarSwitchYaml struct {
	Enabled bool `yaml:"enabled"`
}

// The keys this reader reads.
type sidecarConfigYaml struct {
	Sidecar struct {
		P0Enforce         sidecarSwitchYaml  `yaml:"p0_enforce"`
		PreviewNetworks   []string           `yaml:"preview_networks"`
		Evs               *sidecarGateYaml   `yaml:"evs"`
		Format2           *sidecarGateYaml   `yaml:"format2"`
		Gkm               *sidecarGateYaml   `yaml:"gkm"`
		Sockets           *sidecarGateYaml   `yaml:"sockets"`
		Tlsedge           *sidecarGateYaml   `yaml:"tlsedge"`
		Mxedge            *sidecarGateYaml   `yaml:"mxedge"`
		Smtpedge          *sidecarGateYaml   `yaml:"smtpedge"`
		Dns               *sidecarGateYaml   `yaml:"dns"`
		Push              *sidecarSwitchYaml `yaml:"push"`
		Wake              *sidecarSwitchYaml `yaml:"wake"`
		Bridge            *sidecarSwitchYaml `yaml:"bridge"`
		EscrowDrawOrder   *sidecarSwitchYaml `yaml:"escrow_draw_order"`
		Reseed            bool               `yaml:"reseed"`
		PlaceNew          string             `yaml:"place_new"`
		Drained           []string           `yaml:"drained"`
		SocketsControlQps *struct {
			Rate  float64 `yaml:"rate"`
			Burst int     `yaml:"burst"`
		} `yaml:"sockets_control_qps"`
		UrnetworkEdge string `yaml:"urnetwork_edge"`
		FileClusters  map[string]struct {
			SettlePassedBuild string `yaml:"settle_passed_build"`
			HddCapacityOps    int64  `yaml:"hdd_capacity_ops"`
		} `yaml:"file_clusters"`
	} `yaml:"sidecar"`
}

// One read of sidecar.yml. Immutable once read. The zero value, what a first
// read of a broken file answers but for P0Enforce, opens nothing.
type SidecarConfig struct {
	// M0's switch for §7.4's stage 1 and D-TEAM-14, which refused only while it
	// was on. Since M1 (TEAM-M1-01) they refuse in code, and no refusal reads
	// this (question team-m1-01-retire-p0-enforce).
	P0Enforce bool
	// the networks that may hold strict_exp clients before stage 2 (D-TRUST-3)
	previewNetworkIds map[Id]bool

	// §19.11's service gates by name, a service the file holds none for absent
	serviceGates map[string]*SidecarServiceGate
	format2      *SidecarFormat2Gate
	// the switches by name, each on only when the file says enabled: true
	switches      map[string]bool
	reseed        bool
	placeNew      string
	drained       []string
	socketsRate   float64
	socketsBurst  int
	socketsSet    bool
	urnetworkEdge *Id
	fileClusters  map[string]SidecarFileCluster

	// whether the file names escrow_draw_order at all, on or off
	escrowDrawOrderNamed bool
}

// Whether the network is listed in preview_networks.
func (self *SidecarConfig) PreviewNetwork(networkId Id) bool {
	return self.previewNetworkIds[networkId]
}

// The ids of a list, each that does not parse skipped and logged, so one bad
// entry admits nothing and costs no other network its entry.
func parseSidecarConfigIds(key string, idStrs []string) []Id {
	ids := []Id{}
	for _, idStr := range idStrs {
		id, err := ParseId(idStr)
		if err != nil {
			glog.Errorf("[sidecar]%s lists an entry under %s that is not an id\n", sidecarConfigResourceName, key)
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// One bad entry costs only that entry.
func newSidecarConfig(y *sidecarConfigYaml) *SidecarConfig {
	config := &SidecarConfig{
		P0Enforce:         y.Sidecar.P0Enforce.Enabled,
		previewNetworkIds: map[Id]bool{},
		serviceGates:      map[string]*SidecarServiceGate{},
		switches:          map[string]bool{},
		reseed:            y.Sidecar.Reseed,
		placeNew:          y.Sidecar.PlaceNew,
		fileClusters:      map[string]SidecarFileCluster{},
	}
	for _, networkId := range parseSidecarConfigIds("preview_networks", y.Sidecar.PreviewNetworks) {
		config.previewNetworkIds[networkId] = true
	}
	for name, gate := range map[string]*sidecarGateYaml{
		SidecarGateEvs:      y.Sidecar.Evs,
		SidecarGateGkm:      y.Sidecar.Gkm,
		SidecarGateSockets:  y.Sidecar.Sockets,
		SidecarGateTlsedge:  y.Sidecar.Tlsedge,
		SidecarGateMxedge:   y.Sidecar.Mxedge,
		SidecarGateSmtpedge: y.Sidecar.Smtpedge,
		SidecarGateDns:      y.Sidecar.Dns,
	} {
		if gate != nil {
			config.serviceGates[name] = newSidecarServiceGate(name, gate)
		}
	}
	if gate := y.Sidecar.Format2; gate != nil {
		config.format2 = &SidecarFormat2Gate{
			gate:  newSidecarServiceGate(SidecarGateFormat2, gate),
			purge: parseSidecarFormat2Mode(gate.Purge),
			drop:  parseSidecarFormat2Mode(gate.Drop),
		}
	}
	for name, value := range map[string]*sidecarSwitchYaml{
		SidecarSwitchPush:            y.Sidecar.Push,
		SidecarSwitchWake:            y.Sidecar.Wake,
		SidecarSwitchBridge:          y.Sidecar.Bridge,
		SidecarSwitchEscrowDrawOrder: y.Sidecar.EscrowDrawOrder,
	} {
		config.switches[name] = value != nil && value.Enabled
	}
	config.escrowDrawOrderNamed = y.Sidecar.EscrowDrawOrder != nil
	for _, name := range y.Sidecar.Drained {
		if name != "" {
			config.drained = append(config.drained, name)
		}
	}
	if qps := y.Sidecar.SocketsControlQps; qps != nil {
		if 0 < qps.Rate && 0 < qps.Burst {
			config.socketsRate, config.socketsBurst, config.socketsSet = qps.Rate, qps.Burst, true
		} else {
			glog.Errorf("[sidecar]%s sets a sockets_control_qps that is not positive, so the engines keep their own\n", sidecarConfigResourceName)
		}
	}
	if y.Sidecar.UrnetworkEdge != "" {
		if ids := parseSidecarConfigIds("urnetwork_edge", []string{y.Sidecar.UrnetworkEdge}); len(ids) == 1 {
			config.urnetworkEdge = &ids[0]
		}
	}
	for clusterId, cluster := range y.Sidecar.FileClusters {
		if cluster.SettlePassedBuild == "" && cluster.HddCapacityOps <= 0 {
			continue
		}
		config.fileClusters[clusterId] = SidecarFileCluster{
			SettlePassedBuild: cluster.SettlePassedBuild,
			HddCapacityOps:    max(cluster.HddCapacityOps, 0),
		}
	}
	return config
}

// Parses the text of a sidecar.yml as a read of the file does.
func parseSidecarConfig(data []byte) (*SidecarConfig, error) {
	y := &sidecarConfigYaml{}
	if err := yaml.Unmarshal(data, y); err != nil {
		return nil, err
	}
	return newSidecarConfig(y), nil
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
			if !errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrResourceUnavailable) {
				// there but unreadable, a dangling link or a directory say
				config = failed(err)
				return
			}
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
