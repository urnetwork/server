package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sidecar/dnsservice"
)

// Signal dns-canary implements SIGNALS.md §28.1 (DNS-M5-17; §12.6's monitor, D-DNS-5, R-DNS-7, D-OPS-9,
// FS-50; plan-DNS-canary-route-state, decided by the owner). It runs at the site and on one outside
// host, each minute, for each root dns.yml serves:
//
//   - the site's copy rewrites canary.<root> TXT through the dns API block's journal, its value the
//     writer's unix milliseconds; the outside copy writes nothing, so no write credential leaves the
//     site;
//   - each NS is read directly over TCP, its SOA serial and the DNSKEY RRset's signatures, and the
//     canary is validated through that NS alone by Unbound, with the KSK as a configured trust anchor
//     (stub-addr, trust-anchor-file), so a second implementation checks what miekg/dns signed; a value
//     over canary_fresh old reads as an NS behind;
//   - two public resolvers check propagation only until the DS, then validate as well (DNS-M7-04):
//     once dns.yml's ds_published lists the root, an answer without AD is a propagation warning, and
//     a SERVFAIL that the same question with checking disabled answers is the signature failing for
//     every validator, which pages as dns-canary-signature;
//   - the verdict goes to the block, ok only when every NS validated a fresh value, and GET
//     /dns/ready reads it, so a bad minute pauses certs' orders within the minute;
//   - a clean-days counter (D-DNS-5; DNS-M7-04) runs over the minutes: an ok verdict with no
//     signature failure extends the root's run of clean minutes, a signature failure ends it, and a
//     stretch longer than CleanGap with no clean minute restarts it, since nothing proved those
//     minutes clean. The run is kept in the monitor's state directory, so a restart does not restart
//     the count, and urnetwork_dns_canary_clean_days and urnetwork_dns_ds_ready report it each
//     minute, ready never below DsReadyCleanDays, §12.6's 30: the DS waits for that report.
//
// dns-canary-signature and dns-ns-down page §19.8's rota at any hour from M5 (owner decision 143);
// dns-ns-failing, one NS failing an hour, is plan-DNS-running-ns's business-hours page, which the pager
// holds until the business calendar opens (QD-754); dns-ns-serial-divergence, dns-key-window and
// dns-canary-propagation warn; and dns-canary-unobservable reports a check that could not run (no
// settings, no anchor, no validator, a write or a report the block refused), which holds every other
// incident open, so a blind monitor never clears one.
func NewDnsCanarySignal() Signal {
	return &signalAdapter{
		number: "28.1", key: "dns-canary", name: "DNS canary, nameservers and DNSKEY window",
		probe: dnsCanaryProbe{clean: newDnsCanaryCleanCounter(), gauges: defaultDnsCanaryGauges},
	}
}

const dnsCanaryProbeId = "dns/canary"

// The canary's classes, SIGNALS.md §28.1's.
const (
	dnsCanaryClassSignature    = "dns-canary-signature"
	dnsCanaryClassNsDown       = "dns-ns-down"
	dnsCanaryClassNsFailing    = "dns-ns-failing"
	dnsCanaryClassDivergence   = "dns-ns-serial-divergence"
	dnsCanaryClassKeyWindow    = "dns-key-window"
	dnsCanaryClassPropagation  = "dns-canary-propagation"
	dnsCanaryClassUnobservable = "dns-canary-unobservable"
)

// The canary's settings: monitor.yml's dns_canary block beside dns.yml (config.go).
type DnsCanarySettings struct {
	// dnsservice.DnsCanarySourceSite, which writes and reports, or DnsCanarySourceOutside, which reads
	// and reports.
	Source string
	// The dns API block's base URL.
	ApiUrl string
	// The bearer this copy sends: internal-monitor.yml's, or internal-monitor-outside.yml's.
	Bearer string
	// dns.yml: the roots, the NS and each root's trust anchors.
	Dns *dnsservice.DnsSettings
	// Each NS's addresses on port 53: dns.yml's glue.
	NsAddresses map[string][]netip.AddrPort
	// The public resolvers' addresses, which check propagation until the DS, and validate as well once
	// Dns.DsPublished lists the root (owner decision 135).
	PublicResolvers []netip.AddrPort
	// unbound-host's path. Empty finds it on PATH.
	UnboundHostPath string
	// Each read and the validation's bound.
	Timeout time.Duration
	// How old a canary value may be: plan-DNS-canary-route-state's 180 s.
	Fresh time.Duration
	// The least the served DNSKEY window may have left: §12.6's 7 d.
	MinWindowLeft time.Duration
	// Consecutive failing minutes before each class opens, the page classes' 2 riding out one lost
	// minute; dns-ns-failing's 60 is plan-DNS-running-ns's hour.
	PageSustain        int
	NsFailingSustain   int
	DivergenceSustain  int
	PropagationSustain int
	// The clean canary days before the DS is reported ready: §12.6's 30 (D-DNS-5). Below it the report
	// is never ready, and a non-positive value reports nothing ready.
	DsReadyCleanDays int
	// The longest stretch with no clean minute that a run of clean days outlasts: a monitor restart or
	// one NS's deploy, one host at a time, fits in it; a longer one restarts the count at the next
	// clean minute, since nothing proved the minutes between clean.
	CleanGap time.Duration
	// Why monitor.yml's block, dns.yml or the bearer file did not read. Set, the check reports itself
	// blind rather than off, since a canary asked for and not running must hold incidents open. Never
	// holds a bearer.
	ConfigError string
}

