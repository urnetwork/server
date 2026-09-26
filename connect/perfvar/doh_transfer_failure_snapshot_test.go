//go:build acklineagetrace

package perfvar

import (
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"gvisor.dev/gvisor/pkg/tcpip"
)

const (
	dohWarmFailureStackCapacity    = 2 * 1024 * 1024
	dohWarmFailureMetadataCapacity = 512 * 1024
	dohWarmFailureEndpointCapacity = 64
	dohWarmFailureCaptureBudget    = 250 * time.Millisecond
)

type dohWarmFailureIdentity struct {
	Phase, Arm, ScenarioHash, ProfileHash, TraceHash string
	RunIndex                                         int
}

type dohWarmFailureEndpoint struct {
	ClientID         clientconnect.Id
	ContextDone      bool
	Settings         dohTransferEffectiveSettings
	AckExpiries      uint64
	Receive          clientconnect.ClientReceiveStatsSnapshot
	Recovery         clientconnect.ClientSendRecoveryStatsSnapshot
	ReadTimeout      time.Duration
	ReadTimeoutKnown bool
}

type dohWarmFailureSnapshot struct {
	Identity                                         dohWarmFailureIdentity
	StartedNS                                        int64
	CaptureDuration, CaptureBudget                   time.Duration
	HardTimeBound                                    bool
	BudgetExceeded, MetadataComplete                 bool
	MetadataTruncated, StackTruncated                bool
	StackBytes, StackCapacity, MetadataCapacity      int
	EndpointsTruncated                               bool
	Devices                                          []dohWarmFailureEndpoint
	Provider                                         dohWarmFailureEndpoint
	DeviceWire, ProviderWire                         dohTransferWireSnapshot
	DeviceH1, ProviderH1                             clientconnect.H1ConnectionStatsSnapshot
	DevicePlatform, ProviderPlatform                 clientconnect.PlatformTransportReceiveStatsSnapshot
	Native                                           h1FailureTCPSnapshot
	ApplicationTCP                                   h1FailureTCPStackCounters
	ApplicationConnection                            *h1FailureTCPConnectionSnapshot `json:",omitempty"`
	ServerAccepted, ServerHTTP2Requests              int64
	DevicePackFailures, ProviderPackFailures         uint64
	DeviceWorkloadFailures, ProviderWorkloadFailures uint64
	TcpCollapseDrops                                 uint64
	stack                                            []byte
}

// This is a bounded diagnostic, not a hard real-time operation. runtime.Stack
// and the existing metadata/TCPInfo locks cannot be interrupted safely. One
// fixed stack buffer, at most 64 endpoint/connection records, and no retries
// bound work and storage; the 250ms budget stops later stages and any overrun
// is reported explicitly. There is no worker left running after collection.
func captureDohWarmFailure(flag string, failed bool, identity dohWarmFailureIdentity, now func() time.Time, stack func([]byte, bool) int, collect func(*dohWarmFailureSnapshot, func() bool) bool) *dohWarmFailureSnapshot {
	if flag != "1" || !failed {
		return nil
	}
	started := now()
	result := &dohWarmFailureSnapshot{
		Identity: identity, StartedNS: started.UnixNano(), CaptureBudget: dohWarmFailureCaptureBudget,
		StackCapacity: dohWarmFailureStackCapacity, MetadataCapacity: dohWarmFailureMetadataCapacity,
	}
	withinBudget := func() bool {
		if now().Sub(started) >= dohWarmFailureCaptureBudget {
			result.BudgetExceeded = true
			return false
		}
		return true
	}
	buffer := make([]byte, dohWarmFailureStackCapacity)
	n := stack(buffer, true)
	result.StackTruncated = n < 0 || n >= len(buffer)
	n = min(max(n, 0), len(buffer))
	result.StackBytes, result.stack = n, buffer[:n]
	if withinBudget() && collect != nil {
		result.MetadataComplete = collect(result, withinBudget)
	}
	result.CaptureDuration = now().Sub(started)
	result.BudgetExceeded = result.BudgetExceeded || result.CaptureDuration >= result.CaptureBudget
	return result
}

func dohWarmFailureMetadata(snapshot *dohWarmFailureSnapshot) ([]byte, error) {
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) <= dohWarmFailureMetadataCapacity {
		return data, err
	}
	// Do not truncate JSON into a misleading/incomplete record. Preserve the
	// exact identity and capture limits, declaring the omitted metadata.
	bounded := *snapshot
	bounded.MetadataTruncated, bounded.MetadataComplete = true, false
	bounded.Devices = nil
	bounded.Provider = dohWarmFailureEndpoint{}
	bounded.Native = h1FailureTCPSnapshot{}
	bounded.ApplicationConnection = nil
	data, err = json.Marshal(&bounded)
	if err == nil && len(data) > dohWarmFailureMetadataCapacity {
		return nil, fmt.Errorf("bounded warmup metadata exceeds %d bytes", dohWarmFailureMetadataCapacity)
	}
	return data, err
}

