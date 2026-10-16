// Package localoperator runs the provider prober's operator ingest in-process:
// the same controller and model calls the operator API handlers make, without
// the HTTP round trip, the shared-secret check or the JSON encoding. It is the
// prober-ingest counterpart of localclient, which does the same for hosted
// client control.
//
// The process that uses it is trusted with the database already, so the
// operator secret, which only ever conveyed "this caller is the operator", has
// nothing left to prove. Everything else the API applies still applies, in the
// same order: the ingest package's client-side refusals and truncations, then
// the controller's validation, limits and transactions. Refusals and failures
// come back as the errors ingest.Client returns for the answers the API would
// have given, so a pass cannot tell the transports apart.
//
// What the API fleet gave these calls for free, the operator supplies itself:
// a bound on how many of its calls hold the host's database at once, and the
// per-method call series (metrics.go) that stand in for the router's per-route
// ones.
package localoperator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/bandwidth"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/ingest"
)

// One shard's in-process operator. Safe for concurrent use.
//
// Over HTTP these calls held the API fleet's database connections; in-process
// they hold the host's own, which in the taskworker every other task shares.
// So each operator owns a fixed number of call slots, sized by New from its
// pass's settings: at most that many of its calls run at once, and a further
// call waits for a slot within its own call bound, as a request waits for a
// connection. Slots are never shared between operators, so one pass's burst
// cannot take another's.
type Operator struct {
	// The slice of the due queues this operator claims, with ingest.Client's
	// semantics: a count of 1 or less takes the whole queue.
	ShardIndex int
	ShardCount int
	// Bounds each call the way the HTTP client's timeout bounds a request, so a
	// stalled database cannot hold a pass longer than an unanswered request
	// could. Zero leaves only the caller's deadline. A call's wait for a slot
	// counts against it.
	CallTimeout time.Duration

	// One token per running call; its capacity is the operator's concurrency.
	callSlots chan struct{}
	// The per-method call series this process publishes; nil publishes nothing.
	metrics *operatorMetrics
	// The URL Due phase timing this process publishes; nil publishes nothing.
	dueTiming *controller.ProviderUrlProbeDueTiming
	// The model's URL claim; nil is model.ClaimProviderUrlProbeDueWithObservation.
	// Only tests replace it.
	claimDue controller.ProviderUrlProbeDueClaim
	// Called when a call finds every slot taken, just before it waits. Only
	// tests set it.
	slotWait func()
}

var _ ingest.Operator = (*Operator)(nil)

// What a call of an Operator that New did not build returns: it has no slots
// to run in.
var errNoCallSlots = errors.New("localoperator: the operator was not built by New")

// The process publishes the API's URL Due timing series under the same names,
// adopting the API's collectors when one binary holds both.
var processDueTiming = sync.OnceValue(func() *controller.ProviderUrlProbeDueTiming {
	return controller.RegisterProviderUrlProbeDueTiming(prometheus.DefaultRegisterer)
})

// An operator for one shard whose calls each run within callTimeout, at most
// concurrency of them at once. Concurrency must be positive.
func New(shardIndex int, shardCount int, callTimeout time.Duration, concurrency int) (*Operator, error) {
	if concurrency < 1 {
		return nil, fmt.Errorf("localoperator: concurrency must be positive (got %d)", concurrency)
	}
	return &Operator{
		ShardIndex:  shardIndex,
		ShardCount:  shardCount,
		CallTimeout: callTimeout,
		callSlots:   make(chan struct{}, concurrency),
		metrics:     processMetrics(),
		dueTiming:   processDueTiming(),
	}, nil
}

// What the API's router answers when a handler panics: 503 for a done-error
// (a child deadline or cancellation inside the handler while the caller is
// still waiting), 500 for anything else. Cause keeps the value for logs and
// errors.Is; the HTTP client only ever saw the status.
type handlerFailure struct {
	status int
	cause  error
}

func (self *handlerFailure) Error() string {
	return fmt.Sprintf("status %d: %s", self.status, self.cause)
}

func (self *handlerFailure) Unwrap() error {
	return self.cause
}

// A request the handler would have refused with a status and message.
type handlerRefusal struct {
	status  int
	message string
	// Set only on a 429.
	retryAfter time.Duration
}

func (self *handlerRefusal) Error() string {
	return fmt.Sprintf("status %d: %s", self.status, self.message)
}