// The production defaults. Source, ApiUrl, Bearer, Dns and the resolvers are the deployment's.
func DefaultDnsCanarySettings() *DnsCanarySettings {
	return &DnsCanarySettings{
		Source:             dnsservice.DnsCanarySourceSite,
		Timeout:            10 * time.Second,
		Fresh:              180 * time.Second,
		MinWindowLeft:      7 * 24 * time.Hour,
		PageSustain:        2,
		NsFailingSustain:   60,
		DivergenceSustain:  3,
		PropagationSustain: 5,
		DsReadyCleanDays:   30,
		CleanGap:           10 * time.Minute,
	}
}

// A deep copy.
func cloneDnsCanarySettings(settings *DnsCanarySettings) *DnsCanarySettings {
	if settings == nil {
		return nil
	}
	clone := *settings
	clone.PublicResolvers = slices.Clone(settings.PublicResolvers)
	clone.NsAddresses = map[string][]netip.AddrPort{}
	for ns, addresses := range settings.NsAddresses {
		clone.NsAddresses[ns] = slices.Clone(addresses)
	}
	return &clone
}

// The dns API block's two canary routes.
type dnsCanaryApi interface {
	writeCanary(ctx context.Context, root string, value string) (uint64, error)
	report(ctx context.Context, report *dnsservice.DnsCanaryReport) error
}

// What one NS answered directly.
type dnsCanaryNsRead struct {
	soaSerial uint32
	// The latest expiration among the DNSKEY RRset's RRSIGs: the window's end.
	windowEnd time.Time
}

// Reads one NS directly. An error means it did not answer.
type dnsCanaryNs interface {
	read(ctx context.Context, root string, addresses []netip.AddrPort) (*dnsCanaryNsRead, error)
}

// What a validator holding the KSK as its anchor made of canary.<root> through one NS.
type dnsCanaryValidation struct {
	// The NS answered at all.
	answered bool
	// Validated to the anchor.
	secure bool
	// A signature or the chain failed.
	bogus  bool
	values []string
}

// Validates through one NS alone. An error means the validator itself could not run: a blind check.
type dnsCanaryValidator interface {
	validate(ctx context.Context, root string, addresses []netip.AddrPort, anchors []string) (*dnsCanaryValidation, error)
}

// What a public resolver answered for the canary's TXT.
type dnsCanaryResolution struct {
	// The answer's rcode: dns.RcodeSuccess, or SERVFAIL where a validating resolver refused the answer.
	rcode  int
	values []string
	// AD: the resolver validated what it answered.
	authenticated bool
}

// Asks a public resolver, recursively, with DNSSEC OK, and with checking disabled when asked (RFC 4035
// §3.2.2), so a validating resolver answers what it would otherwise refuse as bogus. An error means it
// did not answer at all.
type dnsCanaryResolver interface {
	resolve(ctx context.Context, resolver netip.AddrPort, name string, checkingDisabled bool) (*dnsCanaryResolution, error)
}

// Where each minute's clean-days run goes besides the state directory: the process's gauges.
type dnsCanaryGauges interface {
	publishCleanDays(source string, root string, cleanDays int, dsReady bool)
}

// The probe and what outlives one minute: the clean-days counter and the gauges it reports to.
type dnsCanaryProbe struct {
	clean  *dnsCanaryCleanCounter
	gauges func() dnsCanaryGauges
}

func (dnsCanaryProbe) id() string             { return dnsCanaryProbeId }
func (dnsCanaryProbe) tier() string           { return tierPage }
func (dnsCanaryProbe) cadence() time.Duration { return time.Minute }

