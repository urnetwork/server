//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The control plane's quotas under concurrent use (the end-to-end plan's e2e-control-plane-concurrency, its quota and
// budget part past config_admissions, which the main case's limits phase takes; §20.4, §20.5, §20.7, §20.8):
// dns_entries, sidecar_clients and evs storage, each on a personal network of its own whose limit a network_sidecar_limit
// row of its own sets just above what the network holds (§20.5's override, as an ops change raises a limit or a §21
// case tightens one), then concurrent use past it, each reconciled exactly at its end. A limit is refused at the limit,
// not before it and not after it:
//
//   - dns_entries (§12.8's record write, step 6, counted under the network's lock): the limit set to the network's
//     user RRsets plus DnsHeadroom, then DnsHeadroom plus DnsOverflow new TXT sets written at once by the network's
//     devices through dns's api, one goroutine each: exactly the headroom lands and every other write is refused by
//     dns_entries at that limit, having found it full; the network's count is then the limit; one set deleted frees
//     one unit, which the next new set takes, and the one after it is refused again
//   - sidecar_clients (§20.7's connected row, §20.8): the limit set to ClientHeadroom on a network with no sidecar yet,
//     then ClientHeadroom plus ClientOverflow sidecars enrolled from their own auth keys at once, each connecting to the
//     exchange as it enrolls: server's connected set ({sq_<network_id>}connected_sidecar, one ZCOUNT) settles at
//     exactly the limit and is never seen past it, exactly that many sidecars are Online, and each other one is
//     refused, at its enrollment (QuotaRefusal sidecar_clients) or at its connection (4001, counted by the exchange's
//     refused nominations, its client then holding its dials); with the set full one more enrollment is refused by
//     sidecar_clients, and with the limit raised by one, one more sidecar enrolls and connects and the next is refused
//     at the new limit (a closed sidecar's slot frees only when its entry expires, ExchangeResidentTtl past its last
//     refresh, which the case does not wait out)
//   - storage (§20.7's limit row; evs's over-limit list, evsservice.QuotaGate): a network's stored count, once its usage
//     batches have all landed, equals what evs's nightly pass counts of its objects; the limit set one byte past it, a
//     write is accepted (the count is under the limit); once that write's batch lands, the count is at the limit, api's
//     over-limit list names the network with its storage refusal, and from then evs refuses every payload write within
//     one of its over-limit rereads (its interval plus one read's bound) while a read is still served; the limit raised
//     past the count, the list no longer names the network and a write is accepted again within the same bound; and at
//     the end the stored count equals the nightly pass's count again
//   - ledgers: api's write budget reconciled exactly with the objects api wrote (control_plane_concurrency_harness_test.go)
//
// The world runs no taskworker, so §20.7's nightly ReconcileSidecarCounters, which carries a limit's change to
// storage_over, runs here after each change, as does evs's nightly count, on a host of the case's own over the world's
// clusters (world_team_state_test.go). A producer reports its usage batch each minute with no hook to wait on, so the
// stored count is read until it moves, within StoredSettleTimeout. A failure from a product bug stays failing. Run
// through the build's wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecart3"
)