func dumpDohWarmFailure(t testing.TB, flag string, record dohTransferRecord, path *fullTunPath, native *h1FailureTCPRecorder, device, provider *dohTransferEndpointObserver, op *tcpRecoveryOperation) {
	dumpDohFailureAtPhase(t, flag, "warmup-before-teardown", "doh-warm-failure", record, path, native, device, provider, op)
}

func dumpDohTerminalFailure(t testing.TB, flag string, record dohTransferRecord, path *fullTunPath, native *h1FailureTCPRecorder, device, provider *dohTransferEndpointObserver, op *tcpRecoveryOperation) {
	dumpDohFailureAtPhase(t, flag, "terminal-join-before-teardown", "doh-terminal-failure", record, path, native, device, provider, op)
}

func dohFailureIdentity(phase string, record dohTransferRecord) dohWarmFailureIdentity {
	return dohWarmFailureIdentity{Phase: phase, Arm: record.Arm.Name, RunIndex: record.RunIndex,
		ScenarioHash: record.ScenarioHash, ProfileHash: record.ProfileHash, TraceHash: record.Trace.IdentityHash}
}

type dohFailureTCPConnection interface {
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	TcpInfo() (tcpip.TCPInfoOption, error)
}

func dohFailureApplicationConnection(raw dohFailureTCPConnection, connectionPointer, readOwnerPointer string, captured time.Time) *h1FailureTCPConnectionSnapshot {
	connection := &h1FailureTCPConnectionSnapshot{
		Node: "application-doh", Local: h1FailureTCPAddress(raw.LocalAddr()), Remote: h1FailureTCPAddress(raw.RemoteAddr()),
		ConnectionPointer: connectionPointer, ReadOwnerPointer: readOwnerPointer, CapturedNS: captured.UnixNano(),
	}
	var err error
	connection.Info, err = raw.TcpInfo()
	if err != nil {
		connection.Error = "TCPInfo unavailable" // Never retain arbitrary error/payload text.
	}
	return connection
}

func dumpDohFailureAtPhase(t testing.TB, flag, phase, label string, record dohTransferRecord, path *fullTunPath, native *h1FailureTCPRecorder, device, provider *dohTransferEndpointObserver, op *tcpRecoveryOperation) {
	identity := dohFailureIdentity(phase, record)
	snapshot := captureDohWarmFailure(flag, true, identity, time.Now, runtime.Stack, func(result *dohWarmFailureSnapshot, withinBudget func() bool) bool {
		endpoint := func(client *clientconnect.Client, observer *dohTransferEndpointObserver) dohWarmFailureEndpoint {
			if client == nil {
				return dohWarmFailureEndpoint{}
			}
			value := dohWarmFailureEndpoint{ClientID: client.ClientId(), ContextDone: client.Ctx().Err() != nil, Receive: client.ReceiveStats(), Recovery: client.SendRecoveryStats()}
			value.Settings, value.AckExpiries = observer.snapshot(value.ClientID)
			if path.apiGenerator != nil {
				value.ReadTimeout, value.ReadTimeoutKnown = path.apiGenerator.ClientReadTimeout(client)
			}
			return value
		}
		// Immutable owner list avoids taking MultiClient flow/window locks at
		// the failing boundary. All retained clients are labeled, not guessed
		// to be the active flow from "newest client" alone.
		if path.deviceTransports != nil {
			node := path.deviceTransports.head.Load()
			for node != nil && len(result.Devices) < dohWarmFailureEndpointCapacity && withinBudget() {
				result.Devices = append(result.Devices, endpoint(node.client, device))
				node = node.next
			}
			result.EndpointsTruncated = node != nil
		}
		if !withinBudget() {
			return false
		}
		result.Provider = endpoint(path.providerClient, provider)
		result.DeviceWire, result.ProviderWire = device.wire.snapshot(), provider.wire.snapshot()
		result.DeviceH1, result.ProviderH1 = device.h1.Snapshot(), provider.h1.Snapshot()
		if path.devicePlatformReceiveStats != nil {
			result.DevicePlatform = path.devicePlatformReceiveStats.Snapshot()
		}
		if path.providerPlatformReceiveStats != nil {
			result.ProviderPlatform = path.providerPlatformReceiveStats.Snapshot()
		}
		result.DevicePackFailures, result.ProviderPackFailures = path.devicePackSends.failures.Load(), path.providerPackSends.failures.Load()
		result.DeviceWorkloadFailures, result.ProviderWorkloadFailures = path.devicePackSends.workloadFailures.Load(), path.providerPackSends.workloadFailures.Load()
		result.TcpCollapseDrops = path.multiClient.TcpCollapseDropCount()
		result.ServerAccepted, result.ServerHTTP2Requests = op.accepted.Load(), op.http2.Load()
		result.ApplicationTCP = h1FailureTCPStats(path.appTun)
		if !withinBudget() {
			return false
		}
		if raw := op.raw.Load(); raw != nil {
			result.ApplicationConnection = dohFailureApplicationConnection(raw, fmt.Sprintf("%p", raw), fmt.Sprintf("%p", raw.TCPConn), time.Now())
		}
		if !withinBudget() {
			return false
		}
		result.Native = native.snapshot(path)
		return withinBudget() && !result.EndpointsTruncated && !result.Native.Incomplete
	})
	if snapshot == nil {
		return
	}
	metadata, err := dohWarmFailureMetadata(snapshot)
	if err != nil {
		t.Errorf("%s snapshot encoding: %v", label, err)
		return
	}
	t.Logf("[%s-snapshot] %s", label, metadata)
	t.Logf("[%s-stack] run_index=%d bytes=%d capacity=%d truncated=%t\n%s\n[%s-stack-end]", label, record.RunIndex, snapshot.StackBytes, dohWarmFailureStackCapacity, snapshot.StackTruncated, snapshot.stack, label)
}

