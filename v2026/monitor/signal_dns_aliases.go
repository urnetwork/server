package monitor

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §18.3 maps to signal_dns_aliases.go and
// signal_dns_aliases_test.go. Exact authoritative RRsets come only from the
// optional dns_aliases desired state in monitor.yml; recursive answers are an
// independent client-view family check and are never used as the baseline.
func NewDNSAliasesSignal() Signal {
	return &signalAdapter{
		number: "18.3",
		key:    "dns-aliases",
		name:   "Authoritative DNS aliases and recursive address families",
		probe:  dnsAliasesProbe{},
	}
}

type dnsAliasesProbe struct{}

func (dnsAliasesProbe) id() string             { return "synthetic/dns-aliases" }
func (dnsAliasesProbe) tier() string           { return tierPage }
func (dnsAliasesProbe) cadence() time.Duration { return 5 * time.Minute }

type dnsAliasTarget struct {
	zone       string
	hostname   string
	recordType DNSRecordType
	expected   []string
}

type dnsAliasResult struct {
	target        dnsAliasTarget
	authoritative DNSAuthoritativeObservation
	authorityErr  error
	recursive     DNSResponseObservation
	recursiveErr  error
}

type dnsAuthorityState int

const (
	dnsAuthorityUnknown dnsAuthorityState = iota
	dnsAuthorityBroken
	dnsAuthorityExact
)

func (dnsAliasesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	configured := env.cfg.dnsAliases
	if !configured.Enabled {
		return nil, nil
	}
	targets, invalid := dnsAliasTargets(configured, env.cfg.env)
	if len(invalid) > 0 {
		return []finding{dnsAliasConfigFinding(invalid)}, nil
	}

	results := make(chan dnsAliasResult, len(targets))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, queued := range targets {
		target := queued
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- dnsAliasResult{target: target, authorityErr: ctx.Err()}
				return
			}

			result := dnsAliasResult{target: target}
			result.authoritative, result.authorityErr = env.runner.dnsAuthoritative(
				ctx, target.zone, target.hostname, target.recordType,
			)
			if ctx.Err() == nil {
				result.recursive, result.recursiveErr = env.runner.dnsRecursive(
					ctx, target.hostname, target.recordType,
				)
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	ordered := make([]dnsAliasResult, 0, len(targets))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].target.hostname != ordered[j].target.hostname {
			return ordered[i].target.hostname < ordered[j].target.hostname
		}
		return ordered[i].target.recordType < ordered[j].target.recordType
	})

	findings := []finding{healthyFinding(
		"synthetic/dns-aliases", tierPage, "dns-alias-config-invalid", "monitor-config/dns-aliases",
	)}
	for _, result := range ordered {
		authorityFindings, authorityState := dnsAuthoritativeFindings(result)
		findings = append(findings, authorityFindings...)
		findings = append(findings, dnsRecursiveFindings(result, authorityState)...)
	}
	return findings, nil
}

func dnsAliasTargets(settings DNSAliasSettings, environment string) ([]dnsAliasTarget, []string) {
	invalid := []string{}
	environment = strings.ToLower(strings.TrimSpace(environment))
	if !validDNSLabel(environment) {
		invalid = append(invalid, "environment")
	}

	domains, ok := canonicalDomains(settings.ManagedDomains)
	if !ok || len(domains) == 0 {
		invalid = append(invalid, "managed_domains")
	}
	expectedA, okA := canonicalConfiguredAddresses(settings.ExpectedA, DNSRecordA)
	if !okA || len(expectedA) == 0 {
		invalid = append(invalid, "expected_a")
	}
	expectedAAAA, okAAAA := canonicalConfiguredAddresses(settings.ExpectedAAAA, DNSRecordAAAA)
	if !okAAAA || len(expectedAAAA) == 0 {
		invalid = append(invalid, "expected_aaaa")
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return nil, invalid
	}

	type aliasShape struct {
		prefix       string
		expectedA    []string
		expectedAAAA []string
	}
	shapes := []aliasShape{
		{prefix: "alt", expectedA: expectedA, expectedAAAA: expectedAAAA},
		{prefix: environment + "-alt", expectedA: expectedA, expectedAAAA: expectedAAAA},
		{prefix: "alt-v4", expectedA: expectedA},
		{prefix: environment + "-alt-v4", expectedA: expectedA},
		{prefix: "alt-v6", expectedAAAA: expectedAAAA},
		{prefix: environment + "-alt-v6", expectedAAAA: expectedAAAA},
	}
	targets := make([]dnsAliasTarget, 0, len(domains)*len(shapes)*2)
	seen := map[string]struct{}{}
	for _, domain := range domains {
		for _, shape := range shapes {
			hostname := shape.prefix + "." + domain
			for _, family := range []struct {
				recordType DNSRecordType
				expected   []string
			}{
				{recordType: DNSRecordA, expected: shape.expectedA},
				{recordType: DNSRecordAAAA, expected: shape.expectedAAAA},
			} {
				identity := hostname + "/" + string(family.recordType)
				if _, duplicate := seen[identity]; duplicate {
					invalid = append(invalid, "generated_aliases")
					continue
				}
				seen[identity] = struct{}{}
				targets = append(targets, dnsAliasTarget{
					zone: domain, hostname: hostname, recordType: family.recordType,
					expected: append([]string(nil), family.expected...),
				})
			}
		}
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return nil, invalid
	}
	return targets, nil
}

