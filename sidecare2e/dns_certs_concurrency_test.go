//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// DNS records and certificates under concurrency (the end-to-end plan's e2e-dns-certs-concurrency; the owner's
// end-to-end tests of 2026-11-21): N sidecars (ClientCount, 8 by default) spread over a few networks, each network aged
// past §21.7's probation, verified by a paid Pro balance and holding a document that grants its members admin on every
// one-label host of its zone (dns:*), so its devices write CNAME and TXT records through dns's api under a standing grant
// while readers query the dns edge.
//
// The records phase: every device writes, with random pauses, in an order of its own,
//
//   - its own TXT set, own-<n>.<label>.<root>, OwnTxtWriteCount times, each write a value no other write puts;
//   - its own CNAME set, link-<n>.<label>.<root>, OwnLinkWriteCount times, each to a .example target of its own: a
//     public name, which §12.8 takes only from a verified network (§12.7's user A records take public unicast
//     addresses alone, which no test may name);
//   - its network's shared TXT set, shared.<label>.<root>, SharedWriteCount times, each a compare-and-swap on the
//     zone's version the device read just before (§12.8's expect_version), read again and tried again on a conflict;
//
// while ReaderCount readers each read a random set at the edge between two reads of the root's SOA, whose serial is
// the root's (§12.6), every answer's signature verified against the world's KSK. Chaos restarts the dns API block
// once ChaosApiAfterCount writes were answered and the dns edge once ChaosEdgeAfterCount were, each on its stores.
//
// What must hold, strictly, with the root's journal as the record of what committed (dns_zone_journal: one row a
// serial, one serial a write):
//
//   - every answered write is in the journal once, at the version its answer named (a plain write sent again after an
//     unknown outcome at a serial no later than the version its second answer found), and no value of any write is
//     in the journal twice, nor any value no write made, so no update is lost and nothing is applied twice;
//   - each device's answered versions rise in the order it wrote, and each answered compare-and-swap committed right
//     after the version it expected: the zone's journal row before its own is that version's;
//   - every read is of a state the journal had at a serial between the two SOA reads around it: its values are those
//     of the set's newest row at or below the later serial, and no row of the set lies between that row and the
//     earlier serial; no reader sees the root's serial go back, across both restarts;
//   - once the edge serves the root's serial, every set answers its newest row's values, signed as §12.6 says; and
//     each zone's version, read through dns's api, is the serial of the zone's newest row;
//   - §12.7's dns_changes_per_hour, the dns write budget the phase spends, reconciles exactly: each network's
//     dns.records admin events since the phase began are its journal rows, and its owner's writes after the phase are
//     answered until the events reach the cap, exactly, where the next write is refused 429 with used at the limit.
//
// Each phase has PhaseTimeout and dumps the world's progress when it stalls. Run it through the build's wrapper:
// -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// What the case runs with (QD-702): every count, name, pause and bound it uses.
type dnsCertsSettings struct {
	// The networks the devices are spread over, and N, the devices across them, each network's owner among them.
	NetworkCount int
	ClientCount  int
	// The writes each device makes to its own TXT set, to its own CNAME set, and its compare-and-swaps on its
	// network's shared TXT set.
	OwnTxtWriteCount  int
	OwnLinkWriteCount int
	SharedWriteCount  int
	// The most version conflicts one compare-and-swap rides before it fails.
	CasTryCount int
	// The hosts of a device's own sets, %d its place among the case's devices, and its network's shared host.
	OwnTxtHostFormat  string
	OwnLinkHostFormat string
	WarmHostFormat    string
	SharedHost        string
	// The domain each CNAME write's target sits under, a reserved name (RFC 2606) no zone of the world's serves.
	LinkTargetDomain string
	// The readers of the dns edge throughout the phase, and the reads each makes.
	ReaderCount int
	ReadCount   int
	// How many answered writes the phase reaches before the dns API block restarts, and before the dns edge does.
	ChaosApiAfterCount  int
	ChaosEdgeAfterCount int
	// How old each network is made before anything reads its standing: past §21.7's probation, so public names are
	// its to write.
	NetworkAge time.Duration
	// The grant each network's document holds.
	DnsAclGrant string
	// §12.7's dns_changes_per_hour as the world's limits set it (sidecar_abuse:, every tier).
	ChangesPerHour int64
	// The most writes the budget's exhaustion makes past the phase before it fails.
	ExhaustMaxCount int
	// The journal rows one read takes.
	JournalPageCount int
	Dns              *WorldDnsSettings
	Bursts           *WorldBurstSettings
	// Each phase's bound, and the case's.
	PhaseTimeout time.Duration
	CaseTimeout  time.Duration
}

