// Exercise raw TCP opening for one signed contract request and its readiness probe.
package perfvar

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

func TestSdkSignedControlLiteralTunHealthy(t *testing.T) {
	sdkSignedControlLiteralTun(t, sdkSignedControlLossNone)
}

func TestSdkSignedControlLiteralTunUsesSecondRawAttempt(t *testing.T) {
	sdkSignedControlLiteralTun(t, sdkSignedControlLossPostSyn)
}

// A lost readiness SYN must recover on a second raw socket before the POST.
func TestSdkSignedControlHelloLiteralTunUsesSecondRawAttempt(t *testing.T) {
	sdkSignedControlLiteralTun(t, sdkSignedControlLossHelloSyn)
}

type sdkSignedControlLoss int

const (
	sdkSignedControlLossNone sdkSignedControlLoss = iota
	sdkSignedControlLossPostSyn
	sdkSignedControlLossHelloSyn
)

// The fixture authority admits one application-lane contract under a verified caller.
// Its in-memory signing key replaces storage; the client, OOB codec, HTTP and TUN are real.
type sdkSignedControlAuthority struct {
	source, destination, contract clientconnect.Id
	verifier                      ed25519.PublicKey
	grantKey                      [32]byte
}

func (self *sdkSignedControlAuthority) authorizeAndGrant(r *http.Request) (*clientconnect.ConnectControlResult, error) {
	bearer, present := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !present {
		return nil, fmt.Errorf("missing bearer")
	}
	_, err := jwt.ParseWithClaims(bearer, &jwt.RegisteredClaims{}, func(_ *jwt.Token) (any, error) {
		return self.verifier, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience("test-signed-control"), jwt.WithSubject(self.source.String()), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("invalid signed caller: %w", err)
	}

	var args clientconnect.ConnectControlArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		return nil, err
	}
	wire, err := base64.StdEncoding.DecodeString(args.Pack)
	if err != nil {
		return nil, err
	}
	var request protocol.Pack
	if err := proto.Unmarshal(wire, &request); err != nil {
		return nil, err
	}
	if len(request.Frames) != 1 || request.Frames[0].GetMessageType() != protocol.MessageType_TransferCreateContract {
		return nil, fmt.Errorf("expected one contract create frame")
	}
	var create protocol.CreateContract
	if err := proto.Unmarshal(request.Frames[0].MessageBytes, &create); err != nil {
		return nil, err
	}
	if !bytes.Equal(create.DestinationId, self.destination.Bytes()) || create.TransferByteCount < 4096 || create.GetSenderRole() != protocol.SequenceRole_SequenceRoleClient {
		return nil, fmt.Errorf("unexpected contract destination, size or sender role")
	}

	stored, err := proto.Marshal(&protocol.StoredContract{ContractId: self.contract.Bytes(), SourceId: self.source.Bytes(), DestinationId: self.destination.Bytes(), TransferByteCount: create.TransferByteCount})
	if err != nil {
		return nil, err
	}
	signature := clientconnect.SignStoredContract(clientconnect.DefaultContractManagerSettingsNoNetworkEvents(), self.grantKey[:], stored)
	result, err := clientconnect.ToFrame(&protocol.CreateContractResult{CreateContract: &create, Contract: &protocol.Contract{StoredContractBytes: stored, StoredContractHmac: signature, ProvideMode: protocol.ProvideMode_Public}}, clientconnect.DefaultProtocolVersion)
	if err != nil {
		return nil, err
	}
	defer clientconnect.MessagePoolReturn(result.MessageBytes)
	payload, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{result}})
	if err != nil {
		return nil, err
	}
	return &clientconnect.ConnectControlResult{Pack: base64.StdEncoding.EncodeToString(payload)}, nil
}