// The refusal a controller error answers: a controller refusal keeps its
// status, any other error is the 400 the handlers answer it with.
func refusalOf(err error) error {
	if err == nil {
		return nil
	}
	if refusal := controller.ProviderEgressIngestRefusalOf(err); refusal != nil {
		return &handlerRefusal{status: refusal.Status, message: refusal.Message, retryAfter: refusal.RetryAfter}
	}
	return &handlerRefusal{status: http.StatusBadRequest, message: err.Error()}
}

// A body field the handler's JSON decoder would have refused.
func badRequest() error {
	return &handlerRefusal{status: http.StatusBadRequest, message: "Bad request"}
}

// Parses an id exactly as server.Id's JSON decoding does: the canonical
// 36-character form only. Anything else is the handler's "Bad request".
func wireId(value string) (server.Id, error) {
	if len(value) != 36 {
		return server.Id{}, badRequest()
	}
	id, err := connect.ParseId(value)
	if err != nil {
		return server.Id{}, badRequest()
	}
	return server.Id(id), nil
}

// Runs one operation of method under the call bound, in one of the operator's
// slots, and records it in the method's call series. A caller that is
// canceled or out of time, while it waits for a slot or while it runs, gets
// its context error, as an abandoned request would; a panic becomes the
// handlerFailure the router would have answered. Returned errors are the
// operation's own (nil, a handlerRefusal, or an ingest sentinel).
func (self *Operator) run(ctx context.Context, method string, operation func(context.Context) error) (err error) {
	if self.callSlots == nil {
		return errNoCallSlots
	}
	if 0 < self.CallTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, self.CallTimeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		self.metrics.unadmitted(method, 0)
		return err
	}
	waitStarted := time.Now()
	select {
	case self.callSlots <- struct{}{}:
	default:
		err := func() error {
			endWait := self.metrics.beginWait(method)
			defer endWait()
			if self.slotWait != nil {
				self.slotWait()
			}
			select {
			case self.callSlots <- struct{}{}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}()
		if err != nil {
			self.metrics.unadmitted(method, time.Since(waitStarted))
			return err
		}
	}
	// the slot frees after the call's series record its end
	defer func() {
		<-self.callSlots
	}()
	endCall := self.metrics.beginCall(method, time.Since(waitStarted))
	defer func() {
		endCall(err)
	}()
	server.HandleError(func() {
		err = operation(ctx)
	}, func(recovered error) {
		switch {
		case ctx.Err() != nil:
			err = ctx.Err()
		case server.IsDoneError(recovered):
			err = &handlerFailure{status: http.StatusServiceUnavailable, cause: recovered}
		default:
			err = &handlerFailure{status: http.StatusInternalServerError, cause: recovered}
		}
	})
	return err
}

// The ingest error a submission or due read returns for a refusal or failure,
// as ingest.Client builds it from the response. Context and ingest errors pass
// through unchanged.
func rejected(err error) error {
	switch v := err.(type) {
	case *handlerRefusal:
		return ingest.RejectedError(v.status, v.message)
	case *handlerFailure:
		return fmt.Errorf("%w: %w", ingest.ErrRejected, v)
	default:
		return err
	}
}

// Due claims the next URL turns of this operator's shard (GET
// /network/provider-egress-due): the handler's limit ceiling and shard rules,
// then the model's atomic claim, observed into the same URL Due timing.
func (self *Operator) Due(ctx context.Context, limit int) ([]ingest.DueProvider, error) {
	shardIndex, shardCount, err := ingest.PrepareDueRequest("due", limit, self.ShardIndex, self.ShardCount)
	if err != nil {
		return nil, err
	}
	var due []ingest.DueProvider
	err = self.run(ctx, "due", func(ctx context.Context) error {
		clamped, err := controller.ProviderUrlProbeDueLimit(limit)
		if err != nil {
			return refusalOf(err)
		}
		if err := controller.ValidateProviderUrlProbeDueShard(shardIndex, shardCount); err != nil {
			return refusalOf(err)
		}
		claim := self.claimDue
		if claim == nil {
			claim = model.ClaimProviderUrlProbeDueWithObservation
		}
		result := controller.ClaimProviderUrlProbeDueObserved(ctx, clamped, shardIndex, shardCount, claim, self.dueTiming)
		if result.PriorityMaintenancePending {
			return ingest.ErrDuePriorityMaintenancePending
		}
		due = DueProvidersOf(result.Providers)
		return nil
	})
	if err != nil {
		return nil, rejected(err)
	}
	return due, nil
}