// What the quota case runs with (QD-702).
type controlPlaneQuotaSettings struct {
	// dns_entries: the network's devices, which write; the limit's room past what the network holds; and the writes
	// past it.
	DnsWriterCount int
	DnsHeadroom    int
	DnsOverflow    int
	// The rule the network's document holds beside grant 1: its members admin on every host the writes name.
	DnsGrant string
	// Each write's host label.
	DnsHostLabel func(index int) string
	// sidecar_clients: the limit, and the sidecars past it.
	ClientHeadroom int
	ClientOverflow int
	// How long the connections may take to settle, and how often the connected set and the sidecars are read
	// meanwhile: a connection's registration has no hook a test reads.
	ClientSettleTimeout  time.Duration
	ClientSampleInterval time.Duration
	// storage: each write's payload, random so nothing compresses it.
	StoragePayloadByteCount int
	// How far past the stored count the limit is raised at the end.
	StorageRaiseByteCount int64
	// How long the stored count may take to move, each producer reporting its usage batch every minute
	// (sidecar.QuotaUsageBatcherSettings.WindowTimeout), and how often it is read meanwhile.
	StoredSettleTimeout time.Duration
	StoredPollInterval  time.Duration
	// evs's bound on following api's over-limit list: one reread interval plus one read's bound
	// (evsservice.QuotaGateSettings), past which every write is judged by the list as it stands; and how often a write
	// is tried meanwhile.
	StorageGateBound   time.Duration
	StorageTryInterval time.Duration
	// The networks one page of the nightly reconcile reads.
	ReconcilePageCount int
	// Each part's bound: storage's waits on three usage batches.
	PhaseTimeout        time.Duration
	StoragePhaseTimeout time.Duration
	// The base case's settings: its world's limits, grant 1, ledgers.
	Base *controlPlaneSettings
}

func defaultControlPlaneQuotaSettings() *controlPlaneQuotaSettings {
	gate := evsservice.DefaultQuotaGateSettings()
	return &controlPlaneQuotaSettings{
		DnsWriterCount: 3,
		DnsHeadroom:    3,
		DnsOverflow:    3,
		DnsGrant:       `{"src": ["autogroup:member"], "dst": ["dns:quota-*"], "allow": ["admin"]}`,
		DnsHostLabel: func(index int) string {
			return fmt.Sprintf("quota-%d", index)
		},
		ClientHeadroom:          3,
		ClientOverflow:          3,
		ClientSettleTimeout:     90 * time.Second,
		ClientSampleInterval:    20 * time.Millisecond,
		StoragePayloadByteCount: 32 * 1024,
		StorageRaiseByteCount:   1024 * 1024 * 1024,
		StoredSettleTimeout:     150 * time.Second,
		StoredPollInterval:      time.Second,
		StorageGateBound:        gate.OverLimitInterval + gate.OverLimitTimeout,
		StorageTryInterval:      500 * time.Millisecond,
		ReconcilePageCount:      500,
		PhaseTimeout:            4 * time.Minute,
		StoragePhaseTimeout:     7 * time.Minute,
		Base:                    defaultControlPlaneSettings(),
	}
}

// One run of the quota case.
type controlPlaneQuota struct {
	*controlPlaneCase
	quota *controlPlaneQuotaSettings
}

func TestE2eControlPlaneConcurrencyQuota(t *testing.T) {
	settings := defaultControlPlaneQuotaSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		base := newControlPlaneCase(t, settings.Base)
		defer base.world.Close()
		self := &controlPlaneQuota{controlPlaneCase: base, quota: settings}
		self.dnsEntries()
		self.sidecarClients()
		self.storage()
		ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
		defer cancel()
		controlPlaneCheckApiBudget(t, ctx, self.world, settings.Base.Ledgers)
	})
}

