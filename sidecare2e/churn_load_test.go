//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Mixed load across every service over rounds, with chaos, then exact ledgers (the end-to-end plan's e2e-churn-load,
// the last of its scenario tracks; the owner's end-to-end tests of 2026-11-21), modeled on server/proxy's
// TestProxyContractChurnLoad and connect's multi-client perf test: concurrent workers of every kind, chaos underneath,
// strict checks between rounds and a reconciliation of every ledger at the end.
//
// The world is the dns and certificates case's stack (newDnsCertsStack): every block of the World on its real stores
// and real keys, plus §13.8's certs worker as a block of its own, api's certs routes and an in-process Pebble. Beside
// it run the other scenarios' harnesses, reused rather than copied: the gkm recorder at gkm's origin and §10.6's
// reconciler (gkm_concurrency_harness_test.go), the write budget sampler and the stand-in for evs.nightly
// (evs_concurrency_harness_test.go), the recorder of sockets' registration calls (blogger_sockets_test.go).
//
// The networks (setup):
//
//	a, b      personal networks of PersonalClientCount sidecars, aged past probation and verified Pro, each with one
//	          document holding mail's grants, dns:* admin and a standing public publish of tls/443/web; mail on
//	          (setUpMailNetwork: the owner reads every mailbox and sends, the second sidecar is the prover host, the
//	          last receives, every other sends), every sidecar a dns writer of its own TXT set, a shared evs repo every
//	          sidecar writes, and a partial repo its owner granted to its second sidecar alone, whose membership the
//	          gkm workers churn; a's owner orders a's certificate through the certs worker at setup
//	          (dnsCertsCertificatesPhase) and listens on tls/443/web
//	c         a personal network of BlogWriterCount writers, verified Pro, its blog (newBloggerBlog): every writer's
//	          webserver behind tlsedge on the blog's one name through sockets' fair scheduler, BlogReaderCount readers
//	team      a team (World.AddTeamNetwork) of its first Owner's TeamOwnerDeviceCount devices, a shared evs repo the
//	          Owner's devices write, and the members and devices the churn adds and takes away
//	itb       the it bridge case's network on this world (newItBridgeNetwork, which builds on a real-keys World as on
//	          its own FakeGkm one: its config group stands, so the owner's CreateGroup takes generation 1 as it stands),
//	          its subnets behind their routers, its port forwards on their hosts and its gVisor clients, every subnet and
//	          forward joined at setup
//
// The rounds (RoundCount): in each, a random mix of workers runs at once, one goroutine a burst, random pauses
// between steps (RunBursts), each kind's count drawn from 0 to its MaxBurstCount, and the last round runs every kind
// no earlier round drew. Every count, rate and pause is a settings field (QD-702). The kinds:
//
//	evs writer     a device writes its own path of its network's shared repo, its content naming its sequence; a
//	               write refused for now (gkm's wait, an unavailable block) is tried again, and one whose outcome is
//	               unknown is resolved from the path's history before it is tried again, so nothing is acknowledged
//	               twice
//	evs reader     a device reads another's path, one of its writes begun by the read's answer, never older than the
//	               burst read before (evsRead)
//	gkm membership the owner grants its partial repo to a device that does not read it, or revokes the grant of one
//	               that does, as its own verified state says; a shrink makes the repo's writers rotate with cause
//	               REMOVE (§10.5, §10.6)
//	gkm rotation   a writer rotates its network's shared or partial repo (`ur keys rotate`)
//	mail sender    a sender of a or b sends to its own mailboxes, to the other network's and every other send to the
//	               internet sender's inbox (mailNetworkSend)
//	mail receiver  the internet sender delivers to a or b through the lb and mxedge's router (mailInternetDeliver)
//	blog writer    a writer of c updates one of its articles, tried again while evs or gkm refuse for now
//	blog reader    a reader of c loads an article through tlsedge (bloggerReader.read)
//	bridge client  an it bridge client sends TCP, UDP or a stream to a random subnet address or forwarded port through
//	               its gVisor stack (ItBridgeWorld.Attempt), which the bridge's oracle judges at the end
//	bridge forward a port forward is removed (a reset BridgeWithdraw) and set up again (a BridgeMap), then probed from
//	               a client its rules grant (Leave, Join, probeComeback)
//	dns writer     a device writes its own TXT set through dns's api, a value no other write puts
//	dns reader     a reader asks the dns edge for the written sets between two reads of the root's SOA
//	certs renewal  Pebble asks a's certificate's renewal now (its ARI window), the certs worker's renewal pass queues
//	               the successor from its stored CSR and its order pass orders it by DNS-01, at most CertRenewalCount
//	               across the case; the device installs it and a visitor through tlsedge is shown it
//	acl saver      a config writer saves its network's document: every grant the network's services need and one
//	               rule of its own, previewed and saved EXACT_HEAD, previewed again while another save moved the head
//	enrollment     the team's Owner invites a new person, who accepts and is admitted as a member, or enrolls and
//	               admits a new device of its own
//	departure      the team's Owner removes a member the churn added (member-remove, then MemberRevoke), or revokes a
//	               device of its own the churn added
//
// Chaos (ChaosTargets) restarts a random host or device, keeping its stores: between rounds, ChaosBetweenRoundCount
// restarts, and during each round past ChaosFreeRoundCount, ChaosDuringRoundCount restarts at random points. The
// hosts: api (T3World.RestartApi), evs, gkm, dns, dnsedge, tlsedge, smtpedge, mxedge, sockets (World.Restart), the
// certs worker (dnsCertsWorkerBlock.Restart); the device: one of the team's devices (T3Device.Restart), held from
// every worker's use while it restarts. Each restart is a chaos window of the bridge's oracle and of the blog's visit
// checks. The mail, dns and blog devices are no device chaos's target, since their mail
// clients, dns writers and webservers are bound to the Sidecar a restart ends.
//
// After each round, strictly:
//
//   - every acknowledged effect is present once: every evs commit in its path's verified history at the push, path
//     version and ref version its writer was answered, main's versions each one acknowledged commit
//     (evsConcurrencyCase.verifyDevice); every dns write in the root's journal once at its answered serial, every read
//     a state the journal had, every set served (dnsCertsCheckRecords); every mail message in each mailbox it names
//     once; every ACL save at a log position of its own, the newest the head's document; every enrollment and
//     departure in the owner's verified state and api's rows;
//   - every device converges within the design's bounds: each network's live devices on one pin and one base after a
//     sync to the owner's tip (§8.9); every reader of each repo holding gkm's newest generation of it (§10.5's
//     convergence, bounded by §8.9's latencies, the writers' pending wait, a holder's late jitter and, with a lost
//     hint, the resync interval); the dns edge serving the root's newest serial and every zone's version its newest
//     journal row;
//   - no false alarm: no device raised a head alarm, evs alarm or gap, gkm refusal or key change; the blog's visits
//     failed only inside a chaos window, and in a round without chaos every commit reached every other webserver
//     within the blog's settle bound.
//
// At the end, every ledger reconciled exactly against an independent count: the evs storage each network's quota
// state holds against evs.nightly's count of its objects; every byte drawn from each network's balances a debit on a
// meter, none overdrawn, the mail meters (mx, smtp) and tlsedge's meter at least the bytes the case moved through
// them; api's write budget against the objects api wrote (controlPlaneCheckApiBudget); every block run that spent on
// the metadata cluster's write budget holding a report, and nothing in flight or queued there; the certs budget at LE
// against the case's first orders (dnsCertsCheckCertsBudget); the challenge names' journal rows against the certs
// worker's writes (dnsCertsCheckChallengeCounts); and the mail sends' outcomes, the internet MX's transactions and
// every mailbox's copies (mailCheckMailbox, mailCheckInternetMx, mailCheckOutbox). Nothing is left in flight: no send
// unended, no certificate row unissued, no budget in flight. Each phase has its bound and a progress dump on a stall
// (World.FailProgress), and the whole case is bounded by CaseTimeout, 10 minutes by default.
//
// Not here, each for its reason: a restart of a mail, dns, blog or bridge device (above, and the bridge's own chaos is
// its case's); a tagged enrollment
// in the team, which answers 500 (finding team-tagged-enroll), and a team member's own device, whose auth key a
// Member cannot make (team-member-own-auth-key), so the churn's members join without devices and the Owner's devices
// are the ones that come and go.
//
// Known findings this case meets, each queued or running as it was placed (2026-11-22), are reported with their
// evidence where they show: an evs run's spend since its last report lost at close (write-budget-close-report), a
// commit answered just before a drain losing its stored-byte delta (evs-drain-step8), a network's first device
// publishing its ReaderKey late (gkm-reader-key-upload-retry), writes refused 503 after a resumed re-form
// (evs-after-reform-resume), and after an api restart a personal network whose base its second device signed not
// loading (config-replay-base-signer), which the case keeps out of the way by having each personal network's first
// device save its documents. A failure from a product bug stays failing. Run it through the build's wrapper:
// -tags 'sidecar_e2e sidecar_pending sidecar_localstack' -run TestE2eChurnLoad.