// The prober's view of a claimed due list, field for field what it decodes
// from the handler's JSON: ids in their canonical string form, no network for
// a claim without one, and no security destinations for an empty list.
func DueProvidersOf(providers []model.ProviderUrlProbeDue) []ingest.DueProvider {
	due := make([]ingest.DueProvider, 0, len(providers))
	for _, provider := range providers {
		entry := ingest.DueProvider{
			ClientId:        provider.ClientId.String(),
			RunsNeeded:      provider.RunsNeeded,
			SuccessesNeeded: provider.SuccessesNeeded,
			CycleStartedAt:  provider.CycleStartedAt,
			OutcomeCount:    provider.OutcomeCount,
			ClaimOrdinal:    provider.ClaimOrdinal,
			ClaimedAt:       provider.ClaimedAt,
			CountryCode:     provider.CountryCode,
			Region:          provider.Region,
		}
		if provider.CompletedRunCount != nil {
			count := *provider.CompletedRunCount
			entry.CompletedRunCount = &count
		}
		if 0 < len(provider.SecurityDestinations) {
			entry.SecurityDestinations = provider.SecurityDestinations
		}
		if provider.NetworkId != nil {
			entry.NetworkId = provider.NetworkId.String()
		}
		due = append(due, entry)
	}
	return due
}

// ReportAttempt records an attempt without a claim (POST
// /network/provider-egress-attempt).
func (self *Operator) ReportAttempt(ctx context.Context, providerClientId string, probeFailure string) error {
	report := ingest.NewAttemptReport(providerClientId, probeFailure)
	return rejected(self.run(ctx, "report_attempt", func(ctx context.Context) error {
		clientId, err := wireId(report.ClientId)
		if err != nil {
			return err
		}
		_, err = controller.RecordProviderEgressProbeAttempt(ctx, &controller.RecordProviderEgressProbeAttemptArgs{
			ClientId:     clientId,
			ProbeFailure: report.ProbeFailure,
		})
		return refusalOf(err)
	}))
}

// ReportUrlProbeCompletion completes one claimed URL turn (POST
// /network/provider-egress-attempt with its claim identity) and checks the
// receipt as the HTTP client does.
func (self *Operator) ReportUrlProbeCompletion(ctx context.Context, completion qualityprobe.UrlProbeCompletion) error {
	completion, err := ingest.PrepareUrlProbeCompletion(completion)
	if err != nil {
		return err
	}
	return rejected(self.run(ctx, "report_url_probe_completion", func(ctx context.Context) error {
		clientId, err := wireId(completion.ClientId)
		if err != nil {
			return err
		}
		result, err := controller.RecordProviderEgressProbeAttempt(ctx, &controller.RecordProviderEgressProbeAttemptArgs{
			ClientId:     clientId,
			ProbeFailure: completion.ProbeFailure,
			ClaimOrdinal: completion.ClaimOrdinal,
			CompletedAt:  completion.CompletedAt,
			AllowPacing:  completion.AllowPacing,
		})
		if err != nil {
			return refusalOf(err)
		}
		return ingest.CheckUrlProbeCompletionReceipt(completion, ingest.UrlProbeCompletionReceipt{
			ClaimOrdinal: result.ClaimOrdinal,
			AttemptAt:    result.AttemptAt,
			ReceivedAt:   result.ReceivedAt,
		})
	}))
}

// SubmitEgressHealth records one run (POST /network/provider-egress-health).
// A nil run sends nothing, exactly as over HTTP.
func (self *Operator) SubmitEgressHealth(ctx context.Context, providerClientId string, res *egresshealth.Result) error {
	submission := ingest.NewEgressHealthSubmission(providerClientId, res)
	if submission == nil {
		return nil
	}
	return rejected(self.run(ctx, "submit_egress_health", func(ctx context.Context) error {
		args, err := EgressHealthArgsOf(submission)
		if err != nil {
			return err
		}
		return refusalOf(controller.SubmitProviderEgressHealth(ctx, args))
	}))
}

