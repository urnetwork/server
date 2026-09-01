package proxy

import (
	"errors"
	"testing"

	"github.com/urnetwork/server"
)

type recordingWgProxyDeviceOpener struct {
	proxyID server.Id
	err     error
}

func (o *recordingWgProxyDeviceOpener) OpenProxyDevice(proxyID server.Id) (*ProxyDevice, error) {
	o.proxyID = proxyID
	return nil, o.err
}

func TestWgTunFactoryRetainsProxyIDValueOnly(t *testing.T) {
	expectedID := server.NewId()
	callerID := expectedID
	opener := &recordingWgProxyDeviceOpener{err: errors.New("synthetic open")}
	factory := wgTunFactory(opener, callerID)

	// Reassign the caller's variable after construction. The durable peer
	// closure must own the original value, not a model.ProxyClient pointer or
	// another mutable startup object that happens to contain it.
	reassignedID := server.NewId()
	callerID = reassignedID
	_, err := factory()
	if err != opener.err {
		t.Fatalf("factory error=%v, want %v", err, opener.err)
	}
	if opener.proxyID != expectedID {
		t.Fatalf("factory opened proxy id %s, want captured value %s (caller now %s)", opener.proxyID, expectedID, callerID)
	}
}
