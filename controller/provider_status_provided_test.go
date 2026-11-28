package controller

// The data figures of GET /network/provider-status without a database: each
// client's row of the newest settled epoch and how it was paid, the network's
// totals with its unpaid data, the process index that reads an epoch's rows
// once and never makes a request wait, and the answers that carry the
// figures. The reads are faked.

import (
	"context"
	"errors"
	"testing"
	"time"

	snprotocol "github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func testingProviderStatusProvidedRow(clientId server.Id, usageBytes uint64, coldkey bool, headExcluded bool) snprotocol.LedgerUsageRow {
	row := snprotocol.LedgerUsageRow{
		ClientID:     [16]byte(clientId),
		UsageBytes:   usageBytes,
		HeadExcluded: headExcluded,
	}
	if coldkey {
		row.Coldkey[0] = 0x2a
	}
	return row
}

// A row with a coldkey was paid in the pool, a head-excluded row earns through
// the head seat, a row without a coldkey was not paid, and a client without a
// row provided nothing the epoch counted. A client in two rows adds them.
func TestProviderStatusProvidedAttributesEachRow(t *testing.T) {
	paid, unpaid, head, twice, absent := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	index := newProviderStatusProvidedIndex(9, []snprotocol.LedgerUsageRow{
		testingProviderStatusProvidedRow(paid, 3000, true, false),
		testingProviderStatusProvidedRow(unpaid, 500, false, false),
		testingProviderStatusProvidedRow(head, 7000, true, true),
		testingProviderStatusProvidedRow(twice, 100, false, false),
		testingProviderStatusProvidedRow(twice, 50, false, false),
	})
	cases := []struct {
		clientId server.Id
		want     model.ProviderStatusProvided
	}{
		{clientId: paid, want: model.ProviderStatusProvided{Epoch: 9, UsageByteCount: 3000, Payout: model.ProviderStatusProvidedPaid}},
		{clientId: unpaid, want: model.ProviderStatusProvided{Epoch: 9, UsageByteCount: 500, Payout: model.ProviderStatusProvidedUnpaid}},
		{clientId: head, want: model.ProviderStatusProvided{Epoch: 9, UsageByteCount: 7000, Payout: model.ProviderStatusProvidedHead}},
		{clientId: twice, want: model.ProviderStatusProvided{Epoch: 9, UsageByteCount: 150, Payout: model.ProviderStatusProvidedUnpaid}},
		{clientId: absent, want: model.ProviderStatusProvided{Epoch: 9, UsageByteCount: 0, Payout: model.ProviderStatusProvidedNone}},
	}
	for _, c := range cases {
		provided := model.NewProviderStatusProvided(index.epoch, index.row(c.clientId))
		if *provided != c.want {
			t.Errorf("provided of %s = %+v, want %+v", c.clientId, *provided, c.want)
		}
	}

	// the network's totals over its clients: the unpaid data is the rows
	// without a payout wallet, never the paid or the head seat's
	rows := []*model.ProviderStatusProvidedRow{}
	for _, clientId := range []server.Id{paid, unpaid, head, twice, absent} {
		rows = append(rows, index.row(clientId))
	}
	network := model.NewProviderStatusNetworkProvided(index.epoch, rows, false)
	want := model.ProviderStatusNetworkProvided{Epoch: 9, UsageByteCount: 10650, UnpaidByteCount: 650}
	if *network != want {
		t.Fatalf("network provided = %+v, want %+v", *network, want)
	}
}

// A fake epoch source for the process index, and the reads it was asked for.
type providerStatusProvidedTestSource struct {
	epoch     uint64
	hasEpoch  bool
	rows      map[uint64][]snprotocol.LedgerUsageRow
	rowsErr   error
	rowReads  []uint64
	now       time.Time
	pending   []func()
	epochRead int
}

func newProviderStatusProvidedTestCache(source *providerStatusProvidedTestSource) *providerStatusProvidedCache {
	cache := newProviderStatusProvidedCache()
	cache.newestEpoch = func(ctx context.Context) (uint64, bool) {
		source.epochRead += 1
		return source.epoch, source.hasEpoch
	}
	cache.epochRows = func(ctx context.Context, epoch uint64) ([]snprotocol.LedgerUsageRow, error) {
		source.rowReads = append(source.rowReads, epoch)
		if source.rowsErr != nil {
			return nil, source.rowsErr
		}
		return source.rows[epoch], nil
	}
	cache.now = func() time.Time {
		return source.now
	}
	// a read is held until the test runs it, so a request is seen not to wait
	cache.startRead = func(read func()) {
		source.pending = append(source.pending, read)
	}
	return cache
}

func (self *providerStatusProvidedTestSource) runReads() {
	pending := self.pending
	self.pending = nil
	for _, read := range pending {
		read()
	}
}

