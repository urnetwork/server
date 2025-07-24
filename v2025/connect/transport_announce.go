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
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
	announceTimeout time.Duration,
) *ConnectionAnnounce {
	return NewConnectionAnnounce(
		ctx,
		cancel,
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
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
	announceTimeout time.Duration,
	settings *ConnectionAnnounceSettings,
) *ConnectionAnnounce {
	announce := &ConnectionAnnounce{
		ctx:             ctx,
		cancel:          cancel,
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
		glog.Infof("[t][%s]could not connect client. err = %s\n", hex.EncodeToString(clientAddressHash), err)
		return
	}
	defer server.HandleError(func() {
		// note use an uncanceled context for cleanup
		cleanupCtx := context.Background()
		model.DisconnectNetworkClient(cleanupCtx, connectionId)
		glog.V(1).Infof("[t][%s]disconnect client\n", hex.EncodeToString(clientAddressHash))
	}, self.cancel)

	self.setConnectionId(connectionId)

	// FIXME
	// established := false

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.SyncConnectionTimeout):
		}

		// FIXME use event
		status := model.GetNetworkClientConnectionStatus(self.ctx, connectionId)
		if err := status.Err(); err != nil {
			glog.Infof("[t][%s]connection err = %s\n", connectionId, err)
			return
		}

		// FIXME client_connection_reliability_model.go
		/*
			if established {
				// changeCount, currentProvideModes := model.GetProvideKeyChanges(blockStartTime)

				// FIXME add reliability stats if all of:
				// 1. established provide public (no changes in block)
				// 2. established connection
				// 3. at least one message count
				// OR
				// 1. new connection (this will invalidate the block)
			} else {
				established = true
			}
		*/

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
