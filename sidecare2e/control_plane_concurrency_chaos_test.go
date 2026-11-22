//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The control-plane case's chaos (control_plane_concurrency_test.go's fifth part; the end-to-end plan's
// e2e-control-plane-concurrency): api's host and a member's device restarted mid-save, mid-invite, mid-removal and
// mid-statement, each keeping its stores, on a personal network of ChaosDeviceCount devices and a team of its Owner's
// devices and an Admin's.
//
// api's restart is sidecart3's: its sequencer with its caches, its gate cache and the installs over it dropped and
// built again on the same stores (T3World.RestartApi), at a request a chaos hook in front of api matches
// (controlPlaneApiChaos, on T3Api.SetApiHook):
//
//	acted     api answers the request in full, then restarts, and the answer is lost (502, as an lb whose upstream
//	          went away before the answer reached it): an invite's create and accept, a member's removal, a base's
//	          publish, a statement's delta
//	lost      api restarts with the request unseen, which is lost the same way: an invite's create and accept, a
//	          member's removal, a base's publish
//	held      the request waits at the hook until the test releases it, or until its sender gives it up: a device's
//	          base publish, and a device's own statement, each held while that device restarts
//
// A device's restart is T3Device.Restart: its Sidecar and instance end and a new one starts on the same store and key
// store, keeping its enrollment, keys and anchor, with new key clients and a follower from its kept anchor.
//
// What must hold, after each chaos and its retry where the sender has one: nothing acknowledged is lost, and nothing is
// applied twice. An accept whose answer was lost made one membership, which a retry finds (§7.5: the invite is spent);
// an accept lost unseen made none, and its retry makes one; an invite whose answer was lost is a row nobody holds the
// bearer of, since api keeps only its secret's hash (§7.5), left to expire beside the one the inviter makes again, as
// the invite route says of a lost answer (model's network_invite_model.go), and the person joins once by the one it
// holds; a removal whose answer was lost denied the member once, the retry refused as finding no such member, and one
// lost unseen denies it at the retry, one denial logged either way, and the admin device's MemberRevoke lands once; a
// statement whose answer was lost stands once, and the same signed bytes posted again are refused as a replay or
// answered at the entry already holding them, never a second entry; a statement a device's own act signed, its post
// held while the device restarts, lands once from the act's pending records (§5.9: a signed statement is kept before it
// is posted, and the device's next generation posts the same bytes again), whether or not the held post is then served;
// a base whose answer was lost, or whose publish was lost unseen or given up by a restarting device, leaves every
// device of the network converged on one pin whose head holds the saved document, the document saved once more planning
// no change; and a restarted inviting device still signs, its kept identity's MemberAdmit landing once.
//
// The personal network's bases are its later-admitted config writers', the bootstrapping device's second and third
// devices', and api restarts after each: every restart after the first base loads that network cold at a base a writer
// other than the bootstrapping device signed, as its first exchange or statement after the restart asks, and after the
// last restart every gate, api's own among them, holds each network at its tip. As the case landed (2026-11-22) a cold
// reader refused such a base as a fork (finding C, TestE2eControlPlaneConcurrencyColdGate), so the steps ran in an
// order that kept every api restart from following one; config-replay-base-signer (sidecar 9ac79c88 to f5892c63,
// QD-1192) fixed it, and e2e-control-plane-verify dropped that order. At the end api's write budget reconciles exactly
// with the objects it wrote across every restart (control_plane_concurrency_harness_test.go). Each step is bounded by
// the case's PhaseTimeout.

package sidecare2e

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
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
	// How long a statement a restarted device's pending records hold may take to land, its new generation posting it
	// as it starts, and how often the log is read meanwhile: a generation's start has no hook a test reads.
	PendingSettleTimeout  time.Duration
	PendingSettleInterval time.Duration
	// The base case's settings: its team, its grant 1, its retries, bounds, limits and ledgers.
	Base *controlPlaneSettings
}