type dohFailureRetiredTCPConnection struct {
	local, remote net.Addr
	infoErr       error
}

func (self dohFailureRetiredTCPConnection) LocalAddr() net.Addr  { return self.local }
func (self dohFailureRetiredTCPConnection) RemoteAddr() net.Addr { return self.remote }
func (self dohFailureRetiredTCPConnection) TcpInfo() (tcpip.TCPInfoOption, error) {
	return tcpip.TCPInfoOption{}, self.infoErr
}

func TestDohTerminalFailureSnapshotRetiredConnectionAddresses(t *testing.T) {
	address := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}
	for _, local := range []net.Addr{nil, address} {
		for _, remote := range []net.Addr{nil, address} {
			for _, infoErr := range []error{nil, fmt.Errorf("do not retain sensitive endpoint error")} {
				identity := dohWarmFailureIdentity{Phase: "terminal-join-before-teardown", Arm: "ack-3-8", RunIndex: 12}
				raw := dohFailureRetiredTCPConnection{local, remote, infoErr}
				result := captureDohWarmFailure("1", true, identity, time.Now, func([]byte, bool) int { return 0 }, func(snapshot *dohWarmFailureSnapshot, _ func() bool) bool {
					snapshot.ApplicationConnection = dohFailureApplicationConnection(raw, "connection", "read-owner", time.Unix(123, 0))
					return true
				})
				data, err := dohWarmFailureMetadata(result)
				if err != nil || !result.MetadataComplete || result.Identity != identity || strings.Contains(string(data), "sensitive") {
					t.Fatalf("retired connection invalidated metadata: error=%v result=%+v", err, result)
				}
				connection := result.ApplicationConnection
				if connection.ConnectionPointer != "connection" || connection.ReadOwnerPointer != "read-owner" || connection.CapturedNS != time.Unix(123, 0).UnixNano() {
					t.Fatal("retirement discarded exact owner identity")
				}
				for _, pair := range []struct {
					address  net.Addr
					captured string
				}{{local, connection.Local}, {remote, connection.Remote}} {
					if pair.address == nil && pair.captured != "" || pair.address != nil && pair.captured != pair.address.String() {
						t.Fatal("retired address was invented or surviving address discarded")
					}
				}
				if infoErr != nil && connection.Error != "TCPInfo unavailable" || infoErr == nil && connection.Error != "" {
					t.Fatal("TCPInfo availability was concealed")
				}
			}
		}
	}
}

func TestDohTerminalFailureSnapshotIdentityAndNilOptIn(t *testing.T) {
	record := dohTransferRecord{Arm: dohTransferArm{Name: "ack-3-8"}, RunIndex: 12,
		ScenarioHash: "scenario", ProfileHash: "profile", Trace: perfvarTrace{IdentityHash: "trace"}}
	identity := dohFailureIdentity("terminal-join-before-teardown", record)
	if identity != (dohWarmFailureIdentity{Phase: "terminal-join-before-teardown", Arm: "ack-3-8", RunIndex: 12,
		ScenarioHash: "scenario", ProfileHash: "profile", TraceHash: "trace"}) {
		t.Fatal("terminal failure lost exact phase/arm/trace identity")
	}
	if allocs := testing.AllocsPerRun(100, func() { dumpDohTerminalFailure(t, "", record, nil, nil, nil, nil, nil) }); allocs != 0 {
		t.Fatalf("disabled terminal snapshot allocations=%g", allocs)
	}
	snapshot := captureDohWarmFailure("1", true, identity, time.Now, func(buffer []byte, all bool) int {
		if !all || len(buffer) != dohWarmFailureStackCapacity {
			t.Fatal("terminal stack bound changed")
		}
		return copy(buffer, "terminal metadata only")
	}, nil)
	if snapshot.Identity != identity || snapshot.HardTimeBound || snapshot.MetadataCapacity != dohWarmFailureMetadataCapacity {
		t.Fatal("terminal snapshot changed boundedness or claimed a hard time bound")
	}
}

