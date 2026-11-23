//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The it-bridge case's world (it_bridge_test.go's header): one network of the World, its devices by role, each with
// its bridge (it_bridge_wire_test.go), the destinations they forward to (it_bridge_targets_test.go), the network's block
// and ACL document, the filings that join and leave subnets and set up and remove forwards, each on a timeline the
// oracle reads, and the clients' attempts. A second network of the case on the same World (AddPeer) is built alike,
// with its own name, device names, document and the block its own bootstrap drew, for a share between the two
// (it_bridge_shares_test.go).
//
//	owner    the network's first device: it bootstraps the network, whose first head carries the block the bootstrap
//	         drew, saves the ACL document (`ur acl push`), and files a share's acts as the network's admin; it runs no
//	         bridge
//	subnets  S devices, each in front of one subnet, a prefix carved from SubnetPrefixes: its route (`ur bridge expose`)
//	         joins the subnet to the network and its BridgeWithdraw, reset, takes it out (§17.8's revocation row, so
//	         clients are refused at once rather than drained for 10 minutes)
//	campus   one more device in front of CampusPrefix, which the first subnets nest in: its route joins once and stays,
//	         so a flow to a nested subnet's address must reach that subnet's device while the client routes the subnet,
//	         the longest prefix winning (§17.7), and never fall back to the campus when the subnet leaves (§17.7's HA
//	         row; §17.8's revocation row holds the subnet's entry as a reject); a client no rule of the subnet's names
//	         routes such an address by the campus's rule alone, and the campus answers it
//	hosts    H devices holding the P port forwards between them, each a BridgeMap of a port of the host's overlay
//	         address to its target service on the host's loopback (`ur bridge expose :<port>=127.0.0.1:<port>`), set
//	         up and removed the same way; each host claims its overlay address first (`ur tun ip`), which clients also
//	         ping (§17.6's rule 3)
//	clients  C devices whose userspace stacks send to the subnets' addresses, the forwarded ports and the hosts'
//	         addresses, under the document's grants
//
// The document grants by a matrix of settings: client c routes subnet s for TCP and UDP, for TCP alone, or not at all,
// by (c+s) mod SubnetGrantModulus; dials forward f's port unless (c+f) mod ForwardGrantModulus is ForwardDeniedResidue;
// and pings host h unless (c+h) mod EchoGrantModulus is EchoDeniedResidue. A subnet's rules name its device as via, so
// its route stands under them (§17.7), and a forward's names its host with its port, so its map does. A denied range a
// client has no rule for leaves its router no entry, so its packets take the default route, which goes nowhere here; a
// port its rule leaves out still enters the link and is the forwarding role's to refuse (§17.6's rule 6).
//
// The filings go through each device's BridgeFiler, the path `ur bridge` and MCP's bridge tools take, through §16.6's
// stock approval gate over the device's follower (World.InstallApprovalGate), whose standing grants are the
// document's: a route or map the document's standing route or dial rule covers passes it with nothing filed, the gate
// asking the engine whether the rule it files stands (AclEngine.StandingRule, the predicate every reader holds a rule
// filed with no approval in force by, AclLocalPolicy.StandingCovers; §17.7, §17.8's approval row). Nothing stands in
// for a person here, so an exposure the gate held for one fails its join. The case found the gate holding every
// exposure, its standing check covering only an action with grant fields, and approved them through the World's
// worldDeviceGate until approval-gate-standing-exposures (sidecar 99d7f101 to e1c83b88) landed.
//
// A network's block is the one its bootstrap drew into its first head (§17.4's first block row; the case set it as an
// admin's act until bridge-block-bootstrap, sidecar 58fd1846 to a49e3bff, landed): the case reads it as a filer reads
// the block in force (readBlock), and holds every host's claimed address to §17.4's host address row in it
// (checkHostAddress) and every device's compile to holding it (checkCompiledBlock). A share's aliases are host bits in
// the accepting network's block, which is that network's own draw.
//
// The case keeps its World on FakeGkm (WorldSettings.FakeGkm), where a World runs real keys by default since
// keys-late-delivery and gkm-repo-create-race landed: a filing seals its body under the config repo's key, which each
// Sidecar holds through the follower's keys the case installs (itBridgeConfigKeys), since a follower installs them
// itself only over a key client of forwarding setup's, which neither the World's FakeGkm client nor a real-keys
// device's key client (sidecart3's t3WorldKeysGkm) is; and the config repo's group, which a real-keys bootstrap forms
// at gkm, is formed at FakeGkm by the owner's key client (formConfigGroup). On real keys only the second would go, so
// the case stays on FakeGkm, as it was built.
//
// Each filing's timeline records when it was begun, when it returned, the log position it landed at, and when every
// device's bridge had adopted it (followed, compiled, published, serving). Safe for concurrent use once made.

package sidecare2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/sidecart3"
)

// What the it-bridge case runs with (QD-702): every count, address, grant, bound and pause.
type ItBridgeSettings struct {
	// The World the network runs in; nil takes DefaultWorldSettings with no networks of its own.
	World *WorldSettings
	// Whether the network subscribes to Pro before its bridges start (World.VerifyNetwork), so it runs under §20.4's Pro
	// column rather than the World's own tier, the Community column: a share's two networks need Pro's
	// acl_network_shares, which Community leaves off (the sharing network's invite and network:* selector, the accepting
	// network's claim and network: selector). false: the case's links fit the Community column's sidecar_clients (20),
	// since a sidecar's window clients count against none (§20.8; window-clients-limit, server f8b013543, before which
	// the case ran on Pro and server closed all but about two of its window clients at the exchange).
	Pro bool
	// Each device's bridge.
	Wire *ItBridgeWireSettings
	// The concurrent connections the exchange's handler takes from one address (server's
	// ConnectionRateLimitSettings.MaxTotalConnectionCount, 200 in production, past which each connection waits 30 s and
	// is refused): every client of the World dials from 127.0.0.1, where production's come from many hosts, and the
	// case's sidecars, links' window clients and the edges' clients pass 200 from it. 4,000.
	ExchangeConnectionMaxCount int
	// S, the subnets, each behind a device of its own: 8.
	SubnetCount int
	// P, the port forwards, over ForwardHostCount hosts: 8 over 4.
	ForwardCount     int
	ForwardHostCount int
	// C, the clients: 4.
	ClientCount int
	// The ranges subnets are carved from, each a SubnetBits-long prefix in order: documentation ranges (RFC 5737), so
	// no subnet is anyone's LAN, /28s of 203.0.113.0/25 and then 198.51.100.0/24, at most 24.
	SubnetPrefixes []netip.Prefix
	SubnetBits     int
	// The campus's prefix (the file's header), which the subnets of 203.0.113.0/25 nest in, its own hosts in the upper
	// /25: 203.0.113.0/24. Invalid runs no campus.
	CampusPrefix netip.Prefix
	// The port every subnet's hosts answer on, TCP and UDP: 7000.
	SubnetServicePort uint16
	// Forward f maps port ForwardPortBase+f of its host's overlay address; every ForwardUdpEvery-th forward, from the
	// second, maps UDP and the others TCP: 8000 and 2.
	ForwardPortBase uint16
	ForwardUdpEvery int
	// The prefixes a network's bootstrap draws its first block from (§17.4's first block row), which the case holds each
	// network's block to (readBlock): the /16s of 100.64.0.0/10 but 100.64.0.0/16 and 100.100.0.0/16, 62 of them.
	BlockBits       int
	BlockRange      netip.Prefix
	BlockExclusions []netip.Prefix
	// The services range at the bottom of a block, which no host's address takes (§17.4's host address row): a /24.
	BlockServicesBits int
	// The grant matrix (the file's header).
	SubnetGrantModulus   int
	SubnetDeniedResidue  int
	SubnetTcpOnlyResidue int
	ForwardGrantModulus  int
	ForwardDeniedResidue int
	EchoGrantModulus     int
	EchoDeniedResidue    int
	// How long, in real time, every device's bridge may take to adopt a filing, and how often a wait looks, since a
	// component's compile and serving raise nothing: 60 s, 25 ms.
	AdoptTimeout      time.Duration
	AdoptPollInterval time.Duration
	// How long a filing may take to land and show in the filer's ledger: 60 s.
	FilingTimeout time.Duration
	// The bound after a subnet leaves or a forward is removed, from the withdrawal's return, past which nothing may
	// reach it: 20 s.
	LeaveBound time.Duration
	// The churn: each subnet and forward leaves and joins again ChurnCycleCount times, held joined and held out for a
	// random time in each range.
	ChurnCycleCount int
	JoinedHoldMin   time.Duration
	JoinedHoldMax   time.Duration
	LeftHoldMin     time.Duration
	LeftHoldMax     time.Duration
	// The clients' traffic: each client sends bursts of up to BurstAttemptCount attempts at once, one goroutine each,
	// with a pause of up to BurstPauseMax between bursts; a stream holds one TCP connection for StreamRequestCount
	// requests, StreamPause apart.
	BurstAttemptCount  int
	BurstPauseMax      time.Duration
	StreamRequestCount int
	StreamPause        time.Duration
	// How long one attempt may take, how many datagrams a UDP attempt sends, and how long an echo waits.
	AttemptTimeout time.Duration
	UdpTryCount    int
	EchoTimeout    time.Duration
	// The largest request or answer a destination reads: 4 KiB.
	ReadByteCount int
	// A stable window shorter than this, an incarnation's from its adoption to its withdrawal, is not held to being
	// reached at least once: 3 s.
	LivenessMinWindow time.Duration
	// How many tries the churn's probe of a subnet or forward joined again gives the one granted client and protocol
	// it picks, SweepPause apart, so every incarnation the churn joins has attempts its liveness is judged by: 6.
	ComebackTryCount int
	// How many tries a sweep gives each granted pair before it fails it, and each denied pair to be answered, and how
	// long it pauses between: 6, 2 and 1 s.
	SweepTryCount       int
	SweepDeniedTryCount int
	SweepPause          time.Duration
	// Chaos: how long a restarted device's bridge stays down, and when in the churn each restart comes, after the
	// churn begins.
	ChaosDownPause       time.Duration
	ChaosDeviceAfter     time.Duration
	ChaosHandlerAfter    time.Duration
	ChaosSecondHostAfter time.Duration
	// How long the ledgers may take to settle at the end: every relayed contract closed by both its parties, and every
	// share contract settled and its debit flushed to its dialer's balances.
	LedgerTimeout      time.Duration
	LedgerPollInterval time.Duration
	// The balances one page of the transfer debit flush visits (model.FlushTransferDebits' maxBalances, at most 64),
	// which the ledgers run for the share contracts' settles as taskworker runs it: 64.
	LedgerFlushBalanceCount int
	// Route grants (it_bridge_grants_test.go): whether the case runs them, the document then carrying a requests rule
	// through which every client the grant matrix denies a subnet that nests in no campus asks a route grant on it, and
	// how long the first such grant asks to last before it expires, long enough to outlast its probes; the rest are
	// revoked once probed. false, 90 s.
	RouteGrants        bool
	RouteGrantLifetime time.Duration
	// The network's name in the World, and what every device's name starts with: itb and none. A second network of
	// the case (AddPeer) takes its own of both.
	NetworkName      string
	DeviceNamePrefix string
	// Shares (it_bridge_shares_test.go): whether the document carries the requests rule a share's inbound grant is
	// activated under, from any network (network:*) to the share's target hosts, every forward host and subnet device,
	// with autogroup:admin as approver, as the implicit document's shares rule reads (§8.5). false.
	ShareTargets bool
	// The drain an accepting network's release of a share's aliases asks (BridgeShareFilerSettings.ReleaseDrain), so a
	// released alias leaves the network's tables within the case: 1 s, where 0 would take the ledger's 300 s.
	ShareReleaseDrain time.Duration
	// How many times a share's invite is filed while api refuses it 403, ShareInvitePause apart: §21.7's gate reads the
	// sharing network's standing from memory and loads it at its first ask, refusing that ask as unread, so an invite
	// on a cold gate is refused once before the load lands (it_bridge_shares_test.go). 10, 1 s.
	ShareInviteTryCount int
	ShareInvitePause    time.Duration
	// How old the sharing network is made before its invite (worldAgeNetwork): §21.7's probation, a network's first
	// 7 days, gives it no outbound share, and a case cannot wait them out. 8 days.
	ShareNetworkAge time.Duration
	// The case's randomness: targets, addresses, holds and pauses. Logged.
	Seed uint64
}

