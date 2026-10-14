// The operator ingest behind the prober's /network/provider-* routes. The API
// handlers keep only authentication, body decoding and the response; the
// taskworker's in-process operator (server/localoperator) calls these same
// functions, so both transports validate, limit and store identically.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// A request the operator ingest refuses, with the status and message the
// operator API answers it with. The handlers write it unchanged; the in-process
// operator returns it as the error the HTTP client would have made of that
// response, so callers cannot tell the transports apart.
type ProviderEgressIngestRefusal struct {
	Status  int
	Message string
	// Set only on a 429: how long until the refused budget renews.
	RetryAfter time.Duration
}

func (self *ProviderEgressIngestRefusal) Error() string {
	return self.Message
}

// A 400 with the message the handler has always written.
func refuseProviderEgressIngest(message string) error {
	return &ProviderEgressIngestRefusal{Status: http.StatusBadRequest, Message: message}
}

// The refusal an error carries, or nil when it is not one.
func ProviderEgressIngestRefusalOf(err error) *ProviderEgressIngestRefusal {
	var refusal *ProviderEgressIngestRefusal
	if errors.As(err, &refusal) {
		return refusal
	}
	return nil
}

// The URL due queue.

const (
	// The batch size when the caller does not ask for one.
	DefaultProviderEgressDueLimit = 100
	// The fallback ceiling on one due batch when provider_egress_due.yml is
	// absent or unusable, so one request cannot ask the database for the whole
	// provider population. The effective value is ProviderEgressDueLimitCeiling.
	DefaultMaxProviderEgressDueLimit = 500
)

// The ceiling on one due batch, read from provider_egress_due.yml.
//
// It is configuration, not a constant, for the same reason the bandwidth budget
// is (see provider_bandwidth.yml): capacity is a property of the deployment.
// The 500 fallback is sized for a beta-scale fleet. A deployment holding ~100k
// providers needs a different regime entirely -- at a 6h re-probe backoff that
// is ~16,700 probes/hour, and a 500-per-request ceiling caps the whole fleet at
// 500/hour no matter how much prober capacity exists behind it.
//
// Optional, exactly like provider_bandwidth.yml and pro.yml: a deployment
// without the file must not fail to boot, it falls back to the conservative
// default. Each process reads it once.
var ProviderEgressDueLimitCeiling = sync.OnceValue(ReadMaxProviderEgressDueLimit)

// The uncached reader gives synthetic configuration tests the same parsing
// path that each process memoizes at startup.
func ReadMaxProviderEgressDueLimit() int {
	resource, err := server.Config.SimpleResource("provider_egress_due.yml")
	if err != nil {
		glog.Infof(
			"[pegl]provider_egress_due.yml not present; using the default due limit of %d\n",
			DefaultMaxProviderEgressDueLimit,
		)
		return DefaultMaxProviderEgressDueLimit
	}
	var y struct {
		MaxDueLimit int `yaml:"max_due_limit"`
	}
	resource.UnmarshalYaml(&y)
	if y.MaxDueLimit <= 0 {
		glog.Errorf(
			"[pegl]provider_egress_due.yml has max_due_limit=%d, which is not usable; using the default %d\n",
			y.MaxDueLimit,
			DefaultMaxProviderEgressDueLimit,
		)
		return DefaultMaxProviderEgressDueLimit
	}
	glog.Infof("[pegl]max due limit: %d from provider_egress_due.yml\n", y.MaxDueLimit)
	return y.MaxDueLimit
}

// Validates an explicit URL Due limit and clamps it to the configured ceiling.
// Not clamped up to 1: limit=0 would come back as an empty list, which the
// prober cannot distinguish from "nothing is due".
func ProviderUrlProbeDueLimit(limit int) (int, error) {
	if limit < 1 {
		return 0, refuseProviderEgressIngest("Bad request")
	}
	return min(limit, ProviderEgressDueLimitCeiling()), nil
}