func TestDohWarmFailureSnapshotRequiresExactOptInAndFailure(t *testing.T) {
	for _, flag := range []string{"", "0", "true", "1"} {
		for _, failed := range []bool{false, true} {
			calls := 0
			stack := func(buffer []byte, all bool) int { calls++; return copy(buffer, "metadata-only stack") }
			result := captureDohWarmFailure(flag, failed, dohWarmFailureIdentity{}, time.Now, stack, nil)
			want := flag == "1" && failed
			if (result != nil) != want || (calls == 1) != want {
				t.Fatalf("flag=%q failed=%t calls=%d", flag, failed, calls)
			}
		}
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if captureDohWarmFailure("1", false, dohWarmFailureIdentity{}, nil, nil, nil) != nil {
			panic("successful warmup captured")
		}
	}); allocs != 0 {
		t.Fatalf("successful path allocates: %g", allocs)
	}
}

func TestDohWarmFailureSnapshotStackBoundsAndIdentity(t *testing.T) {
	identity := dohWarmFailureIdentity{Phase: "warmup-before-teardown", Arm: "ack-8-8", ScenarioHash: strings.Repeat("a", 64), ProfileHash: strings.Repeat("b", 64), TraceHash: "53bd1340aad695294aa5919cf43fb0c4629d305cc68b4994577183780005fa8a", RunIndex: 2}
	for _, size := range []int{-1, 0, 17, dohWarmFailureStackCapacity, dohWarmFailureStackCapacity + 1} {
		calls := 0
		result := captureDohWarmFailure("1", true, identity, time.Now, func(buffer []byte, all bool) int {
			calls++
			if !all || len(buffer) != dohWarmFailureStackCapacity {
				t.Fatal("stack bounds changed")
			}
			return size
		}, nil)
		if calls != 1 || result.Identity != identity || result.StackTruncated != (size < 0 || size >= dohWarmFailureStackCapacity) || len(result.stack) != min(max(size, 0), dohWarmFailureStackCapacity) {
			t.Fatalf("stack/identity result invalid for size %d", size)
		}
	}
}

func TestDohWarmFailureSnapshotBudgetReportsUninterruptibleOverrun(t *testing.T) {
	now := time.Unix(100, 0)
	called := false
	result := captureDohWarmFailure("1", true, dohWarmFailureIdentity{}, func() time.Time { return now }, func([]byte, bool) int {
		now = now.Add(dohWarmFailureCaptureBudget + time.Nanosecond)
		return 0
	}, func(*dohWarmFailureSnapshot, func() bool) bool { called = true; return true })
	if called || !result.BudgetExceeded || result.MetadataComplete || result.HardTimeBound || result.CaptureDuration != dohWarmFailureCaptureBudget+time.Nanosecond {
		t.Fatalf("overrun concealed or later metadata collected: %+v", result)
	}
}

func TestDohWarmFailureSnapshotMetadataBoundIsExplicit(t *testing.T) {
	identity := dohWarmFailureIdentity{Phase: "warmup-before-teardown", Arm: "ack-8-8", RunIndex: 2}
	value := &dohWarmFailureSnapshot{Identity: identity, MetadataComplete: true}
	value.Native.Connections = []h1FailureTCPConnectionSnapshot{{Node: strings.Repeat("x", dohWarmFailureMetadataCapacity)}}
	data, err := dohWarmFailureMetadata(value)
	var decoded dohWarmFailureSnapshot
	if err != nil || len(data) > dohWarmFailureMetadataCapacity || json.Unmarshal(data, &decoded) != nil || !decoded.MetadataTruncated || decoded.MetadataComplete || decoded.Identity != identity {
		t.Fatalf("metadata bound silently discarded identity/truncation: bytes=%d error=%v", len(data), err)
	}
	if value.MetadataTruncated || !value.MetadataComplete || len(value.Native.Connections) != 1 {
		t.Fatal("encoding mutated live snapshot")
	}
}
