// The fixture the gossip service tests share (connect/EXTENDER.md C6).
//
// Everything here is synthetic: `.example` names, RFC 5737 and RFC 3849
// documentation addresses, and keys generated in the test. The mesh itself is
// real -- a node built exactly as the service builds it, listening with the
// websocket transport on loopback `ws` the way it does behind nginx, and a
// member node that dials it -- because a faked mesh would prove nothing about
// the one thing this service does.

package gossip

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ma "github.com/multiformats/go-multiaddr"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	connectgossip "github.com/urnetwork/connect/v2026/gossip"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The operator host of these tests.
const testNetworkHost = "ur.example"

// How long a barrier waits for the mesh to reach a state. Generous, because
// the race build of two libp2p hosts is slow; nothing waits this long when the
// mesh is healthy.
const testMeshTimeout = 60 * time.Second

// The vault resource, the root key that signs what the queue carries, and the
// service configuration resolved from it.
type testConfig struct {
	operator       *operatorConfig
	extender       *controller.ExtenderConfig
	rootPrivateKey ed25519.PrivateKey
}

// Installs an `extender.yml` with a fresh root key and gossip identity, and
// resolves it exactly as the service does at startup.
func newTestConfig(t testing.TB) *testConfig {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey, err := connect.ExtenderPublicKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	gossipIdentityKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	yamlLines := []string{
		"root_private_key_hex: " + connect.ExtenderKeySeedHex(rootKeySeed),
		"root_public_keys_hex:",
		"  - " + hex.EncodeToString(rootPublicKey),
		"network_host: " + testNetworkHost,
		"api_url: https://api." + testNetworkHost,
		"gossip_identity_key_hex: " + connect.ExtenderKeySeedHex(gossipIdentityKeySeed),
	}
	installTestConfigYaml(t, strings.Join(yamlLines, "\n"))

	extenderConfig, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	operator, err := newOperatorConfig(extenderConfig)
	if err != nil {
		t.Fatal(err)
	}
	return &testConfig{
		operator:       operator,
		extender:       extenderConfig,
		rootPrivateKey: rootPrivateKey,
	}
}

// Pushes one `extender.yml` body through the vault resolver and drops the
// cached configuration so the next read sees it.
func installTestConfigYaml(t testing.TB, body string) {
	t.Helper()
	pop := server.Vault.PushSimpleResource("extender.yml", []byte(body))
	controller.Testing_ResetExtenderConfig()
	t.Cleanup(func() {
		pop()
		controller.Testing_ResetExtenderConfig()
	})
}

// One node and the directory it fills, joined and closed together.
type testNode struct {
	directory *connect.ExtenderDirectory
	node      *connectgossip.Node
	cancel    context.CancelFunc
}

func (self *testNode) close() {
	self.node.Close()
	self.directory.Close()
	self.cancel()
}