package sidecare2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecart3"
)

// One kind of worker's share of a round (QD-702).
type churnLoadKindSettings struct {
	// The most bursts of the kind a round runs: each round draws from 0 to this many.
	MaxBurstCount int
	// The steps each burst runs, in order, a random pause before each.
	StepCount int
}

// Every kind's share of a round.
type churnLoadMixSettings struct {
	EvsWriters    *churnLoadKindSettings
	EvsReaders    *churnLoadKindSettings
	GkmMembership *churnLoadKindSettings
	GkmRotations  *churnLoadKindSettings
	MailSenders   *churnLoadKindSettings
	MailReceivers *churnLoadKindSettings
	BlogWriters   *churnLoadKindSettings
	BlogReaders   *churnLoadKindSettings
	DnsWriters    *churnLoadKindSettings
	DnsReaders    *churnLoadKindSettings
	CertRenewals  *churnLoadKindSettings
	AclSavers     *churnLoadKindSettings
	Enrollments   *churnLoadKindSettings
	Departures    *churnLoadKindSettings
	BridgeClients *churnLoadKindSettings
	BridgeForward *churnLoadKindSettings
}

// What the case runs with (QD-702): every count, rate, pause and bound it adds to the harnesses' own.
type churnLoadSettings struct {
	// The case's bound from the world's start: the conventions' 10 minutes.
	CaseTimeout time.Duration
	// The setup's bound, each round's, each round's checks', and the end's.
	SetupTimeout time.Duration
	RoundTimeout time.Duration
	CheckTimeout time.Duration
	FinalTimeout time.Duration
	// The personal networks running mail, dns and evs, and the sidecars of each: at least 4, mail's owner, prover
	// host, a further sender and its receiver.
	PersonalNetworkCount int
	PersonalClientCount  int
	// The blog network's writers, each a webserver behind tlsedge, and its readers.
	BlogWriterCount int
	BlogReaderCount int
	// The rounds, and the seed every round's draws and pauses come from, logged so a run's shape repeats.
	RoundCount int
	Seed       uint64
	// Each kind's share of a round.
	Mix *churnLoadMixSettings
	// The bursts' pauses; their phase bound is RoundTimeout.
	Bursts *WorldBurstSettings
	// What chaos restarts: block names, churnLoadChaosApi, churnLoadChaosCerts and churnLoadChaosDevice.
	ChaosTargets []string
	// The restarts between two rounds, and during each round past the first ChaosFreeRoundCount, each at a random
	// point up to ChaosMaxDelay into the round.
	ChaosBetweenRoundCount int
	ChaosDuringRoundCount  int
	ChaosFreeRoundCount    int
	ChaosMaxDelay          time.Duration
	// How long a step refused for now (a block restarting, gkm's wait, a moved head) is tried again, and how often.
	RetryTimeout  time.Duration
	RetryInterval time.Duration
	// Each sidecar's gkm hint resync (churn_load_harness_test.go): what a hint a chaos lost costs at most (§6.8).
	KeyResyncInterval time.Duration
	// The convergence bounds: §8.9's targets, a statement at a server gate within GateLatency and applied on a
	// connected client within ClientApplyLatency, and what the requests of a rotation or a delivery take beside the
	// waits the sidecars' gkm settings give; and how often a convergence wait asks again.
	GateLatency             time.Duration
	ClientApplyLatency      time.Duration
	WorkAllowance           time.Duration
	ConvergencePollInterval time.Duration
	// §7.9's config_bases_per_min for every tier the case runs on: each ACL save publishes a base.
	ConfigBasesPerMinute int64
	// The tier limits the case installs past the world's (churn_load_harness_test.go), filled from the fields above.
	TierLimits map[model.NetworkTier]map[string]int64
	// How long a round's check waits for each mail network's reader to list every message so far: under the mail
	// settings' MailboxTimeout, which the end waits, so a lost message costs the case its bound once.
	MailboxRoundTimeout time.Duration
	// How old the personal networks read, past §21.7's probation.
	NetworkAge time.Duration
	// The grants of a personal network's document: mail's, dns:* admin and tls/443/web's standing publish, given when
	// it expires, and its life.
	MailGrants   []string
	DnsGrant     string
	TlsGrant     func(expires time.Time) string
	TlsGrantLife time.Duration
	// The rule each ACL save adds past its network's grants, one of its own by the save's number, so every save
	// changes the head.
	AclChurnGrant func(index int) string
	// The devices of each personal network, from its first, that save its documents: one, so no base is signed by a
	// second device (finding config-replay-base-signer). Every Owner's device of the team saves.
	PersonalAclSaverCount int
	// The certs worker's renewals the case runs across its rounds, the certs renewal kind's bursts past it doing
	// nothing: each order writes its name's challenge twice, under §12.7's ten an hour (dns_challenges_per_name_hour).
	CertRenewalCount int
	// The devices of a, from its first, that order its certificate at setup.
	CertDeviceCount int
	// The repo path every evs writer writes its own file under.
	EvsDirectory string
	// The host format of each dns writer's own TXT set, %d its place among the case's writers.
	DnsHostFormat string
	// The it bridge case's network on the case's world (newItBridgeNetwork): its subnets, forwards over their hosts,
	// clients and grant matrix, and the exchange's connection limit it asks, which the case raises before its first
	// network (churn_load_harness_test.go). nil runs no bridge.
	Bridge *ItBridgeSettings
	// The harnesses' own settings, which the case reuses.
	Evs    *evsConcurrencySettings
	Gkm    *gkmservice.GkmReconcilerSettings
	Mail   *mailConcurrencySettings
	Blog   *bloggerSettings
	Dns    *dnsCertsSettings
	Team   *WorldTeamSettings
	Ledger *controlPlaneLedgerSettings
	// How often the budget sampler reads every metadata cluster's reports: inside their 15 s ttl.
	BudgetSampleInterval time.Duration
	// The blocks restarted at the end so each drains its usage window before the ledgers.
	FinalDrainBlocks []string
}