// shard_index / shard_count partition the URL queue across independent
// workers; durable task rows own these slices rather than particular hosts.
// An out-of-range index would silently return nothing forever, which looks
// identical to "the fleet is fully probed", so it is refused instead.
func ValidateProviderUrlProbeDueShard(shardIndex, shardCount int) error {
	if shardCount < 1 || model.ProviderUrlProbeSlotCount < shardCount {
		return refuseProviderEgressIngest("Bad request")
	}
	if shardIndex < 0 || shardCount <= shardIndex {
		return refuseProviderEgressIngest("Bad request")
	}
	return nil
}

// The model's URL claim, injectable so transport tests need no database.
type ProviderUrlProbeDueClaim func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult

// One already-validated URL Due claim, observed into timing exactly as the
// handler observes its own, minus the response encoding an in-process caller
// does not have. A model panic propagates after its partial timings are kept.
func ClaimProviderUrlProbeDueObserved(
	ctx context.Context,
	limit, shardIndex, shardCount int,
	claim ProviderUrlProbeDueClaim,
	timing *ProviderUrlProbeDueTiming,
) model.ProviderUrlProbeDueResult {
	started := time.Now()
	var observation model.ProviderUrlProbeDueObservation
	outcome := ProviderUrlProbeDuePanic
	defer func() { timing.Observe(observation, time.Since(started), server.DbTimingSample{}, outcome) }()
	// Stored timestamps hold UTC without a timezone; keep the same UTC clock.
	result := claim(ctx, server.NowUtc(), limit, shardIndex, shardCount, &observation)
	if result.PriorityMaintenancePending {
		outcome = ProviderUrlProbeDueMaintenance
	} else {
		outcome = ProviderUrlProbeDueReturned
	}
	return result
}

// The blackhole due list and checks.

const (
	// A batch when the caller names none. Larger than the egress-location
	// default because a blackhole check is a single request through an
	// already-required tunnel rather than a ~131 destination sweep -- the whole
	// point is that a batch of these is cheap.
	DefaultProviderBlackholeDueLimit = 500
	// Caps one batch. The sweep is meant to cover the fleet hourly, so this is
	// sized to let a deployment do that in a few requests rather than hundreds.
	MaxProviderBlackholeDueLimit = 5000
	// Bounds one submitted batch, so a malformed or hostile body cannot make the
	// server hold an unbounded slice. It is above MaxProviderBlackholeDueLimit
	// so a caller can always report a full batch it was legitimately handed.
	MaxProviderBlackholeChecksPerRequest = 10000
)

// One provider of a due list, with the place it is published under: the prober
// draws a provider's sample only from the destinations compatible with that
// place, so a site blocked in a country never counts against that country's
// exits (connect/GEOMAP.md §11.3). Both place fields are empty for a provider
// the reliability rollup has not placed, which excludes nothing.
type ProviderEgressDueProvider struct {
	ClientId server.Id `json:"client_id"`
	// CountryCode is lowercase alpha-2.
	CountryCode string `json:"country_code,omitempty"`
	Region      string `json:"region,omitempty"`
}

// The retained diagnostic due-list shape, served by the blackhole due route.
// The URL due route additionally returns the token, quota deficit and ordinal
// through model.ProviderUrlProbeDue; this shape can read only its place subset.
type ProviderEgressLocationDueResult struct {
	Providers []ProviderEgressDueProvider `json:"providers"`
}

// Joins a due list, in its order, to the places its providers are published
// under.
func ProviderEgressDueProviders(ctx context.Context, clientIds []server.Id) []ProviderEgressDueProvider {
	places := model.GetProviderEgressPlaces(ctx, clientIds)
	providers := make([]ProviderEgressDueProvider, 0, len(clientIds))
	for _, clientId := range clientIds {
		place := places[clientId]
		providers = append(providers, ProviderEgressDueProvider{
			ClientId:    clientId,
			CountryCode: place.CountryCode,
			Region:      place.Region,
		})
	}
	return providers
}