func dnsAliasConfigFinding(invalid []string) finding {
	return finding{
		probeId: "synthetic/dns-aliases", tier: tierPage,
		class: "dns-alias-config-invalid", target: "monitor-config/dns-aliases", sustain: 1,
		symptom:   "The DNS alias probe is enabled but its desired-state configuration is incomplete or invalid.",
		mechanism: "Without explicit managed domains and nonempty, family-correct A and AAAA expectations, the monitor cannot distinguish a correct authoritative RRset from the wrong initial DNS state.",
		baseline:  "A present dns_aliases block has valid managed_domains, expected_a, and expected_aaaa values and produces six distinct alias names per domain.",
		observed:  fmt.Sprintf("invalid_category_count=%d invalid_categories=%s", len(invalid), strings.Join(invalid, ",")),
		action:    "Correct the operator-owned monitor.yml dns_aliases block from the intended Route 53 desired state. Do not copy current DNS answers into the configuration merely to make the probe green.",
		verify:    "Rerun dns-aliases and require every authoritative nameserver to return the exact configured direct RRsets and every recursive family check to pass.",
		playbook:  "SIGNALS.md §18.3",
	}
}

func dnsAuthoritativeFindings(result dnsAliasResult) ([]finding, dnsAuthorityState) {
	target := result.target.hostname
	frame := string(result.target.recordType)
	visibility := dnsFindingBase(result.target, tierWarn, "dns-authoritative-unobservable", 2)
	visibility.symptom = fmt.Sprintf("The monitor could not fully observe every authoritative %s response for %s.", frame, target)
	visibility.mechanism = "Nameserver discovery, direct wire transport, or structural DNS decoding was incomplete. The exact authoritative RRset remains unknown even when one authority or recursive cache answers."
	visibility.action = "Restore bounded direct DNS observation and query every authoritative nameserver. Do not change records or infer health from a recursive answer until this boundary is observable."

	if result.authorityErr != nil {
		visibility.observed = fmt.Sprintf(
			"record_type=%s expected_count=%d error_class=%s",
			frame, len(result.target.expected), classifyObservationError(result.authorityErr),
		)
		return []finding{visibility}, dnsAuthorityUnknown
	}

	observation := result.authoritative
	if observation.NameserverCount <= 0 || observation.FailedNameservers < 0 ||
		observation.FailedNameservers+len(observation.Responses) != observation.NameserverCount {
		visibility.observed = fmt.Sprintf(
			"record_type=%s expected_count=%d error_class=observation-malformed authority_count=%d response_count=%d failed_count=%d",
			frame, len(result.target.expected), observation.NameserverCount,
			len(observation.Responses), observation.FailedNameservers,
		)
		return []finding{visibility}, dnsAuthorityUnknown
	}

	matching := 0
	mismatching := 0
	nonAuthoritative := 0
	nonSuccess := 0
	cnameCount := 0
	missingCount := 0
	unexpectedCount := 0
	malformed := 0
	responseCodes := map[DNSResponseCode]int{}
	for _, response := range observation.Responses {
		if !validDNSResponseCode(response.ResponseCode) {
			malformed++
			continue
		}
		responseCodes[response.ResponseCode]++
		addresses, valid := canonicalObservedAddresses(response.Addresses, result.target.recordType)
		if !valid || response.CNAMECount < 0 {
			malformed++
			continue
		}
		missing, unexpected := dnsSetDelta(result.target.expected, addresses)
		missingCount += missing
		unexpectedCount += unexpected
		cnameCount += response.CNAMECount
		if !response.Authoritative {
			nonAuthoritative++
		}
		if response.ResponseCode != DNSResponseSuccess {
			nonSuccess++
		}
		if response.Authoritative && response.ResponseCode == DNSResponseSuccess &&
			response.CNAMECount == 0 && missing == 0 && unexpected == 0 {
			matching++
		} else {
			mismatching++
		}
	}

	findings := []finding{}
	if malformed > 0 || observation.FailedNameservers > 0 {
		visibility.observed = fmt.Sprintf(
			"record_type=%s expected_count=%d authority_count=%d response_count=%d failed_count=%d malformed_count=%d",
			frame, len(result.target.expected), observation.NameserverCount,
			len(observation.Responses), observation.FailedNameservers, malformed,
		)
		findings = append(findings, visibility)
	} else {
		findings = append(findings, healthyFinding(
			"synthetic/dns-aliases", tierWarn, "dns-authoritative-unobservable", target,
		))
		findings[len(findings)-1].frame = frame
	}

	state := dnsAuthorityUnknown
	if mismatching > 0 {
		state = dnsAuthorityBroken
		mismatch := dnsFindingBase(result.target, tierPage, "dns-authoritative-rrset", 1)
		mismatch.symptom = fmt.Sprintf("At least one authoritative nameserver returned the wrong direct %s RRset for %s.", frame, target)
		mismatch.mechanism = "An authority returned a non-authoritative or non-NOERROR response, a CNAME, or an address set that differs from the explicit operator-owned desired state. Recursive DNS rotation can hide this split authority."
		mismatch.observed = fmt.Sprintf(
			"record_type=%s expected_count=%d authority_count=%d matching_count=%d mismatching_count=%d non_authoritative_count=%d non_success_count=%d cname_count=%d missing_count=%d unexpected_count=%d response_codes=%s",
			frame, len(result.target.expected), observation.NameserverCount, matching,
			mismatching, nonAuthoritative, nonSuccess, cnameCount, missingCount,
			unexpectedCount, formatDNSResponseCodeCounts(responseCodes),
		)
		mismatch.action = "Compare the intended Route 53 RRset privately with every authoritative nameserver and repair only the proven hostname and family. Preserve intentional single-stack aliases; do not infer desired values from current DNS or change unrelated records."
		findings = append(findings, mismatch)
	} else if malformed == 0 && observation.FailedNameservers == 0 && matching == observation.NameserverCount {
		state = dnsAuthorityExact
		healthy := healthyFinding("synthetic/dns-aliases", tierPage, "dns-authoritative-rrset", target)
		healthy.frame = frame
		findings = append(findings, healthy)
	}
	return findings, state
}