// The controller arguments the handler's strict decoder makes of the
// submission's JSON body, field for field.
func EgressHealthArgsOf(submission *ingest.EgressHealthSubmission) (*controller.SubmitProviderEgressHealthArgs, error) {
	clientId, err := wireId(submission.ClientId)
	if err != nil {
		return nil, err
	}
	runId, err := wireId(submission.RunId)
	if err != nil {
		return nil, err
	}
	var classResults map[string]controller.ProviderEgressHealthClassResult
	if submission.ClassResults != nil {
		classResults = make(map[string]controller.ProviderEgressHealthClassResult, len(submission.ClassResults))
		for class, tally := range submission.ClassResults {
			classResults[class] = controller.ProviderEgressHealthClassResult{OK: tally.Ok, Total: tally.Total}
		}
	}
	return &controller.SubmitProviderEgressHealthArgs{
		ClientId:                 clientId,
		RunId:                    runId,
		CycleStartedAt:           submission.CycleStartedAt,
		UrlProbeEvidence:         submission.UrlProbeEvidence,
		OKCount:                  submission.OkCount,
		TotalCount:               submission.TotalCount,
		ClassResults:             classResults,
		ReputationOK:             submission.ReputationOk,
		ReputationTotal:          submission.ReputationTotal,
		FailedNames:              submission.FailedNames,
		ReputationFailedNames:    submission.ReputationFailedNames,
		TLSAuthenticationFailure: submission.TlsAuthenticationFailure,
		NotMeasuredCount:         submission.NotMeasuredCount,
		NotMeasuredNames:         submission.NotMeasuredNames,
		CanaryPassedNames:        submission.CanaryPassedNames,
		CanaryFailedNames:        submission.CanaryFailedNames,
		ShortClasses:             submission.ShortClasses,
	}, nil
}

// Submit records an observed exit address (POST
// /network/provider-egress-location).
func (self *Operator) Submit(ctx context.Context, providerClientId string, exitIp string, observedAt time.Time) error {
	submission, err := ingest.NewLocationSubmission(providerClientId, exitIp, observedAt)
	if err != nil {
		return err
	}
	return rejected(self.run(ctx, "submit_location", func(ctx context.Context) error {
		clientId, err := wireId(submission.ClientId)
		if err != nil {
			return err
		}
		_, err = controller.SubmitProviderEgressLocation(ctx, &controller.SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			ExitIp:           submission.ExitIp,
			ObservedAt:       submission.ObservedAt,
			CountryCode:      submission.CountryCode,
			Country:          submission.Country,
			Region:           submission.Region,
			City:             submission.City,
			ASN:              submission.Asn,
			Org:              submission.Org,
			Hosting:          submission.Hosting,
			Proxy:            submission.Proxy,
			Mobile:           submission.Mobile,
			CountryConfident: submission.CountryConfident,
			CityConfident:    submission.CityConfident,
		})
		return refusalOf(err)
	}))
}

// BlackholeDue serves this operator's slice of the blackhole sweep (GET
// /network/provider-blackhole-due). A done-error inside the bounded due query
// is the route's 503, which the sweep retries.
func (self *Operator) BlackholeDue(ctx context.Context, limit int) ([]ingest.DueProvider, error) {
	shardIndex, shardCount, err := ingest.PrepareDueRequest("blackhole due", limit, self.ShardIndex, self.ShardCount)
	if err != nil {
		return nil, err
	}
	var due []ingest.DueProvider
	err = self.run(ctx, "blackhole_due", func(ctx context.Context) error {
		result, err := controller.ProviderBlackholeCheckDue(ctx, limit, shardIndex, shardCount)
		if err != nil {
			return refusalOf(err)
		}
		due = make([]ingest.DueProvider, 0, len(result.Providers))
		for _, provider := range result.Providers {
			due = append(due, ingest.DueProvider{
				ClientId:    provider.ClientId.String(),
				CountryCode: provider.CountryCode,
				Region:      provider.Region,
			})
		}
		return nil
	})
	if failure, ok := err.(*handlerFailure); ok && failure.status == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("%w: %w: %w", ingest.ErrRejected, ingest.ErrBlackholeDueUnavailable, failure)
	}
	if err != nil {
		return nil, rejected(err)
	}
	return due, nil
}

// SubmitBlackholeChecks records one pass (POST
// /network/provider-blackhole-checks). The whole batch is validated before any
// of it is written.
func (self *Operator) SubmitBlackholeChecks(ctx context.Context, checks []ingest.BlackholeCheck) error {
	submission, err := ingest.NewBlackholeChecksSubmission(checks)
	if err != nil || submission == nil {
		return err
	}
	return rejected(self.run(ctx, "submit_blackhole_checks", func(ctx context.Context) error {
		args, err := BlackholeChecksArgsOf(submission)
		if err != nil {
			return err
		}
		return refusalOf(controller.SubmitProviderBlackholeChecks(ctx, args))
	}))
}

