//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The churn load case's checks (churn_load_test.go's header): after each round, every acknowledged effect present
// once, every device converged within the design's bounds and no false alarm; at the end, every ledger reconciled
// exactly against an independent count and nothing left in flight. Each check reuses the scenario whose effect it
// checks: evs's verification (evsConcurrencyCase.verifyDevice), dns's journal checks (dnsCertsCheckRecords), the
// mailboxes' counts (mailCountMailbox), the blog's visit checks (checkBloggerVisits), the control-plane case's api
// budget (controlPlaneCheckApiBudget), and the certificate case's budget and challenge counts.

package sidecare2e

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// The checks after a round (the file's header).
func (self *churnLoadCase) checkRound(round int, roundStartedAt time.Time) {
	t, settings := self.t, self.settings
	phase := churnLoadPhase(round)
	ctx, cancel := self.world.StepContext(t.Context(), settings.CheckTimeout)
	defer cancel()
	startedAt := time.Now()
	// a hint a chaos took down since the round began may be lost, which the resync interval bounds (§6.8)
	lostHints := self.chaosSince(roundStartedAt.Add(-settings.ChaosMaxDelay))
	self.checkConfigConvergence(ctx, phase, lostHints)
	self.checkKeyConvergence(ctx, phase, lostHints)
	self.checkMembership(ctx, phase)
	self.checkAclSaves(ctx, phase)
	self.checkEvs(ctx, phase, false)
	self.checkDns(ctx, phase)
	self.checkMailboxes(ctx, phase)
	self.checkBlog(phase, roundStartedAt)
	self.checkAlarms(phase)
	self.logRefusals(phase)
	t.Logf("%s's checks took %s", phase, time.Since(startedAt).Round(time.Millisecond))
}

// Every network's live devices follow its log to its owner's tip within the config bound, and then stand on one pin
// and one base (§8.9).
func (self *churnLoadCase) checkConfigConvergence(ctx context.Context, phase string, lostHints bool) {
	t := self.t
	bound := self.settings.configConvergenceBound(lostHints)
	for _, network := range self.networks {
		owner := network.owner()
		follower := owner.device.Follower()
		if follower == nil || follower.Client() == nil {
			t.Errorf("%s: %s's owner follows no log", phase, network.Name)
			continue
		}
		if err := follower.Client().CatchUp(ctx); err != nil {
			t.Errorf("%s: %s's owner's catch-up: %v", phase, network.Name, err)
			continue
		}
		tip := follower.Client().Pin().GetVersion()
		startedAt := time.Now()
		syncCtx, cancel := context.WithTimeout(ctx, bound)
		err := network.Sync(syncCtx, tip)
		cancel()
		if err != nil {
			t.Errorf("%s: %s's devices did not follow its log to %d within %s: %v", phase, network.Name, tip, bound, err)
			continue
		}
		live := []*sidecart3.T3Device{}
		for _, device := range network.liveDevices() {
			live = append(live, device.device)
		}
		if divergent := controlPlaneDivergent(live); 0 < len(divergent) {
			t.Errorf("%s: %s's devices stand on more than one pin past its tip %d: %s", phase, network.Name, tip, strings.Join(divergent, "; "))
			continue
		}
		t.Logf("%s: %s's %d live devices stand at n %d, %s after the check began", phase, network.Name, len(live), tip, time.Since(startedAt).Round(time.Millisecond))
	}
}

// The repos whose keys the case holds every reader of to gkm's newest generation: each network's config repo, its
// shared repo, a personal network's partial repo, the team's directory and the blog's repo.
func (self *churnLoadCase) keyRepos(network *churnLoadNetwork) []*gkmConcurrencyRepo {
	repos := []*gkmConcurrencyRepo{{name: network.Name + "'s config repo", repoId: network.configRepoId, config: true}}
	if network.evs != nil {
		repos = append(repos, &gkmConcurrencyRepo{name: network.evs.name, repoId: network.evs.repoId})
	}
	if network.partial != (sidecar.Id{}) {
		repos = append(repos, &gkmConcurrencyRepo{name: network.Name + "'s partial repo", repoId: network.partial})
	}
	if network.directoryRepoId != (sidecar.Id{}) {
		repos = append(repos, &gkmConcurrencyRepo{name: network.Name + "'s directory", repoId: network.directoryRepoId})
	}
	if network == self.blogNet {
		repos = append(repos, &gkmConcurrencyRepo{name: network.Name + "'s blog repo", repoId: self.blog.repoId})
	}
	return repos
}