// The chaos targets that are no block of the world's.
const (
	churnLoadChaosApi    = "api"
	churnLoadChaosCerts  = "certs"
	churnLoadChaosDevice = "device"
)

func defaultChurnLoadSettings() *churnLoadSettings {
	bursts := DefaultWorldBurstSettings()
	bursts.MaxPause = 2 * time.Second
	mail := defaultMailConcurrencySettings()
	mail.Groups = []string{"team"}
	blog := defaultBloggerSettings()
	dnsSettings := defaultDnsCertsSettings()
	dnsSettings.CertDeviceCount = 1
	team := DefaultWorldTeamSettings()
	team.OwnerDeviceCount = 2
	team.Tags = []string{}
	configBasesPerMinute := int64(6000)
	team.SidecarLimits["config_bases_per_min"] = configBasesPerMinute
	bridge := DefaultItBridgeSettings()
	bridge.SubnetCount = 1
	bridge.CampusPrefix = netip.Prefix{}
	bridge.ForwardCount = 2
	bridge.ForwardHostCount = 1
	bridge.ClientCount = 2
	reconciler := gkmservice.DefaultGkmReconcilerSettings()
	reconciler.PendingHintInterval = 10 * time.Second
	reconciler.DroppedLeafHintInterval = 10 * time.Second
	settings := &churnLoadSettings{
		CaseTimeout:          10 * time.Minute,
		SetupTimeout:         4 * time.Minute,
		RoundTimeout:         2 * time.Minute,
		CheckTimeout:         2 * time.Minute,
		FinalTimeout:         3 * time.Minute,
		PersonalNetworkCount: 2,
		PersonalClientCount:  4,
		BlogWriterCount:      2,
		BlogReaderCount:      2,
		RoundCount:           3,
		Seed:                 uint64(time.Now().UnixNano()),
		Mix: &churnLoadMixSettings{
			EvsWriters:    &churnLoadKindSettings{MaxBurstCount: 6, StepCount: 8},
			EvsReaders:    &churnLoadKindSettings{MaxBurstCount: 4, StepCount: 10},
			GkmMembership: &churnLoadKindSettings{MaxBurstCount: 1, StepCount: 3},
			GkmRotations:  &churnLoadKindSettings{MaxBurstCount: 2, StepCount: 2},
			MailSenders:   &churnLoadKindSettings{MaxBurstCount: 4, StepCount: 2},
			MailReceivers: &churnLoadKindSettings{MaxBurstCount: 2, StepCount: 2},
			BlogWriters:   &churnLoadKindSettings{MaxBurstCount: 2, StepCount: 6},
			BlogReaders:   &churnLoadKindSettings{MaxBurstCount: 2, StepCount: 12},
			DnsWriters:    &churnLoadKindSettings{MaxBurstCount: 4, StepCount: 2},
			DnsReaders:    &churnLoadKindSettings{MaxBurstCount: 1, StepCount: 20},
			CertRenewals:  &churnLoadKindSettings{MaxBurstCount: 1, StepCount: 1},
			AclSavers:     &churnLoadKindSettings{MaxBurstCount: 3, StepCount: 2},
			Enrollments:   &churnLoadKindSettings{MaxBurstCount: 1, StepCount: 2},
			Departures:    &churnLoadKindSettings{MaxBurstCount: 1, StepCount: 2},
			BridgeClients: &churnLoadKindSettings{MaxBurstCount: 2, StepCount: 10},
			BridgeForward: &churnLoadKindSettings{MaxBurstCount: 1, StepCount: 2},
		},
		Bursts: bursts,
		ChaosTargets: []string{
			churnLoadChaosApi, WorldBlockEvs, WorldBlockGkm, WorldBlockDns, WorldBlockDnsedge, WorldBlockTlsedge,
			WorldBlockSmtpedge, WorldBlockMxedge, WorldBlockSockets, churnLoadChaosCerts, churnLoadChaosDevice,
		},
		ChaosBetweenRoundCount:  1,
		ChaosDuringRoundCount:   2,
		ChaosFreeRoundCount:     1,
		ChaosMaxDelay:           15 * time.Second,
		RetryTimeout:            2 * time.Minute,
		RetryInterval:           500 * time.Millisecond,
		KeyResyncInterval:       45 * time.Second,
		GateLatency:             5 * time.Second,
		ClientApplyLatency:      5 * time.Second,
		WorkAllowance:           20 * time.Second,
		ConvergencePollInterval: 500 * time.Millisecond,
		ConfigBasesPerMinute:    configBasesPerMinute,
		MailboxRoundTimeout:     45 * time.Second,
		NetworkAge:              8 * 24 * time.Hour,
		MailGrants:              defaultWorldSmokeMailSettings().AclGrants,
		DnsGrant:                dnsSettings.DnsAclGrant,
		TlsGrant:                dnsSettings.TlsAclGrant,
		TlsGrantLife:            12 * time.Hour,
		AclChurnGrant: func(index int) string {
			return fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["dns:churn-%d"], "allow": ["admin"]}`, index)
		},
		PersonalAclSaverCount: 1,
		CertRenewalCount:      1,
		CertDeviceCount:       1,
		Bridge:                bridge,
		EvsDirectory:          "churn",
		DnsHostFormat:         "churn-%d",
		Evs:                   defaultEvsConcurrencySettings(),
		Gkm:                   reconciler,
		Mail:                  mail,
		Blog:                  blog,
		Dns:                   dnsSettings,
		Team:                  team,
		Ledger:                defaultControlPlaneLedgerSettings(),
		BudgetSampleInterval:  time.Second,
		FinalDrainBlocks:      []string{WorldBlockTlsedge, WorldBlockSmtpedge, WorldBlockMxedge, WorldBlockSockets, WorldBlockEvs},
	}
	settings.Bridge.Seed = settings.Seed
	settings.TierLimits = map[model.NetworkTier]map[string]int64{
		model.NetworkTierFree: {"config_bases_per_min": configBasesPerMinute},
		model.NetworkTierPro:  {"config_bases_per_min": configBasesPerMinute},
		model.NetworkTierTeam: {"config_bases_per_min": configBasesPerMinute},
	}
	return settings
}

// The convergence bound on every holder holding a repo's newest generation: §8.9's latencies, the sidecars' own
// pending wait and jitter and late jitter (the world's sidecars run gkm's defaults), the requests' allowance, and with a
// hint a chaos may have lost, the resync interval.
func (self *churnLoadSettings) keyConvergenceBound(lostHints bool) time.Duration {
	gkm := sidecar.DefaultSidecarSettings().Gkm
	bound := self.GateLatency + self.ClientApplyLatency + gkm.PendingCommitDelay + gkm.PendingCommitJitter + gkm.LateDeliverJitter + self.WorkAllowance
	if lostHints {
		bound += self.KeyResyncInterval
	}
	return bound
}

// The convergence bound on a network's devices following its log to the owner's tip: §8.9's latencies and the
// requests' allowance, and with a hint a chaos may have lost, the resync interval.
func (self *churnLoadSettings) configConvergenceBound(lostHints bool) time.Duration {
	bound := self.GateLatency + self.ClientApplyLatency + self.WorkAllowance
	if lostHints {
		bound += self.KeyResyncInterval
	}
	return bound
}

// One device of the case. Safe for concurrent use.
type churnLoadDevice struct {
	device  *sidecart3.T3Device
	network *churnLoadNetwork
	// whether device chaos may restart it: no mail client, dns writer or webserver is bound to its Sidecar
	restartable bool
	// a step holds it shared, a restart or a departure exclusive (T3Device.Restart must not run beside a use)
	useLock sync.RWMutex

	stateLock sync.Mutex
	// it left: revoked, or its member removed; what it raises from then is logged, not failed
	departed bool
	// how many writes its evs path has begun
	evsSequence int
	// every alarm it raised, and how many a check has reported
	alarms        []string
	alarmsChecked int
	removeAlarm   func()
}

// Whether the device left the network.
func (self *churnLoadDevice) gone() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.departed
}

// Runs use with the device held shared, unless it left: then nothing, and false.
func (self *churnLoadDevice) use(use func() error) (bool, error) {
	self.useLock.RLock()
	defer self.useLock.RUnlock()
	if self.gone() {
		return false, nil
	}
	return true, use()
}

// Watches the device's current Sidecar for the alarms the case refuses, replacing any watch of an earlier one.
func (self *churnLoadDevice) watch() {
	remove := self.device.Sidecar().AddEventCallback(self.event)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.removeAlarm != nil {
		self.removeAlarm()
	}
	self.removeAlarm = remove
}

// Records an alarm the case refuses.
func (self *churnLoadDevice) event(event sidecar.SidecarEvent) {
	line := ""
	switch event.Kind {
	case sidecar.SidecarEventHeadAlarm:
		line = fmt.Sprintf("head_alarm %s", event.HeadAlarm)
	case sidecar.SidecarEventRefused:
		if event.GroupId == nil {
			line = "refused: its generation"
		} else {
			line = fmt.Sprintf("refused: gkm group %s", *event.GroupId)
		}
	case sidecar.SidecarEventEvsAlarm:
		line = fmt.Sprintf("evs_alarm %s (%s) at version %d", event.EvsAlarm, event.EvsAlarmDetail, event.Version)
	case sidecar.SidecarEventEvsGap:
		line = fmt.Sprintf("evs_gap verified since %s", event.VerifiedSince)
	case sidecar.SidecarEventKeyChanged:
		line = "key_changed"
	case sidecar.SidecarEventHistoryDrop:
		if event.DropState == sidecar.SidecarDropInvalid {
			line = "history_drop invalid"
		}
	}
	if line == "" {
		return
	}
	if event.RepoId != nil {
		line += fmt.Sprintf(" on repo %s", *event.RepoId)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	line = fmt.Sprintf("%s at %s", line, time.Now().Format("15:04:05.000"))
	if self.departed {
		line += " (after it left)"
	}
	self.alarms = append(self.alarms, line)
}

// One ACL save its saver was answered for.
type churnLoadAclSave struct {
	saver  *churnLoadDevice
	index  int
	source string
	// its base's log position
	position uint64
	round    int
}

// One network of the case and what its workers did there. Safe for concurrent use once set up.
type churnLoadNetwork struct {
	*WorldNetwork
	// the team, nil for a personal network
	team *WorldTeam
	// the shared evs repo and the evs case over it whose write, read and verification the workers reuse; nil for the
	// blog network
	evs     *evsConcurrencyRepo
	evsCase *evsConcurrencyCase
	// the partial repo whose membership the gkm workers churn, and the device it is granted to for good; nil off a
	// personal network
	partial       sidecar.Id
	partialWriter *churnLoadDevice
	// the network's config repo, and the team's directory, which every member reads
	configRepoId    sidecar.Id
	directoryRepoId sidecar.Id
	// the grants every document of the network keeps, and how a document is written of them
	baseGrants []string
	document   func(grants ...string) string
	// the mail and dns parts, nil off a personal network
	mail *mailNetwork
	dns  *dnsCertsNetwork

	stateLock sync.Mutex
	devices   []*churnLoadDevice
	// the devices that save its documents
	savers []*churnLoadDevice
	saves  []*churnLoadAclSave
	// the members the churn added, the ones it removed, and the devices it added and revoked
	joined         []*WorldTeamMember
	removed        []*WorldTeamMember
	enrolled       []*churnLoadDevice
	revoked        []*churnLoadDevice
	membershipTime time.Time
}

// Every device of the network the case runs, in order.
func (self *churnLoadNetwork) deviceList() []*churnLoadDevice {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.devices)
}

// The devices that have not left.
func (self *churnLoadNetwork) liveDevices() []*churnLoadDevice {
	return slices.DeleteFunc(self.deviceList(), func(device *churnLoadDevice) bool { return device.gone() })
}

// The case's device of t3, nil for none.
func (self *churnLoadNetwork) deviceOf(t3 *sidecart3.T3Device) *churnLoadDevice {
	for _, device := range self.deviceList() {
		if device.device == t3 {
			return device
		}
	}
	return nil
}

// The owner's device: the network's first.
func (self *churnLoadNetwork) owner() *churnLoadDevice {
	return self.deviceList()[0]
}

// One chaos restart: what it restarted, when, and how it ended.
type churnLoadChaos struct {
	round     int
	target    string
	during    bool
	startedAt time.Time
	endedAt   time.Time
	err       error
}

// The case: the stack, its networks and every record the checks read.
type churnLoadCase struct {
	t        testing.TB
	settings *churnLoadSettings
	stack    *dnsCertsStack
	world    *World

	origin      *gkmConcurrencyOrigin
	reconciler  *gkmConcurrencyReconciler
	sampler     *evsConcurrencyBudgetSampler
	worker      *evsConcurrencyWorker
	socketCalls *bloggerSocketCalls

	personal []*churnLoadNetwork
	blogNet  *churnLoadNetwork
	team     *churnLoadNetwork
	networks []*churnLoadNetwork
	blog     *bloggerBlog
	// the it bridge case's network, nil where Bridge is
	bridge *ItBridgeWorld
	// mail's ledger over every message of the case, and its networks
	mailLedger   *mailLedger
	mailNetworks []*mailNetwork
	// dns's records: the zone keys, the journal's start, every reader; and its networks
	dnsRecords  *dnsCertsRecords
	dnsNetworks []*dnsCertsNetwork
	// the certificate publishers setup ordered
	publishers []*dnsCertsPublisher

	stateLock sync.Mutex
	chaos     []*churnLoadChaos
	// each kind's bursts so far, by its name
	kindCounts map[string]int
	// each retry of a step refused for now, by its reason
	retryCounts map[string]int
	// the ACL saves' numbers, the dns writes' and the mail tokens'
	saveCount  int
	tokenCount int
	renewals   int
	// each publisher's newest issued certificate row, from its first on, which the next renewal renews
	currentCertIds map[*dnsCertsPublisher]sidecar.Id
	// each message a mailbox check reported missing, "<token> in <mailbox>" or "<token> (its sent copy)", which later
	// checks wait for no more
	mailMissing map[string]bool
	// the blog's chaos windows of the round under way, which its visit checks read
	blogChaos *bloggerChaos
}

// See the file's header.
func TestE2eChurnLoad(t *testing.T) {
	settings := defaultChurnLoadSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		runChurnLoad(t, settings)
	})
}

// The case on settings: see the file's header.
func runChurnLoad(t testing.TB, settings *churnLoadSettings) {
	switch {
	case settings.PersonalNetworkCount < 1 || settings.PersonalClientCount < 4:
		t.Fatalf("%d personal networks of %d sidecars hold no mail network's roles: an owner, a prover host, a sender and a receiver", settings.PersonalNetworkCount, settings.PersonalClientCount)
	case settings.BlogWriterCount < 1 || settings.BlogReaderCount < 1:
		t.Fatalf("a blog of %d writers and %d readers serves nothing", settings.BlogWriterCount, settings.BlogReaderCount)
	case settings.RoundCount < 1:
		t.Fatalf("%d rounds run nothing", settings.RoundCount)
	case settings.Team.OwnerDeviceCount < 2:
		t.Fatalf("a team of %d Owner devices leaves device chaos no device beside the one that admits", settings.Team.OwnerDeviceCount)
	}
	t.Logf("the churn load: seed %d, %d rounds", settings.Seed, settings.RoundCount)
	dnsSettings := *settings.Dns
	dnsSettings.CaseTimeout = settings.CaseTimeout
	dnsSettings.CertDeviceCount = settings.CertDeviceCount
	settings.Dns = &dnsSettings
	stack := newDnsCertsStack(t, settings.Dns)
	world := stack.World
	defer world.Close()
	churnLoadSetKeyResyncInterval(world, settings.KeyResyncInterval)
	popLimits := churnLoadInstallLimits(settings.TierLimits)
	defer popLimits()
	if settings.Bridge != nil {
		ctx, cancel := world.StepContext(t.Context(), settings.SetupTimeout)
		err := churnLoadRaiseExchangeConnections(ctx, world, settings.Bridge.ExchangeConnectionMaxCount)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}

	// the recorder at gkm's origin before any device, so every claim passes it
	origin, err := installGkmConcurrencyOrigin(world)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := startGkmConcurrencyReconciler(world, settings.Gkm)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.close()
	sampler := startEvsConcurrencyBudgetSampler(world, settings.BudgetSampleInterval)
	defer sampler.stop()
	worker, err := newEvsConcurrencyWorker(world)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.close()
	socketCalls, err := installBloggerSocketCalls(world, settings.Blog.SocketCalls)
	if err != nil {
		t.Fatal(err)
	}
	self := &churnLoadCase{
		t:              t,
		settings:       settings,
		stack:          stack,
		world:          world,
		origin:         origin,
		reconciler:     reconciler,
		sampler:        sampler,
		worker:         worker,
		socketCalls:    socketCalls,
		mailLedger:     newMailLedger(),
		kindCounts:     map[string]int{},
		retryCounts:    map[string]int{},
		currentCertIds: map[*dnsCertsPublisher]sidecar.Id{},
		mailMissing:    map[string]bool{},
	}
	defer self.release()

	setupStartedAt := time.Now()
	ctx, cancel := world.StepContext(t.Context(), settings.SetupTimeout)
	if err := self.setUp(ctx); err != nil {
		cancel()
		world.FailProgress(t, "the setup", err, nil)
	}
	cancel()
	t.Logf("the setup stood %s after it began", time.Since(setupStartedAt).Round(time.Millisecond))

	// one record of chaos windows over the case, which every round's visit checks read
	self.blogChaos = &bloggerChaos{completedReads: sidecar.NewMonitorValue(0)}
	for round := range settings.RoundCount {
		if 0 < round {
			self.chaosBetween(round)
		}
		roundStartedAt := self.runRound(round)
		self.checkRound(round, roundStartedAt)
	}
	self.finish()
	self.logSummary()
}

// Ends what the case started on its devices: the bridges, the blog's webservers and subscriptions, and every alarm
// watch.
func (self *churnLoadCase) release() {
	if self.bridge != nil {
		self.bridge.Close()
	}
	if self.blog != nil {
		for _, writer := range self.blog.writers {
			writer.stop()
			if writer.subscription != nil {
				writer.subscription.Close()
			}
		}
	}
	for _, network := range self.networks {
		for _, device := range network.deviceList() {
			device.stateLock.Lock()
			remove := device.removeAlarm
			device.removeAlarm = nil
			device.stateLock.Unlock()
			if remove != nil {
				remove()
			}
		}
	}
}

// The networks and every part each runs (the file's header).
func (self *churnLoadCase) setUp(ctx context.Context) error {
	settings := self.settings
	world := self.world

	// the personal networks: aged, verified Pro, their documents
	certsResource := world.T3().Api().ServiceOrigin("certs")
	for index := range settings.PersonalNetworkCount {
		added, err := world.AddNetwork(ctx, worldNetworkName(index), settings.PersonalClientCount, func(name string, sidecarSettings *sidecar.SidecarSettings) {
			sidecarSettings.Tls.CertsResource = certsResource
		})
		if err != nil {
			return fmt.Errorf("personal network %d: %w", index, err)
		}
		if err := worldAgeNetwork(ctx, added.NetworkId, settings.NetworkAge); err != nil {
			return err
		}
		if err := world.VerifyNetwork(ctx, added, true, nil); err != nil {
			return fmt.Errorf("%s's paid balance: %w", added.Name, err)
		}
		network := self.addNetwork(added, nil, false)
		network.baseGrants = slices.Concat(settings.MailGrants, []string{settings.DnsGrant, settings.TlsGrant(time.Now().Add(settings.TlsGrantLife))})
		network.document = worldAclPolicy
		devices := network.deviceList()
		network.savers = devices[:min(max(1, settings.PersonalAclSaverCount), len(devices))]
		if _, err := world.SaveAclPolicy(ctx, added, added.Owner(), network.document(network.baseGrants...)); err != nil {
			return fmt.Errorf("%s's document: %w", added.Name, err)
		}
		self.personal = append(self.personal, network)
	}

	// the blog network and its blog, published and routed through tlsedge
	blogAdded, err := world.AddNetwork(ctx, worldNetworkName(settings.PersonalNetworkCount), settings.BlogWriterCount, nil)
	if err != nil {
		return fmt.Errorf("the blog network: %w", err)
	}
	self.blogNet = self.addNetwork(blogAdded, nil, false)
	if self.blog, err = newBloggerBlog(ctx, world, settings.Blog, blogAdded, 0, settings.BlogReaderCount, self.socketCalls); err != nil {
		return fmt.Errorf("%s's blog: %w", blogAdded.Name, err)
	}
	if err := self.blog.publish(ctx); err != nil {
		return fmt.Errorf("%s's publish: %w", blogAdded.Name, err)
	}
	for _, writer := range self.blog.writers {
		if _, err := writer.start(ctx); err != nil {
			return fmt.Errorf("%s's webserver: %w", writer.device.Name, err)
		}
	}
	if err := self.blog.awaitRoute(ctx); err != nil {
		return fmt.Errorf("the blog's route through tlsedge: %w", err)
	}

	// the it bridge case's network: its block, document and every bridge adopted, then every subnet and forward joined
	if settings.Bridge != nil {
		self.bridge = newItBridgeNetwork(self.t, world, settings.Bridge, world.T3().Settings().Exchange)
		if err := self.bridge.JoinAll(ctx); err != nil {
			return fmt.Errorf("the bridge's first joins: %w", err)
		}
	}

	// the team, its Owner's devices the device chaos's targets
	team, err := world.AddTeamNetwork(ctx, settings.Team)
	if err != nil {
		return fmt.Errorf("the team: %w", err)
	}
	self.team = self.addNetwork(team.WorldNetwork, team, true)
	self.team.baseGrants = []string{}
	self.team.document = func(grants ...string) string { return team.AclPolicy(grants...) }
	self.team.savers = self.team.deviceList()
	self.team.directoryRepoId = team.DirectoryId()

	// the shared evs repos, and each personal network's partial repo
	for _, network := range slices.Concat(self.personal, []*churnLoadNetwork{self.team}) {
		repoId, err := network.CreateSharedRepo(ctx, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER)
		if err != nil {
			return fmt.Errorf("%s's shared repo: %w", network.Name, err)
		}
		network.evs = newEvsConcurrencyRepo(network.Name+"'s shared repo", repoId)
		network.evsCase = &evsConcurrencyCase{t: self.t, settings: settings.Evs, world: world, repos: []*evsConcurrencyRepo{network.evs}}
	}
	for _, network := range self.personal {
		owner := network.owner()
		repo, err := network.CreateRepo(ctx, owner.device, protocol.RepoKind_REPO_KIND_GENERAL)
		if err != nil {
			return fmt.Errorf("%s's partial repo: %w", network.Name, err)
		}
		network.partial = repo.RepoId()
		network.partialWriter = network.deviceList()[1]
		if _, err := network.Grant(ctx, owner.device, network.partial, network.partialWriter.device.Sidecar().Identity().SidecarId, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER); err != nil {
			return fmt.Errorf("%s's partial repo's grant: %w", network.Name, err)
		}
	}

	// dns: every personal network's sidecars writers of their own sets, each's first write answered so the API's gate
	// holds the document's grant, then a's certificate through the certs worker
	writerIndex := 0
	for _, network := range self.personal {
		dnsNetwork := &dnsCertsNetwork{WorldNetwork: network.WorldNetwork}
		for _, device := range network.deviceList() {
			writer, err := newDnsCertsWriter(world, dnsNetwork, device.device, writerIndex)
			if err != nil {
				return fmt.Errorf("%s's dns client: %w", device.device.Name, err)
			}
			dnsNetwork.Writers = append(dnsNetwork.Writers, writer)
			writerIndex += 1
		}
		zone, err := dnsNetwork.Writers[0].zone(ctx, settings.Dns.Dns)
		if err != nil || zone.Apex() == "" {
			return fmt.Errorf("%s's zone: %+v, %w", network.Name, zone, err)
		}
		dnsNetwork.Root = dns.CanonicalName(zone.Root)
		dnsNetwork.Apex = dns.CanonicalName(zone.Apex())
		for _, writer := range dnsNetwork.Writers {
			owner := dnsNetwork.OwnerName(fmt.Sprintf(settings.Dns.WarmHostFormat, writer.Index))
			if _, err := world.SetDnsRecord(ctx, writer.Device, &sidecar.DnsRecordUpsert{Name: owner, Type: "TXT", Values: []string{fmt.Sprintf("warm-%d", writer.Index)}}, settings.Dns.Dns); err != nil {
				return fmt.Errorf("%s's first dns write: %w", writer.Device.Name, err)
			}
		}
		network.dns = dnsNetwork
		self.dnsNetworks = append(self.dnsNetworks, dnsNetwork)
	}
	// the certificate phase holds its own checks and ends the case where an order fails
	self.publishers = dnsCertsCertificatesPhase(self.t, self.stack, settings.Dns, self.dnsNetworks[:1])
	for _, publisher := range self.publishers {
		self.currentCertIds[publisher] = publisher.firstCertId
	}

	// mail, after the certificate's publish, so each sender's gate is mail's stand-in over the stock gate
	for _, network := range self.personal {
		mail, err := setUpMailNetwork(ctx, world, settings.Mail, network.WorldNetwork)
		if err != nil {
			return fmt.Errorf("%s's mail: %w", network.Name, err)
		}
		network.mail = mail
		self.mailNetworks = append(self.mailNetworks, mail)
	}

	// the dns records' start: the journal's serial and each zone's version, every set written from here on tracked
	root := self.dnsNetworks[0].Root
	startSerial, err := dnsCertsRootSerial(ctx, root)
	if err != nil {
		return err
	}
	self.dnsRecords = &dnsCertsRecords{Keys: self.stack.Keys, Root: root, StartSerial: startSerial, PhaseStart: time.Now(), StartVersions: map[server.Id]uint64{}}
	for _, network := range self.dnsNetworks {
		zone, err := network.Writers[0].zone(ctx, settings.Dns.Dns)
		if err != nil {
			return err
		}
		self.dnsRecords.StartVersions[network.NetworkId] = zone.Version
	}

	// each network's config repo, as its owner's verified state names it, and every device watched for alarms
	for _, network := range self.networks {
		configRepoId, err := gkmConcurrencyConfigRepoId(network.owner().device)
		if err != nil {
			return err
		}
		network.configRepoId = configRepoId
		for _, device := range network.deviceList() {
			device.watch()
		}
	}
	return nil
}

// Keeps a network of the case with its devices so far, the team's devices restartable.
func (self *churnLoadCase) addNetwork(world *WorldNetwork, team *WorldTeam, restartable bool) *churnLoadNetwork {
	network := &churnLoadNetwork{WorldNetwork: world, team: team}
	for _, device := range world.Clients {
		network.devices = append(network.devices, &churnLoadDevice{device: device, network: network, restartable: restartable})
	}
	self.networks = append(self.networks, network)
	return network
}

// One round (the file's header): its draw of workers, its chaos when past ChaosFreeRoundCount, all at once. When it
// began.
func (self *churnLoadCase) runRound(round int) time.Time {
	t, settings := self.t, self.settings
	random := rand.New(rand.NewPCG(settings.Seed, uint64(round)+1))
	phase := churnLoadPhase(round)
	bursts, counts := self.roundBursts(round, random)
	if settings.ChaosFreeRoundCount <= round {
		bursts = append(bursts, self.chaosBurst(round))
	}
	t.Logf("%s: workers %s", phase, churnLoadDescribeCounts(counts))
	burstSettings := *settings.Bursts
	burstSettings.Seed = settings.Seed + uint64(round)
	burstSettings.PhaseTimeout = settings.RoundTimeout
	ctx, cancel := self.world.StepContext(t.Context(), settings.RoundTimeout)
	defer cancel()
	startedAt := time.Now()
	if err := RunBursts(ctx, t, phase, bursts, &burstSettings); err != nil {
		self.world.FailProgress(t, phase, err, bursts)
	}
	FailBurstErrors(t, bursts)
	t.Logf("%s took %s", phase, time.Since(startedAt).Round(time.Millisecond))
	return startedAt
}

// A round's name, as its visits and failures name it.
func churnLoadPhase(round int) string {
	return fmt.Sprintf("round %d", round)
}

// Each kind's count, sorted by name, as one line.
func churnLoadDescribeCounts(counts map[string]int) string {
	lines := []string{}
	for name, count := range counts {
		lines = append(lines, fmt.Sprintf("%s %d", name, count))
	}
	slices.Sort(lines)
	return strings.Join(lines, ", ")
}

// Counts one retry of a step refused for now, by its reason.
func (self *churnLoadCase) countRetry(reason string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.retryCounts[reason] += 1
}

// The chaos restarts so far, oldest first.
func (self *churnLoadCase) chaosList() []*churnLoadChaos {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.chaos)
}

// Whether a chaos restart overlapped the span from since on: a hint it took down may be lost (§6.8).
func (self *churnLoadCase) chaosSince(since time.Time) bool {
	for _, chaos := range self.chaosList() {
		if !chaos.endedAt.Before(since) {
			return true
		}
	}
	return false
}

// The case's summary: the chaos it ran, each kind's bursts, the retries by reason and the blocks' run counts.
func (self *churnLoadCase) logSummary() {
	t := self.t
	for _, chaos := range self.chaosList() {
		when := "between rounds"
		if chaos.during {
			when = "during it"
		}
		t.Logf("chaos in round %d, %s: %s restarted in %s, %v", chaos.round, when, chaos.target, chaos.endedAt.Sub(chaos.startedAt).Round(time.Millisecond), chaos.err)
	}
	self.stateLock.Lock()
	kinds := churnLoadDescribeCounts(self.kindCounts)
	retries := churnLoadDescribeCounts(self.retryCounts)
	self.stateLock.Unlock()
	t.Logf("bursts by kind over the case: %s", kinds)
	t.Logf("steps tried again, by reason: %s", retries)
	runs := []string{}
	for _, block := range self.world.blocks {
		runs = append(runs, fmt.Sprintf("%s %d", block.Name(), block.RunCount()))
	}
	t.Logf("block runs: %s; the certs worker's runs %d", strings.Join(runs, ", "), self.stack.Worker.RunCount())
}