func DefaultItBridgeSettings() *ItBridgeSettings {
	return &ItBridgeSettings{
		Wire:                       DefaultItBridgeWireSettings(),
		Pro:                        false,
		ExchangeConnectionMaxCount: 4000,
		SubnetCount:                8,
		ForwardCount:               8,
		ForwardHostCount:           4,
		ClientCount:                4,
		SubnetPrefixes:             []netip.Prefix{netip.MustParsePrefix("203.0.113.0/25"), netip.MustParsePrefix("198.51.100.0/24")},
		SubnetBits:                 28,
		CampusPrefix:               netip.MustParsePrefix("203.0.113.0/24"),
		SubnetServicePort:          7000,
		ForwardPortBase:            8000,
		ForwardUdpEvery:            2,
		BlockBits:                  16,
		BlockRange:                 netip.MustParsePrefix("100.64.0.0/10"),
		BlockExclusions:            []netip.Prefix{netip.MustParsePrefix("100.64.0.0/16"), netip.MustParsePrefix("100.100.0.0/16")},
		BlockServicesBits:          24,
		SubnetGrantModulus:         4,
		SubnetDeniedResidue:        3,
		SubnetTcpOnlyResidue:       2,
		ForwardGrantModulus:        4,
		ForwardDeniedResidue:       1,
		EchoGrantModulus:           3,
		EchoDeniedResidue:          0,
		AdoptTimeout:               60 * time.Second,
		AdoptPollInterval:          25 * time.Millisecond,
		FilingTimeout:              60 * time.Second,
		LeaveBound:                 20 * time.Second,
		ChurnCycleCount:            3,
		JoinedHoldMin:              4 * time.Second,
		JoinedHoldMax:              12 * time.Second,
		LeftHoldMin:                2 * time.Second,
		LeftHoldMax:                6 * time.Second,
		BurstAttemptCount:          4,
		BurstPauseMax:              300 * time.Millisecond,
		StreamRequestCount:         8,
		StreamPause:                250 * time.Millisecond,
		AttemptTimeout:             5 * time.Second,
		UdpTryCount:                3,
		EchoTimeout:                3 * time.Second,
		ReadByteCount:              4 * 1024,
		LivenessMinWindow:          3 * time.Second,
		ComebackTryCount:           6,
		SweepTryCount:              6,
		SweepDeniedTryCount:        2,
		SweepPause:                 time.Second,
		ChaosDownPause:             2 * time.Second,
		ChaosDeviceAfter:           8 * time.Second,
		ChaosHandlerAfter:          20 * time.Second,
		ChaosSecondHostAfter:       32 * time.Second,
		LedgerTimeout:              2 * time.Minute,
		LedgerPollInterval:         250 * time.Millisecond,
		LedgerFlushBalanceCount:    64,
		RouteGrants:                false,
		RouteGrantLifetime:         90 * time.Second,
		NetworkName:                "itb",
		DeviceNamePrefix:           "",
		ShareTargets:               false,
		ShareReleaseDrain:          time.Second,
		ShareInviteTryCount:        10,
		ShareInvitePause:           time.Second,
		ShareNetworkAge:            8 * 24 * time.Hour,
		Seed:                       uint64(time.Now().UnixNano()),
	}
}

// What a client's rules let it send to a subnet (the file's header).
type itBridgeSubnetGrant int

const (
	itBridgeSubnetDenied itBridgeSubnetGrant = iota
	itBridgeSubnetTcpOnly
	itBridgeSubnetTcpUdp
)

// The rules client c holds for subnet s.
func (self *ItBridgeSettings) subnetGrant(client int, subnet int) itBridgeSubnetGrant {
	switch (client + subnet) % self.SubnetGrantModulus {
	case self.SubnetDeniedResidue:
		return itBridgeSubnetDenied
	case self.SubnetTcpOnlyResidue:
		return itBridgeSubnetTcpOnly
	}
	return itBridgeSubnetTcpUdp
}

// Whether client c may dial forward f's port.
func (self *ItBridgeSettings) forwardGranted(client int, forward int) bool {
	return (client+forward)%self.ForwardGrantModulus != self.ForwardDeniedResidue
}

// Whether client c may ping host h's overlay address.
func (self *ItBridgeSettings) echoGranted(client int, host int) bool {
	return (client+host)%self.EchoGrantModulus != self.EchoDeniedResidue
}

// Forward f's protocol, "tcp" or "udp".
func (self *ItBridgeSettings) forwardProtocol(forward int) string {
	if 0 < self.ForwardUdpEvery && forward%self.ForwardUdpEvery == self.ForwardUdpEvery-1 {
		return "udp"
	}
	return "tcp"
}

// The subnets' prefixes, SubnetCount of them carved in order from SubnetPrefixes.
func (self *ItBridgeSettings) subnetPrefixes() ([]netip.Prefix, error) {
	prefixes := []netip.Prefix{}
	for _, parent := range self.SubnetPrefixes {
		if self.SubnetBits < parent.Bits() || 32 < self.SubnetBits {
			return nil, fmt.Errorf("a /%d subnet does not fit %s", self.SubnetBits, parent)
		}
		size := uint32(1) << (32 - self.SubnetBits)
		count := uint32(1) << (self.SubnetBits - parent.Bits())
		base := itBridgeIpv4(parent.Masked().Addr())
		for index := range count {
			if len(prefixes) == self.SubnetCount {
				return prefixes, nil
			}
			prefixes = append(prefixes, netip.PrefixFrom(itBridgeAddr(base+index*size), self.SubnetBits))
		}
	}
	if len(prefixes) < self.SubnetCount {
		return nil, fmt.Errorf("%d subnets of /%d do not fit the ranges given", self.SubnetCount, self.SubnetBits)
	}
	return prefixes, nil
}

// An IPv4 address as a number.
func itBridgeIpv4(address netip.Addr) uint32 {
	octets := address.As4()
	return uint32(octets[0])<<24 | uint32(octets[1])<<16 | uint32(octets[2])<<8 | uint32(octets[3])
}

// A number as an IPv4 address.
func itBridgeAddr(ipv4 uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(ipv4 >> 24), byte(ipv4 >> 16), byte(ipv4 >> 8), byte(ipv4)})
}