// Every reader of each repo, as its network owner's verified state has its readers (ConfigState.RepoReaders), holds
// gkm's newest generation of it as the recorder at gkm's origin saw it taken, within the key bound; and gkm took no two
// commitments at one generation.
func (self *churnLoadCase) checkKeyConvergence(ctx context.Context, phase string, lostHints bool) {
	t, settings := self.t, self.settings
	view := &gkmConcurrencyCase{origin: self.origin}
	if _, forks := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList()); 0 < len(forks) {
		for _, fork := range forks {
			t.Errorf("%s: a fork at gkm: %s", phase, fork)
		}
	}
	bound := settings.keyConvergenceBound(lostHints)
	deadline := time.Now().Add(bound)
	startedAt := time.Now()
	for _, network := range self.networks {
		follower := network.owner().device.Follower()
		if follower == nil || follower.Client() == nil || follower.Client().State() == nil {
			t.Errorf("%s: %s's owner holds no state to read its repos' readers by", phase, network.Name)
			continue
		}
		state := follower.Client().State()
		for _, repo := range self.keyRepos(network) {
			readers := state.RepoReaders(repo.repoId)
			holders := []*churnLoadDevice{}
			for _, device := range network.liveDevices() {
				if slices.Contains(readers, device.device.Sidecar().Identity().SidecarId) {
					holders = append(holders, device)
				}
			}
			for {
				newest := view.newestGeneration(repo)
				lagging := []string{}
				if newest == nil {
					lagging = append(lagging, "gkm took no generation of it")
				} else {
					for _, holder := range holders {
						if line := view.holdsNewest(ctx, &gkmConcurrencyDevice{device: holder.device, name: holder.device.Name}, repo, newest); line != "" {
							lagging = append(lagging, line)
						}
					}
				}
				if len(lagging) == 0 {
					break
				}
				if time.Now().After(deadline) || ctx.Err() != nil {
					t.Errorf("%s: %s's readers do not hold gkm's newest generation within %s: %s", phase, repo.name, bound, strings.Join(lagging, "; "))
					for _, holder := range holders {
						for _, line := range self.world.SidecarLogLines(holder.device.Name) {
							t.Logf("%s | %s", holder.device.Name, line)
						}
					}
					break
				}
				if err := evsConcurrencyPause(ctx, settings.ConvergencePollInterval); err != nil {
					break
				}
			}
		}
	}
	t.Logf("%s: the key check took %s of its %s bound", phase, time.Since(startedAt).Round(time.Millisecond), bound)
}

