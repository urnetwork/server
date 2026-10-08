package model

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/onboarding"
)

// The results side of the onboarding program (mmm/onboarding/PLAN.md
// "OPTIMIZATION LOOP" §3-§5): the nightly aggregate the analysis script reads,
// the per-network facts it is computed from, and the experiment-state overlay
// that pauses a variant without editing the registry.

// ----- results rows -----

// OnboardingResultsRow is one row of onboarding_results_daily: the dimension
// tuple and the counts. Counts are networks, not events.
type OnboardingResultsRow struct {
	CohortDay      time.Time `json:"-"`
	CohortDayStr   string    `json:"cohort_day"`
	Experiment     string    `json:"experiment"`
	Variant        string    `json:"variant"`
	Surface        string    `json:"surface"`
	Platform       string    `json:"platform"`
	Tier           string    `json:"tier"`
	Path           string    `json:"path"`
	Exposures      int       `json:"exposures"`
	Sent           int       `json:"sent"`
	Delivered      int       `json:"delivered"`
	Opened         int       `json:"opened"`
	Clicked        int       `json:"clicked"`
	LandingClicked int       `json:"landing_clicked"`
	AppOpen48h     int       `json:"app_open_48h"`
	Connect7d      int       `json:"connect_7d"`
	Widget7d       int       `json:"widget_7d"`
	Feedback7d     int       `json:"feedback_7d"`
	ProStart14d    int       `json:"pro_start_14d"`
	TrialToPaid35d int       `json:"trial_to_paid_35d"`
	Refund60d      int       `json:"refund_60d"`
	RetentionD7    int       `json:"retention_d7"`
	RetentionD30   int       `json:"retention_d30"`
	Unsubscribe    int       `json:"unsubscribe"`
	Complaint      int       `json:"complaint"`
	// MaturedDays is the cohort's age at rollup time: an outcome window longer
	// than this has not been measured yet (its count is provisional).
	MaturedDays int       `json:"matured_days"`
	ComputedAt  time.Time `json:"computed_at"`
}

// Key is the ordering tuple, the admin endpoint's keyset cursor.
func (r *OnboardingResultsRow) Key() onboarding.ResultsKey {
	return onboarding.ResultsKey{
		CohortDay:  r.CohortDayStr,
		Experiment: r.Experiment,
		Variant:    r.Variant,
		Surface:    r.Surface,
		Platform:   r.Platform,
		Tier:       r.Tier,
		Path:       r.Path,
	}
}

// Add adds one network's outcomes to the row.
func (r *OnboardingResultsRow) Add(o onboarding.Outcomes) {
	r.Exposures += 1
	inc := func(v bool, n *int) {
		if v {
			*n += 1
		}
	}
	inc(o.Sent, &r.Sent)
	inc(o.Delivered, &r.Delivered)
	inc(o.Opened, &r.Opened)
	inc(o.Clicked, &r.Clicked)
	inc(o.LandingClicked, &r.LandingClicked)
	inc(o.AppOpen48h, &r.AppOpen48h)
	inc(o.Connect7d, &r.Connect7d)
	inc(o.Widget7d, &r.Widget7d)
	inc(o.Feedback7d, &r.Feedback7d)
	inc(o.ProStart14d, &r.ProStart14d)
	inc(o.TrialToPaid35d, &r.TrialToPaid35d)
	inc(o.Refund60d, &r.Refund60d)
	inc(o.RetentionD7, &r.RetentionD7)
	inc(o.RetentionD30, &r.RetentionD30)
	inc(o.Unsubscribe, &r.Unsubscribe)
	inc(o.Complaint, &r.Complaint)
}

// Guardrail is the row's contribution to a guardrail check.
func (r *OnboardingResultsRow) Guardrail() onboarding.GuardrailInput {
	return onboarding.GuardrailInput{
		Exposures:   r.Exposures,
		Sent:        r.Sent,
		Delivered:   r.Delivered,
		Unsubscribe: r.Unsubscribe,
		Complaint:   r.Complaint,
		ProStart14d: r.ProStart14d,
		Refund60d:   r.Refund60d,
	}
}

const cohortDayLayout = "2006-01-02"

