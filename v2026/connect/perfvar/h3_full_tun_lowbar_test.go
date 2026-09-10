// This file measures a complete TCP upload through the production full-TUN,
// DeviceRemote, server/connect, and provider path on a seeded one-bar profile.
package perfvar

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
)

const h3LowBarPacketTrackPayloadByteCount = int64(64 * 1024)

var h3FullTunWireFingerprintTable = crc32.MakeTable(crc32.Castagnoli)

type h3FullTunWireClass struct {
	messageId                        clientconnect.Id
	sequenceNumber                   uint64
	ipFrameCount                     uint64
	toProviderTcpRstCount            uint64
	fromProviderTcpRstCount          uint64
	toProviderTcpFinCount            uint64
	fromProviderTcpFinCount          uint64
	head                             bool
	contract                         bool
	compactContract                  bool
	resend                           bool
	promotedHead                     bool
	compactContractRecoverySupported bool
}

type h3FullTunLaneSnapshot struct {
	RegisteredMessageCount            uint64
	RegisteredIpMessageCount          uint64
	RegisteredIpFrameCount            uint64
	RegisteredToProviderTcpRstCount   uint64
	RegisteredFromProviderTcpRstCount uint64
	RegisteredToProviderTcpFinCount   uint64
	RegisteredFromProviderTcpFinCount uint64
	DecodeErrorCount                  uint64
	DatagramMessageCount              uint64
	DatagramIpMessageCount            uint64
	DatagramIpFrameCount              uint64
	DatagramContractMessageCount      uint64
	StreamMessageCount                uint64
	StreamIpMessageCount              uint64
	StreamIpFrameCount                uint64
	StreamContractMessageCount        uint64
	StreamIpDistinctMessageCount      uint64
	StreamIpRepeatMessageCount        uint64
	StreamIpHeadMessageCount          uint64
	StreamIpFullContractHeadCount     uint64
	StreamIpFullContractNonHeadCount  uint64
	StreamIpCompactHeadCount          uint64
	StreamIpResendCount               uint64
	StreamIpPromotedHeadCount         uint64
	StreamIpRecoverySupportedCount    uint64
	StreamIpPromotedSupportedCount    uint64
	UnclassifiedDatagramCount         uint64
	UnclassifiedStreamCount           uint64
}

func h3FullTunTcpFlags(packet []byte) (rst bool, fin bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 || packet[9] != 6 {
		return false, false
	}
	ipHeaderByteCount := int(packet[0]&0x0f) * 4
	if ipHeaderByteCount < 20 || len(packet) < ipHeaderByteCount+20 {
		return false, false
	}
	flags := packet[ipHeaderByteCount+13]
	return flags&0x04 != 0, flags&0x01 != 0
}

// Correlates the inspectable pre-encryption Transfer frame with the exact wire
// bytes later admitted to an H3 lane. SHA-256 avoids retaining pooled messages;
// the state lock spans only bounded test bookkeeping on sender goroutines.
type h3FullTunLaneTrace struct {
	stateLock sync.Mutex

	wireClassesByDigest           map[[sha256.Size]byte]h3FullTunWireClass
	streamIpSendCountsByMessageId map[clientconnect.Id]uint64
	snapshot                      h3FullTunLaneSnapshot
}

func newH3FullTunLaneTrace() *h3FullTunLaneTrace {
	return &h3FullTunLaneTrace{
		wireClassesByDigest:           map[[sha256.Size]byte]h3FullTunWireClass{},
		streamIpSendCountsByMessageId: map[clientconnect.Id]uint64{},
	}
}

// Accepts both the direct Pack encoding and the legacy nested TransferPack.
func decodeH3FullTunWireClass(
	transferFrameBytes []byte,
) (h3FullTunWireClass, error) {
	var transferFrame protocol.TransferFrame
	if err := clientconnect.ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
		return h3FullTunWireClass{}, err
	}
	pack := transferFrame.GetPack()
	if pack == nil {
		frame := transferFrame.GetFrame()
		if frame == nil || frame.GetMessageType() != protocol.MessageType_TransferPack {
			return h3FullTunWireClass{}, errors.New("Transfer frame has no Pack")
		}
		pack = &protocol.Pack{}
		if err := clientconnect.ProtoUnmarshal(frame.GetMessageBytes(), pack); err != nil {
			return h3FullTunWireClass{}, err
		}
	}
	messageId, err := clientconnect.IdFromBytes(pack.GetMessageId())
	if err != nil {
		return h3FullTunWireClass{}, err
	}
	wireClass := h3FullTunWireClass{
		messageId:       messageId,
		sequenceNumber:  pack.GetSequenceNumber(),
		head:            pack.GetHead(),
		contract:        pack.GetContractFrame() != nil,
		compactContract: pack.GetContractFrame() == nil && 0 < len(pack.GetContractId()),
	}
	for _, frame := range pack.GetFrames() {
		switch frame.GetMessageType() {
		case protocol.MessageType_IpIpPacketToProvider:
			wireClass.ipFrameCount += 1
			rst, fin := h3FullTunTcpFlags(frame.GetMessageBytes())
			if rst {
				wireClass.toProviderTcpRstCount += 1
			}
			if fin {
				wireClass.toProviderTcpFinCount += 1
			}
		case protocol.MessageType_IpIpPacketFromProvider:
			wireClass.ipFrameCount += 1
			rst, fin := h3FullTunTcpFlags(frame.GetMessageBytes())
			if rst {
				wireClass.fromProviderTcpRstCount += 1
			}
			if fin {
				wireClass.fromProviderTcpFinCount += 1
			}
		}
	}
	return wireClass, nil
}

func (self *h3FullTunLaneTrace) register(
	observation clientconnect.TransferWireMessageObservation,
) {
	wireClass, err := decodeH3FullTunWireClass(observation.TransferFrameBytes)
	digest := sha256.Sum256(observation.WireMessageBytes)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.snapshot.RegisteredMessageCount += 1
	if err != nil {
		self.snapshot.DecodeErrorCount += 1
		return
	}
	if 0 < wireClass.ipFrameCount {
		self.snapshot.RegisteredIpMessageCount += 1
		self.snapshot.RegisteredIpFrameCount += wireClass.ipFrameCount
	}
	self.snapshot.RegisteredToProviderTcpRstCount += wireClass.toProviderTcpRstCount
	self.snapshot.RegisteredFromProviderTcpRstCount += wireClass.fromProviderTcpRstCount
	self.snapshot.RegisteredToProviderTcpFinCount += wireClass.toProviderTcpFinCount
	self.snapshot.RegisteredFromProviderTcpFinCount += wireClass.fromProviderTcpFinCount
	wireClass.resend = observation.Resend
	wireClass.promotedHead = observation.PromotedHead
	wireClass.compactContractRecoverySupported =
		observation.CompactContractRecoverySupported
	self.wireClassesByDigest[digest] = wireClass
}

func (self *h3FullTunLaneTrace) observe(message []byte, datagram bool) {
	digest := sha256.Sum256(message)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	wireClass, ok := self.wireClassesByDigest[digest]
	if !ok {
		// ACKs and forwarded traffic bypass SendSequence's plaintext seam. They
		// remain useful aggregate H3 counters but cannot carry a local IP frame.
		if datagram {
			self.snapshot.UnclassifiedDatagramCount += 1
		} else {
			self.snapshot.UnclassifiedStreamCount += 1
		}
		return
	}
	if datagram {
		self.snapshot.DatagramMessageCount += 1
		if 0 < wireClass.ipFrameCount {
			self.snapshot.DatagramIpMessageCount += 1
			self.snapshot.DatagramIpFrameCount += wireClass.ipFrameCount
		}
		if wireClass.contract {
			self.snapshot.DatagramContractMessageCount += 1
		}
		return
	}
	self.snapshot.StreamMessageCount += 1
	if 0 < wireClass.ipFrameCount {
		self.snapshot.StreamIpMessageCount += 1
		self.snapshot.StreamIpFrameCount += wireClass.ipFrameCount
		if self.streamIpSendCountsByMessageId[wireClass.messageId] == 0 {
			self.snapshot.StreamIpDistinctMessageCount += 1
		} else {
			self.snapshot.StreamIpRepeatMessageCount += 1
		}
		self.streamIpSendCountsByMessageId[wireClass.messageId] += 1
		if wireClass.head {
			self.snapshot.StreamIpHeadMessageCount += 1
		}
		if wireClass.contract && wireClass.head {
			self.snapshot.StreamIpFullContractHeadCount += 1
		} else if wireClass.contract {
			self.snapshot.StreamIpFullContractNonHeadCount += 1
		}
		if wireClass.compactContract && wireClass.head {
			self.snapshot.StreamIpCompactHeadCount += 1
		}
		if wireClass.resend {
			self.snapshot.StreamIpResendCount += 1
		}
		if wireClass.promotedHead {
			self.snapshot.StreamIpPromotedHeadCount += 1
		}
		if wireClass.compactContractRecoverySupported {
			self.snapshot.StreamIpRecoverySupportedCount += 1
		}
		if wireClass.promotedHead && wireClass.compactContractRecoverySupported {
			self.snapshot.StreamIpPromotedSupportedCount += 1
		}
	}
	if wireClass.contract {
		self.snapshot.StreamContractMessageCount += 1
	}
}

func (self *h3FullTunLaneTrace) Snapshot() h3FullTunLaneSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.snapshot
}

func subtractH3FullTunLanes(
	start h3FullTunLaneSnapshot,
	end h3FullTunLaneSnapshot,
) h3FullTunLaneSnapshot {
	return h3FullTunLaneSnapshot{
		RegisteredMessageCount:            end.RegisteredMessageCount - start.RegisteredMessageCount,
		RegisteredIpMessageCount:          end.RegisteredIpMessageCount - start.RegisteredIpMessageCount,
		RegisteredIpFrameCount:            end.RegisteredIpFrameCount - start.RegisteredIpFrameCount,
		RegisteredToProviderTcpRstCount:   end.RegisteredToProviderTcpRstCount - start.RegisteredToProviderTcpRstCount,
		RegisteredFromProviderTcpRstCount: end.RegisteredFromProviderTcpRstCount - start.RegisteredFromProviderTcpRstCount,
		RegisteredToProviderTcpFinCount:   end.RegisteredToProviderTcpFinCount - start.RegisteredToProviderTcpFinCount,
		RegisteredFromProviderTcpFinCount: end.RegisteredFromProviderTcpFinCount - start.RegisteredFromProviderTcpFinCount,
		DecodeErrorCount:                  end.DecodeErrorCount - start.DecodeErrorCount,
		DatagramMessageCount:              end.DatagramMessageCount - start.DatagramMessageCount,
		DatagramIpMessageCount:            end.DatagramIpMessageCount - start.DatagramIpMessageCount,
		DatagramIpFrameCount:              end.DatagramIpFrameCount - start.DatagramIpFrameCount,
		DatagramContractMessageCount:      end.DatagramContractMessageCount - start.DatagramContractMessageCount,
		StreamMessageCount:                end.StreamMessageCount - start.StreamMessageCount,
		StreamIpMessageCount:              end.StreamIpMessageCount - start.StreamIpMessageCount,
		StreamIpFrameCount:                end.StreamIpFrameCount - start.StreamIpFrameCount,
		StreamContractMessageCount:        end.StreamContractMessageCount - start.StreamContractMessageCount,
		StreamIpDistinctMessageCount:      end.StreamIpDistinctMessageCount - start.StreamIpDistinctMessageCount,
		StreamIpRepeatMessageCount:        end.StreamIpRepeatMessageCount - start.StreamIpRepeatMessageCount,
		StreamIpHeadMessageCount:          end.StreamIpHeadMessageCount - start.StreamIpHeadMessageCount,
		StreamIpFullContractHeadCount:     end.StreamIpFullContractHeadCount - start.StreamIpFullContractHeadCount,
		StreamIpFullContractNonHeadCount:  end.StreamIpFullContractNonHeadCount - start.StreamIpFullContractNonHeadCount,
		StreamIpCompactHeadCount:          end.StreamIpCompactHeadCount - start.StreamIpCompactHeadCount,
		StreamIpResendCount:               end.StreamIpResendCount - start.StreamIpResendCount,
		StreamIpPromotedHeadCount:         end.StreamIpPromotedHeadCount - start.StreamIpPromotedHeadCount,
		StreamIpRecoverySupportedCount:    end.StreamIpRecoverySupportedCount - start.StreamIpRecoverySupportedCount,
		StreamIpPromotedSupportedCount:    end.StreamIpPromotedSupportedCount - start.StreamIpPromotedSupportedCount,
		UnclassifiedDatagramCount:         end.UnclassifiedDatagramCount - start.UnclassifiedDatagramCount,
		UnclassifiedStreamCount:           end.UnclassifiedStreamCount - start.UnclassifiedStreamCount,
	}
}

