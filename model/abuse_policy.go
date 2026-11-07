package model

// The policy core of §21: categories, signals, machine actions and the strike
// ladders, as pure functions. Nothing here reads a store or a clock, so every rule
// runs on an injected time; the review clocks are in abuse_clock.go.
//
// Controls act only on names, metadata, rates and reports, at the smallest unit and
// never the root (D-ABUSE-1). Machines act only on §21.8's signals, and an action
// holds until a person reviews it, so nothing here lifts anything and no answer
// here depends on who a report came from except through a signal.

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// A §21.4 row, as abuse_case and abuse_action store it.
type AbuseCategory string

const (
	AbuseCategorySpam              AbuseCategory = "spam"
	AbuseCategoryPhishing          AbuseCategory = "phishing"
	AbuseCategoryMalware           AbuseCategory = "malware"
	AbuseCategoryCommandAndControl AbuseCategory = "command_and_control"
	AbuseCategoryCopyright         AbuseCategory = "copyright"
	AbuseCategoryExposedPanel      AbuseCategory = "exposed_panel"
	AbuseCategoryCompromise        AbuseCategory = "compromise"
	AbuseCategoryCsam              AbuseCategory = "csam"
	AbuseCategoryThreatToLife      AbuseCategory = "threat_to_life"
	// a name ScreenName sent to review, with no ladder (§21.6)
	AbuseCategoryBrand AbuseCategory = "brand"
	// a case with no §21.4 row, which only a person reads
	AbuseCategoryOther AbuseCategory = "other"
)

// Phishing, malware and command-and-control share one §21.4 row, so one ladder.
func abuseThreatCategory(category AbuseCategory) bool {
	switch category {
	case AbuseCategoryPhishing, AbuseCategoryMalware, AbuseCategoryCommandAndControl:
		return true
	}
	return false
}

// A row of §21.8's table. A signal decides machine action and review clock and is
// not stored: a case keeps its category and reporter class.
type AbuseSignal string

const (
	// §14.8's joins. A low join never halts, and §21 reviews it.
	AbuseSignalMailJoinHigh   AbuseSignal = "mail_join_high"
	AbuseSignalMailJoinMedium AbuseSignal = "mail_join_medium"
	AbuseSignalMailJoinLow    AbuseSignal = "mail_join_low"
	AbuseSignalWebRiskHit     AbuseSignal = "web_risk_hit"
	AbuseSignalFeedHit        AbuseSignal = "feed_hit"
	// a trusted flagger's report naming a url; any other report of theirs is a report
	AbuseSignalTrustedFlaggerUrl AbuseSignal = "trusted_flagger_url"
	// Notices page at any hour. These come only from a filed notice, never from
	// AbuseSignalOfReport, because the form is public and a report that could reach
	// one could page the rota, or with a url kill any host (plan-ABUSE-notice-intake).
	AbuseSignalRegistrarNotice         AbuseSignal = "registrar_notice"
	AbuseSignalRegistryNotice          AbuseSignal = "registry_notice"
	AbuseSignalRegistrarUrl            AbuseSignal = "registrar_url"
	AbuseSignalRegistryUrl             AbuseSignal = "registry_url"
	AbuseSignalLawEnforcementEmergency AbuseSignal = "law_enforcement_emergency"
	AbuseSignalEuRemovalOrder          AbuseSignal = "eu_removal_order"
	AbuseSignalProductionOrder         AbuseSignal = "production_order"
	// a report of phishing, malware or command-and-control naming a url
	AbuseSignalThreatReport AbuseSignal = "threat_report"
	// §21.8's last row
	AbuseSignalReport AbuseSignal = "report"
)

// Who a case came from, as abuse_case stores it. Classes separate a feed from a
// stranger, so a false report only ever reaches review (§21.17).
type AbuseReporterClass string

const (
	// the public form
	AbuseReporterClassPublic AbuseReporterClass = "public"
	// abuse@ and postmaster@
	AbuseReporterClassRoleMail AbuseReporterClass = "role_mail"
	// §14.8's feedback and aggregate reports
	AbuseReporterClassMailReport     AbuseReporterClass = "mail_report"
	AbuseReporterClassWebRisk        AbuseReporterClass = "web_risk"
	AbuseReporterClassFeed           AbuseReporterClass = "feed"
	AbuseReporterClassCtWatch        AbuseReporterClass = "ct_watch"
	AbuseReporterClassSearchConsole  AbuseReporterClass = "search_console"
	AbuseReporterClassTrustedFlagger AbuseReporterClass = "trusted_flagger"
	AbuseReporterClassRegistrar      AbuseReporterClass = "registrar"
	AbuseReporterClassRegistry       AbuseReporterClass = "registry"
	AbuseReporterClassLawEnforcement AbuseReporterClass = "law_enforcement"
	AbuseReporterClassStaff          AbuseReporterClass = "staff"
)