// ReplaceOnboardingResults replaces every row whose cohort day is in
// [from, to] with `rows`, in one transaction, so a recompute never leaves a
// dimension combination behind that no longer exists.
func ReplaceOnboardingResults(ctx context.Context, from time.Time, to time.Time, rows []*OnboardingResultsRow, computedAt time.Time) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", r)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM onboarding_results_daily WHERE cohort_day >= $1 AND cohort_day <= $2`,
			onboarding.CohortDay(from),
			onboarding.CohortDay(to),
		))
		for _, r := range rows {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO onboarding_results_daily (
						cohort_day, experiment, variant, surface, platform, tier, path,
						exposures, sent, delivered, opened, clicked, landing_clicked,
						app_open_48h, connect_7d, widget_7d, feedback_7d, pro_start_14d,
						trial_to_paid_35d, refund_60d, retention_d7, retention_d30,
						unsubscribe, complaint, matured_days, computed_at
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)
					ON CONFLICT (cohort_day, experiment, variant, surface, platform, tier, path) DO UPDATE
					SET
						exposures = $8, sent = $9, delivered = $10, opened = $11, clicked = $12, landing_clicked = $13,
						app_open_48h = $14, connect_7d = $15, widget_7d = $16, feedback_7d = $17, pro_start_14d = $18,
						trial_to_paid_35d = $19, refund_60d = $20, retention_d7 = $21, retention_d30 = $22,
						unsubscribe = $23, complaint = $24, matured_days = $25, computed_at = $26
				`,
				r.CohortDay, r.Experiment, r.Variant, r.Surface, r.Platform, r.Tier, r.Path,
				r.Exposures, r.Sent, r.Delivered, r.Opened, r.Clicked, r.LandingClicked,
				r.AppOpen48h, r.Connect7d, r.Widget7d, r.Feedback7d, r.ProStart14d,
				r.TrialToPaid35d, r.Refund60d, r.RetentionD7, r.RetentionD30,
				r.Unsubscribe, r.Complaint, r.MaturedDays, computedAt,
			))
		}
	})
	return
}

// OnboardingResultsFilter selects rows for the admin endpoint.
type OnboardingResultsFilter struct {
	Experiment string
	From       time.Time
	To         time.Time
	Surface    string
	Platform   string
	Tier       string
	Path       string
	// After is the keyset cursor: rows strictly after this key in
	// (cohort_day, experiment, variant, surface, platform, tier, path) order.
	After *onboarding.ResultsKey
	// MinExposures suppresses rows under the volume floor.
	MinExposures int
	Limit        int
}

const onboardingResultsSelect = `
	SELECT
		cohort_day, experiment, variant, surface, platform, tier, path,
		exposures, sent, delivered, opened, clicked, landing_clicked,
		app_open_48h, connect_7d, widget_7d, feedback_7d, pro_start_14d,
		trial_to_paid_35d, refund_60d, retention_d7, retention_d30,
		unsubscribe, complaint, matured_days, computed_at
	FROM onboarding_results_daily
`

func scanOnboardingResultsRow(result pgx.Rows) *OnboardingResultsRow {
	r := &OnboardingResultsRow{}
	server.Raise(result.Scan(
		&r.CohortDay, &r.Experiment, &r.Variant, &r.Surface, &r.Platform, &r.Tier, &r.Path,
		&r.Exposures, &r.Sent, &r.Delivered, &r.Opened, &r.Clicked, &r.LandingClicked,
		&r.AppOpen48h, &r.Connect7d, &r.Widget7d, &r.Feedback7d, &r.ProStart14d,
		&r.TrialToPaid35d, &r.Refund60d, &r.RetentionD7, &r.RetentionD30,
		&r.Unsubscribe, &r.Complaint, &r.MaturedDays, &r.ComputedAt,
	))
	r.CohortDayStr = r.CohortDay.UTC().Format(cohortDayLayout)
	return r
}

// ListOnboardingResults pages the aggregate. Returns the rows and, when more
// remain, the key of the last row returned (the caller encodes it).
func ListOnboardingResults(ctx context.Context, filter OnboardingResultsFilter) (rows []*OnboardingResultsRow, next *onboarding.ResultsKey) {
	limit := filter.Limit
	if limit <= 0 || 1000 < limit {
		limit = 500
	}
	rows = []*OnboardingResultsRow{}
	server.Db(ctx, func(conn server.PgConn) {
		args := []any{filter.Experiment, onboarding.CohortDay(filter.From), onboarding.CohortDay(filter.To), filter.MinExposures}
		where := []string{"experiment = $1", "cohort_day >= $2", "cohort_day <= $3", "exposures >= $4"}
		add := func(column string, value string) {
			if value == "" {
				return
			}
			args = append(args, value)
			where = append(where, column+" = $"+itoa(len(args)))
		}
		add("surface", filter.Surface)
		add("platform", filter.Platform)
		add("tier", filter.Tier)
		add("path", filter.Path)
		if filter.After != nil {
			cohortDay, err := time.Parse(cohortDayLayout, filter.After.CohortDay)
			server.Raise(err)
			args = append(args, cohortDay, filter.After.Experiment, filter.After.Variant, filter.After.Surface, filter.After.Platform, filter.After.Tier, filter.After.Path)
			n := len(args)
			where = append(where, "(cohort_day, experiment, variant, surface, platform, tier, path) > ($"+itoa(n-6)+", $"+itoa(n-5)+", $"+itoa(n-4)+", $"+itoa(n-3)+", $"+itoa(n-2)+", $"+itoa(n-1)+", $"+itoa(n)+")")
		}
		args = append(args, limit+1)
		query := onboardingResultsSelect +
			" WHERE " + strings.Join(where, " AND ") +
			" ORDER BY cohort_day, experiment, variant, surface, platform, tier, path" +
			" LIMIT $" + itoa(len(args))
		result, err := conn.Query(ctx, query, args...)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rows = append(rows, scanOnboardingResultsRow(result))
			}
		})
	})
	if limit < len(rows) {
		rows = rows[:limit]
		key := rows[len(rows)-1].Key()
		next = &key
	}
	return
}

// ListOnboardingResultsForGuardrails is every row of an experiment whose cohort
// day is in [from, to], unfiltered by volume (the guardrail check sums them).
func ListOnboardingResultsForGuardrails(ctx context.Context, experiment string, from time.Time, to time.Time) (rows []*OnboardingResultsRow) {
	rows = []*OnboardingResultsRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			onboardingResultsSelect+` WHERE experiment = $1 AND cohort_day >= $2 AND cohort_day <= $3`,
			experiment,
			onboarding.CohortDay(from),
			onboarding.CohortDay(to),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rows = append(rows, scanOnboardingResultsRow(result))
			}
		})
	})
	return
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// ----- cohort facts -----

// ListNetworkOnboardingCohort is every campaign row created in [from, to),
// oldest first.
func ListNetworkOnboardingCohort(ctx context.Context, from time.Time, to time.Time) (rows []*NetworkOnboarding) {
	rows = []*NetworkOnboarding{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			strings.Replace(networkOnboardingSelect, "WHERE network_id = $1", "WHERE created_at >= $1 AND created_at < $2 ORDER BY created_at", 1),
			from,
			to,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rows = append(rows, scanNetworkOnboarding(result))
			}
		})
	})
	return
}

// LoadOnboardingCohortFacts loads the outcome facts of a cohort in four set
// queries: the first time of each event name per network, the connection days
// (connect.day events), the first feedback and the first subscription renewal.
func LoadOnboardingCohortFacts(ctx context.Context, rows []*NetworkOnboarding) map[server.Id]*onboarding.NetworkFacts {
	facts := map[server.Id]*onboarding.NetworkFacts{}
	if len(rows) == 0 {
		return facts
	}
	networkIds := make([]server.Id, 0, len(rows))
	for _, row := range rows {
		networkIds = append(networkIds, row.NetworkId)
		facts[row.NetworkId] = &onboarding.NetworkFacts{
			CohortAt:     row.CreatedAt,
			FirstEventAt: map[string]time.Time{},
		}
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, name, MIN(at)
				FROM network_onboarding_event
				WHERE network_id = ANY($1)
				GROUP BY network_id, name
			`,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var name string
				var at time.Time
				server.Raise(result.Scan(&networkId, &name, &at))
				if f := facts[networkId]; f != nil {
					f.FirstEventAt[name] = at
				}
			}
		})

		// connection days come from the connect.day events the client
		// connection path writes (one per network per UTC day). The
		// connection table cannot be used: RemoveDisconnectedNetworkClients
		// prunes its rows 8 h after disconnect, so it holds days of history
		// against the 31 days the retention windows need. There is no
		// history before the deploy of that writer.
		result, err = conn.Query(
			ctx,
			`
				SELECT network_id, date_trunc('day', at)
				FROM network_onboarding_event
				WHERE network_id = ANY($1) AND name = $2
				GROUP BY network_id, date_trunc('day', at)
			`,
			networkIds,
			EventConnectDay,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var day time.Time
				server.Raise(result.Scan(&networkId, &day))
				if f := facts[networkId]; f != nil {
					f.ConnectionDays = append(f.ConnectionDays, day)
				}
			}
		})

		result, err = conn.Query(
			ctx,
			`SELECT network_id, MIN(feedback_time) FROM account_feedback WHERE network_id = ANY($1) GROUP BY network_id`,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var at time.Time
				server.Raise(result.Scan(&networkId, &at))
				if f := facts[networkId]; f != nil {
					t := at
					f.FirstFeedbackAt = &t
				}
			}
		})

		result, err = conn.Query(
			ctx,
			`SELECT network_id, MIN(start_time) FROM subscription_renewal WHERE network_id = ANY($1) GROUP BY network_id`,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var at time.Time
				server.Raise(result.Scan(&networkId, &at))
				if f := facts[networkId]; f != nil {
					t := at
					f.FirstProAt = &t
				}
			}
		})
	})
	return facts
}

