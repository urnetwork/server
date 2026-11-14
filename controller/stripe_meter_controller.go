package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/stripe/stripe-go/v82"
	stripemeterevent "github.com/stripe/stripe-go/v82/billing/meterevent"
	stripemetereventsummary "github.com/stripe/stripe-go/v82/billing/metereventsummary"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Team's meter reports to Stripe (QUOTA-M6-04; DESIGN.md 20.9's Meters; D-QUOTA-8, FI-13(b), FS-43,
// R-QUOTA-1). model.RollupTeamServiceUsage writes one stripe_meter_report row per (network, meter,
// closed UTC day); ReportStripeMeters sends them, and AuditStripeMeters holds what was sent against
// Stripe's summaries.
//
//	send      one meter event a row: the meter's event_name, the Team's customer, the row's value,
//	          the row's identifier and the day's timestamp (model.TeamStripeMeterSendWindow). The
//	          three meters ingest pre-aggregated daily, so Stripe keeps a day's most recently
//	          received event per customer: a re-send replaces and never adds
//	outcomes  200, or a refusal naming a duplicate identifier: sent. Any other 4xx: failed, counted
//	          for the billing alert, never re-sent. A timeout, a transport error, 429 or 5xx:
//	          uncertain, re-sent with the same identifier and value after a backoff until the row's
//	          deadline. An answer this code cannot place is failed, which bills less, never twice
//	order     one row a (network, meter) at a time, and a day's versions oldest first, each only once
//	          the one before has a known outcome (model.ClaimStripeMeterReports), so the day's newest
//	          value is the event Stripe received last
//	missed    a row still unsent at its invoice's finalization is missed: unbilled, counted, never
//	          added to a later invoice. A row claimed within 6 h of that is counted for the alert
//	audit     daily, each live Team's sent rows of the last AuditDayCount days against Stripe's daily
//	          meter event summaries. A difference is counted and logged, and nothing is re-sent on
//	          it: summaries lag, and past Stripe's 24 h dedup only the daily replacement keeps a
//	          re-send from adding
//
// A meter cannot change in Stripe after it is made, its display name aside, so the three meters are
// made once before M6 (External) and this code only names them, from config/<env>/stripe.yml's
// team_meters. It refuses nothing for a meter it has no config for: the event goes out under the
// meter's own name, and the audit, which needs Stripe's meter id, passes that meter over.
//
// No metric here carries a network id (I-13). Nothing here runs unless the binary links MG6.

// One Team meter as Stripe knows it.
type StripeTeamMeter struct {
	// the meter's event_name; empty reads as the meter's own name (QD-430)
	EventName string `yaml:"event_name"`
	// the meter's Stripe id (mtr_...), which summaries are listed by; empty leaves the meter unaudited
	MeterId string `yaml:"meter_id"`
}

// config/<env>/stripe.yml's team_meters, by meter:
//
//	team_meters:
//	  team_seats: {event_name: team_seats, meter_id: mtr_...}
//	  team_transfer_overage: {event_name: team_transfer_overage, meter_id: mtr_...}
//	  team_storage_overage: {event_name: team_storage_overage, meter_id: mtr_...}
//
// Optional, as team_prices is: an env with none sends under the meters' own names.
var stripeTeamMeters = sync.OnceValue(func() map[string]StripeTeamMeter {
	var c struct {
		TeamMeters map[string]StripeTeamMeter `yaml:"team_meters"`
	}
	if resource, err := server.Config.SimpleResource("stripe.yml"); err == nil {
		resource.UnmarshalYaml(&c)
	}
	if c.TeamMeters == nil {
		return map[string]StripeTeamMeter{}
	}
	return c.TeamMeters
})

// Replaceable only by tests.
var stripeTeamMetersFunc = func() map[string]StripeTeamMeter { return stripeTeamMeters() }

func stripeTeamMeterEventName(meter string) string {
	if eventName := stripeTeamMetersFunc()[meter].EventName; eventName != "" {
		return eventName
	}
	return meter
}

type StripeMeterSettings struct {
	// Pairs of (network, meter) one claim takes, and the sends a pass makes at most before it
	// leaves the rest to the next.
	ClaimPageCount int
	MaxSendCount   int
	// Bounds one call to Stripe.
	StripeTimeout time.Duration
	// How many closed days back the audit reads, and the Teams it takes a page.
	AuditDayCount  int
	AuditPageCount int
	// The error codes of a 4xx that names a duplicate identifier.
	DuplicateIdentifierCodes []string
}

