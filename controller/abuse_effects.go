package controller

// §21's real effects (ABUSE-M5-06, ABUSE-M9-01; D-ABUSE-1, D-ABUSE-3, D-TLS-9, D-SMTP-9, I-12, I-14, I-15):
// what an abuse action does outside its own row, each kind through its owner's
// hook (§21.16's Consumes), and what a lift returns.
//
//	host_kill        the TLS kill of the host first, since that is the ≤5 s
//	                 path (§13.9), then DNS's SuspendHost
//	label_kill       the TLS kill of every host (*), then DNS's SuspendZone
//	zone_suspend     SuspendZone alone
//	host_suspend     SuspendHost alone
//	cert_revoke      RequestTlsCertRevocation for every issued certificate of
//	                 the network, cessationOfOperation; no renewal follows a row
//	                 that is not issued (§21.6, §13.8)
//	network_suspend  plan-ABUSE-suspension-effects' fan-out (owner, QD-049):
//	                 network_abuse_state = suspended, a * kill, SuspendZone, and
//	                 every open contract closed, one subject at a time. No rev/,
//	                 nothing at evs or gkm, and no certificate revoked unless the
//	                 case orders a cert_revoke of its own. Its mail halt from M9
//	                 (ABUSE-M9-01): the send gate refuses a suspended standing
//	                 at every preamble from ServiceHost.Abuse's memory, which the
//	                 suspended row reaches within the hard deny's reload, and
//	                 the network's queues are held SuspensionMailHoldTimeout on
//	                 topic mail, then released at the lift; no smtp_halt row is
//	                 written, since one would count as a halt toward the
//	                 ladder's own rungs
//	notice_forward   nothing outside Postgres
//	limit_tighten    its network_sidecar_limit row citing the case, which its
//	                 lift clears (ABUSE-M5-19, abuse_limit_controller.go)
//	mail_halt, mail_label_halt
//	                 SMTP's halt (§14.8, ABUSE-M9-01): its Redis flag and
//	                 smtp_halt row are SMTP's ladder's, written before §21
//	                 records the halt under the halt's id, so the effect leaves a
//	                 row that is there as it is and writes one, under §21's
//	                 trigger, only where §19.10's restore lost a halt still
//	                 holding (model.EnsureAbuseSmtpHalt), holding the network's
//	                 queues until the halt ends; a lift removes the row, so every
//	                 edge drops the halt at its next reload, and releases them
//	lift             returns what the lifted action withheld, less every unit
//	                 another standing action of the network still holds, so a
//	                 lift restores exactly what its action suspended. A
//	                 revocation cannot be undone: the network's devices order
//	                 new certificates, and LiftAbuseAction's answer says so
//
// A §21 suspension is server state and never a signed Suspend: URnetwork holds
// no customer key, so nothing here touches the authorization log or rev/ (I-4).
// The TLS kill and the DNS suspension are separate systems: a DNS write that
// fails leaves the kill standing and the action owed, and AbuseActionPass
// finishes the rest. Every hook is idempotent, so an effect run twice changes
// nothing the second time. A reason holds abuse:<case_id> and no free text,
// since tls-4 shows it. While a root is restoring, DNS's hooks take no serial,
// which §12.6's restore step relies on; nothing here asks for one.
//
// A standing change prompts the network's sidecars with a core hint (§21.16),
// unless the case is barred. The sock topic's prompt is the kill's own
// {tes_<network_id>}n trigger, which sockets' KillChanged follows to its
// revocation sweep (SOCK-M5-07). The mail topic's SmtpStatus hint is written
// for what §21 changes and SMTP's ladder did not hint: a halt written back, a
// halt's lift, a suspension and its lift (model.AbuseSmtpStatusHint, QD-479),
// to the clients that sent within MailHintWindow. A hint is a prompt only: one
// that fails is counted and never fails the effect.
//
// A host is named as the case names it: under one of abuse.yml's roots, as
// shop.<label>.<root> through the network's label or an alias, or already
// relative to the network's zones, as shop. A name that is neither, or whose
// label is another network's, is refused and the action stays owed. A custom
// host (§12, M6) has no row here yet and is refused the same way.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The steps of an effect, which AbuseEffectsDeps.Step sees before each owner's
// call, with the host, subject or nothing as its detail.
const (
	AbuseEffectStepState = "abuse.effect.state"
	AbuseEffectStepTls   = "abuse.effect.tls"
	AbuseEffectStepDns   = "abuse.effect.dns"
	AbuseEffectStepCert  = "abuse.effect.cert"
	AbuseEffectStepClose = "abuse.effect.close"
	AbuseEffectStepHint  = "abuse.effect.hint"
	AbuseEffectStepMail  = "abuse.effect.mail"
)

