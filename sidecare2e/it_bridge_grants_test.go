//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The it-bridge case's route grants (it_bridge_test.go's TestE2eItBridgeRouteGrants): §17's third way a subnet joins
// a client's reach and leaves it, beside a device's route and its withdrawal. §17.7 has §8's route grants decide who
// uses a route; §8.6's requested grant is one a requests rule takes, which an approver decides and its requester
// activates by a GrantActivate (§8.8), its prefixes and ports sealed in its action's sealed_detail (owner decision
// 220), and which ends at its expiry or by a GrantRevoke (§17.8's Disposition).
//
//	request   every client the grant matrix denies a subnet asks a route grant on that subnet through the device's
//	          real grant filer (sidecar.AclGrantFiler.Action, as bridge_forward builds its version-4 action), its clear
//	          grant_dst the subnet's device and its detail the subnet's prefix, under the document's one requests rule,
//	          which names those clients and devices with autogroup:self as approver; §16.6's stock gate on the client
//	          files the request and answers it pending
//	approve   the owner's device, an approval-capable device of the clients' member, signs the device decision
//	          `ur approve` signs once its person confirms (ApprovalApprover.Decide's body), over the request as the
//	          client's ledger stores it, after opening its sealed detail as Decide does
//	spend     the client's gate takes that decision (Authorize's decision argument, as a decision a hint delivers is
//	          taken) and spends it: its grant component files the GrantActivate, which api's spend check and every
//	          follower's (sidecart3's FollowerSpendCheck, as ur daemon's) judge
//	end       the first grant asks RouteGrantLifetime and ends at its expiry; the rest are revoked by their subject
//	          (AclGrantFiler.Revoke, bridge_close's GrantRevoke)
//
// What stands in, and why: the request's round trip through the member's vault (§16.8's Approvals bullet). The world
// creates no vault, so the client's router keeps the request pending in its ledger with a warning, as a network with
// no route does, and the decision reaches the client as a decision argument rather than through the log. Everything
// that judges the grant is the product's: the requester's gate and spend, api's and every reader's spend check, the
// fold, each device's compile and the forwarding role's enforcement.
//
// While a grant is in force, its client must reach its subnet by TCP and UDP, and the compiles must say why if not:
// the client's table needs an entry of the subnet's prefix linked to the subnet's device, and that device's rows a
// route row of the client's under the grant (rule 6). After a grant ends, past LeaveBound, nothing reaches the subnet
// from the client: the oracle (ItBridgeWorld.Check) holds every answer and arrival to the document's grants and the
// grants' timelines.

package sidecare2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// One route grant of the case (the file's header): a client's on a subnet the document denies it.
type itBridgeRouteGrant struct {
	name   string
	client *itBridgeDevice
	subnet *itBridgeTarget
	// whether it ends at its expiry, lifetime past its request, rather than by a GrantRevoke
	expires  bool
	lifetime time.Duration
	// its one incarnation: filed when the request began, landed when the spend returned, at the position the owner's
	// follower first held it, adopted once every bridge had; its withdrawal the revoke's, or its expiry for both times
	timeline *itBridgeTimeline

	stateLock sync.Mutex
	grantId   []byte
}

// The grant's id, nil before it activated.
func (self *itBridgeRouteGrant) GrantId() []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return bytes.Clone(self.grantId)
}

// Whether the grant was in force at at: adopted by every bridge before it, and not yet withdrawn.
func (self *itBridgeRouteGrant) inForce(at time.Time) bool {
	incarnation := self.timeline.newest()
	if incarnation == nil {
		return false
	}
	in := false
	self.timeline.update(func() {
		in = !incarnation.AdoptedAt.IsZero() && !at.Before(incarnation.AdoptedAt) &&
			(incarnation.WithdrawAt.IsZero() || at.Before(incarnation.WithdrawAt))
	})
	return in
}

