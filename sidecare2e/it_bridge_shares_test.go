//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The it-bridge case's bridge share (it_bridge_test.go's TestE2eItBridgeShares; §17.9, §8.11, §17.16's share routes):
// §17's last way a subnet or a forward joins a reach and leaves it, here another network's. Two networks of one World:
// X, the case's network, shares a forward of its host and the subnet behind its router; Y, a second network of the
// case (ItBridgeWorld.AddPeer), accepts. Each act is an admin's confirmed command on its network's owner's device
// through its BridgeFiler, the acts `ur bridge invite`, `accept-invite` and `activate` run (bridge_share_daemon.go),
// over the share routes:
//
//	invite    X's owner seals the two targets under the code's key and posts them (POST /acl/invite)
//	accept    Y's owner claims the invite by the code's hash (POST /acl/accept-invite), opens the targets, plans an alias
//	          of each in Y's block's upper /17 and files the BridgeAliases, writes the share's first ShareSigner into a
//	          head of Y's, and completes the share with its sealed alias table and ShareKey
//	activate  X's owner previews the share (GET /network/bridge/share/<id>), then activates it as shown: ActivateShare
//	          asks the share's inbound grant, a dial on both target hosts for Y's network under the document's shares
//	          rule, through §16.6's stock gate, which holds it for an admin; another device of X's member signs the
//	          decision, and ActivateShare asked again with it presents the action its request was filed with, kept in
//	          the invite's record, spends the decision into the grant it activates and writes the BridgeShareGrant pin
//	          file in a head of X's; asked once more it answers that grant and files nothing
//	reach     Y's first client, whose document dials network:X (Y's half of §8.11's intersection), routes each alias over
//	          a share link to its target's host, whose hook maps it to the target, the host providing FriendsAndFamily
//	          while it serves the share; Y's second client has no such rule
//	end       X's owner revokes the share's grant (RevokeShare's GrantRevoke) and Y's owner releases its aliases
//	          (ReleaseShare)
//
// The checks: the alias tables, Y's accept and X's preview naming the same aliases, one a target at its length, in Y's
// upper /17; the activation's grant the one the admin's decision activates, AclGrantId of the request and the action
// it stored, spent once; X's target hosts compiling the share with those aliases, Y's first client's table an entry of
// each alias linked to its target's host under the share, and Y's second client's none; Y's first client reaching
// each target through its alias, by each protocol the target takes, while the share is in force, and Y's second never;
// neither network counting a share link's window client against sidecar_clients; and once the share is revoked and
// released, past LeaveBound, nothing reaching either target from Y. The oracle (Check) holds every arrival at X's
// targets from Y's clients to the share's targets, its timeline and its reachers.
//
// What stands in, beside the case's own (it_bridge_world_test.go's header): the share grant's person, whose decision
// the approve stand-in signs on another device of X's member (it_bridge_grants_test.go's header); Y's document's
// network:X selector, which a writer resolves only through networks its own host is enrolled in
// (sidecar.AclWriterSubjects.Networks), so Y's save hands it X's owner's Sidecar, as one daemon of a person who is an
// admin of both networks would hold both. X's label it reads from the network label table in X's owner's own dns/
// directory, which Sidecar.NetworkLabels opens where no composition fed its DnsClient one (dns-feed-labels-wiring), and
// the activation is ActivateShare's own (listen-actions-mcp-tls), where the case stood in for both before those fixes.
// Y's network starts with a document that grants nothing and saves its own once X's label is known. Both networks
// subscribe to Pro (ItBridgeSettings.Pro), since §20.4's Community column gives no acl_network_shares, which X's invite
// and network:* selector and Y's claim and network:X selector each need. X is aged past §21.7's probation, a network's
// first 7 days, which gives no outbound share (worldAgeNetwork, as the World's mail steps age a sender), and §21.7's
// standing reads api's process installs at its start, which the World's api does not, are installed for the case
// (itBridgeInstallAbuseStanding); a first invite on that cold gate is refused 403 while it loads X's standing, and the
// case files it again (ShareInviteTryCount). The platform settles a share's contracts by §8.10's contract gate, which
// the World turns on (WorldSettings.AclContractGate), as FriendsAndFamily, escrowed from Y's network in both directions
// (§20.6's share row), which the case's ledgers reconcile (it_bridge_test.go's Reconcile).

package sidecare2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// The store directory and seal class an admin's device keeps its ShareKeys under (sidecar.NewBridgeShareKeyLocalStore). A
// format value of the case's.
const itBridgeShareKeyDirectory = "itb-share-key"

