package controller

// The data figures of GET /network/provider-status
// (model/provider_status_provided.go): the newest settled epoch's provider
// rows, the usage bytes settlement v2 split that epoch's operator pool by,
// indexed by client.
//
// An epoch's rows never change once its root is finalized, so an api process
// reads an epoch's rows once, from the ledger object store, and keeps their
// index until a newer epoch settles. A request never waits for that read and
// never queries for it: a request that finds no index, or one older than a
// recheck allows, starts one read on its own goroutine and answers with what
// it has. The first answers after an epoch settles carry the previous
// epoch's figures, or none at all in a process that has read nothing yet,
// which the apps show as not yet known.
//
// The index holds a client id and a row's three facts per provider of the
// epoch, sorted by client id: a few tens of bytes per provider.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/urfoundation/sn/ledger"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type providerStatusProvidedSettings struct {
	// How long the newest settled epoch is trusted before the next request
	// looks again. Epochs settle about weekly, so an hour costs little.
	EpochRecheck time.Duration
	// How long a failed read waits before a request starts another.
	ReadRetry time.Duration
	// The bound of one read: the newest epoch's lookup and its rows.
	ReadTimeout time.Duration
}

func defaultProviderStatusProvidedSettings() *providerStatusProvidedSettings {
	return &providerStatusProvidedSettings{
		EpochRecheck: time.Hour,
		ReadRetry:    5 * time.Minute,
		ReadTimeout:  5 * time.Minute,
	}
}

// One epoch's provider rows, by client id.
type providerStatusProvidedIndex struct {
	epoch     uint64
	clientIds []server.Id
	rows      []model.ProviderStatusProvidedRow
}

// The index of rows: one per client, the usages of a client that appears
// twice added. A row's usage past int64 is held at its maximum.
func newProviderStatusProvidedIndex(epoch uint64, rows []snprotocol.LedgerUsageRow) *providerStatusProvidedIndex {
	byClient := map[server.Id]*model.ProviderStatusProvidedRow{}
	for _, row := range rows {
		clientId := server.Id(row.ClientID)
		indexed := byClient[clientId]
		if indexed == nil {
			indexed = &model.ProviderStatusProvidedRow{}
			byClient[clientId] = indexed
		}
		usage := model.ByteCount(min(row.UsageBytes, uint64(1<<63-1)))
		if model.ByteCount(1<<63-1)-indexed.UsageByteCount < usage {
			indexed.UsageByteCount = 1<<63 - 1
		} else {
			indexed.UsageByteCount += usage
		}
		indexed.Coldkey = indexed.Coldkey || row.Coldkey != ([32]byte{})
		indexed.HeadExcluded = indexed.HeadExcluded || row.HeadExcluded
	}
	index := &providerStatusProvidedIndex{
		epoch:     epoch,
		clientIds: make([]server.Id, 0, len(byClient)),
		rows:      make([]model.ProviderStatusProvidedRow, 0, len(byClient)),
	}
	for clientId := range byClient {
		index.clientIds = append(index.clientIds, clientId)
	}
	slices.SortFunc(index.clientIds, server.Id.Cmp)
	for _, clientId := range index.clientIds {
		index.rows = append(index.rows, *byClient[clientId])
	}
	return index
}

// The client's row, nil when the epoch has none for it.
func (self *providerStatusProvidedIndex) row(clientId server.Id) *model.ProviderStatusProvidedRow {
	i, found := slices.BinarySearchFunc(self.clientIds, clientId, server.Id.Cmp)
	if !found {
		return nil
	}
	row := self.rows[i]
	return &row
}

// The process's index of the newest settled epoch, and its one read.
type providerStatusProvidedCache struct {
	settings *providerStatusProvidedSettings

	stateLock sync.Mutex
	index     *providerStatusProvidedIndex
	// when the last read started, zero before the first, and whether one runs
	readTime time.Time
	reading  bool
	// whether the last read failed, which waits ReadRetry before the next
	failed bool

	// the reads, replaced in the pure tests
	newestEpoch func(ctx context.Context) (epoch uint64, ok bool)
	epochRows   func(ctx context.Context, epoch uint64) ([]snprotocol.LedgerUsageRow, error)
	now         func() time.Time
	// runs a read; a goroutine outside tests
	startRead func(read func())
}

func newProviderStatusProvidedCache() *providerStatusProvidedCache {
	return &providerStatusProvidedCache{
		settings:    defaultProviderStatusProvidedSettings(),
		newestEpoch: providerStatusProvidedNewestEpoch,
		epochRows:   providerStatusProvidedEpochRows,
		now:         server.NowUtc,
		startRead: func(read func()) {
			go server.HandleError(read)
		},
	}
}

