//go:build sidecar_pending

package controller

// BRIDGE-M6-08's rows on T3 (§22.4; FS-38's share rows, CC-05, §22.7's FriendsAndFamily clause): sidecartest's
// BridgeShareNet, the same schedules sidecartest runs on T2, on server's platform (bridgeM6Platform): both networks'
// clients rows of server's, each sidecar's own client bound to its sidecar, every control frame they send answered by
// server's controller (bridgeM4Platform, connect_bridge_m4_test.go), and server's own contract gate over both networks'
// verified states, with network_acl_head raised for each and sidecar.yml's switch on. So a reader's window gets its
// contracts from server's CreateContract: the gate's share half (acl_share.go) and model.AclShareBetween settle it
// FriendsAndFamily, C13's seals are the subject and network server's rows hold, the host's companion carries the
// forward contract's seals, each is escrowed where server's escrow puts it, read back from transfer_contract, and a
// reader Y grants nothing on X is refused. The operator's windows are minted through the host's provide secret, which
// the test stack holds.
//
// X's state is the one X's readers verified; Y's is hand-made (sidecartest.FakeAclView), as ACL's own CreateContract
// tests make theirs. The Net's clock starts at the wall clock, because server's gate judges X's grant on server's
// clock, which a fixed start years apart would find lapsed. Run under -tags sidecar_pending through
// .stack/server-test.sh.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// server's platform for a share's two networks: bridgeM4Platform's rows and controller, server's contract gate over
// the states a share world hands in, and each contract's payer from transfer_contract. Safe for concurrent use.
type bridgeM6Platform struct {
	*bridgeM4Platform
	states *bridgeM6States
}

var _ sidecartest.BridgeNetSharePlatform = (*bridgeM6Platform)(nil)

func newBridgeM6Platform(t testing.TB, ctx context.Context) *bridgeM6Platform {
	return &bridgeM6Platform{
		bridgeM4Platform: newBridgeM4Platform(t, ctx),
		states:           &bridgeM6States{snapshots: map[sidecar.Id]*sidecar.AclGateSnapshot{}},
	}
}

// The states a share world handed in, as the process's gate cache would hold them loaded: a
// sidecar.AclContractStateSource. Safe for concurrent use.
type bridgeM6States struct {
	stateLock sync.Mutex
	snapshots map[sidecar.Id]*sidecar.AclGateSnapshot
}

var _ sidecar.AclContractStateSource = (*bridgeM6States)(nil)

func (self *bridgeM6States) State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	snapshot := self.snapshots[networkId]
	if snapshot == nil {
		return nil, sidecar.AclGateUnknown
	}
	return snapshot, sidecar.AclGateLoaded
}

// Every state is handed in whole, so a load asked for changes nothing.
func (self *bridgeM6States) Changed(networkId sidecar.Id) {
}

// Each network's head row raised to its state's position, as api's sequencer raises it, the states kept, and a gate
// over them installed in place of any before, its heads read once before this returns, with sidecar.yml's switch on
// until the test ends.
func (self *bridgeM6Platform) SetShareStates(ctx context.Context, states map[sidecar.Id]*sidecar.AclGateSnapshot) (returnErr error) {
	for networkId, snapshot := range states {
		if snapshot == nil || snapshot.Acl == nil {
			return fmt.Errorf("network %s's state holds no acl part", networkId)
		}
		self.network(server.Id(networkId))
		chain := sha256.Sum256(networkId.Bytes())
		if _, err := model.AdvanceNetworkAclHead(self.ctx, server.Id(networkId), max(1, snapshot.Pin.GetVersion()), chain[:], true); err != nil {
			return fmt.Errorf("network %s's head row: %w", networkId, err)
		}
		func() {
			self.states.stateLock.Lock()
			defer self.states.stateLock.Unlock()
			self.states.snapshots[networkId] = snapshot
		}()
	}
	loads := make(chan error, 1)
	settings := sidecar.DefaultAclContractGateSettings()
	settings.HeadsInterval = 0
	settings.OnHeads = func(networkCount int, err error) {
		select {
		case loads <- err:
		default:
		}
	}
	gate := NewAclContractGate(self.ctx, self.states, settings)
	select {
	case err := <-loads:
		if err != nil {
			gate.Close()
			return fmt.Errorf("the gate's heads: %w", err)
		}
	case <-time.After(30 * time.Second):
		gate.Close()
		return errors.New("the gate read no heads")
	case <-self.ctx.Done():
		gate.Close()
		return self.ctx.Err()
	}
	restoreGate := SetAclContractGate(gate)
	restoreConfig := server.Testing_PushSidecarConfig("sidecar:\n  acl_contract_gate: {enabled: true}\n")
	self.t.Cleanup(func() {
		restoreConfig()
		restoreGate()
		gate.Close()
	})
	return nil
}