func (self dnsCanaryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.dnsCanary
	if settings == nil {
		return []finding{{
			probeId: dnsCanaryProbeId, tier: tierWarn, class: dnsCanaryClassUnobservable, target: "dns-canary",
			sustain: 1, configuredOff: true,
			symptom: "monitor.yml has no dns_canary block",
		}}, nil
	}
	if settings.ConfigError != "" || settings.Dns == nil {
		return []finding{{
			probeId: dnsCanaryProbeId, tier: tierWarn, class: dnsCanaryClassUnobservable, target: "dns-canary",
			sustain: 1, unobservable: true,
			symptom:  "the dns canary is asked for and its settings do not read",
			observed: settings.ConfigError,
		}}, nil
	}
	timeout := settings.Timeout
	clean, stateDir := self.clean, env.cfg.stateDir
	if clean == nil {
		// a probe built bare, as check-burn's one-shot run: its minute counts toward nothing kept, and the state
		// directory's run is the monitor's alone
		clean, stateDir = newDnsCanaryCleanCounter(), ""
	}
	var gauges dnsCanaryGauges
	if self.gauges != nil {
		gauges = self.gauges()
	}
	check := &dnsCanaryCheck{
		settings:  settings,
		api:       &dnsCanaryHttpApi{settings: settings, client: &http.Client{Timeout: timeout}},
		ns:        &dnsCanaryDirectNs{timeout: timeout},
		validator: &dnsCanaryUnbound{path: settings.UnboundHostPath, run: dnsCanaryRunCommand},
		resolver:  &dnsCanaryDirectNs{timeout: timeout},
		clean:     clean,
		stateDir:  stateDir,
		gauges:    gauges,
	}
	return check.run(ctx, env.now()), nil
}

// One minute's check over its seams.
type dnsCanaryCheck struct {
	settings  *DnsCanarySettings
	api       dnsCanaryApi
	ns        dnsCanaryNs
	validator dnsCanaryValidator
	resolver  dnsCanaryResolver
	// the clean-days counter, kept under stateDir when it is set
	clean    *dnsCanaryCleanCounter
	stateDir string
	// nil reports to nothing
	gauges dnsCanaryGauges
}

// Every root's findings at now.
func (self *dnsCanaryCheck) run(ctx context.Context, now time.Time) []finding {
	findings := []finding{}
	for _, root := range self.settings.Dns.Roots {
		findings = append(findings, self.runRoot(ctx, root, now)...)
	}
	return findings
}

// A finding of class at target, sustained as the settings say.
func (self *dnsCanaryCheck) finding(class string, target string, symptom string, observed string) finding {
	finding := finding{
		probeId:  dnsCanaryProbeId,
		tier:     tierWarn,
		class:    class,
		target:   target,
		symptom:  symptom,
		observed: observed,
		playbook: "§12.6's runbook (DESIGN.md), dns",
	}
	switch class {
	case dnsCanaryClassSignature, dnsCanaryClassNsDown:
		finding.tier = tierPage
		finding.sustain = self.settings.PageSustain
	case dnsCanaryClassNsFailing:
		// a page for business hours: the pager holds it until the calendar opens
		finding.tier = tierPage
		finding.sustain = self.settings.NsFailingSustain
	case dnsCanaryClassDivergence:
		finding.sustain = self.settings.DivergenceSustain
	case dnsCanaryClassPropagation:
		finding.sustain = self.settings.PropagationSustain
	case dnsCanaryClassUnobservable:
		finding.sustain = 1
		finding.unobservable = true
	default:
		finding.sustain = 1
	}
	return finding
}

// Whether a value the canary holds was written under fresh before now. Any value fresh will do: an NS
// may answer the last write or the one before it.
func dnsCanaryFresh(values []string, now time.Time, fresh time.Duration) bool {
	for _, value := range values {
		unixMilliseconds, err := dnsservice.ParseDnsCanaryValue(value)
		if err == nil && now.Sub(time.UnixMilli(unixMilliseconds)) < fresh {
			return true
		}
	}
	return false
}

