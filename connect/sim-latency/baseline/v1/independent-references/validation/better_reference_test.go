package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestV5ReferenceUpdateActiveContractCacheUsesAuthoritativeSourceSnapshot(t *testing.T) {
	sourceID := server.NewId()
	activeDestinationID := server.NewId()
	closedDestinationID := server.NewId()
	missingDestinationID := server.NewId()
	unrelatedSourceID := server.NewId()
	unrelatedDestinationID := server.NewId()
	activePair := model.NewUnorderedTransferPair(sourceID, activeDestinationID)
	closedPair := model.NewUnorderedTransferPair(sourceID, closedDestinationID)
	missingPair := model.NewUnorderedTransferPair(sourceID, missingDestinationID)
	unrelatedPair := model.NewUnorderedTransferPair(unrelatedSourceID, unrelatedDestinationID)
	oldTime := time.Now().Add(-time.Minute)
	checkTime := time.Now()

	activeContracts := map[model.TransferPair]*activeContractEntry{
		missingPair: {
			checkTime: oldTime,
			active:    true,
		},
		unrelatedPair: {
			checkTime: oldTime,
			active:    true,
		},
	}
	activeSources := map[server.Id]*activeContractSourceEntry{}
	pairs := map[model.TransferPair]map[server.Id]model.ContractParty{
		activePair: {
			server.NewId(): "",
		},
		closedPair: {
			server.NewId(): model.ContractPartySource,
		},
	}

	entry := updateActiveContractCache(
		activeContracts,
		activeSources,
		sourceID,
		missingPair,
		pairs,
		checkTime,
	)
	if entry == nil || entry.active {
		t.Fatal("missing pair was not cached inactive")
	}
	if entry := activeContracts[activePair]; entry == nil || !entry.active {
		t.Fatal("open pair was not cached active")
	}
	if entry := activeContracts[closedPair]; entry == nil || entry.active {
		t.Fatal("partially closed pair was not cached inactive")
	}
	if entry := activeContracts[unrelatedPair]; entry == nil || !entry.active || !entry.checkTime.Equal(oldTime) {
		t.Fatal("unrelated pair was modified")
	}
	if entry := activeSources[sourceID]; entry == nil || !entry.checkTime.Equal(checkTime) {
		t.Fatal("source snapshot was not recorded")
	}
}

func TestV5ReferenceCachedActiveContractAnswersAbsentPairFromSourceSnapshot(t *testing.T) {
	sourceID := server.NewId()
	destinationID := server.NewId()
	pair := model.NewUnorderedTransferPair(sourceID, destinationID)
	settings := DefaultExchangeSettings()
	manager := newResidentContractManager(
		t.Context(),
		func() {},
		server.NewId(),
		settings,
	)
	checkTime := time.Now()
	manager.activeContractSources[sourceID] = &activeContractSourceEntry{checkTime: checkTime}

	entry, cached, refresh := manager.cachedActiveContract(sourceID, pair, true)
	if !cached || refresh || entry == nil || entry.active {
		t.Fatalf("absent pair cache = (%v, %v, %#v), want cached inactive", cached, refresh, entry)
	}
	if !entry.checkTime.Equal(checkTime) {
		t.Fatalf("entry time = %v, want %v", entry.checkTime, checkTime)
	}
}