// One target of a share: an X destination, and the alias Y's accept gave it.
type itBridgeShareTarget struct {
	target *itBridgeTarget
	// in the block Y's bootstrap drew (ItBridgeWorld.readBlock); invalid before the accept
	alias netip.Prefix
}

// One share between two networks of the case (the file's header).
type itBridgeShare struct {
	name string
	// the sharing network and the accepting one
	x *ItBridgeWorld
	y *ItBridgeWorld
	// in the order their indexes take
	targets []*itBridgeShareTarget
	// Y's clients whose document lets them reach X
	reachers []*itBridgeDevice
	// its one incarnation: filed at the invite, landed once the activation returned, adopted once every target host
	// compiled it and every reacher's table held its aliases; withdrawn from the revoke's start until both the revoke and
	// the release returned, adopted once the hosts and the reachers held it no more
	timeline *itBridgeTimeline

	stateLock sync.Mutex
	shareId   sidecar.Id
	grantId   []byte
	accepted  *sidecar.BridgeShareAccepted
	preview   *sidecar.BridgeSharePreview
	// how many invites api refused 403 before one was taken
	inviteRefusedCount int
}

// A share from self to y of targets, X destinations self's devices host, which y's reachers may reach.
func (self *ItBridgeWorld) NewShare(y *ItBridgeWorld, targets []*itBridgeTarget, reachers []*itBridgeDevice) *itBridgeShare {
	share := &itBridgeShare{
		name:     fmt.Sprintf("%s's share to %s", self.settings.NetworkName, y.settings.NetworkName),
		x:        self,
		y:        y,
		reachers: reachers,
		timeline: &itBridgeTimeline{},
	}
	for _, target := range targets {
		share.targets = append(share.targets, &itBridgeShareTarget{target: target})
	}
	for _, network := range []*ItBridgeWorld{self, y} {
		network.stateLock.Lock()
		network.shares = append(network.shares, share)
		network.stateLock.Unlock()
	}
	return share
}

// The share's id, zero before the invite.
func (self *itBridgeShare) ShareId() sidecar.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.shareId
}

// What a share target's host hosts for the invite: a forward as its host's overlay address and port, a subnet as its
// prefix on every protocol.
func (self *itBridgeShare) targetRequests() ([]*sidecar.BridgeShareTargetRequest, error) {
	requests := []*sidecar.BridgeShareTargetRequest{}
	for _, shareTarget := range self.targets {
		target := shareTarget.target
		switch target.kind {
		case itBridgeTargetForward:
			protocolNumber := uint32(6)
			if target.protocol == "udp" {
				protocolNumber = 17
			}
			requests = append(requests, &sidecar.BridgeShareTargetRequest{
				Host:     target.device.SidecarId(),
				Prefix:   netip.PrefixFrom(self.x.hostAddresses[target.device.hostIndex()], 32),
				Protocol: protocolNumber,
				Ports:    []*bridge.BridgePortRange{{First: uint32(target.port), Last: uint32(target.port)}},
			})
		case itBridgeTargetSubnet:
			requests = append(requests, &sidecar.BridgeShareTargetRequest{Host: target.device.SidecarId(), Prefix: target.prefix})
		default:
			return nil, fmt.Errorf("%s shares no %s", self.name, target.kind)
		}
	}
	return requests, nil
}

// Opens the share (the file's header's invite, accept and activate rows), its tables checked, then waits until every
// target host compiled it and every reacher's table holds its aliases.
func (self *itBridgeShare) Open(ctx context.Context) error {
	x, y := self.x, self.y
	origin := &sidecar.BridgeFileOrigin{Act: sidecar.SidecarActConfirmed}
	incarnation := self.timeline.begin()
	requests, err := self.targetRequests()
	if err != nil {
		return err
	}
	var invite *sidecar.BridgeShareInvite
	for try := range max(1, x.settings.ShareInviteTryCount) {
		if 0 < try {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(x.settings.ShareInvitePause):
			}
		}
		invite, err = x.owner.filer.Invite(ctx, origin, &sidecar.BridgeShareInviteRequest{Targets: requests})
		var apiErr *sidecar.SidecarApiError
		if err == nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
			break
		}
		// §21.7's gate loads X's standing at its first ask and refuses that ask as unread (ShareInviteTryCount)
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.inviteRefusedCount += 1
		}()
	}
	if err != nil {
		identity := x.owner.t3.Sidecar().Identity()
		return fmt.Errorf("%s's invite, tried %d times: %w; %s", self.name, max(1, x.settings.ShareInviteTryCount), err,
			itBridgeInviteFacts(ctx, x.network.NetworkId, server.Id(identity.ClientId)))
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.shareId = invite.ShareId
	}()
	accepted, err := y.owner.filer.AcceptInvite(ctx, origin, invite.Code)
	if err != nil {
		return fmt.Errorf("%s's accept: %w", self.name, err)
	}
	preview, err := x.owner.filer.PreviewShare(ctx, origin, invite.ShareId)
	if err != nil {
		return fmt.Errorf("%s's preview: %w", self.name, err)
	}
	if err := self.checkTables(accepted, preview); err != nil {
		return err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.accepted, self.preview = accepted, preview
		for index, shareTarget := range self.targets {
			alias := accepted.Aliases[index]
			shareTarget.alias = netip.PrefixFrom(itBridgeAddr(itBridgeIpv4(y.block.Masked().Addr())|alias.GetAliasHostBits()), int(alias.GetLength()))
		}
	}()
	grantId, err := self.activate(ctx, preview)
	if err != nil {
		return err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.grantId = bytes.Clone(grantId)
	}()
	self.timeline.update(func() {
		incarnation.RuleId, incarnation.LandedAt, incarnation.Pin = bytes.Clone(grantId), time.Now(), x.tip()
	})
	if err := self.awaitCompiled(ctx, true); err != nil {
		return err
	}
	self.timeline.update(func() {
		incarnation.AdoptedAt = time.Now()
	})
	return nil
}

