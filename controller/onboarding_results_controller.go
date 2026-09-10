package controller

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The results side of the onboarding program (mmm/onboarding/PLAN.md
// "OPTIMIZATION LOOP" §3-§7 and "MEASUREMENT"):
//
//   - the server-written outcome events (trial.converted, trial.cancelled,
//     refund, retention.d7, retention.d30), written by the store notification
//     handlers and the nightly rollup, idempotent per network and name
//   - the nightly OnboardingResultsRollup task (02:00 UTC): retention events,
//     the trial outcome backstop, the onboarding_results_daily recompute over
//     the last `results.rollup_days` cohort days, the guardrail check with
//     auto-pause, and the event retention prune
//   - the admin endpoints GET /admin/onboarding/results,
//     GET /admin/onboarding/email-tracker, and
//     GET /admin/onboarding/experiments behind the vault admin bearers
//   - the experiment-state overlay commands behind bringyourctl

// ----- metrics -----

var onboardingResultsRollupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "results_rollup_total",
	Help:      "Nightly results rollup runs by result: ok, failed.",
}, []string{"result"})

var onboardingResultsRows = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "results_rows",
	Help:      "Rows of onboarding_results_daily written by the last rollup.",
})

var onboardingResultsCohortNetworks = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "results_cohort_networks",
	Help:      "Networks in the cohort window the last rollup recomputed.",
})

var onboardingGuardrailBreachesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "guardrail_breaches_total",
	Help:      "Guardrail breaches that paused a variant, by experiment, variant and guardrail.",
}, []string{"experiment", "variant", "guardrail"})

var onboardingExperimentVariantsPaused = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "experiment_variants_paused",
	Help:      "Experiment variants currently paused by the state overlay (served as control).",
})

var onboardingOutcomeEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "outcome_events_total",
	Help:      "Server-written outcome events by name and store (trial.converted, trial.cancelled, refund, retention.d7, retention.d30).",
}, []string{"name", "store"})

var onboardingTrialBackstopTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "trial_backstop_total",
	Help:      "Trial outcomes decided by the daily backstop rather than a store notification, by outcome.",
}, []string{"outcome"})

var onboardingAdminRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "admin_requests_total",
	Help:      "Requests to the admin onboarding endpoints by endpoint and result: ok, unauthorized, forbidden, bad_request.",
}, []string{"endpoint", "result"})

func init() {
	prometheus.MustRegister(
		onboardingResultsRollupTotal,
		onboardingResultsRows,
		onboardingResultsCohortNetworks,
		onboardingGuardrailBreachesTotal,
		onboardingExperimentVariantsPaused,
		onboardingOutcomeEventsTotal,
		onboardingTrialBackstopTotal,
		onboardingAdminRequestsTotal,
	)
}

// ----- outcome events -----

// recordOutcomeEvent writes a server-side outcome event once per network and
// name. A second call for the same (network, name) is a no-op, so every store
// hook and the backstop may report the same outcome.
func recordOutcomeEvent(ctx context.Context, networkId server.Id, name string, props map[string]any, store string) bool {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]outcome %s for network %s failed: %v\n", name, networkId, r)
		}
	}()
	if model.HasOnboardingEvent(ctx, networkId, name) {
		return false
	}
	if !WriteServerEventWithContext(ctx, networkId, name, props, "") {
		return false
	}
	if store == "" {
		store = "unknown"
	}
	onboardingOutcomeEventsTotal.WithLabelValues(name, store).Inc()
	return true
}

func outcomeStoreProps(store string, plan string) map[string]any {
	props := map[string]any{}
	if store != "" {
		props["store"] = store
	}
	if plan != "" {
		props["plan"] = plan
	}
	return props
}

// RecordTrialConverted writes trial.converted: the network's free trial became
// a paid period. `store` is one of the model.OnboardingStore* names and `plan`
// yearly or monthly (either may be empty when the store does not say).
func RecordTrialConverted(ctx context.Context, networkId server.Id, store string, plan string) bool {
	return recordOutcomeEvent(ctx, networkId, model.EventTrialConverted, outcomeStoreProps(store, plan), store)
}

