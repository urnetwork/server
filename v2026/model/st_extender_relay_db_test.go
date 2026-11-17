// The SN25 extender pool's relay read (GetStEpochExtenderRelay) on a real
// database: contract_extender walked by create time from the lookback before
// the earning window, joined to each contract's live row, counting an extender
// party's unpooled share of a funded, non-measurement contract that closed in
// the window with a credited outcome; read whole or in slices to the same
// totals, a failed slice read again from nothing, and a refused row refused at
// once. Each test runs on the test environment's fresh database, with every
// migration applied.
//
// The defect (owner 2026-11-16, D1): the pool was split by unit alone, so an
// extender that relayed nothing took as much of Z' as one that carried the
// clients; half of Z' now follows these relay bytes.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/v2026/ledger"

	"github.com/urnetwork/server/v2026"
)

// One contract and its extender parties as the relay read finds them.
type stRelayDbContract struct {
	created, closed time.Time
	// nil writes an open outcome
	outcome   *string
	origin    server.Id
	payer     *server.Id
	providers []contractProviderUsage
	// the provider clients of its extender parties, one row each, on
	// alternating sides
	parties []server.Id
	// writes a NULL usage direction, as a legacy contract has
	legacyDirection bool
}

func stRelayDbOutcome(outcome string) *string { return &outcome }

// Writes the contract's live row and its parties' rows, created together as
// the contract's parties are, and returns its id.
func addStRelayDbContract(t testing.TB, ctx context.Context, c stRelayDbContract) server.Id {
	t.Helper()
	var total ByteCount
	for _, provider := range c.providers {
		total += provider.ByteCount
	}
	usage, err := json.Marshal(&contractUsageSnapshot{Version: 1, ByteCount: total, Providers: c.providers})
	if err != nil {
		t.Fatal(err)
	}
	var originIsSource *bool
	if !c.legacyDirection {
		source := true
		originIsSource = &source
	}
	contractId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (contract_id,source_id,source_network_id,destination_id,destination_network_id,
				transfer_byte_count,outcome,create_time,close_time,provider_usage,usage_origin_is_source,payer_network_id)
			VALUES ($1,$2,$3,$4,$5,1000000,$6,$7,$8,$9,$10,$11)
		`, contractId, server.NewId(), c.origin, server.NewId(), server.NewId(), c.outcome, c.created, c.closed, string(usage), originIsSource, c.payer))
		for i, clientId := range c.parties {
			party := "source"
			if i%2 == 1 {
				party = "destination"
			}
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO contract_extender (contract_id,extender_id,party,client_id,network_id,create_time)
				VALUES ($1,$2,$3,$4,$5,$6)
			`, contractId, server.NewId(), party, clientId, server.NewId(), c.created))
		}
	})
	return contractId
}

// A three-hour window's contracts with extender parties, and the relay bytes
// they add up to.
type stRelayDbFixture struct {
	start, end  time.Time
	measurement map[server.Id]bool
	amendment   StExtenderAmendment
	ext, ext2   server.Id
	want        map[server.Id]ByteCount
}

