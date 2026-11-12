package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// SIGNALS.md §28.2 maps to signal_extender_dns_records.go and
// signal_extender_dns_records_test.go. What a client resolving the extender
// record name gets: the TXT values of the name, each the base64 of one
// operator-signed extender record (connect/EXTENDER.md C5), read through the
// platform resolver as a client's system resolver would, decoded as a client
// decodes them (connect.DecodeExtenderDnsRecord), and judged by the issue and
// expiry times signed into each.
//
// The publish tick re-signs every record of the TXT sets every 10 minutes, and
// the sets carry a 60-second TTL, so the newest record a resolver serves is
// minutes old. From 2026-10-28 18:12Z Route 53 refused every tick's batch,
// while the dns gauges, drawn from the sets the tick wanted, read healthy; the
// public name kept serving the records of 10-28 18:12Z, which expired on 10-29
// (factury/research/extender-health-2026-11-11.md §1.4). This is the end of
// the dns channel: §27.2 reports the publisher's failures, this what clients
// are served, whatever the publisher believes.
//
// The signature is not verified: the monitor holds no root key, and the
// operator's hosted zone has no other writer. The name is public; no address
// or key a record carries reaches an alert.
func NewExtenderDnsRecordsSignal() Signal {
	return NewExtenderDnsRecordsSignalWithSettings(DefaultExtenderDnsRecordsSettings())
}

// The signal with explicit thresholds.
func NewExtenderDnsRecordsSignalWithSettings(settings *ExtenderDnsRecordsSettings) Signal {
	return &signalAdapter{
		number: "28.2", key: "extender-dns-records", name: "Extender records in public DNS",
		probe: extenderDnsRecordsProbe{settings: settings},
	}
}

const extenderDnsRecordsProbeId = "dns/extender-dns-records"

// The thresholds and bounds of §28.2.
type ExtenderDnsRecordsSettings struct {
	// the extender record name clients resolve, by environment; an
	// environment with none is not judged. A client derives it as
	// extender.<network host> (sdk ExtenderDnsName).
	RecordNames map[string]string
	// the newest record served issued longer ago than this is stale
	MaxNewestAge time.Duration
	// consecutive stale runs before the finding opens
	Sustain int
	// how often the name is resolved
	Cadence time.Duration
	// the most values one answer may carry; more are left undecoded
	MaxRecords int
}

// The defaults of SIGNALS.md §28.2.
func DefaultExtenderDnsRecordsSettings() *ExtenderDnsRecordsSettings {
	return &ExtenderDnsRecordsSettings{
		RecordNames:  map[string]string{"main": "extender.bringyour.com"},
		MaxNewestAge: time.Hour,
		Sustain:      2,
		Cadence:      10 * time.Minute,
		MaxRecords:   256,
	}
}

func (self *ExtenderDnsRecordsSettings) valid() bool {
	return self != nil && 0 < self.MaxNewestAge && 0 < self.Sustain && 0 < self.Cadence && 0 < self.MaxRecords
}

type extenderDnsRecordsProbe struct {
	settings *ExtenderDnsRecordsSettings
}

func (extenderDnsRecordsProbe) id() string   { return extenderDnsRecordsProbeId }
func (extenderDnsRecordsProbe) tier() string { return tierWarn }

func (self extenderDnsRecordsProbe) cadence() time.Duration {
	if settings := self.effectiveSettings(); 0 < settings.Cadence {
		return settings.Cadence
	}
	return DefaultExtenderDnsRecordsSettings().Cadence
}

func (self extenderDnsRecordsProbe) effectiveSettings() *ExtenderDnsRecordsSettings {
	if self.settings == nil {
		return DefaultExtenderDnsRecordsSettings()
	}
	return self.settings
}

// What one answer carried.
type extenderDnsRecordsObservation struct {
	// TXT values served
	served int
	// values that decoded to a signed record
	records int
	// values that are not a gossip message, or a record whose body does not
	// decode, or past MaxRecords
	undecodable int
	// messages that decoded to something other than a record
	other int
	// records whose expiry is after now, and those past it
	valid   int
	expired int
	// the newest issue and the latest expiry among the records
	newestIssue  time.Time
	latestExpire time.Time
}

// Decodes every served value as a client does and reduces the answer to its
// counts and times.
func observeExtenderDnsRecords(txts []string, now time.Time, maxRecords int) extenderDnsRecordsObservation {
	observation := extenderDnsRecordsObservation{served: len(txts)}
	for index, txt := range txts {
		if maxRecords <= index {
			observation.undecodable++
			continue
		}
		message, err := connect.DecodeExtenderDnsRecord(txt)
		if err != nil {
			observation.undecodable++
			continue
		}
		record := message.GetRecord()
		if record == nil {
			observation.other++
			continue
		}
		body := &protocol.ExtenderRecordBody{}
		if err := proto.Unmarshal(record.Body, body); err != nil {
			observation.undecodable++
			continue
		}
		observation.records++
		issue := time.UnixMilli(int64(body.IssueTimeMs)).UTC()
		expire := time.UnixMilli(int64(body.ExpireTimeMs)).UTC()
		if observation.newestIssue.Before(issue) {
			observation.newestIssue = issue
		}
		if observation.latestExpire.Before(expire) {
			observation.latestExpire = expire
		}
		if now.Before(expire) {
			observation.valid++
		} else {
			observation.expired++
		}
	}
	return observation
}

// Why an answer is stale, in a fixed order; none when it is fresh.
func (self extenderDnsRecordsObservation) staleReasons(now time.Time, maxNewestAge time.Duration) []string {
	reasons := []string{}
	switch {
	case self.served == 0:
		reasons = append(reasons, "none_served")
	case self.records == 0:
		reasons = append(reasons, "none_decoded")
	default:
		if maxNewestAge < now.Sub(self.newestIssue) {
			reasons = append(reasons, "newest_too_old")
		}
		if self.valid == 0 {
			reasons = append(reasons, "all_expired")
		}
	}
	sort.Strings(reasons)
	return reasons
}