func (self *dnsCanaryCheck) runRoot(ctx context.Context, root string, now time.Time) []finding {
	findings := []finding{}
	nsCount := len(self.settings.Dns.Ns)
	if self.settings.Source == dnsservice.DnsCanarySourceSite {
		value := strconv.FormatInt(now.UnixMilli(), 10) + " " + self.settings.Source
		if _, err := self.api.writeCanary(ctx, root, value); err != nil {
			findings = append(findings, self.finding(dnsCanaryClassUnobservable, root, "the canary could not be written through the journal", err.Error()))
		}
	}
	anchors := self.settings.Dns.TrustAnchors[root]
	if len(anchors) == 0 {
		// nothing proved this minute clean or failed
		self.recordClean(root, now, dnsCanaryMinuteUnobserved)
		return append(findings, self.finding(dnsCanaryClassUnobservable, root, "dns.yml names no trust anchor for the root, so nothing validates", ""))
	}
	verdictOk := true
	// a canary that did not validate, at an NS or, once the DS is out, at a public resolver
	signatureFailed := false
	failingNs := []string{}
	soaSerials := map[string]uint32{}
	for _, ns := range self.settings.Dns.Ns {
		addresses := self.settings.NsAddresses[ns]
		target := ns + "." + root
		read, err := self.ns.read(ctx, root, addresses)
		if err != nil {
			failingNs = append(failingNs, ns)
			verdictOk = false
			continue
		}
		soaSerials[ns] = read.soaSerial
		if left := read.windowEnd.Sub(now); left <= self.settings.MinWindowLeft {
			findings = append(findings, self.finding(dnsCanaryClassKeyWindow, target, "the DNSKEY window served has 7 d or less left: a ceremony's windows are due", left.Round(time.Minute).String()))
		}
		validation, err := self.validator.validate(ctx, root, addresses, anchors)
		if err != nil {
			findings = append(findings, self.finding(dnsCanaryClassUnobservable, target, "the validator could not run", err.Error()))
			verdictOk = false
			continue
		}
		switch {
		case !validation.answered:
			failingNs = append(failingNs, ns)
			verdictOk = false
		case validation.bogus || !validation.secure:
			findings = append(findings, self.finding(dnsCanaryClassSignature, target, "the canary does not validate against the KSK", strings.Join(validation.values, " ")))
			verdictOk = false
			signatureFailed = true
		case !dnsCanaryFresh(validation.values, now, self.settings.Fresh):
			// a validated value too old: the NS is behind the journal
			failingNs = append(failingNs, ns)
			verdictOk = false
		}
	}
	slices.Sort(failingNs)
	failingNs = slices.Compact(failingNs)
	switch {
	case nsCount <= len(failingNs):
		findings = append(findings, self.finding(dnsCanaryClassNsDown, root, "every nameserver of the root fails", strings.Join(failingNs, " ")))
	default:
		for _, ns := range failingNs {
			findings = append(findings, self.finding(dnsCanaryClassNsFailing, ns+"."+root, "a nameserver fails its SOA read or serves an old canary", ""))
		}
	}
	distinct := map[uint32]bool{}
	lowest := uint32(0)
	for _, serial := range soaSerials {
		if len(distinct) == 0 || int32(serial-lowest) < 0 {
			lowest = serial
		}
		distinct[serial] = true
	}
	if 1 < len(distinct) {
		findings = append(findings, self.finding(dnsCanaryClassDivergence, root, "the nameservers' SOA serials differ", fmt.Sprint(soaSerials)))
	}
	dsPublished := self.settings.Dns.DsPublishedFor(root)
	for _, resolver := range self.settings.PublicResolvers {
		target := resolver.Addr().String() + " " + root
		switch self.publicVerdict(ctx, resolver, root, now, dsPublished) {
		case dnsCanaryPublicBogus:
			findings = append(findings, self.finding(dnsCanaryClassSignature, target, "a public resolver refuses the canary as bogus once the DS is out: every validator fails the root", ""))
			signatureFailed = true
		case dnsCanaryPublicUnvalidated:
			findings = append(findings, self.finding(dnsCanaryClassPropagation, target, "a public resolver holds the canary but does not validate it once the DS is out", ""))
		case dnsCanaryPublicStale:
			findings = append(findings, self.finding(dnsCanaryClassPropagation, target, "a public resolver lacks a fresh canary", ""))
		}
	}
	report := &dnsservice.DnsCanaryReport{Root: root, Source: self.settings.Source, Serial: uint64(lowest), Ok: verdictOk}
	if err := self.api.report(ctx, report); err != nil {
		findings = append(findings, self.finding(dnsCanaryClassUnobservable, root, "the verdict could not be reported, so GET /dns/ready reads it as failing", err.Error()))
	}
	switch {
	case signatureFailed:
		self.recordClean(root, now, dnsCanaryMinuteFailed)
	case verdictOk:
		self.recordClean(root, now, dnsCanaryMinuteClean)
	default:
		// an NS down, behind or blind: no signature failed, and none proved the minute clean
		self.recordClean(root, now, dnsCanaryMinuteUnobserved)
	}
	return findings
}