// What an action does, as abuse_action stores it.
type AbuseActionKind string

const (
	// no action
	AbuseActionKindNone AbuseActionKind = ""
	// §14.8's ladder decides. Not a row: smtpedge halts, and each halt reaches §21 as
	// a mail_halt or mail_label_halt.
	AbuseActionKindMailLadder AbuseActionKind = "mail_ladder"
	// a copyright notice passed to the network's Owners and Admins
	AbuseActionKindNoticeForward  AbuseActionKind = "notice_forward"
	AbuseActionKindHostKill       AbuseActionKind = "host_kill"
	AbuseActionKindLabelKill      AbuseActionKind = "label_kill"
	AbuseActionKindCertRevoke     AbuseActionKind = "cert_revoke"
	AbuseActionKindZoneSuspend    AbuseActionKind = "zone_suspend"
	AbuseActionKindHostSuspend    AbuseActionKind = "host_suspend"
	AbuseActionKindNetworkSuspend AbuseActionKind = "network_suspend"
	// a halt of the whole network (§14.8)
	AbuseActionKindMailHalt AbuseActionKind = "mail_halt"
	// a halt of one burner label, which counts toward no ladder here (reading 4)
	AbuseActionKindMailLabelHalt AbuseActionKind = "mail_label_halt"
	// a row of its own, naming the lifted action (reading 11)
	AbuseActionKindLift AbuseActionKind = "lift"
)

// How an action stands with review, as abuse_action stores it.
type AbuseReviewState string

const (
	AbuseReviewStatePending AbuseReviewState = "pending"
	AbuseReviewStateUpheld  AbuseReviewState = "upheld"
	// lifted and still a strike: the network fixed what the action was for
	AbuseReviewStateLifted AbuseReviewState = "lifted"
	// lifted because it was wrong, which takes it out of every count (readings 4, 5)
	AbuseReviewStateLiftedWrong AbuseReviewState = "lifted_wrong"
)

// A network's §21.7 state. Takes AbuseStanding's values, which ABUSE-M5-02 declares
// in the sidecar module once server requires it.
type AbuseStanding string

const (
	AbuseStandingProbation  AbuseStanding = "probation"
	AbuseStandingStandard   AbuseStanding = "standard"
	AbuseStandingTrusted    AbuseStanding = "trusted"
	AbuseStandingRestricted AbuseStanding = "restricted"
	AbuseStandingSuspended  AbuseStanding = "suspended"
)

// A network's tier for §21.8's clocks. Takes NetworkTier's values, which QUOTA-M1-01
// declares, until callers can pass that type.
type AbuseTier string

const (
	// Community
	AbuseTierFree AbuseTier = "free"
	AbuseTierPro  AbuseTier = "pro"
	AbuseTierTeam AbuseTier = "team"
)

// The unit a case names, as abuse_case stores it (§21.4's principle 1).
type AbuseTargetKind string

const (
	AbuseTargetKindHost          AbuseTargetKind = "host"
	AbuseTargetKindSendingDomain AbuseTargetKind = "sending_domain"
	AbuseTargetKindLabel         AbuseTargetKind = "label"
	AbuseTargetKindNetwork       AbuseTargetKind = "network"
)

// What a step would act on.
type AbuseTarget struct {
	Kind AbuseTargetKind
	Name string
}

// One recorded action of a network, as far as a ladder reads it.
type AbuseHistoryAction struct {
	CaseId   server.Id
	Category AbuseCategory
	Kind     AbuseActionKind
	// the action's target, which for a kill is the host
	Target      string
	ReviewState AbuseReviewState
	CreateTime  time.Time
	// when the action ends by itself; zero when only a lift ends it
	UntilTime time.Time
}

// An action holds until a person lifts it or its own until passes.
func (self AbuseHistoryAction) stands(now time.Time) bool {
	if self.ReviewState == AbuseReviewStateLifted || self.ReviewState == AbuseReviewStateLiftedWrong {
		return false
	}
	return self.UntilTime.IsZero() || now.Before(self.UntilTime)
}

