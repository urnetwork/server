// The monitor host's own path to the inventory's overlay addresses, judged
// against the operator's observer contract. A route that leaves through an
// interface or gateway outside the contract, or no route at all, makes every
// overlay observation of those hosts a local observation-path candidate
// rather than a production fault. The lookups are monitor-local: no host is
// contacted.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const observerRouteProbeId = "monitor/observer-route"

// The operator-owned observer contract (vault/<env>/monitor.yml
// observer_overlay): the interfaces and next-hop gateways a route from the
// monitor host to an inventory overlay address may use. Either list
// qualifies a route; both empty leaves the contract missing. macOS tunnel
// interface names change across reconnects, so a gateway is the stable
// contract there.
type ObserverOverlaySettings struct {
	Interfaces []string `yaml:"interfaces"`
	Gateways   []string `yaml:"gateways"`
}

func cloneObserverOverlaySettings(settings ObserverOverlaySettings) ObserverOverlaySettings {
	return ObserverOverlaySettings{
		Interfaces: append([]string(nil), settings.Interfaces...),
		Gateways:   append([]string(nil), settings.Gateways...),
	}
}

// SIGNALS.md §1.7 (observer-route): observer-overlay route attribution.
func NewObserverRouteSignal() Signal {
	return NewObserverRouteSignalWithSettings(DefaultObserverRouteSettings())
}

// The signal with explicit tunables and route adapter.
func NewObserverRouteSignalWithSettings(settings *ObserverRouteSettings) Signal {
	return &signalAdapter{
		number: "1.7", key: "observer-route", name: "Observer overlay route conformance",
		probe: observerRouteProbe{settings: settings},
	}
}

// The tunables of SIGNALS.md §1.7.
type ObserverRouteSettings struct {
	// Normal sampling cadence and consecutive observations before an alert.
	Cadence time.Duration
	Sustain int
	// The local route adapter: "darwin" or "linux". Other values are unknown.
	Platform string
	// The largest route lookup output read.
	MaxRouteOutputBytes int
}

// Five-minute local lookups; two observations ride out a VPN reconnect.
func DefaultObserverRouteSettings() *ObserverRouteSettings {
	return &ObserverRouteSettings{
		Cadence:             5 * time.Minute,
		Sustain:             2,
		Platform:            runtime.GOOS,
		MaxRouteOutputBytes: 16 * 1024,
	}
}

// Stateless: every run looks up every route afresh.
type observerRouteProbe struct {
	settings *ObserverRouteSettings
}

func (self observerRouteProbe) id() string             { return observerRouteProbeId }
func (self observerRouteProbe) tier() string           { return tierWarn }
func (self observerRouteProbe) cadence() time.Duration { return self.settings.Cadence }

// One route lookup's outcome.
type observerRouteState string

const (
	observerRouteConforming observerRouteState = "conforming"
	observerRouteDiverted   observerRouteState = "diverted"
	observerRouteAbsent     observerRouteState = "absent"
	observerRouteUnknown    observerRouteState = "unknown"
)

// The route a lookup selected: interface and optional gateway.
type observerRoute struct {
	state         observerRouteState
	interfaceName string
	gateway       netip.Addr
}

var observerRouteInterfacePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,14}$`)

// Look up the route to one address with the platform's native command.
// Absence is a fixed diagnostic; anything unparsed is unknown.
func (self observerRouteProbe) lookup(ctx context.Context, runner probeRunner, address netip.Addr) observerRoute {
	unknown := observerRoute{state: observerRouteUnknown}
	switch self.settings.Platform {
	case "darwin":
		family := "-inet"
		if address.Is6() {
			family = "-inet6"
		}
		output, err := runner.local(ctx, "/sbin/route", "-n", "get", family, address.String())
		if ctx.Err() != nil || len(output) > self.settings.MaxRouteOutputBytes {
			return unknown
		}
		// Darwin can write its not-in-table diagnostic with a zero exit.
		if ipv6RouteAbsentOutput(output) {
			return observerRoute{state: observerRouteAbsent}
		}
		if err != nil {
			return unknown
		}
		route := observerRoute{}
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) != 2 {
				continue
			}
			switch fields[0] {
			case "interface:":
				if route.interfaceName != "" || !observerRouteInterfacePattern.MatchString(fields[1]) {
					return unknown
				}
				route.interfaceName = fields[1]
			case "gateway:":
				if gateway, err := netip.ParseAddr(fields[1]); err == nil {
					route.gateway = gateway.Unmap()
				}
			}
		}
		if route.interfaceName == "" {
			return unknown
		}
		return route
	case "linux":
		args := []string{"-j", "route", "get", address.String()}
		if address.Is6() {
			args = []string{"-j", "-6", "route", "get", address.String()}
		}
		output, err := runner.local(ctx, "ip", args...)
		if ctx.Err() != nil || len(output) > self.settings.MaxRouteOutputBytes {
			return unknown
		}
		if err != nil {
			var process *exec.ExitError
			if errors.As(err, &process) && process.ExitCode() == 2 {
				switch strings.TrimSpace(output) {
				case "RTNETLINK answers: Network is unreachable", "RTNETLINK answers: No route to host":
					return observerRoute{state: observerRouteAbsent}
				}
			}
			return unknown
		}
		return observerRouteLinux(output, address)
	}
	return unknown
}

// Exactly one route object with a unique key set, for exactly this address.
func observerRouteLinux(output string, address netip.Addr) observerRoute {
	unknown := observerRoute{state: observerRouteUnknown}
	decoder := json.NewDecoder(strings.NewReader(output))
	if open, err := decoder.Token(); err != nil || open != json.Delim('[') {
		return unknown
	}
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return unknown
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		name, named := token.(string)
		if err != nil || !named {
			return unknown
		}
		if _, duplicate := fields[name]; duplicate {
			return unknown
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return unknown
		}
		fields[name] = value
	}
	var trailing json.RawMessage
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return unknown
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') || decoder.Decode(&trailing) != io.EOF {
		return unknown
	}
	var destination, device, gatewayText string
	if json.Unmarshal(fields["dst"], &destination) != nil || json.Unmarshal(fields["dev"], &device) != nil ||
		!observerRouteInterfacePattern.MatchString(device) {
		return unknown
	}
	if parsed, err := netip.ParseAddr(destination); err != nil || parsed.Unmap() != address.Unmap() {
		return unknown
	}
	route := observerRoute{interfaceName: device}
	if raw, present := fields["gateway"]; present {
		if json.Unmarshal(raw, &gatewayText) != nil {
			return unknown
		}
		gateway, err := netip.ParseAddr(gatewayText)
		if err != nil {
			return unknown
		}
		route.gateway = gateway.Unmap()
	}
	return route
}

// Every enabled overlay destination is classified against the contract. An
// excluded host is neither looked up nor certified.
func (self observerRouteProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env.cfg.addressMode != addressModeOverlay {
		// LAN mode dials LAN addresses; the overlay contract does not apply.
		return nil, nil
	}
	settings := self.settings
	target := "monitor-host/overlay"
	contract := env.cfg.observerOverlay
	if len(contract.Interfaces) == 0 && len(contract.Gateways) == 0 {
		return []finding{self.contractMissing(target)}, nil
	}
	interfaces, gateways := map[string]bool{}, map[netip.Addr]bool{}
	for _, name := range contract.Interfaces {
		if !observerRouteInterfacePattern.MatchString(name) {
			return []finding{self.unknown(target, "invalid-contract-interface", nil)}, nil
		}
		interfaces[name] = true
	}
	for _, text := range contract.Gateways {
		gateway, err := netip.ParseAddr(strings.TrimSpace(text))
		if err != nil {
			return []finding{self.unknown(target, "invalid-contract-gateway", nil)}, nil
		}
		gateways[gateway.Unmap()] = true
	}
	scoped, isScoped := env.runner.(*hostScopeRunner)
	hostStates := map[observerRouteState][]string{}
	interfaceCounts := map[string]int{}
	looked := 0
	for _, configured := range env.cfg.hosts {
		if isScoped {
			if err := scoped.guardHost(ctx, configured); err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return nil, contextErr
				}
				continue
			}
		}
		address, err := netip.ParseAddr(strings.TrimSpace(configured.overlayIp))
		if err != nil {
			hostStates[observerRouteUnknown] = append(hostStates[observerRouteUnknown], configured.name)
			continue
		}
		looked++
		route := self.lookup(ctx, env.runner, address.Unmap())
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if route.state == "" {
			route.state = observerRouteDiverted
			if interfaces[route.interfaceName] || (route.gateway.IsValid() && gateways[route.gateway]) {
				route.state = observerRouteConforming
			}
		}
		if route.state == observerRouteDiverted {
			interfaceCounts[route.interfaceName]++
		}
		hostStates[route.state] = append(hostStates[route.state], configured.name)
	}
	for _, names := range hostStates {
		sort.Strings(names)
	}
	counts := fmt.Sprintf("targets=%d looked_up=%d conforming=%d diverted=%d absent=%d unknown=%d",
		len(hostStates[observerRouteConforming])+len(hostStates[observerRouteDiverted])+len(hostStates[observerRouteAbsent])+len(hostStates[observerRouteUnknown]),
		looked, len(hostStates[observerRouteConforming]), len(hostStates[observerRouteDiverted]), len(hostStates[observerRouteAbsent]), len(hostStates[observerRouteUnknown]))
	findings := []finding{}
	if broken := append(append([]string{}, hostStates[observerRouteDiverted]...), hostStates[observerRouteAbsent]...); len(broken) != 0 {
		sort.Strings(broken)
		exits := []string{}
		for name, count := range interfaceCounts {
			exits = append(exits, fmt.Sprintf("%s:%d", name, count))
		}
		sort.Strings(exits)
		findings = append(findings, finding{
			probeId: observerRouteProbeId, tier: tierWarn, class: "observer-route-diverted", target: target, sustain: settings.Sustain,
			symptom:   fmt.Sprintf("The monitor host's route to %d inventory overlay address(es) is outside its observer contract", len(broken)),
			mechanism: "Every overlay observation of these hosts leaves the monitor host through an interface or gateway the operator's observer_overlay contract does not name, or has no route at all. A broken local tunnel sends them to the LAN or default route; until it is restored, their SSH failures and stale tails are observation-path loss, not production faults.",
			baseline:  "Every enabled inventory overlay address routes through a contract interface or gateway.",
			observed:  counts + " diverted_exit_interfaces=[" + strings.Join(exits, ",") + "]",
			evidence:  "Monitor-local route lookups only (route get / ip route get); no host was contacted. Affected inventory hosts: diverted=[" + strings.Join(hostStates[observerRouteDiverted], ",") + "] absent=[" + strings.Join(hostStates[observerRouteAbsent], ",") + "]. Addresses and gateways stay private.",
			context:   "Correlate cannot-observe and stale-tail findings for the listed hosts from the same minutes: they are local observation-path candidates. A route sample cannot establish when the path broke or that every earlier failure shared it, and a conforming route does not prove the tunnel delivers packets.",
			action:    "Restore the intended observer route or VPN session on the monitor host under the workstation operator's authority, then rerun the affected signals. Do not restart production services, deploy, or mutate a VPN merely to improve this observation; never contact disabled hosts.",
			verify:    "Two consecutive observations with every overlay address on a contract route, and every affected target supplying a concrete healthy or broken observation again.",
			playbook:  "SIGNALS.md §1.7",
		})
	} else {
		findings = append(findings, healthyFinding(observerRouteProbeId, tierWarn, "observer-route-diverted", target))
	}
	if unknownHosts := hostStates[observerRouteUnknown]; len(unknownHosts) != 0 {
		findings = append(findings, self.unknown(target, counts+" unknown_hosts=["+strings.Join(unknownHosts, ",")+"]", nil))
	} else {
		findings = append(findings, healthyFinding(observerRouteProbeId, tierWarn, "observer-route-unobservable", target))
	}
	return findings, nil
}

// The operator has not declared the intended observer path. This is a
// standing state of the watcher's configuration, kept in the alert file but
// never posted as an incident.
func (self observerRouteProbe) contractMissing(target string) finding {
	return finding{
		probeId: observerRouteProbeId, tier: tierWarn, class: "observer-route-contract-missing", target: target, sustain: 1, configuredOff: true,
		symptom:   "The monitor host's observer overlay route contract is not configured, so overlay route attribution is unknown",
		mechanism: "Inventory supplies destination addresses but not the interface or gateway the observer's overlay path must use, so a broken local tunnel cannot be told apart from a deliberately LAN-routed overlay.",
		baseline:  "vault/<env>/monitor.yml declares observer_overlay with the intended interfaces or gateways.",
		observed:  "contract=missing",
		evidence:  "No route lookup ran.",
		context:   "Unknown is not healthy: cannot-observe findings across many hosts stay unattributed between a local path loss and production faults.",
		action:    "Have the monitor/workstation operator declare observer_overlay.interfaces (Linux) or observer_overlay.gateways (macOS, whose tunnel interface names change) in monitor.yml.",
		verify:    "The next observation classifies every overlay address against the declared contract.",
		playbook:  "SIGNALS.md §1.7",
	}
}

// Lookups that could not be classified, or an invalid contract.
func (self observerRouteProbe) unknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: observerRouteProbeId, tier: tierWarn, class: "observer-route-unobservable", target: target, sustain: self.settings.Sustain,
		symptom:   "The monitor host's route to some inventory overlay addresses is unknown",
		mechanism: "The contract is invalid, a host has no parseable overlay address, the platform has no route adapter, or the native route lookup failed or returned an unparsed answer.",
		baseline:  "A valid contract and one parseable native route answer per enabled overlay address.",
		observed:  observed,
		evidence:  "Only fixed reasons, counts and inventory host names are retained; addresses, gateways and raw route output stay private.",
		context:   "Unknown is not healthy; these hosts' overlay observation path is unattributed.",
		action:    observationFailureAction(err, classifyObservationError(err), "Fix the observer_overlay contract or the host's overlay_ip in monitor.yml, or run the watcher on a platform with a route adapter."),
		verify:    "Every enabled overlay address is classified on the next observation.",
		playbook:  "SIGNALS.md §1.7",
	}
}
