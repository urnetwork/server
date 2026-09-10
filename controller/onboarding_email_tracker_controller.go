package controller

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const (
	onboardingEmailTrackerSyncInterval = 15 * time.Minute
	onboardingEmailTrackerTaskKey      = "onboarding_email_tracker_sync"
)

var onboardingEmailTrackerNetworks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "email_tracker_networks",
	Help:      "Distinct networks in the rolling 28-day onboarding email send cohort, by bounded flow step and outcome.",
}, []string{"step", "outcome"})

var onboardingEmailTrackerSnapshotTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "email_tracker_snapshot_timestamp_seconds",
	Help:      "Unix time when this Taskworker completed its onboarding email tracker snapshot; dashboards must select one fresh process snapshot.",
})

func init() {
	prometheus.MustRegister(
		onboardingEmailTrackerNetworks,
		onboardingEmailTrackerSnapshotTimestamp,
	)
}

type OnboardingEmailTrackerSyncArgs struct {
	// Optional inclusive send-day bounds. The recurring task uses the rolling
	// 28-day window ending today.
	From time.Time `json:"from,omitempty"`
	To   time.Time `json:"to,omitempty"`
}

type OnboardingEmailTrackerSyncResult struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	Rows             int64     `json:"rows"`
	DurationSeconds  float64   `json:"duration_seconds"`
	SnapshotUnixTime int64     `json:"snapshot_unix_time"`
}

func ScheduleOnboardingEmailTrackerSync(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		OnboardingEmailTrackerSync,
		&OnboardingEmailTrackerSyncArgs{},
		clientSession,
		task.RunOnce(onboardingEmailTrackerTaskKey),
		task.RunAt(at),
		task.MaxTime(10*time.Minute),
	)
}

func onboardingEmailTrackerWindow(now time.Time, from time.Time, to time.Time) (time.Time, time.Time, error) {
	if to.IsZero() {
		to = onboarding.CohortDay(now)
	}
	if from.IsZero() {
		from = onboarding.CohortDay(to).Add(-time.Duration(onboarding.EmailTrackerRebuildDays-1) * 24 * time.Hour)
	}
	from = onboarding.CohortDay(from)
	to = onboarding.CohortDay(to)
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("onboarding email tracker window is empty")
	}
	return from, to, nil
}

func onboardingEmailTrackerMetricWindow(now time.Time) (time.Time, time.Time) {
	to := onboarding.CohortDay(now)
	from := to.Add(-time.Duration(onboarding.EmailTrackerWindowDays-1) * 24 * time.Hour)
	return from, to
}

var onboardingEmailTrackerGaugeLock sync.Mutex

func setOnboardingEmailTrackerGauges(totals map[string]*model.OnboardingEmailTrackerTotals, refreshedAt time.Time) {
	onboardingEmailTrackerGaugeLock.Lock()
	defer onboardingEmailTrackerGaugeLock.Unlock()
	onboardingEmailTrackerNetworks.Reset()
	for _, step := range onboarding.FlowSteps() {
		values := totals[step]
		if values == nil {
			values = &model.OnboardingEmailTrackerTotals{Step: step}
		}
		byOutcome := values.Outcomes()
		for _, outcome := range onboarding.EmailTrackerOutcomes() {
			onboardingEmailTrackerNetworks.WithLabelValues(step, outcome).Set(float64(byOutcome[outcome]))
		}
	}
	onboardingEmailTrackerSnapshotTimestamp.Set(float64(refreshedAt.Unix()))
}

// OnboardingEmailTrackerSync refreshes the durable daily rows and replaces the
// executing taskworker's complete Prometheus snapshot.
func OnboardingEmailTrackerSync(args *OnboardingEmailTrackerSyncArgs, clientSession *session.ClientSession) (*OnboardingEmailTrackerSyncResult, error) {
	startedAt := server.NowUtc()
	if args == nil {
		args = &OnboardingEmailTrackerSyncArgs{}
	}
	from, to, err := onboardingEmailTrackerWindow(startedAt, args.From, args.To)
	if err != nil {
		return nil, err
	}
	rows, err := model.RebuildOnboardingEmailTracker(clientSession.Ctx, from, to, startedAt)
	if err != nil {
		return nil, err
	}
	// The exported projection is always the same rolling window, even when an
	// operator invokes a historical rebuild with explicit bounds.
	metricFrom, metricTo := onboardingEmailTrackerMetricWindow(startedAt)
	totals := model.SumOnboardingEmailTracker(clientSession.Ctx, metricFrom, metricTo)
	completedAt := server.NowUtc()
	setOnboardingEmailTrackerGauges(totals, completedAt)
	return &OnboardingEmailTrackerSyncResult{
		From: from, To: to, Rows: rows,
		DurationSeconds:  completedAt.Sub(startedAt).Seconds(),
		SnapshotUnixTime: completedAt.Unix(),
	}, nil
}

