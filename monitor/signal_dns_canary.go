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
	"time"

	"github.com/miekg/dns"

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
//   - two public resolvers check propagation only until the DS, then validate as well;
//   - the verdict goes to the block, ok only when every NS validated a fresh value, and GET
//     /dns/ready reads it, so a bad minute pauses certs' orders within the minute.
//
// dns-canary-signature and dns-ns-down page §19.8's rota at any hour from M5 (owner decision 143);
// dns-ns-failing, dns-ns-serial-divergence, dns-key-window and dns-canary-propagation warn; and
// dns-canary-unobservable reports a check that could not run (no settings, no anchor, no validator, a
// write or a report the block refused), which holds every other incident open, so a blind monitor never
// clears one. plan-DNS-running-ns's business-hours page for one NS failing an hour waits for a
// business-hours path in the pager; until then that is dns-ns-failing on Slack.
func NewDnsCanarySignal() Signal {
	return &signalAdapter{
		number: "28.1", key: "dns-canary", name: "DNS canary, nameservers and DNSKEY window",
		probe: dnsCanaryProbe{},
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
	// The public resolvers' addresses, which check propagation until the DS.
	PublicResolvers []netip.AddrPort
	// The DS is published (owner decision 135): the public resolvers must validate too.
	DsPublished bool
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

// Asks a public resolver, recursively, with DNSSEC OK. An error means it did not answer.
type dnsCanaryResolver interface {
	resolve(ctx context.Context, resolver netip.AddrPort, name string) (values []string, authenticated bool, err error)
}

type dnsCanaryProbe struct{}

func (dnsCanaryProbe) id() string             { return dnsCanaryProbeId }
func (dnsCanaryProbe) tier() string           { return tierPage }
func (dnsCanaryProbe) cadence() time.Duration { return time.Minute }

func (dnsCanaryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
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
	check := &dnsCanaryCheck{
		settings:  settings,
		api:       &dnsCanaryHttpApi{settings: settings, client: &http.Client{Timeout: timeout}},
		ns:        &dnsCanaryDirectNs{timeout: timeout},
		validator: &dnsCanaryUnbound{path: settings.UnboundHostPath, run: dnsCanaryRunCommand},
		resolver:  &dnsCanaryDirectNs{timeout: timeout},
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
		return append(findings, self.finding(dnsCanaryClassUnobservable, root, "dns.yml names no trust anchor for the root, so nothing validates", ""))
	}
	verdictOk := true
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
	for _, resolver := range self.settings.PublicResolvers {
		values, authenticated, err := self.resolver.resolve(ctx, resolver, dnsservice.DnsCanaryOwner(root))
		if err != nil || !dnsCanaryFresh(values, now, self.settings.Fresh) || (self.settings.DsPublished && !authenticated) {
			findings = append(findings, self.finding(dnsCanaryClassPropagation, resolver.Addr().String()+" "+root, "a public resolver lacks a fresh canary, or does not validate it once the DS is out", ""))
		}
	}
	report := &dnsservice.DnsCanaryReport{Root: root, Source: self.settings.Source, Serial: uint64(lowest), Ok: verdictOk}
	if err := self.api.report(ctx, report); err != nil {
		findings = append(findings, self.finding(dnsCanaryClassUnobservable, root, "the verdict could not be reported, so GET /dns/ready reads it as failing", err.Error()))
	}
	return findings
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

func (self *dnsCanaryDirectNs) resolve(ctx context.Context, resolver netip.AddrPort, name string) ([]string, bool, error) {
	query := &dns.Msg{}
	query.SetQuestion(name, dns.TypeTXT)
	query.SetEdns0(dns.DefaultMsgSize, true)
	reply, err := self.exchange(ctx, []netip.AddrPort{resolver}, query)
	if err != nil {
		return nil, false, err
	}
	values := []string{}
	for _, record := range reply.Answer {
		if txt, ok := record.(*dns.TXT); ok {
			values = append(values, strings.Join(txt.Txt, ""))
		}
	}
	return values, reply.AuthenticatedData, nil
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