// What every reason starts with, before the case id (§13.9's tls-4 shows it).
const abuseEffectReasonPrefix = "abuse:"

// The apex of a network's zone, as a TLS kill names it. DNS withholds no apex
// alone.
const abuseEffectApexHost = "@"

// An action the effects cannot place: no network, or a host that is not one of
// its network's. The action stays owed. The message names no target (I-13).
var ErrAbuseEffectTarget = errors.New("abuse effect target refused")

// A kind the effects do not know. The action stays owed.
var ErrAbuseEffectKindUnbuilt = errors.New("abuse effect kind not built")

// Tunables of the effects.
type AbuseEffectsSettings struct {
	// The subjects one read takes while a suspension closes contracts. 256:
	// each costs two reads and its closes, so a page stays well inside one
	// statement's time.
	SubjectPageSize int
	// The most sidecar clients one standing change hints; a client past it
	// learns at its hourly resync.
	HintClientMaxCount int
	// How long a lifted suspension, not lifted as wrong, keeps its network
	// restricted, from the suspension's start (§21.7,
	// plan-ABUSE-small-readings #4): AbuseSettings.RestrictedWindow's 30 days.
	RestrictedWindow time.Duration
	// The clients a mail hint reaches: those with a send started this long
	// before it, 24 hours, §14.8's halt window, as SMTP's ladder hints them.
	MailHintWindow time.Duration
	// How long a suspension holds the network's queues on topic mail (§21.7's
	// mail halt): 24 hours, §21.8's review clock for a machine action, so a
	// queue held at the suspension outlasts its first review; its lift
	// releases them sooner. Non-positive holds none.
	SuspensionMailHoldTimeout time.Duration
}

// The effects' numbers.
func DefaultAbuseEffectsSettings() *AbuseEffectsSettings {
	return &AbuseEffectsSettings{
		SubjectPageSize:           256,
		HintClientMaxCount:        10000,
		RestrictedWindow:          model.DefaultAbuseSettings().RestrictedWindow,
		MailHintWindow:            24 * time.Hour,
		SuspensionMailHoldTimeout: 24 * time.Hour,
	}
}