// ListOnboardingTrialsPending is every network with a purchase.completed
// event that carried trial = true, at or before `startedBefore`, and no
// trial.converted or trial.cancelled yet: the daily backstop's work list.
func ListOnboardingTrialsPending(ctx context.Context, startedBefore time.Time, limit int) (trials map[server.Id]time.Time) {
	trials = map[server.Id]time.Time{}
	if limit <= 0 {
		limit = 5000
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT p.network_id, MIN(p.at)
				FROM network_onboarding_event p
				WHERE p.name = $1
					AND p.at <= $2
					AND (p.props->>'trial') = 'true'
					AND NOT EXISTS (
						SELECT 1 FROM network_onboarding_event o
						WHERE o.network_id = p.network_id AND o.name IN ($3, $4)
					)
				GROUP BY p.network_id
				LIMIT $5
			`,
			onboarding.EventPurchaseCompleted,
			startedBefore,
			onboarding.EventTrialConverted,
			onboarding.EventTrialCancelled,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var at time.Time
				server.Raise(result.Scan(&networkId, &at))
				trials[networkId] = at
			}
		})
	})
	return
}

// NetworkHadTrialPurchase is whether the network recorded a purchase.completed
// with trial = true (any store).
func NetworkHadTrialPurchase(ctx context.Context, networkId server.Id) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT 1 FROM network_onboarding_event WHERE network_id = $1 AND name = $2 AND (props->>'trial') = 'true' LIMIT 1`,
			networkId,
			onboarding.EventPurchaseCompleted,
		)
		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
	})
	return
}

