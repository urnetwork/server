package controller

// §8.10's contract gate in CreateContract (ACL-M4-01; D-ACL-4, D-ACL-5, D-PROTO-18, R-ACL-8, I-10, I-12). A process
// that hosts §8.9's gate cache installs one sidecar.AclContractGate over it (SetAclContractGate), and CreateContract
// consults it while sidecar.yml's acl_contract_gate switch is on. An enabled host without verified heads refuses
// protected operations. With the switch off every network takes the legacy path.
//
// The relationship's one query reads each end's facts (model.GetProvideRelationshipDetailsWithAcl) whether or not a
// gate is on, since C13 seals the subject among them (PROTO-M4-02), so the gate adds no query, and decideContractAcl
// turns them into the gate's request with no I/O: the gate answers
// from its heads snapshot and the cache's state alone, under DenyPostgresForTest and with no Redis call. Only a reply,
// a non-companion that b's reach to a alone allows, reads Postgres again: CreateContract then waits for b's origin
// contract to a with the companion path's bounded wait, and only with a head row and only when reach(a,b) failed
// (plan-ACL-network-reply-contracts). A gate refusal answers ContractError_NoPermission and is never recorded as the
// route's rejection, since a grant can activate at any moment.
//
// Connect follows api's internal gate surface (sidecar.AclGateApiSource). Api follows its first metadata cluster
// (sidecar.AclGateDirectSource). Both install a contract gate only while the rollout switch is enabled.
//
// §21's hard deny runs first, whether or not a gate is installed or switched on (ABUSE-M5-07; D-ABUSE-4, I-12): §8.6's
// "abuse halt (§21)". A §21 suspension refuses new contracts for sidecar subjects only, shares included, so an
// ordinary VPN client of a suspended network keeps its reach (plan-ABUSE-suspension-effects, QD-049). An end is a
// sidecar subject when its subject is bound to a sidecar or its owner row marks it sidecar-held (strict_exp or
// tagged), from the relationship's one query, which reads each end's network on every leg; the control id's contracts,
// the platform's own channel, are skipped as §8.10's gate skips them. The held networks come
// from model.AbuseHardDenySnapshot, which the process installs (SetAbuseHardDeny; api, taskworker and connect at
// startup) and which reloads network_abuse_state every 5 s off the request path, so a request pays one map read per
// sidecar end and no query (DenyPostgresForTest). A snapshot not loaded, or too old to trust, refuses every sidecar
// end, never passes one. A process with no snapshot installed holds no network, as one with no gate takes today's
// path; a binary that does not link MG5 installs none, having no network_abuse_state. The refusal is NoPermission,
// counted as the gate's own (acl_no_permission, or acl_unavailable for a snapshot not in force), never recorded as the
// route's rejection, and it names no case: a barred case's network learns nothing beyond the deny itself (§21.8).

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The process's contract gate, nil while none is installed.
var aclContractGate atomic.Pointer[sidecar.AclContractGate]

// A gate with no heads refuses contracts and peer lists until the configured
// process has installed its verifier. The protocol's platform exception holds.
var unloadedContractGate sidecar.AclContractGate
var contractGateChanges = connect.NewMonitor()

// Wakes residents waiting for a switch or for a new gate to start serving.
func AclContractGateChanges() <-chan struct{} {
	return contractGateChanges.NotifyChannel()
}

// Installs the process's contract gate, nil removing it, and answers what restores the one before.
func SetAclContractGate(gate *sidecar.AclContractGate) (restore func()) {
	previous := aclContractGate.Swap(gate)
	contractGateChanges.NotifyAll()
	return func() {
		aclContractGate.Store(previous)
		contractGateChanges.NotifyAll()
	}
}

// The gate for CreateContract and §8.19 peer lists. Nil selects the disabled legacy path.
func ActiveAclContractGate() *sidecar.AclContractGate {
	gate, _ := activeAclContractGate()
	return gate
}

// Whether this process has installed a verified projection reader for its contract gate.
func AclContractGateInstalled() bool {
	return aclContractGate.Load() != nil
}