// Serves the sweep: which providers to check next -- oldest-due existing checks
// interleaved with first checks, lending unused share to the other class and
// retaining retry priority for one slot (connect/GEOMAP.md §11.3) -- each with
// the place it is published under, so the check draws only connectivity
// destinations compatible with it.
//
// limit must be positive (limit=0 returns an empty list, which the caller
// cannot tell apart from "nothing is due") and is clamped to
// MaxProviderBlackholeDueLimit. An out-of-range shard index returns nothing
// forever, which looks exactly like "the fleet is fully checked", so it is
// refused too.
func ProviderBlackholeCheckDue(ctx context.Context, limit, shardIndex, shardCount int) (*ProviderEgressLocationDueResult, error) {
	if limit < 1 {
		return nil, refuseProviderEgressIngest("Bad request")
	}
	limit = min(limit, MaxProviderBlackholeDueLimit)
	if shardCount < 1 {
		return nil, refuseProviderEgressIngest("Bad request")
	}
	if shardIndex < 0 || shardCount <= shardIndex {
		return nil, refuseProviderEgressIngest("Bad request")
	}
	// computed here and passed in: next_due_at and checked_at are naive
	// timestamps holding utc, so comparing them to sql now() in the query
	// would cast through the session timezone
	clientIds := model.GetProviderBlackholeCheckDue(
		ctx,
		server.NowUtc(),
		limit,
		shardIndex,
		shardCount,
	)
	return &ProviderEgressLocationDueResult{Providers: ProviderEgressDueProviders(ctx, clientIds)}, nil
}

