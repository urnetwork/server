package perfvar

import (
	"context"
	"fmt"
	"net/netip"

	clientconnect "github.com/urnetwork/connect"
)

const fullTunBridgeFailureBatchLimit = 4

// No payload, packet references, or provider credentials enter diagnostics.
// A native batch has at most 64 members; retain only four failed batches.
type fullTunBridgeAdmissionPacket struct {
	Index    int
	Window   uint64
	Flow     fullTunBridgeFlowKey
	Bytes    clientconnect.ByteCount
	Accepted bool
}

type fullTunBridgeAdmissionBatch struct {
	Id             uint64
	ReturnedCount  int
	AcceptedCount  int
	ResultMismatch bool
	Packets        []fullTunBridgeAdmissionPacket
}

func (key fullTunBridgeFlowKey) String() string {
	if !key.valid {
		return "unavailable"
	}
	source, destination := netip.AddrFrom16(key.sourceIp), netip.AddrFrom16(key.destinationIp)
	if key.ipVersion == 4 {
		source = netip.AddrFrom4([4]byte(key.sourceIp[:4]))
		destination = netip.AddrFrom4([4]byte(key.destinationIp[:4]))
	}
	return fmt.Sprintf("v%d/p%d/%s->%s", key.ipVersion, key.protocol,
		netip.AddrPortFrom(source, key.sourcePort), netip.AddrPortFrom(destination, key.destinationPort))
}

// Retain exact outcomes before terminal publication can wake a failing
// workload. Inconsistent native results fail closed, never infer successes.
func (self *fullTunBridgeSendTracker) recordBatchAdmission(
	entries []*fullTunBridgeSendEntry,
	accepted []bool,
	returnedCount int,
) bool {
	acceptedCount := 0
	for _, admitted := range accepted {
		if admitted {
			acceptedCount++
		}
	}
	consistent := returnedCount == acceptedCount
	batchId := self.admissionBatchId.Add(1)
	if consistent && acceptedCount == len(entries) {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.admissionFailureBatchCount++
	sample := fullTunBridgeAdmissionBatch{
		Id: batchId, ReturnedCount: returnedCount,
		AcceptedCount: acceptedCount, ResultMismatch: !consistent,
		Packets: make([]fullTunBridgeAdmissionPacket, len(entries)),
	}
	for i, entry := range entries {
		sample.Packets[i] = fullTunBridgeAdmissionPacket{
			Index: i, Window: entry.windowId, Flow: entry.flowKey,
			Bytes: entry.packetByteCount, Accepted: accepted[i],
		}
		if !consistent && self.active != nil && self.active.window.id == entry.windowId {
			self.active.invalid = true
		}
	}
	if len(self.admissionFailureBatches) == fullTunBridgeFailureBatchLimit {
		copy(self.admissionFailureBatches, self.admissionFailureBatches[1:])
		self.admissionFailureBatches = self.admissionFailureBatches[:fullTunBridgeFailureBatchLimit-1]
	}
	self.admissionFailureBatches = append(self.admissionFailureBatches, sample)
	return consistent
}

type fullTunBridgeFlowDiagnostic struct {
	WindowMatches         bool
	Invalid               bool
	ObservedPackets       int64
	ObservedBytes         clientconnect.ByteCount
	AcceptedPackets       int
	RejectedPackets       int
	PendingPackets        int
	GlobalFailures        uint64
	OmittedFailureBatches uint64
	RecentFailures        []fullTunBridgeAdmissionBatch
}

func (self *fullTunBridgeSendTracker) flowDiagnostic(window fullTunBridgeFlowWindow) fullTunBridgeFlowDiagnostic {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	diagnostic := fullTunBridgeFlowDiagnostic{
		GlobalFailures:        self.failureCount.Load(),
		RecentFailures:        append([]fullTunBridgeAdmissionBatch(nil), self.admissionFailureBatches...),
		OmittedFailureBatches: self.admissionFailureBatchCount - uint64(len(self.admissionFailureBatches)),
	}
	if self.active == nil || self.active.window != window {
		return diagnostic
	}
	diagnostic.WindowMatches = true
	diagnostic.Invalid = self.active.invalid
	diagnostic.ObservedPackets = self.active.packetCount
	diagnostic.ObservedBytes = self.active.packetByteCount
	for _, entry := range self.active.entries {
		if entry.state.Load() != fullTunBridgeSendEntryTerminal {
			diagnostic.PendingPackets++
		} else if entry.sent.Load() {
			diagnostic.AcceptedPackets++
		} else {
			diagnostic.RejectedPackets++
		}
	}
	return diagnostic
}

// Target, observed source admission and receiver observations are distinct.
// ctx is a liveness bound, not a fabricated explanation for structural failure.
func (self *fullTunBridgeSendTracker) flowFailure(
	ctx context.Context,
	window fullTunBridgeFlowWindow,
	targetPackets int64,
	targetBytes clientconnect.ByteCount,
	boundaryOK bool,
	delivered, duplicate, corrupt int64,
	tcpCollapseDrops uint64,
) error {
	diagnostic := self.flowDiagnostic(window)
	reason := "terminal admission incomplete"
	if diagnostic.RejectedPackets > 0 {
		reason = "terminal admission rejected"
	}
	if !boundaryOK {
		reason = "flow boundary rejected"
	}
	detail := fmt.Sprintf("%s flow=%s target_packets=%d target_bytes=%d source=%+v receiver_delivered=%d receiver_duplicate=%d receiver_corrupt=%d tcp_collapse_drops_global=%d",
		reason, window.flowKey, targetPackets, targetBytes, diagnostic, delivered, duplicate, corrupt, tcpCollapseDrops)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", detail, err)
	}
	return fmt.Errorf("%s", detail)
}
