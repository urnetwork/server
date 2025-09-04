package main

import (
	"context"
	"encoding/hex"
	// "fmt"
	"sync"
	"time"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

func DefaultConnectionAnnounceSettings() *ConnectionAnnounceSettings {
	return &ConnectionAnnounceSettings{
		SyncConnectionTimeout: model.ReliabilityBlockDuration / 2,
		LocationRetryTimeout:  5 * time.Minute,
		MaxLatencyCount:       16,
	}
}

type ConnectionAnnounceSettings struct {
	SyncConnectionTimeout time.Duration
	LocationRetryTimeout  time.Duration
	MaxLatencyCount       int
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
		MaxLatencyCount: 10,
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

	stateLock           sync.Mutex
	connectionId        *server.Id
	receiveMessageCount uint64
	receiveByteCount    ByteCount
	sendMessageCount    uint64
	sendByteCount       ByteCount

	latencyCount           int
	latencyTest            *LatencyTest
	latencyTestSendTime    time.Time
	minLatencyMilliseconds uint64
	speedCount             int
	speedTestId            uint32
	speedTest              *SpeedTest
	speedTestSendTime      time.Time
	maxBytesPerSecond      model.ByteCount

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
		ctx:                ctx,
		cancel:             cancel,
		networkId:          networkId,
		clientId:           clientId,
		clientAddress:      clientAddress,
		handlerId:          handlerId,
		announceTimeout:    announceTimeout,
		settings:           settings,
		testConfig:         testConfig,
		PendingLatencyTest: make(chan *LatencyTest),
		PendingSpeedTest:   make(chan *SpeedTest),
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
				if self.testConfig.AllowLatency() {
					nextLatency = (self.latencyCount == 0 && self.latencyTest == nil)
					if !nextLatency {
						nextSpeed = (self.speedCount == 0 && self.speedTest == nil)
					}
				} else if self.testConfig.AllowSpeed() {
					nextSpeed = (self.speedCount == 0 && self.speedTest == nil)
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
	defer server.HandleError(func() {
		// note use an uncanceled context for cleanup
		cleanupCtx := context.Background()
		model.DisconnectNetworkClient(cleanupCtx, connectionId)
		glog.V(1).Infof("[t][%s]disconnect client\n", hex.EncodeToString(clientAddressHash[:]))
	}, self.cancel)

	self.setConnectionId(connectionId)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.setLatencyWithLock()
		self.setSpeedWithLock()
	}()

	established := self.announceTimeout == 0
	nextStartTime := server.NowUtc()
	for {
		statsStartTime := nextStartTime
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.SyncConnectionTimeout):
		}
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

		if established {
			changedCount, currentProvideModes := model.GetProvideKeyChanges(self.ctx, self.clientId, statsStartTime)
			provideEnabled := currentProvideModes[model.ProvideModePublic]

			nextLatency := false
			nextSpeed := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.testConfig.AllowLatency() {
					nextLatency = (self.latencyCount == 0 && self.latencyTest == nil)
					if !nextLatency {
						nextSpeed = (self.speedCount == 0 && self.speedTest == nil)
					}
				} else if self.testConfig.AllowSpeed() {
					nextSpeed = (self.speedCount == 0 && self.speedTest == nil)
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
			established = true
			stats.ConnectionNewCount = 1
		}
		model.AddClientReliabilityStats(
			self.ctx,
			self.networkId,
			self.clientId,
			clientAddressHash,
			statsStartTime,
			stats,
		)
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
}

func (self *ConnectionAnnounce) SendMessage(messageByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.sendMessageCount += 1
	self.sendByteCount += messageByteCount
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
	go func() {
		select {
		case <-self.ctx.Done():
			return
		case self.PendingLatencyTest <- latencyTest:
		}
	}()
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
	nextLatency := false
	nextSpeed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.latencyTest != nil && self.latencyTest.TestId == latencyTest.TestId {
			latencyMilliseconds := uint64((time.Now().Sub(self.latencyTestSendTime) + time.Millisecond/2) / time.Millisecond)

			glog.Infof("[ta][%s]latency %dms\n", self.clientId, latencyMilliseconds)

			self.latencyCount += 1
			if self.latencyCount == 1 || latencyMilliseconds < self.minLatencyMilliseconds {
				self.minLatencyMilliseconds = latencyMilliseconds
			}

			self.latencyTest = nil

			nextLatency = self.latencyCount < self.settings.MaxLatencyCount
			if !nextLatency {
				self.setLatencyWithLock()
				if self.testConfig.AllowSpeed() {
					nextSpeed = (self.speedCount == 0 && self.speedTest == nil)
				}
			}

			success = true
		}
	}()
	if nextLatency {
		self.nextLatency()
	}
	if nextSpeed {
		self.nextSpeed()
	}
	return
}

func (self *ConnectionAnnounce) setLatencyWithLock() {
	if 0 < self.latencyCount && self.connectionId != nil {
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
				INSERT INTO network_client_latency (
					connection_id,
					latency_ms
				)
				VALUES ($1, $2)
				ON CONFLICT (connection_id) DO UPDATE
				SET
					latency_ms = LEAST(network_client_latency.latency_ms, $2)
				`,
				*self.connectionId,
				self.minLatencyMilliseconds,
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
	go func() {
		select {
		case <-self.ctx.Done():
			return
		case self.PendingSpeedTest <- speedTest:
		}
	}()
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
	nextSpeed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.speedTest != nil && self.speedTest.TestId == speedTest.TestId && self.speedTest.TotalByteCount == speedTest.TotalByteCount {

			bytesPerSecond := (1000 * speedTest.TotalByteCount) / model.ByteCount(time.Now().Sub(self.speedTestSendTime)/time.Millisecond)

			glog.Infof("[ta][%s]speed %.2fmib/s\n", self.clientId, float64(bytesPerSecond)/float64(1024*1024))

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
	if 0 < self.speedCount && self.connectionId != nil {
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
				INSERT INTO network_client_speed (
					connection_id,
					bytes_per_second
				)
				VALUES ($1, $2)
				ON CONFLICT (connection_id) DO UPDATE
				SET
					bytes_per_second = GREATEST(network_client_speed.bytes_per_second, $2)
				`,
				*self.connectionId,
				self.maxBytesPerSecond,
			))
		})
	}

}