// Both endpoint collectors see every measured server-routed message once: the
// sender collector before server/connect and the destination collector after
// it. Keeping them separate also catches asymmetric lane selection.
type h3FullTunDatagramSnapshot struct {
	device   clientconnect.H3DatagramStatsSnapshot
	provider clientconnect.H3DatagramStatsSnapshot
}

type h3FullTunQuicSnapshot struct {
	device   clientconnect.H3QuicPacketStatsSnapshot
	provider clientconnect.H3QuicPacketStatsSnapshot
	server   clientconnect.H3QuicPacketStatsSnapshot
}

type h3FullTunQuicFingerprintSnapshot struct {
	device   clientconnect.H3QuicPacketFingerprintStatsSnapshot
	provider clientconnect.H3QuicPacketFingerprintStatsSnapshot
	server   clientconnect.H3QuicPacketFingerprintStatsSnapshot
}

type h3FullTunQuicFingerprintCorrelation struct {
	MatchedCount   uint64
	UnmatchedCount uint64
}

type h3FullTunWireFingerprintSnapshot struct {
	bySource         map[string]map[uint32]uint64
	unavailableCount uint64
	invalidCount     uint64
}

// This test-only trace computes the same CRC32C as quic-go qlog, but at the
// source TUN boundary after the encrypted QUIC UDP payload enters the IP stack.
// It retains only bounded integer counts and never packet bytes.
type h3FullTunWireFingerprintTrace struct {
	stateLock sync.Mutex
	h3Port    uint16
	bySource  map[string]map[uint32]uint64

	unavailableCount uint64
	invalidCount     uint64
}

func newH3FullTunWireFingerprintTrace(h3Port int) *h3FullTunWireFingerprintTrace {
	return &h3FullTunWireFingerprintTrace{
		h3Port:   uint16(h3Port),
		bySource: map[string]map[uint32]uint64{},
	}
}

func (self *h3FullTunWireFingerprintTrace) observe(sourceNode string, packet []byte) {
	if len(packet) < 28 || packet[0]>>4 != 4 || packet[9] != 17 {
		return
	}
	ipHeaderByteCount := int(packet[0]&0x0f) * 4
	totalByteCount := int(binary.BigEndian.Uint16(packet[2:4]))
	if ipHeaderByteCount < 20 || totalByteCount < ipHeaderByteCount+8 ||
		len(packet) < totalByteCount || binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		self.stateLock.Lock()
		self.invalidCount += 1
		self.stateLock.Unlock()
		return
	}
	udp := packet[ipHeaderByteCount:totalByteCount]
	udpByteCount := int(binary.BigEndian.Uint16(udp[4:6]))
	if binary.BigEndian.Uint16(udp[2:4]) != self.h3Port ||
		udpByteCount < 8 || len(udp) < udpByteCount {
		return
	}
	checksum := crc32.Checksum(udp[8:udpByteCount], h3FullTunWireFingerprintTable)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if checksum == 0 {
		self.unavailableCount += 1
		return
	}
	counts := self.bySource[sourceNode]
	if counts == nil {
		counts = map[uint32]uint64{}
		self.bySource[sourceNode] = counts
	}
	counts[checksum] += 1
}

func (self *h3FullTunWireFingerprintTrace) Snapshot() h3FullTunWireFingerprintSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	result := h3FullTunWireFingerprintSnapshot{
		bySource:         map[string]map[uint32]uint64{},
		unavailableCount: self.unavailableCount,
		invalidCount:     self.invalidCount,
	}
	for source, values := range self.bySource {
		result.bySource[source] = cloneH3FullTunQuicFingerprintCounts(values)
	}
	return result
}

func cloneH3FullTunQuicFingerprintCounts(values map[uint32]uint64) map[uint32]uint64 {
	result := make(map[uint32]uint64, len(values))
	for checksum, count := range values {
		result[checksum] = count
	}
	return result
}

func subtractH3FullTunWireFingerprints(
	start h3FullTunWireFingerprintSnapshot,
	end h3FullTunWireFingerprintSnapshot,
) h3FullTunWireFingerprintSnapshot {
	result := h3FullTunWireFingerprintSnapshot{
		bySource:         map[string]map[uint32]uint64{},
		unavailableCount: end.unavailableCount - start.unavailableCount,
		invalidCount:     end.invalidCount - start.invalidCount,
	}
	for source, counts := range end.bySource {
		result.bySource[source] = subtractH3FullTunQuicFingerprintCounts(
			start.bySource[source],
			counts,
		)
	}
	return result
}

func mergeH3FullTunWireFingerprintSources(
	snapshot h3FullTunWireFingerprintSnapshot,
) map[uint32]uint64 {
	values := make([]map[uint32]uint64, 0, len(snapshot.bySource))
	for _, counts := range snapshot.bySource {
		values = append(values, counts)
	}
	return mergeH3FullTunQuicFingerprintCounts(values...)
}

func subtractH3FullTunQuicFingerprintCounts(
	start map[uint32]uint64,
	end map[uint32]uint64,
) map[uint32]uint64 {
	result := map[uint32]uint64{}
	for checksum, endCount := range end {
		if startCount := start[checksum]; startCount < endCount {
			result[checksum] = endCount - startCount
		}
	}
	return result
}

func subtractH3FullTunQuicFingerprints(
	start clientconnect.H3QuicPacketFingerprintStatsSnapshot,
	end clientconnect.H3QuicPacketFingerprintStatsSnapshot,
) clientconnect.H3QuicPacketFingerprintStatsSnapshot {
	return clientconnect.H3QuicPacketFingerprintStatsSnapshot{
		Sent: subtractH3FullTunQuicFingerprintCounts(start.Sent, end.Sent),
		Received: subtractH3FullTunQuicFingerprintCounts(
			start.Received,
			end.Received,
		),
		DroppedPayloadDecrypt: subtractH3FullTunQuicFingerprintCounts(
			start.DroppedPayloadDecrypt,
			end.DroppedPayloadDecrypt,
		),
		RefusedFingerprintCount: end.RefusedFingerprintCount - start.RefusedFingerprintCount,
		UnavailableCount:        end.UnavailableCount - start.UnavailableCount,
	}
}

func mergeH3FullTunQuicFingerprintCounts(values ...map[uint32]uint64) map[uint32]uint64 {
	result := map[uint32]uint64{}
	for _, counts := range values {
		for checksum, count := range counts {
			result[checksum] += count
		}
	}
	return result
}

func correlateH3FullTunQuicFingerprints(
	sent map[uint32]uint64,
	observed map[uint32]uint64,
) h3FullTunQuicFingerprintCorrelation {
	result := h3FullTunQuicFingerprintCorrelation{}
	for checksum, observedCount := range observed {
		matchedCount := min(observedCount, sent[checksum])
		result.MatchedCount += matchedCount
		result.UnmatchedCount += observedCount - matchedCount
	}
	return result
}

func subtractH3FullTunQuic(
	start clientconnect.H3QuicPacketStatsSnapshot,
	end clientconnect.H3QuicPacketStatsSnapshot,
) clientconnect.H3QuicPacketStatsSnapshot {
	return clientconnect.H3QuicPacketStatsSnapshot{
		ConnectionCount:             end.ConnectionCount - start.ConnectionCount,
		ClosedConnectionCount:       end.ClosedConnectionCount - start.ClosedConnectionCount,
		SentPacketCount:             end.SentPacketCount - start.SentPacketCount,
		SentPacketByteCount:         end.SentPacketByteCount - start.SentPacketByteCount,
		SentDatagramPacketCount:     end.SentDatagramPacketCount - start.SentDatagramPacketCount,
		SentDatagramFrameCount:      end.SentDatagramFrameCount - start.SentDatagramFrameCount,
		SentDatagramByteCount:       end.SentDatagramByteCount - start.SentDatagramByteCount,
		ReceivedPacketCount:         end.ReceivedPacketCount - start.ReceivedPacketCount,
		ReceivedPacketByteCount:     end.ReceivedPacketByteCount - start.ReceivedPacketByteCount,
		ReceivedDatagramPacketCount: end.ReceivedDatagramPacketCount - start.ReceivedDatagramPacketCount,
		ReceivedDatagramFrameCount:  end.ReceivedDatagramFrameCount - start.ReceivedDatagramFrameCount,
		ReceivedDatagramByteCount:   end.ReceivedDatagramByteCount - start.ReceivedDatagramByteCount,
		DroppedPacketCount:          end.DroppedPacketCount - start.DroppedPacketCount,
		DroppedPacketByteCount: end.DroppedPacketByteCount -
			start.DroppedPacketByteCount,
		DroppedDosPreventionPacketCount: end.DroppedDosPreventionPacketCount -
			start.DroppedDosPreventionPacketCount,
		DroppedDuplicatePacketCount: end.DroppedDuplicatePacketCount -
			start.DroppedDuplicatePacketCount,
		DroppedOtherPacketCount: end.DroppedOtherPacketCount -
			start.DroppedOtherPacketCount,
		DroppedKeyUnavailablePacketCount: end.DroppedKeyUnavailablePacketCount -
			start.DroppedKeyUnavailablePacketCount,
		DroppedUnknownConnectionIdPacketCount: end.DroppedUnknownConnectionIdPacketCount -
			start.DroppedUnknownConnectionIdPacketCount,
		DroppedHeaderParseErrorPacketCount: end.DroppedHeaderParseErrorPacketCount -
			start.DroppedHeaderParseErrorPacketCount,
		DroppedPayloadDecryptErrorPacketCount: end.DroppedPayloadDecryptErrorPacketCount -
			start.DroppedPayloadDecryptErrorPacketCount,
		DroppedProtocolViolationPacketCount: end.DroppedProtocolViolationPacketCount -
			start.DroppedProtocolViolationPacketCount,
		DroppedUnsupportedVersionPacketCount: end.DroppedUnsupportedVersionPacketCount -
			start.DroppedUnsupportedVersionPacketCount,
		DroppedUnexpectedPacketCount: end.DroppedUnexpectedPacketCount -
			start.DroppedUnexpectedPacketCount,
		DroppedUnexpectedSourceConnectionIdPacketCount: end.DroppedUnexpectedSourceConnectionIdPacketCount -
			start.DroppedUnexpectedSourceConnectionIdPacketCount,
		DroppedUnexpectedVersionPacketCount: end.DroppedUnexpectedVersionPacketCount -
			start.DroppedUnexpectedVersionPacketCount,
		DroppedPayloadDecryptBeforeKeyUpdateCount: end.DroppedPayloadDecryptBeforeKeyUpdateCount -
			start.DroppedPayloadDecryptBeforeKeyUpdateCount,
		DroppedPayloadDecryptAfterKeyUpdateCount: end.DroppedPayloadDecryptAfterKeyUpdateCount -
			start.DroppedPayloadDecryptAfterKeyUpdateCount,
		LocalKeyUpdateCount:  end.LocalKeyUpdateCount - start.LocalKeyUpdateCount,
		RemoteKeyUpdateCount: end.RemoteKeyUpdateCount - start.RemoteKeyUpdateCount,
		KeyDiscardCount:      end.KeyDiscardCount - start.KeyDiscardCount,
		LostPacketCount:      end.LostPacketCount - start.LostPacketCount,
		MtuUpdateCount:       end.MtuUpdateCount - start.MtuUpdateCount,
		CurrentMtu:           end.CurrentMtu,
	}
}

