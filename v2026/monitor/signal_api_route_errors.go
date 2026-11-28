// SIGNALS.md §25.3. Compare completed customer-route requests with the 5xx
// statuses recorded by the API router, independently on the API and alt jobs.
package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	apiRouteErrorsClass    = "api-route-errors"
	apiRouteErrorsUnknown  = "api-route-errors-unobservable"
	apiRouteErrorsProbeId  = "mimir/api-route-errors"
	apiRouteRequestsMetric = "urnetwork_http_requests_total"
	apiRouteCountMetric    = "urnetwork_http_request_duration_seconds_count"
)

// Fixed route IDs from api.Routes and router.NewRoute. Only these names can
// become finding targets. The reliability route is the October 19 replay.
var apiCustomerRoutes = [...]struct{ name, route string }{
	{"connect-control", "POST ^/connect/control$"},
	{"auth-client", "POST ^/network/auth-client$"},
	{"find-providers2", "POST ^/network/find-providers2$"},
	{"provider-locations", "GET ^/network/provider-locations$"},
	{"auth-refresh", "GET ^/auth/refresh$"},
	{"network-user", "GET ^/network/user$"},
	{"subscription-balance", "GET ^/subscription/balance$"},
	{"network-reliability", "GET ^/network/reliability$"},
	{"auth-login", "POST ^/auth/login$"},
	{"auth-login-with-password", "POST ^/auth/login-with-password$"},
	{"auth-code-login", "POST ^/auth/code-login$"},
	{"auth-verify", "POST ^/auth/verify$"},
	{"auth-verify-send", "POST ^/auth/verify-send$"},
	{"auth-wallet-challenge", "POST ^/auth/wallet-challenge$"},
	{"auth-wallet-nonce", "POST ^/auth/wallet-nonce$"},
	{"auth-apple-callback-get", "GET ^/auth/apple/callback$"},
	{"auth-apple-callback-post", "POST ^/auth/apple/callback$"},
	{"auth-google-callback", "GET ^/auth/google/callback$"},
}

// Each band uses a trailing rate and a sampled hold of its own duration.
type ApiRouteErrorsSettings struct {
	// Rate and hold windows, in whole evaluation steps. Defaults ten
	// minutes and five minutes. Nonpositive durations are invalid.
	WarnWindow, PageWindow time.Duration
	// Inclusive 5xx share bands, by default 0.02 and 0.10. Both must be
	// positive and the page band must exceed the warning band.
	WarnShare, PageShare float64
	// One completed request/s by default. Nonpositive mass is invalid.
	MinRequestsPerSecond float64
	// Maximum observed status/outcome children per route and slot.
	// The default is 512; zero or an excess leaves the source unknown.
	MaxChildrenPerRoute int
	// Exact clean server revisions whose router preserves a live caller's
	// canceled 5xx and a panic's numeric status. Empty keeps the uncertainty
	// bounds for every slot. A revision match needs same-push image evidence.
	VerifiedStatusRevisions []string
	StormReadSettings
}

// The catalog bands and the bounds of a single range query.
func DefaultApiRouteErrorsSettings() *ApiRouteErrorsSettings {
	read := DefaultStormReadSettings()
	read.Freshness = 45 * time.Second
	read.MaxRows = 65536
	read.MaxResponseBytes = 4 * 1024 * 1024
	return &ApiRouteErrorsSettings{
		WarnWindow: 10 * time.Minute, PageWindow: 5 * time.Minute,
		WarnShare: .02, PageShare: .10, MinRequestsPerSecond: 1,
		MaxChildrenPerRoute: 512, StormReadSettings: read,
		// Each revision has the same router/metrics.go and router/router.go
		// blobs as the original corrected disposition contract at bc9d61d23.
		VerifiedStatusRevisions: []string{
			"bc9d61d238f6119bbf79c5f84e3c377b39c30f96",
			"92cf3b5e541fcea67df66539ac0247249e1390f4",
			"13a7f74164e5fab3936fdda7ac959949eb34a39f",
		},
	}
}

// Register the per-route readings for both public API fronts.
func NewApiRouteErrorsSignal() Signal {
	return NewApiRouteErrorsSignalWithSettings(DefaultApiRouteErrorsSettings())
}

// Override the read bounds and bands for a specific monitor.
func NewApiRouteErrorsSignalWithSettings(settings *ApiRouteErrorsSettings) Signal {
	return &signalAdapter{number: "25.3", key: apiRouteErrorsClass, name: "API errors on customer routes", probe: apiRouteErrorsProbe{settings}}
}