// The dns_entries part (the file's header).
func (self *controlPlaneQuota) dnsEntries() {
	t := self.t
	settings := self.quota
	world := self.world
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	network, err := world.AddNetwork(ctx, "d", settings.DnsWriterCount, nil)
	if err != nil {
		world.FailProgress(t, "dns_entries' network", err, nil)
	}
	if _, err := world.SaveAclPolicy(ctx, network, network.Owner(), worldAclPolicy(settings.Base.GrantOne, settings.DnsGrant)); err != nil {
		world.FailProgress(t, "dns_entries' grant", err, nil)
	}
	dnsClient, err := network.Owner().Sidecar().Dns()
	if err != nil {
		t.Fatalf("%s's dns client: %v", network.Owner().Name, err)
	}
	zone, err := dnsClient.Zone(ctx)
	if err != nil || zone.Apex() == "" {
		t.Fatalf("%s's zone: %+v, %v", network.Name, zone, err)
	}
	host := func(index int) string {
		return settings.DnsHostLabel(index) + "." + zone.Apex()
	}
	used, err := worldDnsEntryCount(ctx, network.NetworkId)
	if err != nil {
		t.Fatal(err)
	}
	limit := used + int64(settings.DnsHeadroom)
	pop, err := worldSetNetworkLimit(ctx, network.NetworkId, model.DnsEntriesLimitKey, limit)
	if err != nil {
		t.Fatal(err)
	}
	defer pop()

	// every write at once, one goroutine each, the network's devices taking them in turn
	devices := network.LiveDevices()
	writeCount := settings.DnsHeadroom + settings.DnsOverflow
	errs := make([]error, writeCount)
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range writeCount {
		device := devices[index%len(devices)]
		group.Go(func() {
			<-start
			_, errs[index] = world.SetDnsRecord(ctx, device, &sidecar.DnsRecordUpsert{Name: host(index), Type: "TXT", Values: []string{host(index)}}, nil)
		})
	}
	close(start)
	group.Wait()
	landed := []int{}
	for index, err := range errs {
		if err == nil {
			landed = append(landed, index)
			continue
		}
		if !controlPlaneDnsRefusedFull(err, limit) {
			t.Errorf("the write of %s was refused other than by dns_entries at %d, full: %v", host(index), limit, err)
		}
	}
	if len(landed) != settings.DnsHeadroom {
		t.Errorf("dns_entries %d with %d held: %d of %d writes at once landed, not %d", limit, used, len(landed), writeCount, settings.DnsHeadroom)
	}
	if count, err := worldDnsEntryCount(ctx, network.NetworkId); err != nil || count != limit {
		t.Errorf("%s holds %d dns entries after the writes, not its dns_entries %d (%v)", network.Name, count, limit, err)
	}
	t.Logf("dns_entries %d with %d held: %d writes at once, %d landed", limit, used, writeCount, len(landed))
	if len(landed) == 0 {
		return
	}

	// the unit one deletion frees is taken by the next new set, and the one after it is refused
	if _, err := controlPlaneDeleteDnsRecord(ctx, network.Owner(), &sidecar.DnsRecordDelete{Name: host(landed[0]), Type: "TXT"}, nil); err != nil {
		t.Errorf("the deletion of %s: %v", host(landed[0]), err)
		return
	}
	if count, err := worldDnsEntryCount(ctx, network.NetworkId); err != nil || count != limit-1 {
		t.Errorf("%s holds %d dns entries after a deletion at its limit %d (%v)", network.Name, count, limit, err)
	}
	if _, err := world.SetDnsRecord(ctx, network.Owner(), &sidecar.DnsRecordUpsert{Name: host(writeCount), Type: "TXT", Values: []string{host(writeCount)}}, nil); err != nil {
		t.Errorf("the write of %s into the unit a deletion freed was refused: %v", host(writeCount), err)
	}
	if _, err := world.SetDnsRecord(ctx, network.Owner(), &sidecar.DnsRecordUpsert{Name: host(writeCount + 1), Type: "TXT", Values: []string{host(writeCount + 1)}}, nil); !controlPlaneDnsRefusedFull(err, limit) {
		t.Errorf("the write of %s past the freed unit answered %v, not dns_entries at %d, full", host(writeCount+1), err, limit)
	}
	if count, err := worldDnsEntryCount(ctx, network.NetworkId); err != nil || count != limit {
		t.Errorf("%s holds %d dns entries at the end, not its dns_entries %d (%v)", network.Name, count, limit, err)
	}
}

// Whether err refuses a record write by dns_entries at limit, the count it found being the limit: full.
func controlPlaneDnsRefusedFull(err error, limit int64) bool {
	var apiErr *sidecar.DnsApiError
	if !errors.As(err, &apiErr) || apiErr.Code != sidecar.DnsApiCodeQuota || apiErr.Refusal == nil {
		return false
	}
	refusal := apiErr.Refusal
	return refusal.LimitKey == model.DnsEntriesLimitKey && refusal.Limit == limit && refusal.Used == limit
}