// The team's members and devices as the churn left them, in every live device's verified state and in api's rows:
// every joined member not removed named, every removed one gone, every enrolled device admitted and every revoked one
// revoked.
func (self *churnLoadCase) checkMembership(ctx context.Context, phase string) {
	t := self.t
	network := self.team
	team := network.team
	network.stateLock.Lock()
	joined := slices.Clone(network.joined)
	removed := slices.Clone(network.removed)
	enrolled := slices.Clone(network.enrolled)
	revoked := slices.Clone(network.revoked)
	network.stateLock.Unlock()
	expected := map[sidecar.Id]string{team.Owner.MemberId: team.Owner.Name}
	for _, member := range joined {
		if !slices.Contains(removed, member) {
			expected[member.MemberId] = member.Name
		}
	}
	for _, device := range network.liveDevices() {
		follower := device.device.Follower()
		if follower == nil || follower.Client() == nil || follower.Client().State() == nil {
			t.Errorf("%s: %s follows no log", phase, device.device.Name)
			continue
		}
		state := follower.Client().State()
		for memberId, name := range expected {
			if state.Member(memberId) == nil {
				t.Errorf("%s: %s's state names no member %s (%s)", phase, device.device.Name, memberId, name)
			}
		}
		for _, memberId := range state.MemberIds() {
			if _, ok := expected[memberId]; !ok {
				t.Errorf("%s: %s's state names a member %s the churn did not keep", phase, device.device.Name, memberId)
			}
		}
		for _, other := range enrolled {
			row := state.SidecarRow(other.device.Sidecar().Identity().SidecarId)
			wantRevoked := slices.Contains(revoked, other)
			switch {
			case row == nil:
				t.Errorf("%s: %s's state never admitted %s", phase, device.device.Name, other.device.Name)
			case wantRevoked && row.GetRevokedAt() == 0:
				t.Errorf("%s: %s's state holds %s admitted after its revocation", phase, device.device.Name, other.device.Name)
			case !wantRevoked && row.GetRevokedAt() != 0:
				t.Errorf("%s: %s's state holds %s revoked at %d, which the churn never revoked", phase, device.device.Name, other.device.Name, row.GetRevokedAt())
			}
		}
	}
	roles, err := churnLoadMemberRoles(ctx, network.NetworkId)
	if err != nil {
		t.Errorf("%s: api's member rows: %v", phase, err)
		return
	}
	for memberId, name := range expected {
		if _, ok := roles[server.Id(memberId)]; !ok {
			t.Errorf("%s: api holds no member row of %s (%s)", phase, memberId, name)
		}
	}
	for memberId := range roles {
		if _, ok := expected[sidecar.Id(memberId)]; !ok {
			t.Errorf("%s: api holds a member row %s the churn did not keep", phase, memberId)
		}
	}
	clientIds, err := churnLoadActiveClientIds(ctx, network.NetworkId)
	if err != nil {
		t.Errorf("%s: api's client rows: %v", phase, err)
		return
	}
	for _, device := range enrolled {
		clientId := server.Id(device.device.Sidecar().Identity().ClientId)
		if !slices.Contains(revoked, device) && !clientIds[clientId] {
			t.Errorf("%s: api holds no active client of %s, which the churn enrolled and never revoked", phase, device.device.Name)
		}
	}
	t.Logf("%s: the team holds %d members (%d joined, %d removed) and %d devices the churn enrolled (%d revoked)", phase, len(expected), len(joined), len(removed), len(enrolled), len(revoked))
}

// Every ACL save its saver was answered for stands at a log position of its own, and the newest is the document the
// head holds: planning it again on the network's owner adds and removes no rule.
func (self *churnLoadCase) checkAclSaves(ctx context.Context, phase string) {
	t, settings := self.t, self.settings
	retry := &controlPlaneRetrySettings{Timeout: settings.RetryTimeout, Interval: settings.RetryInterval}
	for _, network := range self.evsNetworks() {
		network.stateLock.Lock()
		saves := slices.Clone(network.saves)
		network.stateLock.Unlock()
		if len(saves) == 0 {
			continue
		}
		positions := map[uint64]*churnLoadAclSave{}
		var newest *churnLoadAclSave
		for _, save := range saves {
			if held, ok := positions[save.position]; ok {
				t.Errorf("%s: %s's saves %d and %d were both answered at log position %d", phase, network.Name, held.index, save.index, save.position)
			}
			positions[save.position] = save
			if newest == nil || newest.position < save.position {
				newest = save
			}
		}
		owner := network.owner()
		_, err := owner.use(func() error {
			plan, err := controlPlanePlanAcl(ctx, owner.device, []byte(newest.source), retry)
			if err != nil {
				return err
			}
			if 0 < len(plan.Preview.RulesAdded) || 0 < len(plan.Preview.RulesRemoved) {
				return fmt.Errorf("the head does not hold save %d's document, the newest answered (at %d): planning it again adds %d rules and removes %d", newest.index, newest.position, len(plan.Preview.RulesAdded), len(plan.Preview.RulesRemoved))
			}
			return nil
		})
		if err != nil {
			t.Errorf("%s: %s's documents: %v", phase, network.Name, err)
		}
		t.Logf("%s: %s holds %d answered saves, the newest save %d at %d", phase, network.Name, len(saves), newest.index, newest.position)
	}
}