func dnsRecursiveFindings(result dnsAliasResult, authorityState dnsAuthorityState) []finding {
	target := result.target.hostname
	frame := string(result.target.recordType)
	visibility := dnsFindingBase(result.target, tierWarn, "dns-recursive-unobservable", 2)
	visibility.symptom = fmt.Sprintf("The monitor could not observe the recursive %s behavior for %s.", frame, target)
	visibility.mechanism = "The platform recursive resolver timed out, failed, or returned a structurally invalid observation, so the client-view address-family contract is unknown."
	visibility.action = "Restore the monitor's recursive resolver path and repeat the same hostname/family query. Do not change authoritative DNS from a recursive observation failure alone."
	if result.recursiveErr != nil {
		visibility.observed = fmt.Sprintf(
			"record_type=%s expected_family_present=%t error_class=%s",
			frame, len(result.target.expected) > 0, classifyObservationError(result.recursiveErr),
		)
		return []finding{visibility}
	}

	response := result.recursive
	addresses, validAddresses := canonicalObservedAddresses(response.Addresses, result.target.recordType)
	if !validDNSResponseCode(response.ResponseCode) || !validAddresses {
		visibility.observed = fmt.Sprintf(
			"record_type=%s expected_family_present=%t error_class=observation-malformed",
			frame, len(result.target.expected) > 0,
		)
		return []finding{visibility}
	}
	findings := []finding{healthyFinding(
		"synthetic/dns-aliases", tierWarn, "dns-recursive-unobservable", target,
	)}
	findings[0].frame = frame

	expectedPresent := len(result.target.expected) > 0
	observedPresent := len(addresses) > 0
	missing, unexpected := dnsSetDelta(result.target.expected, addresses)
	healthyFamily := response.ResponseCode == DNSResponseSuccess && missing == 0 && unexpected == 0
	if authorityState != dnsAuthorityExact {
		// Authoritative state is the earlier causal boundary. Preserve any open
		// recursive ticket as unknown: neither a cached healthy answer nor a
		// downstream drift may resolve or duplicate it from this sample.
		return findings
	}
	if healthyFamily {
		healthy := healthyFinding("synthetic/dns-aliases", tierWarn, "dns-recursive-answer-family", target)
		healthy.frame = frame
		return append(findings, healthy)
	}

	drift := dnsFindingBase(result.target, tierWarn, "dns-recursive-answer-family", 2)
	drift.pageSustain = 3
	drift.symptom = fmt.Sprintf("Recursive DNS exposes the wrong %s RRset or family behavior for %s.", frame, target)
	drift.mechanism = "Every authority has the exact configured direct RRset, but the recursive client view returns a stale same-family address, the forbidden family, omits the required family, or returns NXDOMAIN instead of NOERROR/NODATA for an intentionally absent family. This isolates resolver/cache behavior from authoritative desired state."
	drift.observed = fmt.Sprintf(
		"record_type=%s expected_family_present=%t observed_family_present=%t expected_count=%d observed_count=%d missing_count=%d unexpected_count=%d response_code=%s",
		frame, expectedPresent, observedPresent, len(result.target.expected), len(addresses), missing, unexpected, response.ResponseCode,
	)
	drift.action = "Inspect recursive cache age, negative caching, and resolver policy without changing the already-correct authoritative RRset. Confirm the exact same-family set or NOERROR/NODATA absence through an independent recursive resolver before intervening."
	return append(findings, drift)
}