// One incarnation of a subnet's route or a forward's map: its filing and its withdrawal, each as the case saw it.
type itBridgeIncarnation struct {
	Number int64
	RuleId []byte
	// Just before the filing began, when it returned, the log position it landed at, and when every bridge had
	// adopted it; zero before each.
	FiledAt   time.Time
	LandedAt  time.Time
	Pin       uint64
	AdoptedAt time.Time
	// The same for its withdrawal; zero while it is joined.
	WithdrawAt        time.Time
	WithdrawnAt       time.Time
	WithdrawPin       uint64
	WithdrawAdoptedAt time.Time
}

// A subnet's or a forward's incarnations, oldest first. Safe for concurrent use.
type itBridgeTimeline struct {
	stateLock    sync.Mutex
	incarnations []*itBridgeIncarnation
}

// Opens the next incarnation, its filing begun now.
func (self *itBridgeTimeline) begin() *itBridgeIncarnation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	incarnation := &itBridgeIncarnation{Number: int64(len(self.incarnations) + 1), FiledAt: time.Now()}
	self.incarnations = append(self.incarnations, incarnation)
	return incarnation
}

// Edits an incarnation under the timeline's lock.
func (self *itBridgeTimeline) update(edit func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	edit()
}

// A copy of every incarnation.
func (self *itBridgeTimeline) Incarnations() []itBridgeIncarnation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	incarnations := []itBridgeIncarnation{}
	for _, incarnation := range self.incarnations {
		incarnations = append(incarnations, *incarnation)
	}
	return incarnations
}

// The newest incarnation, nil before one.
func (self *itBridgeTimeline) newest() *itBridgeIncarnation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.incarnations) == 0 {
		return nil
	}
	return self.incarnations[len(self.incarnations)-1]
}

// Whether something may have reached the destination over [start, end]: an incarnation's filing began at or before
// end, and its withdrawal had not returned bound before start.
func (self *itBridgeTimeline) Allows(start time.Time, end time.Time, bound time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, incarnation := range self.incarnations {
		if end.Before(incarnation.FiledAt) {
			continue
		}
		if !incarnation.WithdrawnAt.IsZero() && incarnation.WithdrawnAt.Add(bound).Before(start) {
			continue
		}
		if incarnation.WithdrawnAt.IsZero() && !incarnation.WithdrawAt.IsZero() && incarnation.WithdrawAt.Add(bound).Before(start) {
			// a withdrawal begun and never returned bounds it from its start
			continue
		}
		return true
	}
	return false
}

// What one destination of the case is: a subnet, a forward or a host's address that clients ping.
type itBridgeTargetKind string

const (
	itBridgeTargetSubnet  itBridgeTargetKind = "subnet"
	itBridgeTargetForward itBridgeTargetKind = "forward"
	itBridgeTargetEcho    itBridgeTargetKind = "echo"
)

// One destination (the file's header).
type itBridgeTarget struct {
	kind  itBridgeTargetKind
	index int
	name  string
	// the device that serves it
	device *itBridgeDevice
	// the destination's service; nil for an echo, which the host's kernel answers
	service *itBridgeService
	// nil for an echo: a host's address never leaves
	timeline *itBridgeTimeline
	// a subnet's prefix
	prefix netip.Prefix
	// a forward's protocol and port of its host's address
	protocol string
	port     uint16
}

// The protocols a client sends it.
func (self *itBridgeTarget) protocols() []string {
	switch self.kind {
	case itBridgeTargetSubnet:
		return []string{"tcp", "udp"}
	case itBridgeTargetForward:
		return []string{self.protocol}
	}
	return []string{"icmp"}
}

// The case's world (the file's header).
type ItBridgeWorld struct {
	t        testing.TB
	settings *ItBridgeSettings
	world    *World
	network  *WorldNetwork
	random   *rand.Rand
	// what every window client's transport dials with: the exchange's strategy and transport settings
	exchangeSettings  *sidecart3.ExchangeSettings
	clientStrategy    *connect.ClientStrategy
	transportSettings *connect.PlatformTransportSettings

	owner   *itBridgeDevice
	subnets []*itBridgeTarget
	// the campus, nil for none
	campus   *itBridgeTarget
	hosts    []*itBridgeDevice
	forwards []*itBridgeTarget
	echoes   []*itBridgeTarget
	clients  []*itBridgeDevice
	// every device that runs a bridge
	devices []*itBridgeDevice
	// the network's block, as its bootstrap drew it into the first head (readBlock)
	block netip.Prefix
	// each host's overlay address, by the host's index
	hostAddresses []netip.Addr
	// the route grants the clients ask (it_bridge_grants_test.go); none unless RouteGrants
	routeGrants []*itBridgeRouteGrant
	// the document the owner saved
	policy string

	randomLock sync.Mutex

	stateLock sync.Mutex
	alarms    []string
	attempts  []*ItBridgeAttempt
	chaos     []*itBridgeChaos
	// the other networks of the case on this World (AddPeer), and the shares between them and this one
	// (it_bridge_shares_test.go)
	peers  []*ItBridgeWorld
	shares []*itBridgeShare
	closed bool
}

// One window in which chaos may refuse what is otherwise reachable: a device's bridge, or every one when device is nil,
// from start to end.
type itBridgeChaos struct {
	device *itBridgeDevice
	what   string
	start  time.Time
	end    time.Time
}

// The case's world, made inside a TestEnv that applies the migrations: the World, its network of 1+S+H+C devices, the
// destinations, the block its bootstrap drew and the document, and every device's bridge adopted at the tip. Closed at
// t's end, or by Close. settings nil takes the defaults.
func NewItBridgeWorld(t testing.TB, settings *ItBridgeSettings) *ItBridgeWorld {
	t.Helper()
	if settings == nil {
		settings = DefaultItBridgeSettings()
	}
	if settings.Wire == nil {
		settings.Wire = DefaultItBridgeWireSettings()
	}
	t.Logf("it bridge: seed %d", settings.Seed)
	worldSettings := settings.World
	if worldSettings == nil {
		worldSettings = DefaultWorldSettings()
	}
	// the case forms its config repo's group at the World's FakeGkm and installs each follower's config keys in its
	// Sidecar over that client (formConfigGroup, itBridgeConfigKeys), so its World runs FakeGkm, not the real keys a
	// World runs by default
	worldSettings.FakeGkm = true
	if worldSettings.T3 == nil {
		worldSettings.T3 = sidecart3.DefaultT3WorldSettings()
	}
	// every follower checks a GrantActivate's decisions as ur daemon's does, so a route grant's activation counts at
	// every device (it_bridge_grants_test.go)
	t3Settings := *worldSettings.T3
	t3Settings.FollowerSpendCheck = true
	worldSettings.T3 = &t3Settings
	if worldSettings.T3.Exchange == nil {
		worldSettings.T3.Exchange = sidecart3.DefaultExchangeSettings()
	}
	handlerSettings := *worldSettings.T3.Exchange.Handler
	handlerSettings.ConnectionRateLimitSettings.MaxTotalConnectionCount = settings.ExchangeConnectionMaxCount
	exchangeSettings := *worldSettings.T3.Exchange
	exchangeSettings.Handler = &handlerSettings
	worldSettings.T3.Exchange = &exchangeSettings
	world := NewWorld(t, worldSettings)
	return newItBridgeNetwork(t, world, settings, &exchangeSettings)
}

// Another network of the case on the World of self, with its own devices, destinations, drawn block and document, built
// as NewItBridgeWorld builds the first (it_bridge_shares_test.go's accepting network). Its settings' World and
// ExchangeConnectionMaxCount are the first network's; its NetworkName and DeviceNamePrefix should differ from the
// first's, so its devices' names are its own. Closed at t's end, before the first.
func (self *ItBridgeWorld) AddPeer(t testing.TB, settings *ItBridgeSettings) *ItBridgeWorld {
	t.Helper()
	if settings == nil {
		settings = DefaultItBridgeSettings()
	}
	if settings.Wire == nil {
		settings.Wire = DefaultItBridgeWireSettings()
	}
	t.Logf("it bridge: %s's seed %d", settings.NetworkName, settings.Seed)
	peer := newItBridgeNetwork(t, self.world, settings, self.exchangeSettings)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.peers = append(self.peers, peer)
	return peer
}

// One network of the case on world (NewItBridgeWorld), its window clients dialing with exchangeSettings' strategy and
// transport.
func newItBridgeNetwork(t testing.TB, world *World, settings *ItBridgeSettings, exchangeSettings *sidecart3.ExchangeSettings) *ItBridgeWorld {
	t.Helper()
	self := &ItBridgeWorld{
		t:                 t,
		settings:          settings,
		world:             world,
		random:            rand.New(rand.NewPCG(settings.Seed, 0)),
		exchangeSettings:  exchangeSettings,
		clientStrategy:    connect.NewClientStrategy(world.T3().Context(), exchangeSettings.ClientStrategy),
		transportSettings: exchangeSettings.Transport,
	}
	t.Cleanup(self.Close)
	ctx, cancel := world.StepContext(t.Context(), world.HostsSettings().CaseTimeout)
	defer cancel()
	if err := self.build(ctx); err != nil {
		self.FailProgress(t, "the it bridge world", err, nil)
	}
	return self
}