func defaultDnsCertsSettings() *dnsCertsSettings {
	bursts := DefaultWorldBurstSettings()
	bursts.MaxPause = 40 * time.Millisecond
	return &dnsCertsSettings{
		NetworkCount:        3,
		ClientCount:         8,
		OwnTxtWriteCount:    4,
		OwnLinkWriteCount:   3,
		SharedWriteCount:    3,
		CasTryCount:         60,
		OwnTxtHostFormat:    "own-%d",
		OwnLinkHostFormat:   "link-%d",
		WarmHostFormat:      "warm-%d",
		SharedHost:          "shared",
		LinkTargetDomain:    "target.example.",
		ReaderCount:         4,
		ReadCount:           80,
		ChaosApiAfterCount:  20,
		ChaosEdgeAfterCount: 45,
		NetworkAge:          8 * 24 * time.Hour,
		DnsAclGrant:         `{"src": ["autogroup:member"], "dst": ["dns:*"], "allow": ["admin"]}`,
		ChangesPerHour:      60,
		ExhaustMaxCount:     80,
		JournalPageCount:    500,
		Dns:                 DefaultWorldDnsSettings(),
		Bursts:              bursts,
		PhaseTimeout:        5 * time.Minute,
		CaseTimeout:         20 * time.Minute,
	}
}

// A world for the case: every block, the case's bound.
func newDnsCertsWorld(t testing.TB, settings *dnsCertsSettings) *World {
	worldSettings := DefaultWorldSettings()
	hostsSettings := defaultE2eHostsSettings()
	hostsSettings.CaseTimeout = settings.CaseTimeout
	worldSettings.Hosts = hostsSettings
	worldSettings.NetworkCount = settings.NetworkCount
	return NewWorld(t, worldSettings)
}