func dnsFindingBase(target dnsAliasTarget, tier, class string, sustain int) finding {
	return finding{
		probeId: "synthetic/dns-aliases", tier: tier, class: class,
		target: target.hostname, frame: string(target.recordType), sustain: sustain,
		baseline: fmt.Sprintf(
			"Every authoritative nameserver returns an authoritative NOERROR direct %s RRset exactly equal to the configured %d-record desired set; recursive DNS %s that family as configured.",
			target.recordType, len(target.expected), dnsFamilyExpectation(target.expected),
		),
		verify:   "Require three consecutive five-minute samples with exact configured RRset agreement from every authority and recursive resolver, including NOERROR/NODATA for each intentionally absent family.",
		playbook: "SIGNALS.md §18.3",
	}
}

func dnsFamilyExpectation(expected []string) string {
	if len(expected) == 0 {
		return "returns NOERROR with no answer for"
	}
	return "returns the exact configured set for"
}

func canonicalDomains(values []string) ([]string, bool) {
	canonical := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	valid := true
	for _, value := range values {
		domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		if !validDNSDomain(domain) {
			valid = false
			continue
		}
		if _, duplicate := seen[domain]; duplicate {
			valid = false
			continue
		}
		seen[domain] = struct{}{}
		canonical = append(canonical, domain)
	}
	sort.Strings(canonical)
	return canonical, valid
}

func validDNSDomain(domain string) bool {
	if domain == "" || len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if !validDNSLabel(label) {
			return false
		}
	}
	return true
}

func validDNSLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, character := range label {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}

func canonicalConfiguredAddresses(values []string, recordType DNSRecordType) ([]string, bool) {
	canonical, valid := canonicalObservedAddresses(values, recordType)
	if !valid || len(canonical) != len(values) {
		return canonical, false
	}
	return canonical, true
}

func canonicalObservedAddresses(values []string, recordType DNSRecordType) ([]string, bool) {
	canonical := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		address, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			return nil, false
		}
		if address.Zone() != "" {
			return nil, false
		}
		address = address.Unmap()
		if (recordType == DNSRecordA && !address.Is4()) || (recordType == DNSRecordAAAA && !address.Is6()) {
			return nil, false
		}
		key := address.String()
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		canonical = append(canonical, key)
	}
	sort.Strings(canonical)
	return canonical, true
}

func canonicalAddressStrings(values []string) []string {
	canonical := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		address, err := netip.ParseAddr(value)
		if err != nil {
			continue
		}
		key := address.Unmap().String()
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		canonical = append(canonical, key)
	}
	sort.Strings(canonical)
	return canonical
}

func dnsSetDelta(expected, observed []string) (missing, unexpected int) {
	expectedSet := make(map[string]struct{}, len(expected))
	observedSet := make(map[string]struct{}, len(observed))
	for _, value := range expected {
		expectedSet[value] = struct{}{}
	}
	for _, value := range observed {
		observedSet[value] = struct{}{}
	}
	for value := range expectedSet {
		if _, ok := observedSet[value]; !ok {
			missing++
		}
	}
	for value := range observedSet {
		if _, ok := expectedSet[value]; !ok {
			unexpected++
		}
	}
	return missing, unexpected
}

func validDNSResponseCode(code DNSResponseCode) bool {
	switch code {
	case DNSResponseSuccess, DNSResponseNameError, DNSResponseServerFailure, DNSResponseRefused, DNSResponseOther:
		return true
	default:
		return false
	}
}

func formatDNSResponseCodeCounts(counts map[DNSResponseCode]int) string {
	parts := []string{}
	for _, code := range []DNSResponseCode{
		DNSResponseSuccess,
		DNSResponseNameError,
		DNSResponseServerFailure,
		DNSResponseRefused,
		DNSResponseOther,
	} {
		if count := counts[code]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", code, count))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}
