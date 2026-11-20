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
//	  evs: {enabled: false, writes: true, networks: [], percent: 0, blocks: <n>}
//	  format2: {enabled: false, networks: [], percent: 0, purge: observe, drop: observe}
//	  gkm: {enabled: false, networks: [], percent: 0}  # sockets, tlsedge, mxedge, smtpedge alike
//	  dns: {enabled: false, writes: true}
//	  push: {enabled: false}
//	  wake: {enabled: false}
//	  bridge: {enabled: false}
//	  escrow_draw_order: {enabled: false}
//	  acl_contract_gate: {enabled: false}
//	  certs: {enabled: false}
//	  reseed: false
//	  place_new: meta-1
//	  drained: []
//	  preview_networks: []          # §7.4, D-PHASE-3: networks that hold strict_exp clients before stage 2
//	  sockets_control_qps: {rate: 50, burst: 500}
//	  urnetwork_edge: <network_id>
//	  file_clusters: {<cluster id>: {settle_passed_build: <commit>, hdd_capacity_ops: <n>}}
//
// No key enforces §7.4's stage 1 or D-TEAM-14: since M1 (TEAM-M1-01) they refuse
// in code, in api's route gates, session's strict_exp expiry and the transport
// lease's end, whatever this file holds or whether it is there. p0_enforce, M0's
// log-only switch for them, is retired (owner decision 510, QD-922): the reader
// has no field for it, so a file an old deployment still carries with the line
// reads as one without it, whatever the line's value.
// file_clusters is plan-EVSB-settle-gate's.
// restore_hold is plan-OPS-restore-hold's (QD-076): while it reads true, api
// answers every public route but its status route 503 with Retry-After, the
// token exchange among them, and CreateContract answers Setup, until §19.10's
// api step ends and the runbook clears it. A process's first read of a file that
// does not parse holds nothing, as it opens nothing: a broken push must not stop
// every route of a running product, and a restore's runbook reads the hold back
// before its api step runs (sidecarrestore).
// acl_contract_gate is not in §19.11's list: it is the switch ACL-M4-01's plan
// keeps CreateContract's §8.10 gate dark behind, which a process consults only
// where it installed a gate (controller.SetAclContractGate).
// certs is QD-846's (question tls-m5-17-certs-host-install), which §19.11's list
// carries (owner decision 429): certs' engine is installed in api (its /tls
// routes) and in taskworker (its order, renewal and revocation chains) only
// while it reads on, so nothing reaches a CA or a zone because a vault file
// arrived, and the first real order is the owner's act (SidecarSwitchInstall).
// It is the server's own, like wake, and never listed to a client.
// evs's blocks is QD-639's (question quota-m2-10-evs-block-count), which §19.4's
// evs row carries (owner decision 500): how many evs blocks serve the env,
// set by the operator with services.yml's evs block count, as tlsedge.yml's
// blocks is (§13.9, TLS reading 12). Each evs process paces a spent network's
// reads at evs_unpaid_read_rate ÷ blocks (§20.6), with no shared counter. It has
// no default: unset, or below 1, reads as unset, and server/evshost refuses to
// start a block whose gate is enabled without it. The count is the env's, never
// what one host sees of its peers.
// sidecar_gates.go holds what each gate answers.
//
// A gate that loosens when the file breaks would open a service, or preview a
// network, on a bad push. So once a read succeeded, a failed or absent read
// keeps it, and on a process's first read a file that is there but cannot be
// read or parsed answers the zero value, as an absent file does: it previews
// nothing and opens no gate, and nothing the file partly parsed is kept. A failed
// read is logged at every reread; only a file the resolver finds absent from
// every home (ErrResourceNotFound) reads as absent, quietly: a dangling link, a
// directory or an inaccessible home is unavailable, not absent.
// The zero value a failed read answers before any read succeeded says so
// (FailedFirstRead; QD-1079), for a reader whose zero value is not the closed
// direction: it names no urnetwork_edge, so taskworker's idle top-level marking,
// which leaves that network's clients alone, marks nothing while it holds. The
// first good read clears it. An absent file is no failed read: before a good
// read it answers the plain zero value, after a failed one too, since the
// environment then names no edge network to protect.
// Taking a gate back off is an edit to the file, not its removal: a removal
// holds until the process restarts. Within a file that parses, one bad entry
// costs only itself: an id that does not parse is skipped, a percent out of
// range reads 0, a purge or drop that is not exactly on reads observe, and an evs
// blocks that is not a whole number of at least 1 reads as unset.

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
	// evs's alone: a node, since it has no default and only a whole number counts
	Blocks yaml.Node `yaml:"blocks"`
}

// A key that is only `enabled`.
type sidecarSwitchYaml struct {
	Enabled bool `yaml:"enabled"`
}

