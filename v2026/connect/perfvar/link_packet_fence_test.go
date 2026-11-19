package perfvar

import (
	"context"
	"fmt"
	"time"
)

// A fence is an exact bounded ownership epoch, not an idle heuristic. Original
// submissions registered before capture and every duplicate derived from them
// retain this identity until their counters and terminal disposition publish.
// All fields are protected by the owning link's stateLock.
type directionalLinkFenceEpoch struct {
	id                  uint64
	pending             int
	startedSubmissions  uint64
	duplicates          uint64
	invalidTerminals    uint64
	sealed              bool
	done                chan struct{}
	capturedAt, endedAt time.Time
}

type directionalLinkPacketFence struct {
	link  *directionalLink
	epoch *directionalLinkFenceEpoch
}

type directionalLinkPacketFenceObservation struct {
	Epoch                  uint64    `json:"epoch"`
	StartedSubmissions     uint64    `json:"started_submissions"`
	Duplicates             uint64    `json:"duplicates"`
	InvalidTerminals       uint64    `json:"invalid_terminals"`
	PendingOwners          int       `json:"pending_owners"`
	CapturedAt             time.Time `json:"captured_at"`
	EndedAt                time.Time `json:"ended_at"`
	PostFenceSubmissions   uint64    `json:"post_fence_submissions"`
	PostFenceQueuedPackets int       `json:"post_fence_queued_packets"`
	PostFenceQueuedBytes   int       `json:"post_fence_queued_bytes"`
}

// Enable only before first admission. Canonical/v4 links keep the nil path and
// their existing global-idle semantics. No per-packet tracker allocation occurs.
func (self *directionalLink) enablePacketFences() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.fenceCurrent != nil || self.closed || self.activeSubmissionCount != 0 ||
		self.queuedPacketCount != 0 || self.submittedPackets.Load() != 0 {
		return fmt.Errorf("packet fence must be enabled once before first admission")
	}
	self.fenceCurrent = &directionalLinkFenceEpoch{id: 1, done: make(chan struct{})}
	return nil
}

func (self *directionalLink) finishFenceOwnerWithLock(epoch *directionalLinkFenceEpoch) {
	if epoch == nil {
		return
	}
	epoch.pending--
	if epoch.pending < 0 {
		panic("physical packet fence ownership became negative")
	}
	if epoch.pending == 0 && epoch.sealed {
		epoch.endedAt = time.Now()
		close(epoch.done)
	}
}

func (self *directionalLink) invalidateFence(epoch *directionalLinkFenceEpoch) {
	if epoch != nil {
		self.stateLock.Lock()
		epoch.invalidTerminals++
		self.stateLock.Unlock()
	}
}

// At most one captured unfinished epoch and one live epoch exist per link.
// Repeated/concurrent captures cannot accumulate an unbounded fence chain.
func (self *directionalLink) capturePacketFence() (directionalLinkPacketFence, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.fenceCurrent == nil || self.closed {
		return directionalLinkPacketFence{}, fmt.Errorf("physical packet fence unavailable")
	}
	if prior := self.fencePrevious; prior != nil && prior.pending != 0 {
		return directionalLinkPacketFence{}, fmt.Errorf("prior physical packet fence %d is unfinished", prior.id)
	}
	epoch := self.fenceCurrent
	epoch.sealed, epoch.capturedAt = true, time.Now()
	if epoch.pending == 0 {
		epoch.endedAt = epoch.capturedAt
		close(epoch.done)
	}
	self.fencePrevious = epoch
	self.fenceCurrent = &directionalLinkFenceEpoch{id: epoch.id + 1, done: make(chan struct{})}
	return directionalLinkPacketFence{self, epoch}, nil
}

func (self directionalLinkPacketFence) wait(ctx context.Context) (directionalLinkPacketFenceObservation, error) {
	if self.link == nil || self.epoch == nil {
		return directionalLinkPacketFenceObservation{}, fmt.Errorf("nil physical packet fence")
	}
	select {
	case <-ctx.Done():
		return self.snapshot(), ctx.Err()
	case <-self.epoch.done:
	}
	observation := self.snapshot()
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	if observation.InvalidTerminals != 0 || observation.PendingOwners != 0 {
		return observation, fmt.Errorf("physical packet fence %d invalid=%d pending=%d", observation.Epoch, observation.InvalidTerminals, observation.PendingOwners)
	}
	return observation, nil
}

func (self directionalLinkPacketFence) snapshot() directionalLinkPacketFenceObservation {
	self.link.stateLock.Lock()
	defer self.link.stateLock.Unlock()
	epoch := self.epoch
	return directionalLinkPacketFenceObservation{
		Epoch: epoch.id, StartedSubmissions: epoch.startedSubmissions,
		Duplicates: epoch.duplicates, InvalidTerminals: epoch.invalidTerminals,
		PendingOwners: epoch.pending, CapturedAt: epoch.capturedAt, EndedAt: epoch.endedAt,
		PostFenceSubmissions:   self.link.fenceCurrent.startedSubmissions,
		PostFenceQueuedPackets: self.link.queuedPacketCount,
		PostFenceQueuedBytes:   self.link.queuedByteCount,
	}
}

// The network starts with only its edge node. Opt-in before the first link
// ensures every later H1 access/control link inherits exact prefix ownership.
func (self *simulatedIPNetwork) enablePacketFences() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.packetFences || len(self.links) != 0 {
		return fmt.Errorf("network packet fences must be enabled before first link")
	}
	self.packetFences = true
	return nil
}
