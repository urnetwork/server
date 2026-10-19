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
// warns when a channel that still delivers fails a large share. Job, channel
// and result are the bounded labels; no address, phone number or message
// leaves Mimir.
func NewAccountMessagesSignal() Signal {
	return &signalAdapter{
		number: "19.3", key: "account-messages", name: "Account message delivery",
		probe: accountMessagesProbe{},
	}
}

type accountMessagesProbe struct{}

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

// Per sending service, channel and result: the sends in the range.
func accountMessagesQuery(environment string) string {
	return fmt.Sprintf(
		`sum by (job,channel,result) (increase(urnetwork_account_message_sends_total{env=%s}[%s]))`,
		strconv.Quote(environment), accountMessagesRange,
	)
}

// One channel's sends in the range, in total and by sending service.
type accountMessagesChannel struct {
	name      string
	sent      float64
	failed    float64
	timeout   float64
	jobFailed map[string]float64
	jobSent   map[string]float64
}

func (self *accountMessagesChannel) failures() float64 {
	return self.failed + self.timeout
}

func (self *accountMessagesChannel) attempts() float64 {
	return self.sent + self.failed + self.timeout
}

// Groups the evaluation by channel. A sample that is unlabeled, stale,
// negative, or names a channel or result the producer does not count makes
// the whole frame unobservable rather than a measured zero. No series at all
// means no sending process exports the counter, which is also unknown: every
// running API has sent sign-in mail since it started.
func parseAccountMessagesFrame(response mimirInstantResponse, now time.Time) (map[string]*accountMessagesChannel, string) {
	if len(response.Data.Result) == 0 {
		return nil, "no_series"
	}
	channels := map[string]*accountMessagesChannel{}
	for _, name := range accountMessagesChannels {
		channels[name] = &accountMessagesChannel{name: name, jobFailed: map[string]float64{}, jobSent: map[string]float64{}}
	}
	for _, series := range response.Data.Result {
		job, channelName, result := series.Metric["job"], series.Metric["channel"], series.Metric["result"]
		if job == "" || channelName == "" || result == "" {
			return nil, "unlabeled_series"
		}
		channel, ok := channels[channelName]
		if !ok || !accountMessagesResults[result] {
			return nil, "unknown_channel_or_result"
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || now.Sub(observedAt) > accountMessagesFreshness || now.Sub(observedAt) < -30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, "invalid_or_stale_metric"
		}
		switch result {
		case "sent":
			channel.sent += value
			channel.jobSent[job] += value
		case "failed":
			channel.failed += value
			channel.jobFailed[job] += value
		case "timeout":
			channel.timeout += value
			channel.jobFailed[job] += value
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
	return fmt.Sprintf(
		"channel=%s range=%s sent=%.0f failed=%.0f timeout=%.0f failure_share=%.3f failed_by_job=%s",
		channel.name, accountMessagesRange, channel.sent, channel.failed, channel.timeout,
		channel.failures()/channel.attempts(), strings.Join(parts, ","),
	)
}

func (accountMessagesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("account messages: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(accountMessagesQuery(env.cfg.env))
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 1048576 '"+queryURL+"'")
	if err != nil {
		return nil, fmt.Errorf("account messages: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("account messages: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("account messages: Mimir response was not a successful vector")
	}

	target := env.cfg.env + "/account-messages"
	channels, visibilityReason := parseAccountMessagesFrame(response, env.now().UTC())
	if visibilityReason != "" {
		return []finding{{
			probeId: "mimir/account-messages", tier: tierWarn, class: "account-message-sends-unobservable",
			target: target, sustain: 2,
			symptom:   "The account message send counters cannot be read, so a channel that delivers nothing is indistinguishable from a healthy one.",
			mechanism: "Every account email and SMS is one SES or Pinpoint SMS call counted by urnetwork_account_message_sends_total{channel,result}. Without readable counters a refused or timing-out channel stays silent: the failure log is info-level and rate limited.",
			baseline:  "Every sample is labeled by job, channel (email, sms) and result (sent, failed, timeout), fresh, and non-negative, and at least one sending process exports the counter.",
			observed:  "visibility_reason=" + visibilityReason + " metrics_gateway=" + gateway.name,
			evidence:  "Only a fixed structural reason is exported; raw samples are discarded.",
			context:   "no_series means no process exports the counter: an API or Taskworker artifact older than the send timeout commit, or a stats pusher that is down. It is not a quiet day: the counter's series persist from a process's first send until it exits.",
			action:    "Check that the API and Taskworker artifacts export urnetwork_account_message_sends_total and that their stats pushers reach Mimir (§8.12), then recheck.",
			verify:    "The counters return readable samples for two cadences.",
			playbook:  "SIGNALS.md §19.3",
		}}, nil
	}

	findings := []finding{healthyFinding("mimir/account-messages", tierWarn, "account-message-sends-unobservable", target)}
	for _, name := range accountMessagesChannels {
		channel := channels[name]
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
				mechanism: "Sign-in and verification codes, password resets and payout or earnings notices for this channel all fail at the provider call. A refusal that repeats with no success (an IAM policy without ses:SendEmail or sms-voice:SendTextMessage, a suspended identity or origination number, an exhausted quota) is not transient: every user who signs in by this channel is locked out.",
				baseline:  fmt.Sprintf("Every channel that sends delivers, or fails fewer than %.0f sends in %s.", accountMessagesDownFloor, accountMessagesRange),
				observed:  accountMessagesObserved(channel) + " metrics_gateway=" + gateway.name,
				evidence:  "Mimir sums the per-service send counters by channel and result over the range; no address, number or message leaves Mimir.",
				context:   "The API reports a failed send at most once a report interval as \"[aws]account <channel> send failed (<code>)\", at info level, so the log rate understates the failures and no log class pages on it. The SDK error code on that line names the refusal; AccessDeniedException is an IAM policy, ThrottlingException a quota. Taskworker sends (earnings and payout notices) burst when an epoch finalizes.",
				action:    "Read the API's or Taskworker's \"[aws]account <channel> send failed (<code>)\" line for the error code, then fix the provider side: grant the messaging IAM user the denied action on the configured identity or origination number, restore the identity, or raise the quota. Do not retry sends by hand or restart services for this alone: the credential and policy are read per call.",
				verify:    "At least one send on the channel succeeds (sent > 0 in the range) and the alert clears; then confirm a real sign-in code arrives.",
				playbook:  "SIGNALS.md §19.3",
			})
		} else {
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
				mechanism: "Part of the channel's provider calls fail or time out: some users never receive their code or notice.",
				baseline:  fmt.Sprintf("A channel fails under %.0f%% of its sends once it has at least %.0f in %s.", 100*accountMessagesDegradedShare, accountMessagesDegradedAttempts, accountMessagesRange),
				observed:  accountMessagesObserved(channel) + " metrics_gateway=" + gateway.name,
				evidence:  "Mimir sums the per-service send counters by channel and result over the range; no address, number or message leaves Mimir.",
				context:   "A partial share comes from a provider throttle, timeouts on one host, or rejected destinations; a full refusal pages account-message-channel-down instead. A recovered channel can stay degraded until its failures leave the range.",
				action:    "Read the \"[aws]account <channel> send failed (<code>)\" lines of the failing services for the error codes and fix the provider-side cause.",
				verify:    "The failure share falls under the baseline for two cadences.",
				playbook:  "SIGNALS.md §19.3",
			})
		} else {
			findings = append(findings, finding{
				probeId: "mimir/account-messages", tier: tierWarn, class: "account-message-channel-degraded",
				target: target, frame: frame, healthy: true,
			})
		}
	}
	return findings, nil
}
