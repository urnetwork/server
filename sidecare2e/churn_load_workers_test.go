//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The churn load case's workers and chaos (churn_load_test.go's header): each kind's bursts, built over the other
// scenarios' harnesses, and the chaos restarts of hosts and devices.
//
// A step refused for now is tried again within RetryTimeout, each retry counted by its reason (churnLoadTransient):
// gkm's own waits and its unavailability, evs's or api's answers that refuse nothing, and a connection a restart cut.
// A step that fails otherwise is kept in its burst's errors and the burst goes on (gkmConcurrencyBurst), so one
// finding does not hide the next.

package sidecare2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/sidecart3"
)

// The kinds' names, as the counts and the bursts name them.
const (
	churnLoadKindEvsWriter     = "evs writer"
	churnLoadKindEvsReader     = "evs reader"
	churnLoadKindGkmMembership = "gkm membership"
	churnLoadKindGkmRotation   = "gkm rotation"
	churnLoadKindMailSender    = "mail sender"
	churnLoadKindMailReceiver  = "mail receiver"
	churnLoadKindBlogWriter    = "blog writer"
	churnLoadKindBlogReader    = "blog reader"
	churnLoadKindDnsWriter     = "dns writer"
	churnLoadKindDnsReader     = "dns reader"
	churnLoadKindAclSaver      = "acl saver"
	churnLoadKindEnrollment    = "enrollment"
	churnLoadKindDeparture     = "departure"
)

// One kind of worker: its share of a round, and a burst of it, nil where the kind has nothing to run on now.
type churnLoadKind struct {
	name     string
	settings *churnLoadKindSettings
	burst    func(round int, random *rand.Rand, picked map[any]bool) *WorldBurst
}

// Every kind, in a fixed order, so a seed repeats a round's draw.
func (self *churnLoadCase) kinds() []*churnLoadKind {
	mix := self.settings.Mix
	return []*churnLoadKind{
		{name: churnLoadKindEvsWriter, settings: mix.EvsWriters, burst: self.evsWriterBurst},
		{name: churnLoadKindEvsReader, settings: mix.EvsReaders, burst: self.evsReaderBurst},
		{name: churnLoadKindGkmMembership, settings: mix.GkmMembership, burst: self.gkmMembershipBurst},
		{name: churnLoadKindGkmRotation, settings: mix.GkmRotations, burst: self.gkmRotationBurst},
		{name: churnLoadKindMailSender, settings: mix.MailSenders, burst: self.mailSenderBurst},
		{name: churnLoadKindMailReceiver, settings: mix.MailReceivers, burst: self.mailReceiverBurst},
		{name: churnLoadKindBlogWriter, settings: mix.BlogWriters, burst: self.blogWriterBurst},
		{name: churnLoadKindBlogReader, settings: mix.BlogReaders, burst: self.blogReaderBurst},
		{name: churnLoadKindDnsWriter, settings: mix.DnsWriters, burst: self.dnsWriterBurst},
		{name: churnLoadKindDnsReader, settings: mix.DnsReaders, burst: self.dnsReaderBurst},
		{name: churnLoadKindAclSaver, settings: mix.AclSavers, burst: self.aclSaverBurst},
		{name: churnLoadKindEnrollment, settings: mix.Enrollments, burst: self.enrollmentBurst},
		{name: churnLoadKindDeparture, settings: mix.Departures, burst: self.departureBurst},
	}
}

