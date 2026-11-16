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

// Raised when the published amendments file was not taken, or no verified one
// is held.
const stLedgerV2AlertAmendments = "v2_amendments"

// Keeps a deployment's last verified amendments file in the
// database. A database failure is an error, which the source treats as no
// stored copy it may overwrite.
type stAmendmentStore struct {
	deploymentKey model.StDeploymentKey
}

// The stored copy, nil without one.
func (self *stAmendmentStore) Load(ctx context.Context) (raw []byte, err error) {
	defer stDemandStoreRecover("amendments load", &err)
	return model.GetStAmendments(ctx, self.deploymentKey), nil
}

// Replaces the stored copy.
func (self *stAmendmentStore) Save(ctx context.Context, raw []byte) (err error) {
	defer stDemandStoreRecover("amendments save", &err)
	model.SaveStAmendments(ctx, self.deploymentKey, raw)
	return nil
}

// The seams tests replace: the pinned signers, the published address, the
// store, and the file of a deployment without a pinned signer.
var (
	stAmendmentSigner = amendment.SignerFor
	stAmendmentUrl    = amendment.URL
	stAmendmentStorer = func(cfg *StConfig) amendment.Store {
		return &stAmendmentStore{deploymentKey: cfg.DeploymentKey()}
	}
	stAmendmentUnpublished = amendment.Unpublished
)

// One source per deployment for the process's life: a source holds the newest
// verified file it has taken, so a lost or older store cannot walk it back.
var (
	stDeploymentKeyAmendmentSourcesLock sync.Mutex
	stDeploymentKeyAmendmentSources     = map[model.StDeploymentKey]*amendment.Source{}
)

// Points the amendments file's published address and pinned signer at a test's
// own, for the tests of other packages, and returns what restores them. Only
// the deployment of chainId and netuid is pinned, so a zero chainId pins none:
// every deployment then publishes no amendments.
func Testing_SetStAmendments(url string, signer common.Address, chainId uint64, netuid uint16) (restore func()) {
	previousUrl, previousSigner, previousUnpublished := stAmendmentUrl, stAmendmentSigner, stAmendmentUnpublished
	stAmendmentUrl = url
	stAmendmentSigner = func(chain uint64, sn uint16) (common.Address, bool) {
		return signer, chainId != 0 && chain == chainId && sn == netuid
	}
	// amendment.Unpublished refuses a deployment the amendment package pins,
	// such as mainnet, which a test that unpins it still names: its file is
	// then the verified file with no entries.
	stAmendmentUnpublished = func(chain uint64, sn uint16) (*amendment.File, error) {
		return amendment.ParseVerified(fmt.Appendf(nil, "sn: %d\n", sn), chain, sn, common.Address{})
	}
	stDeploymentKeyAmendmentSourcesLock.Lock()
	stDeploymentKeyAmendmentSources = map[model.StDeploymentKey]*amendment.Source{}
	stDeploymentKeyAmendmentSourcesLock.Unlock()
	return func() {
		stAmendmentUrl, stAmendmentSigner, stAmendmentUnpublished = previousUrl, previousSigner, previousUnpublished
		stDeploymentKeyAmendmentSourcesLock.Lock()
		stDeploymentKeyAmendmentSources = map[model.StDeploymentKey]*amendment.Source{}
		stDeploymentKeyAmendmentSourcesLock.Unlock()
	}
}

// The amendments in force as one build reads them: the verified file and the
// configured fallback.
type stLedgerV2Amendments struct {
	noId     uint64
	file     *amendment.File
	fallback *amendment.Fallback
	// the extender pool's networks
	pool *stExtenderPoolConfig
	// the platform endpoint registry, read once, when an epoch the amendment
	// covers first needs it
	platformEndpoints model.StPlatformEndpointSet
}