type h3FullTunClientSnapshot struct {
	deviceClient     *clientconnect.Client
	deviceClientId   clientconnect.Id
	deviceClientFlow int
	deviceSource     string
	deviceRecovery   clientconnect.ClientSendRecoveryStatsSnapshot
	providerRecovery clientconnect.ClientSendRecoveryStatsSnapshot
	deviceReceive    clientconnect.ClientReceiveStatsSnapshot
	providerReceive  clientconnect.ClientReceiveStatsSnapshot
}

type h3FullTunTcpSnapshot struct {
	Retransmits         uint64
	Timeouts            uint64
	FastRetransmit      uint64
	EstablishedResets   uint64
	EstablishedTimedout uint64
	ResetsSent          uint64
	ResetsReceived      uint64
}

func snapshotH3FullTunTcp(path *fullTunPath) h3FullTunTcpSnapshot {
	tcp := path.appTun.Stats().TCP
	return h3FullTunTcpSnapshot{
		Retransmits:         tcp.Retransmits.Value(),
		Timeouts:            tcp.Timeouts.Value(),
		FastRetransmit:      tcp.FastRetransmit.Value(),
		EstablishedResets:   tcp.EstablishedResets.Value(),
		EstablishedTimedout: tcp.EstablishedTimedout.Value(),
		ResetsSent:          tcp.ResetsSent.Value(),
		ResetsReceived:      tcp.ResetsReceived.Value(),
	}
}

func subtractH3FullTunTcp(
	start h3FullTunTcpSnapshot,
	end h3FullTunTcpSnapshot,
) h3FullTunTcpSnapshot {
	return h3FullTunTcpSnapshot{
		Retransmits:         end.Retransmits - start.Retransmits,
		Timeouts:            end.Timeouts - start.Timeouts,
		FastRetransmit:      end.FastRetransmit - start.FastRetransmit,
		EstablishedResets:   end.EstablishedResets - start.EstablishedResets,
		EstablishedTimedout: end.EstablishedTimedout - start.EstablishedTimedout,
		ResetsSent:          end.ResetsSent - start.ResetsSent,
		ResetsReceived:      end.ResetsReceived - start.ResetsReceived,
	}
}

func subtractH3FullTunProviderCongestion(
	start clientconnect.ProviderCongestionDrops,
	end clientconnect.ProviderCongestionDrops,
) clientconnect.ProviderCongestionDrops {
	return clientconnect.ProviderCongestionDrops{
		IngressNatPacketCount:  end.IngressNatPacketCount - start.IngressNatPacketCount,
		IngressNatByteCount:    end.IngressNatByteCount - start.IngressNatByteCount,
		ReturnQueuePacketCount: end.ReturnQueuePacketCount - start.ReturnQueuePacketCount,
		ReturnQueueByteCount:   end.ReturnQueueByteCount - start.ReturnQueueByteCount,
		ReturnSendPacketCount:  end.ReturnSendPacketCount - start.ReturnSendPacketCount,
		ReturnSendByteCount:    end.ReturnSendByteCount - start.ReturnSendByteCount,
	}
}

// The generated-client pointer is ordered by identity for replacement
// fencing; it is not a statement about which exit owns a flow. Resolve the
// active exit reported by MultiClient and map its ClientId back to the
// fixture's append-only generated-client owner.
func activeH3FullTunDeviceClient(
	path *fullTunPath,
) (*clientconnect.Client, clientconnect.Id, int, string) {
	if path.multiClient != nil && path.deviceTransports != nil {
		var active *clientconnect.ExitInfo
		for _, exit := range path.multiClient.Exits() {
			if exit.FlowCount <= 0 {
				continue
			}
			if active == nil || active.FlowCount < exit.FlowCount ||
				(active.FlowCount == exit.FlowCount && exit.ClientId.Cmp(active.ClientId) < 0) {
				active = exit
			}
		}
		if active != nil {
			if client := path.deviceTransports.clientById(active.ClientId); client != nil {
				return client, active.ClientId, active.FlowCount, "flow"
			}
		}
	}
	if path.deviceClient != nil {
		if client := path.deviceClient.Load(); client != nil {
			return client, client.ClientId(), 0, "newest-fallback"
		}
	}
	return nil, clientconnect.Id{}, 0, "missing"
}

type h3FullTunDatagramObservation struct {
	SentMessageCount                             uint64
	SentMessageByteCount                         uint64
	SentFragmentCount                            uint64
	SentByteCount                                uint64
	SendErrorCount                               uint64
	ReceivedMessageCount                         uint64
	ReceivedMessageByteCount                     uint64
	ReceivedFragmentCount                        uint64
	ReceivedByteCount                            uint64
	DuplicateFragmentCount                       uint64
	MalformedFragmentCount                       uint64
	ChecksumFailureCount                         uint64
	ReassemblyTimeoutCount                       uint64
	ReassemblyLimitCount                         uint64
	StreamSentMessageCount                       uint64
	StreamSentMessageByteCount                   uint64
	StreamReceivedMessageCount                   uint64
	StreamReceivedMessageByteCount               uint64
	HybridStreamQueueCurrentMessageCount         uint64
	HybridStreamQueueCurrentByteCount            uint64
	HybridStreamQueueLifetimeMaximumMessageCount uint64
	HybridStreamQueueLifetimeMaximumByteCount    uint64
	HybridStreamQueueWaitCount                   uint64
	HybridStreamQueueWaitDuration                time.Duration
	HybridStreamQueueOversizeCount               uint64
}

func snapshotH3FullTunDatagrams(path *fullTunPath) h3FullTunDatagramSnapshot {
	return h3FullTunDatagramSnapshot{
		device:   path.deviceH3DatagramStats.Snapshot(),
		provider: path.providerH3DatagramStats.Snapshot(),
	}
}

func snapshotH3FullTunClients(path *fullTunPath) h3FullTunClientSnapshot {
	snapshot := h3FullTunClientSnapshot{}
	deviceClient, deviceClientId, deviceClientFlow, deviceSource :=
		activeH3FullTunDeviceClient(path)
	snapshot.deviceClient = deviceClient
	snapshot.deviceClientId = deviceClientId
	snapshot.deviceClientFlow = deviceClientFlow
	snapshot.deviceSource = deviceSource
	if deviceClient != nil {
		snapshot.deviceRecovery = deviceClient.SendRecoveryStats()
		snapshot.deviceReceive = deviceClient.ReceiveStats()
	}
	if path.providerClient != nil {
		snapshot.providerRecovery = path.providerClient.SendRecoveryStats()
		snapshot.providerReceive = path.providerClient.ReceiveStats()
	}
	return snapshot
}

func subtractH3FullTunRecovery(
	start clientconnect.ClientSendRecoveryStatsSnapshot,
	end clientconnect.ClientSendRecoveryStatsSnapshot,
) clientconnect.ClientSendRecoveryStatsSnapshot {
	return clientconnect.ClientSendRecoveryStatsSnapshot{
		TimeoutResendWriteCount:               end.TimeoutResendWriteCount - start.TimeoutResendWriteCount,
		CarrierChangeWriteCount:               end.CarrierChangeWriteCount - start.CarrierChangeWriteCount,
		SelectiveGapWriteCount:                end.SelectiveGapWriteCount - start.SelectiveGapWriteCount,
		AckTailProbeWriteCount:                end.AckTailProbeWriteCount - start.AckTailProbeWriteCount,
		CumulativeProbeWriteCount:             end.CumulativeProbeWriteCount - start.CumulativeProbeWriteCount,
		RecoveryWriteErrorCount:               end.RecoveryWriteErrorCount - start.RecoveryWriteErrorCount,
		MissingContractWriteCount:             end.MissingContractWriteCount - start.MissingContractWriteCount,
		MissingContractRequestCount:           end.MissingContractRequestCount - start.MissingContractRequestCount,
		CompactRecoveryAckCount:               end.CompactRecoveryAckCount - start.CompactRecoveryAckCount,
		CompactRecoveryContractCount:          end.CompactRecoveryContractCount - start.CompactRecoveryContractCount,
		UnreliableFlowIsolationBypassCount:    end.UnreliableFlowIsolationBypassCount - start.UnreliableFlowIsolationBypassCount,
		UnreliableNoAckAdmissionBypassCount:   end.UnreliableNoAckAdmissionBypassCount - start.UnreliableNoAckAdmissionBypassCount,
		UnreliableFlowReserveSelectionCount:   end.UnreliableFlowReserveSelectionCount - start.UnreliableFlowReserveSelectionCount,
		UnreliableFlowReserveUseCount:         end.UnreliableFlowReserveUseCount - start.UnreliableFlowReserveUseCount,
		UnreliableFlightWaitCount:             end.UnreliableFlightWaitCount - start.UnreliableFlightWaitCount,
		UnreliableFlightWaitDuration:          end.UnreliableFlightWaitDuration - start.UnreliableFlightWaitDuration,
		UnreliableFlightMaximumWaitDuration:   end.UnreliableFlightMaximumWaitDuration,
		UnreliableFlightGapCount:              end.UnreliableFlightGapCount - start.UnreliableFlightGapCount,
		UnreliableFlightTimeoutCount:          end.UnreliableFlightTimeoutCount - start.UnreliableFlightTimeoutCount,
		UnreliableFlightReductionCount:        end.UnreliableFlightReductionCount - start.UnreliableFlightReductionCount,
		UnreliableFlightMaximumByteCount:      end.UnreliableFlightMaximumByteCount,
		UnreliableFlightMaximumLimitByteCount: end.UnreliableFlightMaximumLimitByteCount,
		UnreliableFlightMaximumMessageCount:   end.UnreliableFlightMaximumMessageCount,
		UnreliableFlightMaximumMessageLimit:   end.UnreliableFlightMaximumMessageLimit,
	}
}