// What one public resolver's answer says of the canary.
type dnsCanaryPublicVerdict int

const (
	// a fresh canary, validated once the DS is out
	dnsCanaryPublicOk dnsCanaryPublicVerdict = iota
	// no answer, or none fresh: propagation
	dnsCanaryPublicStale
	// a fresh canary without AD once the DS is out
	dnsCanaryPublicUnvalidated
	// once the DS is out, SERVFAIL for a canary the resolver holds: validation fails
	dnsCanaryPublicBogus
)

// One public resolver's verdict on root's canary at now. Until the DS a resolver checks propagation
// only, so neither AD nor a SERVFAIL's cause is judged; once it is out, an answer must carry AD, and a
// SERVFAIL is asked again with checking disabled, an answer then meaning the canary is there and does
// not validate, where no answer means the resolver itself failed.
func (self *dnsCanaryCheck) publicVerdict(ctx context.Context, resolver netip.AddrPort, root string, now time.Time, dsPublished bool) dnsCanaryPublicVerdict {
	owner := dnsservice.DnsCanaryOwner(root)
	resolution, err := self.resolver.resolve(ctx, resolver, owner, false)
	if err != nil {
		return dnsCanaryPublicStale
	}
	if resolution.rcode == dns.RcodeServerFailure && dsPublished {
		unchecked, err := self.resolver.resolve(ctx, resolver, owner, true)
		if err == nil && unchecked.rcode == dns.RcodeSuccess && 0 < len(unchecked.values) {
			return dnsCanaryPublicBogus
		}
		return dnsCanaryPublicStale
	}
	if resolution.rcode != dns.RcodeSuccess || !dnsCanaryFresh(resolution.values, now, self.settings.Fresh) {
		return dnsCanaryPublicStale
	}
	if dsPublished && !resolution.authenticated {
		return dnsCanaryPublicUnvalidated
	}
	return dnsCanaryPublicOk
}

// Records root's minute in the clean-days counter and reports the run to the gauges. A count that could
// not be kept reports nothing ready, the closed direction.
func (self *dnsCanaryCheck) recordClean(root string, now time.Time, minute dnsCanaryMinute) {
	if self.clean == nil {
		return
	}
	cleanDays, err := self.clean.record(self.stateDir, self.settings.Source, root, now, minute, self.settings.CleanGap)
	if err != nil {
		cleanDays = 0
	}
	if self.gauges != nil {
		self.gauges.publishCleanDays(self.settings.Source, root, cleanDays, dnsCanaryDsReady(cleanDays, self.settings.DsReadyCleanDays))
	}
}

// Whether cleanDays reach the DS: never below readyDays, and never with no readyDays set.
func dnsCanaryDsReady(cleanDays int, readyDays int) bool {
	return 0 < readyDays && readyDays <= cleanDays
}

// The API block over HTTPS with the copy's bearer.
type dnsCanaryHttpApi struct {
	settings *DnsCanarySettings
	client   *http.Client
}

// One POST of body to path, answering into answer when it is not nil.
func (self *dnsCanaryHttpApi) post(ctx context.Context, path string, body any, answer any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(self.settings.ApiUrl, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+self.settings.Bearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := self.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("%s answered %d", path, response.StatusCode)
	}
	if answer == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(answer)
}

func (self *dnsCanaryHttpApi) writeCanary(ctx context.Context, root string, value string) (uint64, error) {
	answer := &dnsservice.DnsCanaryAnswer{}
	if err := self.post(ctx, dnsservice.DnsCanaryPath, &dnsservice.DnsCanaryRequest{Root: root, Value: value}, answer); err != nil {
		return 0, err
	}
	return answer.Serial, nil
}

func (self *dnsCanaryHttpApi) report(ctx context.Context, report *dnsservice.DnsCanaryReport) error {
	return self.post(ctx, dnsservice.DnsCanaryReportPath, report, nil)
}

// Direct DNS reads over TCP: an NS's SOA and DNSKEY signatures, and a public resolver's recursive
// answer with DNSSEC OK.
type dnsCanaryDirectNs struct {
	timeout time.Duration
}