// device's dns client deletes recordDelete through dns's api, trying again while the API answers unavailable, as
// World.SetDnsRecord writes.
func controlPlaneDeleteDnsRecord(ctx context.Context, device *sidecart3.T3Device, recordDelete *sidecar.DnsRecordDelete, settings *WorldDnsSettings) (*sidecar.DnsRecordsAnswer, error) {
	if settings == nil {
		settings = DefaultWorldDnsSettings()
	}
	client, err := device.Sidecar().Dns()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		answer, err := client.DeleteRecord(ctx, recordDelete, nil)
		var apiErr *sidecar.DnsApiError
		if err == nil || !errors.As(err, &apiErr) || apiErr.Code != sidecar.DnsApiCodeUnavailable {
			return answer, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the dns API stayed unavailable to %s: %w (%w)", device.Name, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// The sidecar_clients part (the file's header).
func (self *controlPlaneQuota) sidecarClients() {
	t := self.t
	settings := self.quota
	world := self.world
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	person, err := world.NewPerson(ctx, "clients")
	if err != nil {
		t.Fatal(err)
	}
	network, err := world.T3().AddNetworkOf(ctx, &sidecart3.T3NetworkOf{
		Name:          "c",
		NetworkId:     person.PersonalNetworkId,
		OwnerUserId:   person.UserId,
		OwnerMemberId: sidecar.Id(person.UserId),
		Root:          person.PersonalRoot,
	})
	if err != nil {
		t.Fatalf("sidecar_clients' network: %v", err)
	}
	limit := int64(settings.ClientHeadroom)
	pop, err := worldSetNetworkLimit(ctx, network.NetworkId, "sidecar_clients", limit)
	if err != nil {
		t.Fatal(err)
	}
	defer pop()
	clients := &controlPlaneClients{world: world, root: person.PersonalRoot}
	defer clients.close()
	refusedBefore, err := controlPlaneNominationRefusals()
	if err != nil {
		t.Fatalf("the exchange's refused nominations: %v", err)
	}

	// the connected set sampled for the burst's life: never past the limit
	sampleCtx, stopSampling := context.WithCancel(ctx)
	var sampling sync.WaitGroup
	var sampleLock sync.Mutex
	maxConnected := int64(0)
	var sampleErr error
	sampling.Go(func() {
		for sampleCtx.Err() == nil {
			connected, err := worldSidecarConnectedCount(sampleCtx, network.NetworkId)
			func() {
				sampleLock.Lock()
				defer sampleLock.Unlock()
				if err != nil && sampleCtx.Err() == nil && sampleErr == nil {
					sampleErr = err
				}
				maxConnected = max(maxConnected, connected)
			}()
			select {
			case <-sampleCtx.Done():
			case <-time.After(settings.ClientSampleInterval):
			}
		}
	})
	stopSampler := func() (int64, error) {
		stopSampling()
		sampling.Wait()
		sampleLock.Lock()
		defer sampleLock.Unlock()
		return maxConnected, sampleErr
	}

	// every sidecar started, then all enrolling at once
	count := settings.ClientHeadroom + settings.ClientOverflow
	started := make([]*sidecart3.T3Sidecar, count)
	startErrs := make([]error, count)
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			started[index], startErrs[index] = clients.start(ctx, fmt.Sprintf("c-%d", index))
		})
	}
	group.Wait()
	for index, err := range startErrs {
		if err != nil {
			stopSampler()
			t.Fatalf("c-%d's start: %v", index, err)
		}
	}
	enrollErrs := make([]error, count)
	start := make(chan struct{})
	for index, running := range started {
		group.Go(func() {
			<-start
			enrollErrs[index] = clients.enroll(ctx, running)
		})
	}
	close(start)
	group.Wait()
	enrolled := []*sidecart3.T3Sidecar{}
	enrollRefused := 0
	for index, err := range enrollErrs {
		switch {
		case err == nil:
			enrolled = append(enrolled, started[index])
		case controlPlaneQuotaRefusalKey(err) == "sidecar_clients":
			enrollRefused += 1
		default:
			t.Errorf("c-%d's enrollment failed other than by sidecar_clients: %v", index, err)
		}
	}

	// settled: the set at the limit, the limit's sidecars Online, and each enrolled one past it refused at the exchange
	refusedWant := max(0, len(enrolled)-int(limit))
	online, refused := 0, float64(0)
	settleErr := controlPlaneAwait(ctx, settings.ClientSettleTimeout, settings.ClientSampleInterval, func() (bool, error) {
		connected, err := worldSidecarConnectedCount(ctx, network.NetworkId)
		if err != nil {
			return false, err
		}
		total, err := controlPlaneNominationRefusals()
		if err != nil {
			return false, err
		}
		refused = total - refusedBefore
		online = controlPlaneOnlineCount(enrolled)
		return connected == limit && online == int(limit) && float64(refusedWant) <= refused, nil
	})
	seen, err := stopSampler()
	if err != nil {
		t.Errorf("the connected set's samples: %v", err)
	}
	connected, err := worldSidecarConnectedCount(ctx, network.NetworkId)
	if err != nil {
		t.Errorf("the connected set: %v", err)
	}
	t.Logf("sidecar_clients %d: %d sidecars at once, %d enrolled and %d refused at enrollment, %d Online, %d connected at the end and at most %d seen, %.0f nominations refused", limit, count, len(enrolled), enrollRefused, online, connected, seen, refused)
	if settleErr != nil {
		t.Errorf("sidecar_clients %d: the connections never settled at the limit (%d connected, %d of %d enrolled Online, %.0f of the %d past it refused at the exchange): %v", limit, connected, online, len(enrolled), refused, refusedWant, settleErr)
	}
	if limit < seen || limit < connected {
		t.Errorf("sidecar_clients %d: server counted %d sidecars connected at once (%d at the end) of %d connecting at once, past the limit", limit, seen, connected, count)
	}
	if settleErr != nil {
		return
	}

	// the set full: one more enrollment is refused by sidecar_clients
	extra, err := clients.start(ctx, "c-extra")
	if err != nil {
		t.Fatalf("c-extra's start: %v", err)
	}
	if err := clients.enroll(ctx, extra); !controlPlaneClientsRefusedFull(err, limit) {
		t.Errorf("an enrollment with sidecar_clients %d full answered %v, not sidecar_clients at %d, full", limit, err, limit)
	}

	// the limit raised by one: one more sidecar enrolls and connects, and the next is refused at the new limit. A
	// closed sidecar's slot frees only when its entry expires, ExchangeResidentTtl past its last refresh (§20.7's
	// expiry score), which the case does not wait out.
	raised := limit + 1
	if _, err := worldSetNetworkLimit(ctx, network.NetworkId, "sidecar_clients", raised); err != nil {
		t.Fatal(err)
	}
	next, err := clients.start(ctx, "c-next")
	if err != nil {
		t.Fatalf("c-next's start: %v", err)
	}
	if err := clients.enroll(ctx, next); err != nil {
		t.Errorf("an enrollment under sidecar_clients raised to %d: %v", raised, err)
		return
	}
	if err := next.AwaitState(ctx, sidecar.SidecarStateOnline); err != nil {
		t.Errorf("the sidecar enrolled under sidecar_clients raised to %d never came Online: %v", raised, err)
	}
	if err := controlPlaneAwait(ctx, settings.ClientSettleTimeout, settings.ClientSampleInterval, func() (bool, error) {
		connected, err := worldSidecarConnectedCount(ctx, network.NetworkId)
		return connected == raised, err
	}); err != nil {
		t.Errorf("sidecar_clients raised to %d: the new sidecar was never counted connected: %v", raised, err)
	}
	last, err := clients.start(ctx, "c-last")
	if err != nil {
		t.Fatalf("c-last's start: %v", err)
	}
	if err := clients.enroll(ctx, last); !controlPlaneClientsRefusedFull(err, raised) {
		t.Errorf("an enrollment past sidecar_clients raised to %d answered %v, not sidecar_clients at %d, full", raised, err, raised)
	}
	if connected, err := worldSidecarConnectedCount(ctx, network.NetworkId); err != nil || connected != raised {
		t.Errorf("sidecar_clients raised to %d: server counts %d connected at the end (%v)", raised, connected, err)
	}
}

