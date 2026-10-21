package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signal account-messages implements SIGNALS.md §19.3. Every account email and
// SMS (sign-in and verification codes, password resets, payout and earnings
// notices) goes through one SES or Pinpoint SMS call, counted by
// urnetwork_account_message_sends_total{channel,result} in each sending
// service. The API logs a failed send only at info level and at most once a
// report interval, so a channel whose every call is refused (an IAM policy
// without the action) raised nothing for days. This signal pages when a channel
// delivered nothing in the range while it failed at least a floor of sends, and
// warns when a channel failed a large share. These are historical outcomes;
// raw source clocks separately qualify freshness of the observed range cohort.
// Job, channel and result are bounded labels; private process labels stay in Mimir.
func NewAccountMessagesSignal() Signal {
	return &signalAdapter{
		number: "19.3", key: "account-messages", name: "Account message delivery",
		probe: accountMessagesProbe{},
	}
}

type accountMessagesProbe struct{}

// The gateway aggregate stays available for lifecycle and admission identity;
// its private host, command and transport text never enters the public error.
type accountMessagesQueryError struct{ cause error }

func (*accountMessagesQueryError) Error() string {
	return "account messages: query Mimir failed"
}

func (self *accountMessagesQueryError) Unwrap() error { return self.cause }

func (accountMessagesProbe) id() string             { return "mimir/account-messages" }
func (accountMessagesProbe) tier() string           { return tierPage }
func (accountMessagesProbe) cadence() time.Duration { return 5 * time.Minute }

const (
	// SMS is rare (a few sign-in codes a day), so a shorter range would hold
	// fewer failures than the floor while every call fails.
	accountMessagesRange     = "24h"
	accountMessagesFreshness = 5 * time.Minute
	// A channel that delivered nothing in the range and failed at least this
	// many sends is down: one or two failures alone can be a transient refusal.
	accountMessagesDownFloor = 3.0
	// A channel that still delivers but fails at least this share of at least
	// accountMessagesDegradedAttempts sends is degraded.
	accountMessagesDegradedShare    = 0.25
	accountMessagesDegradedAttempts = 20.0
)

// The channels and results accountMessageSendResult counts.
var accountMessagesChannels = []string{"email", "sms"}
var accountMessagesResults = map[string]bool{"sent": true, "failed": true, "timeout": true}
var accountMessagesJobs = map[string]bool{"api": true, "taskworker": true, "alt": true}

// The companions retain the least sample count and oldest raw clock across
// every series in the range cohort. Match before aggregation: a fresh process
// cannot cover a stopped process with the same job/channel/result. Zero marks
// a range contributor absent from the instant selector, never a source clock.
func accountMessagesQuery(environment string) string {
	selector := fmt.Sprintf(`urnetwork_account_message_sends_total{env=%s}`, strconv.Quote(environment))
	rangeSelector := selector + "[" + accountMessagesRange + "]"
	parts := []string{}
	for _, part := range []struct{ name, expression string }{
		{name: "increase", expression: "sum by (job,channel,result) (increase(" + rangeSelector + "))"},
		{name: "min_samples", expression: "min by (job,channel,result) (count_over_time(" + rangeSelector + "))"},
		{name: "source_timestamp", expression: "min by (job,channel,result) (timestamp(" + selector + ") or (0 * count_over_time(" + rangeSelector + ")))"},
	} {
		parts = append(parts, fmt.Sprintf(`label_replace(%s,"monitor_account_part",%s,"job",".*")`, part.expression, strconv.Quote(part.name)))
	}
	return strings.Join(parts, " or ")
}

// One channel's sends in the range, in total and by sending service.
type accountMessagesChannel struct {
	name          string
	sent          float64
	failed        float64
	timeout       float64
	jobFailed     map[string]float64
	jobSent       map[string]float64
	series        int
	rangeComplete bool
	sourceFresh   bool
}

func (self *accountMessagesChannel) failures() float64 {
	return self.failed + self.timeout
}

func (self *accountMessagesChannel) attempts() float64 {
	return self.sent + self.failed + self.timeout
}