// Each owner's hook as the effects call it: the real ones from
// DefaultAbuseEffectsDeps, or a test's fakes. Every member but Step and the
// two SMTP halt hooks is required: effects built on a set missing one refuse
// every action, and a set missing an SMTP halt hook refuses the mail halts.
type AbuseEffectsDeps struct {
	// TLS-M5-13's kill switch.
	SetTlsSuspension  func(ctx context.Context, args *sidecar.TlsSuspendArgs) error
	LiftTlsSuspension func(ctx context.Context, networkId server.Id, host string) error
	// DNS-M5-14's hooks. RestoreDns with host "" returns the zone.
	SuspendZone func(ctx context.Context, networkId server.Id, reason string, until *time.Time) error
	SuspendHost func(ctx context.Context, networkId server.Id, host string) error
	RestoreDns  func(ctx context.Context, networkId server.Id, host string) error
	// TLS-M5-16's revocation request.
	RequestTlsCertRevocation func(ctx context.Context, request *model.TlsCertRevocationRequest) (int, error)
	// The network's subjects, a page at a time, and ACL-M4-03's close of one.
	NetworkSubjects       func(ctx context.Context, networkId server.Id, after server.Id, limit int) ([]server.Id, bool)
	CloseSubjectContracts func(ctx context.Context, networkId server.Id, subjectId server.Id) error
	// network_abuse_state's suspended row.
	SetNetworkSuspended func(ctx context.Context, networkId server.Id, caseId server.Id, now time.Time) error
	EndNetworkSuspended func(ctx context.Context, networkId server.Id, restrictedUntil *time.Time, now time.Time) (bool, error)
	// The network's actions that still stand, which bound what a lift frees.
	StandingActions func(ctx context.Context, networkId server.Id, now time.Time) []*model.AbuseAction
	// §12's label map: the network a label or alias names in root.
	ResolveLabel func(ctx context.Context, root string, label string) (server.Id, error)
	// abuse.yml's roots, read at each effect.
	RootNames func() []string
	// PROTO-M1-07's hint to the network's sidecar clients.
	HintClientIds func(ctx context.Context, networkId server.Id, limit int) []server.Id
	Hint          func(ctx context.Context, clientIds []server.Id, hint *sidecar.ClientHint) error
	// SMTP's halt rows (§14.8, ABUSE-M9-01): a mail halt's row written back
	// where a restore lost it, and removed at its lift. nil refuses the mail
	// halts, which stay owed.
	EnsureSmtpHalt func(ctx context.Context, halt *smtpedgeservice.SmtpHalt, now time.Time) (bool, error)
	DeleteSmtpHalt func(ctx context.Context, haltId server.Id) error
	// The SmtpStatus hint on topic mail to the network's clients that sent
	// since since. nil writes no mail hint.
	SmtpStatusHint func(ctx context.Context, networkId server.Id, kind protocol.SmtpStatusKind, retryAfter time.Duration, since time.Time, now time.Time) error
	Now            func() time.Time
	// Called before each owner's call with an AbuseEffectStep. nil in
	// production.
	Step func(step string, detail string)
}

// The real hooks: model's, on this process's Postgres and Redis.
func DefaultAbuseEffectsDeps() *AbuseEffectsDeps {
	return &AbuseEffectsDeps{
		SetTlsSuspension:         model.SetTlsSuspension,
		LiftTlsSuspension:        model.LiftTlsSuspension,
		SuspendZone:              model.SuspendZone,
		SuspendHost:              model.SuspendHost,
		RestoreDns:               model.Restore,
		RequestTlsCertRevocation: model.RequestTlsCertRevocation,
		NetworkSubjects:          model.AbuseNetworkSubjects,
		CloseSubjectContracts: func(ctx context.Context, networkId server.Id, subjectId server.Id) error {
			_, err := model.CloseAclSubjectContracts(ctx, networkId, subjectId)
			return err
		},
		SetNetworkSuspended: model.SetNetworkAbuseSuspended,
		EndNetworkSuspended: func(ctx context.Context, networkId server.Id, restrictedUntil *time.Time, now time.Time) (bool, error) {
			ended, err := model.EndNetworkAbuseSuspended(ctx, networkId, restrictedUntil, now)
			if err != nil {
				return ended, err
			}
			// the restricted row as the whole history leaves it, not the
			// lifted suspension's window alone (ABUSE-M5-07)
			model.ReevaluateNetworkAbuseState(ctx, networkId, now)
			return ended, nil
		},
		StandingActions: model.GetNetworkStandingAbuseActions,
		ResolveLabel: func(ctx context.Context, root string, label string) (server.Id, error) {
			networkId, _, err := model.ResolveNetworkLabel(ctx, root, label, false)
			return networkId, err
		},
		RootNames: func() []string {
			return server.GetAbuseConfig().RootNames()
		},
		HintClientIds:  model.AbuseNetworkHintClientIds,
		Hint:           model.SidecarHint,
		EnsureSmtpHalt: model.EnsureAbuseSmtpHalt,
		DeleteSmtpHalt: model.DeleteSmtpHalt,
		SmtpStatusHint: model.AbuseSmtpStatusHint,
		Now:            server.NowUtc,
	}
}