func subtractH3FullTunReceive(
	start clientconnect.ClientReceiveStatsSnapshot,
	end clientconnect.ClientReceiveStatsSnapshot,
) clientconnect.ClientReceiveStatsSnapshot {
	return clientconnect.ClientReceiveStatsSnapshot{
		PackHandoffDropCount:     end.PackHandoffDropCount - start.PackHandoffDropCount,
		PackHandoffDropByteCount: end.PackHandoffDropByteCount - start.PackHandoffDropByteCount,
		AckHandoffDropCount:      end.AckHandoffDropCount - start.AckHandoffDropCount,
	}
}

// Lifetime counters are reduced at the post-handshake application boundary so
// QUIC authentication and route setup cannot satisfy the packet-track gate.
func observeH3FullTunDatagrams(
	start h3FullTunDatagramSnapshot,
	end h3FullTunDatagramSnapshot,
) h3FullTunDatagramObservation {
	device := subtractH3FullTunDatagrams(start.device, end.device)
	provider := subtractH3FullTunDatagrams(start.provider, end.provider)
	return h3FullTunDatagramObservation{
		SentMessageCount:               device.SentMessageCount + provider.SentMessageCount,
		SentMessageByteCount:           device.SentMessageByteCount + provider.SentMessageByteCount,
		SentFragmentCount:              device.SentFragmentCount + provider.SentFragmentCount,
		SentByteCount:                  device.SentByteCount + provider.SentByteCount,
		SendErrorCount:                 device.SendErrorCount + provider.SendErrorCount,
		ReceivedMessageCount:           device.ReceivedMessageCount + provider.ReceivedMessageCount,
		ReceivedMessageByteCount:       device.ReceivedMessageByteCount + provider.ReceivedMessageByteCount,
		ReceivedFragmentCount:          device.ReceivedFragmentCount + provider.ReceivedFragmentCount,
		ReceivedByteCount:              device.ReceivedByteCount + provider.ReceivedByteCount,
		DuplicateFragmentCount:         device.DuplicateFragmentCount + provider.DuplicateFragmentCount,
		MalformedFragmentCount:         device.MalformedFragmentCount + provider.MalformedFragmentCount,
		ChecksumFailureCount:           device.ChecksumFailureCount + provider.ChecksumFailureCount,
		ReassemblyTimeoutCount:         device.ReassemblyTimeoutCount + provider.ReassemblyTimeoutCount,
		ReassemblyLimitCount:           device.ReassemblyLimitCount + provider.ReassemblyLimitCount,
		StreamSentMessageCount:         device.StreamSentMessageCount + provider.StreamSentMessageCount,
		StreamSentMessageByteCount:     device.StreamSentMessageByteCount + provider.StreamSentMessageByteCount,
		StreamReceivedMessageCount:     device.StreamReceivedMessageCount + provider.StreamReceivedMessageCount,
		StreamReceivedMessageByteCount: device.StreamReceivedMessageByteCount + provider.StreamReceivedMessageByteCount,
		HybridStreamQueueCurrentMessageCount: device.HybridStreamQueueCurrentMessageCount +
			provider.HybridStreamQueueCurrentMessageCount,
		HybridStreamQueueCurrentByteCount: device.HybridStreamQueueCurrentByteCount +
			provider.HybridStreamQueueCurrentByteCount,
		HybridStreamQueueLifetimeMaximumMessageCount: device.HybridStreamQueueLifetimeMaximumMessageCount +
			provider.HybridStreamQueueLifetimeMaximumMessageCount,
		HybridStreamQueueLifetimeMaximumByteCount: device.HybridStreamQueueLifetimeMaximumByteCount +
			provider.HybridStreamQueueLifetimeMaximumByteCount,
		HybridStreamQueueWaitCount: device.HybridStreamQueueWaitCount +
			provider.HybridStreamQueueWaitCount,
		HybridStreamQueueWaitDuration: device.HybridStreamQueueWaitDuration +
			provider.HybridStreamQueueWaitDuration,
		HybridStreamQueueOversizeCount: device.HybridStreamQueueOversizeCount +
			provider.HybridStreamQueueOversizeCount,
	}
}

func subtractH3FullTunDatagrams(
	start clientconnect.H3DatagramStatsSnapshot,
	end clientconnect.H3DatagramStatsSnapshot,
) h3FullTunDatagramObservation {
	return h3FullTunDatagramObservation{
		SentMessageCount:                             end.SentMessageCount - start.SentMessageCount,
		SentMessageByteCount:                         end.SentMessageByteCount - start.SentMessageByteCount,
		SentFragmentCount:                            end.SentFragmentCount - start.SentFragmentCount,
		SentByteCount:                                end.SentByteCount - start.SentByteCount,
		SendErrorCount:                               end.SendErrorCount - start.SendErrorCount,
		ReceivedMessageCount:                         end.ReceivedMessageCount - start.ReceivedMessageCount,
		ReceivedMessageByteCount:                     end.ReceivedMessageByteCount - start.ReceivedMessageByteCount,
		ReceivedFragmentCount:                        end.ReceivedFragmentCount - start.ReceivedFragmentCount,
		ReceivedByteCount:                            end.ReceivedByteCount - start.ReceivedByteCount,
		DuplicateFragmentCount:                       end.DuplicateFragmentCount - start.DuplicateFragmentCount,
		MalformedFragmentCount:                       end.MalformedFragmentCount - start.MalformedFragmentCount,
		ChecksumFailureCount:                         end.ChecksumFailureCount - start.ChecksumFailureCount,
		ReassemblyTimeoutCount:                       end.ReassemblyTimeoutCount - start.ReassemblyTimeoutCount,
		ReassemblyLimitCount:                         end.ReassemblyLimitCount - start.ReassemblyLimitCount,
		StreamSentMessageCount:                       end.StreamSentMessageCount - start.StreamSentMessageCount,
		StreamSentMessageByteCount:                   end.StreamSentMessageByteCount - start.StreamSentMessageByteCount,
		StreamReceivedMessageCount:                   end.StreamReceivedMessageCount - start.StreamReceivedMessageCount,
		StreamReceivedMessageByteCount:               end.StreamReceivedMessageByteCount - start.StreamReceivedMessageByteCount,
		HybridStreamQueueCurrentMessageCount:         end.HybridStreamQueueCurrentMessageCount,
		HybridStreamQueueCurrentByteCount:            end.HybridStreamQueueCurrentByteCount,
		HybridStreamQueueLifetimeMaximumMessageCount: end.HybridStreamQueueMaximumMessageCount,
		HybridStreamQueueLifetimeMaximumByteCount:    end.HybridStreamQueueMaximumByteCount,
		HybridStreamQueueWaitCount: end.HybridStreamQueueWaitCount -
			start.HybridStreamQueueWaitCount,
		HybridStreamQueueWaitDuration: end.HybridStreamQueueWaitDuration -
			start.HybridStreamQueueWaitDuration,
		HybridStreamQueueOversizeCount: end.HybridStreamQueueOversizeCount -
			start.HybridStreamQueueOversizeCount,
	}
}

func TestSubtractH3FullTunDatagramsPreservesQueueGaugesAndIntervals(t *testing.T) {
	start := clientconnect.H3DatagramStatsSnapshot{
		HybridStreamQueueCurrentMessageCount: 1,
		HybridStreamQueueCurrentByteCount:    1024,
		HybridStreamQueueMaximumMessageCount: 2,
		HybridStreamQueueMaximumByteCount:    2048,
		HybridStreamQueueWaitCount:           3,
		HybridStreamQueueWaitDuration:        4 * time.Millisecond,
		HybridStreamQueueOversizeCount:       5,
	}
	end := clientconnect.H3DatagramStatsSnapshot{
		HybridStreamQueueCurrentMessageCount: 2,
		HybridStreamQueueCurrentByteCount:    3072,
		HybridStreamQueueMaximumMessageCount: 4,
		HybridStreamQueueMaximumByteCount:    4096,
		HybridStreamQueueWaitCount:           9,
		HybridStreamQueueWaitDuration:        11 * time.Millisecond,
		HybridStreamQueueOversizeCount:       8,
	}
	observation := subtractH3FullTunDatagrams(start, end)
	if observation.HybridStreamQueueCurrentMessageCount != 2 ||
		observation.HybridStreamQueueCurrentByteCount != 3072 ||
		observation.HybridStreamQueueLifetimeMaximumMessageCount != 4 ||
		observation.HybridStreamQueueLifetimeMaximumByteCount != 4096 ||
		observation.HybridStreamQueueWaitCount != 6 ||
		observation.HybridStreamQueueWaitDuration != 7*time.Millisecond ||
		observation.HybridStreamQueueOversizeCount != 3 {
		t.Fatalf("hybrid stream queue interval=%+v", observation)
	}
}

func h3LowBarDropCounts(carrier perfvarCarrierObservation) (loss uint64, queue uint64, mtu uint64) {
	for _, link := range carrier.Links {
		loss += link.LossDropPacketCount
		queue += link.QueueDropPacketCount
		mtu += link.MtuDropPacketCount
	}
	return
}

func requireH3FullTunPacketTrack(
	t testing.TB,
	observation h3FullTunDatagramObservation,
	lanes h3FullTunLaneSnapshot,
) {
	t.Helper()
	if observation.SentMessageCount == 0 || observation.ReceivedMessageCount == 0 {
		t.Fatalf("measured H3 packet track was idle: %+v", observation)
	}
	// Correlation uses the exact encrypted bytes delivered to H3, so this gate
	// classifies actual physical sends rather than inferring contents from a
	// contract's variable serialized size.
	if lanes.DecodeErrorCount != 0 || lanes.DatagramIpMessageCount == 0 ||
		lanes.DatagramIpFrameCount == 0 {
		t.Fatalf("measured H3 packet lane had no classified IP traffic: %+v", lanes)
	}
	if lanes.StreamIpMessageCount != 0 || lanes.StreamIpFrameCount != 0 {
		t.Fatalf("tunneled IP traffic leaked onto H3 stream lane: %+v", lanes)
	}
	if observation.StreamSentMessageCount < lanes.StreamMessageCount ||
		observation.SentMessageCount < lanes.DatagramMessageCount {
		t.Fatalf(
			"classified H3 lane counts exceeded physical counters: datagrams=%+v lanes=%+v",
			observation,
			lanes,
		)
	}
	if observation.SentFragmentCount < observation.SentMessageCount ||
		2*observation.SentMessageCount < observation.SentFragmentCount {
		t.Fatalf("measured H3 traffic exceeded two DATAGRAMs per packet message: %+v", observation)
	}
	if observation.SendErrorCount != 0 ||
		observation.DuplicateFragmentCount != 0 ||
		observation.MalformedFragmentCount != 0 ||
		observation.ChecksumFailureCount != 0 ||
		observation.ReassemblyLimitCount != 0 {
		t.Fatalf("measured H3 DATAGRAM integrity or bound failure: %+v", observation)
	}
	// Losing one fragment leaves an intentionally bounded partial message. Its
	// expiry is expected on this loss profile; Transfer owns whole-message
	// recovery. Keep the count in benchmark output rather than treating loss as
	// envelope corruption.
	if observation.SentMessageCount < observation.ReassemblyTimeoutCount {
		t.Fatalf("H3 fragment expiry exceeded sent messages: %+v", observation)
	}
}