// Reads the amendments for one build: it fetches the published file once, for a
// deployment with a pinned signer, and parses the configured lines. A file that
// cannot be taken leaves the last verified one in force, with an alert; with
// none held the amendments are unknown, and the build stops
// (amendment.ErrUnknown).
func stLedgerV2ReadAmendments(ctx context.Context, cfg *StConfig) (*stLedgerV2Amendments, error) {
	fallback, err := stLedgerV2ConfiguredAmendments(cfg)
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
		if read.file, err = stAmendmentUnpublished(cfg.ChainId, uint16(cfg.Netuid)); err != nil {
			return nil, err
		}
		return read, nil
	}
	// one source per deployment for the process's life
	source := func() *amendment.Source {
		stDeploymentKeyAmendmentSourcesLock.Lock()
		defer stDeploymentKeyAmendmentSourcesLock.Unlock()
		key := cfg.DeploymentKey()
		source := stDeploymentKeyAmendmentSources[key]
		if source == nil {
			source = amendment.NewSource(stAmendmentUrl, cfg.ChainId, uint16(cfg.Netuid), signer, stAmendmentStorer(cfg))
			stDeploymentKeyAmendmentSources[key] = source
		}
		return source
	}()
	file, problem := source.Refresh(ctx)
	if problem != nil {
		held := "no verified amendments file is held, so the amendments in force are unknown and no epoch's rows are built"
		if file != nil {
			held = "the last verified amendments file stays in force"
		}
		glog.Errorf("[st]v2 AMENDMENTS PROBLEM: %s; %s\n", stScrubEndpoints(problem.Error()), held)
		stLedgerV2Alert(cfg, stLedgerV2AlertAmendments, "amendments", "%s; %s", stScrubEndpoints(problem.Error()), held)
	}
	if file == nil {
		return nil, amendment.ErrUnknown
	}
	read.file = file
	return read, nil
}

// Parses st.yml's configured amendment lines, the fallback, for the
// deployment's netuid and operator.
func stLedgerV2ConfiguredAmendments(cfg *StConfig) (*amendment.Fallback, error) {
	if cfg == nil || cfg.Netuid == 0 || cfg.Netuid > 65535 || cfg.NoId == 0 {
		return nil, errors.New("st ledger v2: the amendments need the deployment's netuid and operator")
	}
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

// The amendments this server holds, read without fetching the published file:
// the verified copy the deployment last stored (every read that takes the
// published file stores it, stLedgerV2ReadAmendments), or the unpublished file
// of a deployment without a pinned signer, and the configured lines. A pinned
// deployment with no stored copy holds no file, and a stored copy that no
// longer parses or verifies is none, as amendment.Source treats it. A failed
// read or a malformed configured line is an error. It is for decisions that
// must not wait on the published file; a build reads that file.
func stLedgerV2HeldAmendments(ctx context.Context, cfg *StConfig) (*amendment.File, *amendment.Fallback, error) {
	fallback, err := stLedgerV2ConfiguredAmendments(cfg)
	if err != nil {
		return nil, nil, err
	}
	signer, pinned := stAmendmentSigner(cfg.ChainId, uint16(cfg.Netuid))
	if !pinned {
		file, err := stAmendmentUnpublished(cfg.ChainId, uint16(cfg.Netuid))
		if err != nil {
			return nil, nil, err
		}
		return file, fallback, nil
	}
	raw, err := stAmendmentStorer(cfg).Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) == 0 {
		return nil, fallback, nil
	}
	file, err := amendment.ParseVerified(raw, cfg.ChainId, uint16(cfg.Netuid), signer)
	if err != nil {
		glog.Errorf("[st]v2 the stored amendments file is not used: %s\n", stScrubEndpoints(err.Error()))
		return nil, fallback, nil
	}
	return file, fallback, nil
}

// The extender_pool block of st.yml, parsed.
type stExtenderPoolConfig struct {
	MeasurementNetworkIdStrs []string `yaml:"measurement_network_ids"`
	DeclaredNetworkIdStrs    []string `yaml:"declared_network_ids"`

	measurementNetworkIds map[server.Id]bool
	declaredNetworkIds    map[server.Id]bool
}

// Parses one extender_pool list of network ids; an entry that is not a
// network id refuses the list, label naming it.
func stParseNetworkIdSet(label string, values []string) (map[server.Id]bool, error) {
	networkIds := map[server.Id]bool{}
	for _, value := range values {
		networkId, err := server.ParseId(strings.TrimSpace(value))
		if err != nil || networkId == (server.Id{}) {
			return nil, fmt.Errorf("st.yml extender_pool %s: %q is not a network id", label, value)
		}
		networkIds[networkId] = true
	}
	return networkIds, nil
}