// The case's route grants (the file's header): one for every client the grant matrix denies a subnet that nests in no
// campus, the first expiring and the rest revoked.
func (self *ItBridgeWorld) newRouteGrants() []*itBridgeRouteGrant {
	grants := []*itBridgeRouteGrant{}
	for clientIndex, client := range self.clients {
		for _, subnet := range self.subnets {
			if self.routesSubnet(clientIndex, subnet) || (self.campus != nil && self.campus.prefix.Overlaps(subnet.prefix)) {
				continue
			}
			grant := &itBridgeRouteGrant{
				name:     fmt.Sprintf("%s's grant on %s", client.name, subnet.name),
				client:   client,
				subnet:   subnet,
				expires:  len(grants) == 0,
				timeline: &itBridgeTimeline{},
			}
			if grant.expires {
				grant.lifetime = self.settings.RouteGrantLifetime
			}
			grants = append(grants, grant)
		}
	}
	return grants
}

// The document's requests rule for the route grants (the file's header), nil with none.
func (self *ItBridgeWorld) routeGrantRequests(selector func(device *itBridgeDevice) string) []map[string]any {
	if len(self.routeGrants) == 0 {
		return nil
	}
	src, dst := []string{}, []string{}
	for _, grant := range self.routeGrants {
		if client := selector(grant.client); !slices.Contains(src, client) {
			src = append(src, client)
		}
		if router := selector(grant.subnet.device); !slices.Contains(dst, router) {
			dst = append(dst, router)
		}
	}
	return []map[string]any{{"src": src, "dst": dst, "allow": []string{"route"}, "approver": []string{"autogroup:self"}}}
}

// Gives each client that asks a route grant what ur daemon composes for a grant (cli's bridge_filing_config.go): a grant
// filer over its follower's engine, its Lookup registered for bridge_forward, and §16.6's stock gate over its follower.
func (self *ItBridgeWorld) installGrantGates(ctx context.Context) error {
	for _, client := range self.clients {
		if !slices.ContainsFunc(self.routeGrants, func(grant *itBridgeRouteGrant) bool { return grant.client == client }) {
			continue
		}
		client.grants = sidecar.NewAclGrantFiler(client.t3.Sidecar(), client.t3.Follower().AclEngine())
		self.world.T3().OnClose(client.t3.Sidecar().RegisterEffectLookup(sidecar.SidecarActionForward, client.grants.Lookup))
		if _, err := self.world.InstallApprovalGate(ctx, client.t3); err != nil {
			return err
		}
	}
	return nil
}