// A round's bursts: each kind's count drawn from 0 to its MaxBurstCount, the last round running each kind no earlier
// round ran at least once; and each kind's count.
func (self *churnLoadCase) roundBursts(round int, random *rand.Rand) ([]*WorldBurst, map[string]int) {
	bursts := []*WorldBurst{}
	counts := map[string]int{}
	// what this round's bursts took for their own: one writer burst a device, one dns writer a writer
	picked := map[any]bool{}
	for _, kind := range self.kinds() {
		if kind.settings == nil || kind.settings.MaxBurstCount <= 0 || kind.settings.StepCount <= 0 {
			continue
		}
		count := random.IntN(kind.settings.MaxBurstCount + 1)
		self.stateLock.Lock()
		ranBefore := 0 < self.kindCounts[kind.name]
		self.stateLock.Unlock()
		if round == self.settings.RoundCount-1 && !ranBefore && count == 0 {
			count = 1
		}
		for range count {
			if burst := kind.burst(round, random, picked); burst != nil {
				bursts = append(bursts, burst)
				counts[kind.name] += 1
			}
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for name, count := range counts {
		self.kindCounts[name] += count
	}
	return bursts, counts
}

// Why err refuses a step for now only, "" for an error that refuses it: gkm's waits and unavailability and evs's
// answers that refuse nothing (gkmConcurrencyTransient), api's 429 or 5xx, and a connection a restart cut.
func churnLoadTransient(err error) string {
	if err == nil {
		return ""
	}
	if reason := gkmConcurrencyTransient(err); reason != "" {
		return reason
	}
	var apiErr *sidecar.SidecarApiError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusTooManyRequests || http.StatusInternalServerError <= apiErr.Status) {
		return fmt.Sprintf("api's %d", apiErr.Status)
	}
	if errors.Is(err, sidecar.ErrSocketUnavailable) {
		return "sockets unavailable"
	}
	return ""
}

// Runs step until it answers, tried again within RetryTimeout every RetryInterval while churnLoadTransient says it
// refused for now, each retry counted under what.
func (self *churnLoadCase) retry(ctx context.Context, what string, step func() error) error {
	deadline := time.Now().Add(self.settings.RetryTimeout)
	for {
		err := step()
		if err == nil {
			return nil
		}
		reason := churnLoadTransient(err)
		if reason == "" || ctx.Err() != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still refused for now past %s: %w", self.settings.RetryTimeout, err)
		}
		self.countRetry(what + ": " + reason)
		if err := evsConcurrencyPause(ctx, self.settings.RetryInterval); err != nil {
			return err
		}
	}
}

// One element of items at random, false for none.
func churnLoadPick[T any](random *rand.Rand, items []T) (T, bool) {
	var zero T
	if len(items) == 0 {
		return zero, false
	}
	return items[random.IntN(len(items))], true
}

// The networks with a shared evs repo: the personal ones and the team.
func (self *churnLoadCase) evsNetworks() []*churnLoadNetwork {
	return slices.Concat(self.personal, []*churnLoadNetwork{self.team})
}

// A device's own path in its network's shared repo.
func (self *churnLoadCase) evsPath(device *churnLoadDevice) string {
	return self.settings.EvsDirectory + "/" + device.device.Name + ".txt"
}

// An evs writer: one device of a network with a shared repo, none another writer burst of the round took, writing its
// own path StepCount times.
func (self *churnLoadCase) evsWriterBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network, _ := churnLoadPick(random, self.evsNetworks())
	free := slices.DeleteFunc(network.liveDevices(), func(device *churnLoadDevice) bool { return picked[device] })
	device, ok := churnLoadPick(random, free)
	if !ok {
		return nil
	}
	picked[device] = true
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.EvsWriters.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			_, err := device.use(func() error {
				return self.evsWrite(ctx, network, device)
			})
			return err
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("%s's evs writes in %s", device.device.Name, churnLoadPhase(round)), steps)
}

// device writes its path's next content once, as evsConcurrencyCase.write writes, kept as acknowledged once its
// writer is answered. A refusal for now is tried again; first the path's history is read for the content, so a write
// whose outcome a cut left unknown and that committed is acknowledged at what committed, never written twice.
func (self *churnLoadCase) evsWrite(ctx context.Context, network *churnLoadNetwork, device *churnLoadDevice) error {
	settings := self.settings
	device.stateLock.Lock()
	sequence := device.evsSequence
	device.evsSequence += 1
	device.stateLock.Unlock()
	repo := network.evs
	path := self.evsPath(device)
	content := evsConcurrencyContent(repo.name, path, sequence)
	name := device.device.Name
	deadline := time.Now().Add(settings.RetryTimeout)
	for {
		client, err := device.device.EvsClient()
		if err != nil {
			return fmt.Errorf("%s's evs client: %w", name, err)
		}
		result, err := client.Repo(repo.repoId).Write(ctx, &sidecar.EvsWriteArgs{Path: path, Content: bytes.NewReader(content), Size: int64(len(content))})
		if err == nil {
			repo.add(&evsConcurrencyCommit{
				path: path, writer: device.device, sequence: sequence, content: content,
				pushId: result.PushId, version: result.Version, pathVersion: result.PathVersion,
				run: self.world.Block(WorldBlockEvs).RunCount(),
			})
			return nil
		}
		reason := churnLoadTransient(err)
		if reason == "" || ctx.Err() != nil {
			return fmt.Errorf("%s's write %d of %s on %s: %w", name, sequence, path, repo.name, err)
		}
		commit, err := self.resolveEvsWrite(ctx, client, repo, device, path, sequence, content, deadline)
		if err != nil {
			return fmt.Errorf("%s's write %d of %s on %s, refused for now (%s), and its outcome unresolved: %w", name, sequence, path, repo.name, reason, err)
		}
		if commit != nil {
			repo.add(commit)
			self.countRetry("an evs write whose outcome its path's history resolved")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s's write %d of %s on %s, still refused for now past %s: %s", name, sequence, path, repo.name, settings.RetryTimeout, reason)
		}
		self.countRetry("evs write: " + reason)
		if err := evsConcurrencyPause(ctx, settings.RetryInterval); err != nil {
			return err
		}
	}
}

