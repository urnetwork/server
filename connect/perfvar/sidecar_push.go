package perfvar

// §22.8's push, config row (PROTO-M2-07; D-PROTO-5, R-PROTO-9): the harness behind the push gate's
// decision, which keeps sidecar.yml's push switch off in production until a run passes at twice
// §19.6's year-two hint rate with each Redis master at or under half a core.
//
// A cell drives hints open loop at a set rate (sidecartest/load's Driver) through the real hint script
// on the real Redis, to two kinds of client: those a resident's publisher tells, each a real
// pushservice.SidecarPublisher kicked through its process's one shared keyspace subscription on
// {scp_*}n, as an exchange's subscriber kicks them, and those that long-poll, each through a real
// pushservice.LongPoll that keeps its poll lease ({scp_<C>}hb). An attempt is one hint, from its due
// time until its seq reaches its client, so the latency is hint to frame (or to a poll's answer) with
// queueing in it. A hint a reset covered instead, and one that never arrived, is a failed attempt,
// counted beside the percentiles by kind. Evs hints carry versions near 1.8 x 10^18, as evs's push
// sequences do, and write {evsh_<repo_id>}, the head cache, as a commit does. Beside it, a config-head
// storm saves N networks' config heads a minute: each writes its config repo's head cache entry and
// hints team to the network's clients, whose events every process's subscription carries.
//
// Redis CPU per master is used_cpu_sys plus used_cpu_user from INFO cpu over the steady phase, as a
// percent of one core, by master in address order and never by address. The connect hop from a
// resident to its client is outside the cell: it costs Redis nothing, and PERFVAR's other cells
// measure it.
//
// A run uses real time, as a load run may. RunSidecarPushCell builds everything from the effective
// constructor settings (connect/CODESTYLE.md, benchmarks), and a run at SmokeSidecarPushCellSettings
// marks its reports Smoke, which never reaches a gate as evidence.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	connectprotocol "github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest/load"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

var (
	// A hint whose seq did not reach its client within HintTimeout.
	errSidecarPushHintLost = errors.New("hint-lost")
	// A hint a reset reached the client in place of.
	errSidecarPushHintCoveredByReset = errors.New("hint-covered-by-reset")
)

// What a cell runs at.
type SidecarPushCellSettings struct {
	// Hints a second, open loop: twice §19.6's year-two 6,000 for the gate.
	HintRate float64
	// Config heads saved a minute, each by another network, beside the hints: 10,000 for the gate.
	ConfigNetworksPerMinute float64
	// The networks the storm cycles through.
	ConfigNetworkCount int
	// The clients of each network its team hint reaches.
	ConfigClientsPerNetwork int
	// The run before the figures, left out of them, and the phase they come from.
	Warmup time.Duration
	Steady time.Duration
	// Clients a resident's publisher tells.
	PublisherClientCount int
	// Clients that long-poll.
	PollClientCount int
	// connect and api processes: each holds one keyspace subscription on every master, which every
	// {scp_*}n event reaches, and hosts its share of the clients.
	ProcessCount int
	// The share of hints on evs:<repo_id> topics, each with its head cache write; the rest go to team,
	// sock, mail, tls, dns and gkm in turn.
	EvsHintFraction float64
	// The share of clients holding a wake token ({scp_<C>}tok), whose acks raise {scp_<C>}ack.
	WakeTokenFraction float64
	// How long a hint may take to reach its client before it counts as lost.
	HintTimeout time.Duration
	// Attempts in flight at once, past which an arrival counts as refused by the driver.
	MaxInFlight int
	// A smoke: its reports say so, and no gate reads them.
	Smoke bool

	// The constructors' settings the cell runs on, as production resolves them.
	Publisher  *pushservice.SidecarPublisherSettings
	LongPoll   *pushservice.LongPollSettings
	HintWriter *pushservice.HintWriterSettings
	Evs        *evsservice.EvsSettings
}