// Keep sparse research inputs and invalid thresholds out of the read path.
func (self *ApiRouteErrorsSettings) valid() bool {
	if len(self.VerifiedStatusRevisions) > 64 {
		return false
	}
	for _, revision := range self.VerifiedStatusRevisions {
		if !validGoSourceRevision(revision) {
			return false
		}
	}
	return self.WarnWindow <= time.Hour && self.PageWindow <= self.WarnWindow && self.QueryTimeout <= time.Minute && self.StormReadSettings.valid(self.WarnWindow, self.PageWindow) &&
		self.Freshness >= time.Second && self.Freshness%time.Second == 0 && self.Freshness <= self.Step &&
		self.WarnShare > 0 && self.WarnShare < self.PageShare && self.PageShare <= 1 &&
		self.MinRequestsPerSecond > 0 && !math.IsInf(self.MinRequestsPerSecond, 0) &&
		self.MaxChildrenPerRoute > 0 && self.MaxChildrenPerRoute <= 2048 &&
		self.MaxRows <= 65536 && self.MaxResponseBytes <= 4*1024*1024
}

// All counter evidence and elapsed holds come from the query range.
type apiRouteErrorsProbe struct{ settings *ApiRouteErrorsSettings }

// Probe identity used by the registry and all findings.
func (apiRouteErrorsProbe) id() string { return apiRouteErrorsProbeId }

// Source failures are separate visibility findings.
func (apiRouteErrorsProbe) tier() string { return tierPage }

// Disallow a nonpositive scheduling interval even if policy validation fails.
func (self apiRouteErrorsProbe) cadence() time.Duration { return self.policy().schedule() }

// A nil policy selects the same defaults as the registry.
func (self apiRouteErrorsProbe) policy() *ApiRouteErrorsSettings {
	if self.settings == nil {
		return DefaultApiRouteErrorsSettings()
	}
	return self.settings
}

// Return per-placement, per-allowlisted-route cumulative snapshots. The
// process nonce, arbitrary statuses and terminal outcomes stay inside Mimir.
// Counters created on their first response need no invented starting row:
// the independent per-route summary counts every observed completion.
func apiRouteErrorsQuery(selector string, settings *ApiRouteErrorsSettings) string {
	routes := map[string]bool{}
	for _, item := range apiCustomerRoutes {
		routes[item.route] = true
	}
	routeSelector := selector + ",route=~" + fleetPattern(routes)
	requests := func(extra string) string { return apiRouteRequestsMetric + "{" + routeSelector + extra + "}" }
	duration := apiRouteCountMetric + "{" + routeSelector + "}"
	start := "process_start_time_seconds{" + selector + "}"
	step := stormDuration(settings.Step)
	lookback := stormDuration(settings.Step + settings.Freshness)
	fresh := strconv.FormatInt(int64(settings.Freshness/time.Second), 10)
	stepSeconds := strconv.FormatInt(int64(settings.Step/time.Second), 10)
	slot := "job,host,block"
	byRoute := slot + ",route"
	id := "env,job,service,host,block,instance"
	seen := func(metric string) string {
		return "(count_over_time(" + metric + "[" + lookback + "]) or " + metric + ")"
	}
	group := func(op, labels, expr string) string { return op + " by (" + labels + ") (" + expr + ")" }

	// The ingest adapter stamps a whole registry push with one receive time.
	// Enforce job/service equality and one complete process across the step.
	clocks := []string{}
	for _, job := range []string{"api", "alt"} {
		producer := "process_start_time_seconds{" + selector + ",job=" + strconv.Quote(job) + ",service=" + strconv.Quote(job) + `,instance=~"[0-9a-f]{8}"}`
		clocks = append(clocks, "(timestamp("+producer+") >= time() - "+fresh+")"+
			" and ("+producer+" > 0) and ("+producer+" <= time() - "+stepSeconds+")"+
			" and ("+producer+" == "+producer+" offset "+step+") and (changes("+producer+"["+lookback+"]) == 0)"+
			" and (timestamp("+producer+" offset "+step+") >= time() - "+stepSeconds+" - "+fresh+")")
	}
	clock := group("max", slot, strings.Join(clocks, " or ")) + " and (" + group("count", slot, seen(start)) + " == 1)"
	atPush := func(metric string) string {
		return "(" + metric + " >= 0) and (timestamp(" + metric + ") == on (" + id + ") group_left timestamp(" + start + "))" +
			" and (resets(" + metric + "[" + lookback + "]) == 0)"
	}
	all := requests("")
	valid := requests(`,status=~"none|[1-5][0-9]{2}",outcome=~"completed|canceled|panic|aborted"`)
	children := group("count", byRoute, seen(all))
	qualified := "(" + children + " == " + group("count", byRoute, atPush(valid)) + ") and (" + children + " <= " +
		strconv.Itoa(settings.MaxChildrenPerRoute) + ") and (" + group("count", byRoute, seen(duration)) + " == 1) and (" +
		group("count", byRoute, atPush(duration)) + " == 1)"
	total := group("sum", byRoute, all)
	sparse := func(extra string) string { return group("sum", byRoute, requests(extra)) + " or (0 * " + total + ")" }
	parts := []string{
		fleetPart("clock", clock), fleetPart("qualified", group("group", byRoute, qualified)),
		fleetPart("total", total), fleetPart("failed", sparse(`,status=~"5[0-9]{2}"`)),
		fleetPart("canceled_failed", sparse(`,status=~"5[0-9]{2}",outcome="canceled"`)),
		fleetPart("panic_none", sparse(`,status="none",outcome="panic"`)),
		fleetPart("completed", group("sum", byRoute, duration)),
	}
	if len(settings.VerifiedStatusRevisions) > 0 {
		revisions := map[string]bool{}
		for _, revision := range settings.VerifiedStatusRevisions {
			revisions[revision] = true
		}
		source := "urnetwork_source_info{" + selector + "}"
		verified := "urnetwork_source_info{" + selector + ",revision=~" + fleetPattern(revisions) + `,modified="false",image_digest=~"sha256:[0-9a-f]{64}"}`
		published := "(" + verified + " == 1) and (timestamp(" + verified + ") == on (" + id + ") group_left timestamp(" + start + "))"
		parts = append(parts, fleetPart("status_contract", group("count", slot, published)+" and ("+group("count", slot, seen(source))+" == 1)"))
	}
	return strings.Join(parts, " or ")
}