// NetworkTrialPurchaseAt is when the network's trial purchase was recorded
// (the earliest purchase.completed with trial = true), if any.
func NetworkTrialPurchaseAt(ctx context.Context, networkId server.Id) (at time.Time, exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT MIN(at) FROM network_onboarding_event WHERE network_id = $1 AND name = $2 AND (props->>'trial') = 'true'`,
			networkId,
			onboarding.EventPurchaseCompleted,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var minAt *time.Time
				server.Raise(result.Scan(&minAt))
				if minAt != nil {
					at = *minAt
					exists = true
				}
			}
		})
	})
	return
}

// ----- experiment state overlay -----

// ExperimentVariantState is one overlay row: the live status of a registry
// variant. The registry (config) defines experiments; this table only pauses
// and resumes their variants.
type ExperimentVariantState struct {
	ExperimentId string    `json:"experiment_id"`
	Variant      string    `json:"variant"`
	Status       string    `json:"status"`
	Reason       string    `json:"reason"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SetExperimentVariantState upserts the overlay row and refreshes the
// assignment cache.
func SetExperimentVariantState(ctx context.Context, state *ExperimentVariantState) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_onboarding_experiment_state (experiment_id, variant, status, reason, updated_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (experiment_id, variant) DO UPDATE
				SET status = $3, reason = $4, updated_at = $5
			`,
			state.ExperimentId,
			state.Variant,
			state.Status,
			state.Reason,
			state.UpdatedAt,
		))
	})
	pausedVariantsCache.invalidate()
}

// ListExperimentVariantStates is every overlay row.
func ListExperimentVariantStates(ctx context.Context) (states []*ExperimentVariantState) {
	states = []*ExperimentVariantState{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT experiment_id, variant, status, reason, updated_at FROM network_onboarding_experiment_state ORDER BY experiment_id, variant`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				s := &ExperimentVariantState{}
				server.Raise(result.Scan(&s.ExperimentId, &s.Variant, &s.Status, &s.Reason, &s.UpdatedAt))
				states = append(states, s)
			}
		})
	})
	return
}