// transfer_contract's payer_network_id for the contract, false for a contract server holds no row of.
func (self *bridgeM6Platform) ContractPayer(ctx context.Context, contractId connect.Id) (payer sidecar.Id, found bool, returnErr error) {
	if r := server.HandleError(func() {
		server.Db(self.ctx, func(conn server.PgConn) {
			result, err := conn.Query(self.ctx, `SELECT payer_network_id FROM transfer_contract WHERE contract_id = $1`, server.Id(contractId))
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var payerNetworkId *server.Id
					server.Raise(result.Scan(&payerNetworkId))
					if payerNetworkId != nil {
						payer, found = sidecar.Id(*payerNetworkId), true
					}
				}
			})
		})
	}); r != nil {
		return sidecar.Id{}, false, fmt.Errorf("transfer_contract: %v", r)
	}
	return payer, found, nil
}

// Runs builder's schedule on a Net of its own, its clock starting at the wall clock, whose BridgeNet takes server's
// share platform, failing t with its trace.
func bridgeM6RunSchedule(t testing.TB, ctx context.Context, name string, builder fault.Builder) {
	t.Helper()
	netSettings := sidecartest.DefaultNetSettings()
	netSettings.ClockStart = server.NowUtc()
	net := sidecartest.NewNet(ctx, netSettings)
	defer net.Close()
	settings := sidecartest.DefaultBridgeNetSettings()
	settings.Platform = newBridgeM6Platform(t, ctx)
	sidecartest.SetNetBridgeSettings(net, settings)
	defer sidecartest.SetNetBridgeSettings(net, nil)
	if result := fault.Run(ctx, builder(net), net); result.Err != nil {
		t.Fatalf("%s on T3: %s", name, result)
	}
}

// FS-38's share rows on server's platform, and §22.7's FriendsAndFamily clause: the twin, whose reader's window runs on
// server's CreateContract and gate, its contracts' seals and payers server's, beside the operator's minted reach; and
// each refusal, the host's restart from its persisted state included, and CC-05's reader the gate refuses.
func TestFs38ShareOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM6RunSchedule(t, ctx, "FS-38's share twin", fault.Fs38ShareSigner)
		bridgeM6RunSchedule(t, ctx, "FS-38's share", fault.Fs38ShareCase.Build)
	})
}

// A network's sidecar on server's share routes: bound, as POST /sidecar/bind leaves it, and the session api builds for
// its client's bearer.
type bridgeM6ShareSide struct {
	networkId server.Id
	sidecarId server.Id
	session   *session.ClientSession
}

// A sidecar bound in a network of its own, its keys synthetic.
func newBridgeM6ShareSide(t testing.TB, ctx context.Context) *bridgeM6ShareSide {
	t.Helper()
	networkId, sidecarId, clientId := server.NewId(), server.NewId(), server.NewId()
	key := func() []byte {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		return key
	}
	record := &model.SidecarBindingRecord{
		NetworkId: networkId, SidecarId: sidecarId, ClientId: clientId, Generation: 1,
		ClientPublicKey: key(), IdentityPublicKey: key(), SignedBinding: append([]byte("synthetic binding of "), clientId.Bytes()...),
	}
	if outcome, _ := model.BindSidecarIdentity(ctx, record, server.NowUtc()); outcome != model.SidecarBindAppended {
		t.Fatalf("bind: %s", outcome)
	}
	return &bridgeM6ShareSide{
		networkId: networkId,
		sidecarId: sidecarId,
		session:   session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, ClientId: &clientId}),
	}
}