// Every network's shared repo verified on a live device, or on every live device at the end: every acknowledged commit
// once in its path's history at what its writer was answered, its content read back, main's versions each one
// acknowledged commit (evsConcurrencyCase.verifyDevice); and no ref version acknowledged twice.
func (self *churnLoadCase) checkEvs(ctx context.Context, phase string, everyDevice bool) {
	t, settings := self.t, self.settings
	for _, network := range self.evsNetworks() {
		network.evs.stateLock.Lock()
		duplicates := slices.Clone(network.evs.duplicates)
		network.evs.stateLock.Unlock()
		for _, duplicate := range duplicates {
			t.Errorf("%s: %s", phase, duplicate)
		}
		if len(network.evs.commitList()) == 0 {
			continue
		}
		devices := network.liveDevices()
		if !everyDevice {
			devices = devices[:1]
		}
		tickets := make(chan struct{}, max(1, settings.Evs.VerifyConcurrency))
		errs := make([][]error, len(devices))
		var group sync.WaitGroup
		for index, device := range devices {
			group.Go(func() {
				tickets <- struct{}{}
				defer func() { <-tickets }()
				_, err := device.use(func() error {
					client, err := device.device.EvsClient()
					if err != nil {
						return err
					}
					errs[index] = network.evsCase.verifyDevice(ctx, &evsConcurrencyDevice{device: device.device, client: client})
					return nil
				})
				if err != nil {
					errs[index] = append(errs[index], err)
				}
			})
		}
		group.Wait()
		for _, deviceErrs := range errs {
			for _, err := range deviceErrs {
				t.Errorf("%s: %v", phase, err)
			}
		}
		t.Logf("%s: %s holds %d acknowledged commits to main's version %d, verified on %d devices", phase, network.evs.name, len(network.evs.commitList()), network.evs.headVersion(), len(devices))
	}
}

// The dns records so far against the root's journal (dnsCertsCheckRecords): every answered write once at its serial,
// every read a state the journal had, every written set served at its newest commit, and each zone's version its
// newest journal row, once the edge serves the root's serial.
func (self *churnLoadCase) checkDns(ctx context.Context, phase string) {
	sets := self.writtenDnsSets()
	if len(sets) == 0 {
		return
	}
	networks := []*dnsCertsNetwork{}
	for _, network := range self.dnsNetworks {
		for _, writer := range network.Writers {
			if 0 < len(writer.Writes()) {
				networks = append(networks, network)
				break
			}
		}
	}
	self.dnsRecords.Sets = sets
	rows := dnsCertsCheckRecords(self.t, ctx, self.world, self.settings.Dns, networks, self.dnsRecords)
	self.t.Logf("%s: %d dns sets written, the journal holding %d rows since the case began", phase, len(sets), len(rows))
}

// Every message so far in each mailbox it names exactly once, its sender's sent copy beside it, and no mailbox holding
// another's: each mail network's reader lists them all within the mail settings' MailboxTimeout.
func (self *churnLoadCase) checkMailboxes(ctx context.Context, phase string) {
	t, settings := self.t, self.settings
	if len(self.mailLedger.all()) == 0 {
		return
	}
	for _, network := range self.mailNetworks {
		entries, err := awaitMailMailbox(ctx, network, self.mailLedger, settings.Mail, settings.Mail.MailboxTimeout)
		if err != nil {
			t.Errorf("%s: %v", phase, err)
		}
		counts := mailCountMailbox(network, entries, self.mailLedger)
		for _, entry := range counts.stray {
			t.Errorf("%s: %s's reader lists %q in repo %s, none of its mailboxes", phase, network.Name, entry.Entry.GetSubject(), entry.RepoId)
		}
		for _, expected := range self.mailLedger.all() {
			for _, mailbox := range network.Mailboxes {
				if !slices.Contains(expected.Mailboxes, mailbox.Name) {
					if held := counts.received[expected.Token][mailbox.Name]; 0 < len(held) {
						t.Errorf("%s: %s holds %s %d times, which names it nowhere", phase, mailbox.Name, expected.Token, len(held))
					}
					continue
				}
				held := len(counts.received[expected.Token][mailbox.Name])
				if held <= 1 {
					continue
				}
				if reason := self.mailLedger.mayRepeat(expected, network.Domain); reason != "" {
					t.Logf("%s: %s holds %s %d times: %s", phase, mailbox.Name, expected.Token, held, reason)
					continue
				}
				t.Errorf("%s: %s holds %s %d times", phase, mailbox.Name, expected.Token, held)
			}
		}
	}
	t.Logf("%s: %d messages of the case so far", phase, len(self.mailLedger.all()))
}