// One query over TCP to the first address that answers.
func (self *dnsCanaryDirectNs) exchange(ctx context.Context, addresses []netip.AddrPort, query *dns.Msg) (*dns.Msg, error) {
	client := &dns.Client{Net: "tcp", Timeout: self.timeout}
	var lastErr error = errors.New("no address")
	for _, address := range addresses {
		reply, _, err := client.ExchangeContext(ctx, query, address.String())
		if err == nil && reply != nil && reply.Rcode == dns.RcodeSuccess {
			return reply, nil
		}
		if err == nil {
			err = fmt.Errorf("rcode %d", reply.Rcode)
		}
		lastErr = err
	}
	return nil, lastErr
}

func (self *dnsCanaryDirectNs) read(ctx context.Context, root string, addresses []netip.AddrPort) (*dnsCanaryNsRead, error) {
	soaQuery := &dns.Msg{}
	soaQuery.SetQuestion(root, dns.TypeSOA)
	soaQuery.RecursionDesired = false
	soaReply, err := self.exchange(ctx, addresses, soaQuery)
	if err != nil {
		return nil, err
	}
	read := &dnsCanaryNsRead{}
	found := false
	for _, record := range soaReply.Answer {
		if soa, ok := record.(*dns.SOA); ok {
			read.soaSerial = soa.Serial
			found = true
		}
	}
	if !found || !soaReply.Authoritative {
		return nil, errors.New("no authoritative SOA")
	}
	keyQuery := &dns.Msg{}
	keyQuery.SetQuestion(root, dns.TypeDNSKEY)
	keyQuery.RecursionDesired = false
	keyQuery.SetEdns0(dns.DefaultMsgSize, true)
	keyReply, err := self.exchange(ctx, addresses, keyQuery)
	if err != nil {
		return nil, err
	}
	for _, record := range keyReply.Answer {
		if rrsig, ok := record.(*dns.RRSIG); ok && rrsig.TypeCovered == dns.TypeDNSKEY {
			end := time.Unix(int64(rrsig.Expiration), 0)
			if end.After(read.windowEnd) {
				read.windowEnd = end
			}
		}
	}
	return read, nil
}

// A public resolver's answer whatever its rcode, since a validating resolver's SERVFAIL is what a bogus
// canary looks like there.
func (self *dnsCanaryDirectNs) resolve(ctx context.Context, resolver netip.AddrPort, name string, checkingDisabled bool) (*dnsCanaryResolution, error) {
	query := &dns.Msg{}
	query.SetQuestion(name, dns.TypeTXT)
	query.SetEdns0(dns.DefaultMsgSize, true)
	query.CheckingDisabled = checkingDisabled
	client := &dns.Client{Net: "tcp", Timeout: self.timeout}
	reply, _, err := client.ExchangeContext(ctx, query, resolver.String())
	if err != nil {
		return nil, err
	}
	if reply == nil || !reply.Response || len(reply.Question) != 1 || !strings.EqualFold(reply.Question[0].Name, name) {
		return nil, errors.New("an answer to another question")
	}
	resolution := &dnsCanaryResolution{rcode: reply.Rcode, values: []string{}, authenticated: reply.AuthenticatedData}
	for _, record := range reply.Answer {
		if txt, ok := record.(*dns.TXT); ok && strings.EqualFold(txt.Hdr.Name, name) {
			resolution.values = append(resolution.values, strings.Join(txt.Txt, ""))
		}
	}
	return resolution, nil
}

// Runs a command and returns its stdout. An error means it did not run; an exit status is not one,
// since the output says what the validator found.
type dnsCanaryCommand func(ctx context.Context, path string, args ...string) (stdout string, err error)

func dnsCanaryRunCommand(ctx context.Context, path string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, path, args...).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return string(output), nil
	}
	return string(output), err
}

// Unbound's validator through one NS: unbound-host with a configuration of its own a run, whose only
// way to the root is a stub zone at the NS and whose only anchor is the root's KSK, so the answer is
// what that NS serves, judged by another implementation than the one that signed it.
type dnsCanaryUnbound struct {
	path string
	run  dnsCanaryCommand
}

// The configuration and anchor file for root at addresses.
func dnsCanaryUnboundConfig(root string, addresses []netip.AddrPort, anchorPath string) string {
	var config strings.Builder
	config.WriteString("server:\n")
	config.WriteString("  module-config: \"validator iterator\"\n")
	fmt.Fprintf(&config, "  trust-anchor-file: %q\n", anchorPath)
	config.WriteString("  do-not-query-localhost: no\n")
	config.WriteString("stub-zone:\n")
	fmt.Fprintf(&config, "  name: %q\n", root)
	for _, address := range addresses {
		fmt.Fprintf(&config, "  stub-addr: %s@%d\n", address.Addr(), address.Port())
	}
	return config.String()
}

