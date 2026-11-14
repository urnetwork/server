package controller

// §21.8's intake (ABUSE-M5-09; D-ABUSE-5, R-ABUSE-2, R-ABUSE-6): a report from
// the form, a feed, a scanner or the mail sink becomes a case, and the signal
// it sits on decides what a machine does at once and when a person reviews it.
//
//  1. model.JoinAbuseReport ties the report to a network, by host or address,
//     headers or an edge connection id. A report that joins nothing leaves no
//     row, and the answer does not say so.
//  2. model.OpenAbuseReportCase opens the case on its clock, or counts the
//     report on the open case it coalesces with.
//  3. model.AbuseMachineAction names the machine's action for the signal, and
//     the ladder (model.NextAbuseStep) names its rung, refusing the root and
//     adding nothing to a host already killed. Each action goes through
//     ApplyAbuseAction, so it is recorded, applied, noticed and mailed on
//     §21's one path.
//
// Reporter classes separate a scanner from a stranger: only a Web Risk or feed
// hit and a keyed flagger's url reach a kill, and every other report, whatever
// it claims, reaches only review (§21.17). A report may not claim a registrar,
// a registry, law enforcement or staff: those file a notice or act through the
// staff route.
//
// A copyright notice follows §21.4's ladder: it is forwarded to the network's
// Owners and Admins through the action path's notice, as a notice_forward
// action, which denies nothing. The rung past it, a kill or a suspension, is a
// person's to take at review, since a notice's validity is a person's to judge.
// A notice that names no host, about evs content say, is forwarded the same
// way and has no rung past it (§21.9).
//
// The answer a public caller gives must not depend on AbuseIntakeResult:
// POST /abuse/report answers the same whether or not the report joined
// (plan-ABUSE-small-readings #7). Nothing here stores or logs a url's path or
// query, a reporter's or a client's address, or report text; logs and metrics
// carry no network id and no target (I-13).

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// What became of a report, for the caller's own use: a feed's run, the worker
// or a test. A public route answers nothing of it.
type AbuseIntakeResult struct {
	// false when the report named no URnetwork host, address or connection; no
	// row was written
	Joined bool
	CaseId server.Id
	// false when the report was counted on a case already open
	Opened bool
	// the §21.8 row the report sat on
	Signal model.AbuseSignal
	// the actions the machine took now, in order, each recorded under the case
	Actions []*AbuseActionResult
}

// The intake's seams, which a test replaces.
var abuseIntakeDeps = struct {
	now      func() time.Time
	settings func() *model.AbuseSettings
}{
	now: server.NowUtc,
	settings: func() *model.AbuseSettings {
		settings := model.DefaultAbuseSettings()
		model.ApplyAbuseConfig(settings, server.GetAbuseConfig())
		return settings
	},
}

// Counts reports by how intake ended: unjoined, a case with no machine action,
// a case with one, refused or failed. No label carries a name.
var abuseIntakeCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "intake_reports_total",
		Help:      "Abuse reports taken at intake, by result",
	},
	[]string{"result"},
)

// Registers the intake counter with the default registry.
func init() {
	prometheus.MustRegister(abuseIntakeCounter)
}

// The default-level reports of machine actions intake could not record.
var abuseIntakeActionFailures = newBoundedReport(abuseActionReportInterval)

// Takes one report, the path in the file header. The error is the report's
// refusal (model.ErrAbuseReportInvalid), a binary that does not link MG5
// (model.ErrAbuseUnlinked) or a store that did not answer. A machine action
// that could not be recorded leaves the case open on its clock, for review and
// for the next hit of the same listing to take again, and is no error here.
func AcceptAbuseReport(ctx context.Context, report *model.AbuseReport) (result *AbuseIntakeResult, returnErr error) {
	defer func() {
		// the store raises a failed read or write
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			result = nil
			returnErr = errors.New("abuse intake: the store did not answer")
			if glog.V(1) {
				glog.Infof("[abuse]intake failed: %v\n", r)
			}
		}
		switch {
		case errors.Is(returnErr, model.ErrAbuseReportInvalid):
			abuseIntakeCounter.WithLabelValues("refused").Inc()
		case returnErr != nil:
			abuseIntakeCounter.WithLabelValues("failed").Inc()
		case !result.Joined:
			abuseIntakeCounter.WithLabelValues("unjoined").Inc()
		case 0 < len(result.Actions):
			abuseIntakeCounter.WithLabelValues("acted").Inc()
		default:
			abuseIntakeCounter.WithLabelValues("case").Inc()
		}
	}()
	if !server.SidecarMg5Linked() {
		return nil, model.ErrAbuseUnlinked
	}
	if err := report.Validate(); err != nil {
		return nil, err
	}
	settings := abuseIntakeDeps.settings()
	now := abuseIntakeDeps.now()

	join, err := model.JoinAbuseReport(ctx, report, now, settings)
	if err != nil {
		return nil, err
	}
	if join == nil {
		return &AbuseIntakeResult{}, nil
	}
	signal := model.AbuseSignalOfReport(report.ReporterClass, report.Category, join.NamesUrl)
	reportCase, err := model.OpenAbuseReportCase(ctx, report, join, signal, now, settings)
	if err != nil {
		return nil, err
	}
	result = &AbuseIntakeResult{
		Joined: true,
		CaseId: reportCase.CaseId,
		Opened: reportCase.Opened,
		Signal: signal,
	}

	for _, kind := range abuseIntakeActionKinds(ctx, report, join, reportCase.CaseId, signal, now, settings) {
		actionResult, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
			ActionId:      server.NewId(),
			CaseId:        reportCase.CaseId,
			Kind:          kind,
			Target:        abuseIntakeActionTarget(kind, join),
			ReviewDueTime: reportCase.ReviewDueTime,
		})
		if err != nil {
			// the case stands on its clock, and the listing's next hit takes
			// the step again
			if suppressedCount, ok := abuseIntakeActionFailures.Allow(time.Now()); ok {
				glog.Infof("[abuse]intake could not record a machine action, and its case waits for review; %d more since the last report\n", suppressedCount)
			}
			break
		}
		result.Actions = append(result.Actions, actionResult)
	}
	return result, nil
}

