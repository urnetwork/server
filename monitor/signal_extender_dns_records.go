package monitor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// SIGNALS.md §28.2 maps to signal_extender_dns_records.go and
// signal_extender_dns_records_test.go. What a client resolving the extender
// record name gets: the TXT values of the name, each the base64 of one
// operator-signed extender record (connect/EXTENDER.md C5), asked of public
// resolvers as a client asks them over DoH, decoded as a client decodes them
// (connect.DecodeExtenderDnsRecord), and judged by the issue and expiry times
// signed into each.
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
// Every configured resolver is asked, and the freshest answer is judged, so
// one resolver's stale cache or failure does not decide. The signature is not
// verified: the monitor holds no root key, and the operator's hosted zone has
// no other writer. The name is public; no address or key a record carries
// reaches an alert.
//
// That judges the zone. Its per-country half (extender_dns_countries.go,
// owner, 2026-11-12: "use the dns resolver settings for the target country.
// The sdk gives a dns per country") asks the resolvers each connection region
// takes, from the sdk's per-country DNS settings, the way a client asks them,
// and judges each (country, resolver) on its own: no fresh records
// (extender-dns-country-stale), a partial set (extender-dns-country-partial),
// or unreachable from here, which only holds the resolver's state.
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
	// the public resolvers asked, each an ip:port; the freshest answer is
	// judged
	Resolvers []string
	// the newest record served issued longer ago than this is stale
	MaxNewestAge time.Duration
	// consecutive stale runs before the finding opens
	Sustain int
	// how often the name is resolved
	Cadence time.Duration
	// the most values one answer may carry; more are left undecoded
	MaxRecords int

	// The connection regions whose paths the per-country half asks, each
	// resolver judged on its own; none turns the half off. The default is
	// the sdk's per-country DNS settings (ExtenderDnsCountriesFromSdk).
	Countries []ExtenderDnsCountry
	// the authoritative nameservers (host:port) of the record name's zone,
	// which give the operator's set for a location; none discovers them
	// through the platform resolver
	AuthoritativeServers []string
	// the most time the per-country half of one run may take; a question it
	// leaves unasked or unanswered holds its resolver's state
	CountryBudget time.Duration
	// the most questions of the per-country half in flight at once
	CountryConcurrency int
	// the least time between launching two questions
	CountryPacing time.Duration
	// what a client reads of one DoH answer (connect maxDohResponseBytes)
	ClientDohResponseBytes int
	// what the monitor reads of one DoH answer, to count what a client's read
	// cut off
	FullDohResponseBytes int
}

// The defaults of SIGNALS.md §28.2.
func DefaultExtenderDnsRecordsSettings() *ExtenderDnsRecordsSettings {
	return &ExtenderDnsRecordsSettings{
		RecordNames:            map[string]string{"main": "extender.bringyour.com"},
		Resolvers:              []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"},
		MaxNewestAge:           time.Hour,
		Sustain:                2,
		Cadence:                10 * time.Minute,
		MaxRecords:             256,
		Countries:              ExtenderDnsCountriesFromSdk(),
		CountryBudget:          60 * time.Second,
		CountryConcurrency:     4,
		CountryPacing:          100 * time.Millisecond,
		ClientDohResponseBytes: ExtenderDnsClientDohResponseBytes,
		FullDohResponseBytes:   64 * 1024,
	}
}