func requireH3FullTunOneDatagramHybrid(
	t testing.TB,
	observation h3FullTunDatagramObservation,
	lanes h3FullTunLaneSnapshot,
) {
	t.Helper()
	if observation.SentMessageCount == 0 || observation.ReceivedMessageCount == 0 {
		t.Fatalf("measured H3 hybrid DATAGRAM lane was idle: %+v", observation)
	}
	if lanes.DecodeErrorCount != 0 || lanes.DatagramIpMessageCount == 0 ||
		lanes.DatagramIpFrameCount == 0 || lanes.StreamIpMessageCount == 0 ||
		lanes.StreamIpFrameCount == 0 {
		t.Fatalf("measured H3 hybrid did not use both classified IP lanes: %+v", lanes)
	}
	if observation.StreamSentMessageCount < lanes.StreamMessageCount ||
		observation.SentMessageCount < lanes.DatagramMessageCount {
		t.Fatalf(
			"classified H3 hybrid counts exceeded physical counters: datagrams=%+v lanes=%+v",
			observation,
			lanes,
		)
	}
	if observation.SentFragmentCount != observation.SentMessageCount {
		t.Fatalf("one-DATAGRAM hybrid fragmented a packet-lane message: %+v", observation)
	}
	if observation.SendErrorCount != 0 ||
		observation.DuplicateFragmentCount != 0 ||
		observation.MalformedFragmentCount != 0 ||
		observation.ChecksumFailureCount != 0 ||
		observation.ReassemblyTimeoutCount != 0 ||
		observation.ReassemblyLimitCount != 0 {
		t.Fatalf("one-DATAGRAM hybrid integrity failure: %+v", observation)
	}
}

// TestH3LowBarFullTcpProductionHybridTrack accepts the global 1,100-byte MTU
// and production H3 defaults only when the exact hashed TCP body completes,
// every DATAGRAM message uses one fragment, and both the packet and reliable
// stream lanes carry classified IP traffic.
func TestH3LowBarFullTcpProductionHybridTrack(t *testing.T) {
	testH3LowBarFullTcpPacketTrackMtu(
		t,
		clientconnect.DefaultMtu,
		"production-hybrid-1100-mtu",
		true,
		0,
		false,
	)
}

// TestH3LowBarFullTcpFragmentedPacketTrack preserves the prior all-packet
// candidate as an explicit control. Full-MTU encrypted messages may use two
// DATAGRAMs here; this is deliberately not a production setting.
func TestH3LowBarFullTcpFragmentedPacketTrack(t *testing.T) {
	testH3LowBarFullTcpPacketTrackMtu(
		t,
		clientconnect.DefaultMtu,
		"fragmented-packet-1100-mtu",
		false,
		8,
		true,
	)
}

// TestH3LowBarFullTcpSingleDatagramMtuTrack measures whether lowering the
// application TUN MTU enough to keep every bounded Transfer message inside one
// initial QUIC DATAGRAM improves the lossy-cell result. This is an experiment,
// not a change to DefaultMtu or any platform default.
func TestH3LowBarFullTcpSingleDatagramMtuTrack(t *testing.T) {
	testH3LowBarFullTcpPacketTrackMtu(t, 900, "single-datagram-900-mtu", true, 1, true)
}

// TestH3LowBarFullTcpOneDatagramHybridTrack keeps the global 1,100-byte MTU
// while restricting the packet lane to one QUIC DATAGRAM. Small IP/control
// messages remain unordered; larger Transfer frames use the reliable stream.
func TestH3LowBarFullTcpOneDatagramHybridTrack(t *testing.T) {
	testH3LowBarFullTcpPacketTrackMtu(
		t,
		clientconnect.DefaultMtu,
		"one-datagram-hybrid-1100-mtu",
		true,
		1,
		false,
	)
}