// Activates grant (the file's header): asked through its client's gate, approved by the owner's device, spent by the
// client's gate, then waited for until the owner's follower holds it and every bridge adopted it.
func (self *ItBridgeWorld) ActivateRouteGrant(ctx context.Context, grant *itBridgeRouteGrant) error {
	client := grant.client
	running := client.t3.Sidecar()
	identity := running.Identity()
	if identity == nil || client.grants == nil {
		return fmt.Errorf("%s asks no grant", client.name)
	}
	incarnation := grant.timeline.begin()
	now := time.Now()
	expiresMs := int64(0)
	if grant.expires {
		expiresMs = now.Add(grant.lifetime).UnixMilli()
	}
	prefix := grant.subnet.prefix.String()
	stored, err := client.grants.Action(ctx, &protocol.CanonicalAction{Kind: uint32(sidecar.SidecarActionForward)}, &sidecar.AclGrantRequest{
		Subject:    identity.SidecarId,
		GrantDst:   []string{"sidecar:" + hex.EncodeToString(grant.subnet.device.SidecarId().Bytes())},
		GrantAllow: []string{"route"},
		ExpiresMs:  expiresMs,
		Detail:     &protocol.CanonicalActionDetail{Cidrs: []string{prefix}, GrantDst: []string{"ip:" + prefix}},
	}, now)
	if err != nil {
		return fmt.Errorf("%s's action: %w", grant.name, err)
	}
	newAction := func() (*sidecar.SidecarAction, error) {
		action, err := sidecar.NewSidecarAction(stored, sidecar.SidecarRiskNormal, sidecar.SidecarRiskNormal, nil, nil)
		if err != nil {
			return nil, err
		}
		action.Effect = client.grants.Effect(stored)
		return action, nil
	}
	action, err := newAction()
	if err != nil {
		return err
	}
	err = running.Authorize(ctx, action, nil)
	var pending *sidecar.ApprovalPendingError
	if !errors.As(err, &pending) {
		return fmt.Errorf("%s: the gate answered %v, not a request pending a decision", grant.name, err)
	}
	ledger, err := running.ApprovalLedger()
	if err != nil {
		return err
	}
	approval, err := ledger.State(pending.ApprovalId)
	if err != nil || approval.Request == nil {
		return fmt.Errorf("%s: the client's ledger holds no request %s: %v", grant.name, pending.ApprovalId, err)
	}
	decision, err := self.approve(self.owner, approval.Request)
	if err != nil {
		return fmt.Errorf("%s's approval: %w", grant.name, err)
	}
	action, err = newAction()
	if err != nil {
		return err
	}
	if err := running.Authorize(ctx, action, decision); err != nil {
		return fmt.Errorf("%s's spend: %w", grant.name, err)
	}
	if action.Outcome == nil || action.Outcome.State != sidecar.ApprovalOutcomeDone || len(action.Outcome.EffectRef) == 0 {
		return fmt.Errorf("%s's spend answered %+v", grant.name, action.Outcome)
	}
	grantId := bytes.Clone(action.Outcome.EffectRef)
	landedAt := time.Now()
	func() {
		grant.stateLock.Lock()
		defer grant.stateLock.Unlock()
		grant.grantId = grantId
	}()
	pin, err := self.awaitRouteGrant(ctx, grant, true)
	if err != nil {
		return err
	}
	grant.timeline.update(func() {
		incarnation.RuleId, incarnation.LandedAt, incarnation.Pin = grantId, landedAt, pin
		if expiresMillis := stored.Action.GetExpiresMillis(); grant.expires && 0 < expiresMillis {
			// an expiry withdraws it at a time known now, with no entry of its own
			incarnation.WithdrawAt = time.UnixMilli(expiresMillis)
			incarnation.WithdrawnAt = incarnation.WithdrawAt
		}
	})
	if err := self.AwaitAdopted(ctx, pin); err != nil {
		return fmt.Errorf("%s at %d: %w", grant.name, pin, err)
	}
	grant.timeline.update(func() {
		incarnation.AdoptedAt = time.Now()
	})
	return nil
}

// The device decision `ur approve` signs on request once its person confirms (ApprovalApprover.Decide): approve, at
// the count the request names, over the request's stored bytes and its action's, by approver's identity key, its
// member the approver and its pin the approver's state. Like Decide it signs only what the approver's device opens and
// only as an approval-capable device of the member it names.
func (self *ItBridgeWorld) approve(approver *itBridgeDevice, requestBytes []byte) (*bridge.Signed, error) {
	request, err := sidecar.ParseStoredApprovalRequest(requestBytes)
	if err != nil {
		return nil, err
	}
	running := approver.t3.Sidecar()
	if _, err := running.OpenActionDetail(request.Action); err != nil {
		return nil, fmt.Errorf("%s opens no detail of the action it would approve: %w", approver.name, err)
	}
	follower := approver.t3.Follower()
	memberId := sidecar.Id(self.network.MemberId())
	acl := follower.AclEngine().State()
	if acl == nil || !acl.ApprovalCapable(approver.SidecarId()) || !acl.ConsentedFor(approver.SidecarId(), memberId) {
		return nil, fmt.Errorf("%s is no approval-capable device of the network's member", approver.name)
	}
	pin := follower.Client().Pin()
	uses, named := request.Uses()
	if !named {
		uses = 1
	}
	requestSha256 := request.Sha256()
	payloadSha256 := request.Action.PayloadSha256()
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.ApprovalDecision{
		ApprovalId:         bytes.Clone(request.Request.GetApprovalId()),
		RequestSha256:      requestSha256[:],
		PayloadSha256:      payloadSha256[:],
		Decision:           uint32(protocol.ApprovalVerdict_APPROVAL_VERDICT_APPROVE),
		Uses:               uses,
		NotAfterMillis:     request.Request.GetNotAfterMillis(),
		ApproverMemberId:   memberId.Bytes(),
		Channel:            uint32(protocol.ApprovalChannel_APPROVAL_CHANNEL_DEVICE),
		RequesterSidecarId: bytes.Clone(request.Request.GetRequesterSidecarId()),
		AuthPin:            &bridge.AuthPin{Version: pin.GetVersion(), Chain: bytes.Clone(pin.GetChain())},
		Version:            1,
	})
	if err != nil {
		return nil, err
	}
	return running.Sign(sidecar.ApprovalPurpose, bridge.SignedType_SIGNED_APPROVAL_DECISION, body, nil)
}