// Whether path's history on repo holds content past its acknowledged commits: the commit, as its writer would have
// been answered, nil for none. The history is read fresh and read again while evs refuses for now, until deadline.
func (self *churnLoadCase) resolveEvsWrite(ctx context.Context, client *sidecar.EvsClient, repo *evsConcurrencyRepo, device *churnLoadDevice, path string, sequence int, content []byte, deadline time.Time) (*evsConcurrencyCommit, error) {
	acknowledged := uint64(0)
	for _, commit := range repo.pathCommits()[path] {
		acknowledged = max(acknowledged, commit.pathVersion)
	}
	repoClient := client.Repo(repo.repoId)
	for {
		history, err := repoClient.History(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: true})
		switch {
		case errors.Is(err, sidecar.ErrEvsPathAbsent):
			return nil, nil
		case err == nil:
			for _, version := range history.Versions {
				if version.Version <= acknowledged {
					continue
				}
				var read bytes.Buffer
				if _, err := repoClient.ReadVersion(ctx, &sidecar.EvsReadArgs{Path: path, Version: version.Version}, &read); err != nil {
					return nil, fmt.Errorf("version %d's read: %w", version.Version, err)
				}
				if bytes.Equal(read.Bytes(), content) {
					return &evsConcurrencyCommit{
						path: path, writer: device.device, sequence: sequence, content: content,
						pushId: version.PushId, version: version.CommitVersion, pathVersion: version.Version,
						run: self.world.Block(WorldBlockEvs).RunCount(),
					}, nil
				}
			}
			return nil, nil
		case churnLoadTransient(err) == "" || ctx.Err() != nil || time.Now().After(deadline):
			return nil, err
		}
		if err := evsConcurrencyPause(ctx, self.settings.RetryInterval); err != nil {
			return nil, err
		}
	}
}

// An evs reader: a device reading other devices' paths of its network's shared repo StepCount times, each read one of
// that path's writes, never older than the burst read before (evsConcurrencyCase.read), a read refused for now tried
// again.
func (self *churnLoadCase) evsReaderBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network, _ := churnLoadPick(random, self.evsNetworks())
	reader, ok := churnLoadPick(random, network.liveDevices())
	if !ok {
		return nil
	}
	seen := map[string]uint64{}
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.EvsReaders.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			others := slices.DeleteFunc(network.deviceList(), func(device *churnLoadDevice) bool { return device == reader })
			target, ok := churnLoadPick(random, others)
			if !ok {
				return nil
			}
			target.stateLock.Lock()
			sequenceCount := target.evsSequence
			target.stateLock.Unlock()
			fresh := random.IntN(2) == 0
			_, err := reader.use(func() error {
				return self.retry(ctx, "evs read", func() error {
					client, err := reader.device.EvsClient()
					if err != nil {
						return err
					}
					return network.evsCase.read(ctx, &evsConcurrencyDevice{device: reader.device, client: client}, network.evs, self.evsPath(target), sequenceCount, fresh, seen)
				})
			})
			return err
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("%s's evs reads in %s", reader.device.Name, churnLoadPhase(round)), steps)
}

