// Source attribution requires a completed family-specific 2xx observation.
// Public findings contain normalized addresses or fixed observation classes.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §8.8 maps to signal_source_attribution.go and signal_source_attribution_test.go.
// The probe is armed per address family by configuring that runner's known
// public source address in SignalSettings.SourceAttribution.
func NewSourceAttributionSignal() Signal {
	return &signalAdapter{number: "8.8", key: "source-attribution", name: "Dual-stack source attribution", probe: sourceAttributionProbe{}}
}

type sourceAttributionProbe struct{}

// The local curl producer appends one final status record after the body.
const sourceAttributionHttpStatusMarker = "\nmonitor-source-attribution-http-status="

func (sourceAttributionProbe) id() string             { return "synthetic/source-attribution" }
func (sourceAttributionProbe) tier() string           { return tierWarn }
func (sourceAttributionProbe) cadence() time.Duration { return time.Minute }

// Each armed family owns one request; failed or malformed acquisition cannot
// establish healthy attribution or a forwarding fault for that family.
func (sourceAttributionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	tests := []struct {
		family   string
		curlFlag string
		url      string
		expected string
	}{
		{family: "ipv4", curlFlag: "--ipv4", url: env.cfg.sourceIPv4URL, expected: env.cfg.expectedSourceIPv4},
		{family: "ipv6", curlFlag: "--ipv6", url: env.cfg.sourceIPv6URL, expected: env.cfg.expectedSourceIPv6},
	}

	findings := []finding{}
	for _, test := range tests {
		if strings.TrimSpace(test.expected) == "" {
			continue
		}
		expected, err := netip.ParseAddr(strings.TrimSpace(test.expected))
		if err != nil || expected.Zone() != "" || expected.Is4() != (test.family == "ipv4") {
			return findings, fmt.Errorf("source attribution %s expected address is invalid or has the wrong family", test.family)
		}
		out, requestErr := env.runner.local(ctx, "curl",
			"--silent", "--show-error", "--fail-with-body",
			"--max-time", strconv.Itoa(max(1, int(env.cfg.commandTimeout.Seconds()))),
			"--write-out", sourceAttributionHttpStatusMarker+"%{http_code}\n",
			test.curlFlag, test.url,
		)
		actual := netip.Addr{}
		observationClass := "request-failed"
		if requestErr == nil {
			actual, observationClass = sourceAddressFromResponse(out)
		}

		target := strings.TrimSpace(test.url)
		if target == "" {
			target = test.family
		}
		if observationClass != "complete" {
			findings = append(findings, finding{
				probeId: "synthetic/source-attribution", tier: tierWarn,
				class: "cannot-observe", target: target, frame: test.family, sustain: 2,
				symptom:   fmt.Sprintf("%s source attribution could not be observed for %s", strings.ToUpper(test.family), target),
				mechanism: "The selected family did not supply a complete, successful 2xx response with a usable address. Source attribution is unknown; this observation does not establish endpoint reachability or a forwarding fault.",
				baseline:  fmt.Sprintf("HTTP 2xx with info.ip=%s over %s, matching both the connection family and the runner's known public source", expected, test.family),
				observed:  fmt.Sprintf("family=%s expected=%s observation=%s", test.family, expected, observationClass),
				evidence:  "Only the fixed observation class is retained; response payloads and command diagnostics are omitted.",
				action:    "Restore the family-specific request and response contract, confirm the runner's current public source, and repeat the same bounded request before attributing a forwarding fault.",
				verify:    fmt.Sprintf("A fresh completed %s request returns HTTP 2xx and a usable address; compare it with the runner's known public source %s.", test.family, expected),
				playbook:  "SIGNALS.md §8.8",
			})
			continue
		}

		problems := []string{}
		if actual.Is4() != (test.family == "ipv4") {
			problems = append(problems, fmt.Sprintf("info.ip %s has the wrong address family for the %s request", actual, test.family))
		}
		if actual != expected {
			problems = append(problems, fmt.Sprintf("info.ip %s does not equal this runner's known source %s", actual, expected))
		}

		if len(problems) == 0 {
			healthy := healthyFinding("synthetic/source-attribution", tierWarn, "source-attribution", target)
			healthy.frame = test.family
			findings = append(findings, healthy)
			continue
		}
		findings = append(findings, finding{
			probeId: "synthetic/source-attribution", tier: tierWarn,
			class: "source-attribution", target: target, frame: test.family, sustain: 2,
			symptom:   fmt.Sprintf("%s source-attribution proof failed for %s", strings.ToUpper(test.family), target),
			mechanism: "A completed HTTP 2xx observation returned an address outside this runner's expected family/source contract. Forwarding-header handling is a candidate cause; this observation alone does not identify a faulty hop or prove fleet-wide client collapse.",
			baseline:  fmt.Sprintf("HTTP 2xx with info.ip=%s over %s, matching both the connection family and the runner's known public source", expected, test.family),
			observed:  fmt.Sprintf("family=%s expected=%s returned=%s observation=complete", test.family, expected, actual),
			evidence:  strings.Join(problems, "\n"),
			context:   "A stale configured runner address or changed egress can also produce a mismatch. This request covers only the sampled family and endpoint, not every serving generation.",
			action:    "Confirm the runner's current public source, then inspect the active Warp/API forwarding-header contract and backend isolation for every serving generation; keep the family-specific public tuple pinned while collecting logs.",
			verify:    fmt.Sprintf("Repeat the %s request from this runner and require HTTP 2xx with info.ip exactly %s, then require no new malformed or legacy untrusted-peer resolver lines for five minutes.", test.family, expected),
			playbook:  "SIGNALS.md §8.8",
		})
	}
	return findings, nil
}

// A unique terminal status record must prove 2xx before the payload is used.
// Invalid payloads, framing, and scoped addresses return no source text.
func sourceAddressFromResponse(out string) (netip.Addr, string) {
	parts := strings.Split(out, sourceAttributionHttpStatusMarker)
	if len(parts) != 2 || len(parts[1]) != 4 || parts[1][3] != '\n' {
		return netip.Addr{}, "invalid-response"
	}
	status, err := strconv.Atoi(parts[1][:3])
	if err != nil || status < 100 || 599 < status {
		return netip.Addr{}, "invalid-response"
	}
	if status < 200 || 300 <= status {
		return netip.Addr{}, "http-non-2xx"
	}
	var response struct {
		IP   string `json:"ip"`
		Info struct {
			IP string `json:"ip"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(parts[0])), &response); err != nil {
		return netip.Addr{}, "invalid-response"
	}
	actualText := response.IP
	if response.Info.IP != "" {
		actualText = response.Info.IP
	}
	actual, err := netip.ParseAddr(strings.TrimSpace(actualText))
	if err != nil || actual.Zone() != "" {
		return netip.Addr{}, "invalid-response"
	}
	return actual, "complete"
}