// The gate's run (§22.8): 12,000 hints a second and 10,000 config heads a minute, for five minutes after
// a minute's warm-up, over a fleet's worth of processes.
func DefaultSidecarPushCellSettings() *SidecarPushCellSettings {
	return &SidecarPushCellSettings{
		HintRate:                2 * 6000,
		ConfigNetworksPerMinute: 10000,
		ConfigNetworkCount:      10000,
		ConfigClientsPerNetwork: 4,
		Warmup:                  1 * time.Minute,
		Steady:                  5 * time.Minute,
		PublisherClientCount:    20000,
		PollClientCount:         2000,
		ProcessCount:            16,
		EvsHintFraction:         0.5,
		WakeTokenFraction:       0,
		HintTimeout:             10 * time.Second,
		MaxInFlight:             200000,
		Publisher:               pushservice.DefaultSidecarPublisherSettings(),
		LongPoll:                pushservice.DefaultLongPollSettings(),
		HintWriter:              pushservice.DefaultHintWriterSettings(),
		Evs:                     evsservice.DefaultEvsSettings(),
	}
}

// A run small enough for a test on the private stack, which proves nothing.
func SmokeSidecarPushCellSettings() *SidecarPushCellSettings {
	settings := DefaultSidecarPushCellSettings()
	settings.HintRate = 20
	settings.ConfigNetworksPerMinute = 600
	settings.ConfigNetworkCount = 20
	settings.ConfigClientsPerNetwork = 2
	settings.Warmup = 1 * time.Second
	settings.Steady = 3 * time.Second
	settings.PublisherClientCount = 8
	settings.PollClientCount = 2
	settings.ProcessCount = 2
	settings.WakeTokenFraction = 0.5
	settings.MaxInFlight = 1000
	settings.Smoke = true
	return settings
}

func (self *SidecarPushCellSettings) validate() error {
	if self.HintRate <= 0 || self.ConfigNetworksPerMinute < 0 || self.Steady <= 0 || self.ProcessCount <= 0 ||
		self.PublisherClientCount+self.PollClientCount <= 0 || self.HintTimeout <= 0 ||
		(0 < self.ConfigNetworksPerMinute && (self.ConfigNetworkCount <= 0 || self.ConfigClientsPerNetwork <= 0)) ||
		self.Publisher == nil || self.LongPoll == nil || self.HintWriter == nil || self.Evs == nil {
		return errors.New("sidecar push cell: settings out of range")
	}
	return nil
}

// The settings as a report records them.
func (self *SidecarPushCellSettings) reportSettings() map[string]string {
	return map[string]string{
		"hint_rate":                  strconv.FormatFloat(self.HintRate, 'f', -1, 64),
		"config_networks_per_minute": strconv.FormatFloat(self.ConfigNetworksPerMinute, 'f', -1, 64),
		"config_network_count":       strconv.Itoa(self.ConfigNetworkCount),
		"config_clients_per_network": strconv.Itoa(self.ConfigClientsPerNetwork),
		"publisher_client_count":     strconv.Itoa(self.PublisherClientCount),
		"poll_client_count":          strconv.Itoa(self.PollClientCount),
		"process_count":              strconv.Itoa(self.ProcessCount),
		"evs_hint_fraction":          strconv.FormatFloat(self.EvsHintFraction, 'f', -1, 64),
		"wake_token_fraction":        strconv.FormatFloat(self.WakeTokenFraction, 'f', -1, 64),
		"hint_timeout":               self.HintTimeout.String(),
		"publisher":                  fmt.Sprintf("%+v", *self.Publisher),
		"long_poll":                  fmt.Sprintf("%+v", *self.LongPoll),
		"hint_writer":                fmt.Sprintf("%+v", *self.HintWriter),
		"evs_head_cache_ttl":         self.Evs.HeadCacheTtl.String(),
	}
}

// What one run reports: the hints' and the storm's reports in sidecartest/load's shape, and Redis CPU
// per master over the steady phase.
type SidecarPushCellReport struct {
	Hints       *load.Report
	ConfigStorm *load.Report
	// Percent of one core each master spent over the steady phase, masters in address order.
	RedisCpuPercents []float64
	// What the clients were sent: hints, and resets, each of which covered whatever hints were pending.
	HintFrameCount  int64
	ResetFrameCount int64
}