// The live devices of network as the syncs of a statement wait on them.
func (self *churnLoadCase) following(network *churnLoadNetwork) []*sidecart3.T3Device {
	devices := []*sidecart3.T3Device{}
	for _, device := range network.liveDevices() {
		devices = append(devices, device.device)
	}
	return devices
}

// A gkm membership worker: a personal network's owner granting its partial repo to a device that does not read it, or
// revoking the grant of one that does, as its own verified state says, StepCount times.
func (self *churnLoadCase) gkmMembershipBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network, ok := churnLoadPick(random, self.personal)
	if !ok {
		return nil
	}
	owner := network.owner()
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.GkmMembership.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			subjects := slices.DeleteFunc(network.liveDevices(), func(device *churnLoadDevice) bool {
				return device == owner || device == network.partialWriter
			})
			subject, ok := churnLoadPick(random, subjects)
			if !ok {
				return nil
			}
			_, err := owner.use(func() error {
				return self.retry(ctx, "a partial repo's grant", func() error {
					return self.churnMembership(ctx, network, owner, subject)
				})
			})
			return err
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("%s's partial repo membership in %s", network.Name, churnLoadPhase(round)), steps)
}

// owner grants network's partial repo to subject as a reader where its verified state has subject read none of it, and
// revokes the grant where it reads it, synced on every live device.
func (self *churnLoadCase) churnMembership(ctx context.Context, network *churnLoadNetwork, owner *churnLoadDevice, subject *churnLoadDevice) error {
	follower := owner.device.Follower()
	if follower == nil || follower.Client() == nil {
		return fmt.Errorf("%s follows no log", owner.device.Name)
	}
	if err := follower.Client().CatchUp(ctx); err != nil {
		return err
	}
	state := follower.Client().State()
	subjectId := subject.device.Sidecar().Identity().SidecarId
	reads := slices.Contains(state.RepoReaders(network.partial), subjectId)
	syncTimeout := self.settings.configConvergenceBound(true)
	if reads {
		_, err := gkmConcurrencyFile(ctx, self.world, network.WorldNetwork, owner.device, bridge.SignedType_SIGNED_REPO_REVOKE, &protocol.RepoRevoke{
			RepoId:  network.partial.Bytes(),
			Subject: subjectId.Bytes(),
		}, self.following(network), syncTimeout)
		return err
	}
	_, err := gkmConcurrencyFile(ctx, self.world, network.WorldNetwork, owner.device, bridge.SignedType_SIGNED_REPO_GRANT, &protocol.RepoGrant{
		RepoId:  network.partial.Bytes(),
		Subject: subjectId.Bytes(),
		Role:    protocol.RepoGrantRole_REPO_GRANT_ROLE_READER,
	}, self.following(network), syncTimeout)
	return err
}

// A gkm rotation worker: a writer of a network's shared repo, or of a personal network's partial repo (its owner or
// its writer for good), rotating it StepCount times; a rotation refused for now tried again.
func (self *churnLoadCase) gkmRotationBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network, _ := churnLoadPick(random, self.evsNetworks())
	repoId := network.evs.repoId
	repoName := network.evs.name
	rotators := network.liveDevices()
	if network.partial != (sidecar.Id{}) && random.IntN(2) == 0 {
		repoId = network.partial
		repoName = network.Name + "'s partial repo"
		rotators = []*churnLoadDevice{network.owner(), network.partialWriter}
	}
	rotator, ok := churnLoadPick(random, rotators)
	if !ok {
		return nil
	}
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.GkmRotations.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			_, err := rotator.use(func() error {
				return self.retry(ctx, "a rotation", func() error {
					_, err := rotator.device.Gkm.RotateRepo(ctx, repoId)
					if err != nil {
						return fmt.Errorf("%s's rotation of %s: %w", rotator.device.Name, repoName, err)
					}
					return nil
				})
			})
			return err
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("%s's rotations of %s in %s", rotator.device.Name, repoName, churnLoadPhase(round)), steps)
}

// The next token of the case's messages and saves.
func (self *churnLoadCase) nextToken() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.tokenCount += 1
	return self.tokenCount
}

