package controller

// st_ledger_v2_amendments.go — the signed settlement amendments
// (sn/amendment, published beside the price sheet at amendment.URL) as the
// operator server reads them for settlement v2, and the SN25 extender pool's
// configuration (sn WHITEPAPER.md §8.6).
//
// The extender pool amendment covers an epoch when the signed entry in force
// for it does, from that entry's first epoch E_x, or, only where no signed
// entry of its kind is in force, when a line configured in st.yml does
// (amendment.File.At). A deployment with a pinned signer reads its verified
// file; without one held, what is in force is unknown, and configuration does
// not stand in: an epoch's rows are not built until a verified file is held.
// A deployment without a pinned signer publishes none, so only its
// configuration can put the amendment in force.
//
// st.yml's blocks, each read only here, so a malformed one refuses the epochs
// whose rows need it and never the st subsystem:
//
//	amendments:                    # configured lines, the fallback (§8.6.7)
//	  - "UR settlement amendment v1: chain 964, sn 25, no 1, from epoch 8, ..."
//	extender_pool:
//	  measurement_network_ids:     # payers of the operator's measurement and
//	    - <network id>             # test traffic, left out of Z (§8.6.2)
//	  declared_network_ids:        # networks the operator declares it controls
//	    - <network id>             # beside durable_network_ids (E5)
//
// with testnet-amendments and testnet-extender-pool for the testnet profile.
// The internal prober's network is always a measurement network.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/amendment"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// StLedgerV2AlertAmendments is raised when the published amendments file was
// not taken, or no verified one is held.
const StLedgerV2AlertAmendments = "v2_amendments"

// stAmendmentStore keeps a deployment's last verified amendments file in the
// database. A database failure is an error, which the source treats as no
// stored copy it may overwrite.
type stAmendmentStore struct {
	deploymentKey model.StDeploymentKey
}

func (self *stAmendmentStore) Load(ctx context.Context) (raw []byte, err error) {
	defer stDemandStoreRecover("amendments load", &err)
	return model.GetStAmendments(ctx, self.deploymentKey), nil
}

func (self *stAmendmentStore) Save(ctx context.Context, raw []byte) (err error) {
	defer stDemandStoreRecover("amendments save", &err)
	model.SaveStAmendments(ctx, self.deploymentKey, raw)
	return nil
}

// The seams tests replace: the pinned signers, the published address and the
// store.
var (
	stAmendmentSigner = amendment.SignerFor
	stAmendmentUrl    = amendment.URL
	stAmendmentStorer = func(cfg *StConfig) amendment.Store {
		return &stAmendmentStore{deploymentKey: cfg.DeploymentKey()}
	}
)

// One source per deployment for the process's life: a source holds the newest
// verified file it has taken, so a lost or older store cannot walk it back.
var (
	stAmendmentSourcesLock sync.Mutex
	stAmendmentSources     = map[model.StDeploymentKey]*amendment.Source{}
)

func stAmendmentSource(cfg *StConfig, signer common.Address) *amendment.Source {
	stAmendmentSourcesLock.Lock()
	defer stAmendmentSourcesLock.Unlock()
	key := cfg.DeploymentKey()
	source := stAmendmentSources[key]
	if source == nil {
		source = amendment.NewSource(stAmendmentUrl, cfg.ChainId, uint16(cfg.Netuid), signer, stAmendmentStorer(cfg))
		stAmendmentSources[key] = source
	}
	return source
}

// stLedgerV2Amendments is the amendments in force as one build reads them:
// the verified file and the configured fallback.
type stLedgerV2Amendments struct {
	noId     uint64
	file     *amendment.File
	fallback *amendment.Fallback
	// platform endpoints and the extender pool's networks
	pool *stExtenderPoolConfig
}