// Whether err refuses an enrollment by sidecar_clients at limit, the set it found holding the limit: full.
func controlPlaneClientsRefusedFull(err error, limit int64) bool {
	var apiErr *sidecar.SidecarApiError
	if !errors.As(err, &apiErr) || apiErr.QuotaRefusal == nil {
		return false
	}
	refusal := apiErr.QuotaRefusal
	return refusal.LimitKey == "sidecar_clients" && refusal.Limit == limit && refusal.Used == limit
}

// The bare sidecars the sidecar_clients part enrolls, each from an auth key a person's credential makes, as a device
// a person set up with that key: no T3 device, since a sidecar refused at its connection never comes Online, which
// T3's enrollment waits for. Safe for concurrent use.
type controlPlaneClients struct {
	world *World
	root  string

	stateLock sync.Mutex
	started   []*sidecart3.T3Sidecar
}

// A sidecar named name started on the world's api and exchange, Unenrolled.
func (self *controlPlaneClients) start(ctx context.Context, name string) (*sidecart3.T3Sidecar, error) {
	world := self.world.T3()
	running, err := sidecart3.StartT3Sidecar(world.Context(), world.Net(), name, world.Api(), world.Exchange(), nil)
	if err != nil {
		return nil, err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.started = append(self.started, running)
	}()
	if err := running.AwaitState(ctx, sidecar.SidecarStateUnenrolled); err != nil {
		return nil, err
	}
	return running, nil
}