// RecordTrialCancelled writes trial.cancelled: the trial ended without a paid
// period. Only written when no trial.converted exists (a conversion is final).
func RecordTrialCancelled(ctx context.Context, networkId server.Id, store string, plan string) bool {
	if model.HasOnboardingEvent(ctx, networkId, model.EventTrialConverted) {
		return false
	}
	return recordOutcomeEvent(ctx, networkId, model.EventTrialCancelled, outcomeStoreProps(store, plan), store)
}

// RecordRefund writes refund with the refunded amount in USD (0 when unknown).
func RecordRefund(ctx context.Context, networkId server.Id, store string, amountUsd float64) bool {
	props := outcomeStoreProps(store, "")
	if 0 <= amountUsd && amountUsd <= 100000 {
		props["amount"] = amountUsd
	}
	return recordOutcomeEvent(ctx, networkId, model.EventRefund, props, store)
}

// ----- the nightly rollup task -----

type OnboardingResultsRollupArgs struct {
	// From and To bound the cohort days to recompute (inclusive). Zero means
	// the configured window ending yesterday.
	From time.Time `json:"from,omitempty"`
	To   time.Time `json:"to,omitempty"`
}

type OnboardingResultsRollupResult struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	CohortNetworks   int       `json:"cohort_networks"`
	Rows             int       `json:"rows"`
	RetentionEvents  int       `json:"retention_events"`
	TrialBackstops   int       `json:"trial_backstops"`
	GuardrailPauses  int       `json:"guardrail_pauses"`
	PrunedEvents     int64     `json:"pruned_events"`
	DurationSeconds  float64   `json:"duration_seconds"`
	ExperimentsFound int       `json:"experiments_found"`
}

const onboardingResultsRollupTaskKey = "onboarding_results_rollup"

// ScheduleOnboardingResultsRollup schedules the nightly task once.
func ScheduleOnboardingResultsRollup(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		OnboardingResultsRollup,
		&OnboardingResultsRollupArgs{},
		clientSession,
		task.RunOnce(onboardingResultsRollupTaskKey),
		task.RunAt(at),
	)
}

// OnboardingResultsRollup is the nightly task body. Idempotent: every step
// either writes an event the network does not have yet or replaces the rows of
// the cohort window it recomputed.
func OnboardingResultsRollup(
	args *OnboardingResultsRollupArgs,
	clientSession *session.ClientSession,
) (result *OnboardingResultsRollupResult, returnErr error) {
	result, returnErr = RunOnboardingResultsRollup(clientSession.Ctx, args.From, args.To)
	if returnErr != nil {
		onboardingResultsRollupTotal.WithLabelValues("failed").Inc()
	} else {
		onboardingResultsRollupTotal.WithLabelValues("ok").Inc()
	}
	return
}

func OnboardingResultsRollupPost(
	args *OnboardingResultsRollupArgs,
	result *OnboardingResultsRollupResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleOnboardingResultsRollup(clientSession, tx, onboarding.NextRollupAt(server.NowUtc()))
	return nil
}

