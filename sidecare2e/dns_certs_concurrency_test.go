//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// DNS records and certificates under concurrency (the end-to-end plan's e2e-dns-certs-concurrency; the owner's
// end-to-end tests of 2026-11-21): N sidecars (ClientCount, 8 by default) spread over a few networks, each network aged
// past §21.7's probation, verified by a paid Pro balance and holding a document that grants its members admin on every
// one-label host of its zone (dns:*), so its devices write CNAME and TXT records through dns's api under a standing grant
// while readers query the dns edge, and a standing public publish of tls/443/web, so each network's first sidecars
// order the network's certificate. The world runs the real certificate chain in place of its stand-in
// (World.IssueTlsCertificateRow): §13.8's certs worker, api's certs routes and Pebble, beside the stand-ins
// dns_certs_concurrency_certs_test.go names (the canary monitor's validator, the internet's resolvers) and those of
// the delegated phase (dns_certs_concurrency_desktop_test.go): the custom domain's and host's rows, which a Team's
// routes write and the case's Pro networks may not, and the secret repos' key files, a map the desktops share.
//
// The records phase: every device writes, with random pauses, in an order of its own,
//
//   - its own TXT set, own-<n>.<label>.<root>, OwnTxtWriteCount times, each write a value no other write puts;
//   - its own CNAME set, link-<n>.<label>.<root>, OwnLinkWriteCount times, each to a .example target of its own: a
//     public name, which §12.8 takes only from a verified network (§12.7's user A records take public unicast
//     addresses alone, which no test may name);
//   - its network's shared TXT set, shared.<label>.<root>, SharedWriteCount times, each a compare-and-swap on the
//     zone's version the device read just before (§12.8's expect_version), read again and tried again on a conflict;
//   - its network's TXT set at the owner a custom domain's host maps to, web.<label>.<root> (§12.8 step 2), which the
//     tls/ name publishes, OwnerWriteCount times, each a compare-and-swap as the shared set's: what the host answers
//     through its customer's CNAME once the delegated phase maps it;
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
//   - the edge's addresses (§12.7's * edge records, owner decision 335, which api's sync after the document's base
//     wrote): each network's tls/ name's owner, which holds a TXT set of its own and so answers through its copy of
//     *'s records (QD-1190), and a host no write names answer A and AAAA with the world's lb interfaces, exactly and
//     signed, and the apex, which no grant publishes, answers no address.
//
// The certificates phase: each network's first CertDeviceCount sidecars, two by default, run a tls component and ask
// for their network's certificate, all at once, as `ur bridge public-tls` does: POST /tls/cert-order through api's certs
// routes, which queue the rows; the certs worker's order pass, kicked once every row is queued, admits each under the
// policy and orders it from Pebble by DNS-01, so one pass holds two orders of each set, {<label>.<root>,
// *.<label>.<root>}, whose four authorizations validate at the one name _acme-challenge.<label>.<root>, which holds at
// most two values (§12.7): as certsservice is built, its solver keeps a name's values together and refuses a third,
// and the order it refuses, which has already opened its order at Pebble and spent a unit of the budget as a new set,
// queues again for the next pass, 30 s later, which admits it as a repeated set (QD-1189 has the admission hold a
// set's second order back until the first has issued instead; the checks hold either way). The challenges are
// written through POST /dns/internal/acme-challenge on the certs bearer and served by the edge, GET
// /dns/internal/ready answering ready once the edge serves the serial and the canary is clean, Pebble refusing 5% of
// nonces and validating through the resolver, which reaches the edge. What must hold:
//
//   - every order ends issued: the device is handed a chain that verifies to Pebble's root for <label>.<root> and
//     web.<label>.<root>, on the device's own key, the chain of the device's one issued row, from LE's place in the
//     CA order, and what TlsDeviceCertificate presents for the name;
//   - each device listens on tls/443/web, and a visitor through tlsedge, behind the lb's PROXY header, verifies the
//     listener's chain against Pebble's root, not a self-signed leaf, and is answered by the device.
//
// The renewals race: Pebble asks for the renewal now of each network's first publisher's certificate, its ARI window
// set to one that began an hour ago (RFC 9773's early renewal); the renewal pass, kicked, re-reads the window and queues
// each successor from its stored CSR, and the order pass, kicked, orders it by DNS-01, while every device writes its own
// TXT set and compare-and-swaps the shared one again (RaceTxtWriteCount, RaceSharedWriteCount) and readers read the
// edge. The challenges move each zone's version as the record writes do, so the compare-and-swaps meet them. One device
// a network renews (RenewDeviceCount): each order writes its name's challenge twice, the apex's value and then the
// wildcard's beside it, and a name's ten writes an hour (dns_challenges_per_name_hour) hold the set's two first orders,
// one renewal and the chaos below, but not a second renewal as well, after which every order of the set fails until
// the hour frees a slot (QD-1189's question). Chaos comes under the orders' own requests
// (dns_certs_concurrency_recorders_test.go's holds and gate): the certs worker restarts under the first renewal's
// challenge write, every write that comes until the restart has ended waiting and cut by the drain before it is sent,
// and the order pass is kicked again; the dns API block stops under the first write after that restart, which meets
// the lb's 503 and rides it as a restore's, the block starting again on its stores once the write is answered; and the
// dns edge restarts on its snapshot under the first renewal's GET /dns/internal/ready, which waits for it. What must
// hold, beside the records phase's checks over every write and read so far:
//
//   - every successor issues from the stored CSR, on the device's key with a serial of its own, and once the device's
//     pass installs it a visitor through tlsedge is shown it;
//   - each restart came under a renewal's request: the worker's under a challenge write, the API's under the next
//     write, the edge's under a ready read;
//   - no challenge of certs' is left: each network's _acme-challenge.<label>.<root> answers no TXT, and its newest
//     journal row deletes it.
//
// The delegated phase (dns_certs_concurrency_desktop_test.go; TLS-M6-05, QD-1165, DESIGN-DETAILS 17.2.6 to 17.2.9): the
// first CustomDomainCount networks each verify a custom domain and map its www host to their tls/ name's owner, a
// weak-mode host whose customer's CNAME points it at the owner, and whose one-time CNAME delegates the host's
// _acme-challenge to the domain's acme name; each network's owner device makes the domain's ACME account at Pebble as
// an admin's ordering desktop does, and each publisher's pass queues one CSR of its own for its custom host, so each
// domain holds a row for every device listening on the owner. Then every desktop's orderer runs its passes at once,
// two desktops on the first domain: each claims the account's lock, orders at Pebble, posts the DNS-01 digest to POST
// /tls/custom-challenge, which api captures as a receipt and hands to the certs worker's command, which writes it at
// the acme name through certs' DNS client and answers the serial; the desktop polls GET /tls/custom-challenge/<serial>
// until ready, Pebble validates through the resolver's CNAME to the edge, and the desktop finalizes and registers the
// chain. The certs worker restarts while the first command holds its write (controller's barrier before the write),
// its run cut before the write is sent, so the dispatch is left unheard; and the dns edge restarts under the first
// command's ready read. What must hold:
//
//   - each domain's custom rows, one a publisher, end custom_issued with a chain that verifies to Pebble's root for its
//     host, on its publisher's key, each registered by one desktop once: one claim wins;
//   - each receipt is one domain and claim, ends written at the serial of the journal's one commit of its digest at
//     the acme name, and every commit there is a written receipt's: the restarted command's dispatch was settled from
//     the journal before the write was made again, and nothing was written twice; a desktop's POST sent again under
//     its claim recovered its receipt and its serial;
//   - each account's newOrder spends are the newOrders its desktops' passes granted;
//   - the host's TXT, asked of the resolver, answers through its customer's CNAME its owner's newest TXT commit, signed
//     under the root's zone keys, and its A and AAAA answer through the same CNAME the world's lb interfaces, exactly
//     and signed, though its owner holds a TXT set of its own.
//
// The delegated value at the acme name is not removed when the order ends: DESIGN-DETAILS 17.2.9's step 5 has it lapse
// in its hour, and its receipt go once that hour from its capture has passed, which the case does not wait out.
//
// The ledgers at the end, exactly: §12.7's dns_changes_per_hour, the dns write budget the case spends, each network's
// dns.records admin events since the records phase began are its journal rows of user records, and its owner's writes
// are answered until the events reach the cap, where the next is refused 429 with used at the limit; and those
// dns_certs_concurrency_ledgers_test.go reconciles: certs' budget at LE ({tcb_letsencrypt_<root>}), each set's first
// certificate's order and any other first order of the set admitted before it issued, and nothing of the renewals or
// the resumed orders; each challenge name's journal rows against certs' writes, and its count in
// dns_challenges_per_name_hour's window, which each network's name is then written to and refused at exactly; api's
// write budget against the objects api's process wrote; and Pebble's nonce refusals, counted, with no nonce replayed.
//
// Each phase has PhaseTimeout and dumps the world's progress when it stalls. Run it through the build's wrapper:
// -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
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
	// The compare-and-swaps each device makes on its network's TXT set at the owner a custom domain's host maps to,
	// <TlsHost>.<label>.<root> (§12.8 step 2): what the host answers through the customer's CNAME.
	OwnerWriteCount int
	// The most version conflicts one compare-and-swap rides before it fails.
	CasTryCount int
	// The hosts of a device's own sets, %d its place among the case's devices, and its network's shared host; and a
	// host no write names, which the zone's wildcard answers.
	OwnTxtHostFormat  string
	OwnLinkHostFormat string
	WarmHostFormat    string
	SharedHost        string
	UnwrittenHost     string
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
	// §12.7's dns_challenges_per_name_hour as the world's limits set it, the window it counts over (model's
	// DnsChallengeSettings.RateWindow), and the most writes its exhaustion makes at a name past the orders before it
	// fails.
	ChallengesPerNameHour    int64
	ChallengeWindow          time.Duration
	ChallengeExhaustMaxCount int
	// The objects one LIST of the metadata cluster takes, and how many times the ledgers list api's objects around a
	// budget report before they give up on the two standing still.
	ObjectPageCount    int
	BudgetReadTryCount int
	// The bound on a dns edge restart a ready read's hold started (dns_certs_concurrency_recorders_test.go), and how
	// long the resolver stand-in asks the edge again while it does not answer: under the 15 s Pebble's validation
	// waits for its answer.
	EdgeRestartTimeout   time.Duration
	ResolverRetryTimeout time.Duration
	// How many of the newest ready answers and Pebble problems the recorders keep for a stalled phase's dump.
	DiagnosticKeepCount int
	// The certificates phase: the sidecars of each network, from its first, that publish its tls/ name and so order
	// the network's certificate; the document's grant of the name, given when it expires, and its life; the name's
	// host; and what a visitor sends.
	CertDeviceCount int
	TlsAclGrant     func(expires time.Time) string
	TlsGrantLife    time.Duration
	TlsHost         string
	TlsRequest      string
	Tls             *WorldTlsSettings
	// How long a certificate order may take from a device's request to its chain.
	OrderTimeout time.Duration
	// certs.yml's order_retry_after_s, what GET /tls/cert-order/<id> tells a device to wait, and the name of the LE
	// account file it names.
	OrderRetryAfterSeconds int
	AccountFileName        string
	// The share of nonces Pebble refuses, in percent: §22.9's 5, as the local stack's Pebble refuses.
	PebbleNonceRejectPercent int
	// The canary monitor stand-in's cadence, inside plan-DNS-canary-route-state's 180 s freshness, and one round's
	// bound.
	CanaryInterval time.Duration
	CanaryTimeout  time.Duration
	// The certs worker's task loops, how often an idle one polls the queue, and how long its drain lets a pass run
	// before it cuts it.
	WorkerLoopCount    int
	WorkerPollTimeout  time.Duration
	WorkerDrainTimeout time.Duration
	// How long the drain waits for a cut pass to unwind before the run's own end: a command the barrier holds rides it.
	WorkerDrainCancelTimeout time.Duration
	// The delegated phase: the networks, from the first, each with one custom domain whose host maps to its tls/ name's
	// owner; the domain's name, %s the network's, and its host's label; the desktops ordering the first domain at once;
	// the orderers' poll of an order; and each desktop's passes, at most, and the pause after each.
	CustomDomainCount       int
	CustomDomainFormat      string
	CustomHostLabel         string
	SameDomainDesktopCount  int
	CustomOrderPollInterval time.Duration
	CustomPassMaxCount      int
	CustomPassInterval      time.Duration
	// The delegated phase's bound: past PhaseTimeout, since the restarted command waits out its dispatch's SettleAfter
	// (model.TlsCustomChallengeSettings, a minute) before it writes again.
	DelegatedPhaseTimeout time.Duration
	// How often a wait on a certificate row reads the rows again: a row moves at a pass, with no hook a test reads.
	CertPollInterval time.Duration
	// The renewals race: the publishers of each network, from its first, whose first certificates renew, one since an
	// order writes its name's challenge twice (the apex's value, then the wildcard's beside it) and §12.7's
	// dns_challenges_per_name_hour gives the name ten writes an hour, which the set's two first orders, a renewal and the
	// chaos's re-dos come near; the window Pebble answers each such certificate's ARI with, starting this long before
	// now and this long, so the renewal is due at once; each device's writes to its own TXT set and its
	// compare-and-swaps on the shared one during the race; and the readers of the edge and the reads each makes.
	RenewDeviceCount     int
	RenewalWindowAge     time.Duration
	RenewalWindowLength  time.Duration
	RaceTxtWriteCount    int
	RaceSharedWriteCount int
	RaceReaderCount      int
	RaceReadCount        int
	Dns                  *WorldDnsSettings
	Bursts               *WorldBurstSettings
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
		OwnerWriteCount:     2,
		CasTryCount:         60,
		OwnTxtHostFormat:    "own-%d",
		OwnLinkHostFormat:   "link-%d",
		WarmHostFormat:      "warm-%d",
		SharedHost:          "shared",
		UnwrittenHost:       "unwritten",
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
		CertDeviceCount:     2,
		TlsAclGrant: func(expires time.Time) string {
			return fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["dns:web", "sock:tls/443/web"], "allow": ["publish", "listen"], "audience": "public", "app_mode": "raw", "expires": %q}`, expires.UTC().Format(time.RFC3339))
		},
		TlsGrantLife:             12 * time.Hour,
		TlsHost:                  "web",
		TlsRequest:               "hello over an issued chain",
		Tls:                      DefaultWorldTlsSettings(),
		OrderTimeout:             4 * time.Minute,
		OrderRetryAfterSeconds:   1,
		AccountFileName:          "acme-account-letsencrypt.yml",
		PebbleNonceRejectPercent: 5,
		CanaryInterval:           30 * time.Second,
		CanaryTimeout:            60 * time.Second,
		WorkerLoopCount:          4,
		WorkerPollTimeout:        100 * time.Millisecond,
		WorkerDrainTimeout:       2 * time.Second,
		WorkerDrainCancelTimeout: time.Second,
		CustomDomainCount:        2,
		CustomDomainFormat:       "shop-%s.customer.example",
		CustomHostLabel:          "www",
		SameDomainDesktopCount:   2,
		CustomOrderPollInterval:  time.Second,
		CustomPassMaxCount:       400,
		CustomPassInterval:       500 * time.Millisecond,
		DelegatedPhaseTimeout:    8 * time.Minute,
		CertPollInterval:         250 * time.Millisecond,
		ChallengesPerNameHour:    10,
		ChallengeWindow:          time.Hour,
		ChallengeExhaustMaxCount: 20,
		ObjectPageCount:          1000,
		BudgetReadTryCount:       5,
		EdgeRestartTimeout:       2 * time.Minute,
		ResolverRetryTimeout:     10 * time.Second,
		DiagnosticKeepCount:      40,
		RenewDeviceCount:         1,
		RenewalWindowAge:         time.Hour,
		RenewalWindowLength:      time.Minute,
		RaceTxtWriteCount:        2,
		RaceSharedWriteCount:     2,
		RaceReaderCount:          2,
		RaceReadCount:            60,
		Dns:                      DefaultWorldDnsSettings(),
		Bursts:                   bursts,
		PhaseTimeout:             5 * time.Minute,
		CaseTimeout:              20 * time.Minute,
	}
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
		// each sidecar orders under certs' audience at the api's domain, as SidecarHostList.ApplyServiceUrls derives it
		certsResource := world.T3().Api().ServiceOrigin("certs")
		added, err := world.AddNetwork(ctx, worldNetworkName(networkIndex), clientCount, func(name string, sidecarSettings *sidecar.SidecarSettings) {
			sidecarSettings.Tls.CertsResource = certsResource
		})
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
			_, saveErrs[networkIndex] = world.SaveAclPolicy(ctx, network.WorldNetwork, network.Owner(), worldAclPolicy(settings.DnsAclGrant, settings.TlsAclGrant(time.Now().Add(settings.TlsGrantLife))))
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
		sets = append(sets,
			dnsCertsSetKey{Owner: network.OwnerName(settings.SharedHost), Type: dns.TypeTXT},
			dnsCertsSetKey{Owner: network.OwnerName(settings.TlsHost), Type: dns.TypeTXT},
		)
	}
	return sets
}

// The kinds of a device's writes in the records phase.
const (
	dnsCertsWriteOwnTxt  = "own-txt"
	dnsCertsWriteOwnLink = "own-link"
	dnsCertsWriteShared  = "shared"
	dnsCertsWriteOwner   = "owner"
)

// Records under concurrency, with chaos on the dns API and the edge, then the dns write budget (see the file header).
func TestE2eDnsCertsConcurrency(t *testing.T) {
	settings := defaultDnsCertsSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		stack := newDnsCertsStack(t, settings)
		world := stack.World
		defer world.Close()
		networks := addDnsCertsNetworks(t, world, settings)
		records := dnsCertsRecordsPhase(t, world, settings, networks)
		publishers := dnsCertsCertificatesPhase(t, stack, settings, networks)
		dnsCertsRenewalsPhase(t, stack, settings, networks, records, publishers)
		dnsCertsDelegatedPhase(t, stack, settings, networks, publishers)
		dnsCertsLedgersPhase(t, stack, settings, networks, records, publishers)
	})
}

// What the records phase leaves the phases after it: the zone keys, the journal's start, each network's events before
// the phase and each zone's version as it began, the sets written and every reader of the edge so far.
type dnsCertsRecords struct {
	Keys           *dnsCertsZoneKeys
	Root           string
	StartSerial    uint64
	PhaseStart     time.Time
	BaselineEvents map[server.Id]int
	StartVersions  map[server.Id]uint64
	Sets           []dnsCertsSetKey

	stateLock sync.Mutex
	readers   []*dnsCertsReader
}

func (self *dnsCertsRecords) addReaders(readers ...*dnsCertsReader) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readers = append(self.readers, readers...)
}

// Every reader of the edge since the records phase began.
func (self *dnsCertsRecords) Readers() []*dnsCertsReader {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.readers)
}

// The records phase and its checks (see the file header).
func dnsCertsRecordsPhase(t testing.TB, world *World, settings *dnsCertsSettings, networks []*dnsCertsNetwork) *dnsCertsRecords {
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
		for range settings.OwnerWriteCount {
			plan = append(plan, dnsCertsWriteOwner)
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
			case dnsCertsWriteOwner:
				set = dnsCertsSetKey{Owner: network.OwnerName(settings.TlsHost), Type: dns.TypeTXT}
				value = fmt.Sprintf("w%d-owner-%d", writer.Index, sequence)
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

	records := &dnsCertsRecords{
		Keys:           keys,
		Root:           root,
		StartSerial:    startSerial,
		PhaseStart:     phaseStart,
		BaselineEvents: baselineEvents,
		StartVersions:  startVersions,
		Sets:           sets,
	}
	records.addReaders(readers...)
	dnsCertsCheckRecords(t, ctx, world, settings, networks, records)
	dnsCertsCheckOwnerAddresses(t, ctx, world, keys, settings, networks)
	return records
}

// Arms a hold on the certs worker's next challenge write that stops block under it (dns_certs_concurrency_recorders_test
// .go): the write goes out once the block's origin answers 503, as the lb does from the stop's first moment, while the
// block drains its long-polls (a ready read holds up to 25 s, past the write's own timeout); once the write is
// answered and the drain has ended, the block starts again on its stores.
func dnsCertsHoldBlockStop(stack *dnsCertsStack, block string, name string) *dnsCertsHold {
	world := stack.World
	var stopped sync.WaitGroup
	var stopErr error
	return stack.Dns.HoldNextWrite(name, func() error {
		stopped.Go(func() {
			stopErr = world.Stop(block)
		})
		// the stop takes the block's run away first, with no hook, so the run is read every RetryInterval
		deadline := time.After(stack.settings.EdgeRestartTimeout)
		for world.Block(block).currentRun() != nil {
			select {
			case <-deadline:
				return fmt.Errorf("%s's origin still had an upstream %s after its stop began", block, stack.settings.EdgeRestartTimeout)
			case <-time.After(stack.settings.Dns.RetryInterval):
			}
		}
		return nil
	}, func() error {
		stopped.Wait()
		if stopErr != nil {
			return stopErr
		}
		return world.Start(block)
	})
}

// The edge's addresses in each network's zone (§12.7's edge records, owner decision 335): the publish of tls/443/web,
// whose base api synced to the zone's * edge records, has the tls/ name's owner, web.<label>.<root>, answer A and AAAA
// with the world's lb interfaces, though it holds a TXT set of its own, through its copy of *'s records (QD-1190), and
// a host no write names, which the wildcard answers, the same, each RRset signed under the root's zone keys; and the
// apex, which no grant publishes, answers no address. Each asked until it answers so within the dns settings' Timeout,
// since the edge's probes run on their own interval and the edge restarted in the phase.
func dnsCertsCheckOwnerAddresses(t testing.TB, ctx context.Context, world *World, keys *dnsCertsZoneKeys, settings *dnsCertsSettings, networks []*dnsCertsNetwork) {
	t.Helper()
	for _, network := range networks {
		lines := []string{}
		for _, host := range []string{settings.TlsHost, settings.UnwrittenHost} {
			owner := network.OwnerName(host)
			for _, rrType := range []uint16{dns.TypeA, dns.TypeAAAA} {
				want := dnsCertsLbAddresses(world, rrType)
				answer, err := world.AwaitDnsAnswer(ctx, owner, rrType, func(answer *dns.Msg) bool {
					return answer.Rcode == dns.RcodeSuccess && slices.Equal(dnsCertsAnswerAddresses(answer, owner, rrType), want)
				}, settings.Dns)
				if err != nil {
					t.Errorf("%s %s at the edge answered %s, not the lb's %v: %v", owner, dns.TypeToString[rrType], dnsCertsDescribeAnswer(answer), want, err)
					continue
				}
				if err := keys.Verify(answer, owner, rrType, time.Now()); err != nil {
					t.Errorf("%s %s at the edge: %v", owner, dns.TypeToString[rrType], err)
					continue
				}
				lines = append(lines, fmt.Sprintf("%s %s %v signed", owner, dns.TypeToString[rrType], want))
			}
		}
		for _, rrType := range []uint16{dns.TypeA, dns.TypeAAAA} {
			answer, err := world.AwaitDnsAnswer(ctx, network.Apex, rrType, func(answer *dns.Msg) bool {
				return answer.Rcode == dns.RcodeSuccess
			}, settings.Dns)
			if err != nil || 0 < len(dnsCertsAnswerAddresses(answer, network.Apex, rrType)) {
				t.Errorf("%s %s, which no grant publishes, answered %s: %v", network.Apex, dns.TypeToString[rrType], dnsCertsDescribeAnswer(answer), err)
				continue
			}
			lines = append(lines, fmt.Sprintf("%s %s none", network.Apex, dns.TypeToString[rrType]))
		}
		t.Logf("the edge's addresses in %s: %s", network.Name, strings.Join(lines, "; "))
	}
}

// What committed since the records phase began, the journal to the root's serial now, which the edge must reach,
// checked against every write and read so far (see the file header): the journal rows checked, for the checks after.
func dnsCertsCheckRecords(t testing.TB, ctx context.Context, world *World, settings *dnsCertsSettings, networks []*dnsCertsNetwork, records *dnsCertsRecords) []dnsservice.DnsJournalRow {
	t.Helper()
	endSerial, err := dnsCertsRootSerial(ctx, records.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := world.AwaitDnsAnswer(ctx, records.Root, dns.TypeSOA, func(answer *dns.Msg) bool {
		for _, record := range answer.Answer {
			if soa, ok := record.(*dns.SOA); ok && endSerial <= uint64(soa.Serial) {
				return true
			}
		}
		return false
	}, settings.Dns); err != nil {
		world.FailProgress(t, "the edge's catch-up", err, nil)
	}
	rows, err := dnsCertsJournal(ctx, records.Root, records.StartSerial, settings.JournalPageCount)
	if err != nil {
		t.Fatal(err)
	}
	history := dnsCertsCheckJournal(t, networks, records.Sets, rows, records.StartVersions)
	dnsCertsCheckReads(t, records.Readers(), history)
	dnsCertsCheckServed(t, ctx, world, records.Keys, settings, records.Sets, history)
	dnsCertsCheckZoneVersions(t, ctx, settings, networks, rows)
	return rows
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

// One sidecar that publishes its network's tls/ name in the certificates phase: its tls component, the name's
// server name, and what its order answered.
type dnsCertsPublisher struct {
	Network    *dnsCertsNetwork
	Device     *sidecart3.T3Device
	Component  *sidecar.TlsComponent
	ServerName string
	// the device's sidecar id, which its certificate rows name
	SidecarId sidecar.Id
	// its first order's row, once issued, and when it issued (the row's state_time then)
	firstCertId   sidecar.Id
	firstIssuedAt time.Time
	// its listener's server, once it publishes
	echo *worldTlsEcho

	stateLock   sync.Mutex
	certificate *tls.Certificate
	err         error
}

func (self *dnsCertsPublisher) ordered(certificate *tls.Certificate, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.certificate = certificate
	self.err = err
}

// What the device's order answered.
func (self *dnsCertsPublisher) Order() (*tls.Certificate, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.certificate, self.err
}

// The certificates phase: each publisher orders its network's certificate through api's certs routes at once; the
// certs worker orders each from Pebble by DNS-01, the challenges written through dns's api and served by the edge,
// which Pebble's validation reaches through the resolver; every order ends issued with a chain to Pebble's root on the
// device's own key, the one issued row of its device; the device presents it at its tls/ listener, and a visitor
// through tlsedge verifies it; and the certs budget holds exactly the orders it spent on.
func dnsCertsCertificatesPhase(t testing.TB, stack *dnsCertsStack, settings *dnsCertsSettings, networks []*dnsCertsNetwork) []*dnsCertsPublisher {
	world := stack.World
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	publishers := []*dnsCertsPublisher{}
	for _, network := range networks {
		for _, device := range network.Clients[:min(settings.CertDeviceCount, len(network.Clients))] {
			args, err := device.Sidecar().TlsForegroundComponentArgs()
			if err != nil {
				t.Fatalf("%s's tls component: %v", device.Name, err)
			}
			component, err := sidecar.NewTlsComponent(world.T3().Context(), args)
			if err != nil {
				t.Fatalf("%s's tls component: %v", device.Name, err)
			}
			if _, err := device.Sidecar().AddComponent(component); err != nil {
				component.Close()
				t.Fatalf("%s's tls component: %v", device.Name, err)
			}
			publishers = append(publishers, &dnsCertsPublisher{
				Network:    network,
				Device:     device,
				Component:  component,
				ServerName: settings.TlsHost + "." + strings.TrimSuffix(network.Apex, "."),
				SidecarId:  device.Sidecar().Identity().SidecarId,
			})
		}
	}

	// every order at once, the order chain's next pass kicked once each is queued
	orderCtx, cancelOrders := context.WithTimeout(ctx, settings.OrderTimeout)
	defer cancelOrders()
	var orders sync.WaitGroup
	for _, publisher := range publishers {
		orders.Go(func() {
			publisher.ordered(dnsCertsEnsureCertificate(orderCtx, publisher.Device, settings))
		})
	}
	// an order that ended before its row was written ends the wait: its error says why
	if err := dnsCertsAwaitRows(ctx, publishers, settings, func(publisher *dnsCertsPublisher, rows []*sidecar.TlsCertRecord) bool {
		_, err := publisher.Order()
		return 0 < len(rows) || err != nil
	}); err != nil {
		cancelOrders()
		orders.Wait()
		world.FailProgress(t, "the certificate orders' rows", fmt.Errorf("%w\n%s", err, stack.DescribeCerts()), nil)
	}
	if _, err := dnsCertsKick(ctx, "tls_cert_orders"); err != nil {
		t.Fatal(err)
	}
	orders.Wait()

	for _, publisher := range publishers {
		certificate, err := publisher.Order()
		if err != nil {
			rows, _ := dnsCertsSidecarCerts(ctx, publisher)
			t.Errorf("%s's certificate order: %v; its rows: %s", publisher.Device.Name, err, dnsCertsDescribeRows(rows))
			continue
		}
		leaf, err := dnsCertsVerifyChain(certificate.Certificate, stack.Pebble.Roots, []string{strings.TrimSuffix(publisher.Network.Apex, "."), publisher.ServerName}, time.Now())
		if err != nil {
			t.Errorf("%s's certificate: %v", publisher.Device.Name, err)
			continue
		}
		spkiSha256, err := worldTlsKeySpkiSha256(publisher.Device)
		if err != nil {
			t.Fatal(err)
		}
		if digest := dnsCertsSpkiSha256(leaf); !bytes.Equal(digest, spkiSha256) {
			t.Errorf("%s's certificate is on a key not its own: %x", publisher.Device.Name, digest)
		}
		rows, err := dnsCertsSidecarCerts(ctx, publisher)
		if err != nil {
			t.Fatal(err)
		}
		issued := []*sidecar.TlsCertRecord{}
		for _, row := range rows {
			if row.State == sidecar.TlsCertStateIssued {
				issued = append(issued, row)
			}
		}
		if len(issued) != 1 || len(issued[0].Chain) == 0 || !bytes.Equal(issued[0].Chain[0], leaf.Raw) {
			t.Errorf("%s holds %d issued rows, where its one certificate should be the one it was handed: %s", publisher.Device.Name, len(issued), dnsCertsDescribeRows(rows))
			continue
		}
		if issued[0].Ca != string(certsservice.CertsCaLetsEncrypt) {
			t.Errorf("%s's certificate came from CA %q", publisher.Device.Name, issued[0].Ca)
		}
		publisher.firstCertId = issued[0].CertId
		publisher.firstIssuedAt = issued[0].StateTime
		presented, err := publisher.Device.Sidecar().TlsDeviceCertificate([]string{publisher.ServerName})
		if err != nil || len(presented.Certificate) == 0 || !bytes.Equal(presented.Certificate[0], leaf.Raw) {
			t.Errorf("%s presents something other than its issued certificate for %s: %v", publisher.Device.Name, publisher.ServerName, err)
		}
	}
	if t.Failed() {
		world.FailProgress(t, "the certificate orders", fmt.Errorf("an order did not end as it should\n%s", stack.DescribeCerts()), nil)
	}

	// each publisher listens on its network's name, and a visitor through tlsedge verifies the issued chain
	for _, publisher := range publishers {
		dnsCertsPublish(t, ctx, stack, settings, publisher)
		certificate, _ := publisher.Order()
		dnsCertsVisit(t, ctx, stack, settings, publisher, certificate.Certificate[0])
	}

	return publishers
}

// device's EnsureTlsCertificate, asked again every CertPollInterval while api answers 503: api's subject waits on its
// gate cache and §21's standing for a network it has not loaded, and a standing loading answers at the caller's retry
// (controller's tls_certs_install.go), as a daemon's publish asks again. Any other answer is the order's.
func dnsCertsEnsureCertificate(ctx context.Context, device *sidecart3.T3Device, settings *dnsCertsSettings) (*tls.Certificate, error) {
	for {
		certificate, err := device.Sidecar().EnsureTlsCertificate(ctx)
		var apiErr *sidecar.SidecarApiError
		if err == nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
			return certificate, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (%w)", err, ctx.Err())
		case <-time.After(settings.CertPollInterval):
		}
	}
}

// publisher's listener on its network's tls/ name, as a desktop's publish runs one (World.StartSockets, the stock
// approval gate, a raw listen), serving TLS on what TlsDeviceCertificate gives its server name at each handshake, for
// the world's life.
func dnsCertsPublish(t testing.TB, ctx context.Context, stack *dnsCertsStack, settings *dnsCertsSettings, publisher *dnsCertsPublisher) {
	world := stack.World
	device := publisher.Device
	if err := world.StartSockets(ctx, device); err != nil {
		t.Fatal(err)
	}
	if _, err := world.InstallApprovalGate(ctx, device); err != nil {
		t.Fatal(err)
	}
	listener, err := world.Listen(ctx, device, "tls/443/"+settings.TlsHost, &sidecar.SocketListenSettings{AppMode: "raw"}, settings.Tls)
	if err != nil {
		world.FailProgress(t, device.Name+"'s tls/ listen", err, nil)
	}
	publisher.echo = dnsCertsServeTls(device, listener, publisher.ServerName, publisher.answer, settings.Tls)
	world.T3().OnClose(func() {
		listener.Close()
		publisher.echo.Wait()
	})
}

// What publisher's listener answers a request line.
func (self *dnsCertsPublisher) answer(line []byte) []byte {
	return fmt.Appendf(nil, "%s: %s\n", self.Device.Name, line)
}

// A visitor through tlsedge, behind the lb's PROXY header, who verifies publisher's chain against Pebble's roots and
// is answered by the device, until the leaf it is shown is want's.
func dnsCertsVisit(t testing.TB, ctx context.Context, stack *dnsCertsStack, settings *dnsCertsSettings, publisher *dnsCertsPublisher, want []byte) {
	world := stack.World
	request := []byte(settings.TlsRequest)
	leaf, err := dnsCertsAwaitVerifiedVisit(ctx, world, publisher.ServerName, stack.Pebble.Roots, request, bytes.TrimSuffix(publisher.answer(request), []byte("\n")), want, settings.Tls)
	if err != nil {
		world.FailProgress(t, publisher.Device.Name+"'s visit through tlsedge", fmt.Errorf("%w; the listener's accepts ended: %v; %s", err, publisher.echo.Ended(), worldTlsServerState(t.Context(), publisher.Device.Network.NetworkId, publisher.Network.Root)), nil)
	}
	if !bytes.Equal(leaf.Raw, want) {
		t.Errorf("a visitor to %s was shown a leaf that is not %s's certificate", publisher.ServerName, publisher.Device.Name)
	}
}

// The row certId of publisher's, nil when it has none.
func dnsCertsRow(rows []*sidecar.TlsCertRecord, certId sidecar.Id) *sidecar.TlsCertRecord {
	for _, row := range rows {
		if row.CertId == certId {
			return row
		}
	}
	return nil
}

// The renewals race (see the file header): Pebble asks for each first certificate's renewal now; while devices write
// records, readers read the edge and the certs worker restarts mid-order, certs' renewal pass queues each successor
// from its stored CSR and the order pass orders it by DNS-01, the challenges and the record writes taking turns on each
// zone's version. Both must hold whole: every successor issued, presented and seen by a visitor, and every write and
// read reconciled against the journal.
func dnsCertsRenewalsPhase(t testing.TB, stack *dnsCertsStack, settings *dnsCertsSettings, networks []*dnsCertsNetwork, records *dnsCertsRecords, publishers []*dnsCertsPublisher) {
	world := stack.World
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	// the publishers whose certificates renew: each network's first RenewDeviceCount
	renewing := []*dnsCertsPublisher{}
	networkRenewals := map[*dnsCertsNetwork]int{}
	for _, publisher := range publishers {
		if networkRenewals[publisher.Network] < settings.RenewDeviceCount {
			renewing = append(renewing, publisher)
			networkRenewals[publisher.Network] += 1
		}
	}
	for _, publisher := range renewing {
		rows, err := dnsCertsSidecarCerts(ctx, publisher)
		if err != nil {
			t.Fatal(err)
		}
		first := dnsCertsRow(rows, publisher.firstCertId)
		if first == nil || len(first.Chain) == 0 {
			t.Fatalf("%s's first certificate is gone: %s", publisher.Device.Name, dnsCertsDescribeRows(rows))
		}
		start := time.Now().Add(-settings.RenewalWindowAge)
		if err := stack.Pebble.SetRenewalWindow(first.Chain[0], start, start.Add(settings.RenewalWindowLength)); err != nil {
			t.Fatalf("%s's renewal window: %v", publisher.Device.Name, err)
		}
	}
	// each publisher's successor: its first row's successor, once queued
	successor := func(publisher *dnsCertsPublisher, rows []*sidecar.TlsCertRecord) *sidecar.TlsCertRecord {
		first := dnsCertsRow(rows, publisher.firstCertId)
		if first == nil || first.SuccessorCertId == (sidecar.Id{}) {
			return nil
		}
		return dnsCertsRow(rows, first.SuccessorCertId)
	}

	writers := []*dnsCertsWriter{}
	for _, network := range networks {
		writers = append(writers, network.Writers...)
	}
	bursts := []*WorldBurst{}
	for _, writer := range writers {
		network := writer.Network
		stepCount := settings.RaceTxtWriteCount + settings.RaceSharedWriteCount
		burst := &WorldBurst{Name: "racing writer " + writer.Device.Name, StepCount: stepCount}
		burst.Step = func(ctx context.Context, index int, random *rand.Rand) error {
			// a compare-and-swap every other step, the shared set's writes spread through the race
			if index%2 == 1 && index/2 < settings.RaceSharedWriteCount {
				set := dnsCertsSetKey{Owner: network.OwnerName(settings.SharedHost), Type: dns.TypeTXT}
				_, err := writer.Write(ctx, set, fmt.Sprintf("w%d-race-shared-%d", writer.Index, index), true, settings.CasTryCount, settings.Dns)
				return err
			}
			set := dnsCertsSetKey{Owner: network.OwnerName(fmt.Sprintf(settings.OwnTxtHostFormat, writer.Index)), Type: dns.TypeTXT}
			_, err := writer.Write(ctx, set, fmt.Sprintf("w%d-race-txt-%d", writer.Index, index), false, settings.CasTryCount, settings.Dns)
			return err
		}
		bursts = append(bursts, burst)
	}
	readers := []*dnsCertsReader{}
	for index := range settings.RaceReaderCount {
		reader := &dnsCertsReader{Name: fmt.Sprintf("racing-reader-%d", index)}
		readers = append(readers, reader)
		bursts = append(bursts, &WorldBurst{
			Name:      reader.Name,
			StepCount: settings.RaceReadCount,
			Step: func(ctx context.Context, index int, random *rand.Rand) error {
				return reader.Read(ctx, world, records.Keys, records.Sets, random, settings.Dns)
			},
		})
	}
	records.addReaders(readers...)
	// the dns edge restarts under the first renewal to read ready: that read is held until the edge is back, on its
	// snapshot and the feed, so the renewal waits on an edge that restarted under it
	edgeHold := stack.Dns.HoldNextReady("the dns edge's restart under a renewal's ready read", func() error {
		return world.Restart(WorldBlockDnsedge)
	})
	defer func() {
		stack.Dns.Disarm(edgeHold)
		edgeHold.Wait(settings.EdgeRestartTimeout)
	}()
	// the certs worker restarts under the first renewal's challenge write: that write and every write that comes until
	// the restart has ended wait, and the drain cuts each before it is sent, so the renewals are under way and spend
	// none of their names' writes; then the dns API block's stop is armed under the first write after the restart,
	// which goes out to the lb's 503 and rides it as a restore's (certsservice's solver), the block starting again on
	// its stores once that write is answered; and the order pass is kicked again
	var apiHold atomic.Pointer[dnsCertsHold]
	workerGate := stack.Dns.GateWrites("the certs worker's restart under a renewal's challenge write", func() error {
		if err := stack.Worker.Restart(); err != nil {
			return err
		}
		apiHold.Store(dnsCertsHoldBlockStop(stack, WorldBlockDns, "the dns API's stop under a renewal's challenge write"))
		_, err := dnsCertsKick(ctx, "tls_cert_orders")
		return err
	})
	defer func() {
		stack.Dns.Disarm(workerGate)
		workerGate.Wait(settings.EdgeRestartTimeout)
		if hold := apiHold.Load(); hold != nil {
			stack.Dns.Disarm(hold)
			hold.Wait(settings.EdgeRestartTimeout)
		}
	}()
	// the certs driver: the renewal pass kicked until every successor is queued; the order pass kicked, and the
	// worker's restart under its first write awaited; then every successor issued
	bursts = append(bursts, &WorldBurst{
		Name:      "renewals",
		StepCount: 3,
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			switch index {
			case 0:
				if _, err := dnsCertsKick(ctx, "tls_cert_renewals"); err != nil {
					return err
				}
				return dnsCertsAwaitRows(ctx, renewing, settings, func(publisher *dnsCertsPublisher, rows []*sidecar.TlsCertRecord) bool {
					return successor(publisher, rows) != nil
				})
			case 1:
				if _, err := dnsCertsKick(ctx, "tls_cert_orders"); err != nil {
					return err
				}
				return workerGate.AwaitDone(ctx)
			default:
				return dnsCertsAwaitRows(ctx, renewing, settings, func(publisher *dnsCertsPublisher, rows []*sidecar.TlsCertRecord) bool {
					renewal := successor(publisher, rows)
					return renewal != nil && (renewal.State == sidecar.TlsCertStateIssued || renewal.State == sidecar.TlsCertStateFailed)
				})
			}
		},
	})
	if err := RunBursts(ctx, t, "the renewals race", bursts, settings.Bursts); err != nil {
		world.FailProgress(t, "the renewals race", fmt.Errorf("%w\n%s", err, stack.DescribeCerts()), bursts)
	}
	FailBurstErrors(t, bursts)
	stack.Dns.Disarm(workerGate)
	workerStarted, workerName := workerGate.Started()
	if err := workerGate.Wait(settings.EdgeRestartTimeout); err != nil {
		t.Errorf("%v", err)
	}
	stack.Dns.Disarm(edgeHold)
	edgeStarted, edgeSerial := edgeHold.Started()
	if err := edgeHold.Wait(settings.EdgeRestartTimeout); err != nil {
		t.Errorf("%v", err)
	}
	apiStarted, apiName := false, ""
	if hold := apiHold.Load(); hold != nil {
		stack.Dns.Disarm(hold)
		apiStarted, apiName = hold.Started()
		if err := hold.Wait(settings.EdgeRestartTimeout); err != nil {
			t.Errorf("%v", err)
		}
	}
	t.Logf("the renewals race: the certs worker restarted at run %d under a challenge write at %q, %d writes held; the dns edge restarted under a ready read of serial %q; the dns API stopped under a challenge write at %q", stack.Worker.RunCount(), workerName, workerGate.HeldCount(), edgeSerial, apiName)
	if !workerStarted {
		t.Errorf("no renewal wrote a challenge, so the certs worker never restarted under one")
	}
	if !edgeStarted {
		t.Errorf("no renewal read ready, so the dns edge never restarted under one")
	}
	if !apiStarted {
		t.Errorf("no renewal wrote a challenge after the certs worker's restart, so the dns API never stopped under one")
	}

	// every successor issued on its device's key, installed by the device's pass, presented and seen by a visitor
	for _, publisher := range renewing {
		rows, err := dnsCertsSidecarCerts(ctx, publisher)
		if err != nil {
			t.Fatal(err)
		}
		renewal := successor(publisher, rows)
		if renewal == nil || renewal.State != sidecar.TlsCertStateIssued || len(renewal.Chain) == 0 {
			t.Errorf("%s's renewal did not issue: %s", publisher.Device.Name, dnsCertsDescribeRows(rows))
			continue
		}
		leaf, err := dnsCertsVerifyChain(renewal.Chain, stack.Pebble.Roots, []string{strings.TrimSuffix(publisher.Network.Apex, "."), publisher.ServerName}, time.Now())
		if err != nil {
			t.Errorf("%s's renewal: %v", publisher.Device.Name, err)
			continue
		}
		first := dnsCertsRow(rows, publisher.firstCertId)
		if first.Serial == renewal.Serial {
			t.Errorf("%s's renewal holds its first certificate's serial", publisher.Device.Name)
		}
		spkiSha256, err := worldTlsKeySpkiSha256(publisher.Device)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dnsCertsSpkiSha256(leaf), spkiSha256) {
			t.Errorf("%s's renewal is on a key not its own", publisher.Device.Name)
		}
		if err := publisher.Component.CertManager().Sync(ctx); err != nil {
			t.Errorf("%s's certificate pass: %v", publisher.Device.Name, err)
		}
		dnsCertsVisit(t, ctx, stack, settings, publisher, leaf.Raw)
	}
	dnsCertsCheckChallengesGone(t, ctx, world, settings, networks, records)
	dnsCertsCheckRecords(t, ctx, world, settings, networks, records)
}

// Once every order has ended, no challenge of certs' is left: each network's _acme-challenge.<label>.<root> answers no
// TXT at the edge, and its newest journal row deletes it (certsservice's solver cleans each order's values up).
func dnsCertsCheckChallengesGone(t testing.TB, ctx context.Context, world *World, settings *dnsCertsSettings, networks []*dnsCertsNetwork, records *dnsCertsRecords) {
	t.Helper()
	rows, err := dnsCertsJournal(ctx, records.Root, records.StartSerial, settings.JournalPageCount)
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range networks {
		owner := dns.CanonicalName("_acme-challenge." + network.Apex)
		last := ""
		for _, row := range rows {
			for _, upsert := range row.Upserts {
				if dns.CanonicalName(upsert.Owner) == owner {
					last = fmt.Sprintf("an upsert at %d", row.Serial)
				}
			}
			for _, deleted := range row.Deletes {
				if dns.CanonicalName(deleted.Owner) == owner {
					last = "a delete"
				}
			}
		}
		if last != "a delete" {
			t.Errorf("%s's newest challenge row is %q, not its delete", owner, last)
		}
		set := dnsCertsSetKey{Owner: owner, Type: dns.TypeTXT}
		if answer, err := world.AwaitDnsAnswer(ctx, owner, dns.TypeTXT, func(answer *dns.Msg) bool {
			return answer.Rcode == dns.RcodeSuccess && len(dnsCertsAnswerValues(answer, set)) == 0
		}, settings.Dns); err != nil {
			t.Errorf("the edge still answers %s's challenge: %v: %v", network.Name, err, answer)
		}
	}
}

// The ledgers at the case's end, exactly (see the file header): §12.7's dns_changes_per_hour, each network's events
// its user rows and its owner's writes answered until the cap, where the next is refused at the limit; and certs'
// budget at LE holding each publisher's first order and nothing else, renewals and resumed orders spending none.
func dnsCertsLedgersPhase(t testing.TB, stack *dnsCertsStack, settings *dnsCertsSettings, networks []*dnsCertsNetwork, records *dnsCertsRecords, publishers []*dnsCertsPublisher) {
	world := stack.World
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	rows, err := dnsCertsJournal(ctx, records.Root, records.StartSerial, settings.JournalPageCount)
	if err != nil {
		t.Fatal(err)
	}
	dnsCertsCheckChangesBudget(t, ctx, settings, networks, rows, records.BaselineEvents, records.PhaseStart)
	dnsCertsCheckCertsBudget(t, ctx, stack, publishers)
	dnsCertsCheckChallengeCounts(t, ctx, stack, settings, records)
	dnsCertsExhaustChallenges(t, ctx, stack, settings, networks, records)
	dnsCertsCheckApiBudget(t, ctx, world, settings)
	dnsCertsCheckNonces(t, stack, settings)
}