// The controller arguments the handler's strict decoder makes of the batch.
func BlackholeChecksArgsOf(submission *ingest.BlackholeChecksSubmission) (*controller.SubmitProviderBlackholeChecksArgs, error) {
	args := &controller.SubmitProviderBlackholeChecksArgs{
		Checks: make([]controller.SubmitProviderBlackholeCheckArgs, 0, len(submission.Checks)),
	}
	for _, check := range submission.Checks {
		clientId, err := wireId(check.ClientId)
		if err != nil {
			return nil, err
		}
		args.Checks = append(args.Checks, controller.SubmitProviderBlackholeCheckArgs{
			ClientId:    clientId,
			OK:          check.Ok,
			Failure:     check.Failure,
			NotMeasured: check.NotMeasured,
			CheckedAt:   check.CheckedAt,
		})
	}
	return args, nil
}

// GeolocationPins reads the pins the server observed (GET
// /network/geolocation-source-pins). Every failure is ErrPinsUnavailable: there
// is no behaviour to fall back to.
func (self *Operator) GeolocationPins(ctx context.Context) (map[string]ingest.GeolocationPin, error) {
	pins := map[string]ingest.GeolocationPin{}
	err := self.run(ctx, "geolocation_pins", func(ctx context.Context) error {
		for host, pin := range model.GetGeolocationSourcePins(ctx) {
			pins[host] = ingest.GeolocationPin{Leaf: pin.LeafSpki, Intermediate: pin.IntermediateSpki}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ingest.ErrPinsUnavailable, err)
	}
	return pins, nil
}

// DestinationPool builds the destination pool the server serves (GET
// /network/provider-egress-destinations) and holds it to the prober's own
// ValidatePool, as egresshealth.FetchPool does. Every failure is
// ErrPoolUnavailable; no other catalog is substituted.
func (self *Operator) DestinationPool(ctx context.Context) (*egresshealth.Pool, error) {
	var pool *egresshealth.Pool
	err := self.run(ctx, "destination_pool", func(ctx context.Context) error {
		built, err := controller.GetProviderEgressDestinationPool(ctx)
		if err != nil {
			glog.Errorf("[pegd]could not build the egress destination pool: %s\n", err)
			return &handlerRefusal{status: http.StatusInternalServerError, message: "The destination pool cannot be served: " + err.Error()}
		}
		pool = built
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", egresshealth.ErrPoolUnavailable, err)
	}
	if err := egresshealth.ValidatePool(pool); err != nil {
		return nil, fmt.Errorf("%w: %w", egresshealth.ErrPoolUnavailable, err)
	}
	return pool, nil
}

// ReserveBandwidth takes budget for one active measurement (POST
// /network/provider-bandwidth-reserve). An exhausted hourly budget is the
// clean skip bandwidth.ErrNoBudget, as the route's 429 is.
func (self *Operator) ReserveBandwidth(ctx context.Context, providerClientId string, byteCount int64) error {
	err := self.run(ctx, "reserve_bandwidth", func(ctx context.Context) error {
		clientId, err := wireId(providerClientId)
		if err != nil {
			return err
		}
		_, err = controller.ReserveProviderBandwidth(ctx, &controller.ReserveProviderBandwidthArgs{ClientId: clientId, ByteCount: byteCount})
		return refusalOf(err)
	})
	if refusal, ok := err.(*handlerRefusal); ok && refusal.status == http.StatusTooManyRequests {
		return fmt.Errorf("%w (provider %s)", bandwidth.ErrNoBudget, providerClientId)
	}
	return rejected(err)
}

// SubmitBandwidth records one measured figure under its source (POST
// /network/provider-bandwidth-result).
func (self *Operator) SubmitBandwidth(
	ctx context.Context,
	providerClientId string,
	source string,
	bytesPerSecond float64,
	sampleByteCount int64,
) error {
	return rejected(self.run(ctx, "submit_bandwidth", func(ctx context.Context) error {
		clientId, err := wireId(providerClientId)
		if err != nil {
			return err
		}
		return refusalOf(controller.SubmitProviderBandwidth(ctx, &controller.SubmitProviderBandwidthArgs{
			ClientId:        clientId,
			Source:          source,
			BytesPerSecond:  bytesPerSecond,
			SampleByteCount: sampleByteCount,
		}))
	}))
}