// The case's networks: ClientCount sidecars spread over NetworkCount networks, each aged and verified before anything
// reads its standing, holding the dns grant, its zone drawn, and every sidecar's first write through its own client
// answered, so the API's gate holds the grant before the phase.
func addDnsCertsNetworks(t testing.TB, world *World, settings *dnsCertsSettings) []*dnsCertsNetwork {
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	networks := []*dnsCertsNetwork{}
	index := 0
	for networkIndex := range settings.NetworkCount {
		clientCount := settings.ClientCount / settings.NetworkCount
		if networkIndex < settings.ClientCount%settings.NetworkCount {
			clientCount += 1
		}
		added, err := world.AddNetwork(ctx, worldNetworkName(networkIndex), clientCount, nil)
		if err != nil {
			world.FailProgress(t, "the enrollment", err, nil)
		}
		if err := worldAgeNetwork(ctx, added.NetworkId, settings.NetworkAge); err != nil {
			t.Fatal(err)
		}
		if err := world.VerifyNetwork(ctx, added, true, nil); err != nil {
			t.Fatalf("%s's paid balance: %v", added.Name, err)
		}
		network := &dnsCertsNetwork{WorldNetwork: added}
		for _, device := range added.Clients {
			writer, err := newDnsCertsWriter(world, network, device, index)
			if err != nil {
				t.Fatalf("%s's dns client: %v", device.Name, err)
			}
			network.Writers = append(network.Writers, writer)
			index += 1
		}
		networks = append(networks, network)
	}
	// one base a minute (§7.9): every network's document at once
	var saves sync.WaitGroup
	saveErrs := make([]error, len(networks))
	for networkIndex, network := range networks {
		saves.Go(func() {
			_, saveErrs[networkIndex] = world.SaveAclPolicy(ctx, network.WorldNetwork, network.Owner(), worldAclPolicy(settings.DnsAclGrant))
		})
	}
	saves.Wait()
	for networkIndex, err := range saveErrs {
		if err != nil {
			world.FailProgress(t, networks[networkIndex].Name+"'s dns grant", err, nil)
		}
	}
	for _, network := range networks {
		zone, err := network.Writers[0].zone(ctx, settings.Dns)
		if err != nil || zone.Apex() == "" {
			t.Fatalf("%s's zone: %+v, %v", network.Name, zone, err)
		}
		network.Root = dns.CanonicalName(zone.Root)
		network.Apex = dns.CanonicalName(zone.Apex())
		if network.Root != dns.CanonicalName(world.HostsSettings().DnsRoot) {
			t.Fatalf("%s's zone is under %s, not the world's root", network.Name, network.Root)
		}
		// each sidecar's first write, its grant's first use at the API, tried again while the API's gate follows the
		// document
		for _, writer := range network.Writers {
			set := dnsCertsSetKey{Owner: network.OwnerName(fmt.Sprintf(settings.WarmHostFormat, writer.Index)), Type: dns.TypeTXT}
			if _, err := world.SetDnsRecord(ctx, writer.Device, &sidecar.DnsRecordUpsert{Name: set.Owner, Type: "TXT", Values: []string{fmt.Sprintf("warm-%d", writer.Index)}}, settings.Dns); err != nil {
				world.FailProgress(t, writer.Device.Name+"'s first write", err, nil)
			}
		}
	}
	return networks
}

// The sets the records phase writes, every network's: each device's own TXT and CNAME sets, and the shared one.
func dnsCertsRecordSets(networks []*dnsCertsNetwork, settings *dnsCertsSettings) []dnsCertsSetKey {
	sets := []dnsCertsSetKey{}
	for _, network := range networks {
		for _, writer := range network.Writers {
			sets = append(sets,
				dnsCertsSetKey{Owner: network.OwnerName(fmt.Sprintf(settings.OwnTxtHostFormat, writer.Index)), Type: dns.TypeTXT},
				dnsCertsSetKey{Owner: network.OwnerName(fmt.Sprintf(settings.OwnLinkHostFormat, writer.Index)), Type: dns.TypeCNAME},
			)
		}
		sets = append(sets, dnsCertsSetKey{Owner: network.OwnerName(settings.SharedHost), Type: dns.TypeTXT})
	}
	return sets
}

// The kinds of a device's writes in the records phase.
const (
	dnsCertsWriteOwnTxt  = "own-txt"
	dnsCertsWriteOwnLink = "own-link"
	dnsCertsWriteShared  = "shared"
)

// Records under concurrency, with chaos on the dns API and the edge, then the dns write budget (see the file header).
func TestE2eDnsCertsConcurrency(t *testing.T) {
	settings := defaultDnsCertsSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		world := newDnsCertsWorld(t, settings)
		defer world.Close()
		networks := addDnsCertsNetworks(t, world, settings)
		dnsCertsRecordsPhase(t, world, settings, networks)
	})
}