// The blog's round: its commits settled within its settle bound (bloggerSettle), its visits failing only inside a chaos
// window (checkBloggerVisits), and in a round no chaos reached, every commit heard by every other writer's webserver
// within the bound (checkBloggerNotifications).
func (self *churnLoadCase) checkBlog(phase string, roundStartedAt time.Time) {
	t := self.t
	blogs := []*bloggerBlog{self.blog}
	bloggerSettle(t, self.world, blogs, roundStartedAt)
	self.stateLock.Lock()
	chaos := self.blogChaos
	self.stateLock.Unlock()
	checkBloggerVisits(t, self.blog, chaos, phase)
	if self.chaosSince(roundStartedAt.Add(-self.settings.Blog.TlsedgeReconnectTimeout)) {
		t.Logf("%s: chaos reached the round, whose lost hints a webserver may hear late, so its notifications are not held to the settle bound", phase)
		return
	}
	checkBloggerNotifications(t, self.blog, roundStartedAt)
}

// No device raised an alarm since the last check; one that left logs what it raised after.
func (self *churnLoadCase) checkAlarms(phase string) {
	t := self.t
	for _, network := range self.networks {
		for _, device := range network.deviceList() {
			device.stateLock.Lock()
			alarms := slices.Clone(device.alarms[min(device.alarmsChecked, len(device.alarms)):])
			device.alarmsChecked = len(device.alarms)
			device.stateLock.Unlock()
			for _, alarm := range alarms {
				if strings.HasSuffix(alarm, "(after it left)") {
					t.Logf("%s: %s raised %s", phase, device.device.Name, alarm)
					continue
				}
				t.Errorf("%s: %s raised %s", phase, device.device.Name, alarm)
			}
		}
	}
}

// Logs every 401 and 403 gkm answered since the last call, with the bearer's claims read unverified, the evidence a
// device's refused alarm or a step's failure is read with.
func (self *churnLoadCase) logRefusals(phase string) {
	for _, refusal := range self.origin.takeRefusals() {
		request := refusal.request
		token := "a bearer with no claims that parse"
		if request.claims != nil {
			token = fmt.Sprintf("a token from log position %d holding %q", request.claims.ProjVersion, request.claims.Scope)
		}
		self.t.Logf("%s: gkm answered %s %d at %s on %s from sidecar %s: %s", phase, request.route, refusal.status, request.time.Format("15:04:05.000"), token, request.sidecarId, refusal.body)
	}
}

