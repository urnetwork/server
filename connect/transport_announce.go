package main

import (
	"context"
	// "encoding/hex"
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
		SyncConnectionTimeout: 60 * time.Second,
	}
}

type ConnectionAnnounceSettings struct {
	// FIXME this should be the reliability block size
	SyncConnectionTimeout time.Duration
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
	go server.HandleError(func() {
		announce.run()
	}, cancel)
	return announce
}

func (self *ConnectionAnnounce) run() {
	defer self.cancel()

	if 0 < self.announceTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.announceTimeout):
		}
	}

	connectionId := controller.ConnectNetworkClient(self.ctx, self.clientId, self.clientAddress, self.handlerId)
	defer server.HandleError(func() {
		// note use an uncanceled context for cleanup
		cleanupCtx := context.Background()
		model.DisconnectNetworkClient(cleanupCtx, connectionId)
	}, self.cancel)

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