// Whether the run shows §22.8's figures: no failed hint, each master at or under half a core, and the
// hints' p99 within 2 s (S2), with the reports fit for a gate. The answer of a smoke is always an error.
func (self *SidecarPushCellReport) GateEvidence() error {
	reasons := []string{}
	for _, report := range []*load.Report{self.Hints, self.ConfigStorm} {
		if err := report.GateEvidence(); err != nil {
			reasons = append(reasons, err.Error())
		}
	}
	for index, percent := range self.RedisCpuPercents {
		if 50 < percent {
			reasons = append(reasons, fmt.Sprintf("master %d spent %.1f%% of a core", index, percent))
		}
	}
	if 0 < self.Hints.Failed {
		reasons = append(reasons, fmt.Sprintf("%d hints failed", self.Hints.Failed))
	}
	if 2000 < self.Hints.LatencyMillis.P99 {
		reasons = append(reasons, fmt.Sprintf("the hints' p99 is %.0f ms", self.Hints.LatencyMillis.P99))
	}
	if len(reasons) == 0 {
		return nil
	}
	return errors.New("the push cell is no gate evidence: " + strings.Join(reasons, "; "))
}

// A client and its seq: one hint's identity while its keys live.
type sidecarPushHintKey struct {
	clientId connect.Id
	seq      uint64
}

// Where each hint's arrival meets its attempt, whichever comes first. Safe for concurrent use.
type sidecarPushArrivals struct {
	stateLock sync.Mutex
	// arrivals no attempt waits for yet
	arrived map[sidecarPushHintKey]bool
	// attempts waiting, by hint, each closed with the error it ends with
	waiters map[sidecarPushHintKey]chan error
	// the clients' waiting hints, for a reset that covers them
	clientWaiters map[connect.Id]map[uint64]bool
	// a client's resets, so an attempt registered after one that covered it ends at once
	clientResets map[connect.Id]int64

	hintFrameCount  atomic.Int64
	resetFrameCount atomic.Int64
}

func newSidecarPushArrivals() *sidecarPushArrivals {
	return &sidecarPushArrivals{
		arrived:       map[sidecarPushHintKey]bool{},
		waiters:       map[sidecarPushHintKey]chan error{},
		clientWaiters: map[connect.Id]map[uint64]bool{},
		clientResets:  map[connect.Id]int64{},
	}
}

func (self *sidecarPushArrivals) hint(clientId connect.Id, seq uint64) {
	self.hintFrameCount.Add(1)
	key := sidecarPushHintKey{clientId: clientId, seq: seq}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if waiter, ok := self.waiters[key]; ok {
		delete(self.waiters, key)
		delete(self.clientWaiters[clientId], seq)
		waiter <- nil
		return
	}
	self.arrived[key] = true
}

// A reset reached the client: every hint it waits for is covered, never to come as itself.
func (self *sidecarPushArrivals) reset(clientId connect.Id) {
	self.resetFrameCount.Add(1)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientResets[clientId] += 1
	for seq := range self.clientWaiters[clientId] {
		key := sidecarPushHintKey{clientId: clientId, seq: seq}
		self.waiters[key] <- errSidecarPushHintCoveredByReset
		delete(self.waiters, key)
	}
	delete(self.clientWaiters, clientId)
}

// Waits for the hint's arrival, which may have come already, up to timeout.
func (self *sidecarPushArrivals) wait(ctx context.Context, clientId connect.Id, seq uint64, timeout time.Duration) error {
	key := sidecarPushHintKey{clientId: clientId, seq: seq}
	waiter := make(chan error, 1)
	arrived := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.arrived[key] {
			delete(self.arrived, key)
			return true
		}
		self.waiters[key] = waiter
		seqs := self.clientWaiters[clientId]
		if seqs == nil {
			seqs = map[uint64]bool{}
			self.clientWaiters[clientId] = seqs
		}
		seqs[seq] = true
		return false
	}()
	if arrived {
		return nil
	}
	forget := func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.waiters, key)
		delete(self.clientWaiters[clientId], seq)
	}
	select {
	case err := <-waiter:
		return err
	case <-time.After(timeout):
		forget()
		return errSidecarPushHintLost
	case <-ctx.Done():
		forget()
		return ctx.Err()
	}
}