func DefaultStripeMeterSettings() *StripeMeterSettings {
	return &StripeMeterSettings{
		ClaimPageCount: 200,
		MaxSendCount:   2000,
		StripeTimeout:  15 * time.Second,
		AuditDayCount:  7,
		AuditPageCount: 100,
		DuplicateIdentifierCodes: []string{
			string(stripe.ErrorCodeResourceAlreadyExists),
			"duplicate_meter_event",
		},
	}
}

// Replaceable only by tests.
var stripeMeterSettingsFunc = DefaultStripeMeterSettings

// One meter event as it is sent.
type stripeMeterEvent struct {
	EventName  string
	CustomerId string
	Identifier string
	Value      int64
	Timestamp  time.Time
}

// What Stripe holds for one customer's meter over one UTC day, by its summary.
type stripeMeterSummary struct {
	Day   time.Time
	Value int64
}

// Stripe's meter calls, behind which FS-43's fake stands in.
type stripeMeterClient interface {
	// Sends one event and says what its answer means for the row.
	SendMeterEvent(ctx context.Context, event *stripeMeterEvent) model.StripeMeterOutcome
	// The customer's daily summaries of the meter over the UTC days [from, to).
	ListMeterSummaries(ctx context.Context, meterId string, customerId string, from time.Time, to time.Time) ([]*stripeMeterSummary, error)
}

// Stripe itself, through stripe-go.
type stripeMeterApiClient struct {
	settings *StripeMeterSettings
}

func newStripeMeterApiClient(settings *StripeMeterSettings) stripeMeterClient {
	return &stripeMeterApiClient{settings: settings}
}

// Replaceable only by tests.
var stripeMeterClientFunc = newStripeMeterApiClient

// What a failed send's error means for its row (the file header's outcomes row).
func stripeMeterErrorOutcome(err error, identifier string, duplicateCodes []string) model.StripeMeterOutcome {
	var stripeErr *stripe.Error
	if !errors.As(err, &stripeErr) || stripeErr.HTTPStatusCode == 0 {
		// no answer: a timeout or a transport error
		return model.StripeMeterOutcomeUncertain
	}
	status := stripeErr.HTTPStatusCode
	switch {
	case status == 429 || 500 <= status:
		return model.StripeMeterOutcomeUncertain
	case 400 <= status:
		for _, code := range duplicateCodes {
			if string(stripeErr.Code) == code {
				return model.StripeMeterOutcomeSent
			}
		}
		// a refusal that names this identifier
		if stripeErr.Param == "identifier" && strings.Contains(stripeErr.Msg, identifier) {
			return model.StripeMeterOutcomeSent
		}
		return model.StripeMeterOutcomeFailed
	default:
		// not an answer the meter API gives
		return model.StripeMeterOutcomeUncertain
	}
}

func (self *stripeMeterApiClient) SendMeterEvent(ctx context.Context, event *stripeMeterEvent) model.StripeMeterOutcome {
	stripe.Key = stripeApiToken()
	stripeCtx, cancel := context.WithTimeout(ctx, self.settings.StripeTimeout)
	defer cancel()
	params := &stripe.BillingMeterEventParams{
		EventName:  stripe.String(event.EventName),
		Identifier: stripe.String(event.Identifier),
		Payload: map[string]string{
			"stripe_customer_id": event.CustomerId,
			"value":              strconv.FormatInt(event.Value, 10),
		},
		Timestamp: stripe.Int64(event.Timestamp.Unix()),
	}
	params.Context = stripeCtx
	if _, err := stripemeterevent.New(params); err != nil {
		return stripeMeterErrorOutcome(err, event.Identifier, self.settings.DuplicateIdentifierCodes)
	}
	return model.StripeMeterOutcomeSent
}

func (self *stripeMeterApiClient) ListMeterSummaries(ctx context.Context, meterId string, customerId string, from time.Time, to time.Time) ([]*stripeMeterSummary, error) {
	stripe.Key = stripeApiToken()
	stripeCtx, cancel := context.WithTimeout(ctx, self.settings.StripeTimeout)
	defer cancel()
	params := &stripe.BillingMeterEventSummaryListParams{
		ID:                  stripe.String(meterId),
		Customer:            stripe.String(customerId),
		StartTime:           stripe.Int64(from.Unix()),
		EndTime:             stripe.Int64(to.Unix()),
		ValueGroupingWindow: stripe.String("day"),
	}
	params.Context = stripeCtx
	summaries := []*stripeMeterSummary{}
	iter := stripemetereventsummary.List(params)
	for iter.Next() {
		summary := iter.BillingMeterEventSummary()
		summaries = append(summaries, &stripeMeterSummary{
			Day:   time.Unix(summary.StartTime, 0).UTC(),
			Value: int64(summary.AggregatedValue + 0.5),
		})
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return summaries, nil
}

var stripeMeterSendCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "urnetwork_stripe_meter_sends_total",
		Help: "Team meter events sent to Stripe, by meter and outcome (§20.9): sent, uncertain, failed, or lost when the claim ended first",
	},
	[]string{"meter", "outcome"},
)

var stripeMeterUnsentCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "urnetwork_stripe_meter_unsent_total",
		Help: "Team meter rows that left the send path unbilled or unknown, by meter and reason (§20.9): missed at finalization, parked uncertain at it, or failed for want of a subscription",
	},
	[]string{"meter", "reason"},
)

var stripeMeterNearDeadlineCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_stripe_meter_near_finalization_total",
	Help: "Team meter rows claimed still unsent within 6 h of their invoice's finalization (§20.9's missed row), which alerts billing",
})

var stripeMeterAuditCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "urnetwork_stripe_meter_audit_days_total",
		Help: "Team meter days the audit held against Stripe's summaries, by meter and result (§20.9): equal, differs or absent",
	},
	[]string{"meter", "result"},
)

func init() {
	prometheus.MustRegister(
		stripeMeterSendCounter,
		stripeMeterUnsentCounter,
		stripeMeterNearDeadlineCounter,
		stripeMeterAuditCounter,
	)
}

type ReportStripeMetersResult struct {
	// sends by outcome, and those whose claim ended before the outcome was recorded
	SentCount      int
	UncertainCount int
	FailedCount    int
	LostCount      int
	// rows that left the send path at a claim
	MissedCount       int
	ParkedCount       int
	UnsubscribedCount int
	NearDeadlineCount int
	// whether the pass stopped at MaxSendCount with rows still due
	More bool
}

// One pass of the sender at the process's clock: claims due rows a page at a time, sends each and
// records its outcome, until nothing is due or MaxSendCount is reached. A pass killed anywhere
// leaves each claimed row sending, to be sent again with the same identifier once its claim ends.
func ReportStripeMeters(ctx context.Context) (*ReportStripeMetersResult, error) {
	settings := stripeMeterSettingsFunc()
	return reportStripeMeters(ctx, stripeMeterClientFunc(settings), server.NowUtc, settings)
}

func reportStripeMeters(ctx context.Context, client stripeMeterClient, now func() time.Time, settings *StripeMeterSettings) (*ReportStripeMetersResult, error) {
	result := &ReportStripeMetersResult{}
	if !server.SidecarMg6Linked() {
		return result, nil
	}
	sendCount := 0
	sweepSendCount := 0
	var after *model.StripeMeterClaimCursor
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		claim, err := model.ClaimStripeMeterReports(ctx, now(), after, settings.ClaimPageCount)
		if err != nil {
			return result, err
		}
		for meter, count := range claim.MissedCounts {
			result.MissedCount += count
			stripeMeterUnsentCounter.WithLabelValues(meter, "missed").Add(float64(count))
		}
		for meter, count := range claim.ParkedCounts {
			result.ParkedCount += count
			stripeMeterUnsentCounter.WithLabelValues(meter, "parked").Add(float64(count))
		}
		for meter, count := range claim.FailedCounts {
			result.UnsubscribedCount += count
			stripeMeterUnsentCounter.WithLabelValues(meter, "unsubscribed").Add(float64(count))
		}
		if 0 < claim.NearDeadlineCount {
			result.NearDeadlineCount += claim.NearDeadlineCount
			stripeMeterNearDeadlineCounter.Add(float64(claim.NearDeadlineCount))
			glog.Errorf("[stripe][meter]%d rows are unsent within their invoice's last hours\n", claim.NearDeadlineCount)
		}
		for _, send := range claim.Sends {
			outcome := client.SendMeterEvent(ctx, &stripeMeterEvent{
				EventName:  stripeTeamMeterEventName(send.Report.Meter),
				CustomerId: send.CustomerId,
				Identifier: send.Report.Identifier,
				Value:      send.Report.Value,
				Timestamp:  send.Timestamp,
			})
			recorded, err := model.RecordStripeMeterOutcome(ctx, send, outcome, now())
			if err != nil {
				return result, err
			}
			label := string(outcome)
			switch {
			case !recorded:
				result.LostCount += 1
				label = "lost"
			case outcome == model.StripeMeterOutcomeSent:
				result.SentCount += 1
			case outcome == model.StripeMeterOutcomeFailed:
				result.FailedCount += 1
				glog.Errorf("[stripe][meter]%s day %s of network %s was refused\n", send.Report.Meter, send.Report.UsageDay.Format(time.DateOnly), send.Report.NetworkId)
			default:
				result.UncertainCount += 1
			}
			stripeMeterSendCounter.WithLabelValues(send.Report.Meter, label).Inc()
		}
		sendCount += len(claim.Sends)
		sweepSendCount += len(claim.Sends)
		if settings.MaxSendCount <= sendCount {
			result.More = true
			return result, nil
		}
		if claim.Next != nil {
			// more pairs follow this page
			after = claim.Next
			continue
		}
		if sweepSendCount == 0 {
			// a whole sweep of the due pairs sent nothing: what is left waits on a claim in flight
			// or an earlier version's backoff
			return result, nil
		}
		// from the start again: a pair's next day, or a version its earlier one held back
		after = nil
		sweepSendCount = 0
	}
}