func OnboardingEmailTrackerSyncPost(
	args *OnboardingEmailTrackerSyncArgs,
	result *OnboardingEmailTrackerSyncResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleOnboardingEmailTrackerSync(clientSession, tx, server.NowUtc().Add(onboardingEmailTrackerSyncInterval))
	return nil
}

type AdminOnboardingEmailTrackerArgs struct {
	From       string
	To         string
	Step       string
	Experiment string
	Platform   string
	Path       string
	Cursor     string
	Limit      int
}

type AdminOnboardingEmailTrackerDefinitions struct {
	Counts                 string   `json:"counts"`
	WindowDays             int      `json:"dashboard_window_days"`
	EngagementWindowDays   int      `json:"engagement_window_days"`
	DeliveryCorrectionDays int      `json:"delivery_correction_days"`
	EngagedIncludes        []string `json:"engaged_includes"`
	EngagedExcludes        []string `json:"engaged_excludes"`
	LegacyAttribution      string   `json:"legacy_attribution"`
}

type AdminOnboardingEmailTrackerResult struct {
	Rows        []*model.OnboardingEmailTrackerRow     `json:"rows"`
	NextCursor  *string                                `json:"next_cursor"`
	MinNetworks int                                    `json:"min_networks"`
	Definitions AdminOnboardingEmailTrackerDefinitions `json:"definitions"`
}

// AdminOnboardingEmailTracker exposes privacy-safe daily aggregates for the
// onboarding research loop. It shares the existing admin bearer and never
// returns a network, address, message or session identifier.
func AdminOnboardingEmailTracker(args *AdminOnboardingEmailTrackerArgs, clientSession *session.ClientSession) (*AdminOnboardingEmailTrackerResult, error) {
	const endpoint = "email_tracker"
	if err := requireOnboardingAdmin(clientSession, endpoint); err != nil {
		return nil, err
	}
	bad := func(err error) (*AdminOnboardingEmailTrackerResult, error) {
		onboardingAdminRequestsTotal.WithLabelValues(endpoint, "bad_request").Inc()
		return nil, err
	}
	if args == nil {
		return bad(fmt.Errorf("%d query is required.", http.StatusBadRequest))
	}
	from, err := parseCohortDay(args.From, "from")
	if err != nil {
		return bad(err)
	}
	to, err := parseCohortDay(args.To, "to")
	if err != nil {
		return bad(err)
	}
	if to.Before(from) {
		return bad(fmt.Errorf("%d to is before from.", http.StatusBadRequest))
	}
	if onboarding.EmailTrackerMaxRangeDays <= int(to.Sub(from)/(24*time.Hour)) {
		return bad(fmt.Errorf("%d range exceeds %d days.", http.StatusBadRequest, onboarding.EmailTrackerMaxRangeDays))
	}
	step := strings.TrimSpace(args.Step)
	if step != "" && !onboarding.IsFlowStep(step) {
		return bad(fmt.Errorf("%d step must be e1, e2, e3, e4, or e5.", http.StatusBadRequest))
	}
	path := strings.TrimSpace(args.Path)
	if path != "" && path != onboarding.PathA && path != onboarding.PathB {
		return bad(fmt.Errorf("%d path must be A or B.", http.StatusBadRequest))
	}
	filter := model.OnboardingEmailTrackerFilter{
		From: from, To: to, Step: step,
		Experiment: strings.TrimSpace(args.Experiment),
		Platform:   strings.TrimSpace(args.Platform), Path: path,
		MinSent: model.Onboarding().EffectiveMinExposures(), Limit: args.Limit,
	}
	if cursor := strings.TrimSpace(args.Cursor); cursor != "" {
		key, err := onboarding.DecodeEmailTrackerCursor(cursor)
		if err != nil {
			return bad(fmt.Errorf("%d cursor is not valid.", http.StatusBadRequest))
		}
		filter.After = &key
	}
	rows, next := model.ListOnboardingEmailTracker(clientSession.Ctx, filter)
	result := &AdminOnboardingEmailTrackerResult{
		Rows:        rows,
		MinNetworks: filter.MinSent,
		Definitions: AdminOnboardingEmailTrackerDefinitions{
			Counts:                 "distinct networks per send-day and dimension tuple",
			WindowDays:             onboarding.EmailTrackerWindowDays,
			EngagementWindowDays:   onboarding.EmailTrackerEngagementWindowDays,
			DeliveryCorrectionDays: onboarding.EmailTrackerDeliveryCorrectionDays,
			EngagedIncludes:        []string{"email.clicked", "landing.clicked", "app.opened", "connected", "widget.added", "feedback.submitted", "pro.started"},
			EngagedExcludes:        []string{"email.opened"},
			LegacyAttribution:      "template-only events are counted only when the template identifies one flow step; shared-template events are attribution_ambiguous",
		},
	}
	if next != nil {
		cursor := onboarding.EncodeEmailTrackerCursor(*next)
		result.NextCursor = &cursor
	}
	onboardingAdminRequestsTotal.WithLabelValues(endpoint, "ok").Inc()
	return result, nil
}