// running enrolled from a one-use auth key of its own: Enroll's answer. It connects to the exchange as it enrolls.
func (self *controlPlaneClients) enroll(ctx context.Context, running *sidecart3.T3Sidecar) error {
	key, err := controlPlaneAuthKey(ctx, self.world, self.root, 1, nil, false)
	if err != nil {
		return err
	}
	return running.Sidecar().Enroll(ctx, &sidecar.SidecarEnrollCredential{AuthKey: key})
}

// Closes every sidecar started.
func (self *controlPlaneClients) close() {
	self.stateLock.Lock()
	started := self.started
	self.started = nil
	self.stateLock.Unlock()
	for _, running := range started {
		running.Close()
	}
}

// How many of sidecars are Online now.
func controlPlaneOnlineCount(sidecars []*sidecart3.T3Sidecar) int {
	online := 0
	for _, running := range sidecars {
		if running.Sidecar().Status().State == sidecar.SidecarStateOnline {
			online += 1
		}
	}
	return online
}

// The exchange's refused nominations for sidecar_clients (server/connect's urnetwork_connect_nominations_refused_total,
// cause sidecar_client_limit), process-wide, from the default registry: each a connection closed with 4001 for its
// network's limit. One world runs in a process at a time.
func controlPlaneNominationRefusals() (float64, error) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0, err
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_connect_nominations_refused_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "cause" && label.GetValue() == "sidecar_client_limit" {
					return metric.GetCounter().GetValue(), nil
				}
			}
		}
	}
	return 0, nil
}