// The row a route answered, as a sidecar reads it.
func bridgeM6ShareRow(result *model.NetworkBridgeShareResult) *sidecar.BridgeShareRow {
	row := &sidecar.BridgeShareRow{
		ShareId:            sidecar.Id(result.ShareId),
		SharingNetworkId:   sidecar.Id(result.NetworkId),
		TargetTable:        result.TargetTable,
		TargetTableVersion: uint64(result.TargetTableVersion),
		AliasTable:         result.AliasTable,
		AliasTableVersion:  uint64(result.AliasTableVersion),
		AcceptStatus:       sidecar.BridgeShareAcceptStatus(result.AcceptStatus),
	}
	if result.InvitedNetworkId != nil {
		row.AcceptingNetworkId = sidecar.Id(*result.InvitedNetworkId)
	}
	return row
}

// §22.7's oversized (/8, /16) and full-block shares refused with their reason, end to end through server's share routes
// on the private stack (BRIDGE-M6-08; §17.9's space row, §8.11, §17.16): X's sidecar seals its targets under the
// code's K and files the invite as POST /acl/invite does (model.AclInvite under X's session); Y's sidecar claims it as
// POST /acl/accept-invite does, opens the target table under its own K from the code, runs §17.9's preflight on its
// ledger, here holding no alias, and posts the refusal in the filer's own form (sidecar.BridgeShareRefusalCompletion,
// what BridgeFiler.AcceptInvite posts); X's sidecar reads the row back as GET /network/bridge/share/<share_id> does and
// opens the status and the same figures; and the refusal is final, Y's later completion of other bytes refused. A full
// block is the upper /17 the first of two /17 targets takes whole. The filer itself, over a T3 sidecar, is not run
// here: no T3 harness builds one (bridge_share_invite_test.go runs both networks' filers over the routes' fake).
func TestBridgeShareRefusalsThroughTheRoutesOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(model.Testing_SetNetworkBridgeShareOutboundCheck(func(ctx context.Context, networkId server.Id) (bool, error) {
			return true, nil
		}))
		for _, c := range []struct {
			name     string
			prefixes []netip.Prefix
			status   sidecar.BridgeShareAcceptStatus
			reason   error
			index    uint32
		}{
			{name: "a /8", prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, status: sidecar.BridgeShareAcceptStatusRefusedTooLarge, reason: sidecar.ErrBridgeShareTooLarge, index: 0},
			{name: "a /16", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.7.0/24"), netip.MustParsePrefix("172.16.0.0/16")}, status: sidecar.BridgeShareAcceptStatusRefusedTooLarge, reason: sidecar.ErrBridgeShareTooLarge, index: 1},
			{name: "two /17s", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/17"), netip.MustParsePrefix("192.168.128.0/17")}, status: sidecar.BridgeShareAcceptStatusRefusedNoSpace, reason: sidecar.ErrBridgeShareNoSpace, index: 1},
		} {
			x, y := newBridgeM6ShareSide(t, ctx), newBridgeM6ShareSide(t, ctx)
			code := make([]byte, 32)
			if _, err := rand.Read(code); err != nil {
				t.Fatal(err)
			}
			codeHash := sha256.Sum256(code)
			hostKey := make([]byte, 32)
			if _, err := rand.Read(hostKey); err != nil {
				t.Fatal(err)
			}

			// X: its targets sealed under K, and the invite
			xSeal, err := sidecar.NewBridgeShareSeal(code, sidecar.Id(x.networkId), nil)
			if err != nil {
				t.Fatal(err)
			}
			targets := &bridge.BridgeShareTargets{}
			for index, prefix := range c.prefixes {
				address := prefix.Addr().As4()
				targets.Targets = append(targets.Targets, &bridge.BridgeShareTarget{
					Index:                 uint32(index),
					HostSidecarId:         x.sidecarId.Bytes(),
					HostIdentityPublicKey: hostKey,
					Prefix:                &bridge.BridgeIpv4Prefix{Address: binary.BigEndian.Uint32(address[:]), Length: uint32(prefix.Bits())},
				})
			}
			targetTable, err := xSeal.Seal(sidecar.BridgeShareLabelTargets, sidecar.Id{}, 1, targets, nil)
			if err != nil {
				t.Fatal(err)
			}
			invite, err := model.AclInvite(&model.AclInviteArgs{CodeHash: codeHash[:], UseCount: 1, TargetTable: targetTable, TargetTableVersion: 1}, x.session)
			if err != nil {
				t.Fatalf("%s: the invite: %v", c.name, err)
			}

			// Y: the claim, the target table under its own K, the preflight, and the refusal as the filer posts it
			claimed, err := model.AclAcceptInvite(&model.AclAcceptInviteArgs{CodeHash: codeHash[:]}, y.session)
			if err != nil || claimed.ShareId != invite.ShareId || claimed.NetworkId != x.networkId {
				t.Fatalf("%s: the claim: %+v, %v", c.name, claimed, err)
			}
			ySeal, err := sidecar.NewBridgeShareSeal(code, sidecar.Id(claimed.NetworkId), nil)
			if err != nil {
				t.Fatal(err)
			}
			claimedRow := bridgeM6ShareRow(claimed)
			opened, err := sidecar.OpenBridgeShareRow(ySeal, &sidecar.BridgeShareRow{
				ShareId: claimedRow.ShareId, SharingNetworkId: claimedRow.SharingNetworkId,
				TargetTable: claimedRow.TargetTable, TargetTableVersion: claimedRow.TargetTableVersion,
			}, nil)
			if err != nil {
				t.Fatalf("%s: Y's opening of the target table: %v", c.name, err)
			}
			_, err = sidecar.BridgeSharePreflight((*sidecar.BridgeLog)(nil).Open(nil), opened.Targets)
			refusal := (*sidecar.BridgeShareRefusal)(nil)
			if !errors.As(err, &refusal) || !errors.Is(err, c.reason) {
				t.Fatalf("%s: the preflight: %v", c.name, err)
			}
			accept, status, err := sidecar.BridgeShareRefusalCompletion(ySeal, sidecar.Id(y.networkId), refusal, nil)
			if err != nil || status != c.status {
				t.Fatalf("%s: the refusal's completion: %s, %v", c.name, status, err)
			}
			completed, err := model.AclAcceptInvite(&model.AclAcceptInviteArgs{
				ShareId: &claimed.ShareId, AliasTable: accept, AliasTableVersion: 1, AcceptStatus: model.NetworkBridgeShareAcceptStatus(status),
			}, y.session)
			if err != nil || string(completed.AcceptStatus) != string(c.status) {
				t.Fatalf("%s: the completion: %+v, %v", c.name, completed, err)
			}

			// X: the row as the route serves it, opened under K
			read, err := model.GetNetworkBridgeShareForSidecar(invite.ShareId, x.session)
			if err != nil {
				t.Fatalf("%s: X's read: %v", c.name, err)
			}
			xOpened, err := sidecar.OpenBridgeShareRow(xSeal, bridgeM6ShareRow(read), nil)
			if err != nil {
				t.Fatalf("%s: X's opening of the row: %v", c.name, err)
			}
			if xOpened.AcceptStatus != c.status || xOpened.AcceptingNetworkId != sidecar.Id(y.networkId) || xOpened.Aliases != nil ||
				len(xOpened.SharePublicKey) != 0 || xOpened.Shortfall.GetIndex() != c.index || !proto.Equal(xOpened.Shortfall, refusal.Shortfall) {
				t.Errorf("%s: X opens %+v, the refusal %v", c.name, xOpened, refusal.Shortfall)
			}
			// a refusal is final: another completion of other bytes is refused
			again, _, err := sidecar.BridgeShareRefusalCompletion(ySeal, sidecar.Id(y.networkId), refusal, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := model.AclAcceptInvite(&model.AclAcceptInviteArgs{
				ShareId: &claimed.ShareId, AliasTable: again, AliasTableVersion: 1, AcceptStatus: model.NetworkBridgeShareAcceptStatus(status),
			}, y.session); err == nil || !strings.HasPrefix(err.Error(), "409") {
				t.Errorf("%s: a second completion: %v", c.name, err)
			}
		}
	})
}
