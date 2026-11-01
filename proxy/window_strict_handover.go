// The kill switch of the strict window handover (connect
// MultiClientSettings.StrictWindowHandover, CPUOPT3 D2) for every hosted
// device of the process.
package proxy

import (
	"errors"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
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
// clients keep their slots, as before. Read once, when the process builds its
// device manager, so a change applies at each block's next start. A missing
// resource or key keeps the default; so does a value that is not a boolean,
// with one error line, because a typo must not change every block's windows
// at the next restart.
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
	enabled, ok := value.(bool)
	if !ok {
		glog.Errorf("[proxy]proxy.yml: %s must be true or false, not %v (%T); keeping %t\n", key, value, value, defaultWindowStrictHandover)
		return defaultWindowStrictHandover
	}
	glog.Infof("[proxy]proxy.yml: %s = %t\n", key, enabled)
	return enabled
}