// RunOnboardingResultsRollup does the rollup for the cohort days [from, to]
// (zero bounds mean the configured window ending yesterday). Shared by the task
// and `bringyourctl onboarding rollup`.
func RunOnboardingResultsRollup(ctx context.Context, from time.Time, to time.Time) (result *OnboardingResultsRollupResult, returnErr error) {
	start := server.NowUtc()
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("onboarding results rollup: %v", r)
			glog.Errorf("[onboarding]results rollup failed: %v\n", r)
		}
	}()
	now := server.NowUtc()
	config := model.Onboarding()
	if to.IsZero() {
		// yesterday: today's cohort is still being written
		to = onboarding.CohortDay(now).Add(-24 * time.Hour)
	}
	if from.IsZero() {
		from = to.Add(-time.Duration(config.EffectiveRollupDays()-1) * 24 * time.Hour)
	}
	from = onboarding.CohortDay(from)
	to = onboarding.CohortDay(to)
	if to.Before(from) {
		return nil, fmt.Errorf("rollup window is empty: from %s after to %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	result = &OnboardingResultsRollupResult{From: from, To: to}

	// 1. the cohort and its facts
	cohort := model.ListNetworkOnboardingCohort(ctx, from, to.Add(24*time.Hour))
	facts := model.LoadOnboardingCohortFacts(ctx, cohort)
	result.CohortNetworks = len(cohort)
	onboardingResultsCohortNetworks.Set(float64(len(cohort)))

	// 2. retention events for the networks whose windows closed
	result.RetentionEvents = writeRetentionEvents(ctx, cohort, facts, now)

	// 3. the trial outcome backstop
	result.TrialBackstops = runTrialBackstop(ctx, now)

	// 4. the aggregate
	rows := buildOnboardingResultsRows(config, cohort, facts, now)
	if err := model.ReplaceOnboardingResults(ctx, from, to, rows, now); err != nil {
		return result, err
	}
	result.Rows = len(rows)
	result.ExperimentsFound = len(config.Experiments)
	onboardingResultsRows.Set(float64(len(rows)))

	// 5. guardrails
	result.GuardrailPauses = checkOnboardingGuardrails(ctx, config, now)

	// 6. event retention
	result.PrunedEvents = model.PruneOnboardingEvents(ctx, now, 100000)

	result.DurationSeconds = server.NowUtc().Sub(start).Seconds()
	glog.Infof("[onboarding]results rollup %s..%s: %d networks, %d rows, %d retention events, %d trial backstops, %d guardrail pauses, %d pruned events in %.1fs\n",
		from.Format("2006-01-02"), to.Format("2006-01-02"),
		result.CohortNetworks, result.Rows, result.RetentionEvents, result.TrialBackstops, result.GuardrailPauses, result.PrunedEvents, result.DurationSeconds)
	return result, nil
}

// connectDaysWithin counts the distinct connection days in [cohort, cohort+days).
func connectDaysWithin(f *onboarding.NetworkFacts, days int) int {
	end := f.CohortAt.Add(time.Duration(days) * 24 * time.Hour)
	seen := map[string]bool{}
	for _, d := range f.ConnectionDays {
		if !d.Before(f.CohortAt.Add(-24*time.Hour)) && d.Before(end) {
			seen[d.UTC().Format("2006-01-02")] = true
		}
	}
	n := len(seen)
	if days < n {
		n = days
	}
	return n
}

// writeRetentionEvents writes retention.d7 for every cohort network older than
// 8 days without one, and retention.d30 for every network older than 31 days
// without one. `connect_days` is the distinct connection days in the first 7
// (30) days. Written in batches; a network that already has the event is
// skipped from the loaded facts, so this costs nothing on a quiet night.
func writeRetentionEvents(ctx context.Context, cohort []*model.NetworkOnboarding, facts map[server.Id]*onboarding.NetworkFacts, now time.Time) int {
	written := 0
	batch := []*model.OnboardingEvent{}
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := model.AddOnboardingEvents(ctx, batch); err != nil {
			glog.Errorf("[onboarding]retention events: %s\n", err)
		} else {
			written += len(batch)
		}
		batch = batch[:0]
	}
	add := func(row *model.NetworkOnboarding, name string, connectDays int) {
		validated, err := model.ValidateServerEvent(name, map[string]any{"connect_days": connectDays})
		if err != nil {
			glog.Errorf("[onboarding]refusing %s: %s\n", name, err)
			return
		}
		tier := ""
		if row.Country != "" {
			tier = model.Pro().PriceTierForCountry(row.Country).Name
		}
		batch = append(batch, &model.OnboardingEvent{
			NetworkId:  row.NetworkId,
			Name:       name,
			At:         now,
			ReceivedAt: now,
			Platform:   row.Platform,
			Tier:       tier,
			Path:       row.Path,
			Props:      validated,
		})
		onboardingOutcomeEventsTotal.WithLabelValues(name, "server").Inc()
		if 500 <= len(batch) {
			flush()
		}
	}
	for _, row := range cohort {
		f := facts[row.NetworkId]
		if f == nil {
			continue
		}
		age := now.Sub(f.CohortAt)
		if _, ok := f.FirstEventAt[onboarding.EventRetentionD7]; !ok && 8*24*time.Hour <= age {
			add(row, onboarding.EventRetentionD7, connectDaysWithin(f, onboarding.RetentionD7))
		}
		if _, ok := f.FirstEventAt[onboarding.EventRetentionD30]; !ok && 31*24*time.Hour <= age {
			add(row, onboarding.EventRetentionD30, connectDaysWithin(f, onboarding.RetentionD30))
		}
	}
	flush()
	return written
}