// The operator node, built through the same constructors the service uses. A
// zero port takes whatever loopback port is free, which `listenPort` reads
// back.
func newTestOperator(t testing.TB, config *testConfig, port int) *testNode {
	t.Helper()
	listenAddrs, err := connectgossip.WebsocketListenAddrs("127.0.0.1", port, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	directory := newDirectory(ctx, config.operator)
	node, err := newNode(ctx, config.operator, directory, listenAddrs)
	if err != nil {
		directory.Close()
		cancel()
		t.Fatal(err)
	}
	return &testNode{
		directory: directory,
		node:      node,
		cancel:    cancel,
	}
}

// The loopback port the node actually bound.
func (self *testNode) listenPort(t testing.TB) int {
	t.Helper()
	for _, listenAddr := range self.node.ListenAddrs() {
		port, err := listenAddr.ValueForProtocol(ma.P_TCP)
		if err != nil {
			continue
		}
		if portNumber, err := strconv.Atoi(port); err == nil && 0 < portNumber {
			return portNumber
		}
	}
	t.Fatal("the operator bound no port")
	return 0
}

// The dialable operator address, taken from what the swarm actually bound so
// nothing waits on the host getting around to advertising it.
func (self *testNode) addrs(t testing.TB) []ma.Multiaddr {
	t.Helper()
	listenAddrs := self.node.ListenAddrs()
	if len(listenAddrs) == 0 {
		t.Fatal("the operator bound no address")
	}
	p2pComponent, err := ma.NewComponent("p2p", self.node.PeerId().String())
	if err != nil {
		t.Fatal(err)
	}
	return []ma.Multiaddr{listenAddrs[0].Encapsulate(p2pComponent)}
}

// One member app: its own identity and directory, the same root keys, no
// listener and no extenders to dial -- only the operator (D3).
func newTestMember(t testing.TB, config *testConfig, operatorAddrs []ma.Multiaddr) *testNode {
	t.Helper()
	identityKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	directory := newDirectory(ctx, config.operator)

	settings := connectgossip.DefaultNodeSettings(connectgossip.NodeRoleMember)
	settings.NetworkHost = config.operator.networkHost
	settings.Directory = directory
	settings.IdentityKeySeed = identityKeySeed
	settings.OperatorAddrs = operatorAddrs
	settings.PeerTarget = 0
	// a round often enough that a restarted operator is redialed inside the
	// barrier below rather than a minute later
	settings.PeerTimeout = 500 * time.Millisecond
	settings.PeerCoalesceTimeout = 10 * time.Millisecond
	settings.StatusTimeout = 50 * time.Millisecond
	node, err := connectgossip.NewNode(ctx, settings)
	if err != nil {
		directory.Close()
		cancel()
		t.Fatal(err)
	}
	return &testNode{
		directory: directory,
		node:      node,
		cancel:    cancel,
	}
}

// Waits for a node's status, watching the status monitor rather than polling.
func waitForNodeStatus(
	t testing.TB,
	node *connectgossip.Node,
	what string,
	condition func(status connectgossip.NodeStatus) bool,
) {
	t.Helper()
	deadline := time.Now().Add(testMeshTimeout)
	for {
		status, change := node.StatusMonitor().Get()
		if condition(status) {
			return
		}
		select {
		case <-change:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the node did not reach %s, status = %+v", what, node.Status())
		}
	}
}

// The same for a directory, watching its change monitor.
func waitForDirectory(
	t testing.TB,
	directory *connect.ExtenderDirectory,
	what string,
	condition func(snapshot *connect.ExtenderDirectorySnapshot) bool,
) {
	t.Helper()
	deadline := time.Now().Add(testMeshTimeout)
	for {
		_, change := directory.ChangeMonitor().Get()
		if condition(directory.Snapshot()) {
			return
		}
		select {
		case <-change:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the directory did not reach %s", what)
		}
	}
}

// The state of one address in a snapshot, empty when it is not known.
func snapshotState(snapshot *connect.ExtenderDirectorySnapshot, ip netip.Addr) string {
	for _, entry := range snapshot.Entries {
		if entry.Ip == ip {
			return entry.State
		}
	}
	return ""
}

// What the drain published, in order, optionally forwarding to a real node.
// Recording the message rather than the row is what lets a test see the drain
// the way the mesh does.
type testPublisher struct {
	// nil records only, which is what the queue tests need
	publisher messagePublisher
	// set to fail every publish, which is what proves a failed row is kept
	err error

	stateLock sync.Mutex
	messages  []*protocol.ExtenderGossipMessage
}

func (self *testPublisher) Publish(
	ctx context.Context,
	message *protocol.ExtenderGossipMessage,
) error {
	if self.err != nil {
		return self.err
	}
	if self.publisher != nil {
		if err := self.publisher.Publish(ctx, message); err != nil {
			return err
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.messages = append(self.messages, message)
	return nil
}

// The identity key of every message published, in publish order, which is what
// an ordering assertion compares. A record and a revocation of the same key
// are distinguished by the prefix.
func (self *testPublisher) publishedKeyHexes() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	keyHexes := []string{}
	for _, message := range self.messages {
		switch {
		case message.GetRecord() != nil:
			body := &protocol.ExtenderRecordBody{}
			if err := proto.Unmarshal(message.GetRecord().GetBody(), body); err != nil {
				keyHexes = append(keyHexes, "unreadable")
				continue
			}
			keyHexes = append(keyHexes, "record:"+hex.EncodeToString(body.GetPublicKey()))
		case message.GetRevocation() != nil:
			body := &protocol.ExtenderRevocationBody{}
			if err := proto.Unmarshal(message.GetRevocation().GetBody(), body); err != nil {
				keyHexes = append(keyHexes, "unreadable")
				continue
			}
			keyHexes = append(keyHexes, "revocation:"+hex.EncodeToString(body.GetPublicKey()))
		default:
			keyHexes = append(keyHexes, "empty")
		}
	}
	return keyHexes
}

// One activated extender in the operator's tables, with one active address of
// the given family.
type testExtender struct {
	extenderId server.Id
	publicKey  ed25519.PublicKey
	ip         netip.Addr
	ipVersion  int
}

func newTestExtender(t testing.TB, ctx context.Context, index int, ipVersion int) *testExtender {
	t.Helper()
	keySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(keySeed)
	if err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 1+index))
	if ipVersion == 6 {
		ip = netip.MustParseAddr(fmt.Sprintf("2001:db8::%d", 1+index))
	}
	extenderId := server.NewId()
	createTime := server.NowUtc()
	model.Testing_CreateNetworkExtender(
		ctx,
		&model.NetworkExtender{
			ExtenderId:  extenderId,
			NetworkId:   server.NewId(),
			ClientId:    server.NewId(),
			PublicKey:   publicKey,
			CreateTime:  createTime,
			TcpPort:     443,
			UdpPort:     443,
			DnsPort:     53,
			DnsTld:      connect.DefaultExtenderDnsTld,
			CountryCode: "US",
			Active:      true,
		},
		[]*model.NetworkExtenderAddress{
			{
				IpVersion:    ipVersion,
				Ip:           ip,
				Carriers:     []string{connect.ExtenderCarrierTcp},
				ActivateTime: createTime,
				Active:       true,
			},
		},
	)
	return &testExtender{
		extenderId: extenderId,
		publicKey:  publicKey,
		ip:         ip,
		ipVersion:  ipVersion,
	}
}