// Whether every required hook is set.
func (self *AbuseEffectsDeps) complete() bool {
	return self.SetTlsSuspension != nil &&
		self.LiftTlsSuspension != nil &&
		self.SuspendZone != nil &&
		self.SuspendHost != nil &&
		self.RestoreDns != nil &&
		self.RequestTlsCertRevocation != nil &&
		self.NetworkSubjects != nil &&
		self.CloseSubjectContracts != nil &&
		self.SetNetworkSuspended != nil &&
		self.EndNetworkSuspended != nil &&
		self.StandingActions != nil &&
		self.ResolveLabel != nil &&
		self.RootNames != nil &&
		self.HintClientIds != nil &&
		self.Hint != nil &&
		self.Now != nil
}

// The effects on deps and settings, the real hooks and the defaults for nil.
// What a process installs at startup (SetAbuseEffects). Effects on a set of
// hooks missing one refuse every action with ErrAbuseEffectsUnavailable.
func NewAbuseEffects(deps *AbuseEffectsDeps, settings *AbuseEffectsSettings) AbuseEffects {
	if deps == nil {
		deps = DefaultAbuseEffectsDeps()
	}
	if settings == nil {
		settings = DefaultAbuseEffectsSettings()
	}
	return &abuseEffects{
		deps:         deps,
		settings:     settings,
		hintFailures: newBoundedReport(abuseActionReportInterval),
	}
}

// Installs the real effects in this process, in a binary that links MG5:
// api's and taskworker's startup. Returns what puts the previous ones back.
func InstallAbuseEffects() (restore func()) {
	if !server.SidecarMg5Linked() {
		return func() {}
	}
	return SetAbuseEffects(NewAbuseEffects(nil, nil))
}

// See the file header. Safe for concurrent use: it holds no state between
// calls but its report limiter.
type abuseEffects struct {
	deps         *AbuseEffectsDeps
	settings     *AbuseEffectsSettings
	hintFailures *boundedReport
}

// What one action withholds outside its row. A lift returns its action's units
// less the ones another standing action holds.
type abuseEffectUnits struct {
	// the network's suspended row
	suspended bool
	// TLS kills, by host: a relative host, the apex or sidecar.TlsKillAllHosts
	tlsHosts []string
	// the zone's suspension
	dnsZone bool
	// DNS host suspensions, by relative host
	dnsHosts []string
	// every issued certificate's revocation, which no lift returns
	revoke bool
	// SMTP's halts a mail halt stands for, by its own id
	smtpHalts []*smtpedgeservice.SmtpHalt
}

// Whether the action changes what the network's sidecars can reach or how it
// stands, and so prompts them.
func (self *abuseEffectUnits) changesStanding() bool {
	return self.suspended || 0 < len(self.tlsHosts) || self.dnsZone || 0 < len(self.dnsHosts) || self.revoke ||
		0 < len(self.smtpHalts)
}

// The reason every hook records for a case's action: its case id and no free
// text.
func abuseEffectReason(caseId server.Id) string {
	return abuseEffectReasonPrefix + caseId.String()
}