type AuditStripeMetersResult struct {
	// the Teams looked at
	LookedCount int
	// days held against a summary, and those that differ from it or that it lacks
	DayCount    int
	DifferCount int
	AbsentCount int
	// (Team, meter) pairs passed over: no meter id, or Stripe did not answer
	SkippedCount int
	// the page's last network when the page was full
	LastNetworkId *server.Id
}

// One page of the daily audit at the process's clock: for each live Team after after, the newest
// sent value of each of the last AuditDayCount closed days against Stripe's summary of that day. It
// changes no row and sends nothing.
func AuditStripeMeters(ctx context.Context, after *server.Id) (*AuditStripeMetersResult, error) {
	settings := stripeMeterSettingsFunc()
	return auditStripeMeters(ctx, stripeMeterClientFunc(settings), server.NowUtc(), after, settings)
}

func auditStripeMeters(ctx context.Context, client stripeMeterClient, now time.Time, after *server.Id, settings *StripeMeterSettings) (*AuditStripeMetersResult, error) {
	result := &AuditStripeMetersResult{}
	if !server.SidecarMg6Linked() {
		return result, nil
	}
	now = now.UTC()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -settings.AuditDayCount)
	subscriptions := model.ListLiveTeamSubscriptionsInConn(nil, ctx, after, settings.AuditPageCount)
	meters := stripeTeamMetersFunc()
	for _, subscription := range subscriptions {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.LookedCount += 1
		sentValues := model.GetSentStripeMeterValues(ctx, subscription.NetworkId, from)
		for _, meter := range model.TeamStripeMeters {
			days := sentValues[meter]
			if len(days) == 0 {
				continue
			}
			meterId := meters[meter].MeterId
			if meterId == "" {
				result.SkippedCount += 1
				continue
			}
			summaries, err := client.ListMeterSummaries(ctx, meterId, subscription.CustomerId, from, to)
			if err != nil {
				result.SkippedCount += 1
				continue
			}
			summaryValues := map[time.Time]int64{}
			for _, summary := range summaries {
				summaryValues[summary.Day.UTC()] = summary.Value
			}
			for day, value := range days {
				if !day.Before(to) {
					continue
				}
				result.DayCount += 1
				summaryValue, ok := summaryValues[day]
				switch {
				case ok && summaryValue == value:
					stripeMeterAuditCounter.WithLabelValues(meter, "equal").Inc()
				case !ok && value == 0:
					// Stripe gives no summary for a day of nothing
					stripeMeterAuditCounter.WithLabelValues(meter, "equal").Inc()
				case !ok:
					result.AbsentCount += 1
					stripeMeterAuditCounter.WithLabelValues(meter, "absent").Inc()
					glog.Errorf("[stripe][meter]audit: %s day %s of network %s is sent and Stripe's summary lacks it\n", meter, day.Format(time.DateOnly), subscription.NetworkId)
				default:
					result.DifferCount += 1
					stripeMeterAuditCounter.WithLabelValues(meter, "differs").Inc()
					glog.Errorf("[stripe][meter]audit: %s day %s of network %s differs from Stripe's summary\n", meter, day.Format(time.DateOnly), subscription.NetworkId)
				}
			}
		}
	}
	if len(subscriptions) == settings.AuditPageCount {
		last := subscriptions[len(subscriptions)-1].NetworkId
		result.LastNetworkId = &last
	}
	return result, nil
}