// A mail sender: a sender of a mail network sending StepCount messages, each to one of its own network's mailboxes,
// one of every other mail network's, and every other one to the internet sender's inbox too (mailNetworkSend). A send
// is not tried again: one that failed may have deposited, and its token is spent.
func (self *churnLoadCase) mailSenderBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network, ok := churnLoadPick(random, self.mailNetworks)
	if !ok {
		return nil
	}
	senderIndex := random.IntN(len(network.Senders))
	device := network.Senders[senderIndex]
	settings := self.settings
	others := slices.DeleteFunc(slices.Clone(self.mailNetworks), func(other *mailNetwork) bool { return other == network })
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range settings.Mix.MailSenders.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			token := self.nextToken()
			own := network.AllAddresses()
			recipients := []string{own[random.IntN(len(own))]}
			for _, other := range others {
				addresses := other.AllAddresses()
				recipients = append(recipients, addresses[random.IntN(len(addresses))])
			}
			if token%2 == 0 {
				recipients = append(recipients, self.world.dnssec.sender.Address(settings.Mail.InternetTo))
			}
			return mailNetworkSend(ctx, self.world, settings.Mail, self.mailLedger, self.mailNetworks, network, senderIndex, fmt.Sprintf("churn-%s-%d", device.Name, token), recipients)
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("%s's sends in %s", device.Name, churnLoadPhase(round)), steps)
}

// A mail receiver: the internet sender delivering StepCount messages as one kind to a mail network, one address or
// every address of it, through the lb and mxedge's router, tried again while refused (mailInternetDeliver).
func (self *churnLoadCase) mailReceiverBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network, ok := churnLoadPick(random, self.mailNetworks)
	if !ok {
		return nil
	}
	kind, _ := churnLoadPick(random, worldSenderKinds())
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.MailReceivers.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			token := self.nextToken()
			all := network.AllAddresses()
			addresses := []string{all[random.IntN(len(all))]}
			if token%2 == 1 {
				addresses = all
			}
			return mailInternetDeliver(ctx, self.world, self.settings.Mail, self.mailLedger, kind, network, fmt.Sprintf("churn-internet-%s-%d", kind, token), addresses)
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("the internet sender's %s deliveries to %s in %s", kind, network.Name, churnLoadPhase(round)), steps)
}

// A blog writer: a writer of the blog updating one of its articles StepCount times, once its webserver serves, a write
// refused for now tried again.
func (self *churnLoadCase) blogWriterBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	free := slices.DeleteFunc(slices.Clone(self.blog.writers), func(writer *bloggerWriter) bool { return picked[writer] })
	writer, ok := churnLoadPick(random, free)
	if !ok {
		return nil
	}
	picked[writer] = true
	capture := self.world.T3().Net().Capture()
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.BlogWriters.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			if err := writer.awaitUp(ctx); err != nil {
				return err
			}
			article := writer.articles[random.IntN(len(writer.articles))]
			return self.retry(ctx, "a blog write", func() error {
				_, err := writer.write(ctx, article, capture.CanaryText(bloggerCanaryArticleText))
				return err
			})
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("%s's article updates in %s", writer.device.Name, churnLoadPhase(round)), steps)
}

// A blog reader: a reader of the blog loading StepCount articles through tlsedge (bloggerReader.burst), each read's
// visits tried again below HTTP within the blog's ReadRetryTimeout.
func (self *churnLoadCase) blogReaderBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	reader, ok := churnLoadPick(random, self.blog.readers)
	if !ok {
		return nil
	}
	return reader.burst(churnLoadPhase(round), self.blog.articles, self.settings.Mix.BlogReaders.StepCount, nil)
}

// A writer's own set of the case's.
func (self *churnLoadCase) dnsSet(writer *dnsCertsWriter) dnsCertsSetKey {
	return dnsCertsSetKey{Owner: writer.Network.OwnerName(fmt.Sprintf(self.settings.DnsHostFormat, writer.Index)), Type: dns.TypeTXT}
}

// A dns writer: a device of a personal network, none another dns writer of the round took, writing its own TXT set
// StepCount times, each a value no other write puts (dnsCertsWriter.Write, which rides an unknown outcome and an
// unavailable API itself).
func (self *churnLoadCase) dnsWriterBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network, ok := churnLoadPick(random, self.dnsNetworks)
	if !ok {
		return nil
	}
	free := slices.DeleteFunc(slices.Clone(network.Writers), func(writer *dnsCertsWriter) bool { return picked[writer] })
	writer, ok := churnLoadPick(random, free)
	if !ok {
		return nil
	}
	picked[writer] = true
	set := self.dnsSet(writer)
	settings := self.settings.Dns
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for step := range self.settings.Mix.DnsWriters.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			_, err := writer.Write(ctx, set, fmt.Sprintf("w%d-churn-r%d-%d", writer.Index, round, step), false, settings.CasTryCount, settings.Dns)
			return err
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("%s's dns writes in %s", writer.Device.Name, churnLoadPhase(round)), steps)
}