// One client of the cell.
type sidecarPushClient struct {
	clientId connect.Id
	// the repo its evs hints name
	repoId sidecar.Id
	// raised by each hint, so every topic's versions rise
	version atomic.Uint64
}

// The topics a hint that is not an evs one cycles through.
var sidecarPushOtherTopics = []string{
	sidecar.PushTopicTeam, sidecar.PushTopicSock, sidecar.PushTopicMail, sidecar.PushTopicTls, sidecar.PushTopicDns, sidecar.PushTopicGkm,
}

// evs's push sequences start at a master's TIME in µs x 1,000 (§11.10): near 1.8 x 10^18.
const sidecarPushEvsVersionBase = 1_800_000_000_000_000_000

// One process: its keyspace subscription, the publishers it hosts, and, as api, a long-poll.
type sidecarPushProcess struct {
	keyEvents  *model.SidecarPushKeyEvents
	publishers map[connect.Id]*pushservice.SidecarPublisher
	longPoll   *pushservice.LongPoll
}

// The long-poll's store: the adapter's reads and writes, with key events from its own process's
// subscription rather than the binary's one.
type sidecarPushProcessRedis struct {
	*model.SidecarPushRedis
	keyEvents *model.SidecarPushKeyEvents
}

func (self *sidecarPushProcessRedis) AddKeyEventCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (remove func(), err error) {
	return self.keyEvents.AddCallback(keyPattern, callback)
}