// The world's network and devices, destinations, block, document and bridges (NewItBridgeWorld).
func (self *ItBridgeWorld) build(ctx context.Context) error {
	settings := self.settings
	prefixes, err := settings.subnetPrefixes()
	if err != nil {
		return err
	}
	deviceCount := 1 + settings.SubnetCount + settings.ForwardHostCount + settings.ClientCount
	if settings.CampusPrefix.IsValid() {
		deviceCount += 1
	}
	network, err := self.world.AddNetwork(ctx, settings.NetworkName, deviceCount, nil)
	if err != nil {
		return fmt.Errorf("the network: %w", err)
	}
	self.network = network
	if settings.Pro {
		if err := self.world.VerifyNetwork(ctx, network, true, nil); err != nil {
			return fmt.Errorf("the network's Pro balance: %w", err)
		}
	}
	postureRoot := self.t.TempDir()
	newDevice := func(t3 *sidecart3.T3Device, role string, index int) *itBridgeDevice {
		name := fmt.Sprintf("%s%s-%d", settings.DeviceNamePrefix, role, index)
		return &itBridgeDevice{
			world:            self,
			t3:               t3,
			name:             name,
			settings:         settings.Wire,
			postureDirectory: filepath.Join(postureRoot, name),
			dialer:           newItBridgeDialer(name),
		}
	}
	self.owner = newDevice(network.Clients[0], "owner", 0)
	next := 1
	for index := range settings.SubnetCount {
		subnetDevice := newDevice(network.Clients[next], "router", index)
		next += 1
		name := fmt.Sprintf("%ssubnet-%d", settings.DeviceNamePrefix, index)
		service, err := newItBridgeService(name, settings.ReadByteCount)
		if err != nil {
			return err
		}
		subnetDevice.dialer.AddSubnet(prefixes[index], settings.SubnetServicePort, service)
		self.subnets = append(self.subnets, &itBridgeTarget{
			kind: itBridgeTargetSubnet, index: index, name: name, device: subnetDevice, service: service,
			timeline: &itBridgeTimeline{}, prefix: prefixes[index],
		})
		self.devices = append(self.devices, subnetDevice)
	}
	if settings.CampusPrefix.IsValid() {
		campusDevice := newDevice(network.Clients[next], "campus", 0)
		next += 1
		campusName := settings.DeviceNamePrefix + "campus"
		service, err := newItBridgeService(campusName, settings.ReadByteCount)
		if err != nil {
			return err
		}
		campusDevice.dialer.AddSubnet(settings.CampusPrefix, settings.SubnetServicePort, service)
		// the campus's index in the grant matrix follows the subnets'
		self.campus = &itBridgeTarget{
			kind: itBridgeTargetSubnet, index: settings.SubnetCount, name: campusName, device: campusDevice, service: service,
			timeline: &itBridgeTimeline{}, prefix: settings.CampusPrefix,
		}
		self.devices = append(self.devices, campusDevice)
	}
	for index := range settings.ForwardHostCount {
		host := newDevice(network.Clients[next], "host", index)
		next += 1
		self.hosts = append(self.hosts, host)
		self.devices = append(self.devices, host)
		self.echoes = append(self.echoes, &itBridgeTarget{kind: itBridgeTargetEcho, index: index, name: host.name, device: host})
	}
	for index := range settings.ForwardCount {
		host := self.hosts[index%len(self.hosts)]
		name := fmt.Sprintf("%sforward-%d", settings.DeviceNamePrefix, index)
		service, err := newItBridgeService(name, settings.ReadByteCount)
		if err != nil {
			return err
		}
		protocol := settings.forwardProtocol(index)
		host.dialer.AddTarget(protocol, service)
		self.forwards = append(self.forwards, &itBridgeTarget{
			kind: itBridgeTargetForward, index: index, name: name, device: host, service: service,
			timeline: &itBridgeTimeline{}, protocol: protocol, port: settings.ForwardPortBase + uint16(index),
		})
	}
	for index := range settings.ClientCount {
		client := newDevice(network.Clients[next], "client", index)
		next += 1
		self.clients = append(self.clients, client)
		self.devices = append(self.devices, client)
	}
	if settings.RouteGrants {
		self.routeGrants = self.newRouteGrants()
		if len(self.routeGrants) == 0 {
			return errors.New("the grant matrix denies no client a subnet that nests in no campus, so no client asks a route grant")
		}
	}

	if err := self.formConfigGroup(ctx); err != nil {
		return err
	}
	// the block the bootstrap drew into the first head (the file's header), which no admin sets here
	if self.block, err = self.readBlock(ctx); err != nil {
		return fmt.Errorf("%s's block: %w", settings.NetworkName, err)
	}
	// the config repo's keys in each Sidecar, which a filing seals its body under: a follower installs its own only
	// over a key client with forwarding setup's key parts (bridge_setup_sources.go), which the World's FakeGkm client
	// has none of, so the case installs the follower's as that would
	for _, device := range slices.Concat([]*itBridgeDevice{self.owner}, self.devices) {
		remove := device.t3.Sidecar().InstallConfigKeys(&itBridgeConfigKeys{follower: device.t3.Follower()})
		self.world.T3().OnClose(remove)
	}
	// filings go through §16.6's stock gate over each filer's follower, its standing grants the document's (the file's
	// header)
	for _, filer := range slices.Concat([]*itBridgeDevice{self.owner}, self.routerDevices(), self.hosts) {
		if _, err := self.world.InstallApprovalGate(ctx, filer.t3); err != nil {
			return err
		}
		var err error
		if filer == self.owner {
			// the owner, an admin's device, also files the shares' acts (it_bridge_shares_test.go)
			filer.filer, err = self.newShareFiler(filer)
		} else {
			filer.filer, err = self.newFiler(filer)
		}
		if err != nil {
			return err
		}
	}
	self.policy, err = self.policySource()
	if err != nil {
		return err
	}
	// the document's save waits out any base of the network's last minute (one a minute, §7.9) while the bridges start
	saved := make(chan error, 1)
	go func() {
		saved <- self.saveDocument(ctx, self.policy, nil)
	}()
	for _, device := range self.devices {
		if err := device.startWire(); err != nil {
			<-saved
			return err
		}
	}
	for index, host := range self.hosts {
		filing, err := host.filer.ClaimAddress(ctx, &sidecar.BridgeFileOrigin{Act: sidecar.SidecarActConfirmed}, netip.Addr{})
		if err != nil {
			<-saved
			return fmt.Errorf("%s's address: %w", host.name, err)
		}
		if err := self.checkHostAddress(filing.Address); err != nil {
			<-saved
			return fmt.Errorf("%s's address: %w", host.name, err)
		}
		self.hostAddresses = append(self.hostAddresses, filing.Address)
		self.echoes[index].prefix = netip.PrefixFrom(filing.Address, 32)
	}
	if err := <-saved; err != nil {
		return fmt.Errorf("the document: %w", err)
	}
	if err := self.awaitDocument(ctx); err != nil {
		return err
	}
	if err := self.installGrantGates(ctx); err != nil {
		return err
	}
	if err := self.AwaitAdopted(ctx, self.tip()); err != nil {
		return err
	}
	return self.checkCompiledBlock()
}

// The network's block as its bootstrap drew it into the first head (§17.4's first block row), read as a filer reads
// the block in force: the head the owner's newest verified base names, its record judged at that state
// (sidecar.ReadBridgeBlock). Refused for a head that holds none, a record the state refuses, a prefix outside the draw
// (ItBridgeSettings.BlockRange), and a block that drains another, where a first block replaces none.
func (self *ItBridgeWorld) readBlock(ctx context.Context) (netip.Prefix, error) {
	follower := self.owner.t3.Follower()
	client := follower.Client()
	if client == nil || client.State() == nil || client.State().Base() == nil {
		return netip.Prefix{}, fmt.Errorf("%s's follower holds no base", self.owner.name)
	}
	heads := follower.Heads()
	if heads == nil {
		return netip.Prefix{}, fmt.Errorf("%s's follower reads no heads", self.owner.name)
	}
	state := client.State()
	base := state.Base()
	head, err := heads.ReadConfigHead(ctx, base.GetConfigVersion(), base.GetConfigHeadSha256())
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("the head base %d names: %w", base.GetVersion(), err)
	}
	block, err := sidecar.ReadBridgeBlock(head, state, nil)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("the head base %d names: %w", base.GetVersion(), err)
	}
	if block == nil {
		return netip.Prefix{}, fmt.Errorf("the head base %d names holds no block, where the bootstrap draws the first (§17.4)", base.GetVersion())
	}
	settings := self.settings
	prefix := bridgeWirePrefix(block.GetIpv4Prefix())
	if prefix.Bits() != settings.BlockBits || !settings.BlockRange.Contains(prefix.Addr()) || slices.Contains(settings.BlockExclusions, prefix) {
		return netip.Prefix{}, fmt.Errorf("the head base %d names holds the block %v, none of the /%d of %s but %v that the bootstrap draws from", base.GetVersion(),
			block.GetIpv4Prefix(), settings.BlockBits, settings.BlockRange, settings.BlockExclusions)
	}
	if block.GetPreviousPrefix() != nil {
		return netip.Prefix{}, fmt.Errorf("the head base %d names holds the block %s draining %v, where a first block replaces none", base.GetVersion(), prefix,
			block.GetPreviousPrefix())
	}
	return prefix, nil
}