// Every set a dns writer of the case has written so far.
func (self *churnLoadCase) writtenDnsSets() []dnsCertsSetKey {
	sets := []dnsCertsSetKey{}
	for _, network := range self.dnsNetworks {
		for _, writer := range network.Writers {
			if 0 < len(writer.Writes()) {
				sets = append(sets, self.dnsSet(writer))
			}
		}
	}
	return sets
}

// A dns reader: StepCount reads of the sets written before the round, each between two reads of the root's SOA
// (dnsCertsReader.Read); none before any set was written.
func (self *churnLoadCase) dnsReaderBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	sets := self.writtenDnsSets()
	if len(sets) == 0 {
		return nil
	}
	reader := &dnsCertsReader{Name: fmt.Sprintf("dns reader %d of %s", len(self.dnsRecords.Readers()), churnLoadPhase(round))}
	self.dnsRecords.addReaders(reader)
	settings := self.settings.Dns
	return &WorldBurst{
		Name:      reader.Name,
		StepCount: self.settings.Mix.DnsReaders.StepCount,
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			return reader.Read(ctx, self.world, self.dnsRecords.Keys, sets, random, settings.Dns)
		},
	}
}

// An ACL saver: a config writer of a personal network or of the team saving its network's document StepCount times,
// each every grant the network's services need and a rule of its own, previewed and saved EXACT_HEAD, previewed again
// while another save moved the head, kept with its base's position once answered.
func (self *churnLoadCase) aclSaverBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network, _ := churnLoadPick(random, self.evsNetworks())
	network.stateLock.Lock()
	savers := slices.DeleteFunc(slices.Clone(network.savers), func(device *churnLoadDevice) bool { return device.gone() })
	network.stateLock.Unlock()
	saver, ok := churnLoadPick(random, savers)
	if !ok {
		return nil
	}
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.AclSavers.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			_, err := saver.use(func() error {
				return self.aclSave(ctx, round, network, saver)
			})
			return err
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("%s's ACL saves in %s", saver.device.Name, churnLoadPhase(round)), steps)
}

// saver saves network's document with a rule of its own, as `ur acl push` saves it: previewed, then saved EXACT_HEAD
// on what it previewed; previewed again where another save moved the head, after a 429's retry_after, and while evs
// or api answer for now; then synced on the network's live devices.
func (self *churnLoadCase) aclSave(ctx context.Context, round int, network *churnLoadNetwork, saver *churnLoadDevice) error {
	settings := self.settings
	self.stateLock.Lock()
	self.saveCount += 1
	index := self.saveCount
	self.stateLock.Unlock()
	source := network.document(append(slices.Clone(network.baseGrants), settings.AclChurnGrant(index))...)
	retry := &controlPlaneRetrySettings{Timeout: settings.RetryTimeout, Interval: settings.RetryInterval}
	deadline := time.Now().Add(settings.RetryTimeout)
	for {
		plan, err := controlPlanePlanAcl(ctx, saver.device, []byte(source), retry)
		var pin *bridge.AuthPin
		if err == nil {
			pin, err = saver.device.Sidecar().SaveAclPolicy(ctx, []byte(source), plan.Digest, sidecar.SidecarActConfirmed)
		}
		if err == nil {
			save := &churnLoadAclSave{saver: saver, index: index, source: source, position: pin.GetVersion(), round: round}
			network.stateLock.Lock()
			network.saves = append(network.saves, save)
			network.stateLock.Unlock()
			return network.Sync(ctx, pin.GetVersion())
		}
		var apiErr *sidecar.SidecarApiError
		switch {
		case ctx.Err() != nil:
			return err
		case time.Now().After(deadline):
			return fmt.Errorf("%s's save %d still refused past %s: %w", saver.device.Name, index, settings.RetryTimeout, err)
		case controlPlaneHeadMoved(err):
			self.countRetry("an ACL save whose head another save moved")
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests:
			self.countRetry("an ACL save past config_bases_per_min")
			if err := evsConcurrencyPause(ctx, time.Duration(apiErr.RetryAfterMs)*time.Millisecond); err != nil {
				return err
			}
			continue
		case churnLoadTransient(err) != "":
			self.countRetry("an ACL save: " + churnLoadTransient(err))
		default:
			return fmt.Errorf("%s's save %d of %s's document: %w", saver.device.Name, index, network.Name, err)
		}
		if err := evsConcurrencyPause(ctx, settings.RetryInterval); err != nil {
			return err
		}
	}
}

