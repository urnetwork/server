package connect

import (
	"context"
	"encoding/hex"
	// "fmt"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

func DefaultConnectionAnnounceSettings() *ConnectionAnnounceSettings {
	return &ConnectionAnnounceSettings{
		SyncConnectionTimeout: model.ReliabilityBlockDuration / 2,
		LocationRetryTimeout:  5 * time.Minute,
		// PeerRegisterTtl is derived from ExchangeResidentTtl in
		// NewConnectHandler (must equal the heartbeat refresh ttl or
		// disconnect detection is delayed); this default is a placeholder.
		PeerRegisterTtl: 300 * time.Second,
		// 2026-07-15: network peers disabled pending the PEERS2 poll rollout
		EnableNetworkPeers:       false,
		MaxLatencyCount:          16,
		MinTestTimeout:           12 * time.Hour,
		MaxTestTimeout:           24 * time.Hour,
		LatencySampleWindowCount: 4,
		SpeedSampleWindowCount:   4,

		PassiveSpeedWindowDuration: 5 * time.Second,
		PassiveSpeedMinByteCount:   512 * model.Kib,
		// aligned with the `bytesPerSecondCutoff` used by provider ranking
		SyntheticSpeedBytesPerSecond: 4 * model.Mib,
		SyntheticSpeedTimeout:        60 * time.Second,
	}
}

type ConnectionAnnounceSettings struct {
	SyncConnectionTimeout time.Duration
	LocationRetryTimeout  time.Duration
	// ttl for the network peer registration made at announce
	PeerRegisterTtl time.Duration
	// mirrors ExchangeSettings.EnableNetworkPeers (2026-07-15: disabled
	// pending pubsub throughput redesign)
	EnableNetworkPeers bool
	MaxLatencyCount    int
	MinTestTimeout     time.Duration
	MaxTestTimeout     time.Duration

	LatencySampleWindowCount int
	SpeedSampleWindowCount   int

	// passive speed measures the actual traffic rate in windows of
	// `PassiveSpeedWindowDuration`, and updates the speed result whenever a
	// new maximum is reached. Windows that move less than
	// `PassiveSpeedMinByteCount` are not counted as samples.
	PassiveSpeedWindowDuration time.Duration
	PassiveSpeedMinByteCount   ByteCount
	// a connection that has not passively demonstrated at least
	// `SyntheticSpeedBytesPerSecond` after `SyntheticSpeedTimeout` runs a
	// synthetic speed test. Connections with faster active traffic prove
	// their speed passively and are never disrupted by a synthetic test
	// (during a test the client transport echoes all messages upstream
	// instead of delivering them).
	SyntheticSpeedBytesPerSecond ByteCount
	SyntheticSpeedTimeout        time.Duration
}

type LatencyTest struct {
	TestId server.Id
}

type SpeedTest struct {
	TestId         uint32
	TotalByteCount model.ByteCount
}

func DefaultTestConfig() *TestConfig {
	return &TestConfig{
		MaxLatencyCount: 2,
		// warm up then test
		MaxSpeedCount:       2,
		SpeedTotalByteCount: 512 * model.Kib,
	}
}

func V0TestConfig() *TestConfig {
	return &TestConfig{
		MaxLatencyCount: 0,
		MaxSpeedCount:   0,
	}
}

type TestConfig struct {
	MaxLatencyCount     int
	MaxSpeedCount       int
	SpeedTotalByteCount ByteCount
}

func (self *TestConfig) AllowLatency() bool {
	return 0 < self.MaxLatencyCount
}

func (self *TestConfig) AllowSpeed() bool {
	return 0 < self.MaxSpeedCount
}

type ConnectionAnnounce struct {
	ctx             context.Context
	cancel          context.CancelFunc
	networkId       server.Id
	clientId        server.Id
	clientAddress   string
	handlerId       server.Id
	announceTimeout time.Duration

	settings *ConnectionAnnounceSettings

	startTime time.Time

	stateLock           sync.Mutex
	connectionId        *server.Id
	receiveMessageCount uint64
	receiveByteCount    ByteCount
	sendMessageCount    uint64
	sendByteCount       ByteCount

	latencyCount        int
	latencyTest         *LatencyTest
	latencyTestSendTime time.Time
	minLatencyMillis    uint64
	speedCount          int
	speedTestId         uint32
	speedTest           *SpeedTest
	speedTestSendTime   time.Time
	maxBytesPerSecond   model.ByteCount

	passiveWindowStartTime        time.Time
	passiveWindowSendByteCount    ByteCount
	passiveWindowReceiveByteCount ByteCount
	passiveMaxBytesPerSecond      ByteCount

	testConfig         *TestConfig
	PendingLatencyTest chan *LatencyTest
	PendingSpeedTest   chan *SpeedTest
}

func NewConnectionAnnounceWithDefaults(
	ctx context.Context,
	cancel context.CancelFunc,
	networkId server.Id,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
	announceTimeout time.Duration,
) *ConnectionAnnounce {
	return NewConnectionAnnounce(
		ctx,
		cancel,
		networkId,
		clientId,
		clientAddress,
		handlerId,
		announceTimeout,
		DefaultTestConfig(),
		DefaultConnectionAnnounceSettings(),
	)
}

func NewConnectionAnnounce(
	ctx context.Context,
	cancel context.CancelFunc,
	networkId server.Id,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
	announceTimeout time.Duration,
	testConfig *TestConfig,
	settings *ConnectionAnnounceSettings,
) *ConnectionAnnounce {
	announce := &ConnectionAnnounce{
		ctx:                    ctx,
		cancel:                 cancel,
		networkId:              networkId,
		clientId:               clientId,
		clientAddress:          clientAddress,
		handlerId:              handlerId,
		announceTimeout:        announceTimeout,
		settings:               settings,
		testConfig:             testConfig,
		startTime:              time.Now(),
		passiveWindowStartTime: time.Now(),
		PendingLatencyTest:     make(chan *LatencyTest),
		PendingSpeedTest:       make(chan *SpeedTest),
	}
	go server.HandleError(announce.run, cancel)
	return announce
}

func (self *ConnectionAnnounce) run() {
	defer self.cancel()

	if 0 < self.announceTimeout {
		model.SetPendingNetworkClientConnection(self.ctx, self.clientId, self.announceTimeout+5*time.Second)
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.announceTimeout / 2):
		}

		if self.testConfig.AllowLatency() || self.testConfig.AllowSpeed() {
			nextLatency := false
			nextSpeed := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.latencyTest == nil && self.speedTest == nil {
					if self.testConfig.AllowLatency() {
						nextLatency = true
					} else if self.allowSyntheticSpeedWithLock() {
						nextSpeed = true
					}
				}
			}()
			if nextLatency || nextSpeed {
				provideModes, err := model.GetProvideModes(self.ctx, self.clientId)
				if err == nil && provideModes[model.ProvideModePublic] {
					if nextLatency {
						self.nextLatency()
					} else if nextSpeed {
						self.nextSpeed()
					}
				}
			}
		}

		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.announceTimeout / 2):
		}
	}

	// FIXME remove ip
	// glog.Infof("[t]announce %s [%s]\n", self.clientId, self.clientAddress)

	// FIXME compute expected latency between client and this edge
	// FIXME store edge coordinated in the config as host variables
	// FIXME pass the host coordinate in here so the expected latency can be set
	connectionId, clientAddressHash, err := controller.ConnectNetworkClient(
		self.ctx,
		self.clientId,
		self.clientAddress,
		self.handlerId,
		self.settings.LocationRetryTimeout,
	)
	if err != nil {
		glog.Infof("[t][%s]could not connect client. err = %s\n", hex.EncodeToString(clientAddressHash[:]), err)
		return
	}

	// verify egress feeder (observed source ip, sn/VALIDATOR.md §8): while
	// connected, this client's observed address is a candidate provider
	// egress for `/verify` source-ip attribution. Feed on connect,
	// ttl-refresh while connected, clear on disconnect. The bijection gate in
	// the model excludes shared or ambiguous ips (§8.2).
	verifySettings := model.DefaultVerifySettings()
	verifyEgressIp, verifyEgressOk := model.ParseVerifyEgressIp(self.clientAddress)
	if verifyEgressOk {
		model.FeedVerifyEgress(self.ctx, self.clientId, verifyEgressIp, verifySettings)
		go server.HandleError(func() {
			for {
				select {
				case <-self.ctx.Done():
					return
				case <-time.After(verifySettings.EgressRefreshInterval):
				}
				model.FeedVerifyEgress(self.ctx, self.clientId, verifyEgressIp, verifySettings)
			}
		}, self.cancel)
	}

	defer server.HandleError(func() {
		// note use an uncanceled context for cleanup
		cleanupCtx := context.Background()
		model.DisconnectNetworkClient(cleanupCtx, connectionId)
		if verifyEgressOk {
			model.ClearVerifyEgress(cleanupCtx, self.clientId, verifyEgressIp)
		}
		if glog.V(1) {
			glog.Infof("[t][%s]disconnect client\n", hex.EncodeToString(clientAddressHash[:]))
		}
	})

	self.setConnectionId(connectionId)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.setLatencyWithLock()
		self.setSpeedWithLock()
	}()

	// register this client in the network peer registry, now that the
	// connection has survived the announce window. 2026-07-15 outage:
	// registration used to run on the resident nomination hot path, where
	// connection churn turned per-nomination publishes into a pubsub storm
	// (subscriber output buffers pushed redis nodes to maxmemory) and the
	// registry write then hung new connections against those full nodes.
	// Announce-time registration means churny connections that die before
	// announcing never publish a peer event at all. A failure here fails the
	// announce and tears down the connection (announce's contract) — the
	// panic propagates to run's HandleError cancel. Proxy clients do not pass
	// through announce and register via the resident heartbeat alone.
	// EnableNetworkPeers: see ExchangeSettings — network peers are disabled
	// 2026-07-15 pending a pubsub throughput redesign.
	if self.settings.EnableNetworkPeers {
		peerNetworkId, topLevel, peerCategory, peerProfile, peersEnabled := model.GetNetworkPeerProfile(self.ctx, self.clientId)
		if peersEnabled && topLevel && peerCategory != model.NetworkPeerCategoryProxy && peerProfile != nil {
			// ttl 0: read-only lookup — extending the resident record's expiry
			// is the resident keepalive's job, not the announce's
			if resident := model.GetResidentForClient(self.ctx, self.clientId, 0); resident != nil {
				model.AddNetworkPeer(self.ctx, peerNetworkId, peerProfile, resident.ResidentId, self.settings.PeerRegisterTtl)
			}
			// no resident nominated yet: the resident heartbeat registers it
		}
	}

	// continuously measure the passive speed of the connection.
	// active traffic proves the connection speed without a synthetic test.
	go server.HandleError(func() {
		for {
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(self.settings.PassiveSpeedWindowDuration):
			}
			self.samplePassiveSpeed()
		}
	}, self.cancel)

	nextTestTime := server.NowUtc().Add(self.settings.MaxTestTimeout)
	nextTest := func() time.Duration {
		timeout := nextTestTime.Sub(server.NowUtc())
		if 0 < timeout {
			return timeout
		}
		nextLatency := false
		nextSpeed := false
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.latencyTest == nil && self.speedTest == nil {
				self.latencyCount = 0
				self.speedCount = 0
				if self.testConfig.AllowLatency() {
					nextLatency = true
				} else if self.allowSyntheticSpeedWithLock() {
					nextSpeed = true
				}
			}
		}()
		if nextLatency {
			self.nextLatency()
		} else if nextSpeed {
			self.nextSpeed()
		}
		timeout = self.settings.MinTestTimeout + time.Duration(
			mathrand.Int63n(int64(self.settings.MaxTestTimeout-self.settings.MinTestTimeout)),
		)
		nextTestTime = server.NowUtc().Add(timeout)
		return timeout
	}

	established := self.announceTimeout == 0
	nextStartTime := server.NowUtc()
	for {
		nextTestTimeout := nextTest()
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(nextTestTimeout):
		case <-time.After(self.settings.SyncConnectionTimeout):
			startTime := nextStartTime
			nextStartTime = server.NowUtc()

			// FIXME need events to support kicking off connections from the model
			/*
				status := model.GetNetworkClientConnectionStatus(self.ctx, connectionId)
				if err := status.Err(); err != nil {
					glog.Infof("[t][%s]connection err = %s\n", connectionId, err)
					return
				}
			*/

			var receiveMessageCount uint64
			var receiveByteCount ByteCount
			var sendMessageCount uint64
			var sendByteCount ByteCount
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				receiveMessageCount = self.receiveMessageCount
				receiveByteCount = self.receiveByteCount
				sendMessageCount = self.sendMessageCount
				sendByteCount = self.sendByteCount
				self.receiveMessageCount = 0
				self.receiveByteCount = 0
				self.sendMessageCount = 0
				self.sendByteCount = 0
			}()

			stats := &model.ClientReliabilityStats{
				ReceiveMessageCount: receiveMessageCount,
				ReceiveByteCount:    receiveByteCount,
				SendMessageCount:    sendMessageCount,
				SendByteCount:       sendByteCount,
			}

			changedCount, currentProvideModes := model.GetProvideKeyChanges(self.ctx, self.clientId, startTime)
			provideEnabled := currentProvideModes[model.ProvideModePublic]
			// stats only matter for providers
			// avoid populating stats for non-providers
			if provideEnabled || 0 < changedCount {
				if established {
					nextLatency := false
					nextSpeed := false
					func() {
						self.stateLock.Lock()
						defer self.stateLock.Unlock()
						if self.testConfig.AllowLatency() {
							nextLatency = (self.latencyCount == 0)
						}
						if !nextLatency && self.allowSyntheticSpeedWithLock() {
							nextSpeed = (self.speedCount == 0)
						}
					}()
					if provideEnabled {
						if nextLatency {
							self.nextLatency()
						} else if nextSpeed {
							self.nextSpeed()
						}
					}

					// add reliability stats if all of:
					// 1. established provide public (no changes in block)
					// 2. established connection
					// 3. at least one message count
					// OR
					// 1. new connection (this will invalidate the block)
					// OR
					// 1. provide change (this will invalidate the block)

					stats.ConnectionEstablishedCount = 1
					if provideEnabled {
						stats.ProvideEnabledCount = 1
					}
					if 0 < changedCount {
						stats.ProvideChangedCount = 1
					}
				} else {
					established = provideEnabled
					stats.ConnectionNewCount = 1
				}
				// record to redis only (never pg): the per-sync stats of every
				// connected provider were the single largest statement load on
				// the database. RollupClientReliabilityStats drains the redis
				// counters into `client_reliability` per block.
				model.RecordClientReliabilityStatsRange(
					self.ctx,
					self.networkId,
					self.clientId,
					clientAddressHash,
					startTime,
					nextStartTime,
					stats,
				)
			}
		}
	}
}

