package main

import (
	"context"
	"encoding/hex"
	// "fmt"
	"sync"
	"time"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/model"
)

func DefaultConnectionAnnounceSettings() *ConnectionAnnounceSettings {
	return &ConnectionAnnounceSettings{
		SyncConnectionTimeout: 60 * time.Second,
		LocationRetryTimeout:  5 * time.Minute,
	}
}

type ConnectionAnnounceSettings struct {
	// FIXME this should be the reliability block size
	SyncConnectionTimeout time.Duration
	LocationRetryTimeout  time.Duration
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
	settings *ConnectionAnnounceSettings,
) *ConnectionAnnounce {
	announce := &ConnectionAnnounce{
		ctx:             ctx,
		cancel:          cancel,
		networkId:       networkId,
		clientId:        clientId,
		clientAddress:   clientAddress,
		handlerId:       handlerId,
		announceTimeout: announceTimeout,
		settings:        settings,
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
		case <-time.After(self.announceTimeout):
		}
	}

	// FIXME remove ip
	// glog.Infof("[t]announce %s [%s]\n", self.clientId, self.clientAddress)

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

		status := model.GetNetworkClientConnectionStatus(self.ctx, connectionId)
		if err := status.Err(); err != nil {
			glog.Infof("[t][%s]connection err = %s\n", connectionId, err)
			return
		}

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

			// add reliability stats if all of:
			// 1. established provide public (no changes in block)
			// 2. established connection
			// 3. at least one message count
			// OR
			// 1. new connection (this will invalidate the block)
			// OR
			// 1. provide change (this will invalidate the block)

			stats.ConnectionEstablishedCount = 1
			if currentProvideModes[model.ProvideModePublic] {
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
