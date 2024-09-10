package client

import (
	"sync"
	"time"
)

type DeviceStats struct {
	stateLock                 sync.Mutex
	connectEnabled            bool
	connectStartTime          time.Time
	connectCount              int
	netConnectDuration        time.Duration
	maxConnectDuration        time.Duration
	netRemoteSendByteCount    ByteCount
	netRemoteReceiveByteCount ByteCount
}

func newDeviceStats() *DeviceStats {
	return &DeviceStats{
		connectEnabled:            false,
		connectStartTime:          time.Time{},
		connectCount:              0,
		netConnectDuration:        time.Duration(0),
		maxConnectDuration:        time.Duration(0),
		netRemoteSendByteCount:    ByteCount(0),
		netRemoteReceiveByteCount: ByteCount(0),
	}
}

func (self *DeviceStats) GetConnectCount() int {
}

func (self *DeviceStats) GetNetConnectDurationSeconds() float32 {
}

func (self *DeviceStats) GetUserSuccess() bool {
	// consider the user successful when all of
	// - max connect time is longer than 30m
	// - receive byte count more than 64mib

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	connectTimeCondition := 30*time.Second <= self.maxConnectDuration
	receiveByteCountCondition := ByteCount(64*1024*1024) <= self.netRemoteReceiveByteCount
	return connectTimeCondition && receiveByteCountCondition
}

func (self *DeviceStats) UpdateConnect(connectEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()

	if self.connectEnabled {
		self.connectCount += 1
		connectDuration := now.Sub(self.connectStartTime)
		if self.maxConnectDuration < connectDuration {
			self.maxConnectDuration = connectDuration
		}
		self.netConnectDuration += connectDuration
	}

	if connectEnabled {
		self.connectStartTime = now
		self.connectEnabled = true
	} else {
		self.connectEnabled = false
	}
}

func (self *DeviceStats) UpdateRemoteReceive(remoteReceiveByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.netRemoteReceiveByteCount += remoteReceiveByteCount
}

func (self *DeviceStats) UpdateRemoteSend(remoteSendByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.netRemoteSendByteCount += remoteSendByteCount
}