// An enrollment: the team's Owner inviting a new person, who accepts and whom an Owner's device admits as a member, or
// enrolling a new device of its own, admitted by its first device and following, StepCount times, each kept.
func (self *churnLoadCase) enrollmentBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network := self.team
	team := network.team
	admin := network.owner()
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.Enrollments.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			token := self.nextToken()
			if random.IntN(2) == 0 {
				_, err := admin.use(func() error {
					person, err := self.world.NewPerson(ctx, fmt.Sprintf("churn-member-%d", token))
					if err != nil {
						return err
					}
					invite, err := team.Invite(ctx, team.Owner, admin.device, person.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER)
					if err != nil {
						return fmt.Errorf("%s's invite: %w", person.Name, err)
					}
					member, err := team.Accept(ctx, person, invite)
					if err != nil {
						return fmt.Errorf("%s's accept: %w", person.Name, err)
					}
					if _, err := team.AdmitMember(ctx, admin.device, member); err != nil {
						return fmt.Errorf("%s's MemberAdmit: %w", person.Name, err)
					}
					network.stateLock.Lock()
					network.joined = append(network.joined, member)
					network.membershipTime = time.Now()
					network.stateLock.Unlock()
					return nil
				})
				return err
			}
			_, err := admin.use(func() error {
				devices, err := team.AddSidecarsWith(ctx, &sidecart3.T3EnrollArgs{Root: team.Owner.Root, MemberId: team.Owner.MemberId, Count: 1, Name: fmt.Sprintf("%s-churn-%d", team.Name, token)})
				if err != nil {
					return fmt.Errorf("the Owner's new device: %w", err)
				}
				if err := team.AdmitMemberDevices(ctx, team.Owner, nil, devices); err != nil {
					return fmt.Errorf("%s's admission: %w", devices[0].Name, err)
				}
				device := &churnLoadDevice{device: devices[0], network: network, restartable: true}
				device.watch()
				network.stateLock.Lock()
				network.devices = append(network.devices, device)
				network.enrolled = append(network.enrolled, device)
				network.savers = append(network.savers, device)
				network.membershipTime = time.Now()
				network.stateLock.Unlock()
				return nil
			})
			return err
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("the team's enrollments in %s", churnLoadPhase(round)), steps)
}

// A departure: the team's Owner removing a member the churn added (member-remove, then MemberRevoke), or revoking a
// device of its own the churn added, StepCount times, each kept; a step with neither to take away does nothing.
func (self *churnLoadCase) departureBurst(round int, random *rand.Rand, picked map[any]bool) *WorldBurst {
	network := self.team
	team := network.team
	admin := network.owner()
	steps := []func(ctx context.Context, random *rand.Rand) error{}
	for range self.settings.Mix.Departures.StepCount {
		steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
			network.stateLock.Lock()
			members := slices.DeleteFunc(slices.Clone(network.joined), func(member *WorldTeamMember) bool { return member.Removed() })
			devices := slices.DeleteFunc(slices.Clone(network.enrolled), func(device *churnLoadDevice) bool { return device.gone() })
			network.stateLock.Unlock()
			if 0 < len(members) && (len(devices) == 0 || random.IntN(2) == 0) {
				member, _ := churnLoadPick(random, members)
				_, err := admin.use(func() error {
					if _, err := team.RemoveMember(ctx, team.Owner, admin.device, member); err != nil {
						return fmt.Errorf("%s's removal: %w", member.Name, err)
					}
					network.stateLock.Lock()
					network.removed = append(network.removed, member)
					network.membershipTime = time.Now()
					network.stateLock.Unlock()
					return nil
				})
				return err
			}
			device, ok := churnLoadPick(random, devices)
			if !ok {
				return nil
			}
			// out of every worker's use before it goes
			device.useLock.Lock()
			device.stateLock.Lock()
			device.departed = true
			device.stateLock.Unlock()
			device.useLock.Unlock()
			_, err := admin.use(func() error {
				if _, err := team.RevokeDevice(ctx, admin.device, device.device, bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED); err != nil {
					return fmt.Errorf("%s's revocation: %w", device.device.Name, err)
				}
				network.stateLock.Lock()
				network.revoked = append(network.revoked, device)
				network.membershipTime = time.Now()
				network.stateLock.Unlock()
				return nil
			})
			return err
		})
	}
	return gkmConcurrencyBurst(fmt.Sprintf("the team's departures in %s", churnLoadPhase(round)), steps)
}

