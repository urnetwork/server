// Package flowtrace retains bounded metadata for an explicitly armed proxy
// diagnostic. It never stores packet payloads or changes forwarding decisions.
package flowtrace

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

const Capacity = 1024

type Flow struct {
	Local  netip.AddrPort `json:"local"`
	Origin netip.AddrPort `json:"origin"`
}

type Event struct {
	Cursor   uint64    `json:"cursor"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Protocol string    `json:"protocol,omitempty"`
	Flow     Flow      `json:"flow"`
	Provider string    `json:"provider,omitempty"`
	Sequence uint32    `json:"tcp_sequence,omitempty"`
	Bytes    int       `json:"tcp_payload_bytes,omitempty"`
}

type Snapshot struct {
	Session string    `json:"session"`
	Now     time.Time `json:"now"`
	Until   time.Time `json:"until"`
	Cursor  uint64    `json:"cursor"`
	Dropped uint64    `json:"dropped"`
	Lost    bool      `json:"lost"`
	Events  []Event   `json:"events"`
}

type StartRequest struct {
	Port uint16 `json:"port"`
}

type Recorder struct {
	mu      sync.Mutex
	session string
	until   time.Time
	port    uint16
	next    uint64
	dropped atomic.Uint64
	events  [Capacity]Event
}

func New(port uint16, now time.Time) (*Recorder, error) {
	if port == 0 {
		return nil, errors.New("target port is required")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return &Recorder{session: hex.EncodeToString(nonce[:]), until: now.Add(45 * time.Minute), port: port}, nil
}

// Provider aliases identify the actual return-envelope source within this
// trace session. They are not the local channel IDs exposed by GetExits.
func ProviderAlias(session string, id connect.Id) string {
	if id == (connect.Id{}) {
		return "unavailable"
	}
	mac := hmac.New(sha256.New, []byte(session))
	_, _ = mac.Write(id[:])
	return hex.EncodeToString(mac.Sum(nil)[:12])
}

func (r *Recorder) add(event Event) {
	if r == nil || !event.At.Before(r.until) || event.Flow.Origin.Port() != r.port {
		return
	}
	// Never delay the borrowed final-injection callback behind an API read.
	// A contended sample is explicitly lost, not silently attributed later.
	if !r.mu.TryLock() {
		r.dropped.Add(1)
		return
	}
	defer r.mu.Unlock()
	r.next++
	event.Cursor = r.next
	r.events[(r.next-1)%Capacity] = event
}

func (r *Recorder) Dial(protocol string, connection net.Conn, now time.Time) {
	if r == nil || connection == nil {
		return
	}
	local, localErr := netip.ParseAddrPort(connection.LocalAddr().String())
	origin, originErr := netip.ParseAddrPort(connection.RemoteAddr().String())
	if localErr != nil || originErr != nil {
		return
	}
	flow := Flow{netip.AddrPortFrom(local.Addr().Unmap(), local.Port()), netip.AddrPortFrom(origin.Addr().Unmap(), origin.Port())}
	r.add(Event{At: now, Kind: "dial", Protocol: protocol, Flow: flow})
}

// Called at the existing authenticated return callback, before TUN injection
// or WireGuard's source-NAT address is rewritten back to the client address.
// Batch packets may have different flows; the callback's advisory path is nil.
func (r *Recorder) Return(source connect.TransferPath, packets [][]byte, now time.Time) {
	if r == nil || !now.Before(r.until) {
		return
	}
	provider := ProviderAlias(r.session, source.SourceId)
	for _, packet := range packets {
		path, err := connect.ParseIpPath(packet)
		if err != nil || path.Protocol != connect.IpProtocolTcp || path.SourcePort != int(r.port) {
			continue
		}
		origin, originOK := netip.AddrFromSlice(path.SourceIp)
		local, localOK := netip.AddrFromSlice(path.DestinationIp)
		if !originOK || !localOK {
			continue
		}
		r.add(Event{
			At: now, Kind: "return", Provider: provider, Sequence: path.SequenceNumber, Bytes: path.TcpPayloadByteCount,
			Flow: Flow{netip.AddrPortFrom(local.Unmap(), uint16(path.DestinationPort)), netip.AddrPortFrom(origin.Unmap(), uint16(path.SourcePort))},
		})
	}
}

func (r *Recorder) Snapshot(after, afterDropped uint64, now time.Time) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := Snapshot{Session: r.session, Now: now, Until: r.until, Cursor: r.next, Dropped: r.dropped.Load()}
	oldest := uint64(1)
	if r.next > Capacity {
		oldest = r.next - Capacity + 1
	}
	result.Lost = after < oldest-1 || after > r.next || afterDropped != result.Dropped
	if after > r.next {
		return result
	}
	for cursor := max(oldest, after+1); cursor <= r.next; cursor++ {
		result.Events = append(result.Events, r.events[(cursor-1)%Capacity])
	}
	return result
}

func (r *Recorder) Cursor(now time.Time) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{Session: r.session, Now: now, Until: r.until, Cursor: r.next, Dropped: r.dropped.Load()}
}

// Attribute joins the exact proxy-side dial to its return packets. The
// diagnostic fixture has one outstanding request per protocol; more than one
// matching dial is ambiguous. WireGuard supplies its own inner source port;
// its server-side NAT changes only the source address, which is learned here.
func Attribute(snapshot Snapshot, protocol string, origin netip.AddrPort, localPort uint16) (Flow, []Event, error) {
	if snapshot.Lost || !snapshot.Now.Before(snapshot.Until) {
		return Flow{}, nil, errors.New("trace interval incomplete")
	}
	flows := map[Flow]bool{}
	dials := 0
	for _, event := range snapshot.Events {
		if protocol == "wireguard" {
			if origin.IsValid() && localPort != 0 && event.Kind == "return" && event.Flow.Origin == origin && event.Flow.Local.Port() == localPort {
				flows[event.Flow] = true
			}
		} else if (protocol == "http" || protocol == "socks") && event.Kind == "dial" && event.Protocol == protocol {
			dials++
			flows[event.Flow] = true
		}
	}
	if len(flows) != 1 || (protocol != "wireguard" && dials != 1) {
		return Flow{}, nil, errors.New("origin flow unavailable or ambiguous")
	}
	var flow Flow
	for matched := range flows {
		flow = matched
	}
	returns := []Event{}
	for _, event := range snapshot.Events {
		if event.Kind == "return" && event.Flow == flow && event.Bytes > 0 {
			returns = append(returns, event)
		}
	}
	if len(returns) == 0 {
		return flow, nil, errors.New("no origin payload observed")
	}
	return flow, returns, nil
}
