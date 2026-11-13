// The strict window handover (connect MultiClientSettings.StrictWindowHandover,
// CPUOPT3 D2) for every hosted device of the process: on unless proxy.yml
// turns it off. Apps and c abi hosts leave it off.
package proxy

import (
	"errors"
	"strings"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

const defaultWindowStrictHandover = true

// The strict window handover switch from the proxy config:
//
//	window_strict_handover: true
//
// On by default: a hosted window at its hard max builds no candidate the hard
// max would refuse, and a warned client that the capacity collapse test would
// remove hands its slot to its evaluated replacement in one swap. false turns
// off both, so windows build candidates the hard max refuses and warned
// clients keep their slots, as before. The YAML 1.1 words on, off, yes and no
// count as booleans too: the config parser reads them as strings, and a kill
// switch written `off` must turn the handover off. Read once, when the
// process builds its device manager, so a change applies at each block's next
// start. A missing resource or key keeps the default; so does any other
// value, with one error line, because a typo must not change every block's
// windows at the next restart.
func proxyWindowStrictHandoverFromConfig() bool {
	const key = "window_strict_handover"
	resource, err := server.Config.SimpleResource("proxy.yml")
	if err != nil {
		// an absent resource is the default; an unreadable one is reported
		if !errors.Is(err, server.ErrResourceNotFound) || errors.Is(err, server.ErrResourceUnavailable) {
			glog.Errorf("[proxy]proxy.yml is unreadable; %s keeps %t\n", key, defaultWindowStrictHandover)
		}
		return defaultWindowStrictHandover
	}
	values, err := resource.ParseE()
	if err != nil {
		glog.Errorf("[proxy]proxy.yml does not parse; %s keeps %t\n", key, defaultWindowStrictHandover)
		return defaultWindowStrictHandover
	}
	value, ok := values[key]
	if !ok {
		return defaultWindowStrictHandover
	}
	enabled, ok := proxyConfigSwitch(value)
	if !ok {
		glog.Errorf("[proxy]proxy.yml: %s must be true or false (or on, off, yes, no), not %v (%T); keeping %t\n", key, value, value, defaultWindowStrictHandover)
		return defaultWindowStrictHandover
	}
	glog.Infof("[proxy]proxy.yml: %s = %t\n", key, enabled)
	return enabled
}

// Reads a switch from the proxy config: a boolean, or one of the YAML 1.1
// boolean words, which the parser leaves as strings.
func proxyConfigSwitch(value any) (enabled bool, ok bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "on", "yes":
			return true, true
		case "false", "off", "no":
			return false, true
		}
	}
	return false, false
}