func testH3LowBarFullTcpPacketTrackMtu(
	t *testing.T,
	applicationMtu int,
	mode string,
	requireSingleFragment bool,
	maximumDatagramFragmentCount int,
	requireAllIpOnPacketTrack bool,
) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		const seed = int64(20260817)
		deviceProfile := cellEdgeNetworkProfiles(seed)[cellEdge1mDown250kUpName]
		providerProfile := initialNetworkProfiles(seed)["clean-lan"]
		providerProfile.SourceNote = "synthetic provider colocated with server/connect"

		resources := mobileTunResourceProfile()
		resources.ApplicationMtu = applicationMtu
		laneTrace := newH3FullTunLaneTrace()
		deviceQuicFingerprints := clientconnect.NewH3QuicPacketFingerprintStats(4096)
		providerQuicFingerprints := clientconnect.NewH3QuicPacketFingerprintStats(4096)
		serverQuicFingerprints := clientconnect.NewH3QuicPacketFingerprintStats(4096)
		deviceQuicStats := &clientconnect.H3QuicPacketStats{
			PacketFingerprints: deviceQuicFingerprints,
		}
		providerQuicStats := &clientconnect.H3QuicPacketStats{
			PacketFingerprints: providerQuicFingerprints,
		}
		serverQuicStats := &clientconnect.H3QuicPacketStats{
			PacketFingerprints: serverQuicFingerprints,
		}
		serverDatagramStats := &clientconnect.H3DatagramStats{}
		var readinessDatagramStart h3FullTunDatagramSnapshot
		var readinessDatagramEnd h3FullTunDatagramSnapshot
		var readinessServerDatagramStart clientconnect.H3DatagramStatsSnapshot
		var readinessServerDatagramEnd clientconnect.H3DatagramStatsSnapshot
		var readinessQuicStart h3FullTunQuicSnapshot
		var readinessQuicEnd h3FullTunQuicSnapshot
		var readinessTcpStart h3FullTunTcpSnapshot
		var readinessTcpEnd h3FullTunTcpSnapshot
		var readinessLinkStart map[string]directionalLinkSnapshot
		var readinessLinkEnd map[string]directionalLinkSnapshot
		var readinessClientEnd h3FullTunClientSnapshot
		var readinessStartTime time.Time
		var readinessEndTime time.Time
		readinessSnapshotCount := 0
		configureClient := func(settings *clientconnect.ClientSettings) {
			settings.SendBufferSettings.TransferWireMessageObserver = laneTrace.register
		}
		configurePlatform := func(
			settings *clientconnect.PlatformTransportSettings,
			quicStats *clientconnect.H3QuicPacketStats,
		) {
			settings.H3SendLaneObserver = laneTrace.observe
			settings.H3QuicPacketStats = quicStats
			if 0 < maximumDatagramFragmentCount {
				settings.H3DatagramSettings = clientconnect.DefaultH3DatagramSettings()
				settings.H3DatagramSettings.MaxFragmentCount = maximumDatagramFragmentCount
			}
		}
		hooks := &fullTunConstructionTestHooks{
			afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
				switch stage {
				case fullTunConstructionStageBridge:
					readinessStartTime = time.Now()
					readinessDatagramStart = snapshotH3FullTunDatagrams(path)
					readinessServerDatagramStart = serverDatagramStats.Snapshot()
					readinessQuicStart = h3FullTunQuicSnapshot{
						device:   deviceQuicStats.Snapshot(),
						provider: providerQuicStats.Snapshot(),
						server:   serverQuicStats.Snapshot(),
					}
					readinessTcpStart = snapshotH3FullTunTcp(path)
					readinessLinkStart = path.environment.network.snapshotLinks()
					readinessSnapshotCount += 1
				case fullTunConstructionStageRouteReady:
					readinessEndTime = time.Now()
					readinessDatagramEnd = snapshotH3FullTunDatagrams(path)
					readinessServerDatagramEnd = serverDatagramStats.Snapshot()
					readinessQuicEnd = h3FullTunQuicSnapshot{
						device:   deviceQuicStats.Snapshot(),
						provider: providerQuicStats.Snapshot(),
						server:   serverQuicStats.Snapshot(),
					}
					readinessTcpEnd = snapshotH3FullTunTcp(path)
					readinessLinkEnd = path.environment.network.snapshotLinks()
					readinessClientEnd = snapshotH3FullTunClients(path)
					readinessSnapshotCount += 1
				}
				return nil
			},
			configureConnectHandlerSettings: func(settings *connectserver.ConnectHandlerSettings) {
				settings.H3DatagramStats = serverDatagramStats
				settings.H3QuicPacketStats = serverQuicStats
				if 0 < maximumDatagramFragmentCount {
					settings.H3DatagramSettings = clientconnect.DefaultH3DatagramSettings()
					settings.H3DatagramSettings.MaxFragmentCount = maximumDatagramFragmentCount
				}
			},
			configureProviderClientSettings: configureClient,
			configureProviderPlatformSettings: func(settings *clientconnect.PlatformTransportSettings) {
				configurePlatform(settings, providerQuicStats)
			},
			configureDeviceClientSettings: configureClient,
			configureDevicePlatformSettings: func(settings *clientconnect.PlatformTransportSettings) {
				configurePlatform(settings, deviceQuicStats)
			},
		}
		fixture, err := newPerfvarCorrectnessFixtureWithHooks(
			t,
			fullTunRouteExchangeH3,
			deviceProfile,
			deviceProfile,
			providerProfile,
			resources,
			4*time.Minute,
			hooks,
		)
		if err != nil {
			t.Fatalf("construct %d-byte packet-track route: %v", applicationMtu, err)
		}
		if readinessSnapshotCount != 2 {
			fixture.close()
			t.Fatalf("readiness snapshot count=%d want=2", readinessSnapshotCount)
		}
		readinessDuration := readinessEndTime.Sub(readinessStartTime)
		readinessDatagrams := observeH3FullTunDatagrams(
			readinessDatagramStart,
			readinessDatagramEnd,
		)
		readinessServerDatagrams := subtractH3FullTunDatagrams(
			readinessServerDatagramStart,
			readinessServerDatagramEnd,
		)
		readinessDeviceQuic := subtractH3FullTunQuic(
			readinessQuicStart.device,
			readinessQuicEnd.device,
		)
		readinessProviderQuic := subtractH3FullTunQuic(
			readinessQuicStart.provider,
			readinessQuicEnd.provider,
		)
		readinessServerQuic := subtractH3FullTunQuic(
			readinessQuicStart.server,
			readinessQuicEnd.server,
		)
		readinessTcp := subtractH3FullTunTcp(readinessTcpStart, readinessTcpEnd)
		readinessLinks := subtractLinkSnapshots(
			readinessLinkStart,
			readinessLinkEnd,
			readinessDuration,
		)
		t.Logf(
			"[lowbar-h3-readiness-diagnostic] observation=%+v duration=%s datagrams=%+v server_datagrams=%+v device_quic=%+v provider_quic=%+v server_quic=%+v device_recovery=%+v provider_recovery=%+v device_receive=%+v provider_receive=%+v tcp=%+v links=%+v",
			fixture.path.readinessObservation,
			readinessDuration,
			readinessDatagrams,
			readinessServerDatagrams,
			readinessDeviceQuic,
			readinessProviderQuic,
			readinessServerQuic,
			readinessClientEnd.deviceRecovery,
			readinessClientEnd.providerRecovery,
			readinessClientEnd.deviceReceive,
			readinessClientEnd.providerReceive,
			readinessTcp,
			readinessLinks,
		)
		wireFingerprintTrace := newH3FullTunWireFingerprintTrace(fixture.environment.h3Port)
		fixture.environment.network.setPacketObserver(wireFingerprintTrace.observe)
		defer fixture.environment.network.setPacketObserver(nil)
		var start h3FullTunDatagramSnapshot
		var laneStart h3FullTunLaneSnapshot
		var clientStart h3FullTunClientSnapshot
		var tcpStart h3FullTunTcpSnapshot
		var quicStart h3FullTunQuicSnapshot
		var quicFingerprintStart h3FullTunQuicFingerprintSnapshot
		var wireFingerprintStart h3FullTunWireFingerprintSnapshot
		var serverDatagramStart clientconnect.H3DatagramStatsSnapshot
		var providerCongestionStart clientconnect.ProviderCongestionDrops
		startCount := 0
		observation, measureErr := fixture.measure(
			perfvarWorkloadTCP,
			perfvarDirectionUpload,
			func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
				return measureFullTunUploadWithStartHook(
					ctx,
					path,
					h3LowBarPacketTrackPayloadByteCount,
					h3LowBarPacketTrackPayloadByteCount,
					func() error {
						start = snapshotH3FullTunDatagrams(path)
						laneStart = laneTrace.Snapshot()
						clientStart = snapshotH3FullTunClients(path)
						tcpStart = snapshotH3FullTunTcp(path)
						quicStart = h3FullTunQuicSnapshot{
							device:   deviceQuicStats.Snapshot(),
							provider: providerQuicStats.Snapshot(),
							server:   serverQuicStats.Snapshot(),
						}
						quicFingerprintStart = h3FullTunQuicFingerprintSnapshot{
							device:   deviceQuicFingerprints.Snapshot(),
							provider: providerQuicFingerprints.Snapshot(),
							server:   serverQuicFingerprints.Snapshot(),
						}
						wireFingerprintStart = wireFingerprintTrace.Snapshot()
						serverDatagramStart = serverDatagramStats.Snapshot()
						providerCongestionStart = path.providerRemoteNat.CongestionDropStats()
						startCount += 1
						return nil
					},
				)
			},
		)
		end := snapshotH3FullTunDatagrams(fixture.path)
		laneEnd := laneTrace.Snapshot()
		clientEnd := snapshotH3FullTunClients(fixture.path)
		tcpEnd := snapshotH3FullTunTcp(fixture.path)
		quicEnd := h3FullTunQuicSnapshot{
			device:   deviceQuicStats.Snapshot(),
			provider: providerQuicStats.Snapshot(),
			server:   serverQuicStats.Snapshot(),
		}
		quicFingerprintEnd := h3FullTunQuicFingerprintSnapshot{
			device:   deviceQuicFingerprints.Snapshot(),
			provider: providerQuicFingerprints.Snapshot(),
			server:   serverQuicFingerprints.Snapshot(),
		}
		wireFingerprintEnd := wireFingerprintTrace.Snapshot()
		wireFingerprintsInterval := subtractH3FullTunWireFingerprints(
			wireFingerprintStart,
			wireFingerprintEnd,
		)
		serverDatagramEnd := serverDatagramStats.Snapshot()
		providerCongestionEnd := fixture.path.providerRemoteNat.CongestionDropStats()
		originalDeviceEnd := clientEnd
		if clientStart.deviceClient != nil {
			originalDeviceEnd.deviceRecovery = clientStart.deviceClient.SendRecoveryStats()
			originalDeviceEnd.deviceReceive = clientStart.deviceClient.ReceiveStats()
		}
		datagrams := observeH3FullTunDatagrams(start, end)
		lanes := subtractH3FullTunLanes(laneStart, laneEnd)
		deviceDatagrams := subtractH3FullTunDatagrams(start.device, end.device)
		providerDatagrams := subtractH3FullTunDatagrams(start.provider, end.provider)
		serverDatagrams := subtractH3FullTunDatagrams(serverDatagramStart, serverDatagramEnd)
		deviceQuic := subtractH3FullTunQuic(quicStart.device, quicEnd.device)
		providerQuic := subtractH3FullTunQuic(quicStart.provider, quicEnd.provider)
		serverQuic := subtractH3FullTunQuic(quicStart.server, quicEnd.server)
		deviceQuicFingerprintsInterval := subtractH3FullTunQuicFingerprints(
			quicFingerprintStart.device,
			quicFingerprintEnd.device,
		)
		providerQuicFingerprintsInterval := subtractH3FullTunQuicFingerprints(
			quicFingerprintStart.provider,
			quicFingerprintEnd.provider,
		)
		serverQuicFingerprintsInterval := subtractH3FullTunQuicFingerprints(
			quicFingerprintStart.server,
			quicFingerprintEnd.server,
		)
		clientSentFingerprints := mergeH3FullTunQuicFingerprintCounts(
			deviceQuicFingerprintsInterval.Sent,
			providerQuicFingerprintsInterval.Sent,
		)
		serverReceivedFingerprintCorrelation := correlateH3FullTunQuicFingerprints(
			clientSentFingerprints,
			serverQuicFingerprintsInterval.Received,
		)
		serverDecryptDropFingerprintCorrelation := correlateH3FullTunQuicFingerprints(
			clientSentFingerprints,
			serverQuicFingerprintsInterval.DroppedPayloadDecrypt,
		)
		clientLifetimeSentFingerprints := mergeH3FullTunQuicFingerprintCounts(
			quicFingerprintEnd.device.Sent,
			quicFingerprintEnd.provider.Sent,
		)
		serverDecryptDropLifetimeFingerprintCorrelation := correlateH3FullTunQuicFingerprints(
			clientLifetimeSentFingerprints,
			serverQuicFingerprintsInterval.DroppedPayloadDecrypt,
		)
		serverDecryptDropServerSentFingerprintCorrelation := correlateH3FullTunQuicFingerprints(
			serverQuicFingerprintsInterval.Sent,
			serverQuicFingerprintsInterval.DroppedPayloadDecrypt,
		)
		serverDecryptDropServerLifetimeSentFingerprintCorrelation := correlateH3FullTunQuicFingerprints(
			quicFingerprintEnd.server.Sent,
			serverQuicFingerprintsInterval.DroppedPayloadDecrypt,
		)
		wireSentFingerprints := mergeH3FullTunWireFingerprintSources(
			wireFingerprintsInterval,
		)
		wireLifetimeSentFingerprints := mergeH3FullTunWireFingerprintSources(
			wireFingerprintEnd,
		)
		clientQlogToWireFingerprintCorrelation := correlateH3FullTunQuicFingerprints(
			clientSentFingerprints,
			wireSentFingerprints,
		)
		serverDecryptDropWireFingerprintCorrelation := correlateH3FullTunQuicFingerprints(
			wireSentFingerprints,
			serverQuicFingerprintsInterval.DroppedPayloadDecrypt,
		)
		serverDecryptDropWireLifetimeFingerprintCorrelation := correlateH3FullTunQuicFingerprints(
			wireLifetimeSentFingerprints,
			serverQuicFingerprintsInterval.DroppedPayloadDecrypt,
		)
		deviceRecovery := subtractH3FullTunRecovery(clientStart.deviceRecovery, originalDeviceEnd.deviceRecovery)
		providerRecovery := subtractH3FullTunRecovery(clientStart.providerRecovery, clientEnd.providerRecovery)
		deviceReceive := subtractH3FullTunReceive(clientStart.deviceReceive, originalDeviceEnd.deviceReceive)
		providerReceive := subtractH3FullTunReceive(clientStart.providerReceive, clientEnd.providerReceive)
		tcp := subtractH3FullTunTcp(tcpStart, tcpEnd)
		providerCongestion := subtractH3FullTunProviderCongestion(
			providerCongestionStart,
			providerCongestionEnd,
		)
		lossDrops, queueDrops, mtuDrops := h3LowBarDropCounts(observation.Carrier)
		t.Logf(
			"[lowbar-h3-full-tcp-diagnostic] error=%v useful_bytes=%d carrier_duration=%s wire_bytes=%d loss_drops=%d queue_drops=%d mtu_drops=%d datagrams=%+v lanes=%+v tcp=%+v provider_congestion=%+v device_datagrams=%+v provider_datagrams=%+v server_datagrams=%+v device_quic=%+v provider_quic=%+v server_quic=%+v device_platform_receive=%+v provider_platform_receive=%+v links=%+v device_client=%s device_client_flows=%d device_client_source=%s device_replaced=%t device_recovery=%+v provider_recovery=%+v device_receive=%+v provider_receive=%+v replacement_device_recovery=%+v replacement_device_receive=%+v device_packets=%+v provider_packets=%+v",
			measureErr,
			observation.Result.UsefulByteCount,
			observation.Carrier.Duration,
			observation.Carrier.WireByteCount,
			lossDrops,
			queueDrops,
			mtuDrops,
			datagrams,
			lanes,
			tcp,
			providerCongestion,
			deviceDatagrams,
			providerDatagrams,
			serverDatagrams,
			deviceQuic,
			providerQuic,
			serverQuic,
			observation.Carrier.DevicePlatformReceive,
			observation.Carrier.ProviderPlatformReceive,
			observation.Carrier.Links,
			clientStart.deviceClientId,
			clientStart.deviceClientFlow,
			clientStart.deviceSource,
			clientStart.deviceClient != clientEnd.deviceClient,
			deviceRecovery,
			providerRecovery,
			deviceReceive,
			providerReceive,
			clientEnd.deviceRecovery,
			clientEnd.deviceReceive,
			fixture.path.multiClient.PacketStats(),
			fixture.path.providerRemoteNat.PacketStats(),
		)
		t.Logf(
			"[lowbar-h3-full-tcp-quic] client_queue_sent=%d client_frame_sent=%d server_frame_received=%d server_app_received=%d server_app_queue_sent=%d server_frame_sent=%d client_frame_received=%d client_app_received=%d client_packet_dropped=%d server_packet_dropped=%d server_drop_dos=%d server_drop_duplicate=%d server_drop_other=%d client_packet_lost=%d server_packet_lost=%d device_mtu=%d provider_mtu=%d server_mtu=%d",
			datagrams.SentFragmentCount,
			deviceQuic.SentDatagramFrameCount+providerQuic.SentDatagramFrameCount,
			serverQuic.ReceivedDatagramFrameCount,
			serverDatagrams.ReceivedFragmentCount,
			serverDatagrams.SentFragmentCount,
			serverQuic.SentDatagramFrameCount,
			deviceQuic.ReceivedDatagramFrameCount+providerQuic.ReceivedDatagramFrameCount,
			datagrams.ReceivedFragmentCount,
			deviceQuic.DroppedPacketCount+providerQuic.DroppedPacketCount,
			serverQuic.DroppedPacketCount,
			serverQuic.DroppedDosPreventionPacketCount,
			serverQuic.DroppedDuplicatePacketCount,
			serverQuic.DroppedOtherPacketCount,
			deviceQuic.LostPacketCount+providerQuic.LostPacketCount,
			serverQuic.LostPacketCount,
			deviceQuic.CurrentMtu,
			providerQuic.CurrentMtu,
			serverQuic.CurrentMtu,
		)
		t.Logf(
			"[lowbar-h3-full-tcp-quic-drop-reasons] server_key_unavailable=%d server_unknown_connection_id=%d server_header_parse=%d server_payload_decrypt=%d server_payload_decrypt_before_key_update=%d server_payload_decrypt_after_key_update=%d server_protocol_violation=%d server_unsupported_version=%d server_unexpected_packet=%d server_unexpected_source_connection_id=%d server_unexpected_version=%d server_local_key_updates=%d server_remote_key_updates=%d server_key_discards=%d device_key_updates=%d/%d provider_key_updates=%d/%d",
			serverQuic.DroppedKeyUnavailablePacketCount,
			serverQuic.DroppedUnknownConnectionIdPacketCount,
			serverQuic.DroppedHeaderParseErrorPacketCount,
			serverQuic.DroppedPayloadDecryptErrorPacketCount,
			serverQuic.DroppedPayloadDecryptBeforeKeyUpdateCount,
			serverQuic.DroppedPayloadDecryptAfterKeyUpdateCount,
			serverQuic.DroppedProtocolViolationPacketCount,
			serverQuic.DroppedUnsupportedVersionPacketCount,
			serverQuic.DroppedUnexpectedPacketCount,
			serverQuic.DroppedUnexpectedSourceConnectionIdPacketCount,
			serverQuic.DroppedUnexpectedVersionPacketCount,
			serverQuic.LocalKeyUpdateCount,
			serverQuic.RemoteKeyUpdateCount,
			serverQuic.KeyDiscardCount,
			deviceQuic.LocalKeyUpdateCount,
			deviceQuic.RemoteKeyUpdateCount,
			providerQuic.LocalKeyUpdateCount,
			providerQuic.RemoteKeyUpdateCount,
		)
		t.Logf(
			"[lowbar-h3-full-tcp-quic-key-state-start] server_local_key_updates=%d server_remote_key_updates=%d server_key_discards=%d device_local_key_updates=%d device_remote_key_updates=%d device_key_discards=%d provider_local_key_updates=%d provider_remote_key_updates=%d provider_key_discards=%d",
			quicStart.server.LocalKeyUpdateCount,
			quicStart.server.RemoteKeyUpdateCount,
			quicStart.server.KeyDiscardCount,
			quicStart.device.LocalKeyUpdateCount,
			quicStart.device.RemoteKeyUpdateCount,
			quicStart.device.KeyDiscardCount,
			quicStart.provider.LocalKeyUpdateCount,
			quicStart.provider.RemoteKeyUpdateCount,
			quicStart.provider.KeyDiscardCount,
		)
		t.Logf(
			"[lowbar-h3-full-tcp-quic-fingerprints] client_sent_distinct=%d client_lifetime_sent_distinct=%d server_sent_distinct=%d server_lifetime_sent_distinct=%d server_received_distinct=%d server_decrypt_drop_distinct=%d server_received_matched=%d server_received_unmatched=%d server_decrypt_drop_matched=%d server_decrypt_drop_unmatched=%d server_decrypt_drop_lifetime_matched=%d server_decrypt_drop_lifetime_unmatched=%d server_decrypt_drop_server_sent_matched=%d server_decrypt_drop_server_sent_unmatched=%d server_decrypt_drop_server_lifetime_sent_matched=%d server_decrypt_drop_server_lifetime_sent_unmatched=%d refused=%d/%d/%d unavailable=%d/%d/%d",
			len(clientSentFingerprints),
			len(clientLifetimeSentFingerprints),
			len(serverQuicFingerprintsInterval.Sent),
			len(quicFingerprintEnd.server.Sent),
			len(serverQuicFingerprintsInterval.Received),
			len(serverQuicFingerprintsInterval.DroppedPayloadDecrypt),
			serverReceivedFingerprintCorrelation.MatchedCount,
			serverReceivedFingerprintCorrelation.UnmatchedCount,
			serverDecryptDropFingerprintCorrelation.MatchedCount,
			serverDecryptDropFingerprintCorrelation.UnmatchedCount,
			serverDecryptDropLifetimeFingerprintCorrelation.MatchedCount,
			serverDecryptDropLifetimeFingerprintCorrelation.UnmatchedCount,
			serverDecryptDropServerSentFingerprintCorrelation.MatchedCount,
			serverDecryptDropServerSentFingerprintCorrelation.UnmatchedCount,
			serverDecryptDropServerLifetimeSentFingerprintCorrelation.MatchedCount,
			serverDecryptDropServerLifetimeSentFingerprintCorrelation.UnmatchedCount,
			deviceQuicFingerprintsInterval.RefusedFingerprintCount,
			providerQuicFingerprintsInterval.RefusedFingerprintCount,
			serverQuicFingerprintsInterval.RefusedFingerprintCount,
			deviceQuicFingerprintsInterval.UnavailableCount,
			providerQuicFingerprintsInterval.UnavailableCount,
			serverQuicFingerprintsInterval.UnavailableCount,
		)
		t.Logf(
			"[lowbar-h3-full-tcp-wire-fingerprints] wire_sent_distinct=%d wire_lifetime_sent_distinct=%d qlog_to_wire_matched=%d qlog_to_wire_unmatched=%d server_decrypt_drop_wire_matched=%d server_decrypt_drop_wire_unmatched=%d server_decrypt_drop_wire_lifetime_matched=%d server_decrypt_drop_wire_lifetime_unmatched=%d sources=%d unavailable=%d invalid=%d",
			len(wireSentFingerprints),
			len(wireLifetimeSentFingerprints),
			clientQlogToWireFingerprintCorrelation.MatchedCount,
			clientQlogToWireFingerprintCorrelation.UnmatchedCount,
			serverDecryptDropWireFingerprintCorrelation.MatchedCount,
			serverDecryptDropWireFingerprintCorrelation.UnmatchedCount,
			serverDecryptDropWireLifetimeFingerprintCorrelation.MatchedCount,
			serverDecryptDropWireLifetimeFingerprintCorrelation.UnmatchedCount,
			len(wireFingerprintsInterval.bySource),
			wireFingerprintsInterval.unavailableCount,
			wireFingerprintsInterval.invalidCount,
		)
		t.Logf(
			"[lowbar-h3-full-tcp-gate] sent_messages=%d sent_fragments=%d received_messages=%d stream_sent=%d stream_received=%d send_errors=%d duplicate_fragments=%d malformed_fragments=%d checksum_failures=%d reassembly_timeouts=%d reassembly_limits=%d lane_decode_errors=%d lane_datagram_messages=%d lane_datagram_ip_messages=%d lane_datagram_ip_frames=%d lane_stream_messages=%d lane_stream_ip_messages=%d lane_stream_ip_frames=%d",
			datagrams.SentMessageCount,
			datagrams.SentFragmentCount,
			datagrams.ReceivedMessageCount,
			datagrams.StreamSentMessageCount,
			datagrams.StreamReceivedMessageCount,
			datagrams.SendErrorCount,
			datagrams.DuplicateFragmentCount,
			datagrams.MalformedFragmentCount,
			datagrams.ChecksumFailureCount,
			datagrams.ReassemblyTimeoutCount,
			datagrams.ReassemblyLimitCount,
			lanes.DecodeErrorCount,
			lanes.DatagramMessageCount,
			lanes.DatagramIpMessageCount,
			lanes.DatagramIpFrameCount,
			lanes.StreamMessageCount,
			lanes.StreamIpMessageCount,
			lanes.StreamIpFrameCount,
		)
		t.Logf(
			"[lowbar-h3-full-tcp-recovery] device_timeout=%d device_carrier_change=%d device_gap=%d device_tail=%d device_cumulative=%d device_contract_write=%d device_contract_request=%d device_flow_bypass=%d device_noack_bypass=%d device_flow_reserve_select=%d device_flow_reserve_lane=%d device_flight_wait=%d/%s/%s device_flight_gap=%d device_flight_timeout=%d device_flight_reduce=%d device_flight_max_bytes=%d/%d device_flight_max_messages=%d/%d provider_timeout=%d provider_carrier_change=%d provider_gap=%d provider_tail=%d provider_cumulative=%d provider_contract_write=%d provider_contract_request=%d provider_flow_bypass=%d provider_noack_bypass=%d provider_flow_reserve_select=%d provider_flow_reserve_lane=%d provider_flight_wait=%d/%s/%s provider_flight_gap=%d provider_flight_timeout=%d provider_flight_reduce=%d provider_flight_max_bytes=%d/%d provider_flight_max_messages=%d/%d device_pack_drop=%d device_ack_drop=%d provider_pack_drop=%d provider_ack_drop=%d tcp_retransmits=%d tcp_timeouts=%d tcp_fast_retransmit=%d tcp_resets=%d/%d",
			deviceRecovery.TimeoutResendWriteCount,
			deviceRecovery.CarrierChangeWriteCount,
			deviceRecovery.SelectiveGapWriteCount,
			deviceRecovery.AckTailProbeWriteCount,
			deviceRecovery.CumulativeProbeWriteCount,
			deviceRecovery.MissingContractWriteCount,
			deviceRecovery.MissingContractRequestCount,
			deviceRecovery.UnreliableFlowIsolationBypassCount,
			deviceRecovery.UnreliableNoAckAdmissionBypassCount,
			deviceRecovery.UnreliableFlowReserveSelectionCount,
			deviceRecovery.UnreliableFlowReserveUseCount,
			deviceRecovery.UnreliableFlightWaitCount,
			deviceRecovery.UnreliableFlightWaitDuration,
			deviceRecovery.UnreliableFlightMaximumWaitDuration,
			deviceRecovery.UnreliableFlightGapCount,
			deviceRecovery.UnreliableFlightTimeoutCount,
			deviceRecovery.UnreliableFlightReductionCount,
			deviceRecovery.UnreliableFlightMaximumByteCount,
			deviceRecovery.UnreliableFlightMaximumLimitByteCount,
			deviceRecovery.UnreliableFlightMaximumMessageCount,
			deviceRecovery.UnreliableFlightMaximumMessageLimit,
			providerRecovery.TimeoutResendWriteCount,
			providerRecovery.CarrierChangeWriteCount,
			providerRecovery.SelectiveGapWriteCount,
			providerRecovery.AckTailProbeWriteCount,
			providerRecovery.CumulativeProbeWriteCount,
			providerRecovery.MissingContractWriteCount,
			providerRecovery.MissingContractRequestCount,
			providerRecovery.UnreliableFlowIsolationBypassCount,
			providerRecovery.UnreliableNoAckAdmissionBypassCount,
			providerRecovery.UnreliableFlowReserveSelectionCount,
			providerRecovery.UnreliableFlowReserveUseCount,
			providerRecovery.UnreliableFlightWaitCount,
			providerRecovery.UnreliableFlightWaitDuration,
			providerRecovery.UnreliableFlightMaximumWaitDuration,
			providerRecovery.UnreliableFlightGapCount,
			providerRecovery.UnreliableFlightTimeoutCount,
			providerRecovery.UnreliableFlightReductionCount,
			providerRecovery.UnreliableFlightMaximumByteCount,
			providerRecovery.UnreliableFlightMaximumLimitByteCount,
			providerRecovery.UnreliableFlightMaximumMessageCount,
			providerRecovery.UnreliableFlightMaximumMessageLimit,
			deviceReceive.PackHandoffDropCount,
			deviceReceive.AckHandoffDropCount,
			providerReceive.PackHandoffDropCount,
			providerReceive.AckHandoffDropCount,
			tcp.Retransmits,
			tcp.Timeouts,
			tcp.FastRetransmit,
			tcp.ResetsSent,
			tcp.ResetsReceived,
		)
		fixture.close()
		if measureErr != nil {
			t.Fatalf("measure %d-byte packet track: %v", applicationMtu, measureErr)
		}
		if startCount != 1 {
			t.Fatalf("measurement start count=%d, want=1", startCount)
		}
		if observation.Result.UsefulByteCount != h3LowBarPacketTrackPayloadByteCount ||
			observation.Result.ContentHash != deterministicPayloadHash(h3LowBarPacketTrackPayloadByteCount) {
			t.Fatalf("TCP verification result=%+v", observation.Result)
		}
		t.Logf(
			"[lowbar-h3-full-tcp] mode=%s profile=%s app_mtu=%d provider_mtu=%d initial_flight_bytes=%d useful_bytes=%d setup=%s duration=%s goodput_Mbps=%.3f carrier_duration=%s wire_bytes=%d wire_efficiency=%.4f loss_drops=%d queue_drops=%d mtu_drops=%d datagram_sent=%d datagram_received=%d stream_sent=%d stream_received=%d",
			mode,
			deviceProfile.Name,
			applicationMtu,
			applicationMtu,
			clientconnect.DefaultSendBufferSettings().UnreliableInitialFlightByteCount,
			observation.Result.UsefulByteCount,
			observation.Result.SetupDuration,
			observation.Result.Duration,
			float64(observation.Result.UsefulByteCount*8)/observation.Result.Duration.Seconds()/1_000_000,
			observation.Carrier.Duration,
			observation.Carrier.WireByteCount,
			float64(observation.Result.UsefulByteCount)/float64(observation.Carrier.WireByteCount),
			lossDrops,
			queueDrops,
			mtuDrops,
			datagrams.SentMessageCount,
			datagrams.ReceivedMessageCount,
			datagrams.StreamSentMessageCount,
			datagrams.StreamReceivedMessageCount,
		)
		if requireAllIpOnPacketTrack {
			requireH3FullTunPacketTrack(t, datagrams, lanes)
		} else {
			requireH3FullTunOneDatagramHybrid(t, datagrams, lanes)
		}
		if requireSingleFragment && datagrams.SentFragmentCount != datagrams.SentMessageCount {
			t.Fatalf(
				"single-DATAGRAM MTU fragmented Transfer messages: sent_messages=%d sent_fragments=%d",
				datagrams.SentMessageCount,
				datagrams.SentFragmentCount,
			)
		}
	})
}