// The end (the file's header): mail's sends ended and every copy checked, every live device's evs verification, the
// dns records, the certificates' rows, then the drains and every ledger.
func (self *churnLoadCase) finish() {
	t, settings := self.t, self.settings
	phase := "the end"
	ctx, cancel := self.world.StepContext(t.Context(), settings.FinalTimeout)
	defer cancel()

	// mail: every send's internet mail ended sent, the internet MX took what names it, every mailbox's copies
	mxByteCounts, smtpByteCounts := map[string]int64{}, map[string]int64{}
	if 0 < len(self.mailLedger.all()) {
		outcomes, err := awaitMailOutcomes(ctx, self.world, self.mailLedger, settings.Mail)
		if err != nil {
			t.Errorf("%s: the sends' outcomes: %v", phase, err)
		}
		mxMessages, err := awaitMailInternetMx(ctx, self.world, self.mailLedger, self.mailNetworks, settings.Mail, settings.Mail.MailboxTimeout)
		if err != nil {
			t.Errorf("%s: the internet MX: %v", phase, err)
		}
		for _, network := range self.mailNetworks {
			entries, err := awaitMailMailbox(ctx, network, self.mailLedger, settings.Mail, settings.Mail.MailboxTimeout)
			if err != nil {
				t.Errorf("%s: %s's mailboxes: %v", phase, network.Name, err)
			}
			mailCheckMailbox(ctx, t, self.world, network, self.mailLedger, entries, mxByteCounts, smtpByteCounts)
		}
		mailCheckInternetMx(ctx, t, self.world, self.mailLedger, self.world.dnssec.sender.Address(settings.Mail.InternetTo), mxMessages, smtpByteCounts)
		for _, network := range self.mailNetworks {
			mailCheckOutbox(ctx, t, network, self.mailLedger, outcomes)
		}
	}

	// evs on every live device, the dns records, and no certificate row left in flight
	self.checkEvs(ctx, phase, true)
	self.checkDns(ctx, phase)
	self.checkCertificates(ctx, phase)
	self.checkAlarms(phase)

	// the webservers stopped, then every metering block drained by a restart
	for _, writer := range self.blog.writers {
		writer.stop()
	}
	for _, name := range settings.FinalDrainBlocks {
		if err := self.world.Restart(name); err != nil {
			self.world.FailProgress(t, name+"'s restart for its drain", err, nil)
		}
	}
	self.checkLedgers(ctx, phase, mxByteCounts, smtpByteCounts)
	self.checkWriteBudgets(ctx, phase)
	controlPlaneCheckApiBudget(t, ctx, self.world, settings.Ledger)
	dnsCertsCheckCertsBudget(t, ctx, self.stack, self.publishers)
	dnsCertsCheckChallengeCounts(t, ctx, self.stack, settings.Dns, &dnsCertsRecords{Root: self.dnsRecords.Root})
	self.logRefusals(phase)
}

// No certificate row of a publisher's is left queued, ordering or waiting on dns: every order the case began ended.
func (self *churnLoadCase) checkCertificates(ctx context.Context, phase string) {
	t := self.t
	for _, publisher := range self.publishers {
		rows, err := dnsCertsSidecarCerts(ctx, publisher)
		if err != nil {
			t.Errorf("%s: %s's certificate rows: %v", phase, publisher.Device.Name, err)
			continue
		}
		for _, row := range rows {
			switch row.State {
			case sidecar.TlsCertStateQueued, sidecar.TlsCertStateOrdering, sidecar.TlsCertStateWaitingDns:
				t.Errorf("%s: %s's certificate row %s is left %s: %s", phase, publisher.Device.Name, row.CertId, row.State, dnsCertsDescribeRows(rows))
			}
		}
	}
}

// Every network's quota ledger once the blocks drained: no balance overdrawn, every byte drawn a debit on a meter,
// exactly; the stored bytes exactly what evs.nightly counts of the network's objects; the mail networks' mx and smtp
// meters at least the bytes their mail moved through mxedge and smtpedge, and the blog's tls meter at least what its
// visitors were sent (checkBloggerLedger).
func (self *churnLoadCase) checkLedgers(ctx context.Context, phase string, mxByteCounts map[string]int64, smtpByteCounts map[string]int64) {
	t := self.t
	ledgers, err := self.world.ReadLedgers(ctx, time.Time{})
	if err != nil {
		t.Errorf("%s: the ledgers: %v", phase, err)
		return
	}
	counts, err := self.worker.storedByteCounts(ctx)
	if err != nil {
		t.Errorf("%s: evs.nightly's count: %v", phase, err)
	}
	for _, network := range self.networks {
		ledger := ledgers.Networks[network.NetworkId]
		meters := ledgers.Meters
		if ledger == nil {
			t.Errorf("%s: %s has no quota ledger", phase, network.Name)
			continue
		}
		switch {
		case ledger.Spent || ledger.BalanceByteCount < 0:
			t.Errorf("%s: %s's balance: %+v", phase, network.Name, ledger)
		case meters.TotalByteCount(network.NetworkId) != ledger.DebitedByteCount():
			t.Errorf("%s: %s's balances were debited %d, its meters: %s", phase, network.Name, ledger.DebitedByteCount(), meters.Describe(network.NetworkId))
		}
		if counts != nil {
			counted := counts[sidecar.Id(network.NetworkId)]
			if ledger.StoredByteCount != counted {
				t.Errorf("%s: server's quota ledger stores %d B for %s, and evs.nightly counts %d B of its objects: %+d B the usage batches never reconciled", phase, ledger.StoredByteCount, network.Name, counted, ledger.StoredByteCount-counted)
			}
		}
		t.Logf("%s: %s's ledger: %d B stored, %d B debited; meters %s", phase, network.Name, ledger.StoredByteCount, ledger.DebitedByteCount(), meters.Describe(network.NetworkId))
	}
	for _, network := range self.mailNetworks {
		meters := ledgers.Meters
		switch {
		case meters.ByteCount(network.NetworkId, sidecar.QuotaMeterMx) < mxByteCounts[network.Name]:
			t.Errorf("%s: %s's mx meter holds %d, under the %d bytes of the mail it took through mxedge", phase, network.Name, meters.ByteCount(network.NetworkId, sidecar.QuotaMeterMx), mxByteCounts[network.Name])
		case meters.ByteCount(network.NetworkId, sidecar.QuotaMeterSmtp) < smtpByteCounts[network.Name]:
			t.Errorf("%s: %s's smtp meter holds %d, under the %d bytes of the mail it sent through smtpedge", phase, network.Name, meters.ByteCount(network.NetworkId, sidecar.QuotaMeterSmtp), smtpByteCounts[network.Name])
		}
	}
	checkBloggerLedger(t, self.blog, ledgers)
}