var providerStatusProvided = newProviderStatusProvidedCache()

// The index an answer reads, replaced in the pure tests.
var getProviderStatusProvidedIndex = func() *providerStatusProvidedIndex {
	return providerStatusProvided.currentIndex()
}

// The index as it stands, nil before the first read succeeds. Starts a read
// when the recheck or the retry after a failure allows and none runs.
func (self *providerStatusProvidedCache) currentIndex() *providerStatusProvidedIndex {
	var index *providerStatusProvidedIndex
	start := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		index = self.index
		if self.reading {
			return
		}
		wait := self.settings.EpochRecheck
		if self.failed {
			wait = self.settings.ReadRetry
		}
		if !self.readTime.IsZero() && self.now().Before(self.readTime.Add(wait)) {
			return
		}
		self.reading = true
		self.readTime = self.now()
		start = true
	}()
	if start {
		self.startRead(self.read)
	}
	return index
}

// Reads the newest settled epoch's rows unless the index already holds that
// epoch, and installs their index. A failure keeps the index there is.
func (self *providerStatusProvidedCache) read() {
	failed := true
	var index *providerStatusProvidedIndex
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.reading = false
		self.failed = failed
		if index != nil {
			self.index = index
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.ReadTimeout)
	defer cancel()
	epoch, ok := self.newestEpoch(ctx)
	if !ok {
		// nothing settled yet: no figures, which is the truth
		failed = false
		return
	}
	if current := func() *providerStatusProvidedIndex {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.index
	}(); current != nil && epoch <= current.epoch {
		failed = false
		return
	}
	rows, err := self.epochRows(ctx, epoch)
	if err != nil {
		glog.Infof("[psp]epoch %d provider rows not read: %s\n", epoch, err)
		return
	}
	index = newProviderStatusProvidedIndex(epoch, rows)
	failed = false
	if glog.V(1) {
		glog.Infof("[psp]epoch %d provider rows read: %d clients\n", epoch, len(index.clientIds))
	}
}

// The newest epoch a finalized settlement v2 root covers whose amounts are
// recorded, the epochs GET /account/epochs lists with an amount.
func providerStatusProvidedNewestEpoch(ctx context.Context) (uint64, bool) {
	cfg := stConfig()
	if cfg == nil || cfg.DeploymentKey() == "" || !cfg.LedgerV2Enabled() {
		return 0, false
	}
	epochs := model.GetStLedgerV2FinalizedEpochs(ctx, cfg.DeploymentKey(), cfg.NoId, 1)
	if len(epochs) == 0 {
		return 0, false
	}
	return epochs[0].Epoch, true
}

// An epoch's recorded provider rows, read from the ledger object store; none
// for an epoch whose inputs the operator declared unrecoverable.
func providerStatusProvidedEpochRows(ctx context.Context, epoch uint64) ([]snprotocol.LedgerUsageRow, error) {
	cfg := stConfig()
	if cfg == nil || cfg.DeploymentKey() == "" {
		return nil, errors.New("st not configured")
	}
	record := model.GetStLedgerV2EpochRows(ctx, cfg.DeploymentKey(), cfg.NoId, epoch, epoch)[epoch]
	if record == nil {
		return nil, fmt.Errorf("epoch %d has no recorded rows", epoch)
	}
	if record.Null() {
		return []snprotocol.LedgerUsageRow{}, nil
	}
	chunks := make([][]byte, len(record.ProviderChunks))
	for i, ref := range record.ProviderChunks {
		chunk, err := GetStLedgerV2Object(ctx, ref)
		if err != nil {
			return nil, err
		}
		chunks[i] = chunk
	}
	return ledger.DecodeProviderChunks(chunks)
}

// Adds each status's data in the newest settled epoch the process has read.
// Without one the statuses are left without figures.
func attachProviderStatusProvided(statuses []*model.ProviderStatus) {
	index := getProviderStatusProvidedIndex()
	if index == nil {
		return
	}
	for _, status := range statuses {
		status.Provided = model.NewProviderStatusProvided(index.epoch, index.row(status.ClientId))
	}
}

// The network's data in the newest settled epoch the process has read, over
// the client ids, nil without an epoch.
func providerStatusNetworkProvided(clientIds []server.Id, truncated bool) *model.ProviderStatusNetworkProvided {
	index := getProviderStatusProvidedIndex()
	if index == nil {
		return nil
	}
	rows := make([]*model.ProviderStatusProvidedRow, 0, len(clientIds))
	for _, clientId := range clientIds {
		rows = append(rows, index.row(clientId))
	}
	return model.NewProviderStatusNetworkProvided(index.epoch, rows, truncated)
}