// The process's §21 hard deny, nil while none is installed.
var abuseHardDeny atomic.Pointer[model.AbuseHardDenySnapshot]

// Installs the snapshot CreateContract's hard deny reads, nil removing it, and answers what restores the one before.
func SetAbuseHardDeny(snapshot *model.AbuseHardDenySnapshot) (restore func()) {
	previous := abuseHardDeny.Swap(snapshot)
	return func() {
		abuseHardDeny.Store(previous)
	}
}

// Whether this process has installed a snapshot for CreateContract's hard deny, by InstallAbuseHardDeny or as
// InstallAbuseStanding's. A process with none holds no network.
func AbuseHardDenyInstalled() bool {
	return abuseHardDeny.Load() != nil
}

// Installs a hard deny snapshot of its own in a process that runs CreateContract and nothing else of §21, connect at
// startup, and answers what removes it and stops the snapshot. A binary that does not link MG5 installs none.
func InstallAbuseHardDeny(ctx context.Context) (restore func()) {
	if !server.SidecarMg5Linked() {
		return func() {}
	}
	snapshot := model.NewAbuseHardDenySnapshot(ctx, nil)
	restoreHardDeny := SetAbuseHardDeny(snapshot)
	return func() {
		restoreHardDeny()
		snapshot.Close()
	}
}

// Installs §21.7's reads in a process that holds Postgres, api's and taskworker's startup: model.InstallAbuseStanding's
// gate, sources and checks, and its snapshot as CreateContract's hard deny, which api's control route runs. Answers what
// puts the previous ones back and stops the snapshot and the gate. A binary that does not link MG5 installs the reads,
// which then answer by a network's age alone, and no hard deny.
func InstallAbuseStanding(ctx context.Context) (restore func()) {
	hardDeny, restoreStanding := model.InstallAbuseStanding(ctx)
	restoreHardDeny := func() {}
	if server.SidecarMg5Linked() {
		restoreHardDeny = SetAbuseHardDeny(hardDeny)
	}
	return func() {
		restoreHardDeny()
		restoreStanding()
	}
}

// The network of an end that is a sidecar subject, false for any other end: one whose subject is bound to a sidecar or
// whose owner row marks it sidecar-held. networkId and strictExp are the relationship's own, read on every leg and
// where MG0 is linked; acl is the end's gate facts, nil where MG0, MG1 and MG2 are not all linked, whose network is
// the same.
func contractAbuseSidecarEnd(networkId *server.Id, strictExp bool, acl *model.ProvideRelationshipAclEnd) (server.Id, bool) {
	sidecarHeld := strictExp
	if acl != nil {
		sidecarHeld = sidecarHeld || acl.SidecarId != nil || acl.StrictExp || acl.Tagged
		if networkId == nil {
			networkId = &acl.NetworkId
		}
	}
	if networkId == nil {
		// a client the server does not have, which creates nothing
		return server.Id{}, false
	}
	return *networkId, sidecarHeld
}

// §21's hard deny for one CreateContract from the relationship's facts: refused when a sidecar end's network is held,
// or when no snapshot is in force and an end is a sidecar's. The control id's contracts are the platform's own channel,
// which §8.10's gate skips too, so a suspension never cuts a connected client off from it. One map read per sidecar end,
// no I/O and no allocation.
func contractAbuseHardDeny(clientId server.Id, destinationId server.Id, details *model.ProvideRelationshipDetails) (verdict sidecar.AclContractVerdict, refused bool) {
	snapshot := abuseHardDeny.Load()
	if snapshot == nil || clientId == ControlId || destinationId == ControlId {
		return sidecar.AclContractVerdict{}, false
	}
	check := func(networkId server.Id, sidecarEnd bool) (sidecar.AclContractVerdict, bool) {
		if !sidecarEnd {
			return sidecar.AclContractVerdict{}, false
		}
		_, denied, ok := snapshot.Denied(networkId)
		switch {
		case !ok:
			return sidecar.AclContractVerdict{
				Outcome:   sidecar.AclContractRefuse,
				Reason:    sidecar.AclContractReasonNotLoaded,
				NetworkId: sidecar.Id(networkId),
				Decision:  sidecar.AclDecision{Reason: sidecar.AclReasonAbuseHalt},
			}, true
		case denied:
			return sidecar.AclContractVerdict{
				Outcome:   sidecar.AclContractRefuse,
				Reason:    sidecar.AclContractReasonDenied,
				NetworkId: sidecar.Id(networkId),
				Decision:  sidecar.AclDecision{Reason: sidecar.AclReasonAbuseHalt},
			}, true
		}
		return sidecar.AclContractVerdict{}, false
	}
	if verdict, refused := check(contractAbuseSidecarEnd(details.SourceNetworkId, details.SourceStrictExp, details.SourceAcl)); refused {
		return verdict, true
	}
	return check(contractAbuseSidecarEnd(details.DestinationNetworkId, details.DestinationStrictExp, details.DestinationAcl))
}