func (self *ExtenderDnsRecordsSettings) valid() bool {
	if self == nil || len(self.Resolvers) == 0 || self.MaxNewestAge <= 0 || self.Sustain <= 0 || self.Cadence <= 0 || self.MaxRecords <= 0 {
		return false
	}
	if len(self.Countries) == 0 {
		return true
	}
	return 0 < self.CountryBudget && 0 < self.CountryConcurrency && 0 <= self.CountryPacing &&
		0 < self.ClientDohResponseBytes && self.ClientDohResponseBytes <= self.FullDohResponseBytes &&
		extenderDnsCountriesValid(self.Countries)
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
	// each record's extender, once: its signed public key, or the value
	// when the record names none. Compared in memory only, never reported.
	keys []string
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
		key := string(body.PublicKey)
		if key == "" {
			key = txt
		}
		if !slices.Contains(observation.keys, key) {
			observation.keys = append(observation.keys, key)
		}
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

// Whether this answer is fresher than another: a newer newest record, then
// more valid records, then more served.
func (self extenderDnsRecordsObservation) fresherThan(other extenderDnsRecordsObservation) bool {
	if !self.newestIssue.Equal(other.newestIssue) {
		return other.newestIssue.Before(self.newestIssue)
	}
	if self.valid != other.valid {
		return other.valid < self.valid
	}
	return other.served < self.served
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

// One resolver's answer.
type extenderDnsRecordsAnswer struct {
	observation DNSResponseObservation
	err         error
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

	findings, zoneReasons, err := self.checkZone(ctx, env, settings, name, target)
	if err != nil {
		return nil, err
	}
	findings = append(findings, self.checkCountries(ctx, env, settings, name, target, zoneReasons)...)
	if err := ctx.Err(); err != nil {
		return nil, probeLifecycleError(ctx, err)
	}
	return findings, nil
}

// The whole-zone half: the freshest answer of the configured public
// resolvers. It also returns why that answer is stale, none when it is fresh
// or when no resolver answered: a resolver of the per-country half stale for
// those reasons alone is the zone's staleness, not the resolver's.
func (self extenderDnsRecordsProbe) checkZone(ctx context.Context, env *probeEnv, settings *ExtenderDnsRecordsSettings, name string, target string) ([]finding, []string, error) {
	answers := make([]extenderDnsRecordsAnswer, len(settings.Resolvers))
	var wait sync.WaitGroup
	for index, resolver := range settings.Resolvers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			observation, err := env.runner.dnsTxt(ctx, resolver, name)
			answers[index] = extenderDnsRecordsAnswer{observation: observation, err: err}
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, probeLifecycleError(ctx, err)
	}

	now := env.now()
	answered := 0
	var freshest *extenderDnsRecordsObservation
	var failure error
	for _, answer := range answers {
		if answer.err != nil {
			failure = answer.err
			continue
		}
		switch answer.observation.ResponseCode {
		case DNSResponseSuccess, DNSResponseNameError:
		default:
			// a resolver that could not answer says nothing of the name
			failure = errors.New("resolver answered " + string(answer.observation.ResponseCode))
			continue
		}
		answered++
		observation := observeExtenderDnsRecords(answer.observation.Texts, now, settings.MaxRecords)
		if freshest == nil || observation.fresherThan(*freshest) {
			freshest = &observation
		}
	}
	if freshest == nil {
		return []finding{
			stormClearHold(extenderDnsRecordsProbeId, "extender-dns-records-stale", target),
			extenderDnsRecordsUnobservable(target, "lookup_failed", failure),
		}, nil, nil
	}

	reasons := freshest.staleReasons(now, settings.MaxNewestAge)
	findings := []finding{healthyFinding(extenderDnsRecordsProbeId, tierWarn, "extender-dns-records-unobservable", target)}
	if len(reasons) == 0 {
		return append(findings, healthyFinding(extenderDnsRecordsProbeId, tierWarn, "extender-dns-records-stale", target)), nil, nil
	}
	newestAge := "none"
	if !freshest.newestIssue.IsZero() {
		newestAge = fmt.Sprintf("%.0f", now.Sub(freshest.newestIssue).Seconds())
	}
	return append(findings, finding{
		probeId: extenderDnsRecordsProbeId, tier: tierWarn, class: "extender-dns-records-stale", target: target, sustain: settings.Sustain,
		symptom:   fmt.Sprintf("The extender records public DNS serves for %s are stale: %s", name, strings.Join(reasons, ", ")),
		mechanism: "A client that resolves the extender record name verifies the signed records of its TXT set and adds them to its directory, beside the addresses of the A and AAAA sets. The publish tick re-signs the records every 10 minutes, so a resolver serves records minutes old. Records older than that mean Route 53 has not accepted the tick's TXT changes; expired ones are refused by every client, which then learns no extender from dns at all.",
		baseline:  fmt.Sprintf("The newest record served was issued within about 11 minutes (the 10-minute tick and the sets' 60-second TTL), and every record is valid. WARN when the freshest answer's newest record is older than %s, when every record has expired, or when none is served, for %d consecutive runs %s apart.", settings.MaxNewestAge, settings.Sustain, self.cadence()),
		observed: fmt.Sprintf("name=%s resolvers_answered=%d/%d served=%d records=%d undecodable=%d other=%d valid=%d expired=%d newest_issued_unix=%s newest_issued_age_seconds=%s latest_expires_unix=%s reasons=%s",
			name, answered, len(settings.Resolvers),
			freshest.served, freshest.records, freshest.undecodable, freshest.other,
			freshest.valid, freshest.expired,
			extenderDnsRecordsUnix(freshest.newestIssue), newestAge, extenderDnsRecordsUnix(freshest.latestExpire),
			strings.Join(reasons, ","),
		),
		evidence: "The TXT answer of each configured public resolver over the DNS wire protocol (UDP, then TCP when truncated), each value decoded with connect.DecodeExtenderDnsRecord and judged by the issue and expiry times signed into its record body; the freshest answer is reported. Counts and times only; no record content reaches the alert.",
		context:  "The name is geo-routed: a resolver gets the TXT set of its own location, or the default set, so this judges the sets the resolvers' locations are answered with. A set that fails only for another continent is §27.2's extender-dns-publish-failing. From 2026-10-28 18:12Z to at least 11-11 every TXT record served was the 10-28 18:12Z set, expired since 10-29.",
		action:   "Read §27.2's extender-dns-publish-failing and the taskworker log for `[extenderpublish]dns publish failed`, which names each refused set and Route 53's answer (InvalidChangeBatch: a set's size; AccessDenied: the dns block's credentials). If the publisher reports every change accepted, compare its TXT sets with the hosted zone's record sets for the name.",
		verify:   fmt.Sprintf("A run reads valid records from the resolvers, the newest issued within %s.", settings.MaxNewestAge),
		playbook: "SIGNALS.md §28.2",
	}), reasons, nil
}

// The visibility finding of §28.2: the policy is invalid, or no resolver
// answered. Fixed reasons and the shared error classification only; the
// resolvers' text never leaves it.
func extenderDnsRecordsUnobservable(target string, reason string, err error) finding {
	observed := "visibility_reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: extenderDnsRecordsProbeId, tier: tierWarn, class: "extender-dns-records-unobservable", target: target, sustain: 2,
		symptom:   "The §28.2 signal could not read the extender records public DNS serves",
		mechanism: "No configured public resolver answered the TXT question of the extender record name (a timeout, a server failure, the monitor host's network or its outbound DNS), or the signal's policy is invalid. Whether clients are served fresh records is unknown, not fresh.",
		baseline:  "Every run reads at least one resolver's answer.",
		observed:  observed,
		evidence:  "The fixed reason and the shared error classification only.",
		context:   "A stale finding already open stays open while this lasts.",
		action:    "Check that the monitor host reaches the public resolvers on port 53 (dig +tcp TXT for the name against each), then rerun the signal.",
		verify:    "Two consecutive runs read a resolver's answer.",
		playbook:  "SIGNALS.md §28.2",
	}
}