// What a ladder says to do next. An empty step acts on nothing, and the case waits
// for review.
type AbuseStep struct {
	// each takes its own abuse_action row, in this order
	ActionKinds []AbuseActionKind
	// the action protects the network and strikes nothing (§21.4)
	Protective bool
	// a person files the report that follows (§21.9); nothing here files one
	Report bool
}

// A target the ladders may not act on, by name or for want of one. No action follows,
// and the error names no target, since it may reach a log (I-13).
var ErrAbuseTargetRefused = errors.New("abuse target refused")

// A category with no §21.4 row. No action follows.
var ErrAbuseCategoryUnknown = errors.New("abuse category unknown")

// Tunables of the ladders and the review clocks, with §21.4's and §21.8's numbers as
// defaults. ABUSE-M5-03's AbuseSettings takes these fields when it lands.
type AbusePolicySettings struct {
	// How long a phishing, malware or command-and-control case stays a strike.
	// §21.4's 30 days.
	StrikeWindow time.Duration
	// The strike that takes a label kill and a mail halt: §21.4's 2nd. Non-positive
	// disables the rung, so a zero settings struct never escalates.
	LabelKillStrike int
	// The strike that suspends: §21.4's 3rd. Non-positive disables the rung.
	SuspendStrike int
	// How long a network halt counts. §14.8's and §21.4's 30 days.
	HaltWindow time.Duration
	// Halts inside the window that suspend: §14.8's 3. Non-positive disables the rung.
	SuspendHalts int
	// How long a valid copyright notice counts toward a kill. §21.4's 30 days.
	CopyrightKillWindow time.Duration
	// The valid notice that kills the host: §21.4's 2nd. Non-positive disables the rung.
	CopyrightKillNotice int
	// How long an upheld copyright case counts toward a suspension, in calendar
	// months. §21.4's 12.
	CopyrightUpheldMonths int
	// Upheld copyright cases inside the window that suspend: §21.4's 3, a repeat
	// infringer under 17 U.S.C. §512(i). Non-positive disables the rung.
	CopyrightSuspendUpheld int

	// Elapsed time to handle a paged notice, at any hour. §21.8's 1 h.
	NoticeReview time.Duration
	// Elapsed time to review a machine action. §21.8's 24 h.
	MachineReview time.Duration
	// Open hours, not elapsed ones, to review a report on the short clock. §21.8's 4.
	ReportOpenReview time.Duration
	// Elapsed time to review a report against a paid network. §21.8's 72 h.
	PaidReview time.Duration
	// Elapsed time to review a report against a Community network. §21.8's 7 days.
	CommunityReview time.Duration

	// The roots, such as <root>. A target equal to one is refused, and names under
	// one are users'. Empty refuses every target, because a ladder that cannot tell
	// the root from a host must not act on either.
	RootNames []string
	// Platform names and domains. A target equal to one, or under one, is refused.
	// Empty refuses every target, as for RootNames.
	PlatformNames []string
}

// Neither name list has a default: both come from the environment's config, and
// without them the ladders refuse every target.
func DefaultAbusePolicySettings() *AbusePolicySettings {
	return &AbusePolicySettings{
		StrikeWindow:           30 * 24 * time.Hour,
		LabelKillStrike:        2,
		SuspendStrike:          3,
		HaltWindow:             30 * 24 * time.Hour,
		SuspendHalts:           3,
		CopyrightKillWindow:    30 * 24 * time.Hour,
		CopyrightKillNotice:    2,
		CopyrightUpheldMonths:  12,
		CopyrightSuspendUpheld: 3,
		NoticeReview:           time.Hour,
		MachineReview:          24 * time.Hour,
		ReportOpenReview:       4 * time.Hour,
		PaidReview:             72 * time.Hour,
		CommunityReview:        7 * 24 * time.Hour,
	}
}

// A name as the policy compares it: case, spaces and a trailing dot are no difference.
func abuseName(name string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(name)), ".")
}

// The §21.8 row a report sits on, and the one way from a reporter class to a signal.
// Only our own scanners and a keyed flagger's url act. Every other class, a claimed
// registrar's included, lands on a report row, whatever its category, so a report
// never reaches a kill or a page by another path. §14.8's joins and filed notices
// take their signals directly, since neither is a report's to claim.
//
// namesUrl is whether the report names a url whose host the join resolved.
func AbuseSignalOfReport(class AbuseReporterClass, category AbuseCategory, namesUrl bool) AbuseSignal {
	switch class {
	case AbuseReporterClassWebRisk:
		return AbuseSignalWebRiskHit
	case AbuseReporterClassFeed:
		return AbuseSignalFeedHit
	case AbuseReporterClassTrustedFlagger:
		if namesUrl {
			return AbuseSignalTrustedFlaggerUrl
		}
	}
	if namesUrl && abuseThreatCategory(category) {
		return AbuseSignalThreatReport
	}
	return AbuseSignalReport
}