// Whether address, one a host claimed with none chosen, is what §17.4's host address row draws in the network's block
// while its lower /17 has room: a /32 of that lower /17 outside the services range at the block's bottom
// (BlockServicesBits), never x.y.z.0 or x.y.z.255. The error says which it is not.
func (self *ItBridgeWorld) checkHostAddress(address netip.Addr) error {
	lower := netip.PrefixFrom(self.block.Masked().Addr(), self.block.Bits()+1)
	services := netip.PrefixFrom(self.block.Masked().Addr(), self.settings.BlockServicesBits)
	last := address.As4()[3]
	if !lower.Contains(address) || services.Contains(address) || last == 0 || last == 255 {
		return fmt.Errorf("%s, where the network's block %s draws a host's address from %s outside %s, never at .0 or .255", address, self.block, lower, services)
	}
	return nil
}

// Whether every device's bridge compiled the network's block (readBlock), as every reader of the heads takes it: an
// error naming each that holds another or none.
func (self *ItBridgeWorld) checkCompiledBlock() error {
	errs := []error{}
	for _, device := range self.devices {
		wire := device.currentWire()
		if wire == nil {
			errs = append(errs, fmt.Errorf("%s's bridge is down", device.name))
			continue
		}
		compiled := wire.component.Compiled()
		if compiled == nil || compiled.Block == nil {
			errs = append(errs, fmt.Errorf("%s's bridge compiled no block, where the network's is %s: %s", device.name, self.block, self.compileWaits(device)))
			continue
		}
		if held := bridgeWirePrefix(compiled.Block.GetIpv4Prefix()); held != self.block {
			errs = append(errs, fmt.Errorf("%s's bridge compiled the block %s, where the network's is %s", device.name, held, self.block))
		}
	}
	return errors.Join(errs...)
}

// A follower's config repo keys as its Sidecar holds them (sidecar.SidecarConfigKeys), as the follower installs its own
// over a key client of forwarding setup's: the current generation's for a new body's seal, and each generation's to
// open one.
type itBridgeConfigKeys struct {
	follower *sidecar.SidecarConfigFollower
}

func (self *itBridgeConfigKeys) CurrentConfigKey(ctx context.Context) (*sidecar.EvsRepoKey, error) {
	return self.follower.CurrentConfigKey(ctx)
}

func (self *itBridgeConfigKeys) ConfigKeyAt(generation uint64) *sidecar.EvsRepoKey {
	return self.follower.RepoKeyAt(generation)
}

// Forms the config repo's group at the World's FakeGkm through the owner's key client, as a real-keys bootstrap's KF1
// forms it at gkm after base 1 (§7.9, QD-824), and waits until every device holds generation 1: the World's FakeGkm
// bootstrap keeps the config heads in memory and forms no group, so no device holds the key a bridge body seals under
// (§17.8's placement) until one does.
func (self *ItBridgeWorld) formConfigGroup(ctx context.Context) error {
	owner := self.owner.t3
	state := owner.Follower().VerifiedState()
	if state == nil {
		return errors.New("the owner's follower holds no state")
	}
	configRepoId, err := sidecar.IdFromBytes(state.Genesis().GetConfigRepoId())
	if err != nil {
		return fmt.Errorf("the genesis names no config repo: %w", err)
	}
	if _, err := owner.Gkm.CreateGroup(ctx, &sidecar.GkmCreateGroupArgs{
		NetworkId: sidecar.Id(self.network.NetworkId), RepoId: configRepoId, Kind: protocol.RepoKind_REPO_KIND_CONFIG,
	}); err != nil {
		return fmt.Errorf("the config repo's group: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, self.settings.AdoptTimeout)
	defer cancel()
	for _, device := range slices.Concat([]*itBridgeDevice{self.owner}, self.devices) {
		for device.t3.Follower().RepoKeyAt(1) == nil {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s never held the config repo's generation 1: %w", device.name, ctx.Err())
			case <-time.After(self.settings.AdoptPollInterval):
			}
		}
	}
	return nil
}

// The routing devices: the subnets', in the subnets' order, and the campus's.
func (self *ItBridgeWorld) routerDevices() []*itBridgeDevice {
	devices := []*itBridgeDevice{}
	for _, subnet := range self.routes() {
		devices = append(devices, subnet.device)
	}
	return devices
}

// The routed destinations: the subnets and the campus.
func (self *ItBridgeWorld) routes() []*itBridgeTarget {
	if self.campus == nil {
		return self.subnets
	}
	return append(slices.Clone(self.subnets), self.campus)
}

// The subnet nesting in the campus that holds address, nil for none.
func (self *ItBridgeWorld) nestedSubnet(address netip.Addr) *itBridgeTarget {
	if self.campus == nil || !self.campus.prefix.Contains(address) {
		return nil
	}
	for _, subnet := range self.subnets {
		if subnet.prefix.Contains(address) {
			return subnet
		}
	}
	return nil
}

// Whether any rule of clientIndex's names subnet, so its router holds the subnet's entry once it is in force.
func (self *ItBridgeWorld) routesSubnet(clientIndex int, subnet *itBridgeTarget) bool {
	return self.settings.subnetGrant(clientIndex, subnet.index) != itBridgeSubnetDenied
}

// The destination that should answer clientIndex's flow to an address of target's: target itself, but for a subnet
// nesting in the campus that no rule of the client's names, which the client routes by the campus's rule.
func (self *ItBridgeWorld) answerer(clientIndex int, target *itBridgeTarget) *itBridgeTarget {
	if target.kind == itBridgeTargetSubnet && self.campus != nil && target != self.campus &&
		self.campus.prefix.Overlaps(target.prefix) && !self.routesSubnet(clientIndex, target) {
		return self.campus
	}
	return target
}

// A filer for device over its follower, as ur daemon's composition builds one (cli's bridge_filing_config.go), with no
// config writer: a router's and a host's filings are statements, which save no head.
func (self *ItBridgeWorld) newFiler(device *itBridgeDevice) (*sidecar.BridgeFiler, error) {
	follower := device.t3.Follower()
	return sidecar.NewBridgeFiler(&sidecar.BridgeFilerArgs{
		Sidecar:  device.t3.Sidecar(),
		Config:   follower.Client(),
		Heads:    follower.Heads(),
		AclLocal: follower.AclLocal,
	})
}

// device's config writer as a daemon's filer saves heads through it (sidecar.BridgeHeadWriter): each SaveHead makes a
// writer over the ref device's follower holds for confirmed saves, which keeps the network's block as the follower's
// writers do (sidecar.WithBridgeBlock), saves through it and closes it, its base published through the device's own
// api, and then syncs the network at the base. A base api refuses 429, past §7.9's config_bases_per_min, is saved
// again once its retry_after has passed, within the World's RateLimitWaitTimeout.
type itBridgeHeadWriter struct {
	world  *ItBridgeWorld
	device *itBridgeDevice
}

var _ sidecar.BridgeHeadWriter = (*itBridgeHeadWriter)(nil)

func (self *itBridgeHeadWriter) SaveHead(ctx context.Context, edit func(head *sidecar.ConfigHead) (*sidecar.ConfigHead, error)) (*bridge.AuthPin, error) {
	device := self.device
	follower := device.t3.Follower()
	provider, ok := follower.Heads().(sidecar.ConfirmedAclConfigRefProvider)
	if !ok {
		return nil, fmt.Errorf("%s's follower holds no ref for confirmed saves", device.name)
	}
	api, err := self.world.generationApi(ctx, device)
	if err != nil {
		return nil, err
	}
	running := device.t3.Sidecar()
	writer, err := sidecar.NewConfigWriter(ctx, &sidecar.ConfigWriterArgs{
		SidecarId: running.Identity().SidecarId,
		Sign: func(purpose string, signedType bridge.SignedType, body []byte) (*bridge.Signed, error) {
			return running.Sign(purpose, signedType, body, nil)
		},
		Client:    follower.Client(),
		Heads:     provider.ConfirmedAclConfigRef(),
		Publisher: api,
		// §17.4 and QD-1205, as the follower's writers take it: a head that holds no block gets one, and a block whose
		// signer no longer stands is signed again
		BridgeBlock: sidecar.WithBridgeBlock,
	})
	if err != nil {
		return nil, err
	}
	defer writer.Close()
	waitCtx, cancel := context.WithTimeout(ctx, self.world.world.Settings().RateLimitWaitTimeout)
	defer cancel()
	for {
		pin, err := writer.SaveHead(waitCtx, edit)
		var apiErr *sidecar.SidecarApiError
		if err == nil {
			version := follower.Client().Pin().GetVersion()
			if pin != nil {
				version = max(version, pin.GetVersion())
			}
			return pin, self.world.network.Sync(ctx, version)
		}
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
			return nil, err
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("%s's head: %w, and its retry_after outlasted the wait: %w", device.name, err, waitCtx.Err())
		case <-time.After(time.Duration(apiErr.RetryAfterMs) * time.Millisecond):
		}
	}
}