func (self *ConnectionAnnounce) setConnectionId(connectionId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.connectionId = &connectionId
}

func (self *ConnectionAnnounce) ConnectionId() *server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.connectionId
}

func (self *ConnectionAnnounce) ReceiveMessage(messageByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.receiveMessageCount += 1
	self.receiveByteCount += messageByteCount
	self.passiveWindowReceiveByteCount += messageByteCount
}

func (self *ConnectionAnnounce) SendMessage(messageByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.sendMessageCount += 1
	self.sendByteCount += messageByteCount
	self.passiveWindowSendByteCount += messageByteCount
}

// samplePassiveSpeed closes the current passive window and updates the speed
// result when the window rate is a new maximum for the connection
func (self *ConnectionAnnounce) samplePassiveSpeed() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()
	windowMillis := ByteCount(now.Sub(self.passiveWindowStartTime) / time.Millisecond)
	windowByteCount := max(self.passiveWindowSendByteCount, self.passiveWindowReceiveByteCount)
	self.passiveWindowStartTime = now
	self.passiveWindowSendByteCount = 0
	self.passiveWindowReceiveByteCount = 0

	if windowByteCount < self.settings.PassiveSpeedMinByteCount {
		return
	}
	if windowMillis <= 0 {
		return
	}

	bytesPerSecond := 1000 * windowByteCount / windowMillis
	if self.passiveMaxBytesPerSecond < bytesPerSecond {
		self.passiveMaxBytesPerSecond = bytesPerSecond
		if glog.V(1) {
			glog.Infof("[ta][%s]passive speed %.2fmib/s\n", self.clientId, float64(bytesPerSecond)/float64(1024*1024))
		}
		self.setSpeedSampleWithLock(bytesPerSecond)
	}
}