// The window's contracts, each a case of the rule:
//   - c1: the extender's hop counts, the receiver's share on the origin's
//     network does not, though the receiver is a party too;
//   - c2: created in the lookback, closed in the window: counts;
//   - c3: created before the lookback: does not;
//   - c4: closed at the window's end, c5 before its start: do not;
//   - c6: closed in the window with an open outcome: does not;
//   - c7 unfunded, c8 paid by the measurement network: do not;
//   - c9: archived, so it has no live row: does not;
//   - c10: the second extender named for both sides counts once;
//   - c11: a provider that is no party does not count.
func newStRelayDbFixture(t testing.TB) *stRelayDbFixture {
	t.Helper()
	ctx := t.Context()
	start := time.Date(2026, 11, 27, 18, 25, 0, 0, time.UTC)
	f := &stRelayDbFixture{start: start, end: start.Add(3 * time.Hour), ext: server.NewId(), ext2: server.NewId(),
		amendment: StExtenderAmendment{FirstEpoch: 8}}
	receiver, provider := server.NewId(), server.NewId()
	extNetwork, ext2Network, providerNetwork, origin, funded, measurement := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	f.measurement = map[server.Id]bool{measurement: true}
	settled := stRelayDbOutcome(string(ContractOutcomeSettled))
	ext := func(bytes ByteCount) contractProviderUsage {
		return contractProviderUsage{ClientId: f.ext, NetworkId: extNetwork, ByteCount: bytes}
	}
	at := func(offset time.Duration) time.Time { return start.Add(offset) }
	contract := func(created, closed time.Time, outcome *string, payer *server.Id, providers []contractProviderUsage, parties ...server.Id) stRelayDbContract {
		return stRelayDbContract{created: created, closed: closed, outcome: outcome, origin: origin, payer: payer, providers: providers, parties: parties}
	}
	for _, c := range []stRelayDbContract{
		contract(at(time.Minute), at(2*time.Minute), settled, &funded,
			[]contractProviderUsage{ext(500), {ClientId: receiver, NetworkId: origin, ByteCount: 300}}, f.ext, receiver),
		contract(at(-23*time.Hour), at(5*time.Minute), stRelayDbOutcome(string(ContractOutcomeDisputeResolvedToSource)), &funded,
			[]contractProviderUsage{ext(40)}, f.ext),
		contract(at(-25*time.Hour), at(6*time.Minute), settled, &funded, []contractProviderUsage{ext(7)}, f.ext),
		contract(at(10*time.Minute), f.end, settled, &funded, []contractProviderUsage{ext(11)}, f.ext),
		contract(at(-2*time.Hour), at(-time.Minute), settled, &funded, []contractProviderUsage{ext(13)}, f.ext),
		contract(at(20*time.Minute), at(30*time.Minute), nil, &funded, []contractProviderUsage{ext(17)}, f.ext),
		contract(at(31*time.Minute), at(32*time.Minute), settled, nil, []contractProviderUsage{ext(1_000)}, f.ext),
		contract(at(33*time.Minute), at(34*time.Minute), settled, &measurement, []contractProviderUsage{ext(2_000)}, f.ext),
		contract(at(70*time.Minute), at(80*time.Minute), stRelayDbOutcome(string(ContractOutcomeDisputeResolvedToDestination)), &funded,
			[]contractProviderUsage{{ClientId: f.ext2, NetworkId: ext2Network, ByteCount: 100}}, f.ext2, f.ext2),
		contract(at(150*time.Minute), at(170*time.Minute), settled, &funded,
			[]contractProviderUsage{{ClientId: f.ext2, NetworkId: ext2Network, ByteCount: 50}, {ClientId: provider, NetworkId: providerNetwork, ByteCount: 70}}, f.ext2),
	} {
		addStRelayDbContract(t, ctx, c)
	}
	// c9: archived, and its live row gone with the contract horizon; the
	// delete archives it, and its party row stays
	archived := addStRelayDbContract(t, ctx, contract(at(40*time.Minute), at(50*time.Minute), settled, &funded, []contractProviderUsage{ext(4_000)}, f.ext))
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id = $1`, archived))
	})
	server.Db(ctx, func(conn server.PgConn) {
		var archivedRows, parties int
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_provider_usage_archive WHERE contract_id = $1`, archived).Scan(&archivedRows))
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_extender WHERE contract_id = $1`, archived).Scan(&parties))
		if archivedRows != 1 || parties != 1 {
			t.Fatalf("the archived contract has %d archived rows and %d parties", archivedRows, parties)
		}
	})
	f.want = map[server.Id]ByteCount{f.ext: 500 + 40, f.ext2: 100 + 50}
	return f
}

// The relay read counts exactly the rule's contracts (c1, c2, c10, c11), and
// reads the same totals whole, in hour slices and in slices that cut the
// window anywhere. Without a lookback, a contract created before the window
// opened adds nothing; an epoch the amendment does not cover reads nothing.
func TestStExtenderDbRelayCountsTheRulesContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newStRelayDbFixture(t)
		read := func(epoch uint64, settings *StEpochRelaySettings) *StEpochExtenderRelay {
			t.Helper()
			relay, err := GetStEpochExtenderRelay(ctx, epoch, f.start, f.end, f.amendment, f.measurement, settings)
			if err != nil {
				t.Fatal(err)
			}
			return relay
		}
		for _, span := range []time.Duration{0, time.Hour, 7 * time.Minute, 48 * time.Hour} {
			settings := DefaultStEpochRelaySettings()
			settings.Span = span
			if relay := read(8, settings); !maps.Equal(relay.ClientIdRelayBytes, f.want) || relay.Contracts != 4 {
				t.Fatalf("span %s: relay %v in %d contracts, want %v in 4", span, relay.ClientIdRelayBytes, relay.Contracts, f.want)
			}
		}
		settings := DefaultStEpochRelaySettings()
		settings.Lookback = 0
		if relay := read(8, settings); !maps.Equal(relay.ClientIdRelayBytes, map[server.Id]ByteCount{f.ext: 500, f.ext2: 150}) || relay.Contracts != 3 {
			t.Fatalf("without a lookback: relay %v in %d contracts", relay.ClientIdRelayBytes, relay.Contracts)
		}
		reads := 0
		t.Cleanup(Testing_SetStEpochRelaySliceHook(func(time.Time, int) error {
			reads++
			return nil
		}))
		if relay := read(7, DefaultStEpochRelaySettings()); len(relay.ClientIdRelayBytes) != 0 || relay.Contracts != 0 || reads != 0 {
			t.Fatalf("an epoch before the amendment: relay %+v after %d reads", relay, reads)
		}
	})
}

// A slice that fails is read again from nothing, so its contracts count once;
// a slice that fails every attempt fails the read; and a row the usage read
// would refuse is refused at once, without another attempt.
func TestStExtenderDbRelaySlicesRetryFromNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newStRelayDbFixture(t)
		settings := &StEpochRelaySettings{Span: time.Hour, Lookback: 24 * time.Hour, Attempts: 2, RetryDelay: time.Millisecond}
		// the window's first hour, which holds c1, fails once
		attempts := map[time.Time]int{}
		failing := f.start
		t.Cleanup(Testing_SetStEpochRelaySliceHook(func(sliceStart time.Time, attempt int) error {
			attempts[sliceStart] = attempt
			if sliceStart.Equal(failing) && attempt == 1 {
				return errors.New("a dropped connection")
			}
			return nil
		}))
		relay, err := GetStEpochExtenderRelay(ctx, 8, f.start, f.end, f.amendment, f.measurement, settings)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(relay.ClientIdRelayBytes, f.want) || relay.Contracts != 4 || attempts[failing] != 2 || len(attempts) != 27 {
			t.Fatalf("relay %v in %d contracts, %d slices, the failed one read %d times", relay.ClientIdRelayBytes, relay.Contracts, len(attempts), attempts[failing])
		}

		settings.Attempts = 1
		if _, err := GetStEpochExtenderRelay(ctx, 8, f.start, f.end, f.amendment, f.measurement, settings); err == nil || !strings.Contains(err.Error(), "a dropped connection") {
			t.Fatalf("a slice failing its only attempt: %v", err)
		}

		// a funded contract with bytes and no usage origin, in the last hour
		settings.Attempts = 3
		payer := server.NewId()
		addStRelayDbContract(t, ctx, stRelayDbContract{created: f.start.Add(130 * time.Minute), closed: f.start.Add(140 * time.Minute),
			outcome: stRelayDbOutcome(string(ContractOutcomeSettled)), origin: server.NewId(), payer: &payer,
			providers: []contractProviderUsage{{ClientId: f.ext, NetworkId: server.NewId(), ByteCount: 3}}, parties: []server.Id{f.ext}, legacyDirection: true})
		clear(attempts)
		_, err = GetStEpochExtenderRelay(ctx, 8, f.start, f.end, f.amendment, f.measurement, settings)
		var refusal *stEpochUsageRefusal
		last := f.start.Add(2 * time.Hour)
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "no usage origin") || attempts[last] != 1 {
			t.Fatalf("a refused row: %v after %d attempts", err, attempts[last])
		}
	})
}

// An epoch's extender component is recorded with its relay chunks, fixed with
// the rest of it: recording the same again is a no-op, and other relay chunks,
// or none, are refused. A component recorded before the relay chunks column,
// which its release left null, reads as one built with no relay rows, so the
// same component with none records again as a no-op and one with relay chunks
// is refused. Relay chunks without a component are a component recorded in
// part.
func TestStExtenderDbEpochRowsRelayChunks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		key := testStDeploymentKey
		chunks := []string{ledger.RefOf([]byte("p8")).String()}
		component := func(relay ...string) *StLedgerV2EpochExtenders {
			return &StLedgerV2EpochExtenders{Bytes: 500, CensusChunks: []string{ledger.RefOf([]byte("c8")).String()}, RelayChunks: relay,
				IntraPlatform: 0, IntraNetwork: 500, Contracts: 1, Payers: 1}
		}
		relay := []string{ledger.RefOf([]byte("r1")).String(), ledger.RefOf([]byte("r2")).String()}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 8, chunks, component(relay...)); err != nil {
			t.Fatal(err)
		}
		if record := GetStLedgerV2EpochRows(ctx, key, 1, 8, 8)[8]; !slices.Equal(record.Extenders.RelayChunks, relay) || !record.Extenders.Equal(component(relay...)) {
			t.Fatalf("record %+v", record.Extenders)
		}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 8, chunks, component(relay...)); err != nil {
			t.Fatalf("recording the same again: %v", err)
		}
		for name, other := range map[string]*StLedgerV2EpochExtenders{
			"other relay chunks": component(relay[:1]...),
			"no relay chunks":    component(),
		} {
			if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 8, chunks, other); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
				t.Fatalf("%s: %v", name, err)
			}
		}

		// epoch 9's component, as a release from before the column wrote it
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 9, chunks, component()); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_ledger_v2_epoch_rows SET extender_relay_chunks = NULL WHERE epoch = 9`))
		})
		if record := GetStLedgerV2EpochRows(ctx, key, 1, 9, 9)[9]; record.Extenders == nil || record.Extenders.RelayChunks == nil || len(record.Extenders.RelayChunks) != 0 {
			t.Fatalf("a component recorded before the relay chunks %+v", record.Extenders)
		}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 9, chunks, component()); err != nil {
			t.Fatalf("recording the same component with no relay rows again: %v", err)
		}
		if err := RecordStLedgerV2EpochComponents(ctx, key, 1, 9, chunks, component(relay...)); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("relay chunks for a component recorded without them: %v", err)
		}

		// relay chunks on an epoch recorded without a component
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 10, chunks); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_ledger_v2_epoch_rows SET extender_relay_chunks = '{}' WHERE epoch = 10`))
		})
		refused := func() (refusal any) {
			defer func() { refusal = recover() }()
			GetStLedgerV2EpochRows(ctx, key, 1, 10, 10)
			return nil
		}()
		if refused == nil || !strings.Contains(fmt.Sprint(refused), "recorded in part") {
			t.Fatalf("relay chunks without a component: %v", refused)
		}
	})
}