// The Sidecar's own api on device's current generation, which a component's Attach is handed: what its base
// publishes go through, DPoP-signed on its token.
func (self *ItBridgeWorld) generationApi(ctx context.Context, device *itBridgeDevice) (*sidecar.SidecarApi, error) {
	capture := &itBridgeApiComponent{attached: make(chan *sidecar.SidecarApi, 1)}
	remove, err := device.t3.Sidecar().AddComponent(capture)
	if err != nil {
		return nil, err
	}
	defer remove()
	select {
	case api := <-capture.attached:
		return api, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s's generation attached no component: %w", device.name, ctx.Err())
	}
}

// A component that takes the generation's api at its Attach and holds nothing else.
type itBridgeApiComponent struct {
	attached chan *sidecar.SidecarApi
}

func (self *itBridgeApiComponent) Name() string {
	return "itb-api"
}

func (self *itBridgeApiComponent) Attach(env *sidecar.SidecarEnv) error {
	if env.SidecarApi != nil {
		select {
		case self.attached <- env.SidecarApi:
		default:
		}
	}
	return nil
}

func (self *itBridgeApiComponent) Close() {}

// The document's source (the file's header's grant matrix).
func (self *ItBridgeWorld) policySource() (string, error) {
	settings := self.settings
	selector := func(device *itBridgeDevice) string {
		return "sidecar:" + device.SidecarId().String()
	}
	servicePort := fmt.Sprint(settings.SubnetServicePort)
	grants := []map[string]any{}
	for _, subnet := range self.routes() {
		tcpUdp, tcpOnly := []string{}, []string{}
		for index, client := range self.clients {
			switch settings.subnetGrant(index, subnet.index) {
			case itBridgeSubnetTcpUdp:
				tcpUdp = append(tcpUdp, selector(client))
			case itBridgeSubnetTcpOnly:
				tcpOnly = append(tcpOnly, selector(client))
			}
		}
		for _, rule := range []struct {
			src   []string
			ports []string
		}{
			{src: tcpUdp, ports: []string{"tcp:" + servicePort, "udp:" + servicePort}},
			{src: tcpOnly, ports: []string{"tcp:" + servicePort}},
		} {
			if len(rule.src) == 0 {
				continue
			}
			grants = append(grants, map[string]any{
				"src": rule.src, "dst": []string{"ip:" + subnet.prefix.String()}, "via": []string{selector(subnet.device)},
				"allow": []string{"route"}, "ip": rule.ports,
			})
		}
	}
	for _, forward := range self.forwards {
		src := []string{}
		for index, client := range self.clients {
			if settings.forwardGranted(index, forward.index) {
				src = append(src, selector(client))
			}
		}
		if len(src) == 0 {
			return "", fmt.Errorf("%s has no client its grant names, so no rule covers its map", forward.name)
		}
		grants = append(grants, map[string]any{
			"src": src, "dst": []string{selector(forward.device)}, "allow": []string{"dial"},
			"ip": []string{fmt.Sprintf("%s:%d", forward.protocol, forward.port)},
		})
	}
	for _, echo := range self.echoes {
		src := []string{}
		for index, client := range self.clients {
			if settings.echoGranted(index, echo.index) {
				src = append(src, selector(client))
			}
		}
		if len(src) == 0 {
			continue
		}
		grants = append(grants, map[string]any{
			"src": src, "dst": []string{selector(echo.device)}, "allow": []string{"dial"}, "ip": []string{"icmp:*"},
		})
	}
	document := map[string]any{"grants": grants}
	if requests := slices.Concat(self.routeGrantRequests(selector), self.shareRequests(selector)); 0 < len(requests) {
		document["requests"] = requests
	}
	source, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(source), nil
}

// Waits until every device's follower holds the document's compile: its acl engine's state has a document and its
// local policy is compiled, which a filer's route rule ids and each compile's rows read.
func (self *ItBridgeWorld) awaitDocument(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.AdoptTimeout)
	defer cancel()
	for _, device := range slices.Concat([]*itBridgeDevice{self.owner}, self.devices) {
		for {
			follower := device.t3.Follower()
			engine := follower.AclEngine()
			if engine != nil && engine.State() != nil && engine.State().HasDocument() && follower.AclLocal() != nil {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s's follower never held the document's compile: %w", device.name, ctx.Err())
			case <-time.After(self.settings.AdoptPollInterval):
			}
		}
	}
	return nil
}

// The network's tip as the owner's follower holds it.
func (self *ItBridgeWorld) tip() uint64 {
	return self.owner.t3.Follower().Client().Pin().GetVersion()
}

// Waits until every device's bridge has adopted the log through n: its follower stands at n, its role published what
// it holds at or past n, its component compiled at or past n unless the entry at n is a pure denial (which reaches the
// role as a delta and through Bridge.Revoke), and its provider serves; then a flow at any device meets what n holds.
// A device whose bridge is down is waited for once it is back.
func (self *ItBridgeWorld) AwaitAdopted(ctx context.Context, n uint64) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.AdoptTimeout)
	defer cancel()
	if err := self.network.Sync(ctx, n); err != nil {
		return err
	}
	for _, device := range self.devices {
		if err := self.awaitDeviceAdopted(ctx, device, n); err != nil {
			return err
		}
	}
	return nil
}

// AwaitAdopted for one device.
func (self *ItBridgeWorld) awaitDeviceAdopted(ctx context.Context, device *itBridgeDevice, n uint64) error {
	client := device.t3.Follower().Client()
	if _, err := client.WaitFor(ctx, n); err != nil {
		return fmt.Errorf("%s's follower did not reach %d: %w", device.name, n, err)
	}
	pure := false
	for _, entry := range client.BridgeEntries(n, n) {
		pure = entry.Pure
	}
	for {
		wire := device.currentWire()
		if wire != nil {
			adopted := wire.adoptedPin.Value()
			status := wire.Status()
			if n <= adopted && (pure || n <= status.CompiledPin.GetVersion()) && status.Serving {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s's bridge never adopted %d: %s: %w", device.name, n, self.deviceProgress(device), ctx.Err())
		case <-time.After(self.settings.AdoptPollInterval):
		}
	}
}

// One line of where a device's bridge stands.
func (self *ItBridgeWorld) deviceProgress(device *itBridgeDevice) string {
	wire := device.currentWire()
	follower := device.t3.Follower()
	line := device.name
	if follower != nil && follower.Client() != nil {
		line += fmt.Sprintf(": following at %d", follower.Client().Pin().GetVersion())
	}
	if wire == nil {
		return line + ", bridge down"
	}
	status := wire.Status()
	decision := wire.bridge.Decision()
	role := wire.bridge.Status()
	defaultRouteCount, refusedSendCount := wire.Counts()
	windowClientCount, windowConnectedCount, windowOpenedCount := wire.links.WindowClientCounts()
	line += fmt.Sprintf(", posture %v at %d (state %d), compiled %d (%d compiles, %d refused), serving %t, waits %d, alarms %d, routes %d, "+
		"%d window clients (%d connected, %d opened), %d default-route packets, %d refused sends, %d local dials, wire %d; role: document %t, %d entries, %d maps, "+
		"%d served, %d subjects, %d links open, %d building, %d principals verified, %d rejects sent, %d held rejects, %d route conflicts, "+
		"%d waits, %d posture alarms, %d alarms",
		decision.State, decision.Pin.GetVersion(), decision.BridgeState.GetAuth().GetVersion(), status.CompiledPin.GetVersion(),
		status.CompileCount, status.CompileErrorCount, status.Serving, len(status.Waits), len(status.Alarms), len(wire.bridge.Routes()),
		windowClientCount, windowConnectedCount, windowOpenedCount, defaultRouteCount, refusedSendCount, len(wire.links.LocalDials()), device.WireCount(),
		role.HasDocument, role.EntryCount, role.MapCount, role.ServedCount, role.SubjectCount, role.LinkOpenCount, role.LinkBuildingCount,
		role.PrincipalVerifiedCount, role.RejectSentCount, role.HeldRejectCount, role.RouteConflictCount, len(role.Waits),
		len(role.PostureAlarms), len(role.Alarms))
	for _, wait := range status.Waits {
		line += fmt.Sprintf("\n      compile wait %+v", wait)
	}
	for _, alarm := range status.Alarms {
		line += fmt.Sprintf("\n      compile alarm %+v", alarm)
	}
	for _, wait := range role.Waits {
		line += fmt.Sprintf("\n      role wait %+v", wait)
	}
	for _, alarm := range role.PostureAlarms {
		line += fmt.Sprintf("\n      posture alarm %+v", alarm)
	}
	for _, alarm := range role.Alarms {
		line += fmt.Sprintf("\n      role alarm %+v", alarm)
	}
	return line
}

// The component's bridge_alarm for device, kept for the dump.
func (self *ItBridgeWorld) noteAlarm(device *itBridgeDevice) sidecar.SidecarEventFunction {
	return func(event sidecar.SidecarEvent) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.alarms = append(self.alarms, fmt.Sprintf("%s: %s %v", device.name, event.Kind, event.BridgeAlarm))
	}
}

// The progress dump (World.FailProgress) with every bridge's line and the alarms before it.
func (self *ItBridgeWorld) FailProgress(t testing.TB, phase string, err error, bursts []*WorldBurst) {
	t.Helper()
	for _, device := range self.devices {
		t.Logf("  bridge %s", self.deviceProgress(device))
	}
	self.stateLock.Lock()
	alarms := slices.Clone(self.alarms)
	self.stateLock.Unlock()
	for _, alarm := range alarms {
		t.Logf("  alarm %s", alarm)
	}
	self.world.FailProgress(t, phase, err, bursts)
}

