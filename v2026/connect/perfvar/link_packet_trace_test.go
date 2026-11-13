package perfvar

import (
	"sync"
	"time"
)

// Diagnostic-only borrowed packet boundaries. The callback must not retain or
// mutate packet bytes. Terminal disposition after an ownership handoff carries
// only its link sequence: the packet may already be in another goroutine.
type linkPacketTraceHook struct {
	join   sync.RWMutex
	closed bool
	call   func(string, linkScheduleObservation, []byte)
}

func (self *directionalLink) installPacketTraceForTest(call func(string, linkScheduleObservation, []byte)) (func(), bool) {
	if call == nil {
		return nil, false
	}
	owner := &linkPacketTraceHook{call: call}
	if !self.packetTraceForTest.CompareAndSwap(nil, owner) {
		return nil, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			owner.join.Lock()
			owner.closed = true
			self.packetTraceForTest.CompareAndSwap(owner, nil)
			owner.join.Unlock()
		})
	}, true
}

func (self *directionalLink) tracePacketForTest(stage string, sequence uint64, packet []byte, observation linkScheduleObservation) {
	owner := self.packetTraceForTest.Load()
	if owner == nil {
		return
	}
	owner.join.RLock()
	defer owner.join.RUnlock()
	defer func() { _ = recover() }()
	if owner.closed {
		return
	}
	observation.sequence = sequence
	if observation.scheduleTime.IsZero() {
		observation.scheduleTime = time.Now()
	}
	owner.call(stage, observation, packet)
}