var apiRouteErrorsShapes = map[string]stormShape{
	"clock":           {keys: []string{"job", "host", "block"}},
	"qualified":       {keys: []string{"job", "host", "block", "route"}},
	"total":           {keys: []string{"job", "host", "block", "route"}},
	"failed":          {keys: []string{"job", "host", "block", "route"}},
	"completed":       {keys: []string{"job", "host", "block", "route"}},
	"canceled_failed": {keys: []string{"job", "host", "block", "route"}},
	"panic_none":      {keys: []string{"job", "host", "block", "route"}},
	"status_contract": {keys: []string{"job", "host", "block"}},
}

// Values share one source push. A non-atomic gather can disagree with the
// summary while the route is busy; that snapshot supplies no recovery credit.
type apiRouteSnapshot struct {
	clock, total, failed, canceledFailed, panicNone float64
	verifiedStatus                                  bool
}

// Read a slot from its desired placement, including a corroborated lazy zero.
func apiRouteSnapshotAt(history *stormHistory, slot fleetSlot, route string, at int64, fresh time.Duration) (apiRouteSnapshot, string) {
	key := slot.label() + "/" + route
	clock := history.reading("clock", slot.label(), at)
	qualified := history.reading("qualified", key, at)
	total := history.reading("total", key, at)
	failed := history.reading("failed", key, at)
	completed := history.reading("completed", key, at)
	canceledFailed := history.reading("canceled_failed", key, at)
	panicNone := history.reading("panic_none", key, at)
	contract := history.reading("status_contract", slot.label(), at)
	switch {
	case !slot.allowed:
		return apiRouteSnapshot{}, "placement_unavailable"
	case !clock.present || !(clock.value > 0 && clock.value <= float64(at) && float64(at)-clock.value <= fresh.Seconds()):
		return apiRouteSnapshot{}, "publisher_clock_unavailable"
	case !qualified.present || qualified.value != 1 || !total.present || !failed.present || !completed.present || !canceledFailed.present || !panicNone.present:
		return apiRouteSnapshot{}, "route_family_incomplete"
	case total.value != completed.value:
		return apiRouteSnapshot{}, "completion_count_disagrees"
	case !(total.value >= 0 && total.value < 1<<53 && math.Trunc(total.value) == total.value &&
		failed.value >= 0 && failed.value <= total.value && math.Trunc(failed.value) == failed.value &&
		canceledFailed.value >= 0 && canceledFailed.value <= failed.value && math.Trunc(canceledFailed.value) == canceledFailed.value &&
		panicNone.value >= 0 && panicNone.value <= total.value-failed.value && math.Trunc(panicNone.value) == panicNone.value):
		return apiRouteSnapshot{}, "invalid_counter"
	}
	return apiRouteSnapshot{clock.value, total.value, failed.value, canceledFailed.value, panicNone.value, contract.present && contract.value == 1}, ""
}