// runTrialBackstop decides the trials the store notifications never resolved:
// past trial length plus grace, a Pro network converted and any other cancelled.
func runTrialBackstop(ctx context.Context, now time.Time) int {
	decided := 0
	pending := model.ListOnboardingTrialsPending(ctx, now.Add(-(onboarding.TrialLength + onboarding.TrialOutcomeGrace)), 5000)
	for networkId, startedAt := range pending {
		isPro := model.IsProNetwork(ctx, networkId)
		switch onboarding.TrialBackstop(startedAt, isPro, now) {
		case onboarding.EventTrialConverted:
			if RecordTrialConverted(ctx, networkId, "", "") {
				onboardingTrialBackstopTotal.WithLabelValues("converted").Inc()
				decided += 1
			}
		case onboarding.EventTrialCancelled:
			if RecordTrialCancelled(ctx, networkId, "", "") {
				onboardingTrialBackstopTotal.WithLabelValues("cancelled").Inc()
				decided += 1
			}
		}
	}
	return decided
}

// ResultsExperimentAll is the pseudo-experiment every network is exposed to,
// so the funnel is measurable per platform, tier and path when no experiment
// covers a cohort. Its only variant is `all`.
const (
	ResultsExperimentAll = "_all"
	ResultsVariantAll    = "all"
	resultsUnknown       = "unknown"
)

// experimentCoversCohort is whether a registry entry was assigning when the
// network signed up: not a draft, started at or before the cohort time and not
// stopped before it. A paused or done experiment keeps its historical rows.
func experimentCoversCohort(e *model.OnboardingExperiment, at time.Time) bool {
	if e.Status == model.ExperimentStatusDraft {
		return false
	}
	start, err := model.ParseExperimentDay(e.Start)
	if err != nil {
		return false
	}
	stop, err := model.ParseExperimentDay(e.Stop)
	if err != nil {
		return false
	}
	if start != nil && at.Before(*start) {
		return false
	}
	if stop != nil && !at.Before(*stop) {
		return false
	}
	return true
}

// buildOnboardingResultsRows aggregates the cohort into result rows. The
// exposure of a network to an experiment is its registry assignment (so
// holdout rows exist without any stamped event); for the email sequence it is
// the variant the campaign row was actually served, and only networks with an
// email address count as exposed to an email experiment.
func buildOnboardingResultsRows(config *model.OnboardingConfig, cohort []*model.NetworkOnboarding, facts map[server.Id]*onboarding.NetworkFacts, now time.Time) []*model.OnboardingResultsRow {
	byKey := map[onboarding.ResultsKey]*model.OnboardingResultsRow{}
	add := func(row *model.NetworkOnboarding, experiment string, variant string, surface string, o onboarding.Outcomes, cohortDay time.Time) {
		platform := row.Platform
		if platform == "" {
			platform = resultsUnknown
		}
		tier := resultsUnknown
		if row.Country != "" {
			tier = model.Pro().PriceTierForCountry(row.Country).Name
		}
		path := row.Path
		if path == "" {
			path = resultsUnknown
		}
		r := &model.OnboardingResultsRow{
			CohortDay:    cohortDay,
			CohortDayStr: cohortDay.Format("2006-01-02"),
			Experiment:   experiment,
			Variant:      variant,
			Surface:      surface,
			Platform:     platform,
			Tier:         tier,
			Path:         path,
			MaturedDays:  onboarding.MaturedDays(cohortDay, now),
			ComputedAt:   now,
		}
		key := r.Key()
		if existing, ok := byKey[key]; ok {
			r = existing
		} else {
			byKey[key] = r
		}
		r.Add(o)
	}
	for _, row := range cohort {
		f := facts[row.NetworkId]
		if f == nil {
			continue
		}
		o := onboarding.ComputeOutcomes(*f, now)
		cohortDay := onboarding.CohortDay(row.CreatedAt)
		add(row, ResultsExperimentAll, ResultsVariantAll, "all", o, cohortDay)
		for _, e := range config.Experiments {
			if !experimentCoversCohort(e, row.CreatedAt) {
				continue
			}
			emailSurface := strings.HasPrefix(e.Surface, "email.")
			if emailSurface && !row.Email {
				continue
			}
			variant := e.Assign(row.NetworkId)
			if emailSurface && row.ExperimentId == e.Id && row.EmailVariant != "" {
				variant = row.EmailVariant
			}
			add(row, e.Id, variant, e.Surface, o, cohortDay)
		}
	}
	rows := make([]*model.OnboardingResultsRow, 0, len(byKey))
	for _, r := range byKey {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Key().Less(rows[j].Key())
	})
	return rows
}

