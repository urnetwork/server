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

func (sourceAttributionProbe) id() string             { return "synthetic/source-attribution" }
func (sourceAttributionProbe) tier() string           { return tierWarn }
func (sourceAttributionProbe) cadence() time.Duration { return time.Minute }

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
		if err != nil {
			return findings, fmt.Errorf("source attribution %s expected address %q: %w", test.family, test.expected, err)
		}
		out, requestErr := env.runner.local(ctx, "curl",
			"--silent", "--show-error", "--fail-with-body",
			"--max-time", strconv.Itoa(max(1, int(env.cfg.commandTimeout.Seconds()))),
			test.curlFlag, test.url,
		)
		actualText, parseErr := sourceAddressFromResponse(out)
		actual, addressErr := netip.ParseAddr(actualText)

		problems := []string{}
		if requestErr != nil {
			problems = append(problems, "the family-specific endpoint did not return HTTP 2xx: "+requestErr.Error())
		}
		if parseErr != nil {
			problems = append(problems, "the response did not contain a usable info.ip: "+parseErr.Error())
		} else if addressErr != nil {
			problems = append(problems, fmt.Sprintf("info.ip %q is not an IP address", actualText))
		} else {
			wantV4 := test.family == "ipv4"
			if actual.Is4() != wantV4 {
				problems = append(problems, fmt.Sprintf("info.ip %s has the wrong address family for the %s request", actual, test.family))
			}
			if actual != expected {
				problems = append(problems, fmt.Sprintf("info.ip %s does not equal this runner's known source %s", actual, expected))
			}
		}

		target := strings.TrimSpace(test.url)
		if target == "" {
			target = test.family
		}
		if len(problems) == 0 {
			findings = append(findings, healthyFinding("synthetic/source-attribution", tierWarn, "source-attribution", target))
			continue
		}
		observedAddress := actualText
		if observedAddress == "" {
			observedAddress = "<missing>"
		}
		findings = append(findings, finding{
			probeId: "synthetic/source-attribution", tier: tierWarn,
			class: "source-attribution", target: target, frame: test.family, sustain: 2,
			symptom:   fmt.Sprintf("%s source-attribution proof failed for %s", strings.ToUpper(test.family), target),
			mechanism: "The API endpoint is reachable through the selected family but is not preserving the monitor runner's client address; clients may be collapsed onto an ingress identity for rate limiting, location, and abuse controls.",
			baseline:  fmt.Sprintf("HTTP 2xx with info.ip=%s over %s, matching both the connection family and the runner's known public source", expected, test.family),
			observed:  fmt.Sprintf("family=%s expected=%s returned=%s request_error=%v", test.family, expected, observedAddress, requestErr),
			evidence:  strings.Join(problems, "\n"),
			action:    "Inspect the active Warp/API forwarding-header contract and backend isolation for every serving generation; keep the family-specific public tuple pinned while collecting logs.",
			verify:    fmt.Sprintf("Repeat the %s request from this runner and require HTTP 2xx with info.ip exactly %s, then require no new malformed or legacy untrusted-peer resolver lines for five minutes.", test.family, expected),
			playbook:  "SIGNALS.md §8.8",
		})
	}
	return findings, nil
}

func sourceAddressFromResponse(out string) (string, error) {
	var response struct {
		IP   string `json:"ip"`
		Info struct {
			IP string `json:"ip"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &response); err != nil {
		return "", err
	}
	if response.Info.IP != "" {
		return strings.TrimSpace(response.Info.IP), nil
	}
	if response.IP != "" {
		return strings.TrimSpace(response.IP), nil
	}
	return "", fmt.Errorf("missing info.ip")
}