// Rates are summed across placements before the route share is computed.
type apiRouteRate struct{ total, failed, minimumFailed, maximumFailed float64 }

// Every earlier endpoint and original push clock in the rate window is
// required. A disappearing status child cannot borrow a healthy sibling.
func apiRouteRateAt(history *stormHistory, slots []fleetSlot, route string, first, last int, fresh time.Duration) (apiRouteRate, string) {
	totals, failures, minima, maxima := []float64{}, []float64{}, []float64{}, []float64{}
	for _, slot := range slots {
		var start, previous apiRouteSnapshot
		var uncertainCanceled, uncertainPanic float64
		for i := first; i <= last; i++ {
			current, reason := apiRouteSnapshotAt(history, slot, route, history.steps[i], fresh)
			if reason != "" {
				return apiRouteRate{}, reason
			}
			if i == first {
				start = current
			} else {
				deltaFailed := current.failed - previous.failed
				deltaCanceled := current.canceledFailed - previous.canceledFailed
				deltaPanic := current.panicNone - previous.panicNone
				if current.clock <= previous.clock || deltaCanceled < 0 || deltaPanic < 0 || deltaFailed < deltaCanceled || current.total-previous.total < deltaFailed+deltaPanic {
					return apiRouteRate{}, "counter_or_clock_discontinuity"
				}
				if !current.verifiedStatus || !previous.verifiedStatus {
					uncertainCanceled += deltaCanceled
					uncertainPanic += deltaPanic
				}
			}
			previous = current
		}
		elapsed := previous.clock - start.clock
		totals = append(totals, (previous.total-start.total)/elapsed)
		failed := previous.failed - start.failed
		failures = append(failures, failed/elapsed)
		minima = append(minima, (failed-uncertainCanceled)/elapsed)
		maxima = append(maxima, (failed+uncertainPanic)/elapsed)
	}
	return apiRouteRate{contractFailureRateSum(totals), contractFailureRateSum(failures), contractFailureRateSum(minima), contractFailureRateSum(maxima)}, ""
}

// Compare all endpoints of the hold, including the oldest. A low-volume
// route cannot clear an incident, even when its recorded 5xx count is zero.
func apiRouteBand(history *stormHistory, slots []fleetSlot, route string, steps int, share float64, settings *ApiRouteErrorsSettings, gaps map[string]bool) (stormHold, apiRouteRate) {
	var latest apiRouteRate
	start := len(history.steps) - steps - 1
	hold := stormHoldOf(history.steps[start:], stormLevel{threshold: .5}, func(at int64) stormReading {
		index := int((at - history.steps[0]) / int64(settings.Step/time.Second))
		rate, reason := apiRouteRateAt(history, slots, route, index-steps, index, settings.Freshness)
		latest = rate
		if reason != "" {
			gaps[reason] = true
			return stormReading{}
		}
		if rate.total < settings.MinRequestsPerSecond {
			return stormLeftOut
		}
		if rate.minimumFailed/rate.total >= share {
			return stormValue(1)
		}
		if rate.maximumFailed/rate.total < share {
			return stormValue(0)
		}
		gaps["status_contract_unverified"] = true
		return stormReading{}
	})
	return hold, latest
}