// Queues one signed record for this extender, exactly as the drip does (C4).
func (self *testExtender) queueRecord(t testing.TB, ctx context.Context, config *testConfig) {
	t.Helper()
	published := model.PublishNetworkExtenderRecord(
		ctx,
		self.extenderId,
		func(
			extender *model.NetworkExtender,
			addresses []*model.NetworkExtenderAddress,
			issueTime time.Time,
		) ([]byte, error) {
			_, message, err := controller.SignExtenderRecord(
				config.extender,
				config.rootPrivateKey,
				extender,
				addresses,
				issueTime,
			)
			return message, err
		},
	)
	if !published {
		t.Fatalf("the record of extender %s was not queued", self.extenderId)
	}
}

// Queues one signed revocation by losing this extender's last address, exactly
// as the probe task does (C3).
func (self *testExtender) queueRevocation(t testing.TB, ctx context.Context, config *testConfig) {
	t.Helper()
	outcome := model.RecordNetworkExtenderProbeResult(
		ctx,
		self.extenderId,
		self.ipVersion,
		false,
		server.NowUtc(),
		1,
		func(extender *model.NetworkExtender, issueTime time.Time) ([]byte, error) {
			_, message, err := controller.SignExtenderRevocation(
				config.extender,
				config.rootPrivateKey,
				extender,
				issueTime,
			)
			return message, err
		},
	)
	if !outcome.ExtenderRevoked {
		t.Fatalf("the revocation of extender %s was not queued", self.extenderId)
	}
}

// Queues one row whose message is not a gossip message at all, which is what a
// writer storing something a reader cannot take back would leave behind.
func queueUndecodablePublish(t testing.TB, ctx context.Context, extender *testExtender) {
	t.Helper()
	published := model.PublishNetworkExtenderRecord(
		ctx,
		extender.extenderId,
		func(
			_ *model.NetworkExtender,
			_ []*model.NetworkExtenderAddress,
			_ time.Time,
		) ([]byte, error) {
			return []byte("this is not a serialized extender gossip message"), nil
		},
	)
	if !published {
		t.Fatalf("the undecodable row of extender %s was not queued", extender.extenderId)
	}
}

// The queue, oldest first, as the claim reads it.
func testPublishes(ctx context.Context) []*model.NetworkExtenderPublish {
	return model.Testing_GetNetworkExtenderPublishes(ctx)
}

// The identity of one test extender as a published message names it.
func hexKey(extender *testExtender) string {
	return hex.EncodeToString(extender.publicKey)
}

// What a publisher that cannot reach the topic returns.
var errTestPublish = errors.New("the test publisher refuses")