// Waits until the owner's follower holds grant as a route rule of its client's through its subnet's device, or with
// held false holds it no more, and answers the follower's position then, at or past the entry that did it.
func (self *ItBridgeWorld) awaitRouteGrant(ctx context.Context, grant *itBridgeRouteGrant, held bool) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, self.settings.FilingTimeout)
	defer cancel()
	grantId := grant.GrantId()
	follower := self.owner.t3.Follower()
	for {
		pin := follower.Client().Pin().GetVersion()
		if self.routeGrantHeld(self.owner, grant, grantId) == held {
			return pin, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("%s: the owner's follower never came to hold it %t, at %d: %w", grant.name, held, pin, ctx.Err())
		case <-time.After(self.settings.AdoptPollInterval):
		}
	}
}

// Whether device's verified state holds grantId as a route rule of grant's client through its subnet's device.
func (self *ItBridgeWorld) routeGrantHeld(device *itBridgeDevice, grant *itBridgeRouteGrant, grantId []byte) bool {
	engine := device.t3.Follower().AclEngine()
	if engine == nil || engine.State() == nil {
		return false
	}
	rules := engine.State().RouteRules(sidecar.AclSidecarSubject(grant.client.SidecarId()), sidecar.AclSidecarSubject(grant.subnet.device.SidecarId()), time.Now())
	return slices.ContainsFunc(rules, func(rule sidecar.AclReachRule) bool { return bytes.Equal(rule.GrantId, grantId) })
}

// Revokes grant by its subject (bridge_close's GrantRevoke), waited for until the owner's follower holds it no more and
// every bridge adopted that.
func (self *ItBridgeWorld) RevokeRouteGrant(ctx context.Context, grant *itBridgeRouteGrant) error {
	incarnation := grant.timeline.newest()
	grantId := grant.GrantId()
	if incarnation == nil || len(grantId) == 0 {
		return fmt.Errorf("%s never activated", grant.name)
	}
	grant.timeline.update(func() {
		incarnation.WithdrawAt = time.Now()
	})
	if err := grant.client.grants.Revoke(ctx, grantId, sidecar.SidecarActConfirmed, nil); err != nil {
		return fmt.Errorf("%s's revoke: %w", grant.name, err)
	}
	withdrawnAt := time.Now()
	pin, err := self.awaitRouteGrant(ctx, grant, false)
	if err != nil {
		return err
	}
	grant.timeline.update(func() {
		incarnation.WithdrawnAt, incarnation.WithdrawPin = withdrawnAt, pin
	})
	if err := self.AwaitAdopted(ctx, pin); err != nil {
		return fmt.Errorf("%s's revoke at %d: %w", grant.name, pin, err)
	}
	grant.timeline.update(func() {
		incarnation.WithdrawAdoptedAt = time.Now()
	})
	return nil
}