// The records phase and its checks (see the file header).
func dnsCertsRecordsPhase(t testing.TB, world *World, settings *dnsCertsSettings, networks []*dnsCertsNetwork) {
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	root := networks[0].Root
	keys, err := newDnsCertsZoneKeys(ctx, world, settings.Dns)
	if err != nil {
		t.Fatal(err)
	}
	startSerial, err := dnsCertsRootSerial(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	phaseStart := server.NowUtc()
	baselineEvents := map[server.Id]int{}
	for _, network := range networks {
		eventTimes, err := dnsCertsChangeEvents(ctx, network.NetworkId, phaseStart.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		baselineEvents[network.NetworkId] = len(eventTimes)
	}
	sets := dnsCertsRecordSets(networks, settings)
	// each zone's version as the phase begins, what its first compare-and-swap may expect
	startVersions := map[server.Id]uint64{}
	for _, network := range networks {
		zone, err := network.Writers[0].zone(ctx, settings.Dns)
		if err != nil {
			t.Fatalf("%s's zone: %v", network.Name, err)
		}
		startVersions[network.NetworkId] = zone.Version
	}

	// the writers, the readers and chaos, all at once
	answered := sidecar.NewMonitorValue(0)
	writersDone := sidecar.NewMonitorValue(0)
	bursts := []*WorldBurst{}
	writers := []*dnsCertsWriter{}
	for _, network := range networks {
		writers = append(writers, network.Writers...)
	}
	for _, writer := range writers {
		plan := []string{}
		for range settings.OwnTxtWriteCount {
			plan = append(plan, dnsCertsWriteOwnTxt)
		}
		for range settings.OwnLinkWriteCount {
			plan = append(plan, dnsCertsWriteOwnLink)
		}
		for range settings.SharedWriteCount {
			plan = append(plan, dnsCertsWriteShared)
		}
		rand.New(rand.NewPCG(settings.Bursts.Seed, uint64(1000+writer.Index))).Shuffle(len(plan), func(i int, j int) {
			plan[i], plan[j] = plan[j], plan[i]
		})
		counts := map[string]int{}
		network := writer.Network
		burst := &WorldBurst{Name: "writer " + writer.Device.Name, StepCount: len(plan)}
		burst.Step = func(ctx context.Context, index int, random *rand.Rand) (returnErr error) {
			// a writer's burst ends at its last step or its first error, which chaos waits on beside the answers
			defer func() {
				if returnErr != nil || index == len(plan)-1 {
					writersDone.Update(func(count int) int { return count + 1 })
				}
			}()
			kind := plan[index]
			sequence := counts[kind]
			counts[kind] += 1
			var set dnsCertsSetKey
			var value string
			cas := false
			switch kind {
			case dnsCertsWriteOwnTxt:
				set = dnsCertsSetKey{Owner: network.OwnerName(fmt.Sprintf(settings.OwnTxtHostFormat, writer.Index)), Type: dns.TypeTXT}
				value = fmt.Sprintf("w%d-txt-%d", writer.Index, sequence)
			case dnsCertsWriteOwnLink:
				set = dnsCertsSetKey{Owner: network.OwnerName(fmt.Sprintf(settings.OwnLinkHostFormat, writer.Index)), Type: dns.TypeCNAME}
				value = fmt.Sprintf("w%d-link-%d.%s", writer.Index, sequence, settings.LinkTargetDomain)
			case dnsCertsWriteShared:
				set = dnsCertsSetKey{Owner: network.OwnerName(settings.SharedHost), Type: dns.TypeTXT}
				value = fmt.Sprintf("w%d-shared-%d", writer.Index, sequence)
				cas = true
			}
			write, err := writer.Write(ctx, set, value, cas, settings.CasTryCount, settings.Dns)
			if err != nil {
				return err
			}
			if write.Answered {
				answered.Update(func(count int) int { return count + 1 })
			}
			return nil
		}
		bursts = append(bursts, burst)
	}
	readers := []*dnsCertsReader{}
	for index := range settings.ReaderCount {
		reader := &dnsCertsReader{Name: fmt.Sprintf("reader-%d", index)}
		readers = append(readers, reader)
		bursts = append(bursts, &WorldBurst{
			Name:      reader.Name,
			StepCount: settings.ReadCount,
			Step: func(ctx context.Context, index int, random *rand.Rand) error {
				return reader.Read(ctx, world, keys, sets, random, settings.Dns)
			},
		})
	}
	// chaos: each restart once the phase has answered its count of writes, or not at all once every writer ended
	chaosRestarts := []string{}
	var chaosLock sync.Mutex
	chaos := []struct {
		block string
		after int
	}{
		{block: WorldBlockDns, after: settings.ChaosApiAfterCount},
		{block: WorldBlockDnsedge, after: settings.ChaosEdgeAfterCount},
	}
	bursts = append(bursts, &WorldBurst{
		Name:      "chaos",
		StepCount: len(chaos),
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			for {
				count, countChanged := answered.Get()
				done, doneChanged := writersDone.Get()
				if done == len(writers) {
					return nil
				}
				if chaos[index].after <= count {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-countChanged:
				case <-doneChanged:
				}
			}
			if err := world.Restart(chaos[index].block); err != nil {
				return fmt.Errorf("%s's restart: %w", chaos[index].block, err)
			}
			chaosLock.Lock()
			defer chaosLock.Unlock()
			chaosRestarts = append(chaosRestarts, chaos[index].block)
			return nil
		},
	})
	if err := RunBursts(ctx, t, "the records phase", bursts, settings.Bursts); err != nil {
		world.FailProgress(t, "the records phase", err, bursts)
	}
	FailBurstErrors(t, bursts)
	if len(chaosRestarts) != len(chaos) {
		t.Errorf("chaos restarted %v, not every one of the dns API and the edge: the writers ended first", chaosRestarts)
	}
	skipped := 0
	readCount := 0
	for _, reader := range readers {
		skipped += reader.Skipped()
		readCount += len(reader.Reads())
	}
	t.Logf("the records phase: %d writes answered, %d reads kept, %d skipped while the edge restarted, restarts %v", answered.Value(), readCount, skipped, chaosRestarts)

	// what committed: the journal from the phase's start to the root's serial now, which the edge must reach
	endSerial, err := dnsCertsRootSerial(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := world.AwaitDnsAnswer(ctx, root, dns.TypeSOA, func(answer *dns.Msg) bool {
		for _, record := range answer.Answer {
			if soa, ok := record.(*dns.SOA); ok && endSerial <= uint64(soa.Serial) {
				return true
			}
		}
		return false
	}, settings.Dns); err != nil {
		world.FailProgress(t, "the edge's catch-up", err, nil)
	}
	rows, err := dnsCertsJournal(ctx, root, startSerial, settings.JournalPageCount)
	if err != nil {
		t.Fatal(err)
	}
	history := dnsCertsCheckJournal(t, networks, sets, rows, startVersions)
	dnsCertsCheckReads(t, readers, history)
	dnsCertsCheckServed(t, ctx, world, keys, settings, sets, history)
	dnsCertsCheckZoneVersions(t, ctx, settings, networks, rows)
	dnsCertsCheckChangesBudget(t, ctx, settings, networks, rows, baselineEvents, phaseStart)
}

// One committed state of a set: the serial of its journal row and the values it holds then, none for a delete.
type dnsCertsCommit struct {
	Serial uint64
	Values []string
}

// The serials of the journal rows that wrote network's zone, at or below its apex, in order.
func dnsCertsZoneSerials(network *dnsCertsNetwork, rows []dnsservice.DnsJournalRow) []uint64 {
	serials := []uint64{}
	inZone := func(owner string) bool {
		owner = dns.CanonicalName(owner)
		return owner == network.Apex || strings.HasSuffix(owner, "."+network.Apex)
	}
	for _, row := range rows {
		wrote := false
		for _, upsert := range row.Upserts {
			wrote = wrote || inZone(upsert.Owner)
		}
		for _, deleted := range row.Deletes {
			wrote = wrote || inZone(deleted.Owner)
		}
		if wrote {
			serials = append(serials, row.Serial)
		}
	}
	return serials
}

// The serials of the journal rows that wrote a user record of network's zone (§12.7's user rows, which alone take a
// dns.records admin event and count against dns_changes_per_hour), in order: a challenge's or a system row's write of
// the zone moves its version but is no user change.
func dnsCertsZoneUserSerials(network *dnsCertsNetwork, rows []dnsservice.DnsJournalRow) []uint64 {
	serials := []uint64{}
	for _, row := range rows {
		for _, upsert := range row.Upserts {
			owner := dns.CanonicalName(upsert.Owner)
			if upsert.Kind == dnsservice.DnsRecordKindUser && (owner == network.Apex || strings.HasSuffix(owner, "."+network.Apex)) {
				serials = append(serials, row.Serial)
				break
			}
		}
	}
	return serials
}

// Each set's commits since the phase began, in serial order, from the journal, after every check of the writes
// against it: answered writes there once at their version, nothing twice and nothing no write made, versions rising in
// each device's order, and each compare-and-swap committed right after the zone's version it expected, startVersions
// holding each zone's as the phase began (see the file header).
func dnsCertsCheckJournal(t testing.TB, networks []*dnsCertsNetwork, sets []dnsCertsSetKey, rows []dnsservice.DnsJournalRow, startVersions map[server.Id]uint64) map[dnsCertsSetKey][]*dnsCertsCommit {
	t.Helper()
	tracked := map[dnsCertsSetKey]bool{}
	for _, set := range sets {
		tracked[set] = true
	}
	history := map[dnsCertsSetKey][]*dnsCertsCommit{}
	// each value's serials in the journal
	valueSerials := map[string][]uint64{}
	for _, row := range rows {
		for _, upsert := range row.Upserts {
			set := dnsCertsSetKey{Owner: dns.CanonicalName(upsert.Owner), Type: upsert.Type}
			if !tracked[set] {
				continue
			}
			values, err := dnsCertsRdataValues(upsert)
			if err != nil {
				t.Errorf("serial %d: %v", row.Serial, err)
				continue
			}
			if len(values) != 1 {
				t.Errorf("serial %d committed %s with %d values, %v, where every write puts one", row.Serial, set, len(values), values)
			}
			for _, value := range values {
				valueSerials[value] = append(valueSerials[value], row.Serial)
			}
			history[set] = append(history[set], &dnsCertsCommit{Serial: row.Serial, Values: values})
		}
		for _, deleted := range row.Deletes {
			set := dnsCertsSetKey{Owner: dns.CanonicalName(deleted.Owner), Type: deleted.Type}
			if tracked[set] {
				t.Errorf("serial %d deleted %s, which no write deletes", row.Serial, set)
				history[set] = append(history[set], &dnsCertsCommit{Serial: row.Serial})
			}
		}
	}
	made := map[string]bool{}
	for _, network := range networks {
		zoneSerials := dnsCertsZoneSerials(network, rows)
		for _, writer := range network.Writers {
			previous := uint64(0)
			for _, write := range writer.Writes() {
				made[write.Value] = true
				serials := valueSerials[write.Value]
				switch {
				case 1 < len(serials):
					t.Errorf("%s's write of %q to %s is in the journal %d times, at %v: applied more than once", write.Writer, write.Value, write.Set, len(serials), serials)
				case write.Answered && !write.Unknown && (len(serials) != 1 || serials[0] != write.Version):
					t.Errorf("%s's write of %q to %s was answered at version %d, and the journal holds it at %v: an acknowledged update lost or moved", write.Writer, write.Value, write.Set, write.Version, serials)
				case write.Answered && write.Unknown && (len(serials) != 1 || write.Version < serials[0]):
					t.Errorf("%s's write of %q to %s, sent again after an unknown outcome, was answered at version %d, and the journal holds it at %v", write.Writer, write.Value, write.Set, write.Version, serials)
				}
				if !write.Answered || write.Unknown {
					continue
				}
				if write.Version <= previous {
					t.Errorf("%s's answers went back: version %d after %d", write.Writer, write.Version, previous)
				}
				previous = write.Version
				if write.Cas {
					// the zone's row before this one is the version the compare-and-swap expected
					before := startVersions[network.NetworkId]
					for _, serial := range zoneSerials {
						if write.Version <= serial {
							break
						}
						before = serial
					}
					if before != write.Expect {
						t.Errorf("%s's compare-and-swap of %q expected version %d and committed at %d, after the zone's row at %d", write.Writer, write.Value, write.Expect, write.Version, before)
					}
				}
			}
		}
	}
	for value, serials := range valueSerials {
		if !made[value] {
			t.Errorf("the journal holds %q at %v, which no write of the case put", value, serials)
		}
	}
	return history
}

// The set's state at serial: the values of its newest commit at or below it and that commit's serial, or none and 0
// before its first commit since the phase began, which the phase's sets do not hold.
func dnsCertsStateAt(commits []*dnsCertsCommit, serial uint64) *dnsCertsCommit {
	state := &dnsCertsCommit{}
	for _, commit := range commits {
		if serial < commit.Serial {
			break
		}
		state = commit
	}
	return state
}

// Every read was of a state the journal had at a serial between the two SOA reads around it (see the file header).
func dnsCertsCheckReads(t testing.TB, readers []*dnsCertsReader, history map[dnsCertsSetKey][]*dnsCertsCommit) {
	t.Helper()
	for _, reader := range readers {
		for _, read := range reader.Reads() {
			commits := history[read.Set]
			served := -1
			for index, commit := range commits {
				if slices.Equal(commit.Values, read.Values) {
					served = index
				}
			}
			newestBefore := dnsCertsStateAt(commits, read.Before)
			switch {
			case len(read.Values) == 0 && newestBefore.Serial != 0:
				t.Errorf("%s read %s empty between serials %d and %d, after its commit at %d", read.Reader, read.Set, read.Before, read.After, newestBefore.Serial)
			case len(read.Values) == 0:
			case served < 0:
				t.Errorf("%s read %s as %v between serials %d and %d, which no commit holds", read.Reader, read.Set, read.Values, read.Before, read.After)
			case read.After < commits[served].Serial:
				t.Errorf("%s read %s as %v, committed at %d, from an edge at serial %d", read.Reader, read.Set, read.Values, commits[served].Serial, read.After)
			case commits[served].Serial < newestBefore.Serial:
				t.Errorf("%s read %s as %v, committed at %d, from an edge already at serial %d, which holds the commit at %d", read.Reader, read.Set, read.Values, commits[served].Serial, read.Before, newestBefore.Serial)
			}
		}
	}
}

// Every set answers its newest commit's values at the edge, signed (see the file header).
func dnsCertsCheckServed(t testing.TB, ctx context.Context, world *World, keys *dnsCertsZoneKeys, settings *dnsCertsSettings, sets []dnsCertsSetKey, history map[dnsCertsSetKey][]*dnsCertsCommit) {
	t.Helper()
	for _, set := range sets {
		commits := history[set]
		if len(commits) == 0 {
			t.Errorf("%s has no commit since the phase began", set)
			continue
		}
		want := commits[len(commits)-1].Values
		answer, err := world.AwaitDnsAnswer(ctx, set.Owner, set.Type, func(answer *dns.Msg) bool {
			return slices.Equal(dnsCertsAnswerValues(answer, set), want)
		}, settings.Dns)
		if err != nil {
			t.Errorf("%s at the edge: %v, not its newest commit's %v at %d", set, err, want, commits[len(commits)-1].Serial)
			continue
		}
		if err := keys.Verify(answer, set.Owner, set.Type, time.Now()); err != nil {
			t.Errorf("%s at the edge: %v", set, err)
		}
	}
}

// Each zone's version through dns's api is the serial of its newest journal row (see the file header).
func dnsCertsCheckZoneVersions(t testing.TB, ctx context.Context, settings *dnsCertsSettings, networks []*dnsCertsNetwork, rows []dnsservice.DnsJournalRow) {
	t.Helper()
	for _, network := range networks {
		zoneSerials := dnsCertsZoneSerials(network, rows)
		if len(zoneSerials) == 0 {
			t.Errorf("%s's zone has no journal row since the phase began", network.Name)
			continue
		}
		zone, err := network.Writers[len(network.Writers)-1].zone(ctx, settings.Dns)
		if err != nil {
			t.Errorf("%s's zone: %v", network.Name, err)
			continue
		}
		if newest := zoneSerials[len(zoneSerials)-1]; zone.Version != newest {
			t.Errorf("%s's zone reads version %d through dns's api, and its newest journal row is at %d", network.Name, zone.Version, newest)
		}
	}
}

// §12.7's dns_changes_per_hour reconciles exactly: each network's events since the phase began are its journal rows,
// and its owner's writes are answered until the events reach the cap, where the next is refused at the limit (see the
// file header). baselineEvents holds each network's events in the hour before the phase.
func dnsCertsCheckChangesBudget(t testing.TB, ctx context.Context, settings *dnsCertsSettings, networks []*dnsCertsNetwork, rows []dnsservice.DnsJournalRow, baselineEvents map[server.Id]int, phaseStart time.Time) {
	t.Helper()
	for _, network := range networks {
		zoneRows := len(dnsCertsZoneUserSerials(network, rows))
		eventTimes, err := dnsCertsChangeEvents(ctx, network.NetworkId, phaseStart)
		if err != nil {
			t.Fatal(err)
		}
		if len(eventTimes) != zoneRows {
			t.Errorf("%s holds %d dns.records events since the phase began, for %d journal rows of user records in its zone", network.Name, len(eventTimes), zoneRows)
		}
		owner := network.Writers[0]
		set := dnsCertsSetKey{Owner: network.OwnerName(fmt.Sprintf(settings.OwnTxtHostFormat, owner.Index)), Type: dns.TypeTXT}
		spent := baselineEvents[network.NetworkId] + len(eventTimes)
		exhausted := false
		for sequence := range settings.ExhaustMaxCount {
			write, err := owner.Write(ctx, set, fmt.Sprintf("w%d-exhaust-%d", owner.Index, sequence), false, settings.CasTryCount, settings.Dns)
			if refusal := dnsCertsChangesRefusal(err); refusal != nil {
				if refusal.Limit != settings.ChangesPerHour || refusal.Used != refusal.Limit || int64(spent) != refusal.Limit {
					t.Errorf("%s's write past its changes was refused with used %d of %d, after %d changes the case counted against a cap of %d", network.Name, refusal.Used, refusal.Limit, spent, settings.ChangesPerHour)
				}
				exhausted = true
				break
			}
			if err != nil {
				t.Errorf("%s's write while exhausting its changes: %v", network.Name, err)
				break
			}
			if write.Unknown {
				t.Errorf("%s's write while exhausting its changes had an unknown outcome, with no chaos running", network.Name)
				break
			}
			spent += 1
		}
		if !exhausted {
			t.Errorf("%s's writes past the phase were never refused for dns_changes_per_hour: %d spent", network.Name, spent)
		}
		hourEvents, err := dnsCertsChangeEvents(ctx, network.NetworkId, phaseStart.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(hourEvents)) != settings.ChangesPerHour {
			t.Errorf("%s holds %d dns.records events in the hour, after its changes were refused at %d", network.Name, len(hourEvents), settings.ChangesPerHour)
		}
	}
}