// checkOnboardingGuardrails sums the last `results.guardrail_days` cohort days
// per variant of every running experiment and pauses a variant whose rate
// crosses a registry guardrail. Returns the number of variants paused now.
func checkOnboardingGuardrails(ctx context.Context, config *model.OnboardingConfig, now time.Time) int {
	paused := 0
	states := model.PausedVariants(ctx)
	to := onboarding.CohortDay(now).Add(-24 * time.Hour)
	from := to.Add(-time.Duration(config.EffectiveGuardrailDays()-1) * 24 * time.Hour)
	for _, e := range config.ActiveExperiments(now) {
		if len(e.Guardrails) == 0 {
			continue
		}
		totals := map[string]onboarding.GuardrailInput{}
		for _, r := range model.ListOnboardingResultsForGuardrails(ctx, e.Id, from, to) {
			t := totals[r.Variant]
			in := r.Guardrail()
			t.Exposures += in.Exposures
			t.Sent += in.Sent
			t.Delivered += in.Delivered
			t.Unsubscribe += in.Unsubscribe
			t.Complaint += in.Complaint
			t.ProStart14d += in.ProStart14d
			t.Refund60d += in.Refund60d
			totals[r.Variant] = t
		}
		variants := make([]string, 0, len(totals))
		for v := range totals {
			variants = append(variants, v)
		}
		sort.Strings(variants)
		for _, v := range variants {
			if states[e.Id][v] {
				continue
			}
			d := onboarding.DecideGuardrails(e.Guardrails, e.MinExposures, totals[v])
			if !d.Breached {
				continue
			}
			reason := fmt.Sprintf("%s %.4f over %.4f on cohorts %s..%s", d.Guardrail, d.Rate, d.Threshold, from.Format("2006-01-02"), to.Format("2006-01-02"))
			model.SetExperimentVariantState(ctx, &model.ExperimentVariantState{
				ExperimentId: e.Id,
				Variant:      v,
				Status:       onboarding.GuardrailPausedState,
				Reason:       reason,
				UpdatedAt:    now,
			})
			onboardingGuardrailBreachesTotal.WithLabelValues(e.Id, v, d.Guardrail).Inc()
			glog.Warningf("[onboarding]guardrail paused %s/%s: %s\n", e.Id, v, reason)
			paused += 1
		}
	}
	states = model.PausedVariants(ctx)
	count := 0
	for _, byVariant := range states {
		count += len(byVariant)
	}
	onboardingExperimentVariantsPaused.Set(float64(count))
	return paused
}

// ----- admin endpoints -----