// The route grants' whole cycle (the file's header): every grant activated at once; each read at both ends of its
// rows and probed while in force, by TCP and UDP, which must reach its subnet within SweepTryCount tries; each ended,
// by its expiry or its revoke; and past the last end and LeaveBound each probed again SweepDeniedTryCount times, which
// the oracle holds to no answer. A failed activation fails the test with a progress dump; every other failure is an
// error.
func (self *ItBridgeWorld) RouteGrantCycle(ctx context.Context, t testing.TB) {
	t.Helper()
	var group sync.WaitGroup
	errs := make([]error, len(self.routeGrants))
	for index, grant := range self.routeGrants {
		group.Go(func() {
			errs[index] = self.ActivateRouteGrant(ctx, grant)
		})
	}
	group.Wait()
	if err := errors.Join(errs...); err != nil {
		self.FailProgress(t, "the route grants' activations", err, nil)
	}
	for _, grant := range self.routeGrants {
		for _, err := range self.routeGrantRows(grant) {
			t.Errorf("%s: %v", grant.name, err)
		}
	}
	probes := self.probeRouteGrants(ctx, "grant", true)
	for _, err := range probes {
		t.Errorf("%v", err)
	}
	clear(errs)
	for index, grant := range self.routeGrants {
		if grant.expires {
			continue
		}
		group.Go(func() {
			errs[index] = self.RevokeRouteGrant(ctx, grant)
		})
	}
	group.Wait()
	if err := errors.Join(errs...); err != nil {
		self.FailProgress(t, "the route grants' revokes", err, nil)
	}
	// past every grant's end and the bound, its pair is the document's again: denied
	ended := time.Time{}
	for _, grant := range self.routeGrants {
		if incarnation := grant.timeline.newest(); incarnation != nil {
			grant.timeline.update(func() {
				if ended.Before(incarnation.WithdrawnAt) {
					ended = incarnation.WithdrawnAt
				}
			})
		}
	}
	select {
	case <-ctx.Done():
		self.FailProgress(t, "the route grants' ends", ctx.Err(), nil)
	case <-time.After(time.Until(ended.Add(self.settings.LeaveBound))):
	}
	self.probeRouteGrants(ctx, "grant ended", false)
}

// Each grant's client to its subnet over TCP and UDP, at once: while in force each pair must be reached within
// SweepTryCount tries, and once ended it is tried SweepDeniedTryCount times, which the oracle judges. The errors of the
// first.
func (self *ItBridgeWorld) probeRouteGrants(ctx context.Context, phase string, inForce bool) []error {
	var group sync.WaitGroup
	errsLock := sync.Mutex{}
	errs := []error{}
	for _, grant := range self.routeGrants {
		for _, ipProtocol := range grant.subnet.protocols() {
			group.Go(func() {
				tryCount := self.settings.SweepTryCount
				if !inForce {
					tryCount = self.settings.SweepDeniedTryCount
				}
				var last *ItBridgeAttempt
				for try := range tryCount {
					if 0 < try {
						select {
						case <-ctx.Done():
							return
						case <-time.After(self.settings.SweepPause):
						}
					}
					last = self.Attempt(ctx, phase, grant.client, grant.subnet, ipProtocol)
					if last.Reached && inForce {
						return
					}
				}
				if inForce {
					errsLock.Lock()
					defer errsLock.Unlock()
					errs = append(errs, fmt.Errorf("%s: %s never reached %s over %s in %d tries while the grant was in force, last at %s: %s",
						grant.name, grant.client.name, grant.subnet.name, ipProtocol, tryCount, last.Address, last.Err))
				}
			})
		}
	}
	group.Wait()
	return errs
}