// See AbuseEffects. An owner's hook that raises is an error here, so the
// request that committed the action answers and the pass retries.
func (self *abuseEffects) ApplyAbuseEffect(ctx context.Context, effect *AbuseEffect) (returnErr error) {
	if !self.deps.complete() {
		return ErrAbuseEffectsUnavailable
	}
	if effect == nil || effect.Action == nil {
		return fmt.Errorf("%w: no action", ErrAbuseEffectTarget)
	}
	defer func() {
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			returnErr = fmt.Errorf("abuse effect: %v", r)
		}
	}()
	action := effect.Action
	if action.Kind == model.AbuseActionKindLift {
		if effect.Lifted == nil {
			return fmt.Errorf("%w: a lift with no lifted action", ErrAbuseEffectTarget)
		}
		return self.lift(ctx, effect)
	}
	if action.Kind == model.AbuseActionKindLimitTighten {
		// §21.7: a network_sidecar_limit row citing the case (ABUSE-M5-19)
		return applyAbuseLimitTightenEffect(ctx, action)
	}
	units, err := self.unitsOf(ctx, action)
	if err != nil {
		return err
	}
	if !units.changesStanding() {
		// a forwarded notice does nothing outside Postgres
		return nil
	}
	networkId := *action.NetworkId
	reason := abuseEffectReason(action.CaseId)

	if units.suspended {
		self.step(AbuseEffectStepState, "")
		if err := self.deps.SetNetworkSuspended(ctx, networkId, action.CaseId, self.deps.Now()); err != nil {
			return err
		}
	}
	// the kill first: it is the ≤5 s path, and it stands whatever DNS does
	for _, host := range units.tlsHosts {
		self.step(AbuseEffectStepTls, host)
		args := &sidecar.TlsSuspendArgs{
			NetworkId: sidecar.Id(networkId),
			Host:      host,
			Reason:    reason,
		}
		if action.UntilTime != nil {
			args.Until = action.UntilTime.UTC()
		}
		if err := self.deps.SetTlsSuspension(ctx, args); err != nil {
			return err
		}
	}
	if units.dnsZone {
		self.step(AbuseEffectStepDns, "")
		if err := self.deps.SuspendZone(ctx, networkId, reason, action.UntilTime); err != nil {
			return err
		}
	}
	for _, host := range units.dnsHosts {
		self.step(AbuseEffectStepDns, host)
		if err := self.deps.SuspendHost(ctx, networkId, host); err != nil {
			return err
		}
	}
	if units.revoke {
		self.step(AbuseEffectStepCert, "")
		if _, err := self.deps.RequestTlsCertRevocation(ctx, &model.TlsCertRevocationRequest{
			NetworkId: networkId,
			Reason:    reason,
		}); err != nil {
			return err
		}
	}
	// how long the network's queues are held on topic mail: a halt written back until it ends, a suspension for
	// SuspensionMailHoldTimeout
	var mailHold time.Duration
	if 0 < len(units.smtpHalts) && action.ReviewState != model.AbuseReviewStateLifted && action.ReviewState != model.AbuseReviewStateLiftedWrong {
		// an action lifted before its effect ran writes no halt back
		if self.deps.EnsureSmtpHalt == nil {
			return ErrAbuseEffectsUnavailable
		}
		for _, halt := range units.smtpHalts {
			self.step(AbuseEffectStepMail, "")
			now := self.deps.Now()
			written, err := self.deps.EnsureSmtpHalt(ctx, halt, now)
			if err != nil {
				return err
			}
			if written {
				// SMTP's ladder hinted the halt it wrote; one a restore lost was hinted by no one
				mailHold = max(mailHold, halt.StartTime.Add(smtpedgeservice.SmtpHaltDuration).Sub(now))
			}
		}
	}
	if units.suspended {
		mailHold = max(mailHold, self.settings.SuspensionMailHoldTimeout)
		if err := self.closeNetworkContracts(ctx, networkId); err != nil {
			return err
		}
	}
	self.hint(ctx, networkId, effect.Barred)
	if 0 < mailHold {
		self.mailHint(ctx, networkId, protocol.SmtpStatusKind_SMTP_STATUS_HALTED, mailHold, effect.Barred)
	}
	return nil
}