// The gate CreateContract consults and the sidecar.yml read that enabled it: the installed gate while the
// acl_contract_gate switch is on, else nil.
func activeAclContractGate() (*sidecar.AclContractGate, *server.SidecarConfig) {
	config := server.CurrentSidecarConfig()
	if !config.AclContractGate() {
		return nil, nil
	}
	gate := aclContractGate.Load()
	if gate == nil {
		gate = &unloadedContractGate
	}
	return gate, config
}

// sidecar.AclContractHeadSource over network_acl_head (ACL-M2-12): every network with a config repo and its newest
// verified position.
type aclContractModelHeads struct{}

func (aclContractModelHeads) Heads(ctx context.Context) (heads map[sidecar.Id]uint64, returnErr error) {
	if r := server.HandleError(func() {
		modelHeads, err := model.GetNetworkAclHeads(ctx)
		if err != nil {
			returnErr = err
			return
		}
		heads = make(map[sidecar.Id]uint64, len(modelHeads))
		for networkId, head := range modelHeads {
			heads[sidecar.Id(networkId)] = head.Version
		}
	}); r != nil {
		return nil, fmt.Errorf("network_acl_head: %v", r)
	}
	return heads, returnErr
}

// A contract gate over states, the process's gate cache, its heads read from network_acl_head every HeadsInterval
// on the wall clock. settings may be nil for §8.10's.
func NewAclContractGate(ctx context.Context, states sidecar.AclContractStateSource, settings *sidecar.AclContractGateSettings) *sidecar.AclContractGate {
	return sidecar.NewAclContractGate(ctx, states, aclContractModelHeads{}, nil, settings)
}

// The gate's end for a client from its facts: the control id and urnetwork-edge's platform clients skip the gate,
// and a client the query did not find is unknown, which a network with a head row refuses.
func contractAclEnd(clientId server.Id, end *model.ProvideRelationshipAclEnd, config *server.SidecarConfig) sidecar.AclContractEnd {
	if clientId == ControlId {
		return sidecar.AclContractEnd{Known: true, Platform: true}
	}
	if end == nil {
		return sidecar.AclContractEnd{}
	}
	contractEnd := end.ContractEnd()
	if edgeNetworkId, found := config.UrnetworkEdge(); found && end.NetworkId == edgeNetworkId {
		contractEnd.Platform = true
	}
	return contractEnd
}

// The gate's verdict for one CreateContract at now, from the relationship's one query, on the request's own
// Companion bit. Nothing here reads a store.
func decideContractAcl(
	gate *sidecar.AclContractGate,
	config *server.SidecarConfig,
	clientId server.Id,
	destinationId server.Id,
	createContract *protocol.CreateContract,
	details *model.ProvideRelationshipDetails,
	now time.Time,
) sidecar.AclContractVerdict {
	request := sidecar.AclContractRequest{
		Source:      contractAclEnd(clientId, details.SourceAcl, config),
		Destination: contractAclEnd(destinationId, details.DestinationAcl, config),
		Companion:   createContract.Companion,
	}
	// QUOTA-M1-04's predicate, which CreateContract also refuses with in every network
	request.Source.DeclaresNoEgress = details.DeclaredSourceRefused(model.ProvideModePublic)
	return gate.Decide(&request, now)
}