// The anchor file: each KSK as a DNSKEY record of root.
func dnsCanaryUnboundAnchors(root string, anchors []string) string {
	var lines strings.Builder
	for _, anchor := range anchors {
		fmt.Fprintf(&lines, "%s 3600 IN DNSKEY %s\n", root, anchor)
	}
	return lines.String()
}

// unbound-host -v's verdict on a TXT lookup: each "has TXT record" line's value and its bracketed
// security, "(secure)", "(insecure)" or "(BOGUS (security failure))"; a validation failure line reads
// as bogus; no answer, as not answered.
func dnsCanaryParseUnbound(output string) *dnsCanaryValidation {
	validation := &dnsCanaryValidation{}
	sawSecure := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, " has TXT record "):
			validation.answered = true
			_, rest, _ := strings.Cut(line, " has TXT record ")
			value := rest
			// the value is quoted, so its security is the bracket after the closing quote
			if index := strings.LastIndex(rest, `" (`); 0 <= index {
				value = rest[:index+1]
				if rest[index+2:] == "(secure)" {
					sawSecure = true
				} else {
					validation.bogus = true
				}
			}
			validation.values = append(validation.values, strings.Trim(value, `"`))
		case strings.Contains(line, "validation failure"), strings.Contains(line, "(BOGUS"):
			validation.answered = true
			validation.bogus = true
		}
	}
	validation.secure = sawSecure && !validation.bogus
	return validation
}

func (self *dnsCanaryUnbound) validate(ctx context.Context, root string, addresses []netip.AddrPort, anchors []string) (*dnsCanaryValidation, error) {
	if len(addresses) == 0 {
		return &dnsCanaryValidation{}, nil
	}
	path := self.path
	if path == "" {
		path = "unbound-host"
	}
	directory, err := os.MkdirTemp("", "dns-canary-unbound-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	anchorPath := filepath.Join(directory, "anchors.key")
	configPath := filepath.Join(directory, "unbound.conf")
	if err := os.WriteFile(anchorPath, []byte(dnsCanaryUnboundAnchors(root, anchors)), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(configPath, []byte(dnsCanaryUnboundConfig(root, addresses, anchorPath)), 0o600); err != nil {
		return nil, err
	}
	stdout, err := self.run(ctx, path, "-C", configPath, "-v", "-t", "TXT", dnsservice.DnsCanaryOwner(root))
	if err != nil {
		return nil, fmt.Errorf("unbound-host did not run: %w", err)
	}
	return dnsCanaryParseUnbound(stdout), nil
}

// One minute of a root's canary, as the clean-days counter takes it.
type dnsCanaryMinute int

const (
	// the verdict ok and no signature failed: the run goes on
	dnsCanaryMinuteClean dnsCanaryMinute = iota
	// a signature failed: the run ends
	dnsCanaryMinuteFailed
	// neither proved: the run is left as it was, and its gap grows
	dnsCanaryMinuteUnobserved
)

// One root's run of clean minutes: its first and its latest.
type dnsCanaryCleanRun struct {
	Since time.Time `json:"since"`
	Last  time.Time `json:"last"`
}

// The counter's file in the state directory: each source and root's run.
type dnsCanaryCleanFile struct {
	Format int                           `json:"format"`
	Runs   map[string]*dnsCanaryCleanRun `json:"runs"`
}

// The counter file's format version.
const dnsCanaryCleanFileFormat = 1

// The counter's file under a state directory.
func dnsCanaryCleanPath(stateDir string) string {
	return filepath.Join(stateDir, "dns-canary", "clean-days.json")
}

// The clean-days counter (D-DNS-5; DNS-M7-04): each root's run of clean canary minutes, in the state
// directory's file when there is one, so a monitor restart keeps the count, else in memory. A file that
// does not read starts every run again, the closed direction: it can only delay the DS. Safe for
// concurrent use.
type dnsCanaryCleanCounter struct {
	stateLock sync.Mutex
	// the runs with no state directory, by source and root
	memoryRuns map[string]*dnsCanaryCleanRun
}

func newDnsCanaryCleanCounter() *dnsCanaryCleanCounter {
	return &dnsCanaryCleanCounter{memoryRuns: map[string]*dnsCanaryCleanRun{}}
}

// Records source's minute of root at now and answers the whole clean days the run holds then. A clean
// minute extends the run, or starts one when the last clean minute is more than gap before it or after
// it (a clock moved back); a failed minute ends the run; an unobserved one leaves it. A run whose last
// clean minute is more than gap before now holds no day, since nothing proved the minutes since clean.
// An error is a file that could not be written, whose count the caller reports as none.
func (self *dnsCanaryCleanCounter) record(stateDir string, source string, root string, now time.Time, minute dnsCanaryMinute, gap time.Duration) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	runs := self.memoryRuns
	if stateDir != "" {
		runs = readDnsCanaryCleanFile(dnsCanaryCleanPath(stateDir))
	}
	key := source + " " + root
	run := runs[key]
	lapsed := func(run *dnsCanaryCleanRun) bool {
		return run == nil || now.Before(run.Last) || gap < now.Sub(run.Last)
	}
	switch minute {
	case dnsCanaryMinuteClean:
		if lapsed(run) {
			run = &dnsCanaryCleanRun{Since: now, Last: now}
		} else {
			run.Last = now
		}
		runs[key] = run
	case dnsCanaryMinuteFailed:
		delete(runs, key)
		run = nil
	}
	if stateDir != "" {
		if err := writeDnsCanaryCleanFile(dnsCanaryCleanPath(stateDir), runs); err != nil {
			return 0, err
		}
	}
	if lapsed(run) {
		return 0, nil
	}
	return int(run.Last.Sub(run.Since) / (24 * time.Hour)), nil
}

// The runs path holds, none when it is absent or does not read.
func readDnsCanaryCleanFile(path string) map[string]*dnsCanaryCleanRun {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]*dnsCanaryCleanRun{}
	}
	file := &dnsCanaryCleanFile{}
	if err := json.Unmarshal(data, file); err != nil || file.Format != dnsCanaryCleanFileFormat || file.Runs == nil {
		return map[string]*dnsCanaryCleanRun{}
	}
	for key, run := range file.Runs {
		if run == nil || run.Last.Before(run.Since) {
			delete(file.Runs, key)
		}
	}
	return file.Runs
}