// Returns what the lifted action withheld and no other standing action holds:
// the kill, then DNS. The suspended row ends only with the last standing
// suspension, and while one stands the row names its case.
func (self *abuseEffects) lift(ctx context.Context, effect *AbuseEffect) error {
	lifted := effect.Lifted
	if lifted.Kind == model.AbuseActionKindLimitTighten {
		// the row outlives no case (ABUSE-M5-19)
		return liftAbuseLimitTightenEffect(ctx, lifted)
	}
	units, err := self.unitsOf(ctx, lifted)
	if err != nil {
		return err
	}
	if !units.changesStanding() {
		return nil
	}
	networkId := *lifted.NetworkId
	now := self.deps.Now()

	// every unit another standing action holds stays
	var standingSuspension *model.AbuseAction
	held := &abuseEffectUnits{}
	for _, other := range self.deps.StandingActions(ctx, networkId, now) {
		if other.ActionId == lifted.ActionId || other.Kind == model.AbuseActionKindLift {
			continue
		}
		otherUnits, err := self.unitsOf(ctx, other)
		if err != nil {
			// an action that cannot be placed holds nothing: its own effect
			// never applied
			continue
		}
		if otherUnits.suspended && standingSuspension == nil {
			standingSuspension = other
		}
		held.suspended = held.suspended || otherUnits.suspended
		held.dnsZone = held.dnsZone || otherUnits.dnsZone
		held.tlsHosts = append(held.tlsHosts, otherUnits.tlsHosts...)
		held.dnsHosts = append(held.dnsHosts, otherUnits.dnsHosts...)
	}

	// whether the network's queues are released on topic mail: a halt's lift, or the last standing suspension's
	releaseMail := false
	if units.suspended {
		releaseMail = !held.suspended
		self.step(AbuseEffectStepState, "")
		if held.suspended {
			// the row names a suspension that stands
			if err := self.deps.SetNetworkSuspended(ctx, networkId, standingSuspension.CaseId, now); err != nil {
				return err
			}
		} else {
			var restrictedUntil *time.Time
			if lifted.ReviewState != model.AbuseReviewStateLiftedWrong && 0 < self.settings.RestrictedWindow {
				until := lifted.CreateTime.Add(self.settings.RestrictedWindow)
				restrictedUntil = &until
			}
			if _, err := self.deps.EndNetworkSuspended(ctx, networkId, restrictedUntil, now); err != nil {
				return err
			}
		}
	}
	for _, host := range units.tlsHosts {
		if slices.Contains(held.tlsHosts, host) {
			continue
		}
		self.step(AbuseEffectStepTls, host)
		if err := self.deps.LiftTlsSuspension(ctx, networkId, host); err != nil {
			return err
		}
	}
	if units.dnsZone && !held.dnsZone {
		self.step(AbuseEffectStepDns, "")
		if err := self.deps.RestoreDns(ctx, networkId, ""); err != nil {
			return err
		}
	}
	for _, host := range units.dnsHosts {
		if slices.Contains(held.dnsHosts, host) {
			continue
		}
		self.step(AbuseEffectStepDns, host)
		if err := self.deps.RestoreDns(ctx, networkId, host); err != nil {
			return err
		}
	}
	if 0 < len(units.smtpHalts) {
		// each halt is its own row, which no other action holds
		if self.deps.DeleteSmtpHalt == nil {
			return ErrAbuseEffectsUnavailable
		}
		for _, halt := range units.smtpHalts {
			self.step(AbuseEffectStepMail, "")
			if err := self.deps.DeleteSmtpHalt(ctx, server.Id(halt.HaltId)); err != nil {
				return err
			}
		}
		releaseMail = true
	}
	// a revocation stands: nothing returns a revoked certificate
	self.hint(ctx, networkId, effect.Barred)
	if releaseMail {
		self.mailHint(ctx, networkId, protocol.SmtpStatusKind_SMTP_STATUS_CLEAR, 0, effect.Barred)
	}
	return nil
}

