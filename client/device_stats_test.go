package client

import (
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestDeviceStats(t *testing.T) {
	// create stats
	// open, wait, close
	// open
	// send data
	// receive data
	// wait
	// close
	// close
	// open
	// close

	// test connect count, max connect time, net data

	deviceStats := newDeviceStats()

	assert.Equal(t, deviceStats.GetConnectCount(), 0)
	assert.Equal(t, deviceStats.GetNetConnectDurationSeconds(), 0)
	assert.Equal(t, deviceStats.GetMaxConnectDurationSeconds(), 0)
	assert.Equal(t, deviceStats.GetNetRemoteSendByteCount(), ByteCount(0))
	assert.Equal(t, deviceStats.GetNetRemoteReceiveByteCount(), ByteCount(0))
	assert.Equal(t, deviceStats.GetUserSuccess(), false)

	deviceStats.UpdateConnect(true)
	connectTime1 := time.Now()
	select {
	case <-time.After(1 * time.Second):
	}

	assert.Equal(t, deviceStats.GetConnectCount(), 0)
	assert.Equal(t, deviceStats.GetNetConnectDurationSeconds(), 0)
	assert.Equal(t, deviceStats.GetMaxConnectDurationSeconds(), 0)

	deviceStats.UpdateRemoteSend(ByteCount(1024))
	deviceStats.UpdateRemoteReceive(ByteCount(1024 * 1024))

	deviceStats.UpdateConnect(true)
	connectTime2 := time.Now()

	assert.Equal(t, deviceStats.GetConnectCount(), 1)
	assert.Equal(t, deviceStats.GetNetConnectDurationSeconds(), int(connectTime2.Sub(connectTime1)/time.Second))
	assert.Equal(t, deviceStats.GetMaxConnectDurationSeconds(), int(connectTime2.Sub(connectTime1)/time.Second))
	assert.Equal(t, deviceStats.GetNetRemoteSendByteCount(), ByteCount(1024))
	assert.Equal(t, deviceStats.GetNetRemoteReceiveByteCount(), ByteCount(1024*1024))

	select {
	case <-time.After(2 * time.Second):
	}

	deviceStats.UpdateRemoteSend(ByteCount(2 * 1024))
	deviceStats.UpdateRemoteReceive(ByteCount(2 * 1024 * 1024))

	deviceStats.UpdateConnect(false)
	connectTime3 := time.Now()

	assert.Equal(t, deviceStats.GetConnectCount(), 2)
	assert.Equal(t, deviceStats.GetNetConnectDurationSeconds(), int((connectTime2.Sub(connectTime1)+connectTime3.Sub(connectTime2))/time.Second))
	assert.Equal(t, deviceStats.GetMaxConnectDurationSeconds(), int(connectTime3.Sub(connectTime2)/time.Second))
	assert.Equal(t, deviceStats.GetNetRemoteSendByteCount(), ByteCount(1024)+ByteCount(2*1024))
	assert.Equal(t, deviceStats.GetNetRemoteReceiveByteCount(), ByteCount(1024*1024)+ByteCount(2*1024*1024))

}