// The keys this reader reads.
type sidecarConfigYaml struct {
	Sidecar struct {
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
		AclContractGate   *sidecarSwitchYaml `yaml:"acl_contract_gate"`
		Certs             *sidecarSwitchYaml `yaml:"certs"`
		Reseed            bool               `yaml:"reseed"`
		RestoreHold       bool               `yaml:"restore_hold"`
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
// read of an absent or broken file answers, previews nothing and opens nothing;
// a broken one's also says it failed (FailedFirstRead).
type SidecarConfig struct {
	// the file was read and parsed: a good read, which a later failed read keeps
	parsed bool
	// stands in for a file that is there but could not be read or parsed, with no
	// good read to keep (QD-1079)
	failedFirstRead bool

	// the networks that may hold strict_exp clients before stage 2 (D-TRUST-3)
	previewNetworkIds map[Id]bool

	// §19.11's service gates by name, a service the file holds none for absent
	serviceGates map[string]*SidecarServiceGate
	format2      *SidecarFormat2Gate
	// the switches by name, each on only when the file says enabled: true
	switches      map[string]bool
	reseed        bool
	restoreHold   bool
	placeNew      string
	drained       []string
	socketsRate   float64
	socketsBurst  int
	socketsSet    bool
	urnetworkEdge *Id
	fileClusters  map[string]SidecarFileCluster

	// whether the file names escrow_draw_order at all, on or off
	escrowDrawOrderNamed bool

	// the evs gate's blocks, 0 when the file sets none that is at least 1
	evsBlocks int
}

// Whether the network is listed in preview_networks.
func (self *SidecarConfig) PreviewNetwork(networkId Id) bool {
	return self.previewNetworkIds[networkId]
}

// Whether this is the zero value of a failed read with no good read before it
// (QD-1079): the file is there but could not be read or parsed, and the process
// does not know what it says. Every gate answers closed, as for an absent file.
// A reader whose zero value is the destructive direction holds off instead, as
// taskworker's idle top-level marking does (model.RemoveDisconnectedNetworkClients),
// since this read names no urnetwork_edge whatever the file does. A good read
// never carries it, nor does an absent file's.
func (self *SidecarConfig) FailedFirstRead() bool {
	return self.failedFirstRead
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

// The evs gate's blocks: a whole number of at least 1, and 0 for anything else,
// said once a read. A fraction is never rounded, since a count read low hands a
// spent network more than its rate.
func parseSidecarConfigEvsBlocks(node *yaml.Node) int {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	if node.Kind == 0 || node.ShortTag() == "!!null" {
		// no key
		return 0
	}
	blocks := 0
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!int" || node.Decode(&blocks) != nil || blocks < 1 {
		glog.Errorf("[sidecar]%s sets an evs blocks that is not a whole number of at least 1, which reads as unset\n", sidecarConfigResourceName)
		return 0
	}
	return blocks
}

// One bad entry costs only that entry.
func newSidecarConfig(y *sidecarConfigYaml) *SidecarConfig {
	config := &SidecarConfig{
		parsed:            true,
		previewNetworkIds: map[Id]bool{},
		serviceGates:      map[string]*SidecarServiceGate{},
		switches:          map[string]bool{},
		reseed:            y.Sidecar.Reseed,
		restoreHold:       y.Sidecar.RestoreHold,
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
	if gate := y.Sidecar.Evs; gate != nil {
		config.evsBlocks = parseSidecarConfigEvsBlocks(&gate.Blocks)
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
		SidecarSwitchAclContractGate: y.Sidecar.AclContractGate,
		SidecarSwitchCerts:           y.Sidecar.Certs,
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

// Reads the file, keeping previous when the read fails and previous is a good
// read. See the file's header for what a failure answers.
func loadSidecarConfig(previous *SidecarConfig) (config *SidecarConfig) {
	failed := func(err any) *SidecarConfig {
		glog.Errorf("[sidecar]could not read %s: %v\n", sidecarConfigResourceName, err)
		if previous != nil && (previous.parsed || previous.failedFirstRead) {
			// the latest good read, or the failed first read itself, so a second
			// failure answers the same read
			return previous
		}
		// the zero value, never what the file partly parsed, and the process
		// knows nothing of what the file says
		return &SidecarConfig{failedFirstRead: true}
	}
	if r := HandleError(func() {
		resource, err := Config.SimpleResource(sidecarConfigResourceName)
		if err != nil {
			if !errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrResourceUnavailable) {
				// there but unreadable, a dangling link or a directory say
				config = failed(err)
				return
			}
			// absent: a good read holds, as an earlier absent read does, and a
			// failed one gives way to the zero value, which is no failed read
			if previous != nil && !previous.failedFirstRead {
				config = previous
			} else {
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

// Forgets every read, so the process's next read of sidecar.yml is a first read.
func Testing_ForgetSidecarConfig() {
	defaultSidecarConfigReader.latest.Store(nil)
}

// Stands in for sidecar.yml until the returned pop, as a config push changes the
// file under a running process: the next read rereads it, as the end of the
// reread interval does, with the latest read as its previous, so a broken file
// keeps a good read and a good file replaces a failed one. The pop forgets every
// read, as Testing_PushSidecarConfig's does, so no read outlives the test.
func Testing_RereadSidecarConfig(yaml string) func() {
	pop := Config.PushSimpleResource(sidecarConfigResourceName, []byte(yaml))
	if read := defaultSidecarConfigReader.latest.Load(); read != nil {
		// a zero read time is always older than the interval
		defaultSidecarConfigReader.latest.Store(&sidecarConfigRead{config: read.config})
	}
	return func() {
		pop()
		defaultSidecarConfigReader.latest.Store(nil)
	}
}