// Runs one cell to its end and reports it.
func RunSidecarPushCell(ctx context.Context, settings *SidecarPushCellSettings) (*SidecarPushCellReport, error) {
	if err := settings.validate(); err != nil {
		return nil, err
	}
	cellCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pushRedis := model.NewSidecarPushRedis(nil)
	writer, err := pushservice.NewHintWriter(pushRedis, settings.HintWriter, nil, nil)
	if err != nil {
		return nil, err
	}
	headCacheLog := &sidecarPushCellLog{}
	headCache := evsservice.NewHeadCache(sidecar.RealClock{}, sidecarhost.NewRedis(nil), settings.Evs, headCacheLog)
	arrivals := newSidecarPushArrivals()

	processes := make([]*sidecarPushProcess, settings.ProcessCount)
	for index := range processes {
		process := &sidecarPushProcess{
			keyEvents:  model.NewSidecarPushKeyEvents(nil),
			publishers: map[connect.Id]*pushservice.SidecarPublisher{},
		}
		processes[index] = process
	}
	clients := make([]*sidecarPushClient, settings.PublisherClientCount+settings.PollClientCount)
	for index := range clients {
		clients[index] = &sidecarPushClient{clientId: connect.NewId(), repoId: sidecar.NewId()}
	}
	tokenCount := int(settings.WakeTokenFraction * float64(len(clients)))
	for _, client := range clients[:tokenCount] {
		if err := pushRedis.Set(cellCtx, pushservice.HintTokenKey(client.clientId), []byte("cell"), 0); err != nil {
			return nil, err
		}
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, client := range clients[:tokenCount] {
			server.Redis(cleanupCtx, func(redisClient server.RedisClient) {
				redisClient.Del(cleanupCtx, pushservice.HintTokenKey(client.clientId))
			})
		}
	}()

	// publishers: each kicked by its process's subscription, as an exchange's subscriber kicks it. At the
	// end the cell's context goes first, which ends every poll loop and publisher run, then each stop in
	// reverse
	stops := []func(){}
	defer func() {
		cancel()
		for _, stop := range slices.Backward(stops) {
			stop()
		}
	}()
	for index, client := range clients[:settings.PublisherClientCount] {
		process := processes[index%len(processes)]
		publisher, err := pushservice.NewSidecarPublisher(settings.Publisher, nil)
		if err != nil {
			return nil, err
		}
		process.publishers[client.clientId] = publisher
	}
	for _, process := range processes {
		remove, err := process.keyEvents.AddCallback(pushservice.HintSeqKeyPattern, func(event sidecar.RedisKeyEvent) {
			if event.Resync {
				for _, publisher := range process.publishers {
					publisher.Kick()
				}
				return
			}
			if clientId, ok := pushservice.ParseHintSeqKey(event.Key); ok {
				if publisher := process.publishers[clientId]; publisher != nil {
					publisher.Kick()
				}
			}
		})
		if err != nil {
			return nil, err
		}
		stops = append(stops, remove)
	}
	for _, process := range processes {
		for clientId, publisher := range process.publishers {
			publisher.Start(cellCtx, &pushservice.PublisherEnv{
				ClientId: clientId,
				Send:     sidecarPushCellSend(clientId, arrivals),
				Redis:    pushRedis,
			})
			stops = append(stops, publisher.Close)
		}
	}

	// long-polls: each api process's one LongPoll, each poll client polling in a loop
	pollers := sync.WaitGroup{}
	stops = append(stops, pollers.Wait)
	for index, client := range clients[settings.PublisherClientCount:] {
		process := processes[index%len(processes)]
		if process.longPoll == nil {
			longPoll, err := pushservice.NewLongPoll(cellCtx, &sidecarPushProcessRedis{SidecarPushRedis: pushRedis, keyEvents: process.keyEvents}, nil, settings.LongPoll, nil)
			if err != nil {
				return nil, err
			}
			process.longPoll = longPoll
			stops = append(stops, longPoll.Close)
		}
		pollers.Add(1)
		go func() {
			defer pollers.Done()
			sidecarPushCellPoll(cellCtx, process.longPoll, client.clientId, arrivals)
		}()
	}

	hintIndex := atomic.Int64{}
	hintDriver := load.NewDriver(&load.DriverSettings{
		Name:            "sidecar-push-hints",
		Rate:            settings.HintRate,
		Warmup:          settings.Warmup,
		Steady:          settings.Steady,
		MaxInFlight:     settings.MaxInFlight,
		MaxFailureKinds: 16,
	}, func(ctx context.Context, arrival int64) error {
		client := clients[int(arrival)%len(clients)]
		version := client.version.Add(1)
		hint := &sidecar.ClientHint{Kind: 0x0A01, Body: []byte(fmt.Sprintf("cell-%d", arrival))}
		evs := float64(hintIndex.Add(1)%100) < 100*settings.EvsHintFraction
		if evs {
			hint.Topic = sidecar.PushTopicEvs(client.repoId)
			hint.Version = sidecarPushEvsVersionBase + version
		} else {
			hint.Topic = sidecarPushOtherTopics[int(arrival)%len(sidecarPushOtherTopics)]
			hint.Version = version
		}
		result, err := writer.HintClient(ctx, client.clientId, hint)
		if err != nil {
			return err
		}
		if evs {
			headCache.Write(ctx, client.repoId, client.repoId, sidecarPushCellHead(hint.Version), hint.Version)
		}
		return arrivals.wait(ctx, client.clientId, result.Seq, settings.HintTimeout)
	})

	type configNetwork struct {
		configRepoId sidecar.Id
		clientIds    []server.Id
		version      atomic.Uint64
	}
	configNetworks := make([]*configNetwork, settings.ConfigNetworkCount)
	for index := range configNetworks {
		network := &configNetwork{configRepoId: sidecar.NewId()}
		for range settings.ConfigClientsPerNetwork {
			network.clientIds = append(network.clientIds, server.NewId())
		}
		configNetworks[index] = network
	}
	var configDriver *load.Driver
	if 0 < settings.ConfigNetworksPerMinute {
		configDriver = load.NewDriver(&load.DriverSettings{
			Name:            "sidecar-push-config-storm",
			Rate:            settings.ConfigNetworksPerMinute / 60,
			Warmup:          settings.Warmup,
			Steady:          settings.Steady,
			MaxInFlight:     settings.MaxInFlight,
			MaxFailureKinds: 16,
		}, func(ctx context.Context, arrival int64) error {
			network := configNetworks[int(arrival)%len(configNetworks)]
			version := network.version.Add(1)
			headCache.Write(ctx, network.configRepoId, network.configRepoId, sidecarPushCellHead(version), version)
			return model.SidecarHint(ctx, network.clientIds, &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: version})
		})
	}

	// the drivers together, Redis CPU read across their shared steady phase
	var hintReport, configReport *load.Report
	runs := sync.WaitGroup{}
	runs.Add(1)
	go func() {
		defer runs.Done()
		hintReport = hintDriver.Run(cellCtx)
	}()
	if configDriver != nil {
		runs.Add(1)
		go func() {
			defer runs.Done()
			configReport = configDriver.Run(cellCtx)
		}()
	}
	var cpuPercents []float64
	var cpuErr error
	select {
	case <-time.After(settings.Warmup):
		startCpu, err := readSidecarPushRedisCpuSeconds(cellCtx)
		startTime := time.Now()
		if err != nil {
			cpuErr = err
			break
		}
		select {
		case <-time.After(settings.Steady):
		case <-cellCtx.Done():
		}
		endCpu, err := readSidecarPushRedisCpuSeconds(cellCtx)
		if err != nil {
			cpuErr = err
			break
		}
		elapsed := time.Since(startTime).Seconds()
		for index := range min(len(startCpu), len(endCpu)) {
			cpuPercents = append(cpuPercents, 100*(endCpu[index]-startCpu[index])/elapsed)
		}
	case <-cellCtx.Done():
	}
	runs.Wait()

	report := &SidecarPushCellReport{
		Hints:            hintReport,
		ConfigStorm:      configReport,
		RedisCpuPercents: cpuPercents,
		HintFrameCount:   arrivals.hintFrameCount.Load(),
		ResetFrameCount:  arrivals.resetFrameCount.Load(),
	}
	if report.ConfigStorm == nil {
		report.ConfigStorm = &load.Report{Name: "sidecar-push-config-storm", Notes: []string{"no storm ran"}}
	}
	for _, each := range []*load.Report{report.Hints, report.ConfigStorm} {
		each.Smoke = settings.Smoke
		each.Settings = settings.reportSettings()
		if cpuErr != nil {
			each.Notes = append(each.Notes, "redis cpu unread: "+cpuErr.Error())
		}
		if failures := headCacheLog.failureCount.Load(); 0 < failures {
			each.Notes = append(each.Notes, fmt.Sprintf("%d head cache writes failed", failures))
		}
		each.Notes = append(each.Notes,
			"the cell hosts real publishers on per-process shared subscriptions, not exchange residents: the connect hop to the client is outside it",
			"evs's push sequence and the ACL projection cache are not built yet, so neither runs; the evs head cache does",
		)
	}
	report.Hints.Targets = []string{"evs-hint-p50", "evs-hint-p99"}
	return report, nil
}