// Range values and source freshness have separate authority. The timestamp
// in each API value is the evaluation clock; only source_timestamp's numeric
// value is a raw source clock. Lazy producer labels leave absent channels
// unknown, and fewer than two raw points cannot establish a range zero.
func parseAccountMessagesFrame(response mimirInstantResponse, now time.Time) (map[string]*accountMessagesChannel, string) {
	if len(response.Data.Result) == 0 {
		return nil, "no_series"
	}
	channels := map[string]*accountMessagesChannel{}
	for _, name := range accountMessagesChannels {
		channels[name] = &accountMessagesChannel{name: name, jobFailed: map[string]float64{}, jobSent: map[string]float64{}}
	}
	type resultKey struct{ job, channel, result string }
	type resultParts struct {
		increase, minSamples, sourceTime             float64
		increaseSeen, minSamplesSeen, sourceTimeSeen bool
		rangeInvalid, sourceInvalid                  bool
	}
	results := map[resultKey]*resultParts{}
	for _, series := range response.Data.Result {
		job, channelName, result := series.Metric["job"], series.Metric["channel"], series.Metric["result"]
		if job == "" || channelName == "" || result == "" {
			return nil, "unlabeled_series"
		}
		if channels[channelName] == nil || !accountMessagesResults[result] {
			return nil, "unknown_channel_or_result"
		}
		if !accountMessagesJobs[job] {
			return nil, "unknown_job"
		}
		for label := range series.Metric {
			if label != "job" && label != "channel" && label != "result" && label != "monitor_account_part" {
				return nil, "unexpected_labels"
			}
		}
		key := resultKey{job: job, channel: channelName, result: result}
		parts := results[key]
		if parts == nil {
			parts = &resultParts{}
			results[key] = parts
		}
		evaluatedAt, value, err := mimirInstantValue(series.Value)
		invalid := err != nil || now.Sub(evaluatedAt) > accountMessagesFreshness || now.Sub(evaluatedAt) < -30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0
		switch series.Metric["monitor_account_part"] {
		case "increase":
			parts.rangeInvalid = parts.rangeInvalid || invalid || parts.increaseSeen
			parts.increase, parts.increaseSeen = value, true
		case "min_samples":
			parts.rangeInvalid = parts.rangeInvalid || invalid || parts.minSamplesSeen || value != math.Trunc(value)
			parts.minSamples, parts.minSamplesSeen = value, true
		case "source_timestamp":
			parts.sourceInvalid = parts.sourceInvalid || invalid || parts.sourceTimeSeen
			parts.sourceTime, parts.sourceTimeSeen = value, true
		default:
			return nil, "unknown_metric_part"
		}
	}
	for key, parts := range results {
		channel := channels[key.channel]
		if channel.series == 0 {
			channel.rangeComplete, channel.sourceFresh = true, true
		}
		channel.series++
		if parts.rangeInvalid || !parts.increaseSeen || !parts.minSamplesSeen || parts.minSamples < 2 {
			channel.rangeComplete = false
		}
		sourceAge := now.Sub(unixFloatTime(parts.sourceTime))
		if parts.sourceInvalid || !parts.sourceTimeSeen || parts.sourceTime == 0 ||
			sourceAge > accountMessagesFreshness || sourceAge < -30*time.Second {
			channel.sourceFresh = false
		}
		switch key.result {
		case "sent":
			channel.sent += parts.increase
			channel.jobSent[key.job] += parts.increase
		case "failed":
			channel.failed += parts.increase
			channel.jobFailed[key.job] += parts.increase
		case "timeout":
			channel.timeout += parts.increase
			channel.jobFailed[key.job] += parts.increase
		}
	}
	for _, name := range accountMessagesChannels {
		if !channels[name].rangeComplete || !channels[name].sourceFresh {
			return channels, "incomplete_channel_evidence"
		}
	}
	return channels, ""
}

