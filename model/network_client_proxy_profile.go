package model

// The initial performance profile of a hosted proxy device: refused on
// auth-client when connect would refuse it, and read back in auto from a
// config stored before that.

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urnetwork/server/session"
)

// A hosted proxy device applies the initial performance profile of its config
// at every creation (proxy/proxy_device.go NewProxyDevice). The profile comes
// from the client on /network/auth-client, and a window the multi client
// cannot install used to panic connect on the proxy host. The profile is
// client input, so an invalid one is counted, and its detail is logged only
// at V(1).
var proxyInvalidPerformanceProfiles = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "proxy",
	Name:      "invalid_performance_profiles_total",
	Help:      "Initial performance profiles of hosted proxy devices that connect refuses, by where they were caught: refused on auth-client, or a stored config read back in auto",
}, []string{"source"})

var (
	proxyInvalidPerformanceProfilesAuthClient   = proxyInvalidPerformanceProfiles.WithLabelValues("auth_client")
	proxyInvalidPerformanceProfilesStoredConfig = proxyInvalidPerformanceProfiles.WithLabelValues("stored_config")
)

// Registers the invalid profile counter with the default registry.
func init() {
	prometheus.MustRegister(proxyInvalidPerformanceProfiles)
}

// Refuses a proxy config whose initial performance profile connect refuses.
// auth-client checks it before the client is created, so a refused request
// creates nothing. The message names only the caller's own values.
func validateProxyConfigArgs(proxyConfig *ProxyConfig, session *session.ClientSession) (message string) {
	if proxyConfig == nil || proxyConfig.InitialDeviceState == nil {
		return
	}
	err := validatePerformanceProfile(proxyConfig.InitialDeviceState.PerformanceProfile)
	if err == nil {
		return
	}
	proxyInvalidPerformanceProfilesAuthClient.Inc()
	if glog.V(1) {
		glog.Infof("[proxy][%s]auth-client refused the initial performance profile: %s\n", session.ByJwt.NetworkId, err)
	}
	message = fmt.Sprintf("Invalid performance profile: %s", err)
	return
}

// Connect's own validation of the profile as the sdk device converts it for its
// multi client, so the server refuses exactly the profiles that connect
// refuses. nil, the auto default, is valid.
func validatePerformanceProfile(performanceProfile *sdk.PerformanceProfile) error {
	if performanceProfile == nil {
		return nil
	}
	return connectPerformanceProfile(performanceProfile).Validate()
}

// Converts the profile the way the sdk device does for its multi client (sdk
// toConnectPerformanceProfile): a window type other than quality or speed is
// auto, no window size is connect's default, and a min equal to the max fixes
// the window size.
func connectPerformanceProfile(performanceProfile *sdk.PerformanceProfile) *connect.PerformanceProfile {
	var windowType connect.WindowType
	switch performanceProfile.WindowType {
	case sdk.WindowTypeQuality:
		windowType = connect.WindowTypeQuality
	case sdk.WindowTypeSpeed:
		windowType = connect.WindowTypeSpeed
	default:
		windowType = connect.WindowTypeAuto
	}
	windowSize := connect.DefaultWindowSizeSettings()
	if w := performanceProfile.WindowSize; w != nil {
		fixedWindowSize := 0
		if w.WindowSizeMin == w.WindowSizeMax {
			fixedWindowSize = w.WindowSizeMin
		}
		windowSize = connect.WindowSizeSettings{
			WindowSizeMin:            w.WindowSizeMin,
			WindowSizeMinP2pOnly:     w.WindowSizeMinP2pOnly,
			WindowSizeMax:            w.WindowSizeMax,
			WindowSizeHardMax:        w.WindowSizeHardMax,
			FixedWindowSize:          fixedWindowSize,
			WindowSizeReconnectScale: w.WindowSizeReconnectScale,
			KeepHealthiestCount:      w.KeepHealthiestCount,
			Ulimit:                   w.Ulimit,
		}
	}
	return &connect.PerformanceProfile{
		WindowType:            windowType,
		WindowSize:            windowSize,
		AllowDirect:           performanceProfile.AllowDirect,
		PostQuantumEncryption: performanceProfile.PostQuantumEncryption,
	}
}

// Reads back the stored initial performance profile. Before auth-client refused
// them, a client could store a profile that connect refuses. Such a profile
// reads back in auto, keeping its direct and post-quantum choices, the way the
// sdk device reads back a saved one. The stored config is not rewritten.
func (self *ProxyDeviceConfig) normalizeStoredPerformanceProfile() {
	initialDeviceState := self.InitialDeviceState
	if initialDeviceState == nil {
		return
	}
	performanceProfile := initialDeviceState.PerformanceProfile
	err := validatePerformanceProfile(performanceProfile)
	if err == nil {
		return
	}
	proxyInvalidPerformanceProfilesStoredConfig.Inc()
	if glog.V(1) {
		glog.Infof("[proxy][%s]stored initial performance profile reads back in auto: %s\n", self.ProxyId, err)
	}
	initialDeviceState.PerformanceProfile = &sdk.PerformanceProfile{
		WindowType:            sdk.WindowTypeAuto,
		AllowDirect:           performanceProfile.AllowDirect,
		PostQuantumEncryption: performanceProfile.PostQuantumEncryption,
	}
}