// Asks done every interval until it answers true, within timeout: for state with no hook a test reads. done's error
// ends the wait.
func controlPlaneAwait(ctx context.Context, timeout time.Duration, interval time.Duration, done func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		ok, err := done()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// The storage part (the file's header).
func (self *controlPlaneQuota) storage() {
	t := self.t
	settings := self.quota
	world := self.world
	ctx, cancel := world.StepContext(t.Context(), settings.StoragePhaseTimeout)
	defer cancel()
	network, err := world.AddNetwork(ctx, "s", 1, nil)
	if err != nil {
		world.FailProgress(t, "storage's network", err, nil)
	}
	device := network.Owner()
	repo, err := network.CreateRepo(ctx, device, protocol.RepoKind_REPO_KIND_GENERAL)
	if err != nil {
		world.FailProgress(t, "storage's repo", err, nil)
	}
	repoId := repo.RepoId()
	nightly, err := newWorldNightlyCount(world)
	if err != nil {
		t.Fatal(err)
	}
	defer nightly.close()
	written := 0
	write := func() ([]byte, error) {
		content := make([]byte, settings.StoragePayloadByteCount)
		if _, err := rand.Read(content); err != nil {
			return nil, err
		}
		path := fmt.Sprintf("fill-%d", written)
		written += 1
		return content, worldKeysWrite(ctx, device, repoId, path, content)
	}
	first, err := write()
	if err != nil {
		world.FailProgress(t, "storage's first write", err, nil)
	}

	// the stored count once every usage batch has landed: what evs's nightly pass counts
	stored, err := self.awaitStoredReconciled(ctx, network, nightly)
	if err != nil {
		t.Errorf("storage: %v", err)
		return
	}

	// the limit one byte past the count: under it, a write is accepted
	limit := stored + 1
	pop, err := worldSetNetworkLimit(ctx, network.NetworkId, "storage", limit)
	if err != nil {
		t.Fatal(err)
	}
	defer pop()
	if err := worldReconcileSidecarCounters(ctx, settings.ReconcilePageCount); err != nil {
		t.Fatal(err)
	}
	if refusal, err := worldStorageOverLimit(ctx, network.NetworkId); err != nil || refusal != nil {
		t.Errorf("api's over-limit list names %s at its count %d under its storage limit %d: %+v (%v)", network.Name, stored, limit, refusal, err)
	}
	if _, err := write(); err != nil {
		t.Errorf("a write under the storage limit (count %d, limit %d) was refused: %v", stored, limit, err)
		return
	}

	// that write's batch takes the count to the limit: api lists the network, and evs refuses payload within its bound
	listedAt, refusal, err := self.awaitStorageListed(ctx, network, true)
	if err != nil {
		t.Errorf("storage %d: api's over-limit list never named %s once its count reached the limit: %v", limit, network.Name, err)
		return
	}
	if refusal.LimitKey != "storage" || refusal.Limit != limit || refusal.Used < limit {
		t.Errorf("api's over-limit list names %s with %+v, not storage at %d with its count", network.Name, refusal, limit)
	}
	accepted, refusedErr := self.awaitWriteJudged(ctx, write, listedAt, false)
	if refusedErr != nil {
		t.Errorf("storage %d at its limit: %v", limit, refusedErr)
	} else {
		t.Logf("storage %d at its limit (count %d): %d writes accepted inside evs's bound of %s, then refused", limit, refusal.Used, accepted, settings.StorageGateBound)
	}
	if err := worldKeysRead(ctx, device, repoId, "fill-0", first); err != nil {
		t.Errorf("a read at the storage limit: %v", err)
	}

	// the limit raised past the count: the list drops the network, and a write is accepted within the bound
	now, err := worldNetworkStoredByteCount(ctx, network.NetworkId)
	if err != nil {
		t.Fatal(err)
	}
	raised := now + settings.StorageRaiseByteCount
	if _, err := worldSetNetworkLimit(ctx, network.NetworkId, "storage", raised); err != nil {
		t.Fatal(err)
	}
	if err := worldReconcileSidecarCounters(ctx, settings.ReconcilePageCount); err != nil {
		t.Fatal(err)
	}
	droppedAt, _, err := self.awaitStorageListed(ctx, network, false)
	if err != nil {
		t.Errorf("storage raised to %d past the count %d: api's over-limit list still names %s: %v", raised, now, network.Name, err)
		return
	}
	refusedCount, acceptedErr := self.awaitWriteJudged(ctx, write, droppedAt, true)
	if acceptedErr != nil {
		t.Errorf("storage raised to %d past the count %d: %v", raised, now, acceptedErr)
	} else {
		t.Logf("storage raised to %d past the count %d: %d writes refused inside evs's bound, then accepted", raised, now, refusedCount)
	}

	// the stored count at the end, exactly what the nightly pass counts
	if final, err := self.awaitStoredReconciled(ctx, network, nightly); err != nil {
		t.Errorf("storage at the end: %v", err)
	} else {
		t.Logf("storage: %d bytes stored at the end, as the nightly pass counts them", final)
	}
}

// Waits until network's stored count, as network_quota_state holds it, equals what evs's nightly pass counts of its
// objects and is above zero: every usage batch of its writes has landed. The count; an error naming both counts when
// they never meet within StoredSettleTimeout, the usage batches never reconciling the objects.
func (self *controlPlaneQuota) awaitStoredReconciled(ctx context.Context, network *WorldNetwork, nightly *worldNightlyCount) (int64, error) {
	settings := self.quota
	stored, counted := int64(0), int64(0)
	err := controlPlaneAwait(ctx, settings.StoredSettleTimeout, settings.StoredPollInterval, func() (bool, error) {
		var err error
		if stored, err = worldNetworkStoredByteCount(ctx, network.NetworkId); err != nil {
			return false, err
		}
		if counted, err = nightly.storedByteCount(ctx, network.NetworkId); err != nil {
			return false, err
		}
		return 0 < stored && stored == counted, nil
	})
	if err != nil {
		return 0, fmt.Errorf("%s's stored count %d never met the %d B evs's nightly pass counts of its objects: %w", network.Name, stored, counted, err)
	}
	return stored, nil
}

// Waits until api's over-limit list names network with a storage refusal (listed) or no longer names it (!listed),
// within StoredSettleTimeout. When the list was first read so, and the refusal it then held.
func (self *controlPlaneQuota) awaitStorageListed(ctx context.Context, network *WorldNetwork, listed bool) (time.Time, *sidecar.QuotaRefusal, error) {
	settings := self.quota
	var refusal *sidecar.QuotaRefusal
	var at time.Time
	err := controlPlaneAwait(ctx, settings.StoredSettleTimeout, settings.StoredPollInterval, func() (bool, error) {
		var err error
		if refusal, err = worldStorageOverLimit(ctx, network.NetworkId); err != nil {
			return false, err
		}
		at = time.Now()
		return (refusal != nil) == listed, nil
	})
	return at, refusal, err
}

// Tries write every StorageTryInterval from since until evs judges one as api's list now stands: refused by storage
// (accept false) or accepted (accept true). Within StorageGateBound of since, evs may still judge by the list before,
// its reread not yet in; past it, a write judged that way is an error. How many writes evs judged the old way first.
func (self *controlPlaneQuota) awaitWriteJudged(ctx context.Context, write func() ([]byte, error), since time.Time, accept bool) (int, error) {
	settings := self.quota
	deadline := since.Add(settings.StorageGateBound)
	stale := 0
	for {
		_, err := write()
		refused := err != nil
		if refused {
			evsError, ok := sidecar.EvsErrorOf(err)
			if !ok || evsError.Quota == nil || evsError.Quota.GetLimitKey() != "storage" {
				return stale, fmt.Errorf("a write failed other than by evs's storage refusal: %w", err)
			}
		}
		if refused != accept {
			return stale, nil
		}
		stale += 1
		if time.Now().After(deadline) {
			judged := "accepted"
			if refused {
				judged = "refused"
			}
			return stale, fmt.Errorf("evs %s %d writes, the last %s past its bound of %s from api's list (%v)", judged, stale, time.Since(deadline)+settings.StorageGateBound, settings.StorageGateBound, err)
		}
		select {
		case <-ctx.Done():
			return stale, ctx.Err()
		case <-time.After(settings.StorageTryInterval):
		}
	}
}