// a synthetic speed test runs only when the connection has not passively
// demonstrated `SyntheticSpeedBytesPerSecond` after `SyntheticSpeedTimeout`.
// Busy connections prove their speed with passive samples, and a synthetic
// test would disrupt active traffic since the client transport echoes all
// messages upstream during the test instead of delivering them.
func (self *ConnectionAnnounce) allowSyntheticSpeedWithLock() bool {
	if !self.testConfig.AllowSpeed() {
		return false
	}
	if self.settings.SyntheticSpeedBytesPerSecond <= self.passiveMaxBytesPerSecond {
		return false
	}
	if time.Now().Sub(self.startTime) < self.settings.SyntheticSpeedTimeout {
		return false
	}
	return true
}

func (self *ConnectionAnnounce) Close() {
	self.cancel()
}

func (self *ConnectionAnnounce) nextLatency() {
	latencyTest := &LatencyTest{
		TestId: server.NewId(),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.latencyTest = latencyTest
	}()
	go server.HandleError(func() {
		select {
		case <-self.ctx.Done():
		case self.PendingLatencyTest <- latencyTest:
		}
	})
}

func (self *ConnectionAnnounce) SendLatency(latencyTest *LatencyTest) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.latencyTest != nil && self.latencyTest.TestId == latencyTest.TestId {
		self.latencyTestSendTime = time.Now()
		return true
	}
	return false
}

