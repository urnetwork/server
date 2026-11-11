package model

// Capture the flag's configuration: the vault resource `ctf.yml`. Absent,
// unreadable or empty, every part of capture the flag is off, so a server
// without it runs exactly as before. The presence and census blocks are read
// here; the driver and the api read their own blocks of the same resource,
// each into its own struct, so none of them needs another's fields:
//
//	presence:
//	  enabled: false            # the score export publishes its Online set and
//	                            # the quarter-hour sampler runs
//	  retention_epochs: 4       # samples kept after an epoch's census is built
//	census:
//	  enabled: false            # each epoch's census is built once its end is final
//	  first_epoch: 7            # the first epoch whose census is built
//	  own_network_ids: []       # networks the operator declares it controls (P6,
//	                            # X6), beside extender.yml's durable_network_ids
//	                            # and every extender canary's network
//	  object_retention_epochs: 8   # stored census objects kept, at least until a
//	                               # finalized ledger covers their epoch
//	  member_retention_epochs: 60

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Retention when ctf.yml leaves it out: presence for four epochs after a
// census; a census's stored objects for eight epochs, and never before a
// finalized ledger covers its epoch (they also stay in the ledger object
// store); and census members for sixty, more than the year a client's status
// covers. A census itself, its counts and its exclusions are kept.
const (
	ctfDefaultPresenceRetentionEpochs = 4
	ctfDefaultObjectRetentionEpochs   = 8
	ctfDefaultMemberRetentionEpochs   = 60
)

// How long one read of ctf.yml decides. Turning presence or the census on or
// off takes effect within this.
const ctfConfigMaxAge = time.Minute

type CtfPresenceConfig struct {
	Enabled         bool   `yaml:"enabled"`
	RetentionEpochs uint64 `yaml:"retention_epochs"`
}

type CtfCensusConfig struct {
	Enabled               bool     `yaml:"enabled"`
	FirstEpoch            uint64   `yaml:"first_epoch"`
	OwnNetworkIds         []string `yaml:"own_network_ids"`
	ObjectRetentionEpochs uint64   `yaml:"object_retention_epochs"`
	MemberRetentionEpochs uint64   `yaml:"member_retention_epochs"`
}

// The presence and census blocks of ctf.yml.
type CtfDataConfig struct {
	Presence CtfPresenceConfig `yaml:"presence"`
	Census   CtfCensusConfig   `yaml:"census"`
	// the parsed census.own_network_ids
	ownNetworkIds []server.Id
}

// OwnNetworkIds are the networks the operator declares it controls.
func (self *CtfDataConfig) OwnNetworkIds() []server.Id {
	return self.ownNetworkIds
}

// PresenceRetentionEpochs is how many epochs of samples are kept after an
// epoch's census.
func (self *CtfDataConfig) PresenceRetentionEpochs() uint64 {
	return ctfRetention(self.Presence.RetentionEpochs, ctfDefaultPresenceRetentionEpochs)
}

func (self *CtfDataConfig) ObjectRetentionEpochs() uint64 {
	return ctfRetention(self.Census.ObjectRetentionEpochs, ctfDefaultObjectRetentionEpochs)
}

func (self *CtfDataConfig) MemberRetentionEpochs() uint64 {
	return ctfRetention(self.Census.MemberRetentionEpochs, ctfDefaultMemberRetentionEpochs)
}

func ctfRetention(configured uint64, unset uint64) uint64 {
	if configured == 0 {
		return unset
	}
	return configured
}

// Checks a decoded configuration and parses its network ids. A census whose
// first epoch is unset, or whose declared networks do not all parse, is
// turned off: a dropped id would let the operator's own network compete.
func newCtfDataConfig(config *CtfDataConfig) *CtfDataConfig {
	if config == nil {
		return &CtfDataConfig{}
	}
	if !config.Census.Enabled {
		return config
	}
	if config.Census.FirstEpoch == 0 {
		glog.Errorf("[ctf]ctf.yml census.first_epoch is unset; the census stays off\n")
		config.Census.Enabled = false
		return config
	}
	for _, networkIdStr := range config.Census.OwnNetworkIds {
		networkId, err := server.ParseId(strings.TrimSpace(networkIdStr))
		if err != nil {
			glog.Errorf("[ctf]ctf.yml census.own_network_ids holds %q, which is not a network id; the census stays off\n", networkIdStr)
			config.Census.Enabled = false
			config.ownNetworkIds = nil
			return config
		}
		config.ownNetworkIds = append(config.ownNetworkIds, networkId)
	}
	return config
}

func readCtfDataConfig() *CtfDataConfig {
	resource, err := server.Vault.SimpleResource("ctf.yml")
	if err != nil {
		return &CtfDataConfig{}
	}
	config := &CtfDataConfig{}
	if err := resource.UnmarshalYamlE(config); err != nil {
		glog.Errorf("[ctf]ctf.yml is not readable; capture the flag stays off: %s\n", err)
		return &CtfDataConfig{}
	}
	return newCtfDataConfig(config)
}

type ctfDataConfigSnapshot struct {
	config   *CtfDataConfig
	loadTime time.Time
}

var ctfDataConfigCache atomic.Pointer[ctfDataConfigSnapshot]

// CtfDataSettings is ctf.yml's presence and census configuration, read at most
// ctfConfigMaxAge ago.
func CtfDataSettings() *CtfDataConfig {
	snapshot := ctfDataConfigCache.Load()
	if snapshot == nil || ctfConfigMaxAge <= time.Since(snapshot.loadTime) {
		snapshot = &ctfDataConfigSnapshot{config: readCtfDataConfig(), loadTime: time.Now()}
		ctfDataConfigCache.Store(snapshot)
	}
	return snapshot.config
}

// CtfPresenceEnabled reports whether ctf.yml turns presence on: the export
// publishes its Online set and the sampler runs.
func CtfPresenceEnabled() bool {
	return CtfDataSettings().Presence.Enabled
}

// Testing_SetCtfDataSettings replaces the configuration until the next reset;
// nil reads ctf.yml again.
func Testing_SetCtfDataSettings(config *CtfDataConfig) {
	if config == nil {
		ctfDataConfigCache.Store(nil)
		return
	}
	ctfDataConfigCache.Store(&ctfDataConfigSnapshot{config: newCtfDataConfig(config), loadTime: time.Now().Add(100 * 365 * 24 * time.Hour)})
}

func init() {
	server.OnReset(func() {
		ctfDataConfigCache.Store(nil)
	})
}
