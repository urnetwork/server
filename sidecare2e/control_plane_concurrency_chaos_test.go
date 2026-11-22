//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The control-plane case's chaos (control_plane_concurrency_test.go's fifth part; the end-to-end plan's
// e2e-control-plane-concurrency): api's host and a member's device restarted mid-save and mid-invite, each keeping its
// stores, on a personal network of ChaosDeviceCount devices and a team of its Owner's devices and an Admin's.
//
// api's restart is sidecart3's: its sequencer with its caches, its gate cache and the installs over it dropped and
// built again on the same stores (T3World.RestartApi), at a request a chaos hook in front of api matches
// (controlPlaneApiChaos, on T3Api.SetApiHook):
//
//	acted     api answers the request in full, then restarts, and the answer is lost (502, as an lb whose upstream
//	          went away before the answer reached it): an invite's accept, a base's publish, a statement's delta
//	lost      api restarts with the request unseen, which is lost the same way: an invite's accept, a base's publish
//	held      the request waits at the hook until the test releases it, or until its sender gives it up: a device's
//	          base publish held while that device restarts
//
// A device's restart is T3Device.Restart: its Sidecar and instance end and a new one starts on the same store and key
// store, keeping its enrollment, keys and anchor, with new key clients and a follower from its kept anchor.
//
// What must hold, after each chaos and its retry where the sender has one: nothing acknowledged is lost, and nothing
// is applied twice. An accept whose answer was lost made one membership, which a retry finds (§7.5: the invite is
// spent); an accept lost unseen made none, and its retry makes one; a statement whose answer was lost stands once, and
// the same signed bytes posted again are refused as a replay or answered at the entry already holding them, never a
// second entry; a base whose answer was lost, or whose publish was lost unseen or given up by a restarting device,
// leaves every device of the network converged on one pin whose head holds the saved document, the document saved once
// more planning no change; and a restarted inviting device still signs, its kept identity's MemberAdmit landing once.
// Each step is bounded by the case's PhaseTimeout.

package sidecare2e

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/sidecart3"
)

// The chaos kinds at api (the file's header).
const (
	controlPlaneChaosActed = iota
	controlPlaneChaosLost
	controlPlaneChaosHeld
)

// The chaos hook in front of api: armed for the next request a match takes, it runs its kind around that request,
// once. Safe for concurrent use.
type controlPlaneApiChaos struct {
	world *World

	stateLock sync.Mutex
	armed     *controlPlaneChaosArm
}

// One armed chaos.
type controlPlaneChaosArm struct {
	route string
	kind  int
	// closed once the hook took its request
	taken chan struct{}
	// a held request goes on to api once this closes
	release chan struct{}
	// the restart's error, or the held request's end, once the chaos ran
	done chan error
}

// The hook, installed in front of api until the world closes.
func newControlPlaneApiChaos(world *World) *controlPlaneApiChaos {
	self := &controlPlaneApiChaos{world: world}
	world.T3().Api().SetApiHook(self.hook)
	world.T3().OnClose(func() { world.T3().Api().SetApiHook(nil) })
	return self
}

// Arms kind for the next request to route, "METHOD /path".
func (self *controlPlaneApiChaos) arm(route string, kind int) *controlPlaneChaosArm {
	arm := &controlPlaneChaosArm{route: route, kind: kind, taken: make(chan struct{}), release: make(chan struct{}), done: make(chan error, 1)}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.armed = arm
	return arm
}

// Takes back an armed chaos no request took.
func (self *controlPlaneApiChaos) disarm() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.armed = nil
}

// Takes the armed chaos for request, nil when none matches it.
func (self *controlPlaneApiChaos) take(request *http.Request) *controlPlaneChaosArm {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.armed == nil || self.armed.route != request.Method+" "+request.URL.Path {
		return nil
	}
	arm := self.armed
	self.armed = nil
	return arm
}