// TestH3LowBarFullTcpLegacyStreamTrack is the corrected-harness control for
// the pre-DATAGRAM H3 carrier. Both endpoints explicitly suppress the QUIC
// transport parameter and authenticated capability offer, so every measured
// routed IP message must use the original reliable stream on the same QUIC
// connection. Separate cold processes provide the distribution used beside
// the packet-track result.
func TestH3LowBarFullTcpLegacyStreamTrack(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		const seed = int64(20260817)
		deviceProfile := cellEdgeNetworkProfiles(seed)[cellEdge1mDown250kUpName]
		providerProfile := initialNetworkProfiles(seed)["clean-lan"]
		providerProfile.SourceNote = "synthetic provider colocated with server/connect"

		resources := mobileTunResourceProfile()
		resources.ApplicationMtu = clientconnect.DefaultMtu
		laneTrace := newH3FullTunLaneTrace()
		var readinessStart time.Time
		var readinessEnd time.Time
		configureClient := func(settings *clientconnect.ClientSettings) {
			settings.SendBufferSettings.TransferWireMessageObserver = laneTrace.register
		}
		configurePlatform := func(settings *clientconnect.PlatformTransportSettings) {
			settings.EnableH3Datagrams = false
			settings.H3SendLaneObserver = laneTrace.observe
		}
		hooks := &fullTunConstructionTestHooks{
			afterStage: func(stage fullTunConstructionStage, _ *fullTunPath) error {
				switch stage {
				case fullTunConstructionStageBridge:
					readinessStart = time.Now()
				case fullTunConstructionStageRouteReady:
					readinessEnd = time.Now()
				}
				return nil
			},
			configureConnectHandlerSettings: func(settings *connectserver.ConnectHandlerSettings) {
				settings.EnableH3Datagrams = false
			},
			configureProviderClientSettings:   configureClient,
			configureProviderPlatformSettings: configurePlatform,
			configureDeviceClientSettings:     configureClient,
			configureDevicePlatformSettings:   configurePlatform,
		}
		fixture, err := newPerfvarCorrectnessFixtureWithHooks(
			t,
			fullTunRouteExchangeH3,
			deviceProfile,
			deviceProfile,
			providerProfile,
			resources,
			4*time.Minute,
			hooks,
		)
		if err != nil {
			t.Fatalf("construct legacy H3 low-bar route: %v", err)
		}
		if readinessStart.IsZero() || readinessEnd.IsZero() || readinessEnd.Before(readinessStart) {
			fixture.close()
			t.Fatalf("invalid legacy H3 readiness interval start=%s end=%s", readinessStart, readinessEnd)
		}

		var datagramStart h3FullTunDatagramSnapshot
		var laneStart h3FullTunLaneSnapshot
		startCount := 0
		observation, measureErr := fixture.measure(
			perfvarWorkloadTCP,
			perfvarDirectionUpload,
			func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
				return measureFullTunUploadWithStartHook(
					ctx,
					path,
					h3LowBarPacketTrackPayloadByteCount,
					h3LowBarPacketTrackPayloadByteCount,
					func() error {
						datagramStart = snapshotH3FullTunDatagrams(path)
						laneStart = laneTrace.Snapshot()
						startCount += 1
						return nil
					},
				)
			},
		)
		datagrams := observeH3FullTunDatagrams(
			datagramStart,
			snapshotH3FullTunDatagrams(fixture.path),
		)
		lanes := subtractH3FullTunLanes(laneStart, laneTrace.Snapshot())
		lossDrops, queueDrops, mtuDrops := h3LowBarDropCounts(observation.Carrier)
		fixture.close()

		t.Logf(
			"[lowbar-h3-legacy-full-tcp] profile=%s useful_bytes=%d readiness=%s setup=%s duration=%s goodput_Mbps=%.3f carrier_duration=%s wire_bytes=%d wire_efficiency=%.4f loss_drops=%d queue_drops=%d mtu_drops=%d stream_messages=%d stream_ip_messages=%d stream_ip_frames=%d datagram_messages=%d datagram_ip_messages=%d datagram_ip_frames=%d",
			deviceProfile.Name,
			observation.Result.UsefulByteCount,
			readinessEnd.Sub(readinessStart),
			observation.Result.SetupDuration,
			observation.Result.Duration,
			float64(observation.Result.UsefulByteCount*8)/observation.Result.Duration.Seconds()/1_000_000,
			observation.Carrier.Duration,
			observation.Carrier.WireByteCount,
			float64(observation.Result.UsefulByteCount)/float64(observation.Carrier.WireByteCount),
			lossDrops,
			queueDrops,
			mtuDrops,
			lanes.StreamMessageCount,
			lanes.StreamIpMessageCount,
			lanes.StreamIpFrameCount,
			lanes.DatagramMessageCount,
			lanes.DatagramIpMessageCount,
			lanes.DatagramIpFrameCount,
		)
		if measureErr != nil {
			t.Fatalf("measure legacy H3 low-bar stream: %v", measureErr)
		}
		if startCount != 1 {
			t.Fatalf("legacy H3 measurement start count=%d, want=1", startCount)
		}
		if observation.Result.UsefulByteCount != h3LowBarPacketTrackPayloadByteCount ||
			observation.Result.ContentHash != deterministicPayloadHash(h3LowBarPacketTrackPayloadByteCount) {
			t.Fatalf("legacy H3 TCP verification result=%+v", observation.Result)
		}
		if datagrams != (h3FullTunDatagramObservation{}) {
			t.Fatalf("legacy H3 unexpectedly negotiated DATAGRAM: %+v", datagrams)
		}
		if lanes.DecodeErrorCount != 0 || lanes.StreamIpMessageCount == 0 ||
			lanes.StreamIpFrameCount == 0 {
			t.Fatalf("legacy H3 stream lane had no classified IP traffic: %+v", lanes)
		}
		if lanes.DatagramMessageCount != 0 || lanes.DatagramIpMessageCount != 0 ||
			lanes.DatagramIpFrameCount != 0 || lanes.UnclassifiedDatagramCount != 0 {
			t.Fatalf("legacy H3 traffic leaked onto DATAGRAM lane: %+v", lanes)
		}
	})
}