// Report holds on independently qualified routes even if a sibling is silent.
func (self apiRouteErrorsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	policy := self.policy()
	envName := env.cfg.env
	unknown := func(reasons, gaps []string) finding {
		f := stormUnobservable(self.id(), apiRouteErrorsUnknown, envName, "§25.3", reasons, gaps, len(apiCustomerRoutes))
		f.mechanism = "A desired route or process lacks a qualified snapshot. A busy registry gather can disagree with the independent completion count. Older router builds encode canceled 5xx or panic statuses differently."
		f.action = "Check desired API and alt placement and each affected route's status counters and duration count at the same push. Check the running build’s status semantics, process overlap, resets and concurrent gathers."
		return f
	}
	if !policy.valid() {
		return []finding{unknown([]string{"invalid_policy"}, nil)}, nil
	}
	inventory := newFleetSlotInventory(env, []string{"api", "alt"}, []string{"alt"})
	selector := inventory.selector(envName)
	if selector == "" {
		return []finding{unknown([]string{"placement_unavailable"}, nil)}, nil
	}
	warnSteps, pageSteps := policy.holdSteps(policy.WarnWindow), policy.holdSteps(policy.PageWindow)
	history, err := readStormHistory(ctx, env, apiRouteErrorsQuery(selector, policy), apiRouteErrorsShapes, 2*warnSteps+1, policy.StormReadSettings)
	if err != nil {
		if canceled := probeLifecycleError(ctx, err); canceled != nil {
			return nil, canceled
		}
		return []finding{stormReadFailureFinding(unknown([]string{stormReadReason(err)}, nil), err)}, nil
	}
	if reasons := history.invalidReasons(); len(reasons) != 0 {
		return []finding{unknown(reasons, nil)}, nil
	}
	byJob := map[string][]fleetSlot{}
	for _, slot := range inventory.slots {
		byJob[slot.service] = append(byJob[slot.service], slot)
	}
	findings := []finding{}
	gaps := append([]string(nil), inventory.missingServices...)
	reasonSet := map[string]bool{}
	if len(gaps) > 0 {
		reasonSet["placement_unavailable"] = true
	}
	for _, job := range []string{"api", "alt"} {
		slots := byJob[job]
		if len(slots) == 0 {
			continue
		}
		for _, item := range apiCustomerRoutes {
			target := envName + "/" + job + "/" + item.name
			warn, warnRate := apiRouteBand(history, slots, item.route, warnSteps, policy.WarnShare, policy, reasonSet)
			page, pageRate := apiRouteBand(history, slots, item.route, pageSteps, policy.PageShare, policy, reasonSet)
			if !warn.complete || !page.complete {
				gaps = append(gaps, job+"/"+item.name)
			}
			switch {
			case page.held || warn.held:
				tier, rate, window := tierWarn, warnRate, policy.WarnWindow
				if page.held {
					tier, rate, window = tierPage, pageRate, policy.PageWindow
				}
				findings = append(findings, finding{
					probeId: self.id(), class: apiRouteErrorsClass, target: target, tier: tier, sustain: 1,
					symptom:   "Customer-route HTTP 5xx responses exceed the sustained error share",
					mechanism: "The router records the first response status and the terminal outcome. A live caller can receive a 5xx even when the handler finishes canceled.",
					baseline:  fmt.Sprintf("WARN at %.2f%% for %s, PAGE at %.2f%% for %s, with at least %.2f completed requests/s on the route.", 100*policy.WarnShare, policy.WarnWindow, 100*policy.PageShare, policy.PageWindow, policy.MinRequestsPerSecond),
					observed:  fmt.Sprintf("route=%s job=%s errors_per_second=%.3f requests_per_second=%.3f share=%.4f minimum_share=%.4f maximum_share=%.4f window=%s slots=%d evaluation_unix=%d", item.route, job, rate.failed, rate.total, rate.failed/rate.total, rate.minimumFailed/rate.total, rate.maximumFailed/rate.total, window, len(slots), history.at()),
					evidence:  "A bounded Mimir range supplies source-clocked cumulative status counters and the independent router duration count for every placed slot.",
					context:   "Caller-gone writes can have status none. An unfinished handler, a request refused upstream or an error returned in a 200 body can escape this share.",
					action:    "Inspect the route's dependency, API or alt rollout and caller cancellations. Compare provider selection, authentication or contract creation with the corresponding service evidence.",
					verify:    fmt.Sprintf("Every placed slot supplies matching completed-request mass. The eligible %s error share stays below %.2f%% through the warning hold and the short-window page has cleared.", policy.WarnWindow, 100*policy.WarnShare),
					playbook:  "SIGNALS.md §25.3",
				})
			case warn.clear && page.clear:
				findings = append(findings, healthyFinding(self.id(), tierPage, apiRouteErrorsClass, target))
			default:
				findings = append(findings, stormClearHold(self.id(), apiRouteErrorsClass, target))
			}
		}
	}
	if len(gaps) != 0 {
		sort.Strings(gaps)
		reasons := []string{"route_coverage_incomplete"}
		for reason := range reasonSet {
			reasons = append(reasons, reason)
		}
		findings = append(findings, unknown(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(self.id(), tierWarn, apiRouteErrorsUnknown, envName))
	}
	return findings, nil
}