func defaultControlPlaneChaosSettings() *controlPlaneChaosSettings {
	base := defaultControlPlaneSettings()
	return &controlPlaneChaosSettings{
		ChaosDeviceCount: 3,
		Document: func(index int) string {
			return worldAclPolicy(base.GrantOne, fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["dns:chaos-%d"], "allow": ["admin"]}`, index))
		},
		PendingSettleTimeout:  60 * time.Second,
		PendingSettleInterval: 200 * time.Millisecond,
		Base:                  base,
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
		// each step its own bound, so a stall in one leaves the next and the ledgers theirs
		step := func(run func(ctx context.Context)) {
			ctx, cancel := world.StepContext(t.Context(), base.PhaseTimeout)
			defer cancel()
			run(ctx)
		}
		var a *WorldNetwork
		var ownerDevice *sidecart3.T3Device
		step(func(ctx context.Context) {
			var err error
			a, err = world.AddNetwork(ctx, "a", settings.ChaosDeviceCount, nil)
			if err != nil {
				world.FailProgress(t, "the personal network", err, nil)
			}
			team, err := world.AddTeamNetwork(ctx, base.Team)
			if err != nil {
				world.FailProgress(t, "the team", err, nil)
			}
			self.team = team
			ownerDevice = team.Owner.Devices()[0]
		})

		// a later-admitted config writer restarted while api holds its base's publish: its base, saved again from its
		// kept anchor, is the first of a's a writer other than the bootstrapping device signs, and each api restart
		// below follows it
		step(func(ctx context.Context) {
			self.chaosRestartMidSave(ctx, chaos, a, a.Clients[1], settings.Document(1))
		})

		// an accept whose answer api lost after it acted: one membership, which the retry finds; and an accept api
		// lost unseen: no membership until the retry makes one
		step(func(ctx context.Context) { self.chaosAccept(ctx, chaos, "acted-invitee", controlPlaneChaosActed) })
		step(func(ctx context.Context) { self.chaosAccept(ctx, chaos, "lost-invitee", controlPlaneChaosLost) })

		// a statement whose answer api lost after it acted, posted again verbatim
		step(func(ctx context.Context) { self.chaosStatement(ctx, chaos, ownerDevice) })

		// a base whose answer api lost after it acted, and one api lost unseen, each a later-admitted writer's, each
		// previewed first through api's exchange as api's gate loads a cold after its last restart
		step(func(ctx context.Context) {
			self.chaosSave(ctx, chaos, a, a.Clients[2], settings.Document(2), controlPlaneChaosActed)
		})
		step(func(ctx context.Context) {
			self.chaosSave(ctx, chaos, a, a.Clients[1], settings.Document(3), controlPlaneChaosLost)
		})

		// the network's bootstrapping device restarted while api holds its own act's SidecarAdmit
		step(func(ctx context.Context) { self.chaosRestartMidStatement(ctx, chaos, a, a.Clients[0], settings) })

		// an invite's create whose answer api lost after it acted, and one api lost unseen
		step(func(ctx context.Context) { self.chaosInviteCreate(ctx, chaos, "acted-invite", controlPlaneChaosActed) })
		step(func(ctx context.Context) { self.chaosInviteCreate(ctx, chaos, "lost-invite", controlPlaneChaosLost) })

		// a member's removal whose answer api lost after it acted, and one api lost unseen
		step(func(ctx context.Context) { self.chaosRemove(ctx, chaos, "acted-removal", controlPlaneChaosActed) })
		step(func(ctx context.Context) { self.chaosRemove(ctx, chaos, "lost-removal", controlPlaneChaosLost) })

		// the inviting device restarted between its invite and its MemberAdmit
		step(func(ctx context.Context) { self.chaosRestartMidInvite(ctx, ownerDevice) })

		// after api's last restart, every gate, api's own among them, holds each network at its tip, a's newest base a
		// later writer's, and a's devices take the config's read at api's exchange
		step(func(ctx context.Context) { self.chaosConverged(ctx, []*WorldNetwork{a, self.team.WorldNetwork}) })

		// api's write budget across every restart, exactly
		step(func(ctx context.Context) { controlPlaneCheckApiBudget(t, ctx, world, base.Ledgers) })
	})
}

// A person joins the team: invited by the first Owner's device, accepted with their own sign-in and admitted by
// MemberAdmit, with no device of their own.
func (self *controlPlaneCase) joinTeam(ctx context.Context, name string) (*WorldTeamMember, error) {
	team := self.team
	ownerDevice := team.Owner.Devices()[0]
	person, err := self.world.NewPerson(ctx, name)
	if err != nil {
		return nil, err
	}
	invite, err := team.Invite(ctx, team.Owner, ownerDevice, person.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER)
	if err != nil {
		return nil, fmt.Errorf("%s's invite: %w", name, err)
	}
	member, err := team.Accept(ctx, person, invite)
	if err != nil {
		return nil, fmt.Errorf("%s's accept: %w", name, err)
	}
	if _, err := team.AdmitMember(ctx, ownerDevice, member); err != nil {
		return nil, fmt.Errorf("%s's MemberAdmit: %w", name, err)
	}
	return member, nil
}

// An invite's create under chaos kind at api (the file's header): the inviter, its answer lost, invites again, and the
// person accepts the invite it holds and joins once. acted leaves api's first invite unspent beside it, a row nobody
// holds the bearer of, left to expire; lost leaves none but the retry's, which the accept spends.
func (self *controlPlaneCase) chaosInviteCreate(ctx context.Context, chaos *controlPlaneApiChaos, name string, kind int) {
	t := self.t
	team := self.team
	ownerDevice := team.Owner.Devices()[0]
	person, err := self.world.NewPerson(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	// the invites api made under the chaos: acted's one, lost's none
	made := 0
	if kind == controlPlaneChaosActed {
		made = 1
	}
	arm := chaos.arm(http.MethodPost+" /network/member-invite", kind)
	if _, err := team.Invite(ctx, team.Owner, ownerDevice, person.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER); err == nil {
		t.Errorf("%s's invite answered through the chaos", name)
	}
	if err := <-arm.done; err != nil {
		t.Errorf("api's restart at %s's invite: %v", name, err)
	}
	inviteIds, err := worldNetworkInviteIds(ctx, team.NetworkId, person.Email)
	if err != nil {
		t.Errorf("%s's invites: %v", name, err)
		return
	}
	if len(inviteIds) != made {
		t.Errorf("api holds %d invites of %s after an invite under chaos (kind %d), not %d", len(inviteIds), name, kind, made)
	}
	invite, err := team.Invite(ctx, team.Owner, ownerDevice, person.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER)
	if err != nil {
		t.Errorf("%s's invite again after api's restart: %v", name, err)
		return
	}
	if again, err := worldNetworkInviteIds(ctx, team.NetworkId, person.Email); err != nil || len(again) != made+1 || !slices.Contains(again, server.Id(invite.InviteId)) {
		t.Errorf("api holds invites %v of %s after the invite again, not the %d before and the retry's %s (%v)", again, name, made, invite.InviteId, err)
	}
	member, err := team.Accept(ctx, person, invite)
	if err != nil {
		t.Errorf("%s's accept of the invite it holds: %v", name, err)
		return
	}
	if memberIds, err := worldNetworkMembersOfUser(ctx, team.NetworkId, person.UserId); err != nil || len(memberIds) != 1 {
		t.Errorf("%s holds %d memberships of the team after one accept (%v)", name, len(memberIds), err)
	}
	// the accept spent its own invite; acted's lost one stays unspent, its bearer held by nobody
	left, err := worldNetworkInviteIds(ctx, team.NetworkId, person.Email)
	if err != nil || len(left) != made || slices.Contains(left, server.Id(invite.InviteId)) {
		t.Errorf("api holds invites %v of %s after the accept, not the %d whose answer was lost (%v)", left, name, made, err)
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
	t.Logf("the chaos at %s's invite (kind %d): %d invites left unspent, one membership %s, admitted", name, kind, len(left), member.MemberId)
}

// A member's removal under chaos kind at api (the file's header): acted denied the member before its answer was lost,
// so the remover's retry is refused as finding no such member; lost denied nothing, so the retry denies it. Either way
// api ran one denial, its row gone and one member.remove logged, and the admin device's MemberRevoke lands once, after
// which no device names the member.
func (self *controlPlaneCase) chaosRemove(ctx context.Context, chaos *controlPlaneApiChaos, name string, kind int) {
	t := self.t
	team := self.team
	ownerDevice := team.Owner.Devices()[0]
	member, err := self.joinTeam(ctx, name)
	if err != nil {
		t.Errorf("the member the removal takes: %v", err)
		return
	}
	held := func() (bool, error) {
		roles, err := worldNetworkMemberRoles(ctx, team.NetworkId)
		_, ok := roles[server.Id(member.MemberId)]
		return ok, err
	}
	arm := chaos.arm(http.MethodPost+" /network/member-remove", kind)
	if err := team.DenyMember(ctx, team.Owner, member); err == nil {
		t.Errorf("%s's removal answered through the chaos", name)
	}
	if err := <-arm.done; err != nil {
		t.Errorf("api's restart at %s's removal: %v", name, err)
	}
	if stands, err := held(); err != nil || stands != (kind == controlPlaneChaosLost) {
		t.Errorf("api holds %s's membership: %t after its removal under chaos (kind %d) (%v)", name, stands, kind, err)
	}
	retryErr := team.DenyMember(ctx, team.Owner, member)
	switch kind {
	case controlPlaneChaosActed:
		// the first removal denied the member; the retry finds none
		if retryErr == nil {
			t.Errorf("%s's removal again denied a member the first removal, its answer lost, had denied", name)
		}
	default:
		if retryErr != nil {
			t.Errorf("%s's removal again, the first lost unseen: %v", name, retryErr)
		}
	}
	if stands, err := held(); err != nil || stands {
		t.Errorf("api still holds %s's membership after its removal and the retry (%v)", name, err)
	}
	if count, err := worldNetworkAdminEventCount(ctx, team.NetworkId, model.AdminEventActionMemberRemove, server.Id(member.MemberId)); err != nil || count != 1 {
		t.Errorf("api logged %d removals of %s after one removal under chaos and its retry, not one (%v)", count, name, err)
	}
	client := ownerDevice.Follower().Client()
	if err := client.CatchUp(ctx); err != nil {
		t.Errorf("%s's catch-up: %v", ownerDevice.Name, err)
		return
	}
	before := client.Pin().GetVersion()
	pin, err := team.RevokeMember(ctx, ownerDevice, member)
	if err != nil {
		t.Errorf("%s's MemberRevoke after its removal under chaos: %v", name, err)
		return
	}
	if pin.GetVersion() != before+1 {
		t.Errorf("%s's MemberRevoke landed at n %d, the log at n %d before it", name, pin.GetVersion(), before)
	}
	for _, device := range team.LiveDevices() {
		if device.Follower().VerifiedState().Member(member.MemberId) != nil {
			t.Errorf("%s's state still names %s after its MemberRevoke", device.Name, name)
		}
	}
	t.Logf("the chaos at %s's removal (kind %d): one denial, the retry answering %v, its MemberRevoke at n %d", name, kind, retryErr, pin.GetVersion())
}

// A new device of network's owner admitted by admitter, a device of the same member, through its own act (OpenAdmission
// and DecideApproval, as `ur approve` does), the act's SidecarAdmit held at api while admitter restarts on its store
// (the file's header): the restarted device's new generation posts the same signed bytes from its pending records,
// which land while the first post is still held; then the held post is released, and the log holds the admission once
// whether api serves it, answering it at the entry holding it, or its sender gave it up.
func (self *controlPlaneCase) chaosRestartMidStatement(ctx context.Context, chaos *controlPlaneApiChaos, network *WorldNetwork, admitter *sidecart3.T3Device, settings *controlPlaneChaosSettings) {
	t := self.t
	devices, err := network.AddSidecarsWith(ctx, &sidecart3.T3EnrollArgs{Count: 1, Name: network.Name + "-mid"})
	if err != nil {
		t.Errorf("the device the admission takes: %v", err)
		return
	}
	device := devices[0]
	identity := device.Sidecar().Identity()
	client := admitter.Follower().Client()
	if err := client.CatchUp(ctx); err != nil {
		t.Errorf("%s's catch-up: %v", admitter.Name, err)
		return
	}
	before := client.Pin().GetVersion()
	running := admitter.Sidecar()
	approvalId, err := running.OpenAdmission(&sidecar.SidecarAdmissionRequest{
		SidecarId:         identity.SidecarId,
		MemberId:          network.MemberId(),
		IdentityPublicKey: identity.IdentityPublicKey,
	})
	if err != nil {
		t.Errorf("%s's admission item on %s: %v", device.Name, admitter.Name, err)
		return
	}
	arm := chaos.arm(http.MethodPost+" /network/config/delta", controlPlaneChaosHeld)
	decided := make(chan error, 1)
	go func() {
		decided <- running.DecideApproval(ctx, approvalId, sidecar.SidecarDecisionApprove, sidecar.SidecarActConfirmed, nil)
	}()
	select {
	case <-arm.taken:
	case err := <-decided:
		chaos.disarm()
		t.Errorf("%s's admission of %s ended before its statement reached api: %v", admitter.Name, device.Name, err)
		return
	case <-ctx.Done():
		chaos.disarm()
		t.Errorf("%s's admission of %s never reached api: %v", admitter.Name, device.Name, ctx.Err())
		return
	}
	restartErr := admitter.Restart(ctx)
	// the restarted device's pending post lands while the first post is held, unless its sender gave it up
	var landedErr error
	if restartErr == nil {
		landedErr = self.awaitAdmitted(ctx, admitter, identity.SidecarId, settings)
	}
	close(arm.release)
	heldErr := <-arm.done
	decideErr := <-decided
	if restartErr != nil {
		t.Errorf("%s's restart mid-statement: %v", admitter.Name, restartErr)
		return
	}
	if landedErr != nil {
		t.Errorf("%s's admission of %s, its act's post held while %s restarted (the act answered %v, its held post %v), never landed from the pending records: %v", admitter.Name, device.Name, admitter.Name, decideErr, heldErr, landedErr)
		return
	}
	client = admitter.Follower().Client()
	if err := client.CatchUp(ctx); err != nil {
		t.Errorf("%s's catch-up after the held post's end: %v", admitter.Name, err)
		return
	}
	after := client.Pin().GetVersion()
	if after != before+1 {
		t.Errorf("one admission, its post held while %s restarted (the act answered %v, its held post %v), took %s's log from n %d to %d, not one entry", admitter.Name, decideErr, heldErr, network.Name, before, after)
	}
	if err := network.Follow(ctx, devices); err != nil {
		t.Errorf("%s's follow after its admission under chaos: %v", device.Name, err)
		return
	}
	if divergent := controlPlaneDivergent(network.LiveDevices()); 0 < len(divergent) {
		t.Errorf("%s's devices did not converge after the restart mid-statement: %s", network.Name, strings.Join(divergent, "; "))
	}
	t.Logf("%s restarted mid-statement (the act answered %v, its held post %v): %s admitted once, n %d to %d", admitter.Name, decideErr, heldErr, device.Name, before, after)
}

// Waits until device's follower, caught up to the log's tip, holds sidecarId admitted, within PendingSettleTimeout.
func (self *controlPlaneCase) awaitAdmitted(ctx context.Context, device *sidecart3.T3Device, sidecarId sidecar.Id, settings *controlPlaneChaosSettings) error {
	ctx, cancel := context.WithTimeout(ctx, settings.PendingSettleTimeout)
	defer cancel()
	for {
		client := device.Follower().Client()
		if client == nil {
			return fmt.Errorf("%s's follower no longer stands", device.Name)
		}
		err := client.CatchUp(ctx)
		if err == nil && device.Follower().VerifiedState().Sidecar(sidecarId) != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the log at n %d admits no %s: %v (%w)", client.Pin().GetVersion(), sidecarId, err, ctx.Err())
		case <-time.After(settings.PendingSettleInterval):
		}
	}
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

// After api's last restart: every gate of the world, api's own among them, holds each of networks at its tip, every live
// device of each on one pin, and each takes the config's read through api's exchange on its own token.
func (self *controlPlaneCase) chaosConverged(ctx context.Context, networks []*WorldNetwork) {
	t := self.t
	for _, network := range networks {
		if err := self.syncTip(ctx, network.T3Network, network.Owner()); err != nil {
			t.Errorf("after api's last restart, %s never stood at its tip at every gate: %v", network.Name, err)
			continue
		}
		if divergent := controlPlaneDivergent(network.LiveDevices()); 0 < len(divergent) {
			t.Errorf("after api's last restart, %s's devices did not converge: %s", network.Name, strings.Join(divergent, "; "))
		}
		configRepoId, _, err := controlPlaneNetworkRepos(network.Owner())
		if err != nil {
			t.Errorf("%s's config repo: %v", network.Name, err)
			continue
		}
		configRead := []string{controlPlaneRepoScope(configRepoId, "r")}
		for _, device := range network.LiveDevices() {
			if granted, err := controlPlaneGrantedScopes(ctx, self.world, device, WorldBlockEvs, configRead); err != nil {
				t.Errorf("after api's last restart, %s's exchange for %s's config read: %v", device.Name, network.Name, err)
			} else if !granted[configRead[0]] {
				t.Errorf("after api's last restart, %s's exchange granted %v, not %s's config read", device.Name, granted, network.Name)
			}
		}
		t.Logf("after api's last restart, %s's %d devices stand at its tip at every gate and take its config's read", network.Name, len(network.LiveDevices()))
	}
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

// Readers that start at a base hold the base a later-admitted config writer of a personal network signed: its owner's
// second device saves a document; then a new gate cache loads the network cold at that base's pin, as api's does after
// a restart and a new block's at its start; a new device of the owner's is admitted and follows to the tip; and api,
// restarted, holds the network at its gate and grants each device the config's read at its exchange. As the case
// landed (2026-11-22) a cold reader refused such a base as a fork, "base N's signer is no config writer in its own
// rows" (finding C: the state built from the genesis and the base's rows did not make the signer a config writer, where
// every warm reader, having replayed the log from the genesis, held it one), so the cold gate never loaded the network
// and api's exchange answered that network's every exchange temporarily_unavailable after a restart.
// config-replay-base-signer (sidecar 9ac79c88 to f5892c63, QD-1192) judges a base's signer at S(b-1), as a reader
// present at b-1 does, and anchors the head there; this case holds every such reader to it.
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

		// a new gate cache, which loads a cold at the base
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
			t.Errorf("a gate loading %s cold never holds the base %s signed at n %d: it holds n %d (status %v): %v", a.Name, a.Clients[1].Name, pin.GetVersion(), held, status, err)
		}

		// a new device of the owner's, admitted past the base, which follows from its anchor to the tip
		devices, err := a.AddSidecars(ctx, 1, nil)
		if err != nil {
			world.FailProgress(t, "the new device", err, nil)
		}
		if err := a.Admit(ctx, devices); err != nil {
			t.Errorf("%s, admitted past the base %s signed at n %d, never followed %s's log: %v", devices[0].Name, a.Clients[1].Name, pin.GetVersion(), a.Name, err)
		}

		// api restarted: its gate holds a at its tip, and each device takes the config's read at its exchange
		if err := world.T3().RestartApi(); err != nil {
			t.Fatalf("api's restart: %v", err)
		}
		if err := self.syncTip(ctx, a.T3Network, a.Owner()); err != nil {
			t.Errorf("after api's restart, %s never stood at its tip past the base %s signed at every gate: %v", a.Name, a.Clients[1].Name, err)
		}
		configRepoId, _, err := controlPlaneNetworkRepos(a.Owner())
		if err != nil {
			t.Fatalf("%s's config repo: %v", a.Name, err)
		}
		configRead := []string{controlPlaneRepoScope(configRepoId, "r")}
		for _, device := range a.LiveDevices() {
			if granted, err := controlPlaneGrantedScopes(ctx, world, device, WorldBlockEvs, configRead); err != nil {
				t.Errorf("after api's restart, %s's exchange for %s's config read: %v", device.Name, a.Name, err)
			} else if !granted[configRead[0]] {
				t.Errorf("after api's restart, %s's exchange granted %v, not %s's config read", device.Name, granted, a.Name)
			}
		}
		t.Logf("%s's base at n %d, %s's: held cold by a new gate, a new device and api's gate after its restart", a.Name, pin.GetVersion(), a.Clients[1].Name)
	})
}