// A random duration in [low, high].
func (self *ItBridgeWorld) between(low time.Duration, high time.Duration) time.Duration {
	self.randomLock.Lock()
	defer self.randomLock.Unlock()
	if high <= low {
		return low
	}
	return low + time.Duration(self.random.Int64N(int64(high-low)+1))
}

// A random int in [0, n).
func (self *ItBridgeWorld) intN(n int) int {
	self.randomLock.Lock()
	defer self.randomLock.Unlock()
	return self.random.IntN(n)
}

// A random nonce for a request.
func (self *ItBridgeWorld) nonce() string {
	self.randomLock.Lock()
	defer self.randomLock.Unlock()
	return fmt.Sprintf("%016x", self.random.Uint64())
}

// Joins target, a subnet or a forward: its route or map filed through its device's filer, as `ur bridge expose` files
// one under a standing grant, then waited for in the filer's ledger and adopted by every bridge. Its timeline records
// each step.
func (self *ItBridgeWorld) Join(ctx context.Context, target *itBridgeTarget) error {
	incarnation := target.timeline.begin()
	target.service.SetIncarnation(incarnation.Number)
	origin := &sidecar.BridgeFileOrigin{Act: sidecar.SidecarActConfirmed}
	filer := target.device.filer
	var filing *sidecar.BridgeFiling
	var err error
	switch target.kind {
	case itBridgeTargetSubnet:
		filing, err = filer.Expose(ctx, origin, &sidecar.BridgeRouteRequest{Prefixes: []netip.Prefix{target.prefix}})
	case itBridgeTargetForward:
		protocol := uint32(6)
		if target.protocol == "udp" {
			protocol = 17
		}
		filing, err = filer.ExposeMap(ctx, origin, &sidecar.BridgeMapRequest{
			Protocol:   protocol,
			FirstPort:  uint32(target.port),
			TargetPort: uint32(target.service.portOf(target.protocol)),
		})
	default:
		return fmt.Errorf("%s joins nothing", target.name)
	}
	if err != nil {
		return fmt.Errorf("%s's filing: %w", target.name, err)
	}
	landedAt := time.Now()
	rule, err := self.awaitRule(ctx, target.device, filing.RuleId, false)
	if err != nil {
		return fmt.Errorf("%s's filing: %w", target.name, err)
	}
	target.timeline.update(func() {
		incarnation.RuleId, incarnation.LandedAt, incarnation.Pin = filing.RuleId, landedAt, rule.FirstAt
	})
	if err := self.AwaitAdopted(ctx, rule.FirstAt); err != nil {
		return fmt.Errorf("%s's filing at %d: %w", target.name, rule.FirstAt, err)
	}
	target.timeline.update(func() {
		incarnation.AdoptedAt = time.Now()
	})
	return nil
}

// Takes target out: a reset BridgeWithdraw of its newest rule filed by its device (`ur bridge close --reset`), waited
// for in the filer's ledger and adopted by every bridge.
func (self *ItBridgeWorld) Leave(ctx context.Context, target *itBridgeTarget) error {
	incarnation := target.timeline.newest()
	if incarnation == nil || len(incarnation.RuleId) == 0 {
		return fmt.Errorf("%s has no rule to withdraw", target.name)
	}
	target.timeline.update(func() {
		incarnation.WithdrawAt = time.Now()
	})
	_, err := target.device.filer.Close(ctx, &sidecar.BridgeFileOrigin{Act: sidecar.SidecarActConfirmed}, incarnation.RuleId, true)
	if err != nil {
		return fmt.Errorf("%s's withdrawal: %w", target.name, err)
	}
	withdrawnAt := time.Now()
	rule, err := self.awaitRule(ctx, target.device, incarnation.RuleId, true)
	if err != nil {
		return fmt.Errorf("%s's withdrawal: %w", target.name, err)
	}
	target.timeline.update(func() {
		incarnation.WithdrawnAt, incarnation.WithdrawPin = withdrawnAt, rule.Withdrawn.At
	})
	if err := self.AwaitAdopted(ctx, rule.Withdrawn.At); err != nil {
		return fmt.Errorf("%s's withdrawal at %d: %w", target.name, rule.Withdrawn.At, err)
	}
	target.timeline.update(func() {
		incarnation.WithdrawAdoptedAt = time.Now()
	})
	return nil
}

// Waits until device's filer reads ruleId in its ledger, withdrawn when withdrawn is set, and answers it.
func (self *ItBridgeWorld) awaitRule(ctx context.Context, device *itBridgeDevice, ruleId []byte, withdrawn bool) (*sidecar.BridgeLedgerRule, error) {
	ctx, cancel := context.WithTimeout(ctx, self.settings.FilingTimeout)
	defer cancel()
	for {
		rule, err := device.filer.FindRule(ctx, ruleId)
		if err == nil && rule != nil && (!withdrawn || rule.Withdrawn != nil) {
			if !withdrawn && (rule.Pending || rule.Void) {
				return nil, fmt.Errorf("the rule landed pending %t, void %t", rule.Pending, rule.Void)
			}
			return rule, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s's ledger never held the rule (withdrawn %t): %v (%w)", device.name, withdrawn, err, ctx.Err())
		case <-time.After(self.settings.AdoptPollInterval):
		}
	}
}

// The port of a service's listener for protocol, tcp or udp.
func (self *itBridgeService) portOf(protocol string) uint16 {
	if protocol == "udp" {
		return self.UdpAddress().Port()
	}
	return self.TcpAddress().Port()
}

// Marks a chaos window opening now for device, nil for every device; the answer closes it.
func (self *ItBridgeWorld) beginChaos(device *itBridgeDevice, what string) func() {
	chaos := &itBridgeChaos{device: device, what: what, start: time.Now()}
	self.stateLock.Lock()
	self.chaos = append(self.chaos, chaos)
	self.stateLock.Unlock()
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		chaos.end = time.Now()
	}
}

// Whether a chaos window that reaches device, or every device, overlaps [start, end] widened by bound: what a
// liveness check forgives.
func (self *ItBridgeWorld) chaosOverlaps(device *itBridgeDevice, start time.Time, end time.Time, bound time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, chaos := range self.chaos {
		if chaos.device != nil && chaos.device != device {
			continue
		}
		chaosEnd := chaos.end
		if chaosEnd.IsZero() {
			chaosEnd = time.Now()
		}
		if chaos.start.Before(end) && start.Before(chaosEnd.Add(bound)) {
			return true
		}
	}
	return false
}

// Restarts device's bridge mid-run, as a stop and start of its daemon does: the wire ends, its posture store kept,
// stays down ChaosDownPause, and a new wire restores the posture and adopts the tip.
func (self *ItBridgeWorld) RestartDevice(ctx context.Context, device *itBridgeDevice) error {
	end := self.beginChaos(device, "restart")
	defer end()
	device.stopWire()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(self.settings.ChaosDownPause):
	}
	if err := device.startWire(); err != nil {
		return err
	}
	adoptCtx, cancel := context.WithTimeout(ctx, self.settings.AdoptTimeout)
	defer cancel()
	return self.awaitDeviceAdopted(adoptCtx, device, self.tip())
}

// Restarts the exchange's connect handler mid-run: every client's transport drops and dials again.
func (self *ItBridgeWorld) RestartHandler(ctx context.Context) error {
	end := self.beginChaos(nil, "connect handler restart")
	defer end()
	return self.world.T3().Exchange().RestartHandler(ctx)
}

// One send of a client's (it_bridge_test.go's header's attempts).
type ItBridgeAttempt struct {
	Phase    string
	Client   *itBridgeDevice
	Target   *itBridgeTarget
	Protocol string
	Address  netip.AddrPort
	// The destination that should answer it (ItBridgeWorld.answerer), and whether the client's rules grant it: the
	// document's, or a route grant in force as it began.
	Expected *itBridgeTarget
	Granted  bool
	Start    time.Time
	End      time.Time
	Reached  bool
	// What the answer named; a stream's newest.
	Owner       string
	Incarnation int64
	// The requests a stream got answers to; one for any other attempt that was reached.
	AnsweredCount int
	Err           string
	// The request and answer bytes the attempt moved, both ways.
	PayloadByteCount int
}

// Whether clientIndex's rules let it send protocol to target, as the destination that should answer it takes it
// (answerer).
func (self *ItBridgeWorld) granted(clientIndex int, target *itBridgeTarget, protocol string) bool {
	return self.grantedBy(clientIndex, self.answerer(clientIndex, target), protocol)
}

// Whether client may have sent protocol to target itself over [start, end]: the document's rules, or a route grant
// whose timeline allows the span (it_bridge_grants_test.go).
func (self *ItBridgeWorld) grantedOver(client *itBridgeDevice, target *itBridgeTarget, protocol string, start time.Time, end time.Time) bool {
	if target.device.world != self {
		// another network's destination, which only a share this network accepted grants (it_bridge_shares_test.go)
		return self.shareAllows(client, target, protocol, start, end)
	}
	return self.grantedBy(client.clientIndex(), target, protocol) || self.routeGrantAllows(client, target, protocol, start, end)
}