// Whether b's origin contract to a stands for a's reply to it, waited for as the companion path waits (QD-185): one
// read, then the bounded wait, woken by the pair's origin notifications. Errs only when ctx ends.
func contractAclReplyOrigin(
	ctx context.Context,
	notifications *model.ContractOriginNotifications,
	clientId server.Id,
	destinationId server.Id,
	contractManagerSettings *connect.ContractManagerSettings,
) (bool, error) {
	// the origin runs from the destination to the source
	originWatch := notifications.Watch(destinationId, clientId)
	defer originWatch.Close()
	originCloseLinger := max(contractManagerSettings.OriginContractLinger, model.CompanionOriginCloseLinger)
	defer server.EnterContractCreationStage(ctx, server.ContractStageCompanionOrigin)()
	_, err := waitForCompanionOrigin(ctx, originWatch, func() (*model.TransferEscrow, error) {
		if companionOriginExists(ctx, clientId, destinationId, originCloseLinger) {
			// a hint the reply needs and nothing more: it creates nothing
			return &model.TransferEscrow{}, nil
		}
		return nil, model.ErrMissingCompanionOrigin
	}, originWatch.Update)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, model.ErrMissingCompanionOrigin):
		return false, nil
	}
	return false, err
}

// The bounded rejection cause a gate refusal counts under (contractRejectionCauses): acl_unavailable when the gate had
// no heads snapshot or verified state to decide from, acl_no_permission when the rule refused. The reason itself goes
// to the V(2) log and the gate's stats, so the counter's reviewed cells stay fixed.
func contractAclRejectionCause(verdict sidecar.AclContractVerdict) string {
	switch verdict.Reason {
	case sidecar.AclContractReasonHeadsUnknown, sidecar.AclContractReasonNotLoaded:
		return "acl_unavailable"
	}
	return "acl_no_permission"
}

// Runs §21's hard deny, then the gate, for CreateContract: nil frames and data when the contract goes on, with the
// class C13 seals (PROTO-M4-02 writes the seal), or the refusal's frames, NoPermission. A gate of nil goes on as today,
// class data, once the hard deny passes.
func contractAclGate(
	ctx context.Context,
	gate *sidecar.AclContractGate,
	config *server.SidecarConfig,
	notifications *model.ContractOriginNotifications,
	clientId server.Id,
	destinationId server.Id,
	createContract *protocol.CreateContract,
	details *model.ProvideRelationshipDetails,
	contractManagerSettings *connect.ContractManagerSettings,
) (sidecar.AclContractVerdict, []*protocol.Frame, error) {
	verdict, abuseRefused := contractAbuseHardDeny(clientId, destinationId, details)
	if !abuseRefused {
		if gate == nil {
			return sidecar.AclContractVerdict{Outcome: sidecar.AclContractSkip, Class: sidecar.AclContractClassData}, nil, nil
		}
		verdict = decideContractAcl(gate, config, clientId, destinationId, createContract, details, server.NowUtc())
		if verdict.Outcome == sidecar.AclContractReply {
			originStands, err := contractAclReplyOrigin(ctx, notifications, clientId, destinationId, contractManagerSettings)
			if err != nil {
				return verdict, nil, err
			}
			verdict = verdict.Origin(originStands)
		}
		if verdict.Allowed() {
			return verdict, nil, nil
		}
	}
	if glog.V(2) {
		glog.Infof("[contract][reject]%s->%s acl %s (companion=%t)\n", clientId, destinationId, verdict.Reason, createContract.Companion)
	}
	contractError := protocol.ContractError_NoPermission
	frame, err := connect.ToFrame(&protocol.CreateContractResult{Error: &contractError}, connect.DefaultProtocolVersion)
	if err != nil {
		return verdict, nil, err
	}
	return verdict, []*protocol.Frame{frame}, nil
}