// Replaces path with runs whole, through a file beside it renamed over it, in a directory only the
// monitor's user reads.
func writeDnsCanaryCleanFile(path string, runs map[string]*dnsCanaryCleanRun) error {
	data, err := json.Marshal(&dnsCanaryCleanFile{Format: dnsCanaryCleanFileFormat, Runs: runs})
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".clean-days-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// The clean-days series (SIGNALS.md §28.1).
const (
	dnsCanaryCleanDaysMetricName = "urnetwork_dns_canary_clean_days"
	dnsDsReadyMetricName         = "urnetwork_dns_ds_ready"
)

// The counter's gauges in a prometheus registry. Safe for concurrent use.
type prometheusDnsCanaryGauges struct {
	cleanDaysGauge *prometheus.GaugeVec
	dsReadyGauge   *prometheus.GaugeVec
}

// The gauges registered in registerer; ones it already holds are taken as they are, so every monitor of
// a process shares one series set.
func newPrometheusDnsCanaryGauges(registerer prometheus.Registerer) dnsCanaryGauges {
	register := func(gauge *prometheus.GaugeVec) *prometheus.GaugeVec {
		err := registerer.Register(gauge)
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := already.ExistingCollector.(*prometheus.GaugeVec); ok {
				return existing
			}
		}
		return gauge
	}
	return &prometheusDnsCanaryGauges{
		cleanDaysGauge: register(prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: dnsCanaryCleanDaysMetricName,
			Help: "Whole days of clean canary minutes for the root, as this monitor copy saw them (D-DNS-5).",
		}, []string{"source", "root"})),
		dsReadyGauge: register(prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: dnsDsReadyMetricName,
			Help: "1 once the root's clean canary days reach §12.6's 30, when its DS may go to the registrar; else 0.",
		}, []string{"source", "root"})),
	}
}

func (self *prometheusDnsCanaryGauges) publishCleanDays(source string, root string, cleanDays int, dsReady bool) {
	self.cleanDaysGauge.WithLabelValues(source, root).Set(float64(cleanDays))
	ready := 0.0
	if dsReady {
		ready = 1
	}
	self.dsReadyGauge.WithLabelValues(source, root).Set(ready)
}

// The process's gauges in its default registry, built once.
var defaultDnsCanaryGauges = sync.OnceValue(func() dnsCanaryGauges {
	return newPrometheusDnsCanaryGauges(prometheus.DefaultRegisterer)
})