// The answer an lb gives when its upstream went away.
func controlPlaneLostAnswer(request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Status:     "502 Bad Gateway",
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader("upstream went away")),
		Request:    request,
	}
}

func (self *controlPlaneApiChaos) hook(request *http.Request, serve func() (*http.Response, error)) (*http.Response, error) {
	arm := self.take(request)
	if arm == nil {
		return serve()
	}
	close(arm.taken)
	switch arm.kind {
	case controlPlaneChaosActed:
		response, err := serve()
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		arm.done <- self.world.T3().RestartApi()
		return controlPlaneLostAnswer(request), nil
	case controlPlaneChaosLost:
		arm.done <- self.world.T3().RestartApi()
		return controlPlaneLostAnswer(request), nil
	default:
		select {
		case <-arm.release:
			arm.done <- nil
			return serve()
		case <-request.Context().Done():
			arm.done <- request.Context().Err()
			return nil, request.Context().Err()
		}
	}
}

// What the chaos case runs with (QD-702).
type controlPlaneChaosSettings struct {
	// The personal network's devices.
	ChaosDeviceCount int
	// The documents the saves under chaos write: grant 1 and a dns rule each, so each differs.
	Document func(index int) string
	// The base case's settings: its team, its grant 1, its retries, bounds and limits.
	Base *controlPlaneSettings
}