// The metadata cluster's write budget once the blocks drained (§11.13): every run of evs and gkm holds a process's
// report, since each spends on it, and no live process holds anything in flight or queued, read again a report
// interval apart while one does.
func (self *churnLoadCase) checkWriteBudgets(ctx context.Context, phase string) {
	t := self.t
	var busy []string
	var apiProcessId string
	for attempt := 0; attempt < 3; attempt += 1 {
		live, err := self.world.ReadLedgers(ctx, time.Now())
		if err != nil {
			t.Errorf("%s: the live budget reports: %v", phase, err)
			return
		}
		apiProcessId = live.ApiProcessId
		busy = nil
		for clusterId, reports := range live.BudgetReports {
			for _, report := range reports {
				if report.InFlightByteCount != 0 || report.P1QueuedByteCount != 0 {
					busy = append(busy, fmt.Sprintf("%s's process %s: %d B in flight, %d B queued", clusterId, report.ProcessId, report.InFlightByteCount, report.P1QueuedByteCount))
				}
			}
		}
		if len(busy) == 0 {
			break
		}
	}
	for _, line := range busy {
		t.Errorf("%s: the write budget holds %s after the case's work ended", phase, line)
	}
	self.sampler.stop()
	clusterReports, sampleErrs := self.sampler.reports()
	for _, err := range sampleErrs {
		t.Errorf("%s: the budget sampler: %v", phase, err)
	}
	standIns := map[string]string{
		apiProcessId:                       "api's",
		self.worker.processId():            "the evs.nightly stand-in's",
		self.reconciler.budget.ProcessId(): "the reconciler stand-in's",
	}
	blockRuns := self.world.Block(WorldBlockEvs).RunCount() + self.world.Block(WorldBlockGkm).RunCount()
	for clusterId, processReports := range clusterReports {
		lines := []string{}
		blockProcesses := 0
		for _, processId := range slices.Sorted(maps.Keys(processReports)) {
			report := processReports[processId]
			role, standIn := standIns[processId]
			if !standIn {
				role = "a block's"
				blockProcesses += 1
			}
			lines = append(lines, fmt.Sprintf("%s %s: %d B (p0 %v, p1 %d, p2 %d unfunded and %d funded)", role, processId, evsConcurrencyReportByteCount(report), report.P0ByteCounts, report.P1ByteCount, report.UnfundedByteCount, report.FundedByteCount))
		}
		t.Logf("%s: %s's write budget, each process's newest report: %s", phase, clusterId, strings.Join(lines, "; "))
		if blockProcesses < blockRuns {
			t.Errorf("%s: %s's write budget holds the reports of %d block processes, fewer than the %d runs of evs and gkm that spent on it: a run that lived under a report interval reported nothing before it stopped (finding write-budget-close-report)", phase, clusterId, blockProcesses, blockRuns)
		}
	}
	if len(clusterReports) == 0 {
		t.Errorf("%s: the budget sampler read no metadata cluster's reports", phase)
	}
}