// The actions a machine takes on a joined report now, in order: none for a
// report's rows, the forward of a copyright notice, once a case, and for a
// scanner's or a keyed flagger's kill signal the ladder's rung. A target the ladder refuses,
// the root or a platform name, takes no action.
func abuseIntakeActionKinds(
	ctx context.Context,
	report *model.AbuseReport,
	join *model.AbuseReportJoin,
	caseId server.Id,
	signal model.AbuseSignal,
	now time.Time,
	settings *model.AbuseSettings,
) []model.AbuseActionKind {
	machineKind := model.AbuseMachineAction(signal)
	copyright := report.Category == model.AbuseCategoryCopyright
	if machineKind != model.AbuseActionKindHostKill && !copyright {
		// §14.8's ladder is smtpedge's, each halt reaching §21 as its own action
		return nil
	}
	hostTarget := join.Target.Kind == model.AbuseTargetKindHost || join.Target.Kind == model.AbuseTargetKindLabel
	if !copyright && !hostTarget {
		// §21.8's machine action is a host kill, so a hit that names no host, a
		// sending domain or a bare network, waits for a person
		return nil
	}
	history := model.GetNetworkAbuseHistory(ctx, join.NetworkId)
	step, err := model.NextAbuseStep(report.Category, join.Target, history, now, &settings.AbusePolicySettings)
	if err != nil {
		return nil
	}
	if copyright {
		// the forward denies nothing, so it goes at once, whatever rung the
		// ladder is on; a kill or a suspension past it waits for a person. One
		// forward a case: a repeat counted on the open case mails no one again.
		for _, action := range history {
			if action.CaseId == caseId && action.Kind == model.AbuseActionKindNoticeForward {
				return nil
			}
		}
		return []model.AbuseActionKind{model.AbuseActionKindNoticeForward}
	}
	switch report.Category {
	case model.AbuseCategoryPhishing, model.AbuseCategoryMalware, model.AbuseCategoryCommandAndControl,
		model.AbuseCategoryExposedPanel, model.AbuseCategoryCompromise:
		// the ladder's rung: a kill, a label kill and a halt for a network
		// already struck, a suspension, or nothing for a host already killed
		return step.ActionKinds
	}
	// any other category on a kill signal takes §21.8's host kill and no more:
	// a suspension and a report to NCMEC are a person's
	for _, standing := range model.GetNetworkStandingAbuseActions(ctx, join.NetworkId, now) {
		switch standing.Kind {
		case model.AbuseActionKindHostKill, model.AbuseActionKindHostSuspend:
			if model.AbuseName(standing.Target) == model.AbuseName(join.Target.Name) {
				return nil
			}
		case model.AbuseActionKindNetworkSuspend:
			return nil
		}
	}
	return []model.AbuseActionKind{model.AbuseActionKindHostKill}
}

// What an action of kind names: the host for a host's action, and for a wider
// one the network's label under its root, or the network when the join went
// through no label.
func abuseIntakeActionTarget(kind model.AbuseActionKind, join *model.AbuseReportJoin) string {
	switch kind {
	case model.AbuseActionKindHostKill, model.AbuseActionKindHostSuspend, model.AbuseActionKindNoticeForward:
		return join.Target.Name
	}
	if join.LabelName != "" {
		return join.LabelName
	}
	return join.NetworkId.String()
}