func defaultControlPlaneChaosSettings() *controlPlaneChaosSettings {
	base := defaultControlPlaneSettings()
	return &controlPlaneChaosSettings{
		ChaosDeviceCount: 3,
		Document: func(index int) string {
			return worldAclPolicy(base.GrantOne, fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["dns:chaos-%d"], "allow": ["admin"]}`, index))
		},
		Base: base,
	}
}

func TestE2eControlPlaneConcurrencyChaos(t *testing.T) {
	settings := defaultControlPlaneChaosSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		base := settings.Base
		self := newControlPlaneCase(t, base)
		defer self.world.Close()
		world := self.world
		chaos := newControlPlaneApiChaos(world)
		ctx, cancel := world.StepContext(t.Context(), base.PhaseTimeout)
		defer cancel()
		a, err := world.AddNetwork(ctx, "a", settings.ChaosDeviceCount, nil)
		if err != nil {
			world.FailProgress(t, "the personal network", err, nil)
		}
		team, err := world.AddTeamNetwork(ctx, base.Team)
		if err != nil {
			world.FailProgress(t, "the team", err, nil)
		}
		self.team = team
		ownerDevice := team.Owner.Devices()[0]

		// an accept whose answer api lost after it acted: one membership, which the retry finds
		self.chaosAccept(ctx, chaos, "acted-invitee", controlPlaneChaosActed)
		// an accept api lost unseen: no membership until the retry makes one
		self.chaosAccept(ctx, chaos, "lost-invitee", controlPlaneChaosLost)

		// a statement whose answer api lost after it acted, posted again verbatim
		self.chaosStatement(ctx, chaos, ownerDevice)

		// a base whose answer api lost after it acted, and one api lost unseen, both by the network's bootstrapping
		// device: a base another config writer signs is refused by every reader that starts at it, api's own gate after
		// its restart among them (TestE2eControlPlaneConcurrencyColdGate), which would hide what the chaos shows
		self.chaosSave(ctx, chaos, a, a.Clients[0], settings.Document(1), controlPlaneChaosActed)
		self.chaosSave(ctx, chaos, a, a.Clients[0], settings.Document(2), controlPlaneChaosLost)

		// a device restarted while api holds its base's publish
		self.chaosRestartMidSave(ctx, chaos, a, a.Clients[1], settings.Document(3))

		// the inviting device restarted between its invite and its MemberAdmit
		self.chaosRestartMidInvite(ctx, ownerDevice)
	})
}

// An invitee's accept under chaos kind at api (the file's header): exactly one membership of the person's after the
// accept and its retry, which every device of the team then names once an Owner's device admits it.
func (self *controlPlaneCase) chaosAccept(ctx context.Context, chaos *controlPlaneApiChaos, name string, kind int) {
	t := self.t
	team := self.team
	ownerDevice := team.Owner.Devices()[0]
	person, err := self.world.NewPerson(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := team.Invite(ctx, team.Owner, ownerDevice, person.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER)
	if err != nil {
		t.Fatalf("%s's invite: %v", name, err)
	}
	arm := chaos.arm(http.MethodPost+" /network/member-invite-accept", kind)
	if _, err := team.Accept(ctx, person, invite); err == nil {
		t.Errorf("%s's accept answered through the chaos", name)
	}
	if err := <-arm.done; err != nil {
		t.Errorf("api's restart at %s's accept: %v", name, err)
	}
	member, retryErr := team.Accept(ctx, person, invite)
	memberIds, err := worldNetworkMembersOfUser(ctx, team.NetworkId, person.UserId)
	if err != nil {
		t.Errorf("%s's memberships: %v", name, err)
		return
	}
	if len(memberIds) != 1 {
		t.Errorf("%s holds %d memberships of the team after an accept under chaos and its retry (%v)", name, len(memberIds), retryErr)
		return
	}
	switch kind {
	case controlPlaneChaosActed:
		// the first accept spent the invite; the retry finds the membership it made, refused
		if retryErr == nil {
			t.Errorf("%s's retried accept made a membership again after the first one's lost answer", name)
		}
		member = &WorldTeamMember{
			WorldPerson: person,
			MemberId:    sidecar.Id(memberIds[0]),
			Role:        bridge.MemberRole_MEMBER_ROLE_MEMBER,
			Root:        worldTeamCredential(team.NetworkId, team.networkName, person.UserId, memberIds[0]),
		}
	default:
		if retryErr != nil {
			t.Errorf("%s's retried accept, the first lost unseen: %v", name, retryErr)
			return
		}
	}
	if _, err := team.AdmitMember(ctx, ownerDevice, member); err != nil {
		t.Errorf("%s's MemberAdmit after api's restart: %v", name, err)
		return
	}
	for _, device := range team.LiveDevices() {
		if device.Follower().VerifiedState().Member(member.MemberId) == nil {
			t.Errorf("%s's state names no member %s after the chaos", device.Name, name)
		}
	}
	t.Logf("the chaos at %s's accept (kind %d): one membership %s, admitted", name, kind, member.MemberId)
}

// A statement whose answer api lost after appending it: the log holds it once, and the same signed bytes posted again
// add no entry.
func (self *controlPlaneCase) chaosStatement(ctx context.Context, chaos *controlPlaneApiChaos, device *sidecart3.T3Device) {
	t := self.t
	team := self.team
	person, err := self.world.NewPerson(ctx, "statement-invitee")
	if err != nil {
		t.Fatal(err)
	}
	invite, err := team.Invite(ctx, team.Owner, device, person.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER)
	if err != nil {
		t.Fatalf("the statement's invite: %v", err)
	}
	member, err := team.Accept(ctx, person, invite)
	if err != nil {
		t.Fatalf("the statement's accept: %v", err)
	}
	client := device.Follower().Client()
	if err := client.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	before := client.Pin().GetVersion()
	signed, err := controlPlaneSignStatement(team.T3Network, device, bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
		MemberId: member.MemberId.Bytes(),
		Role:     bridge.MemberRole_MEMBER_ROLE_MEMBER,
	}, self.world.T3().Settings().StatementLife)
	if err != nil {
		t.Fatalf("the MemberAdmit: %v", err)
	}
	arm := chaos.arm(http.MethodPost+" /network/config/delta", controlPlaneChaosActed)
	status, _, err := self.world.T3().Api().Call(ctx, http.MethodPost, "/network/config/delta", device.Bearer(), &controller.NetworkConfigDeltaArgs{Signed: signed})
	if err != nil || status != http.StatusBadGateway {
		t.Errorf("the MemberAdmit under chaos answered %d (%v), not the lost answer", status, err)
	}
	if err := <-arm.done; err != nil {
		t.Errorf("api's restart at the MemberAdmit: %v", err)
	}
	status, answer, err := self.world.T3().Api().Call(ctx, http.MethodPost, "/network/config/delta", device.Bearer(), &controller.NetworkConfigDeltaArgs{Signed: signed})
	if err != nil {
		t.Errorf("the MemberAdmit posted again: %v", err)
	}
	againPin := uint64(0)
	if status == http.StatusOK {
		result := &controller.NetworkConfigPinResult{}
		if json.Unmarshal(answer, result) == nil {
			againPin = result.N
		}
	}
	if err := client.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	after := client.Pin().GetVersion()
	if after != before+1 {
		t.Errorf("one MemberAdmit, its answer lost and its bytes posted again, took the log from n %d to %d, not one entry (the repost answered %d %s)", before, after, status, strings.TrimSpace(string(answer)))
	}
	if status == http.StatusOK && againPin != before+1 {
		t.Errorf("the repost of the landed MemberAdmit answered n %d, not its entry's %d", againPin, before+1)
	}
	if err := team.Sync(ctx, after); err != nil {
		t.Errorf("the team at the MemberAdmit: %v", err)
	}
	t.Logf("the chaos at a statement: n %d to %d, the repost answered %d %s", before, after, status, strings.TrimSpace(string(answer)))
}

// device's statement, signed as T3Network.FileStatement signs it (its header at device's verified pin, a fresh nonce,
// not_after life from now), not posted: the signed bytes a test posts and posts again.
func controlPlaneSignStatement(network *sidecart3.T3Network, device *sidecart3.T3Device, signedType bridge.SignedType, body proto.Message, life time.Duration) ([]byte, error) {
	purpose, ok := sidecar.ConfigStatementPurpose(signedType)
	if !ok {
		return nil, fmt.Errorf("%s is no statement of the log", signedType)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := &bridge.StatementHeader{
		NetworkId:  sidecar.Id(network.NetworkId).Bytes(),
		At:         proto.Clone(device.Follower().Client().Pin()).(*bridge.AuthPin),
		Nonce:      nonce,
		NotAfterMs: time.Now().Add(life).UnixMilli(),
	}
	statement := proto.Clone(body)
	message := statement.ProtoReflect()
	field := message.Descriptor().Fields().ByName("header")
	if field == nil || field.Message() == nil || field.Message().FullName() != header.ProtoReflect().Descriptor().FullName() {
		return nil, fmt.Errorf("%s carries no StatementHeader", message.Descriptor().FullName())
	}
	message.Set(field, protoreflect.ValueOfMessage(header.ProtoReflect()))
	statementBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(statement)
	if err != nil {
		return nil, err
	}
	signed, err := device.Sidecar().Sign(purpose, signedType, statementBytes, nil)
	if err != nil {
		return nil, err
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(signed)
}

// device saves source under chaos kind at its base's publish: whatever the save answered, a save of source again where
// the first was not acknowledged, then every device of network converged on one pin, and source planned once more
// changes nothing, so the head holds it, applied once.
func (self *controlPlaneCase) chaosSave(ctx context.Context, chaos *controlPlaneApiChaos, network *WorldNetwork, device *sidecart3.T3Device, source string, kind int) {
	t := self.t
	before, err := controlPlaneConfigVersion(device)
	if err != nil {
		t.Errorf("%s's config version: %v", device.Name, err)
		return
	}
	plan, err := controlPlanePlanAcl(ctx, device, []byte(source), self.settings.Retry)
	if err != nil {
		t.Errorf("%s's preview under chaos: %v", device.Name, err)
		return
	}
	arm := chaos.arm(http.MethodPost+" /network/config/projection", kind)
	_, saveErr := device.Sidecar().SaveAclPolicy(ctx, []byte(source), plan.Digest, sidecar.SidecarActConfirmed)
	select {
	case <-arm.taken:
		if err := <-arm.done; err != nil {
			t.Errorf("api's restart at %s's base: %v", device.Name, err)
		}
	default:
		t.Errorf("%s's save published no base through api (%v)", device.Name, saveErr)
		chaos.disarm()
	}
	if saveErr != nil {
		// not acknowledged: the person saves again, previewing at the head as it now stands
		if _, err := self.world.SaveAclPolicy(ctx, network, device, source); err != nil {
			t.Errorf("%s's save again after the chaos (kind %d, first answered %v): %v", device.Name, kind, saveErr, err)
		}
	}
	self.checkSaved(ctx, network, device, source, before, fmt.Sprintf("the chaos at %s's base (kind %d, first answered %v)", device.Name, kind, saveErr))
}

// Every live device of network on one pin, and source planned again by device changes nothing: its head holds it.
func (self *controlPlaneCase) checkSaved(ctx context.Context, network *WorldNetwork, device *sidecart3.T3Device, source string, before uint64, what string) {
	t := self.t
	if err := self.syncTip(ctx, network.T3Network, device); err != nil {
		t.Errorf("%s: %s's network at its tip: %v", what, network.Name, err)
		return
	}
	if divergent := controlPlaneDivergent(network.LiveDevices()); 0 < len(divergent) {
		t.Errorf("%s: %s's devices did not converge: %s", what, network.Name, strings.Join(divergent, "; "))
	}
	plan, err := controlPlanePlanAcl(ctx, device, []byte(source), self.settings.Retry)
	if err != nil {
		t.Errorf("%s: the document's preview after the chaos: %v", what, err)
		return
	}
	if 0 < len(plan.Preview.RulesAdded) || 0 < len(plan.Preview.RulesRemoved) {
		t.Errorf("%s: the head does not hold the saved document: planning it again adds %d rules and removes %d", what, len(plan.Preview.RulesAdded), len(plan.Preview.RulesRemoved))
	}
	after, _ := controlPlaneConfigVersion(device)
	t.Logf("%s: config version %d to %d", what, before, after)
}

// device's save with its base's publish held at api while device restarts on its store: the held request is given up
// with the old Sidecar, the restarted device follows from its kept anchor, saves again, and the network converges on a
// head holding the document.
func (self *controlPlaneCase) chaosRestartMidSave(ctx context.Context, chaos *controlPlaneApiChaos, network *WorldNetwork, device *sidecart3.T3Device, source string) {
	t := self.t
	before, err := controlPlaneConfigVersion(device)
	if err != nil {
		t.Errorf("%s's config version: %v", device.Name, err)
		return
	}
	plan, err := controlPlanePlanAcl(ctx, device, []byte(source), self.settings.Retry)
	if err != nil {
		t.Errorf("%s's preview before its restart: %v", device.Name, err)
		return
	}
	arm := chaos.arm(http.MethodPost+" /network/config/projection", controlPlaneChaosHeld)
	running := device.Sidecar()
	saved := make(chan error, 1)
	go func() {
		_, err := running.SaveAclPolicy(ctx, []byte(source), plan.Digest, sidecar.SidecarActConfirmed)
		saved <- err
	}()
	select {
	case <-arm.taken:
	case err := <-saved:
		t.Errorf("%s's save ended before its base reached api: %v", device.Name, err)
		return
	case <-ctx.Done():
		t.Errorf("%s's base never reached api: %v", device.Name, ctx.Err())
		return
	}
	restartErr := device.Restart(ctx)
	if restartErr != nil {
		t.Errorf("%s's restart mid-save: %v", device.Name, restartErr)
	}
	// the old Sidecar gave its request up; were it still held, it goes on now
	close(arm.release)
	heldErr := <-arm.done
	saveErr := <-saved
	if restartErr != nil {
		return
	}
	if saveErr != nil {
		if _, err := self.world.SaveAclPolicy(ctx, network, device, source); err != nil {
			t.Errorf("%s's save again after its restart (the first answered %v, its held publish %v): %v", device.Name, saveErr, heldErr, err)
		}
	}
	self.checkSaved(ctx, network, device, source, before, fmt.Sprintf("%s restarted mid-save (the save answered %v, its held publish %v)", device.Name, saveErr, heldErr))
}

// The inviting device restarts on its store between its invite and the invitee's MemberAdmit, which it then signs: the
// membership lands once and every device names it.
func (self *controlPlaneCase) chaosRestartMidInvite(ctx context.Context, device *sidecart3.T3Device) {
	t := self.t
	team := self.team
	person, err := self.world.NewPerson(ctx, "restart-invitee")
	if err != nil {
		t.Fatal(err)
	}
	invite, err := team.Invite(ctx, team.Owner, device, person.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER)
	if err != nil {
		t.Fatalf("the restart's invite: %v", err)
	}
	if err := device.Restart(ctx); err != nil {
		t.Errorf("%s's restart mid-invite: %v", device.Name, err)
		return
	}
	member, err := team.Accept(ctx, person, invite)
	if err != nil {
		t.Errorf("the accept after the inviter's restart: %v", err)
		return
	}
	if _, err := team.AdmitMember(ctx, device, member); err != nil {
		t.Errorf("%s's MemberAdmit after its restart: %v", device.Name, err)
		return
	}
	memberIds, err := worldNetworkMembersOfUser(ctx, team.NetworkId, person.UserId)
	if err != nil || len(memberIds) != 1 {
		t.Errorf("the invitee holds %d memberships after the inviter's restart (%v)", len(memberIds), err)
	}
	for _, other := range team.LiveDevices() {
		if other.Follower().VerifiedState().Member(member.MemberId) == nil {
			t.Errorf("%s's state names no member after the inviter's restart", other.Name)
		}
	}
	t.Logf("the inviter %s restarted mid-invite: the invitee's membership %s admitted by it once", device.Name, member.MemberId)
}

// A reader that starts at a base, as a gate cache does when it loads a network cold (api's after a restart, a new
// block's), holds the base an admitted config writer of a personal network signed: its owner's second device saves a
// document, and a new gate cache then loads the network at that base's pin. As this lands it refuses the base as a fork,
// "base N's signer is no config writer in its own rows" (sidecar's config_replay.go: the state built from the genesis
// and the base's rows does not make the signer a config writer, where every warm reader, having replayed the log from
// the genesis, holds it one), so the cold gate never loads the network, and api's exchange, after api restarts, answers
// that network's every exchange temporarily_unavailable. The failure stands, with the gate's alarm as its evidence.
func TestE2eControlPlaneConcurrencyColdGate(t *testing.T) {
	settings := defaultControlPlaneChaosSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		base := settings.Base
		self := newControlPlaneCase(t, base)
		defer self.world.Close()
		world := self.world
		ctx, cancel := world.StepContext(t.Context(), base.PhaseTimeout)
		defer cancel()
		a, err := world.AddNetwork(ctx, "a", 2, nil)
		if err != nil {
			world.FailProgress(t, "the personal network", err, nil)
		}
		pin, err := world.SaveAclPolicy(ctx, a, a.Clients[1], settings.Document(1))
		if err != nil {
			world.FailProgress(t, "the second device's save", err, nil)
		}
		cache, closeCache, err := world.T3().AddGate("cold")
		if err != nil {
			t.Fatalf("the cold gate: %v", err)
		}
		defer closeCache()
		if err := world.T3().SyncGates(ctx, a.NetworkId, pin.GetVersion()); err != nil {
			snapshot, status := cache.State(sidecar.Id(a.NetworkId))
			held := uint64(0)
			if snapshot != nil {
				held = snapshot.Pin.GetVersion()
			}
			t.Errorf("finding: a gate loading %s cold never holds the base %s signed at n %d: it holds n %d (status %v): %v", a.Name, a.Clients[1].Name, pin.GetVersion(), held, status, err)
		}
	})
}