// What an action of this kind withholds. A kind with no network or a host that
// is not its network's is refused, and so is a kind another task builds.
func (self *abuseEffects) unitsOf(ctx context.Context, action *model.AbuseAction) (*abuseEffectUnits, error) {
	units := &abuseEffectUnits{}
	switch action.Kind {
	case model.AbuseActionKindNoticeForward, model.AbuseActionKindLimitTighten, model.AbuseActionKindScreenAllow:
		// a tightening's row is no unit a lift of another action frees, and a screen allow is read where names
		// are screened (abuse_screen_model.go)
		return units, nil
	}
	if action.NetworkId == nil {
		return nil, fmt.Errorf("%w: no network", ErrAbuseEffectTarget)
	}
	switch action.Kind {
	case model.AbuseActionKindHostKill:
		host, err := self.relativeHost(ctx, *action.NetworkId, action.Target)
		if err != nil {
			return nil, err
		}
		units.tlsHosts = []string{host}
		if host != abuseEffectApexHost {
			// DNS withholds no apex alone, so an apex's kill is the edge's
			units.dnsHosts = []string{host}
		}
	case model.AbuseActionKindHostSuspend:
		host, err := self.relativeHost(ctx, *action.NetworkId, action.Target)
		if err != nil {
			return nil, err
		}
		if host == abuseEffectApexHost {
			return nil, fmt.Errorf("%w: an apex has no host suspension", ErrAbuseEffectTarget)
		}
		units.dnsHosts = []string{host}
	case model.AbuseActionKindLabelKill:
		units.tlsHosts = []string{sidecar.TlsKillAllHosts}
		units.dnsZone = true
	case model.AbuseActionKindZoneSuspend:
		units.dnsZone = true
	case model.AbuseActionKindCertRevoke:
		units.revoke = true
	case model.AbuseActionKindNetworkSuspend:
		units.suspended = true
		units.tlsHosts = []string{sidecar.TlsKillAllHosts}
		units.dnsZone = true
	case model.AbuseActionKindMailHalt, model.AbuseActionKindMailLabelHalt:
		halt, err := abuseMailHaltOf(action)
		if err != nil {
			return nil, err
		}
		units.smtpHalts = []*smtpedgeservice.SmtpHalt{halt}
	default:
		return nil, fmt.Errorf("%w: %s", ErrAbuseEffectKindUnbuilt, action.Kind)
	}
	return units, nil
}

// The host relative to its network's zones, as the kill switch and SuspendHost
// take it: shop for shop.<label>.<root>, the apex for <label>.<root>. A single
// label is taken as relative already. A name under a root must name this
// network by its label or an alias; anything else is refused.
func (self *abuseEffects) relativeHost(ctx context.Context, networkId server.Id, target string) (string, error) {
	name := strings.TrimRight(strings.ToLower(strings.TrimSpace(target)), ".")
	if name == "" || strings.Contains(name, "*") {
		return "", fmt.Errorf("%w: no host", ErrAbuseEffectTarget)
	}
	if name == abuseEffectApexHost {
		return name, nil
	}
	roots := slices.Clone(self.deps.RootNames())
	// the longest root first, so a root under another root is matched whole
	slices.SortFunc(roots, func(a string, b string) int {
		return len(b) - len(a)
	})
	for _, root := range roots {
		root = strings.TrimRight(strings.ToLower(strings.TrimSpace(root)), ".")
		if root == "" {
			continue
		}
		under, found := strings.CutSuffix(name, "."+root)
		if !found {
			continue
		}
		labels := strings.Split(under, ".")
		label := labels[len(labels)-1]
		resolvedId, err := self.deps.ResolveLabel(ctx, root, label)
		if err != nil {
			if errors.Is(err, sidecar.ErrNetworkLabelUnknown) {
				return "", fmt.Errorf("%w: a host under no network's label", ErrAbuseEffectTarget)
			}
			return "", err
		}
		if resolvedId != networkId {
			return "", fmt.Errorf("%w: a host of another network", ErrAbuseEffectTarget)
		}
		if len(labels) == 1 {
			return abuseEffectApexHost, nil
		}
		return strings.Join(labels[:len(labels)-1], "."), nil
	}
	if strings.Contains(name, ".") {
		// neither under a root nor one relative label: a custom host is M6's
		return "", fmt.Errorf("%w: a host under no root", ErrAbuseEffectTarget)
	}
	return name, nil
}