// The chaos of a round: ChaosDuringRoundCount restarts, each after a random pause up to ChaosMaxDelay.
func (self *churnLoadCase) chaosBurst(round int) *WorldBurst {
	settings := self.settings
	return &WorldBurst{
		Name:      fmt.Sprintf("the chaos of %s", churnLoadPhase(round)),
		StepCount: settings.ChaosDuringRoundCount,
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			if err := evsConcurrencyPause(ctx, time.Duration(random.Int64N(int64(settings.ChaosMaxDelay)+1))); err != nil {
				return err
			}
			target, _ := churnLoadPick(random, settings.ChaosTargets)
			return self.restart(ctx, round, target, true, random)
		},
	}
}

// The chaos between two rounds: ChaosBetweenRoundCount restarts, one after another.
func (self *churnLoadCase) chaosBetween(round int) {
	t, settings := self.t, self.settings
	random := rand.New(rand.NewPCG(settings.Seed, uint64(1000+round)))
	ctx, cancel := self.world.StepContext(t.Context(), settings.CheckTimeout)
	defer cancel()
	for range settings.ChaosBetweenRoundCount {
		target, _ := churnLoadPick(random, settings.ChaosTargets)
		if err := self.restart(ctx, round, target, false, random); err != nil {
			t.Errorf("the chaos before %s: %v", churnLoadPhase(round), err)
		}
	}
}

// Restarts target on its stores (the file's header of churn_load_test.go): api, the certs worker, a block by name, or
// one of the team's restartable devices, held from every use while it restarts. Kept as the case's chaos and as a
// window of the blog's visit checks.
func (self *churnLoadCase) restart(ctx context.Context, round int, target string, during bool, random *rand.Rand) error {
	chaos := &churnLoadChaos{round: round, target: target, during: during, startedAt: time.Now()}
	var err error
	switch target {
	case churnLoadChaosApi:
		err = self.world.T3().RestartApi()
	case churnLoadChaosCerts:
		err = self.stack.Worker.Restart()
	case churnLoadChaosDevice:
		candidates := slices.DeleteFunc(self.team.liveDevices(), func(device *churnLoadDevice) bool { return !device.restartable })
		device, ok := churnLoadPick(random, candidates)
		if !ok {
			chaos.target = "no device"
			break
		}
		chaos.target = "device " + device.device.Name
		device.useLock.Lock()
		if !device.gone() {
			err = device.device.Restart(ctx)
			device.watch()
		}
		device.useLock.Unlock()
	default:
		err = self.world.Restart(target)
	}
	chaos.endedAt = time.Now()
	chaos.err = err
	self.stateLock.Lock()
	self.chaos = append(self.chaos, chaos)
	blogChaos := self.blogChaos
	self.stateLock.Unlock()
	if blogChaos != nil {
		blogChaos.stateLock.Lock()
		blogChaos.events = append(blogChaos.events, &bloggerChaosEvent{name: chaos.target + " restart", grace: self.settings.Blog.TlsedgeReconnectTimeout, startedAt: chaos.startedAt, endedAt: chaos.endedAt})
		blogChaos.stateLock.Unlock()
	}
	self.t.Logf("chaos in %s: %s restarted in %s, %v", churnLoadPhase(round), chaos.target, chaos.endedAt.Sub(chaos.startedAt).Round(time.Millisecond), err)
	if err != nil {
		return fmt.Errorf("%s's restart: %w", chaos.target, err)
	}
	return nil
}