func sdkSignedControlLiteralTun(t *testing.T, loss sdkSignedControlLoss) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(4013)["clean-lan"]
	profile.InnerMtu = carrierTunMtu(profile)
	path, err := newTunPath(ctx, profile, defaultTunResourceProfile())
	if err != nil {
		t.Fatal(err)
	}
	defer path.close()
	listener, err := path.right.ListenTCP(&net.TCPAddr{IP: path.endpointAddress(false)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peer := sdkH1LiteralTunEndpoint(listener.Addr())
	localIp := path.left.LocalAddresses()[0].Unmap()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority := sdkSignedControlAuthority{source: clientconnect.NewId(), destination: clientconnect.NewId(), contract: clientconnect.NewId(), verifier: public}
	if _, err := rand.Read(authority.grantKey[:]); err != nil {
		t.Fatal(err)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.RegisteredClaims{Subject: authority.source.String(), Audience: jwt.ClaimStrings{"test-signed-control"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}).SignedString(private)
	if err != nil {
		t.Fatal(err)
	}

	tap := time.Now()
	var scopeStart atomic.Int64
	var helloCount, postCount atomic.Int64
	var helloPeer, postPeer atomic.Pointer[netip.AddrPort]
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, addrErr := netip.ParseAddrPort(r.RemoteAddr)
		if addrErr != nil || r.TLS != nil || r.ProtoMajor != 1 || r.Host != peer.String() {
			t.Error("unexpected control socket or authority")
			http.Error(w, "authority", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/hello" {
			helloCount.Add(1)
			helloPeer.Store(&addr)
			// This independent qualification socket cannot satisfy the subsequent POST.
			w.Header().Set("Connection", "close")
			scopeStart.Store(time.Since(tap).Nanoseconds())
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/connect/control" {
			t.Error("unexpected HTTP operation")
			http.NotFound(w, r)
			return
		}
		postCount.Add(1)
		result, err := authority.authorizeAndGrant(r)
		if err != nil {
			t.Error(err)
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		postPeer.Store(&addr)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		server.Close()
		if err := <-serveDone; err != http.ErrServerClosed {
			t.Error(err)
		}
	}()

	trace := &sdkH1LiteralTunOpeningTrace{device: localIp, peer: peer, tap: tap}
	var removers [2]func()
	for direction, link := range [2]*directionalLink{path.forwardLink, path.reverseLink} {
		remove, ok := link.installPacketTraceForTest(func(stage string, observation linkScheduleObservation, packet []byte) {
			trace.observe(direction, stage, observation, packet)
		})
		if !ok {
			t.Fatal("link hook already owned")
		}
		defer remove()
		removers[direction] = remove
	}
	var dropped atomic.Pointer[nativeDuplicateAckTcpHeader]
	if loss != sdkSignedControlLossNone {
		path.forwardLink.forcedLossForTest.Store(&linkLossTestHook{drop: func(packet []byte) bool {
			if helloStarted := scopeStart.Load() != 0; helloStarted == (loss == sdkSignedControlLossHelloSyn) {
				return false
			}
			header, ok := nativeDuplicateAckHeader(packet)
			return ok && header.flags&0x17 == 0x02 && header.payloadByteCount == 0 &&
				netip.AddrFrom4(header.sourceIp) == localIp && netip.AddrPortFrom(netip.AddrFrom4(header.destinationIp), header.destinationPort) == peer && dropped.CompareAndSwap(nil, &header)
		}})
		defer path.forwardLink.forcedLossForTest.Store(nil)
	}

	settings := routeClientStrategySettings()
	settings.EnableNormal, settings.EnableResilient = true, false
	settings.MinNextConnectDelay, settings.MaxNextConnectDelay = 0, 0
	settings.ConnectSettings.DialContextSettings = &clientconnect.DialContextSettings{DialContext: path.left.DialContext}
	strategy := clientconnect.NewClientStrategy(ctx, settings)
	defer strategy.Close()
	control := clientconnect.NewApiOutOfBandControl(ctx, strategy, token, "http://"+peer.String())
	clientSettings := clientconnect.DefaultClientSettingsNoNetworkEvents()
	clientSettings.ControlPingTimeout = 0
	clientSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	clientSettings.Log = clientconnect.NewNoopLogger()
	client := clientconnect.NewClient(ctx, authority.source, control, clientSettings)
	defer func() {
		if err := client.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	}()
	key := clientconnect.ContractKey{Destination: clientconnect.DestinationId(authority.destination), LogicalLane: 4}
	manager := client.ContractManager()
	manager.CreateContract(key, 0, 4096)
	grant := manager.TakeContract(ctx, key, 6*time.Second)
	if grant == nil || !clientconnect.VerifyStoredContract(clientSettings.ContractManagerSettings, authority.grantKey[:], grant.StoredContractBytes, grant.StoredContractHmac) {
		t.Fatal("manager did not take the verified signed grant")
	}
	var stored protocol.StoredContract
	if err := proto.Unmarshal(grant.StoredContractBytes, &stored); err != nil || !bytes.Equal(stored.GetContractId(), authority.contract.Bytes()) || !bytes.Equal(stored.GetSourceId(), authority.source.Bytes()) || !bytes.Equal(stored.GetDestinationId(), authority.destination.Bytes()) || stored.TransferByteCount < 4096 {
		t.Fatal("granted contract does not match its owner")
	}
	// Join the real operation before counting HTTP requests and stopping the link observation.
	if err := control.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	if !path.waitForTerminalIdle(ctx) {
		t.Fatal("link did not reach terminal idle")
	}
	for _, remove := range removers {
		remove()
	}
	begin := scopeStart.Load()
	hello, winner := helloPeer.Load(), postPeer.Load()
	if begin <= 0 || trace.incomplete || helloCount.Load() != 1 || postCount.Load() != 1 || hello == nil || winner == nil || *hello == *winner {
		t.Fatalf("expected distinct qualification and one authorized POST socket, hello=%d posts=%d", helloCount.Load(), postCount.Load())
	}
	openingOwner := winner
	if loss == sdkSignedControlLossHelloSyn {
		openingOwner = hello
	}
	var forward []sdkH1LiteralTunOpening
	for _, opening := range trace.rows[:trace.count] {
		if !opening.Reverse && (opening.StartNs < begin) == (loss == sdkSignedControlLossHelloSyn) {
			forward = append(forward, opening)
		}
	}
	sort.Slice(forward, func(i, j int) bool { return forward[i].LinkSequence < forward[j].LinkSequence })
	if loss == sdkSignedControlLossNone {
		if len(forward) != 1 || forward[0].Terminal != "delivered" || forward[0].Local != *winner || !sdkH1LiteralTunPeerAck(trace.rows[:trace.count], forward[0]) {
			t.Fatalf("healthy POST opening was not unique: %+v", forward)
		}
		t.Logf("SIGNED_CONTROL_TUN_HEALTHY: socket=%s typed_posts=1 verified_grants=1", *winner)
		return
	}
	firstDrop := dropped.Load()
	if len(forward) < 2 || firstDrop == nil || forward[0].LinkSequence == 0 || forward[0].Terminal != "loss-drop" || forward[0].Local.Port() != firstDrop.sourcePort || forward[0].Sequence != firstDrop.sequence {
		t.Fatalf("missing initial dropped SYN: %+v", forward)
	}
	first, next := forward[0], forward[1]
	if next.LinkSequence <= first.LinkSequence || next.Terminal != "delivered" || next.Local != *openingOwner || !sdkH1LiteralTunPeerAck(trace.rows[:trace.count], next) {
		t.Fatalf("second observed opening did not own the HTTP request and SYN-ACK: %+v", forward)
	}
	if next.Local == first.Local && next.Sequence == first.Sequence {
		if loss == sdkSignedControlLossHelloSyn {
			t.Fatalf("SIGNED_CONTROL_HELLO_FIRST_SYN_RETRY_PRECEDED_HEDGE: first=%s next=%s isn=%d hello=1 typed_posts=1 verified_grants=1", first.Local, next.Local, next.Sequence)
		}
		t.Fatalf("SIGNED_CONTROL_TUN_FIRST_SYN_RETRY_PRECEDED_HEDGE: first=%s next=%s isn=%d typed_posts=1 verified_grants=1", first.Local, next.Local, next.Sequence)
	}
	if next.Local == first.Local {
		t.Fatal("same tuple with conflicting TCP generation")
	}
	if loss == sdkSignedControlLossHelloSyn {
		t.Logf("SIGNED_CONTROL_HELLO_SECOND_RAW_SOCKET_WON: first=%s next=%s hello=1 typed_posts=1 verified_grants=1", first.Local, next.Local)
		return
	}
	t.Logf("SIGNED_CONTROL_TUN_SECOND_RAW_SOCKET_WON: first=%s next=%s typed_posts=1 verified_grants=1", first.Local, next.Local)
}