// The first request starts one read and answers at once without figures;
// requests while it runs start no other. Once read, the epoch's index answers
// until the recheck, which looks again and reads a newer epoch only.
func TestProviderStatusProvidedCacheReadsAnEpochOnceWithoutWaiting(t *testing.T) {
	clientId := server.NewId()
	source := &providerStatusProvidedTestSource{
		epoch:    7,
		hasEpoch: true,
		rows: map[uint64][]snprotocol.LedgerUsageRow{
			7: {testingProviderStatusProvidedRow(clientId, 1000, true, false)},
			8: {testingProviderStatusProvidedRow(clientId, 2500, false, false)},
		},
		now: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC),
	}
	cache := newProviderStatusProvidedTestCache(source)

	if index := cache.currentIndex(); index != nil {
		t.Fatal("a request before any read had an index")
	}
	if index := cache.currentIndex(); index != nil || len(source.pending) != 1 {
		t.Fatalf("requests during a read started %d reads, want 1", len(source.pending))
	}
	source.runReads()
	index := cache.currentIndex()
	if index == nil || index.epoch != 7 || index.row(clientId).UsageByteCount != 1000 {
		t.Fatalf("index after the read = %+v", index)
	}
	if len(source.pending) != 0 {
		t.Fatal("a request inside the recheck started a read")
	}

	// the recheck finds the same epoch: no rows are read again
	source.now = source.now.Add(cache.settings.EpochRecheck)
	cache.currentIndex()
	source.runReads()
	if len(source.rowReads) != 1 {
		t.Fatalf("rows read for epochs %v, want epoch 7 once", source.rowReads)
	}

	// a newer epoch settles: the next recheck reads it, and until it is read
	// the answers keep epoch 7
	source.epoch = 8
	source.now = source.now.Add(cache.settings.EpochRecheck)
	if index := cache.currentIndex(); index == nil || index.epoch != 7 {
		t.Fatalf("index while epoch 8 is read = %+v, want epoch 7", index)
	}
	source.runReads()
	if index := cache.currentIndex(); index == nil || index.epoch != 8 || index.row(clientId).UsageByteCount != 2500 {
		t.Fatalf("index after epoch 8 = %+v", index)
	}
}

// A failed read keeps the index there is and waits the retry, not the
// recheck, before the next one; nothing settled yet is no figures and no
// failure.
func TestProviderStatusProvidedCacheRetriesAFailedRead(t *testing.T) {
	source := &providerStatusProvidedTestSource{
		epoch:    7,
		hasEpoch: true,
		rowsErr:  errors.New("object store unavailable"),
		now:      time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC),
	}
	cache := newProviderStatusProvidedTestCache(source)
	cache.currentIndex()
	source.runReads()
	if index := cache.currentIndex(); index != nil || len(source.pending) != 0 {
		t.Fatal("a failed read was retried before the retry")
	}
	source.now = source.now.Add(cache.settings.ReadRetry)
	source.rowsErr = nil
	source.rows = map[uint64][]snprotocol.LedgerUsageRow{7: {}}
	cache.currentIndex()
	source.runReads()
	if index := cache.currentIndex(); index == nil || index.epoch != 7 {
		t.Fatalf("index after the retry = %+v", index)
	}

	unsettled := &providerStatusProvidedTestSource{now: source.now}
	cache = newProviderStatusProvidedTestCache(unsettled)
	cache.currentIndex()
	unsettled.runReads()
	if index := cache.currentIndex(); index != nil || len(unsettled.pending) != 0 || len(unsettled.rowReads) != 0 {
		t.Fatal("no settled epoch read rows or retried before the recheck")
	}
}

// One client's answer carries the client's data in the epoch the process
// read, and none while it has read no epoch.
func TestProviderStatusQueryAnswerCarriesTheProvidedData(t *testing.T) {
	reads := newProviderStatusQueryTestReads(t)
	network := newProviderStatusQueryTestNetwork(reads)
	callerSession := testingProviderStatusQuerySession(network.networkId, &network.callerClientId)

	result, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: network.callerClientId.String()}, callerSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 1 || result.Providers[0].Provided != nil {
		t.Fatalf("answer without a read epoch = %s", testingProviderStatusQueryJson(t, result))
	}

	reads.providedIndex = newProviderStatusProvidedIndex(9, []snprotocol.LedgerUsageRow{
		testingProviderStatusProvidedRow(network.callerClientId, 4096, false, false),
	})
	for _, format := range []string{"", ProviderStatusFormatCompact} {
		var provided *model.ProviderStatusProvided
		if format == "" {
			result, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: network.callerClientId.String()}, callerSession)
			if err != nil || len(result.Providers) != 1 {
				t.Fatalf("answer = %+v, %v", result, err)
			}
			provided = result.Providers[0].Provided
		} else {
			result, err := GetProviderStatusCompact(&GetProviderStatusArgs{ClientId: network.callerClientId.String()}, callerSession)
			if err != nil || len(result.Providers) != 1 {
				t.Fatalf("compact answer = %+v, %v", result, err)
			}
			provided = result.Providers[0].Provided
		}
		want := model.ProviderStatusProvided{Epoch: 9, UsageByteCount: 4096, Payout: model.ProviderStatusProvidedUnpaid}
		if provided == nil || *provided != want {
			t.Fatalf("format %q provided = %+v, want %+v", format, provided, want)
		}
	}
}