// The channel's totals and its failing services, most failures first.
func accountMessagesObserved(channel *accountMessagesChannel) string {
	jobs := []string{}
	for job := range channel.jobFailed {
		if 0 < channel.jobFailed[job] {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if channel.jobFailed[jobs[i]] != channel.jobFailed[jobs[j]] {
			return channel.jobFailed[jobs[i]] > channel.jobFailed[jobs[j]]
		}
		return jobs[i] < jobs[j]
	})
	parts := []string{}
	for _, job := range jobs {
		parts = append(parts, fmt.Sprintf("%s=%.0f/%.0f", job, channel.jobFailed[job], channel.jobFailed[job]+channel.jobSent[job]))
	}
	freshness := "unknown"
	if channel.sourceFresh {
		freshness = "fresh"
	}
	return fmt.Sprintf(
		"channel=%s range=%s sent=%.0f failed=%.0f timeout=%.0f failure_share=%.3f failed_by_job=%s source_freshness=%s cohort=observed_range",
		channel.name, accountMessagesRange, channel.sent, channel.failed, channel.timeout,
		channel.failures()/channel.attempts(), strings.Join(parts, ","), freshness,
	)
}

func (accountMessagesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("account messages: no services host in inventory for the loopback Mimir query")
	}
	now := env.now().UTC()
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(accountMessagesQuery(env.cfg.env)) +
		"&time=" + strconv.FormatInt(now.Unix(), 10)
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 1048576 '"+queryURL+"'")
	if err != nil {
		return nil, &accountMessagesQueryError{cause: err}
	}
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("account messages: decode Mimir response failed")
	}
	if response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 {
		return nil, fmt.Errorf("account messages: Mimir response was not a successful vector")
	}

	target := env.cfg.env + "/account-messages"
	channels, visibilityReason := parseAccountMessagesFrame(response.mimirInstantResponse, now)
	findings := []finding{}
	if visibilityReason != "" {
		coverage := []string{}
		for _, name := range accountMessagesChannels {
			if channel := channels[name]; channel != nil {
				rangeStatus, freshness := "incomplete", "unknown"
				if channel.series == 0 {
					rangeStatus = "no_series"
				} else if channel.rangeComplete {
					rangeStatus = "complete"
				}
				if channel.sourceFresh {
					freshness = "fresh"
				}
				coverage = append(coverage, fmt.Sprintf("channel=%s range_status=%s source_freshness=%s", name, rangeStatus, freshness))
			}
		}
		findings = append(findings, finding{
			probeId: "mimir/account-messages", tier: tierWarn, class: "account-message-sends-unobservable",
			target: target, sustain: 2,
			symptom:   "Account message range coverage or raw source freshness is unknown for at least one channel.",
			mechanism: "An increase over 24h carries the query evaluation timestamp even after its sender stops. Its historical outcomes cannot prove current source visibility; missing lazy channel labels cannot establish zero attempts.",
			baseline:  "Both channels have readable range outcomes, at least two raw samples per observed range contributor, and source clocks at most five minutes old and thirty seconds ahead.",
			observed:  "visibility_reason=" + visibilityReason + " " + strings.Join(coverage, "; ") + " cohort=observed_range metrics_gateway=" + gateway.name,
			evidence:  "Fixed coverage states are exported separately from historical outcomes. Raw process labels remain in Mimir.",
			context:   "A retired sender can leave historical outcomes while its raw clock is unavailable. A never-used channel can have no labels. Neither proves a current delivery fault or healthy zero; complete active-publisher inventory remains a separate §8.12 observation.",
			action:    "Check current API, Taskworker and Alt publisher coverage and artifact support (§8.12). Separate stopped or replaced contributors from a missing pusher, then verify the channel's present delivery independently.",
			verify:    "Readable range evidence and raw source coverage return for two cadences; confirm present delivery separately before closing the provider incident.",
			playbook:  "SIGNALS.md §19.3",
		})
	} else {
		findings = append(findings, healthyFinding("mimir/account-messages", tierWarn, "account-message-sends-unobservable", target))
	}
	if channels == nil {
		return findings, nil
	}

	for _, name := range accountMessagesChannels {
		channel := channels[name]
		if !channel.rangeComplete {
			continue
		}
		frame := "channel=" + name
		down := channel.sent == 0 && accountMessagesDownFloor <= channel.failures()
		degraded := !down && accountMessagesDegradedAttempts <= channel.attempts() &&
			accountMessagesDegradedShare <= channel.failures()/channel.attempts()
		if down {
			findings = append(findings, finding{
				probeId: "mimir/account-messages", tier: tierPage, class: "account-message-channel-down",
				target: target, frame: frame, sustain: 2,
				symptom: fmt.Sprintf(
					"account %s delivered nothing in the last %s while %.0f send(s) failed",
					name, accountMessagesRange, channel.failures(),
				),
				mechanism: "The observed 24h counter range records failures with no successful provider call for this channel. Possible causes include an IAM policy without ses:SendEmail or sms-voice:SendTextMessage, a suspended identity or origination number, or an exhausted quota. Historical outcomes alone do not establish a current refusal.",
				baseline:  fmt.Sprintf("Every channel that sends delivers, or fails fewer than %.0f sends in %s.", accountMessagesDownFloor, accountMessagesRange),
				observed:  accountMessagesObserved(channel) + " metrics_gateway=" + gateway.name,
				evidence:  "Mimir sums per-service counter increases over the historical range. Raw source freshness is reported separately for every observed range contributor; no address, number or message leaves Mimir.",
				context:   "The API reports a failed send at most once a report interval as \"[aws]account <channel> send failed (<code>)\", at info level, so the log rate understates the failures and no log class pages on it. AccessDeniedException alone does not identify the cause: an IAM policy denial and, for SMS, an account spend threshold refusal can return it. ThrottlingException reports throttling. The send counters contain no original error details. Taskworker sends (earnings and payout notices) burst when an epoch finalizes.",
				action:    "Establish current publisher coverage and read a contemporaneous \"[aws]account <channel> send failed (<code>)\" line before attributing a current provider fault. Use the provider's original error details to distinguish IAM policy, SMS account spend, identity or throttling restrictions before changing provider settings; a missing discriminator leaves the cause unknown. Follow the existing provider-incident owner for a confirmed refusal. Do not retry sends by hand or restart services for this alone: the credential and policy are read per call.",
				verify:    "Restore raw source coverage, observe a successful send after the corrective action, and confirm a real sign-in code arrives. An earlier success anywhere in the 24h range alone does not prove current recovery.",
				playbook:  "SIGNALS.md §19.3",
			})
		} else if channel.sourceFresh {
			findings = append(findings, finding{
				probeId: "mimir/account-messages", tier: tierPage, class: "account-message-channel-down",
				target: target, frame: frame, healthy: true,
			})
		}
		if degraded {
			findings = append(findings, finding{
				probeId: "mimir/account-messages", tier: tierWarn, class: "account-message-channel-degraded",
				target: target, frame: frame, sustain: 2,
				symptom: fmt.Sprintf(
					"account %s failed %.0f of %.0f sends in the last %s",
					name, channel.failures(), channel.attempts(), accountMessagesRange,
				),
				mechanism: "Part of this channel's provider calls failed or timed out in the observed 24h range. The range does not establish the channel's present delivery state.",
				baseline:  fmt.Sprintf("A channel fails under %.0f%% of its sends once it has at least %.0f in %s.", 100*accountMessagesDegradedShare, accountMessagesDegradedAttempts, accountMessagesRange),
				observed:  accountMessagesObserved(channel) + " metrics_gateway=" + gateway.name,
				evidence:  "Mimir sums per-service counter increases over the historical range; raw source freshness is reported separately. No address, number or message leaves Mimir.",
				context:   "A partial share comes from a provider throttle, timeouts on one host, or rejected destinations; a full refusal pages account-message-channel-down instead. A recovered channel can stay degraded until its failures leave the range.",
				action:    "Check current publisher coverage and contemporaneous \"[aws]account <channel> send failed (<code>)\" lines before attributing a present provider fault; retain the existing incident owner for a confirmed cause.",
				verify:    "Raw source coverage is restored and the range failure share falls under the baseline for two cadences; present delivery is verified independently.",
				playbook:  "SIGNALS.md §19.3",
			})
		} else if channel.sourceFresh {
			findings = append(findings, finding{
				probeId: "mimir/account-messages", tier: tierWarn, class: "account-message-channel-degraded",
				target: target, frame: frame, healthy: true,
			})
		}
	}
	return findings, nil
}