// X's activation of the share as preview showed it (the file's header's activate row), answering the share grant's id.
// ActivateShare asks its grant through §16.6's gate, which holds it for an admin's decision (the document's shares
// rule, owner decision 74): its first call must answer pending, having filed nothing. Another device of X's member
// signs the decision (approve), and ActivateShare asked again with it must spend it into the grant that decision
// activates, AclGrantId of the request and the action it stored, since it presents the action its request was filed
// with, kept in the invite's record (listen-actions-mcp-tls, sidecar 468b7a92; the case activated in its place until
// then, the gate answering every later call a decision on another action). Asked once more with no decision, it must
// answer that grant again, filing no second request: a person's decision is spent once.
func (self *itBridgeShare) activate(ctx context.Context, preview *sidecar.BridgeSharePreview) ([]byte, error) {
	x := self.x
	origin := &sidecar.BridgeFileOrigin{Act: sidecar.SidecarActConfirmed}
	filing, err := x.owner.filer.ActivateShare(ctx, origin, preview, 0)
	var pending *sidecar.ApprovalPendingError
	if !errors.As(err, &pending) {
		if err != nil {
			return nil, fmt.Errorf("%s's activation: %w", self.name, err)
		}
		return nil, fmt.Errorf("%s's activation answered grant %x with no admin's decision, where the document's shares rule holds it for one", self.name, filing.GrantId)
	}
	if len(x.clients) == 0 {
		return nil, fmt.Errorf("%s: no other device of X's member approves", self.name)
	}
	request, err := x.storedRequest(x.owner, pending.ApprovalId)
	if err != nil {
		return nil, fmt.Errorf("%s's request: %w", self.name, err)
	}
	decision, err := x.approve(x.clients[0], request)
	if err != nil {
		return nil, fmt.Errorf("%s's approval: %w", self.name, err)
	}
	filing, err = x.owner.filer.ActivateShare(ctx, &sidecar.BridgeFileOrigin{Act: sidecar.SidecarActConfirmed, Decision: decision}, preview, 0)
	if err != nil {
		return nil, fmt.Errorf("%s's activation handed the decision signed on its own request %s: %w", self.name, pending.ApprovalId, err)
	}
	grantId := sidecar.AclGrantId(pending.ApprovalId, request.Action.PayloadSha256())
	if !bytes.Equal(filing.GrantId, grantId[:]) || filing.ApprovalId != pending.ApprovalId {
		return nil, fmt.Errorf("%s's activation spent under %s into grant %x, where the decision on request %s activates grant %x", self.name,
			filing.ApprovalId, filing.GrantId, pending.ApprovalId, grantId)
	}
	again, err := x.owner.filer.ActivateShare(ctx, origin, preview, 0)
	if err != nil {
		return nil, fmt.Errorf("%s's activation asked again after its decision was spent: %w", self.name, err)
	}
	if !bytes.Equal(again.GrantId, grantId[:]) {
		return nil, fmt.Errorf("%s's activation asked again answered grant %x, not the spent decision's %x", self.name, again.GrantId, grantId)
	}
	return grantId[:], nil
}