// onboardingAdminBearers are the tokens of vault onboarding.yml
// `onboarding.admin_bearers`. The vault file is optional: without it every
// admin request is 401 (the endpoints are never open by default).
var onboardingAdminBearers = sync.OnceValue(func() []string {
	res, err := server.Vault.SimpleResource("onboarding.yml")
	if err != nil {
		glog.Warningf("[onboarding]no vault onboarding.yml: the admin results endpoints are closed\n")
		return []string{}
	}
	bearers := []string{}
	for _, b := range res.StringList("onboarding", "admin_bearers") {
		if b = strings.TrimSpace(b); b != "" {
			bearers = append(bearers, b)
		}
	}
	if len(bearers) == 0 {
		glog.Warningf("[onboarding]vault onboarding.yml has no admin_bearers: the admin results endpoints are closed\n")
	}
	return bearers
})

// requireOnboardingAdmin checks `Authorization: Bearer <token>` against the
// vault admin bearers in constant time. A missing or unknown token is 401; a
// network JWT (three dot-separated parts) is 403, so a misdirected app token
// is told apart from a missing one.
func requireOnboardingAdmin(clientSession *session.ClientSession, endpoint string) error {
	token := ""
	if clientSession != nil {
		for _, auth := range clientSession.Header["Authorization"] {
			if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
				token = strings.TrimSpace(auth[7:])
				break
			}
		}
	}
	if token == "" {
		onboardingAdminRequestsTotal.WithLabelValues(endpoint, "unauthorized").Inc()
		return fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
	}
	for _, b := range onboardingAdminBearers() {
		if len(b) == len(token) && subtle.ConstantTimeCompare([]byte(b), []byte(token)) == 1 {
			return nil
		}
	}
	if strings.Count(token, ".") == 2 {
		onboardingAdminRequestsTotal.WithLabelValues(endpoint, "forbidden").Inc()
		return fmt.Errorf("%d Forbidden: a network token is not an admin bearer.", http.StatusForbidden)
	}
	onboardingAdminRequestsTotal.WithLabelValues(endpoint, "unauthorized").Inc()
	return fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
}

// AdminOnboardingResultsArgs are the query parameters of GET /admin/onboarding/results.
type AdminOnboardingResultsArgs struct {
	Experiment string
	From       string
	To         string
	Surface    string
	Platform   string
	Tier       string
	Path       string
	Cursor     string
	Limit      int
}

// AdminOnboardingResultsResult is OnboardingResultsPage in the spec.
type AdminOnboardingResultsResult struct {
	Rows       []*model.OnboardingResultsRow `json:"rows"`
	NextCursor *string                       `json:"next_cursor"`
	// MinExposures is the volume floor applied to this page.
	MinExposures int `json:"min_exposures"`
}

func parseCohortDay(value string, name string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("%d %s must be YYYY-MM-DD.", http.StatusBadRequest, name)
	}
	return t.UTC(), nil
}

