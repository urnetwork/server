package main

import (
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestPingTracker(t *testing.T) {
	pingTracker := NewPingTracker(5)

	timeout := pingTracker.MinPingTimeout()
	assert.Equal(t, timeout, time.Duration(0))

	pingTracker.Receive()
	pingTracker.Receive()

	n := 10
	for i := range n {
		select {
		case <-time.After(time.Duration(n-i) * time.Second):
		}

		pingTracker.ReceivePing()

		// round down
		timeout := pingTracker.MinPingTimeout()
		assert.Equal(t, timeout/time.Second, time.Duration(n-i))
	}
}

func TestPingTrackerChaos(t *testing.T) {
	pingTracker := NewPingTracker(5)

	for range 1024 {
		if mathrand.Intn(2) == 0 {
			pingTracker.Receive()
		} else {
			select {
			case <-time.After(time.Duration(mathrand.Intn(32)) * time.Millisecond):
			}
			pingTracker.ReceivePing()
		}
		pingTracker.MinPingTimeout()
	}
}