// stLedgerV2ReadAmendments reads the amendments for one build: it fetches the
// published file once, for a deployment with a pinned signer, and parses the
// configured lines. A file that cannot be taken leaves the last verified one
// in force, with an alert; with none held the amendments are unknown, and the
// build stops (amendment.ErrUnknown).
func stLedgerV2ReadAmendments(ctx context.Context, cfg *StConfig) (*stLedgerV2Amendments, error) {
	if cfg == nil || cfg.Netuid == 0 || cfg.Netuid > 65535 || cfg.NoId == 0 {
		return nil, errors.New("st ledger v2: the amendments need the deployment's netuid and operator")
	}
	fallback, err := stAmendmentFallback(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := stExtenderPoolSettings(cfg)
	if err != nil {
		return nil, err
	}
	read := &stLedgerV2Amendments{noId: cfg.NoId, fallback: fallback, pool: pool}
	signer, pinned := stAmendmentSigner(cfg.ChainId, uint16(cfg.Netuid))
	if !pinned {
		if read.file, err = amendment.Unpublished(cfg.ChainId, uint16(cfg.Netuid)); err != nil {
			return nil, err
		}
		return read, nil
	}
	file, problem := stAmendmentSource(cfg, signer).Refresh(ctx)
	if problem != nil {
		held := "no verified amendments file is held, so the amendments in force are unknown and no epoch's rows are built"
		if file != nil {
			held = "the last verified amendments file stays in force"
		}
		glog.Errorf("[st]v2 AMENDMENTS PROBLEM: %s; %s\n", stScrubEndpoints(problem.Error()), held)
		stLedgerV2Alert(cfg, StLedgerV2AlertAmendments, "amendments", "%s; %s", stScrubEndpoints(problem.Error()), held)
	}
	if file == nil {
		return nil, amendment.ErrUnknown
	}
	read.file = file
	return read, nil
}

// stAmendmentFallback parses st.yml's configured amendment lines.
func stAmendmentFallback(cfg *StConfig) (*amendment.Fallback, error) {
	var lines []string
	if node := &cfg.Amendments; !node.IsZero() {
		if err := node.Decode(&lines); err != nil {
			return nil, fmt.Errorf("st.yml %s amendments: %w", cfg.Profile, err)
		}
	}
	fallback, err := amendment.ParseFallback(cfg.ChainId, uint16(cfg.Netuid), lines)
	if err != nil {
		return nil, fmt.Errorf("st.yml %s %w", cfg.Profile, err)
	}
	return fallback, nil
}

// stExtenderPoolConfig is st.yml's extender_pool block, parsed.
type stExtenderPoolConfig struct {
	MeasurementNetworkIds []string `yaml:"measurement_network_ids"`
	DeclaredNetworkIds    []string `yaml:"declared_network_ids"`

	measurement map[server.Id]bool
	declared    map[server.Id]bool
}

func stParseNetworkIdSet(label string, values []string) (map[server.Id]bool, error) {
	set := map[server.Id]bool{}
	for _, value := range values {
		networkId, err := server.ParseId(strings.TrimSpace(value))
		if err != nil || networkId == (server.Id{}) {
			return nil, fmt.Errorf("st.yml extender_pool %s: %q is not a network id", label, value)
		}
		set[networkId] = true
	}
	return set, nil
}

// stExtenderPoolSettings parses st.yml's extender_pool block. An entry that
// is not a network id refuses the block, rather than counting the traffic or
// extender it was meant to leave out.
func stExtenderPoolSettings(cfg *StConfig) (*stExtenderPoolConfig, error) {
	pool := &stExtenderPoolConfig{}
	if node := &cfg.ExtenderPool; !node.IsZero() {
		if err := node.Decode(pool); err != nil {
			return nil, fmt.Errorf("st.yml %s extender_pool: %w", cfg.Profile, err)
		}
	}
	var err error
	if pool.measurement, err = stParseNetworkIdSet("measurement_network_ids", pool.MeasurementNetworkIds); err != nil {
		return nil, err
	}
	if pool.declared, err = stParseNetworkIdSet("declared_network_ids", pool.DeclaredNetworkIds); err != nil {
		return nil, err
	}
	return pool, nil
}

// extenders is the extender pool amendment as the epoch reader applies it to
// epoch: in force from the first epoch of the entry that covers it, and the
// zero amendment when none does. Platform endpoints are sidecar's edges, which
// no registry names yet, so there are none.
func (self *stLedgerV2Amendments) extenders(epoch uint64) (model.StExtenderAmendment, error) {
	in, err := self.file.At(self.noId, epoch, self.fallback)
	if err != nil {
		return model.StExtenderAmendment{}, err
	}
	if in.Extenders == nil {
		return model.StExtenderAmendment{}, nil
	}
	return model.StExtenderAmendment{FirstEpoch: in.Extenders.FromEpoch, PlatformEndpoints: map[server.Id]bool{}}, nil
}

// stExtenderPoolMeasurementNetworks are the payer networks whose contracts
// never count toward Z: the configured measurement and test networks and the
// internal prober's.
func stExtenderPoolMeasurementNetworks(ctx context.Context, pool *stExtenderPoolConfig) map[server.Id]bool {
	measurement := map[server.Id]bool{}
	for networkId := range pool.measurement {
		measurement[networkId] = true
	}
	if prober := model.GetProberIdentity(ctx); prober.HasNetwork() {
		measurement[*prober.NetworkId] = true
	}
	return measurement
}