// What a machine does on a signal, at once and before any review (§21.8). Anything but
// these rows acts on nothing, an unknown signal included. The answer names no target,
// so no caller acts on it alone: a target goes through NextAbuseStep, which refuses
// the root, and on a threat category gives the rung, climbing past a host kill for a
// network already struck and adding nothing to a host already killed.
func AbuseMachineAction(signal AbuseSignal) AbuseActionKind {
	switch signal {
	case AbuseSignalMailJoinHigh, AbuseSignalMailJoinMedium:
		return AbuseActionKindMailLadder
	case AbuseSignalWebRiskHit, AbuseSignalFeedHit, AbuseSignalTrustedFlaggerUrl,
		AbuseSignalRegistrarUrl, AbuseSignalRegistryUrl:
		return AbuseActionKindHostKill
	}
	return AbuseActionKindNone
}

// Walks §21.4 for one network: what follows, given what is recorded. history holds the
// network's actions, without the one being decided, so a phishing network with one
// strike recorded answers the 2nd rung. The ladders say what is due and not who may
// do it: a caller holding a report takes a step only after a person's review, since
// AbuseMachineAction answers none for a report.
//
// An action counts unless review lifted it as wrong (readings 4, 5). A threat strike
// and a copyright notice are one per case, however many rows a step took, while each
// network halt counts on its own, since one open case can take several. A protective
// action strikes nothing, a compromise's halt included. A suspension answered what
// came before it, so once lifted those actions still count and only a newer one
// suspends again; a threat case is itself that newer action.
//
// Act on the smallest unit, never the root (§21.4, R-ABUSE-3): a root, a platform name
// or anything under one, a wildcard, an empty name and an unknown kind take no action
// and answer ErrAbuseTargetRefused. So does every target while either name list is
// empty, because a ladder that cannot tell the root from a host must act on neither.
func NextAbuseStep(
	category AbuseCategory,
	target AbuseTarget,
	history []AbuseHistoryAction,
	now time.Time,
	settings *AbusePolicySettings,
) (AbuseStep, error) {
	actionable := func() bool {
		switch target.Kind {
		case AbuseTargetKindHost, AbuseTargetKindSendingDomain, AbuseTargetKindLabel, AbuseTargetKindNetwork:
		default:
			return false
		}
		if len(settings.RootNames) == 0 || len(settings.PlatformNames) == 0 {
			return false
		}
		name := abuseName(target.Name)
		if name == "" || strings.Contains(name, "*") {
			return false
		}
		for _, rootName := range settings.RootNames {
			if name == abuseName(rootName) {
				return false
			}
		}
		for _, platformName := range settings.PlatformNames {
			platformName = abuseName(platformName)
			if name == platformName || strings.HasSuffix(name, "."+platformName) {
				return false
			}
		}
		return true
	}
	if !actionable() {
		return AbuseStep{}, ErrAbuseTargetRefused
	}

	counts := func(action AbuseHistoryAction, since time.Time) bool {
		return action.Kind != AbuseActionKindLift &&
			action.ReviewState != AbuseReviewStateLiftedWrong &&
			!action.CreateTime.Before(since)
	}
	caseCount := func(match func(action AbuseHistoryAction) bool) int {
		caseIds := map[server.Id]bool{}
		for _, action := range history {
			if match(action) {
				caseIds[action.CaseId] = true
			}
		}
		return len(caseIds)
	}
	// A suspension holds until reviewed, however old, and one is enough.
	suspended := slices.ContainsFunc(history, func(action AbuseHistoryAction) bool {
		return action.Kind == AbuseActionKindNetworkSuspend && action.stands(now)
	})
	var suspensionTime time.Time
	for _, action := range history {
		if action.Kind == AbuseActionKindNetworkSuspend && action.CreateTime.After(suspensionTime) {
			suspensionTime = action.CreateTime
		}
	}
	// A kill that stands already holds its host, so a repeat of the same hit is a
	// stale listing and no second incident to escalate on.
	killed := slices.ContainsFunc(history, func(action AbuseHistoryAction) bool {
		return (action.Kind == AbuseActionKindHostKill || action.Kind == AbuseActionKindHostSuspend) &&
			action.stands(now) &&
			abuseName(action.Target) == abuseName(target.Name)
	})
	hostTarget := target.Kind == AbuseTargetKindHost || target.Kind == AbuseTargetKindLabel
	mailTarget := target.Kind == AbuseTargetKindSendingDomain
	suspendStep := AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindNetworkSuspend}}
	killStep := AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindHostKill}}
	haltStep := AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindMailHalt}}

	switch category {
	case AbuseCategorySpam:
		since := now.Add(-settings.HaltWindow)
		haltCount := 0
		newHalt := false
		for _, action := range history {
			if action.Kind == AbuseActionKindMailHalt &&
				action.Category != AbuseCategoryCompromise &&
				counts(action, since) {
				haltCount += 1
				newHalt = newHalt || action.CreateTime.After(suspensionTime)
			}
		}
		if 0 < settings.SuspendHalts && settings.SuspendHalts <= haltCount && newHalt && !suspended {
			return suspendStep, nil
		}
		return AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindMailLadder}}, nil

	case AbuseCategoryPhishing, AbuseCategoryMalware, AbuseCategoryCommandAndControl:
		if suspended || killed {
			return AbuseStep{}, nil
		}
		// one kind for each way a rung acts, so a revocation following a kill strikes
		// nothing of its own
		strikes := func(kind AbuseActionKind) bool {
			switch kind {
			case AbuseActionKindHostKill, AbuseActionKindHostSuspend,
				AbuseActionKindLabelKill, AbuseActionKindZoneSuspend,
				AbuseActionKindNetworkSuspend, AbuseActionKindMailHalt:
				return true
			}
			return false
		}
		since := now.Add(-settings.StrikeWindow)
		strikeNumber := 1 + caseCount(func(action AbuseHistoryAction) bool {
			return abuseThreatCategory(action.Category) && strikes(action.Kind) && counts(action, since)
		})
		switch {
		case 0 < settings.SuspendStrike && settings.SuspendStrike <= strikeNumber:
			return suspendStep, nil
		case 0 < settings.LabelKillStrike && settings.LabelKillStrike <= strikeNumber:
			return AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindLabelKill, AbuseActionKindMailHalt}}, nil
		case hostTarget:
			return killStep, nil
		case mailTarget:
			return haltStep, nil
		}
		// a bare network names no host to kill and no mail to halt
		return AbuseStep{}, nil

	case AbuseCategoryCopyright:
		upheldSince := now.UTC().AddDate(0, -settings.CopyrightUpheldMonths, 0)
		upheld := func(action AbuseHistoryAction) bool {
			return action.Category == AbuseCategoryCopyright &&
				action.ReviewState == AbuseReviewStateUpheld &&
				counts(action, upheldSince)
		}
		newUpheld := slices.ContainsFunc(history, func(action AbuseHistoryAction) bool {
			return upheld(action) && action.CreateTime.After(suspensionTime)
		})
		upheldCount := caseCount(upheld)
		if 0 < settings.CopyrightSuspendUpheld && settings.CopyrightSuspendUpheld <= upheldCount && newUpheld && !suspended {
			return suspendStep, nil
		}
		since := now.Add(-settings.CopyrightKillWindow)
		noticeNumber := 1 + caseCount(func(action AbuseHistoryAction) bool {
			return action.Category == AbuseCategoryCopyright && counts(action, since)
		})
		if 0 < settings.CopyrightKillNotice && settings.CopyrightKillNotice <= noticeNumber && hostTarget && !killed {
			return killStep, nil
		}
		return AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindNoticeForward}}, nil

	case AbuseCategoryExposedPanel, AbuseCategoryCompromise:
		// a compromise's halt or kill holds until a person lifts it, once the device and
		// its DKIM selector are revoked
		switch {
		case hostTarget && !killed:
			return AbuseStep{ActionKinds: killStep.ActionKinds, Protective: true}, nil
		case mailTarget && category == AbuseCategoryCompromise:
			return AbuseStep{ActionKinds: haltStep.ActionKinds, Protective: true}, nil
		}
		return AbuseStep{}, nil

	case AbuseCategoryCsam, AbuseCategoryThreatToLife:
		if suspended {
			return AbuseStep{Report: true}, nil
		}
		return AbuseStep{ActionKinds: suspendStep.ActionKinds, Report: true}, nil

	case AbuseCategoryBrand, AbuseCategoryOther:
		return AbuseStep{}, nil
	}
	return AbuseStep{}, ErrAbuseCategoryUnknown
}