// Closes every open contract of the network, one subject at a time (§8.6). A
// failure stops at its subject, and the retry starts over: each close is
// idempotent.
func (self *abuseEffects) closeNetworkContracts(ctx context.Context, networkId server.Id) error {
	after := server.Id{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		subjectIds, more := self.deps.NetworkSubjects(ctx, networkId, after, self.settings.SubjectPageSize)
		for _, subjectId := range subjectIds {
			self.step(AbuseEffectStepClose, subjectId.String())
			if err := self.deps.CloseSubjectContracts(ctx, networkId, subjectId); err != nil {
				return err
			}
			after = subjectId
		}
		if !more || len(subjectIds) == 0 {
			return nil
		}
	}
}

// Prompts the network's sidecars on the core topic, at api's clock (§6.8). A
// barred case prompts no one. A failure is reported and nothing more: a
// sidecar that misses the prompt learns at its hourly resync.
func (self *abuseEffects) hint(ctx context.Context, networkId server.Id, barred bool) {
	if barred {
		return
	}
	self.step(AbuseEffectStepHint, "")
	var err error
	if r := server.HandleError(func() {
		clientIds := self.deps.HintClientIds(ctx, networkId, self.settings.HintClientMaxCount)
		if len(clientIds) == 0 {
			return
		}
		err = self.deps.Hint(ctx, clientIds, &sidecar.ClientHint{
			Topic:   sidecar.PushTopicCore,
			Version: uint64(self.deps.Now().UnixMicro()),
		})
	}); r != nil {
		err = fmt.Errorf("%v", r)
	}
	if err == nil {
		return
	}
	abuseActionStepCounter.WithLabelValues("hint", "failed").Inc()
	if suppressedCount, ok := self.hintFailures.Allow(time.Now()); ok {
		glog.Infof("[abuse]a standing hint was not written, and its sidecars learn at their resync; %d more since the last report\n", suppressedCount)
	}
}

// Holds or releases the network's queues on topic mail, as hint does the core prompt: a barred case prompts no one,
// a process with no mail hook writes none, and a failure is counted and reported and nothing more.
func (self *abuseEffects) mailHint(ctx context.Context, networkId server.Id, kind protocol.SmtpStatusKind, retryAfter time.Duration, barred bool) {
	if barred || self.deps.SmtpStatusHint == nil {
		return
	}
	self.step(AbuseEffectStepHint, "mail")
	now := self.deps.Now()
	var err error
	if r := server.HandleError(func() {
		err = self.deps.SmtpStatusHint(ctx, networkId, kind, retryAfter, now.Add(-self.settings.MailHintWindow), now)
	}); r != nil {
		err = fmt.Errorf("%v", r)
	}
	if err == nil {
		return
	}
	abuseActionStepCounter.WithLabelValues("hint", "failed").Inc()
	if suppressedCount, ok := self.hintFailures.Allow(time.Now()); ok {
		glog.Infof("[abuse]a mail hint was not written, and its sidecars meet the change at their next preamble; %d more since the last report\n", suppressedCount)
	}
}

// Calls the step hook, if any.
func (self *abuseEffects) step(step string, detail string) {
	if self.deps.Step != nil {
		self.deps.Step(step, detail)
	}
}