// What grant's compiles hold while it is in force (the file's header): an entry of the subnet's prefix in its client's
// table linked to the subnet's device, and a route row of the client's under the grant in that device's rows, holding
// the prefix. One error for each that is missing, naming where the compile reads a grant's prefixes.
func (self *ItBridgeWorld) routeGrantRows(grant *itBridgeRouteGrant) []error {
	errs := []error{}
	grantId := grant.GrantId()
	prefix := grant.subnet.prefix
	router := grant.subnet.device
	clientRules, err := compiledBridgeRules(grant.client)
	if err != nil {
		errs = append(errs, err)
	} else if !slices.ContainsFunc(clientRules.GetEntries(), func(entry *bridge.BridgeRuleEntry) bool {
		return bridgeWirePrefixIs(entry.GetPrefix(), prefix) && slices.ContainsFunc(entry.GetLinks(), func(link *bridge.BridgeRuleLink) bool {
			return bytes.Equal(link.GetTargetSidecarId(), router.SidecarId().Bytes())
		})
	}) {
		errs = append(errs, fmt.Errorf("%s's table holds no entry of %s linked to %s, which the grant's sealed detail names (the sender's compile "+
			"reads an activated grant's prefixes from the head's local policy, which holds none for it: bridge_compile_rows.go's mayUseRoute), %d entries",
			grant.client.name, prefix, router.name, len(clientRules.GetEntries())))
	}
	routerRules, err := compiledBridgeRules(router)
	if err != nil {
		errs = append(errs, err)
		return errs
	}
	subjectIndex := slices.IndexFunc(routerRules.GetSubjects(), func(subject *bridge.BridgeRuleSubject) bool {
		return bytes.Equal(subject.GetSidecarId(), grant.client.SidecarId().Bytes())
	})
	if subjectIndex < 0 {
		errs = append(errs, fmt.Errorf("%s's rows name no subject %s, which the grant lets route through it", router.name, grant.client.name))
		return errs
	}
	rows := routerRules.GetSubjects()[subjectIndex].GetRoute()
	if !slices.ContainsFunc(rows, func(row *bridge.BridgeRuleReach) bool {
		return bytes.Equal(row.GetGrantId(), grantId) && bridgeWirePrefixHolds(row.GetPrefix(), prefix)
	}) {
		errs = append(errs, fmt.Errorf("%s's rows give %s no route row of %s under the grant, so its forwarding role refuses the grant's flows "+
			"(rule 6; bridge_compile_rows.go's reachRows reads an activated grant's prefixes from the head's local policy, which holds none for it, so "+
			"routeRowPrefixes gives it none), %d route rows", router.name, grant.client.name, prefix, len(rows)))
	}
	return errs
}

// The BridgeRules device's bridge compiled last.
func compiledBridgeRules(device *itBridgeDevice) (*bridge.BridgeRules, error) {
	wire := device.currentWire()
	if wire == nil {
		return nil, fmt.Errorf("%s's bridge is down", device.name)
	}
	compiled := wire.component.Compiled()
	if compiled == nil || compiled.State == nil {
		return nil, fmt.Errorf("%s's bridge compiled no state", device.name)
	}
	rules := &bridge.BridgeRules{}
	if err := proto.Unmarshal(compiled.State.GetRules(), rules); err != nil {
		return nil, fmt.Errorf("%s's compiled rules: %w", device.name, err)
	}
	return rules, nil
}

// A wire prefix as netip's, invalid for one that is no IPv4 prefix.
func bridgeWirePrefix(wire *bridge.BridgeIpv4Prefix) netip.Prefix {
	if wire == nil || 32 < wire.GetLength() {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(itBridgeAddr(wire.GetAddress()), int(wire.GetLength())).Masked()
}

// Whether a wire prefix is prefix.
func bridgeWirePrefixIs(wire *bridge.BridgeIpv4Prefix, prefix netip.Prefix) bool {
	return bridgeWirePrefix(wire) == prefix.Masked()
}

// Whether a wire prefix holds the whole of prefix.
func bridgeWirePrefixHolds(wire *bridge.BridgeIpv4Prefix, prefix netip.Prefix) bool {
	held := bridgeWirePrefix(wire)
	return held.IsValid() && held.Bits() <= prefix.Bits() && held.Contains(prefix.Addr())
}

// Whether a route grant of client on target let ipProtocol through over [start, end]: one whose timeline allows the
// span, its withdrawal LeaveBound past.
func (self *ItBridgeWorld) routeGrantAllows(client *itBridgeDevice, target *itBridgeTarget, ipProtocol string, start time.Time, end time.Time) bool {
	if target.kind != itBridgeTargetSubnet || ipProtocol == "icmp" {
		return false
	}
	return slices.ContainsFunc(self.routeGrants, func(grant *itBridgeRouteGrant) bool {
		return grant.client == client && grant.subnet == target && grant.timeline.Allows(start, end, self.settings.LeaveBound)
	})
}

// Whether a route grant of client on target was in force at at.
func (self *ItBridgeWorld) routeGrantInForce(client *itBridgeDevice, target *itBridgeTarget, ipProtocol string, at time.Time) bool {
	if target.kind != itBridgeTargetSubnet || ipProtocol == "icmp" {
		return false
	}
	return slices.ContainsFunc(self.routeGrants, func(grant *itBridgeRouteGrant) bool {
		return grant.client == client && grant.subnet == target && grant.inForce(at)
	})
}