// Parses st.yml's extender_pool block. An entry that is not a network id
// refuses the block, rather than counting the traffic or extender it was meant
// to leave out.
func stExtenderPoolSettings(cfg *StConfig) (*stExtenderPoolConfig, error) {
	pool := &stExtenderPoolConfig{}
	if node := &cfg.ExtenderPool; !node.IsZero() {
		if err := node.Decode(pool); err != nil {
			return nil, fmt.Errorf("st.yml %s extender_pool: %w", cfg.Profile, err)
		}
	}
	var err error
	if pool.measurementNetworkIds, err = stParseNetworkIdSet("measurement_network_ids", pool.MeasurementNetworkIdStrs); err != nil {
		return nil, err
	}
	if pool.declaredNetworkIds, err = stParseNetworkIdSet("declared_network_ids", pool.DeclaredNetworkIdStrs); err != nil {
		return nil, err
	}
	return pool, nil
}

// The extender pool amendment as the epoch reader applies it to
// epoch: in force from the first epoch of the entry that covers it, with the
// operator's registered platform endpoints (model/st_platform_endpoint.go), and
// the zero amendment when none covers it.
func (self *stLedgerV2Amendments) extenders(ctx context.Context, epoch uint64) (model.StExtenderAmendment, error) {
	in, err := self.file.At(self.noId, epoch, self.fallback)
	if err != nil {
		return model.StExtenderAmendment{}, err
	}
	if in.Extenders == nil {
		return model.StExtenderAmendment{}, nil
	}
	if self.platformEndpoints == nil {
		if self.platformEndpoints, err = stPlatformEndpoints(ctx); err != nil {
			return model.StExtenderAmendment{}, err
		}
	}
	return model.StExtenderAmendment{FirstEpoch: in.Extenders.FromEpoch, PlatformEndpoints: self.platformEndpoints}, nil
}

// Reads the platform endpoint registry: a seam the pure tests replace.
var stPlatformEndpointsRead = model.GetStPlatformEndpoints

// Reads the platform endpoint registry. A failed read, such
// as a table not migrated yet, is an error, never an empty registry.
func stPlatformEndpoints(ctx context.Context) (set model.StPlatformEndpointSet, err error) {
	defer stDemandStoreRecover("platform endpoint registry read", &err)
	return stPlatformEndpointsRead(ctx), nil
}

// The operator's command registering a client as a platform endpoint of kind
// from now on (model.RegisterStPlatformEndpoint). A registration is checked
// against the operator's own networks, the one list E5 reads
// (readStExtenderOwnNetworks: its gated durable networks, the networks it
// declares it controls and the networks of its canaries), and its measurement
// networks.
func StRegisterPlatformEndpoint(ctx context.Context, clientId server.Id, kind string, by string) (*model.StPlatformEndpoint, error) {
	cfg, _, err := stRequire()
	if err != nil {
		return nil, err
	}
	pool, err := stExtenderPoolSettings(cfg)
	if err != nil {
		return nil, err
	}
	ownNetworks, err := readStExtenderOwnNetworks(ctx, cfg)
	if err != nil {
		return nil, err
	}
	networks := model.StPlatformEndpointNetworks{
		OperatorNetworkIds:    ownNetworks.allNetworkIds(),
		MeasurementNetworkIds: stExtenderPoolMeasurementNetworks(ctx, pool),
	}
	registered, err := model.RegisterStPlatformEndpoint(ctx, clientId, kind, by, networks)
	if err == nil {
		glog.Infof("[st]platform endpoint %s (%s, network %s) registered by %s from %s\n", clientId, kind, registered.NetworkId, by, registered.RegisterTime.Format("2006-01-02T15:04:05Z"))
	}
	return registered, err
}

// The operator's command ending a client's platform endpoint registration now.
func StRevokePlatformEndpoint(ctx context.Context, clientId server.Id, by string) error {
	err := model.RevokeStPlatformEndpoint(ctx, clientId, by)
	if err == nil {
		glog.Infof("[st]platform endpoint %s revoked by %s\n", clientId, by)
	}
	return err
}

// The payer networks whose contracts never count toward Z: the configured
// measurement and test networks and the internal prober's.
func stExtenderPoolMeasurementNetworks(ctx context.Context, pool *stExtenderPoolConfig) map[server.Id]bool {
	measurementNetworkIds := map[server.Id]bool{}
	for networkId := range pool.measurementNetworkIds {
		measurementNetworkIds[networkId] = true
	}
	if prober := model.GetProberIdentity(ctx); prober.HasNetwork() {
		measurementNetworkIds[*prober.NetworkId] = true
	}
	return measurementNetworkIds
}
