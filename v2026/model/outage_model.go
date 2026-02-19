package model

// this signals if the network is operating in normal stable state (true),
// or a degraded state (false). There can be many reasons for a degraded state not captured here.
func NormalNetworkConditions() bool {
	// FIXME set to false if any service reported outage in the last N minutes
	return true
}