// One provider's result. Kept to the single question the check answers: did
// anything get through.
type SubmitProviderBlackholeCheckArgs struct {
	ClientId server.Id `json:"client_id"`
	OK       bool      `json:"ok"`
	// Failure is a short class when OK is false: tunnel_failed,
	// all_destinations_failed, and so on. Ignored when OK.
	Failure string `json:"failure,omitempty"`
	// NotMeasured is a check none of whose loads could be measured: its tunnel
	// was gone and could not be re-created. It is not a verdict -- ok is false
	// and failure not_measured only because those fields must say something --
	// and it only reschedules the provider (model.NextProviderBlackholeCheck).
	NotMeasured bool      `json:"not_measured,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

// A batch of results.
//
// Batched, unlike the egress-location submission, because the sweep produces
// hundreds of one-bit answers per pass and a request each would spend more on
// http than on the checks themselves.
type SubmitProviderBlackholeChecksArgs struct {
	Checks []SubmitProviderBlackholeCheckArgs `json:"checks"`
}

// Validates the whole batch before writing any of it, then records it. A
// partial write would leave the caller unable to tell which results landed,
// and it reports fire-and-forget.
func SubmitProviderBlackholeChecks(ctx context.Context, args *SubmitProviderBlackholeChecksArgs) error {
	if len(args.Checks) == 0 {
		return refuseProviderEgressIngest("Missing checks.")
	}
	if MaxProviderBlackholeChecksPerRequest < len(args.Checks) {
		return refuseProviderEgressIngest("Too many checks.")
	}

	now := server.NowUtc()

	for _, check := range args.Checks {
		if check.ClientId == (server.Id{}) {
			return refuseProviderEgressIngest("Missing client_id.")
		}
		if check.CheckedAt.IsZero() {
			// never fabricated here: an "as of now" timestamp would defeat the
			// freshness bound the gate depends on, and could pin a stale answer
			return refuseProviderEgressIngest("Missing checked_at.")
		}
		// a future-dated check would outlive every real one under the monotonic
		// upsert and could never be corrected
		if now.Add(time.Minute).Before(check.CheckedAt) {
			return refuseProviderEgressIngest("checked_at is in the future.")
		}
		if model.MaxProviderBlackholeFailureLen < len(check.Failure) {
			return refuseProviderEgressIngest("Failure class too long.")
		}
		if strings.TrimSpace(check.Failure) == "" && !check.OK {
			return refuseProviderEgressIngest("A failed check must name a failure class.")
		}
	}

	reports := make([]model.ProviderBlackholeCheckReport, 0, len(args.Checks))
	for _, check := range args.Checks {
		reports = append(reports, model.ProviderBlackholeCheckReport{
			ClientId:    check.ClientId,
			CheckedAt:   check.CheckedAt,
			Ok:          check.OK,
			Failure:     check.Failure,
			NotMeasured: !check.OK && check.NotMeasured,
		})
	}
	// one failed check is a failure, not a verdict: the model keeps the run of
	// consecutive failures and the backoff that schedules the next check
	model.RecordProviderBlackholeChecks(ctx, reports, model.GetProviderEgressRules())
	return nil
}

// Egress health.

// The class name that must never appear inside class_results. See
// SubmitProviderEgressHealth for why this is checked rather than tolerated.
const providerEgressHealthReputationClass = "reputation"

// One class's ok/total tally over the destinations the run sampled.
type ProviderEgressHealthClassResult struct {
	OK    int `json:"ok"`
	Total int `json:"total"`
}

// One immutable URL outcome, or an older aggregate retained only for audit.
// Validated URL evidence preserves its measurement time rather than arrival
// time, so delayed publication cannot refresh rolling eligibility. Timestamps
// more than one minute in the future are rejected. Legacy evidence without a
// policy timestamp uses arrival time but cannot satisfy the URL-policy gate.
type SubmitProviderEgressHealthArgs struct {
	ClientId         server.Id                      `json:"client_id"`
	RunId            server.Id                      `json:"run_id"`
	CycleStartedAt   time.Time                      `json:"cycle_started_at,omitempty"`
	UrlProbeEvidence *egresshealth.UrlProbeEvidence `json:"url_probe_evidence,omitempty"`
	// OKCount/TotalCount cover every class over the measured scored loads. A
	// load that was not measured, and a canary, is in neither.
	OKCount    int `json:"ok_count"`
	TotalCount int `json:"total_count"`
	// ClassResults is the per-class tally. Its ok and total must sum to
	// exactly OKCount and TotalCount.
	ClassResults map[string]ProviderEgressHealthClassResult `json:"class_results"`
	// ReputationOK/ReputationTotal/ReputationFailedNames are the dissolved
	// reputation class's: its sites are scored with the rest now. They are
	// accepted, always zero from a current prober, and ignored.
	ReputationOK          int    `json:"reputation_ok"`
	ReputationTotal       int    `json:"reputation_total"`
	FailedNames           string `json:"failed_names"`
	ReputationFailedNames string `json:"reputation_failed_names"`
	// TLSAuthenticationFailure is separate from the score. A single peer that
	// cannot authenticate the requested HTTPS host is a hard integrity failure,
	// even when unrelated destinations make ok_count/total_count look healthy.
	TLSAuthenticationFailure bool `json:"tls_authentication_failure"`
	// NotMeasuredCount/NotMeasuredNames are the loads whose tunnel was gone and
	// could not be re-created in time: already out of ok_count and
	// total_count, and never counted against the exit.
	NotMeasuredCount int    `json:"not_measured_count"`
	NotMeasuredNames string `json:"not_measured_names"`
	// CanaryPassedNames/CanaryFailedNames are the unscored canaries of the
	// run, loaded from a place their site is marked incompatible with.
	CanaryPassedNames string `json:"canary_passed_names"`
	CanaryFailedNames string `json:"canary_failed_names"`
	// ShortClasses is the comma-joined classes too thin, for the provider's
	// place, to fill their sample.
	ShortClasses string `json:"short_classes"`
}

// Validates and stores one egress-health run from the operator's prober.
//
// # Everything is validated before anything is stored
//
// The row is an upsert keyed on client_id, so a bad submission does not sit
// beside the good one waiting to be noticed -- it destroys the last good
// measurement for that provider. That is why every rule below is refused
// before the store, and why none of them is a stored-then-flagged warning.
//
// # What counts
//
// The counts are the prober's, over the loads it measured after their
// retries, with one server-side cut: a failed load of a site the pool has on
// probation, or of one marked incompatible with the provider's place, leaves
// the counts and is named in unscored_failed_names (model.
// ScoreProviderEgressHealth). The probe task submits runs already cut this way,
// passes included; the cut here is what the wire allows for any other
// submitter, since a run names its failures and not its passes.
//
// A "reputation" key inside class_results is still rejected outright. The
// class dissolved into the sites, so no current prober sends one; a body that
// carries one is from a prober that would have scored the class twice, and the
// sum check below would otherwise be "fixed" by relaxing it.
func SubmitProviderEgressHealth(ctx context.Context, args *SubmitProviderEgressHealthArgs) error {
	if args.ClientId == (server.Id{}) {
		return refuseProviderEgressIngest("Missing client id.")
	}
	if err := args.UrlProbeEvidence.ValidateOutcome(args.OKCount, args.TotalCount, args.TLSAuthenticationFailure); err != nil {
		return refuseProviderEgressIngest(err.Error())
	}
	if args.UrlProbeEvidence != nil && args.UrlProbeEvidence.MeasuredAt.After(server.NowUtc().Add(time.Minute)) {
		return refuseProviderEgressIngest("URL evidence timestamp is in the future.")
	}
	if args.UrlProbeEvidence != nil && args.RunId == (server.Id{}) {
		return refuseProviderEgressIngest("A versioned URL result requires an immutable run id.")
	}
	securityOnly := args.TotalCount == 0 && args.UrlProbeEvidence != nil && len(args.UrlProbeEvidence.Security) > 0
	if !args.CycleStartedAt.IsZero() && (args.RunId == (server.Id{}) || args.UrlProbeEvidence == nil || (args.TotalCount != 1 && !securityOnly)) {
		return refuseProviderEgressIngest("A scheduled URL probe requires a run id and versioned evidence for one measured outcome or independent TLS security evidence.")
	}
	if args.UrlProbeEvidence != nil {
		tlsFailure := false
		for _, event := range args.UrlProbeEvidence.Security {
			tlsFailure = tlsFailure || event.TlsFailure
		}
		if tlsFailure != args.TLSAuthenticationFailure {
			return refuseProviderEgressIngest("URL security evidence disagrees with the aggregate TLS flag.")
		}
	}
	if args.OKCount < 0 || args.TotalCount < 0 {
		return refuseProviderEgressIngest("ok_count and total_count must be non-negative.")
	}
	if args.ReputationOK < 0 || args.ReputationTotal < 0 {
		return refuseProviderEgressIngest("reputation_ok and reputation_total must be non-negative.")
	}
	if args.NotMeasuredCount < 0 {
		return refuseProviderEgressIngest("not_measured_count must be non-negative.")
	}
	if args.TotalCount < args.OKCount {
		// more destinations passed than were attempted: the submitter is not
		// measuring what it thinks it is, and storing this would overwrite a
		// real measurement with an impossible one
		return refuseProviderEgressIngest("ok_count must not exceed total_count.")
	}
	if args.ReputationTotal < args.ReputationOK {
		return refuseProviderEgressIngest("reputation_ok must not exceed reputation_total.")
	}

	sumOK, sumTotal := 0, 0
	for class, tally := range args.ClassResults {
		if class == providerEgressHealthReputationClass {
			return refuseProviderEgressIngest(fmt.Sprintf(
				"%q is not a scored class: it is reported in reputation_ok/reputation_total and must never be part of ok_count/total_count.",
				providerEgressHealthReputationClass,
			))
		}
		if tally.OK < 0 || tally.Total < 0 {
			return refuseProviderEgressIngest(fmt.Sprintf("class %q: ok and total must be non-negative.", class))
		}
		if tally.Total < tally.OK {
			// checked per class as well as in aggregate: {ok:5,total:2} and
			// {ok:0,total:3} sum to a consistent 5/5 while describing a class
			// where more destinations passed than ran
			return refuseProviderEgressIngest(fmt.Sprintf("class %q: ok must not exceed total.", class))
		}
		sumOK += tally.OK
		sumTotal += tally.Total
	}
	// exact equality, not <=: the classes are the score. A total that does not
	// decompose into its classes means the two halves of the payload were
	// produced by different runs, or that something not in class_results was
	// counted into the score -- which is precisely how reputation would get in.
	if sumOK != args.OKCount || sumTotal != args.TotalCount {
		return refuseProviderEgressIngest(fmt.Sprintf(
			"class_results sum to %d/%d but ok_count/total_count are %d/%d.",
			sumOK, sumTotal, args.OKCount, args.TotalCount,
		))
	}

	classResults := map[string]model.ProviderEgressHealthClassResult{}
	for class, tally := range args.ClassResults {
		classResults[class] = model.ProviderEgressHealthClassResult{
			OK:    tally.OK,
			Total: tally.Total,
		}
	}

	health := &model.ProviderEgressHealth{
		ClientId:         args.ClientId,
		RunId:            args.RunId,
		CycleStartedAt:   args.CycleStartedAt,
		MeasuredAt:       server.NowUtc(),
		OKCount:          args.OKCount,
		Total:            args.TotalCount,
		ClassResults:     classResults,
		UrlProbeEvidence: args.UrlProbeEvidence,
		// the reputation class dissolved into the sites (GEOMAP §11.3):
		// whatever an older prober sends here is not a measurement anything
		// reads, and is stored as none
		ReputationOK:             0,
		ReputationTotal:          0,
		FailedNames:              args.FailedNames,
		ReputationFailedNames:    "",
		TLSAuthenticationFailure: args.TLSAuthenticationFailure,
		NotMeasuredCount:         args.NotMeasuredCount,
		NotMeasuredNames:         args.NotMeasuredNames,
		CanaryPassedNames:        args.CanaryPassedNames,
		CanaryFailedNames:        args.CanaryFailedNames,
		ShortClasses:             args.ShortClasses,
	}
	if args.UrlProbeEvidence != nil {
		health.MeasuredAt = args.UrlProbeEvidence.MeasuredAt
	}
	// Preserve legacy scoring semantics only for legacy diagnostic reports.
	// Every versioned URL error remains in its policy-version denominator.
	if args.UrlProbeEvidence == nil {
		place := model.GetProviderEgressPlaces(ctx, []server.Id{args.ClientId})[args.ClientId]
		health = model.ScoreProviderEgressHealth(health, place, model.GetProviderEgressHealthScoring(ctx))
	}

	model.SetProviderEgressHealth(ctx, health)
	return nil
}

// Active bandwidth.

// An active bandwidth measurement taken by the prober over a provider's tunnel.
type SubmitProviderBandwidthArgs struct {
	ClientId server.Id `json:"client_id"`
	// Source names which target produced this figure
	// (model.ProviderBandwidthSourceActiveOperator or ...ActiveCDN). It is
	// part of the storage key, so it is what keeps the two targets' figures in
	// separate rows instead of overwriting each other.
	Source          string  `json:"source"`
	BytesPerSecond  float64 `json:"bytes_per_second"`
	SampleByteCount int64   `json:"sample_byte_count"`
}

// Stores an active bandwidth measurement. An active probe is a point
// measurement rather than an aggregate over a window, so window_start and
// window_end are both the arrival time.
//
// A non-positive rate or sample size is not a usable measurement, and storing
// one would overwrite a real figure with a meaningless one -- so those are
// refused before anything is written.
//
// The source is validated against the known active set rather than merely
// stored. An unrecognised source creates a row nothing will ever read or
// replace, and "passive" is refused specifically: that figure is derived
// server-side from bytes the provider has already been paid to carry, which is
// exactly what makes it ungameable.
func SubmitProviderBandwidth(ctx context.Context, args *SubmitProviderBandwidthArgs) error {
	if args.ClientId == (server.Id{}) {
		return refuseProviderEgressIngest("Missing client id.")
	}
	if args.BytesPerSecond <= 0 {
		return refuseProviderEgressIngest("bytes_per_second must be positive.")
	}
	if args.SampleByteCount <= 0 {
		return refuseProviderEgressIngest("sample_byte_count must be positive.")
	}
	if !model.IsSubmittableProviderBandwidthSource(args.Source) {
		return refuseProviderEgressIngest(fmt.Sprintf(
			"source must be one of %s, %s.",
			model.ProviderBandwidthSourceActiveOperator,
			model.ProviderBandwidthSourceActiveCDN,
		))
	}

	now := server.NowUtc()
	model.StoreProviderBandwidth(ctx, &model.ProviderBandwidth{
		ClientId:        args.ClientId,
		BytesPerSecond:  args.BytesPerSecond,
		Source:          args.Source,
		SampleByteCount: args.SampleByteCount,
		WindowStart:     now,
		WindowEnd:       now,
	})
	return nil
}

// Asks for budget to run one active probe.
type ReserveProviderBandwidthArgs struct {
	ClientId  server.Id `json:"client_id"`
	ByteCount int64     `json:"byte_count"`
}

// The reservation the prober just took. BucketStart is always the current
// hourly bucket (see ReserveProviderBandwidth).
type ReserveProviderBandwidthResult struct {
	ReservationId server.Id `json:"reservation_id"`
	BucketStart   time.Time `json:"bucket_start"`
}

// A 429 that names when the budget renews.
func refuseProviderBandwidthBudget(retryAfter time.Duration, message string) error {
	return &ProviderEgressIngestRefusal{Status: http.StatusTooManyRequests, Message: message, RetryAfter: retryAfter}
}

// Reserves deployment-wide byte budget for one active bandwidth probe. Active
// probing pulls real data through a provider's tunnel, which is real paid
// contract traffic, so it is rationed (model.ReserveProviderBandwidthSlot).
//
// The prober measures over a tunnel it already has open, right now: it has no
// use for budget in a later hour. model.ReserveProviderBandwidthSlot will
// happily defer a reservation into a future bucket for callers that can
// schedule a RunAt, so when it does that here the reservation is cancelled
// again and refused with a 429 -- the hourly ceiling would otherwise be
// decorative, since the prober could spend the whole daily budget inside one
// hour. RetryAfter points at the bucket that does have room.
func ReserveProviderBandwidth(ctx context.Context, args *ReserveProviderBandwidthArgs) (*ReserveProviderBandwidthResult, error) {
	if args.ClientId == (server.Id{}) {
		return nil, refuseProviderEgressIngest("Missing client id.")
	}
	if args.ByteCount <= 0 {
		return nil, refuseProviderEgressIngest("byte_count must be positive.")
	}
	// the byte count is caller-supplied; a probe never legitimately needs more
	// than the per-probe figure, and an oversized request must not be able to
	// swallow a large slice of a bucket in one reservation
	byteCount := args.ByteCount
	if model.MaxProviderBandwidthBytesPerProbe < byteCount {
		byteCount = model.MaxProviderBandwidthBytesPerProbe
	}

	now := server.NowUtc()
	currentBucketStart := now.UTC().Truncate(model.ProviderBandwidthBucketDuration)

	reservationId, bucketStart, err := model.ReserveProviderBandwidthSlot(ctx, args.ClientId, byteCount)
	if err != nil {
		// every bucket in the lookahead window is full: the deployment's daily
		// budget is exhausted
		return nil, refuseProviderBandwidthBudget(currentBucketStart.Add(model.ProviderBandwidthBucketDuration).Sub(now), err.Error())
	}
	if bucketStart.After(currentBucketStart) {
		// budget exists, but not until a later hour -- of no use to a probe
		// that runs now, so give it back rather than burning it on a request
		// that is about to be skipped
		model.CancelProviderBandwidthReservation(ctx, reservationId)
		return nil, refuseProviderBandwidthBudget(bucketStart.Sub(now), "The active bandwidth probe budget for this hour has been reached.")
	}
	return &ReserveProviderBandwidthResult{
		ReservationId: reservationId,
		BucketStart:   bucketStart,
	}, nil
}