// A publisher's send: each frame decoded as the client reads it, its hints and resets told to the
// arrivals, its buffer returned, and its ack given as a client's connect stack gives it, on its own
// goroutine.
func sidecarPushCellSend(clientId connect.Id, arrivals *sidecarPushArrivals) func(frame *connectprotocol.Frame, ack connect.AckFunction) bool {
	return func(frame *connectprotocol.Frame, ack connect.AckFunction) bool {
		wrapper := &connectprotocol.SubprotocolMessage{}
		envelope := &sidecarprotocol.Envelope{}
		if proto.Unmarshal(frame.MessageBytes, wrapper) == nil && proto.Unmarshal(wrapper.MessageBytes, envelope) == nil {
			switch sidecarprotocol.MessageType(envelope.Type) {
			case sidecarprotocol.MessageType_PUSH_HINT:
				hint := &sidecarprotocol.PushHint{}
				if proto.Unmarshal(envelope.Body, hint) == nil {
					arrivals.hint(clientId, hint.Seq)
				}
			case sidecarprotocol.MessageType_PUSH_RESET:
				arrivals.reset(clientId)
			}
		}
		connect.MessagePoolReturn(frame.MessageBytes)
		if ack != nil {
			go ack(nil)
		}
		return true
	}
}

// One poll client's loop until ctx ends: each answer's hints and reset told to the arrivals, its cursor
// kept for the next. A refused or failed poll waits a moment, as a client's backoff does.
func sidecarPushCellPoll(ctx context.Context, longPoll *pushservice.LongPoll, clientId connect.Id, arrivals *sidecarPushArrivals) {
	cursor := []byte(nil)
	for ctx.Err() == nil {
		response, err := longPoll.Poll(ctx, clientId, cursor)
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if response.Reset_ != nil {
			arrivals.reset(clientId)
		}
		for _, hint := range response.Hints {
			arrivals.hint(clientId, hint.Seq)
		}
		cursor = response.Cursor
	}
}