func (self *ConnectionAnnounce) ReceiveLatency(latencyTest *LatencyTest) (success bool) {
	receiveTime := time.Now()
	nextLatency := false
	nextSpeed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.latencyTest != nil && self.latencyTest.TestId == latencyTest.TestId {
			latencyMillis := uint64((receiveTime.Sub(self.latencyTestSendTime) + time.Millisecond/2) / time.Millisecond)

			if glog.V(1) {
				glog.Infof("[ta][%s]latency %dms\n", self.clientId, latencyMillis)
			}

			self.latencyCount += 1
			if self.latencyCount == 1 || latencyMillis < self.minLatencyMillis {
				self.minLatencyMillis = latencyMillis
			}

			self.latencyTest = nil

			nextLatency = self.latencyCount < self.settings.MaxLatencyCount
			if !nextLatency {
				self.setLatencyWithLock()
				if self.allowSyntheticSpeedWithLock() {
					nextSpeed = (self.speedCount == 0 && self.speedTest == nil)
				}
			}

			success = true
		}
	}()
	if nextLatency {
		self.nextLatency()
	} else if nextSpeed {
		self.nextSpeed()
	}
	return
}

func (self *ConnectionAnnounce) setLatencyWithLock() {
	if 0 < self.latencyCount && self.connectionId != nil {
		// average of `LatencySampleWindowCount` samples
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
				INSERT INTO network_client_latency (
					connection_id,
					latency_ms,
					sample_count
				)
				VALUES ($1, $2, $3)
				ON CONFLICT (connection_id) DO UPDATE
				SET
					latency_ms = ((LEAST(network_client_latency.sample_count + $3, $4) - 1) * network_client_latency.latency_ms + $2) / LEAST(network_client_latency.sample_count + $3, $4),
					sample_count = network_client_latency.sample_count + $3
				`,
				*self.connectionId,
				self.minLatencyMillis,
				1,
				self.settings.LatencySampleWindowCount,
			))
		})
	}
}

func (self *ConnectionAnnounce) nextSpeed() {
	var speedTest *SpeedTest
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		speedTest = &SpeedTest{
			TestId:         self.speedTestId,
			TotalByteCount: self.testConfig.SpeedTotalByteCount,
		}
		self.speedTestId += 1
		self.speedTest = speedTest
	}()
	go server.HandleError(func() {
		select {
		case <-self.ctx.Done():
		case self.PendingSpeedTest <- speedTest:
		}
	})
}

func (self *ConnectionAnnounce) SendSpeed(speedTest *SpeedTest) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.speedTest != nil && self.speedTest.TestId == speedTest.TestId {
		self.speedTestSendTime = time.Now()
		return true
	}
	return false
}

func (self *ConnectionAnnounce) ReceiveSpeed(speedTest *SpeedTest) (success bool) {
	receiveTime := time.Now()
	nextSpeed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.speedTest != nil && self.speedTest.TestId == speedTest.TestId && self.speedTest.TotalByteCount == speedTest.TotalByteCount {
			testMillis := model.ByteCount((receiveTime.Sub(self.speedTestSendTime) + time.Millisecond/2) / time.Millisecond)
			bytesPerSecond := (1000*speedTest.TotalByteCount + testMillis/2) / testMillis

			if glog.V(1) {
				glog.Infof("[ta][%s]speed %.2fmib/s\n", self.clientId, float64(bytesPerSecond)/float64(1024*1024))
			}

			self.speedCount += 1
			if self.speedCount == 1 || self.maxBytesPerSecond < bytesPerSecond {
				self.maxBytesPerSecond = bytesPerSecond
			}

			self.speedTest = nil

			nextSpeed = self.speedCount < self.testConfig.MaxSpeedCount
			if !nextSpeed {
				self.setSpeedWithLock()
			}

			success = true
		}
	}()
	if nextSpeed {
		self.nextSpeed()
	}
	return
}

func (self *ConnectionAnnounce) setSpeedWithLock() {
	if 0 < self.speedCount {
		self.setSpeedSampleWithLock(self.maxBytesPerSecond)
	}
}

func (self *ConnectionAnnounce) setSpeedSampleWithLock(bytesPerSecond ByteCount) {
	if self.connectionId == nil {
		return
	}
	// average of `SpeedSampleWindowCount` samples
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`
			INSERT INTO network_client_speed (
				connection_id,
				bytes_per_second,
				sample_count
			)
			VALUES ($1, $2, $3)
			ON CONFLICT (connection_id) DO UPDATE
			SET
				bytes_per_second = ((LEAST(network_client_speed.sample_count + $3, $4) - 1) * network_client_speed.bytes_per_second + $2) / LEAST(network_client_speed.sample_count + $3, $4),
				sample_count = network_client_speed.sample_count + $3
			`,
			*self.connectionId,
			bytesPerSecond,
			1,
			self.settings.SpeedSampleWindowCount,
		))
	})
}