// The alias tables (the file's header's checks): Y's accept active with one alias a target at its length in Y's upper
// /17, and X's preview of the share as the sharing network's api row holds it naming Y, the same targets' count and the
// same aliases and ShareKey.
func (self *itBridgeShare) checkTables(accepted *sidecar.BridgeShareAccepted, preview *sidecar.BridgeSharePreview) error {
	errs := []error{}
	if accepted.Status != sidecar.BridgeShareAcceptStatusActive {
		errs = append(errs, fmt.Errorf("Y's accept answered %s", accepted.Status))
	}
	if accepted.SharingNetworkId != sidecar.Id(self.x.network.NetworkId) {
		errs = append(errs, fmt.Errorf("Y's accept names sharing network %s, not X's", accepted.SharingNetworkId))
	}
	if len(accepted.Aliases) != len(self.targets) {
		errs = append(errs, fmt.Errorf("Y's accept holds %d aliases for %d targets", len(accepted.Aliases), len(self.targets)))
	} else {
		requests, _ := self.targetRequests()
		for index, alias := range accepted.Aliases {
			want := uint32(requests[index].Prefix.Bits())
			if alias.GetIndex() != uint32(index) || alias.GetLength() != want || alias.GetAliasHostBits() < 0x8000 || 0xffff < alias.GetAliasHostBits() {
				errs = append(errs, fmt.Errorf("Y's alias %d is %+v: index %d, length %d, in the upper /17", index, alias, index, want))
			}
		}
	}
	if preview.AcceptingNetworkId != sidecar.Id(self.y.network.NetworkId) || preview.Status != sidecar.BridgeShareAcceptStatusActive {
		errs = append(errs, fmt.Errorf("X's preview names accepting network %s, status %s", preview.AcceptingNetworkId, preview.Status))
	}
	if len(preview.Targets) != len(self.targets) || len(preview.Aliases) != len(accepted.Aliases) {
		errs = append(errs, fmt.Errorf("X's preview holds %d targets and %d aliases", len(preview.Targets), len(preview.Aliases)))
	} else {
		for index, alias := range preview.Aliases {
			held := accepted.Aliases[index]
			if alias.GetIndex() != held.GetIndex() || alias.GetAliasHostBits() != held.GetAliasHostBits() || alias.GetLength() != held.GetLength() {
				errs = append(errs, fmt.Errorf("X's preview's alias %d is %+v, and Y's accept holds %+v", index, alias, held))
			}
		}
	}
	if !bytes.Equal(preview.SharePublicKey, accepted.SharePublicKey) || len(preview.AliasTableSha256) == 0 || len(preview.TargetTableSha256) == 0 {
		errs = append(errs, errors.New("X's preview pins another ShareKey than Y's accept, or no table's hash"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%s's alias tables: %w", self.name, err)
	}
	return nil
}

// Waits until the compiles hold the share, or with held false hold it no more: each target host's rules hold the
// share with its targets there at their aliases, and each reacher's table an entry of each alias linked to its target's
// host under the share; Y's other clients' tables link no alias under it either way. A host holds a revoked share no
// more once its rules drop it or once its role's posture holds the GrantRevoke of the share's grant as a denial, which
// is how §17.8's forwarding roles apply one between compiles, so its rules may keep the share until the next compile.
// Bounded by AdoptTimeout; the error names what each compile held.
func (self *itBridgeShare) awaitCompiled(ctx context.Context, held bool) error {
	ctx, cancel := context.WithTimeout(ctx, self.x.settings.AdoptTimeout)
	defer cancel()
	for {
		missing := self.compiledMissing(held)
		if len(missing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: the compiles never came to hold it %t: %w (%w)", self.name, held, errors.Join(missing...), ctx.Err())
		case <-time.After(self.x.settings.AdoptPollInterval):
		}
	}
}

// What the compiles do not hold as awaitCompiled waits for it, one error a compile.
func (self *itBridgeShare) compiledMissing(held bool) []error {
	shareId := self.ShareId()
	errs := []error{}
	hosts := []*itBridgeDevice{}
	for _, shareTarget := range self.targets {
		if !slices.Contains(hosts, shareTarget.target.device) {
			hosts = append(hosts, shareTarget.target.device)
		}
	}
	for _, host := range hosts {
		rules, err := compiledBridgeRules(host)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		hostTargets := 0
		for _, shareTarget := range self.targets {
			if shareTarget.target.device == host {
				hostTargets += 1
			}
		}
		served := slices.ContainsFunc(rules.GetShares(), func(share *bridge.BridgeRuleShare) bool {
			if !bytes.Equal(share.GetShareId(), shareId.Bytes()) || len(share.GetTargets()) != hostTargets {
				return false
			}
			for _, target := range share.GetTargets() {
				if uint32(len(self.targets)) <= target.GetIndex() {
					return false
				}
				shareTarget := self.targets[target.GetIndex()]
				if shareTarget.target.device != host || shareTarget.alias.Bits() != int(target.GetLength()) ||
					itBridgeIpv4(shareTarget.alias.Addr())&0xffff != target.GetAliasHostBits() {
					return false
				}
			}
			return true
		})
		if served && !held && self.revokedAtRole(host) {
			// the GrantRevoke denies the share at the role until a compile drops it
			served = false
		}
		if served != held {
			errs = append(errs, fmt.Errorf("%s's rules serve the share %t, %d shares, its role denies the share's grant %t", host.name, served,
				len(rules.GetShares()), self.revokedAtRole(host)))
		}
	}
	// a client of Y's its document lets reach no other network links no alias under the share, held or not
	for _, client := range self.y.clients {
		if slices.Contains(self.reachers, client) {
			continue
		}
		rules, err := compiledBridgeRules(client)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if slices.ContainsFunc(rules.GetEntries(), func(entry *bridge.BridgeRuleEntry) bool {
			return slices.ContainsFunc(entry.GetLinks(), func(link *bridge.BridgeRuleLink) bool {
				return bytes.Equal(link.GetShareId(), shareId.Bytes())
			})
		}) {
			errs = append(errs, fmt.Errorf("%s's table links an alias under the share, and its document lets it reach no other network", client.name))
		}
	}
	for _, reacher := range self.reachers {
		rules, err := compiledBridgeRules(reacher)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, shareTarget := range self.targets {
			linked := slices.ContainsFunc(rules.GetEntries(), func(entry *bridge.BridgeRuleEntry) bool {
				return bridgeWirePrefixIs(entry.GetPrefix(), shareTarget.alias) && slices.ContainsFunc(entry.GetLinks(), func(link *bridge.BridgeRuleLink) bool {
					return bytes.Equal(link.GetShareId(), shareId.Bytes()) && bytes.Equal(link.GetTargetSidecarId(), shareTarget.target.device.SidecarId().Bytes())
				})
			})
			if linked != held {
				errs = append(errs, fmt.Errorf("%s's table links %s's alias %s to %s under the share %t, %d entries, %s",
					reacher.name, shareTarget.target.name, shareTarget.alias, shareTarget.target.device.name, linked, len(rules.GetEntries()),
					self.y.compileWaits(reacher)))
			}
		}
	}
	return errs
}

// Whether host's role holds the GrantRevoke of the share's grant in its posture's overlay, a denial in force.
func (self *itBridgeShare) revokedAtRole(host *itBridgeDevice) bool {
	self.stateLock.Lock()
	grantId := bytes.Clone(self.grantId)
	self.stateLock.Unlock()
	wire := host.currentWire()
	if wire == nil || len(grantId) == 0 {
		return false
	}
	return slices.ContainsFunc(wire.bridge.Decision().Overlay, func(entry device.PostureOverlayEntry) bool {
		return entry.Kind == device.PostureOverlayGrantRevoke && bytes.Equal(entry.Id, grantId)
	})
}

// What device's compile waited for, for a failure's message.
func (self *ItBridgeWorld) compileWaits(device *itBridgeDevice) string {
	wire := device.currentWire()
	if wire == nil {
		return "bridge down"
	}
	status := wire.Status()
	return fmt.Sprintf("compile waits %+v, alarms %+v", status.Waits, status.Alarms)
}

// Ends the share (the file's header's end row): X's owner revokes its grant and Y's owner releases its aliases, at
// once, then the compiles are waited for until they hold it no more.
func (self *itBridgeShare) Close(ctx context.Context) error {
	incarnation := self.timeline.newest()
	shareId := self.ShareId()
	if incarnation == nil || shareId == (sidecar.Id{}) {
		return fmt.Errorf("%s never opened", self.name)
	}
	origin := &sidecar.BridgeFileOrigin{Act: sidecar.SidecarActConfirmed}
	self.timeline.update(func() {
		incarnation.WithdrawAt = time.Now()
	})
	var group sync.WaitGroup
	var revokeErr, releaseErr error
	group.Go(func() {
		_, revokeErr = self.x.owner.filer.RevokeShare(ctx, origin, shareId)
	})
	group.Go(func() {
		_, releaseErr = self.y.owner.filer.ReleaseShare(ctx, origin, shareId)
	})
	group.Wait()
	if err := errors.Join(revokeErr, releaseErr); err != nil {
		return fmt.Errorf("%s's end: %w", self.name, err)
	}
	self.timeline.update(func() {
		incarnation.WithdrawnAt, incarnation.WithdrawPin = time.Now(), self.x.tip()
	})
	if err := self.awaitCompiled(ctx, false); err != nil {
		return err
	}
	self.timeline.update(func() {
		incarnation.WithdrawAdoptedAt = time.Now()
	})
	return nil
}

// The protocols a share target takes: a forward's own, or a subnet's TCP and UDP.
func (self *itBridgeShareTarget) protocols() []string {
	return self.target.protocols()
}

// The address a Y client sends to for a share target: a random host of a subnet's alias on the subnet's service port,
// keeping host bits, or a forward's alias and port.
func (self *itBridgeShare) addressOf(shareTarget *itBridgeShareTarget) netip.AddrPort {
	switch shareTarget.target.kind {
	case itBridgeTargetSubnet:
		address := self.x.addressOf(shareTarget.target)
		hostBits := itBridgeIpv4(address.Addr()) &^ itBridgeIpv4(shareTarget.target.prefix.Masked().Addr())
		return netip.AddrPortFrom(itBridgeAddr(itBridgeIpv4(shareTarget.alias.Masked().Addr())|hostBits), address.Port())
	}
	return netip.AddrPortFrom(shareTarget.alias.Addr(), shareTarget.target.port)
}

// Every share target from every Y client over each protocol it takes, at once, phase naming them: while the share is in
// force a reacher must be answered within SweepTryCount tries, and every other try, SweepDeniedTryCount of them, is the
// oracle's to judge. The errors of the first, with the target hosts' provide modes for a pair never answered.
func (self *itBridgeShare) Probe(ctx context.Context, phase string, inForce bool) []error {
	y := self.y
	var group sync.WaitGroup
	errsLock := sync.Mutex{}
	errs := []error{}
	for _, client := range y.clients {
		reacher := slices.Contains(self.reachers, client)
		for _, shareTarget := range self.targets {
			for _, ipProtocol := range shareTarget.protocols() {
				group.Go(func() {
					want := inForce && reacher
					tryCount := y.settings.SweepTryCount
					if !want {
						tryCount = y.settings.SweepDeniedTryCount
					}
					var last *ItBridgeAttempt
					for try := range tryCount {
						if 0 < try {
							select {
							case <-ctx.Done():
								return
							case <-time.After(y.settings.SweepPause):
							}
						}
						last = y.AttemptAt(ctx, phase, client, shareTarget.target, ipProtocol, self.addressOf(shareTarget), want)
						if last.Reached && want {
							return
						}
					}
					if want {
						host := shareTarget.target.device
						clientId := server.Id(host.t3.Sidecar().Identity().ClientId)
						errsLock.Lock()
						defer errsLock.Unlock()
						errs = append(errs, fmt.Errorf("%s: %s never reached %s at its alias %s over %s in %d tries while the share was in force, last %s: %s; "+
							"%s's client provides %v, and a share link's contracts settle FriendsAndFamily only to a client providing it "+
							"(controller's resolveNonCompanionProvideMode), which a host holds while it serves the share (Subprotocol.HoldProvideMode); "+
							"%s's links asked %d share links",
							self.name, client.name, shareTarget.target.name, shareTarget.alias, ipProtocol, tryCount, last.Address, last.Err,
							host.name, itBridgeProvideModes(ctx, clientId), client.name, client.shareLinkCount()))
					}
				})
			}
		}
	}
	group.Wait()
	return errs
}

// How many share links device's current wire asked for.
func (self *itBridgeDevice) shareLinkCount() int {
	if wire := self.currentWire(); wire != nil {
		return wire.links.ShareLinkCount()
	}
	return 0
}

// The share whose accepting network is self and whose targets hold target, with the share target; nil for none.
func (self *ItBridgeWorld) acceptedShareOf(target *itBridgeTarget) (*itBridgeShare, *itBridgeShareTarget) {
	self.stateLock.Lock()
	shares := slices.Clone(self.shares)
	self.stateLock.Unlock()
	for _, share := range shares {
		if share.y != self {
			continue
		}
		for _, shareTarget := range share.targets {
			if shareTarget.target == target {
				return share, shareTarget
			}
		}
	}
	return nil, nil
}

// Whether client, of self, may have sent protocol to target, another network's, over [start, end]: a share self
// accepted holds the target and the client among its reachers, the protocol is one the target takes, and the share's
// timeline allows the span, its end LeaveBound past.
func (self *ItBridgeWorld) shareAllows(client *itBridgeDevice, target *itBridgeTarget, ipProtocol string, start time.Time, end time.Time) bool {
	share, shareTarget := self.acceptedShareOf(target)
	if share == nil || !slices.Contains(share.reachers, client) {
		return false
	}
	network := ipProtocol
	if network == "stream" {
		network = "tcp"
	}
	return slices.Contains(shareTarget.protocols(), network) && share.timeline.Allows(start, end, self.settings.LeaveBound)
}

// Why an arrival at target, of self, from a client of no network of self's but a peer's, is not one a share allows; nil
// for one it allows: the client is a reacher of a share self gave that holds target, by a protocol the target takes,
// to the target's alias in the reacher's block, within the share's timeline.
func (self *ItBridgeWorld) checkShareArrival(target *itBridgeTarget, arrival *ItBridgeArrival) error {
	self.stateLock.Lock()
	shares := slices.Clone(self.shares)
	self.stateLock.Unlock()
	for _, share := range shares {
		if share.x != self {
			continue
		}
		client := share.y.clientNamed(arrival.Client)
		if client == nil {
			continue
		}
		index := slices.IndexFunc(share.targets, func(shareTarget *itBridgeShareTarget) bool { return shareTarget.target == target })
		if index < 0 {
			return fmt.Errorf("%s of %s reached %s, which is no target of %s", client.name, share.y.settings.NetworkName, target.name, share.name)
		}
		shareTarget := share.targets[index]
		if !slices.Contains(share.reachers, client) {
			return fmt.Errorf("%s reached %s, and its network's document lets it reach no other network", client.name, target.name)
		}
		if !slices.Contains(shareTarget.protocols(), arrival.Network) {
			return fmt.Errorf("%s reached %s over %s, which the share's target does not take", client.name, target.name, arrival.Network)
		}
		if !share.timeline.Allows(arrival.At, arrival.At, self.settings.LeaveBound) {
			return fmt.Errorf("%s reached %s outside %s's life, %s", client.name, target.name, share.name, describeItBridgeTimeline(share.timeline))
		}
		address, err := netip.ParseAddrPort(arrival.Destination)
		port := self.settings.SubnetServicePort
		if target.kind == itBridgeTargetForward {
			port = target.port
		}
		if err != nil || !shareTarget.alias.Contains(address.Addr()) || address.Port() != port {
			return fmt.Errorf("%s reached %s at %s, which is not its alias %s", client.name, target.name, arrival.Destination, shareTarget.alias)
		}
		return nil
	}
	return errors.New("from no client of the case")
}

// A filer for an admin's device that also files the shares' acts, as ur daemon's composition builds one for a config
// writer (cli's bridge_filing_config.go): newFiler's, with the device's grant filer over its follower's engine and its
// Lookup registered for bridge_forward, its config writer (itBridgeHeadWriter), and a ShareKey keyring over its own
// sealed store (sidecar.NewBridgeShareKeyLocalStore, bridge_share_key.go's narrower custody), its share acts' settings
// the defaults with ShareReleaseDrain as a release's drain.
func (self *ItBridgeWorld) newShareFiler(device *itBridgeDevice) (*sidecar.BridgeFiler, error) {
	follower := device.t3.Follower()
	running := device.t3.Sidecar()
	device.grants = sidecar.NewAclGrantFiler(running, follower.AclEngine())
	self.world.T3().OnClose(running.RegisterEffectLookup(sidecar.SidecarActionForward, device.grants.Lookup))
	store, err := running.LocalStore().Directory(itBridgeShareKeyDirectory, itBridgeShareKeyDirectory)
	if err != nil {
		return nil, err
	}
	keyStore, err := sidecar.NewBridgeShareKeyLocalStore(store)
	if err != nil {
		return nil, err
	}
	keys, err := sidecar.NewBridgeShareKeyring(&sidecar.BridgeShareKeyringArgs{Store: keyStore})
	if err != nil {
		return nil, err
	}
	shareSettings := sidecar.DefaultBridgeShareFilerSettings()
	shareSettings.ReleaseDrain = self.settings.ShareReleaseDrain
	writer := &itBridgeHeadWriter{world: self, device: device}
	return sidecar.NewBridgeFiler(&sidecar.BridgeFilerArgs{
		Sidecar:    running,
		Config:     follower.Client(),
		Heads:      follower.Heads(),
		Grants:     device.grants,
		AclLocal:   follower.AclLocal,
		HeadWriter: func() sidecar.BridgeHeadWriter { return writer },
		Share:      &sidecar.BridgeShareFilerArgs{Keys: keys, Settings: shareSettings},
	})
}

// The document's requests rule a share's inbound grant is activated under (ItBridgeSettings.ShareTargets), nil without
// one.
func (self *ItBridgeWorld) shareRequests(selector func(device *itBridgeDevice) string) []map[string]any {
	if !self.settings.ShareTargets {
		return nil
	}
	dst := []string{}
	for _, device := range slices.Concat(self.routerDevices(), self.hosts) {
		dst = append(dst, selector(device))
	}
	return []map[string]any{{"src": []string{"network:*"}, "dst": dst, "allow": []string{"dial"}, "approver": []string{"autogroup:admin"}}}
}

// Saves source as the network's document by its owner's device (World.SaveAclPolicy), or with subjects through a save
// that resolves selectors with them, as `ur acl push` on a host holding those networks does: planned and saved at the
// plan's digest as a confirmed act, again past each 429 once its retry_after has passed within the World's
// RateLimitWaitTimeout, then synced at the base.
func (self *ItBridgeWorld) saveDocument(ctx context.Context, source string, subjects *sidecar.AclWriterSubjects) error {
	if subjects == nil {
		_, err := self.world.SaveAclPolicy(ctx, self.network, self.owner.t3, source)
		return err
	}
	running := self.owner.t3.Sidecar()
	waitCtx, cancel := context.WithTimeout(ctx, self.world.Settings().RateLimitWaitTimeout)
	defer cancel()
	for {
		pin, err := func() (*bridge.AuthPin, error) {
			plan, err := running.PlanAclPolicyWithSubjects(waitCtx, []byte(source), subjects)
			if err != nil {
				return nil, fmt.Errorf("%s's ACL plan: %w", self.owner.name, err)
			}
			if err := plan.Tests.Err(); err != nil {
				return nil, fmt.Errorf("%s's ACL document fails its tests: %w", self.owner.name, err)
			}
			return running.SaveAclPolicyWithSubjects(waitCtx, []byte(source), plan.Digest, sidecar.SidecarActConfirmed, subjects)
		}()
		var apiErr *sidecar.SidecarApiError
		if err == nil {
			return self.network.Sync(ctx, pin.GetVersion())
		}
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
			return err
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("the document: %w, and its retry_after outlasted the wait: %w", err, waitCtx.Err())
		case <-time.After(time.Duration(apiErr.RetryAfterMs) * time.Millisecond):
		}
	}
}

// The accepting network's document (the file's header's reach row), saved by Y's owner: its reachers dial
// network:<xLabel>, X's label, which the save resolves through X's owner's Sidecar as a writer subject, reading the label
// from the network label table in that Sidecar's own dns/ directory (Sidecar.NetworkLabels), and nothing else is
// granted.
func (self *itBridgeShare) SaveAcceptingDocument(ctx context.Context, t testing.TB, xLabel string) {
	t.Helper()
	y := self.y
	src := []string{}
	for _, reacher := range self.reachers {
		src = append(src, "sidecar:"+reacher.SidecarId().String())
	}
	source, err := json.Marshal(map[string]any{"grants": []map[string]any{{"src": src, "dst": []string{"network:" + xLabel}, "allow": []string{"dial"}}}})
	if err != nil {
		t.Fatal(err)
	}
	subjects := &sidecar.AclWriterSubjects{Networks: []*sidecar.Sidecar{self.x.owner.t3.Sidecar()}}
	if err := y.saveDocument(ctx, string(source), subjects); err != nil {
		y.FailProgress(t, "Y's document", err, nil)
	}
	y.policy = string(source)
	if err := y.awaitDocument(ctx); err != nil {
		y.FailProgress(t, "Y's document", err, nil)
	}
	if err := y.AwaitAdopted(ctx, y.tip()); err != nil {
		y.FailProgress(t, "Y's document", err, nil)
	}
}

// The share's whole cycle (the file's header), failing t with a progress dump where an act fails: open, probe while in
// force and read both networks' sidecar client counts, close, and past the end and LeaveBound probe again.
func (self *itBridgeShare) Cycle(ctx context.Context, t testing.TB) {
	t.Helper()
	if err := self.Open(ctx); err != nil {
		self.y.FailProgress(t, "the share's opening", err, nil)
	}
	for _, err := range self.Probe(ctx, "share", true) {
		t.Errorf("%v", err)
	}
	// the share links' window clients, children of Y's reachers' clients, count against neither network
	self.x.CheckSidecarClients(ctx, t)
	self.y.CheckSidecarClients(ctx, t)
	if err := self.Close(ctx); err != nil {
		self.y.FailProgress(t, "the share's end", err, nil)
	}
	ended := time.Time{}
	if incarnation := self.timeline.newest(); incarnation != nil {
		self.timeline.update(func() {
			ended = incarnation.WithdrawnAt
		})
	}
	select {
	case <-ctx.Done():
		self.y.FailProgress(t, "the share's end", ctx.Err(), nil)
	case <-time.After(time.Until(ended.Add(self.y.settings.LeaveBound))):
	}
	self.Probe(ctx, "share ended", false)
}