// TestH1LowBarFullTcpStreamTrack is the same full-TUN, cold-process control on
// the production H1 exchange carrier. Keeping it beside the H3 packet and
// stream tracks prevents a favorable H3 change from being judged only against
// an obsolete historical baseline.
func TestH1LowBarFullTcpStreamTrack(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		const seed = int64(20260817)
		deviceProfile := cellEdgeNetworkProfiles(seed)[cellEdge1mDown250kUpName]
		providerProfile := initialNetworkProfiles(seed)["clean-lan"]
		providerProfile.SourceNote = "synthetic provider colocated with server/connect"

		resources := mobileTunResourceProfile()
		resources.ApplicationMtu = clientconnect.DefaultMtu
		fixture, err := newPerfvarCorrectnessFixture(
			t,
			fullTunRouteExchangeH1,
			deviceProfile,
			deviceProfile,
			providerProfile,
			resources,
			4*time.Minute,
		)
		if err != nil {
			t.Fatalf("construct H1 low-bar route: %v", err)
		}

		startCount := 0
		observation, measureErr := fixture.measure(
			perfvarWorkloadTCP,
			perfvarDirectionUpload,
			func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
				return measureFullTunUploadWithStartHook(
					ctx,
					path,
					h3LowBarPacketTrackPayloadByteCount,
					h3LowBarPacketTrackPayloadByteCount,
					func() error {
						startCount += 1
						return nil
					},
				)
			},
		)
		lossDrops, queueDrops, mtuDrops := h3LowBarDropCounts(observation.Carrier)
		fixture.close()

		t.Logf(
			"[lowbar-h1-full-tcp] profile=%s useful_bytes=%d setup=%s duration=%s goodput_Mbps=%.3f carrier_duration=%s wire_bytes=%d wire_efficiency=%.4f loss_drops=%d queue_drops=%d mtu_drops=%d",
			deviceProfile.Name,
			observation.Result.UsefulByteCount,
			observation.Result.SetupDuration,
			observation.Result.Duration,
			float64(observation.Result.UsefulByteCount*8)/observation.Result.Duration.Seconds()/1_000_000,
			observation.Carrier.Duration,
			observation.Carrier.WireByteCount,
			float64(observation.Result.UsefulByteCount)/float64(observation.Carrier.WireByteCount),
			lossDrops,
			queueDrops,
			mtuDrops,
		)
		if measureErr != nil {
			t.Fatalf("measure H1 low-bar stream: %v", measureErr)
		}
		if startCount != 1 {
			t.Fatalf("H1 measurement start count=%d, want=1", startCount)
		}
		if observation.Result.UsefulByteCount != h3LowBarPacketTrackPayloadByteCount ||
			observation.Result.ContentHash != deterministicPayloadHash(h3LowBarPacketTrackPayloadByteCount) {
			t.Fatalf("H1 TCP verification result=%+v", observation.Result)
		}
	})
}