// PausedVariants is experiment id -> variant -> true for every overlay row in
// the paused state.
func PausedVariants(ctx context.Context) map[string]map[string]bool {
	paused := map[string]map[string]bool{}
	for _, s := range ListExperimentVariantStates(ctx) {
		if s.Status != ExperimentStatusPaused {
			continue
		}
		if paused[s.ExperimentId] == nil {
			paused[s.ExperimentId] = map[string]bool{}
		}
		paused[s.ExperimentId][s.Variant] = true
	}
	return paused
}

// pausedVariantsCache is the assignment path's view of the overlay: refreshed
// at most every pausedVariantsTtl, from a background context so a request
// never blocks on the refresh beyond one short query, and kept on a refresh
// error. A test may install a fake through SetPausedVariantsForTest.
var pausedVariantsCache = &pausedVariantsCacheState{}

const pausedVariantsTtl = 60 * time.Second

type pausedVariantsCacheState struct {
	mutex     sync.Mutex
	loadedAt  time.Time
	paused    map[string]map[string]bool
	override  map[string]map[string]bool
	overriden bool
}

func (c *pausedVariantsCacheState) invalidate() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.loadedAt = time.Time{}
}

func (c *pausedVariantsCacheState) get(now time.Time) map[string]map[string]bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.overriden {
		return c.override
	}
	if c.paused != nil && now.Sub(c.loadedAt) < pausedVariantsTtl {
		return c.paused
	}
	loaded := map[string]map[string]bool{}
	ok := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				ok = false
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		loaded = PausedVariants(ctx)
		return true
	}()
	if ok {
		c.paused = loaded
		c.loadedAt = now
	} else if c.paused == nil {
		c.paused = map[string]map[string]bool{}
		// retry soon rather than in a minute
		c.loadedAt = now.Add(-pausedVariantsTtl + 5*time.Second)
	}
	return c.paused
}

// PausedVariantsForExperiment is the overlay's paused variants of one
// experiment, from the cache.
func PausedVariantsForExperiment(experimentId string, now time.Time) map[string]bool {
	paused := pausedVariantsCache.get(now)[experimentId]
	if paused == nil {
		return map[string]bool{}
	}
	return paused
}

// SetPausedVariantsForTest installs (nil clears) a fixed overlay so pure tests
// exercise the assignment precedence without a database.
func SetPausedVariantsForTest(paused map[string]map[string]bool) {
	pausedVariantsCache.mutex.Lock()
	defer pausedVariantsCache.mutex.Unlock()
	pausedVariantsCache.override = paused
	pausedVariantsCache.overriden = paused != nil
}

// SortedVariantNames is the registry variants of an experiment in sorted order
// (a small helper for the overlay callers).
func SortedVariantNames(e *OnboardingExperiment) []string {
	names := e.VariantNames()
	sort.Strings(names)
	return names
}