// A time for an observed field: unix seconds, or none.
func extenderDnsRecordsUnix(value time.Time) string {
	if value.IsZero() {
		return "none"
	}
	return fmt.Sprintf("%d", value.Unix())
}

func (self extenderDnsRecordsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	target := env.cfg.env + "/extender-dns-records"
	if !settings.valid() {
		return []finding{extenderDnsRecordsUnobservable(target, "invalid_policy", nil)}, nil
	}
	name := strings.TrimSpace(settings.RecordNames[env.cfg.env])
	if name == "" {
		// the environment publishes no extender record name
		return nil, nil
	}
	txts, err := env.runner.dnsTxt(ctx, name)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		if !dnsNameNotFound(err) {
			return []finding{
				stormClearHold(extenderDnsRecordsProbeId, "extender-dns-records-stale", target),
				extenderDnsRecordsUnobservable(target, "lookup_failed", err),
			}, nil
		}
		// no such name, or no TXT records under it: nothing is served
		txts = nil
	}
	now := env.now()
	observation := observeExtenderDnsRecords(txts, now, settings.MaxRecords)
	reasons := observation.staleReasons(now, settings.MaxNewestAge)
	findings := []finding{healthyFinding(extenderDnsRecordsProbeId, tierWarn, "extender-dns-records-unobservable", target)}
	if len(reasons) == 0 {
		return append(findings, healthyFinding(extenderDnsRecordsProbeId, tierWarn, "extender-dns-records-stale", target)), nil
	}
	newestAge := "none"
	if !observation.newestIssue.IsZero() {
		newestAge = fmt.Sprintf("%.0f", now.Sub(observation.newestIssue).Seconds())
	}
	return append(findings, finding{
		probeId: extenderDnsRecordsProbeId, tier: tierWarn, class: "extender-dns-records-stale", target: target, sustain: settings.Sustain,
		symptom:   fmt.Sprintf("The extender records public DNS serves for %s are stale: %s", name, strings.Join(reasons, ", ")),
		mechanism: "A client that resolves the extender record name verifies the signed records of its TXT set and adds them to its directory, beside the addresses of the A and AAAA sets. The publish tick re-signs the records every 10 minutes, so a resolver serves records minutes old. Records older than that mean Route 53 has not accepted the tick's TXT changes; expired ones are refused by every client, which then learns no extender from dns at all.",
		baseline:  fmt.Sprintf("The newest record served was issued within about 11 minutes (the 10-minute tick and the sets' 60-second TTL), and every record is valid. WARN when the newest is older than %s, when every record has expired, or when none is served, for %d consecutive runs %s apart.", settings.MaxNewestAge, settings.Sustain, self.cadence()),
		observed: fmt.Sprintf("name=%s served=%d records=%d undecodable=%d other=%d valid=%d expired=%d newest_issued_unix=%s newest_issued_age_seconds=%s latest_expires_unix=%s reasons=%s",
			name, observation.served, observation.records, observation.undecodable, observation.other,
			observation.valid, observation.expired,
			extenderDnsRecordsUnix(observation.newestIssue), newestAge, extenderDnsRecordsUnix(observation.latestExpire),
			strings.Join(reasons, ","),
		),
		evidence: "The monitor host's platform resolver's TXT answer for the name, each value decoded with connect.DecodeExtenderDnsRecord and judged by the issue and expiry times signed into its record body. Counts and times only; no record content reaches the alert.",
		context:  "The name is geo-routed: a resolver gets the TXT set of its own continent, or the default set, so this judges the set the monitor's location is answered with. A set that fails only for another continent is §27.2's extender-dns-publish-failing. From 2026-10-28 18:12Z to at least 11-11 every TXT record served was the 10-28 18:12Z set, expired since 10-29.",
		action:   "Read §27.2's extender-dns-publish-failing and the taskworker log for `[extenderpublish]dns publish failed`, which names each refused set and Route 53's answer (InvalidChangeBatch: a set's size; AccessDenied: the dns block's credentials). If the publisher reports every change accepted, compare its TXT sets with what Route 53 serves (the hosted zone's record sets for the name) and the resolver's TTL.",
		verify:   fmt.Sprintf("A run resolves the name to valid records, the newest issued within %s.", settings.MaxNewestAge),
		playbook: "SIGNALS.md §28.2",
	}), nil
}

// The visibility finding of §28.2: the policy is invalid, or the lookup failed
// for a reason other than the name serving nothing. Fixed reasons and the
// shared error classification only; the resolver's text never leaves it.
func extenderDnsRecordsUnobservable(target string, reason string, err error) finding {
	observed := "visibility_reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: extenderDnsRecordsProbeId, tier: tierWarn, class: "extender-dns-records-unobservable", target: target, sustain: 2,
		symptom:   "The §28.2 signal could not read the extender records public DNS serves",
		mechanism: "The TXT lookup of the extender record name failed (a timeout, a server failure, the monitor host's resolver or network), or the signal's policy is invalid. Whether clients are served fresh records is unknown, not fresh.",
		baseline:  "Every run resolves the name and decodes its answer.",
		observed:  observed,
		evidence:  "The fixed reason and the shared error classification only.",
		context:   "A stale finding already open stays open while this lasts.",
		action:    "Check the monitor host's resolver and network (dig TXT for the name against a public resolver), then rerun the signal.",
		verify:    "Two consecutive runs resolve the name.",
		playbook:  "SIGNALS.md §28.2",
	}
}