// Whether clientIndex's rules for target itself let it send protocol there.
func (self *ItBridgeWorld) grantedBy(clientIndex int, target *itBridgeTarget, protocol string) bool {
	switch target.kind {
	case itBridgeTargetSubnet:
		switch self.settings.subnetGrant(clientIndex, target.index) {
		case itBridgeSubnetTcpUdp:
			return true
		case itBridgeSubnetTcpOnly:
			return protocol == "tcp" || protocol == "stream"
		}
		return false
	case itBridgeTargetForward:
		return self.settings.forwardGranted(clientIndex, target.index)
	case itBridgeTargetEcho:
		return self.settings.echoGranted(clientIndex, target.index)
	}
	return false
}

// The address an attempt sends to: a random host of a subnet on its service port, the campus's outside its nested
// subnets, a forward's host's overlay address and port, a host's overlay address.
func (self *ItBridgeWorld) addressOf(target *itBridgeTarget) netip.AddrPort {
	switch target.kind {
	case itBridgeTargetSubnet:
		size := uint32(1) << (32 - target.prefix.Bits())
		for {
			host := uint32(1) + uint32(self.intN(int(max(1, size-2))))
			address := itBridgeAddr(itBridgeIpv4(target.prefix.Addr()) + host)
			// the campus's own hosts are those no subnet nesting in it holds
			if target != self.campus || self.nestedSubnet(address) == nil {
				return netip.AddrPortFrom(address, self.settings.SubnetServicePort)
			}
		}
	case itBridgeTargetForward:
		return netip.AddrPortFrom(self.hostAddresses[target.device.hostIndex()], target.port)
	}
	return netip.AddrPortFrom(self.hostAddresses[target.index], 0)
}

// A host's index among the hosts, -1 for a device that is none.
func (self *itBridgeDevice) hostIndex() int {
	return slices.Index(self.world.hosts, self)
}

// A client's index among the clients, -1 for a device that is none.
func (self *itBridgeDevice) clientIndex() int {
	return slices.Index(self.world.clients, self)
}

// Sends one attempt of protocol, tcp, udp, stream or icmp, from client to target through its stack and router, and
// keeps it. Never fails the test: what it reached is the oracle's to judge.
func (self *ItBridgeWorld) Attempt(ctx context.Context, phase string, client *itBridgeDevice, target *itBridgeTarget, protocol string) *ItBridgeAttempt {
	start := time.Now()
	expected := self.answerer(client.clientIndex(), target)
	granted := self.granted(client.clientIndex(), target, protocol) || self.routeGrantInForce(client, expected, protocol, start)
	return self.attempt(ctx, phase, client, target, protocol, self.addressOf(target), expected, granted)
}

// Attempt to address, for target, another network's destination a share reaches through its alias
// (it_bridge_shares_test.go): the target answers it, and granted says whether the share lets the client reach it as it
// begins.
func (self *ItBridgeWorld) AttemptAt(ctx context.Context, phase string, client *itBridgeDevice, target *itBridgeTarget, protocol string, address netip.AddrPort, granted bool) *ItBridgeAttempt {
	return self.attempt(ctx, phase, client, target, protocol, address, target, granted)
}

// One attempt (Attempt), its address, the destination that should answer it and whether it is granted given.
func (self *ItBridgeWorld) attempt(ctx context.Context, phase string, client *itBridgeDevice, target *itBridgeTarget, protocol string,
	address netip.AddrPort, expected *itBridgeTarget, granted bool) *ItBridgeAttempt {
	attempt := &ItBridgeAttempt{
		Phase:    phase,
		Client:   client,
		Target:   target,
		Protocol: protocol,
		Address:  address,
		Expected: expected,
		Granted:  granted,
		Start:    time.Now(),
	}
	err := func() error {
		wire := client.currentWire()
		if wire == nil {
			return errors.New("the client's bridge is down")
		}
		attemptCtx, cancel := context.WithTimeout(ctx, self.settings.AttemptTimeout)
		defer cancel()
		switch protocol {
		case "tcp":
			return self.attemptTcp(attemptCtx, wire, attempt, 1)
		case "stream":
			streamCtx, streamCancel := context.WithTimeout(ctx, self.settings.AttemptTimeout+time.Duration(self.settings.StreamRequestCount)*self.settings.StreamPause)
			defer streamCancel()
			return self.attemptTcp(streamCtx, wire, attempt, self.settings.StreamRequestCount)
		case "udp":
			return self.attemptUdp(attemptCtx, wire, attempt)
		case "icmp":
			return self.attemptEcho(wire, attempt)
		}
		return fmt.Errorf("no protocol %s", protocol)
	}()
	attempt.End = time.Now()
	if err != nil {
		attempt.Err = err.Error()
	}
	self.stateLock.Lock()
	self.attempts = append(self.attempts, attempt)
	self.stateLock.Unlock()
	return attempt
}

// A TCP connection through the stack, answered requestCount times, StreamPause apart past the first.
func (self *ItBridgeWorld) attemptTcp(ctx context.Context, wire *itBridgeWire, attempt *ItBridgeAttempt, requestCount int) error {
	conn, err := wire.DialTcp(ctx, attempt.Address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	buffer := make([]byte, self.settings.ReadByteCount)
	pending := []byte{}
	for index := range requestCount {
		if 0 < index {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(self.settings.StreamPause):
			}
		}
		nonce := self.nonce()
		request := itBridgeRequest(attempt.Client.name, nonce, attempt.Address.String())
		if _, err := conn.Write(request); err != nil {
			return err
		}
		attempt.PayloadByteCount += len(request)
		for !strings.Contains(string(pending), "\n") {
			n, err := conn.Read(buffer)
			if err != nil {
				return err
			}
			pending = append(pending, buffer[:n]...)
		}
		line, rest, _ := strings.Cut(string(pending), "\n")
		pending = []byte(rest)
		attempt.PayloadByteCount += len(line) + 1
		owner, incarnation, err := itBridgeParseAnswer(line, nonce)
		if err != nil {
			return err
		}
		attempt.Reached, attempt.Owner, attempt.Incarnation = true, owner, incarnation
		attempt.AnsweredCount += 1
	}
	return nil
}

// A UDP request through the stack, sent up to UdpTryCount times until answered.
func (self *ItBridgeWorld) attemptUdp(ctx context.Context, wire *itBridgeWire, attempt *ItBridgeAttempt) error {
	conn, err := wire.DialUdp(ctx, attempt.Address)
	if err != nil {
		return err
	}
	defer conn.Close()
	nonce := self.nonce()
	request := itBridgeRequest(attempt.Client.name, nonce, attempt.Address.String())
	buffer := make([]byte, self.settings.ReadByteCount)
	tryTimeout := self.settings.AttemptTimeout / time.Duration(max(1, self.settings.UdpTryCount))
	var lastErr error
	for range max(1, self.settings.UdpTryCount) {
		if _, err := conn.Write(request); err != nil {
			return err
		}
		attempt.PayloadByteCount += len(request)
		conn.SetReadDeadline(time.Now().Add(tryTimeout))
		n, err := conn.Read(buffer)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		attempt.PayloadByteCount += n
		owner, incarnation, err := itBridgeParseAnswer(strings.TrimSpace(string(buffer[:n])), nonce)
		if err != nil {
			return err
		}
		attempt.Reached, attempt.Owner, attempt.Incarnation, attempt.AnsweredCount = true, owner, incarnation, 1
		return nil
	}
	return lastErr
}

// An ICMP echo to a host's overlay address through the router, its id and sequence random.
func (self *ItBridgeWorld) attemptEcho(wire *itBridgeWire, attempt *ItBridgeAttempt) error {
	id, sequence := uint16(self.intN(1<<16)), uint16(self.intN(1<<16))
	if !wire.SendEcho(attempt.Address.Addr(), id, sequence) {
		return errors.New("no link took the echo")
	}
	attempt.PayloadByteCount += len(self.settings.Wire.EchoPayload)
	replied, icmpType := wire.WaitEcho(id, sequence, self.settings.EchoTimeout)
	switch {
	case replied:
		attempt.Reached, attempt.Owner, attempt.AnsweredCount = true, attempt.Expected.name, 1
		attempt.PayloadByteCount += len(self.settings.Wire.EchoPayload)
		return nil
	case icmpType != 0:
		return fmt.Errorf("an ICMP error of type %d", icmpType)
	}
	return errors.New("no echo reply")
}

// Every attempt so far.
func (self *ItBridgeWorld) Attempts() []*ItBridgeAttempt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.attempts)
}

// Every target, subnets, forwards and echoes.
func (self *ItBridgeWorld) targets() []*itBridgeTarget {
	return slices.Concat(self.routes(), self.forwards, self.echoes)
}

// Ends every device's bridge, the destinations, and the window clients' strategy. Idempotent; the World closes on its
// own cleanup.
func (self *ItBridgeWorld) Close() {
	first := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.closed = true
		return true
	}()
	if !first {
		return
	}
	self.stopWires()
	for _, target := range self.targets() {
		if target.service != nil {
			target.service.close()
		}
	}
	self.clientStrategy.Close()
}

// Ends every device's bridge, clients first, so no sender's link outlives its host's provider.
func (self *ItBridgeWorld) stopWires() {
	var group sync.WaitGroup
	for _, device := range self.clients {
		group.Go(device.stopWire)
	}
	group.Wait()
	for _, device := range self.devices {
		group.Go(device.stopWire)
	}
	group.Wait()
}