// The head cache's log, which counts the writes it reports failed and keeps no text: a load run's
// failures are figures.
type sidecarPushCellLog struct {
	failureCount atomic.Int64
}

func (self *sidecarPushCellLog) Info(args ...any) {}

func (self *sidecarPushCellLog) Infof(format string, args ...any) {}

func (self *sidecarPushCellLog) Warningf(format string, args ...any) {
	self.failureCount.Add(1)
}

func (self *sidecarPushCellLog) Errorf(format string, args ...any) {
	self.failureCount.Add(1)
}

func (self *sidecarPushCellLog) V(level int32) sidecar.Verbose {
	return sidecar.NewNoopLogger().V(level)
}

// A committed head at version, as a commit would leave it for the head cache.
func sidecarPushCellHead(version uint64) *evsservice.EvsStoredHead {
	head := &sidecarprotocol.EvsHead{Version: version}
	headBytes, _ := proto.Marshal(head)
	return &evsservice.EvsStoredHead{Head: head, Bytes: headBytes, Etag: strconv.FormatUint(version, 10)}
}

// Each master's used_cpu_sys plus used_cpu_user, in seconds, masters in address order.
func readSidecarPushRedisCpuSeconds(ctx context.Context) ([]float64, error) {
	read := func(node *redis.Client) (float64, error) {
		info, err := node.Info(ctx, "cpu").Result()
		if err != nil {
			return 0, err
		}
		seconds := 0.0
		for _, line := range strings.Split(info, "\n") {
			name, value, found := strings.Cut(strings.TrimSpace(line), ":")
			if !found || (name != "used_cpu_sys" && name != "used_cpu_user") {
				continue
			}
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return 0, err
			}
			seconds += parsed
		}
		return seconds, nil
	}
	addrSeconds := map[string]float64{}
	var readErr error
	server.Redis(ctx, func(client server.RedisClient) {
		switch typed := client.(type) {
		case *redis.ClusterClient:
			stateLock := sync.Mutex{}
			readErr = typed.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
				seconds, err := read(node)
				stateLock.Lock()
				defer stateLock.Unlock()
				addrSeconds[node.Options().Addr] = seconds
				return err
			})
		case *redis.Client:
			seconds, err := read(typed)
			addrSeconds[typed.Options().Addr] = seconds
			readErr = err
		default:
			readErr = fmt.Errorf("a redis client of %T", client)
		}
	})
	if readErr != nil {
		return nil, readErr
	}
	secondsByMaster := []float64{}
	for _, addr := range slices.Sorted(func(yield func(string) bool) {
		for addr := range addrSeconds {
			if !yield(addr) {
				return
			}
		}
	}) {
		secondsByMaster = append(secondsByMaster, addrSeconds[addr])
	}
	return secondsByMaster, nil
}