// AdminOnboardingResults pages onboarding_results_daily for one experiment.
func AdminOnboardingResults(args *AdminOnboardingResultsArgs, clientSession *session.ClientSession) (*AdminOnboardingResultsResult, error) {
	const endpoint = "results"
	if err := requireOnboardingAdmin(clientSession, endpoint); err != nil {
		return nil, err
	}
	bad := func(err error) (*AdminOnboardingResultsResult, error) {
		onboardingAdminRequestsTotal.WithLabelValues(endpoint, "bad_request").Inc()
		return nil, err
	}
	experiment := strings.TrimSpace(args.Experiment)
	if experiment == "" {
		return bad(fmt.Errorf("%d experiment is required.", http.StatusBadRequest))
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
	filter := model.OnboardingResultsFilter{
		Experiment:   experiment,
		From:         from,
		To:           to,
		Surface:      strings.TrimSpace(args.Surface),
		Platform:     strings.TrimSpace(args.Platform),
		Tier:         strings.TrimSpace(args.Tier),
		Path:         strings.TrimSpace(args.Path),
		MinExposures: model.Onboarding().EffectiveMinExposures(),
		Limit:        args.Limit,
	}
	if cursor := strings.TrimSpace(args.Cursor); cursor != "" {
		key, err := onboarding.DecodeResultsCursor(cursor)
		if err != nil {
			return bad(fmt.Errorf("%d cursor is not valid.", http.StatusBadRequest))
		}
		filter.After = &key
	}
	rows, next := model.ListOnboardingResults(clientSession.Ctx, filter)
	result := &AdminOnboardingResultsResult{
		Rows:         rows,
		MinExposures: filter.MinExposures,
	}
	if next != nil {
		cursor := onboarding.EncodeResultsCursor(*next)
		result.NextCursor = &cursor
	}
	onboardingAdminRequestsTotal.WithLabelValues(endpoint, "ok").Inc()
	return result, nil
}

// AdminOnboardingExperiment is OnboardingExperiment in the spec: a registry
// entry as loaded, with the live overlay state of its variants.
type AdminOnboardingExperiment struct {
	Id            string                          `json:"id"`
	Surface       string                          `json:"surface"`
	Status        string                          `json:"status"`
	Start         string                          `json:"start,omitempty"`
	Stop          string                          `json:"stop,omitempty"`
	Allocation    map[string]float64              `json:"allocation"`
	Variants      map[string]map[string]any       `json:"variants"`
	PrimaryMetric string                          `json:"primary_metric,omitempty"`
	Secondary     []string                        `json:"secondary,omitempty"`
	Guardrails    map[string]float64              `json:"guardrails,omitempty"`
	MinExposures  int                             `json:"min_exposures,omitempty"`
	Active        bool                            `json:"active"`
	VariantStates []*model.ExperimentVariantState `json:"variant_states"`
}

type AdminOnboardingExperimentsResult struct {
	Experiments []*AdminOnboardingExperiment `json:"experiments"`
	// Results is the results configuration the rollup and the results
	// endpoint apply, so the analysis script judges with the same floors.
	Results AdminOnboardingResultsConfig `json:"results"`
}

type AdminOnboardingResultsConfig struct {
	MinExposures  int `json:"min_exposures"`
	RollupDays    int `json:"rollup_days"`
	GuardrailDays int `json:"guardrail_days"`
}

// OnboardingExperimentsState is the registry with the overlay applied, as the
// admin endpoint and bringyourctl print it.
func OnboardingExperimentsState(ctx context.Context) *AdminOnboardingExperimentsResult {
	now := server.NowUtc()
	config := model.Onboarding()
	states := map[string][]*model.ExperimentVariantState{}
	for _, s := range model.ListExperimentVariantStates(ctx) {
		states[s.ExperimentId] = append(states[s.ExperimentId], s)
	}
	result := &AdminOnboardingExperimentsResult{
		Experiments: []*AdminOnboardingExperiment{},
		Results: AdminOnboardingResultsConfig{
			MinExposures:  config.EffectiveMinExposures(),
			RollupDays:    config.EffectiveRollupDays(),
			GuardrailDays: config.EffectiveGuardrailDays(),
		},
	}
	for _, e := range config.Experiments {
		variantStates := states[e.Id]
		if variantStates == nil {
			variantStates = []*model.ExperimentVariantState{}
		}
		sort.Slice(variantStates, func(i, j int) bool {
			return variantStates[i].Variant < variantStates[j].Variant
		})
		result.Experiments = append(result.Experiments, &AdminOnboardingExperiment{
			Id:            e.Id,
			Surface:       e.Surface,
			Status:        e.Status,
			Start:         e.Start,
			Stop:          e.Stop,
			Allocation:    e.Allocation,
			Variants:      e.Variants,
			PrimaryMetric: e.PrimaryMetric,
			Secondary:     e.Secondary,
			Guardrails:    e.Guardrails,
			MinExposures:  e.MinExposures,
			Active:        e.Active(now),
			VariantStates: variantStates,
		})
	}
	return result
}

// AdminOnboardingExperiments is GET /admin/onboarding/experiments.
func AdminOnboardingExperiments(clientSession *session.ClientSession) (*AdminOnboardingExperimentsResult, error) {
	const endpoint = "experiments"
	if err := requireOnboardingAdmin(clientSession, endpoint); err != nil {
		return nil, err
	}
	onboardingAdminRequestsTotal.WithLabelValues(endpoint, "ok").Inc()
	return OnboardingExperimentsState(clientSession.Ctx), nil
}

// ----- the overlay commands (bringyourctl) -----

func findRegistryExperiment(experimentId string) (*model.OnboardingExperiment, error) {
	for _, e := range model.Onboarding().Experiments {
		if e.Id == experimentId {
			return e, nil
		}
	}
	return nil, fmt.Errorf("experiment %q is not in the registry", experimentId)
}

// ResumeExperimentVariant clears a pause: the variant is served again from the
// next assignment (the overlay cache refreshes immediately in this process and
// within a minute elsewhere).
func ResumeExperimentVariant(ctx context.Context, experimentId string, variant string) error {
	e, err := findRegistryExperiment(experimentId)
	if err != nil {
		return err
	}
	if _, ok := e.Variants[variant]; !ok {
		return fmt.Errorf("experiment %q has no variant %q", experimentId, variant)
	}
	model.SetExperimentVariantState(ctx, &model.ExperimentVariantState{
		ExperimentId: experimentId,
		Variant:      variant,
		Status:       model.ExperimentStatusRunning,
		Reason:       "resumed by operator",
		UpdatedAt:    server.NowUtc(),
	})
	glog.Infof("[onboarding]resumed %s/%s\n", experimentId, variant)
	return nil
}

// PauseExperimentVariant pauses a variant by hand (the same overlay row the
// guardrail check writes).
func PauseExperimentVariant(ctx context.Context, experimentId string, variant string, reason string) error {
	e, err := findRegistryExperiment(experimentId)
	if err != nil {
		return err
	}
	if _, ok := e.Variants[variant]; !ok {
		return fmt.Errorf("experiment %q has no variant %q", experimentId, variant)
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "paused by operator"
	}
	model.SetExperimentVariantState(ctx, &model.ExperimentVariantState{
		ExperimentId: experimentId,
		Variant:      variant,
		Status:       onboarding.GuardrailPausedState,
		Reason:       reason,
		UpdatedAt:    server.NowUtc(),
	})
	glog.Warningf("[onboarding]paused %s/%s: %s\n", experimentId, variant, reason)
	return nil
}

// ----- store hooks -----

// TrialCancelWindow bounds how long after the trial purchase a store's
// "expired" or "cancelled" signal still counts as the trial's outcome: past the
// trial plus this, the network converted (or the backstop already decided) and
// the signal is the end of a paid period instead.
const TrialCancelWindow = onboarding.TrialLength + 30*24*time.Hour

// planForProductId maps a store product id to yearly/monthly by its name, or
// "" when the id does not say.
func planForProductId(productId string) string {
	id := strings.ToLower(productId)
	switch {
	case strings.Contains(id, "year"), strings.Contains(id, "annual"):
		return model.PlanYearly
	case strings.Contains(id, "month"):
		return model.PlanMonthly
	}
	return ""
}

// storeTrialConverted records trial.converted for a network the store just
// charged after a trial it recorded (purchase.completed{trial:true}). The
// trial must be old enough for the charge to be its first paid period.
func storeTrialConverted(ctx context.Context, networkId server.Id, store string, plan string, now time.Time) bool {
	trialAt, ok := model.NetworkTrialPurchaseAt(ctx, networkId)
	if !ok {
		return false
	}
	// a day of slack: stores bill a little before the period rolls over
	if now.Before(trialAt.Add(onboarding.TrialLength - 24*time.Hour)) {
		return false
	}
	return RecordTrialConverted(ctx, networkId, store, plan)
}

// storeTrialCancelled records trial.cancelled for a network whose trial the
// store reports ended, expired or cancelled while it could still be the trial.
func storeTrialCancelled(ctx context.Context, networkId server.Id, store string, plan string, now time.Time) bool {
	trialAt, ok := model.NetworkTrialPurchaseAt(ctx, networkId)
	if !ok {
		return false
	}
	if trialAt.Add(TrialCancelWindow).Before(now) {
		return false
	}
	return RecordTrialCancelled(ctx, networkId, store, plan)
}
