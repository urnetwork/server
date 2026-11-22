package model

// §14.14's two §8 checks (SmtpApiGate) over api's one gate cache (C-09), which api's process installs over the cache
// its projection publishes, beside the approval gate's states and the dns scopes' labels
// (controller.installApiProjection; sidecart3's T3 world mirrors it):
//
//	mail_send   POST /smtp/dkim-keys' publish: §8.10's mail check, AclGateView.MaySendMail, on the cache's verified
//	            state, with §8.6's hard deny of a §21 suspension read from Postgres for the one network asked, which an
//	            api route may read (it is no inline path, I-12) and the handler asks before its transaction holds the
//	            connection. A network the cache has not loaded asks the cache for a load and, like a standing that did
//	            not read, answers unavailable (the route's 503, the edge's TRY_LATER); every other refusal, no grant or
//	            a suspended standing, is no mail_send (403)
//	read        GET /smtp/reports: evs read for the caller's sidecar at now on the repo the rows name as the network
//	            mail key's (AclState.NetworkMailKeyRepo, §15.8), the audience the sealed copies are sealed to
//	            (plan-SMTP-small-readings#16). A §21 suspension does not reach it: a suspension never reaches the
//	            network's own data (§8.6, §21.7), and the sealed copies are the only content evidence an appeal may
//	            volunteer (§14.8). Rows that name no such repo give no sidecar read, since no copy has an audience
//	            then; a network the cache has not loaded answers unavailable
//
// Neither check reads an alias, a recipient or a client IP (I-13). Safe for concurrent use.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The checks over states, the process's gate cache or a stand-in for it. See the file header.
type smtpApiCacheGate struct {
	states sidecar.AclContractStateSource
}

var _ SmtpApiGate = (*smtpApiCacheGate)(nil)

// The routes' §8 checks over states, the process's AclGateCache (C-09), as api's process installs them
// (SetSmtpApiGate). A nil states answers every check unavailable.
func SmtpApiGateOf(states sidecar.AclContractStateSource) SmtpApiGate {
	return &smtpApiCacheGate{states: states}
}

// See SmtpApiGate.
func (self *smtpApiCacheGate) MaySendMail(ctx context.Context, networkId server.Id, sidecarId server.Id, now time.Time) (bool, error) {
	if self.states == nil {
		return false, fmt.Errorf("%w: no gate cache", ErrSmtpApiUnavailable)
	}
	// a view of this one call: its standing is the asked network's, read at now
	standing := &smtpApiNetworkStanding{ctx: ctx, networkId: networkId, now: now}
	decision := sidecar.NewAclGateView(self.states, standing, nil).MaySendMail(sidecar.Id(networkId), sidecar.AclSidecarSubject(sidecar.Id(sidecarId)), now)
	switch {
	case decision.Allowed:
		return true, nil
	case decision.Reason == sidecar.AclReasonNoState:
		return false, fmt.Errorf("%w: the network's verified state or standing is not loaded", ErrSmtpApiUnavailable)
	}
	return false, nil
}

// See SmtpApiGate.
func (self *smtpApiCacheGate) MayReadReports(ctx context.Context, networkId server.Id, sidecarId server.Id, now time.Time) (bool, error) {
	if self.states == nil {
		return false, fmt.Errorf("%w: no gate cache", ErrSmtpApiUnavailable)
	}
	snapshot := sidecar.RequestAclGateSnapshot(self.states, sidecar.Id(networkId))
	if snapshot == nil || snapshot.Acl == nil {
		self.states.Changed(sidecar.Id(networkId))
		return false, fmt.Errorf("%w: the network's verified state is not loaded", ErrSmtpApiUnavailable)
	}
	repoId, found := snapshot.Acl.NetworkMailKeyRepo()
	if !found {
		return false, nil
	}
	access := snapshot.Acl.EvsAccess(sidecar.AclSidecarSubject(sidecar.Id(sidecarId)), repoId, sidecar.AclTokenAudienceEvs, now)
	return access.Rights.Has(sidecar.AclEvsRead), nil
}

// §21.7's standing of the one network a mail_send check asks, read from Postgres when the view asks it
// (GetNetworkAbuseStanding), as a sidecar.AbuseGate. Any other network, a network Postgres does not hold and a read
// that failed answer not loaded. Verified is no part of the mail check, and answers not loaded.
type smtpApiNetworkStanding struct {
	ctx       context.Context
	networkId server.Id
	now       time.Time
}

func (self *smtpApiNetworkStanding) Standing(networkId connect.Id) (sidecar.AbuseStanding, bool) {
	if server.Id(networkId) != self.networkId {
		return "", false
	}
	var standing *NetworkAbuseStanding
	found := false
	if r := server.HandleError(func() {
		standing, found = GetNetworkAbuseStanding(self.ctx, self.networkId, self.now)
	}); r != nil || !found || standing == nil {
		return "", false
	}
	return standing.Standing, true
}

func (self *smtpApiNetworkStanding) Verified(networkId connect.Id) (bool, bool) {
	return false, false
}
