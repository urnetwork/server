// Hosted windows race fewer candidates than the connect default. The multiple
// is merged onto whatever reliability settings each window has in effect, so a
// routing tier or a remote override keeps every field it set.
package proxy

import (
	"github.com/urnetwork/sdk"
)

// A hosted window expands by requesting and evaluating this multiple of the
// candidates it needs. The connect default is 2: every expansion mints twice
// the identities it admits and cancels the rest. On 2026-10-22 hosted windows
// minted about 310 identities a second and retired most before they connected,
// so a hosted window evaluates only the candidates it needs.
const defaultHostedEvaluationPoolMultiple = 1

// The part of sdk.DeviceLocal the hosted multiple reads and writes.
type hostedReliabilityDevice interface {
	GetReliabilitySettings() *sdk.ReliabilitySettings
	SetReliabilitySettings(reliabilitySettings *sdk.ReliabilitySettings)
}

// Sets the evaluation pool multiple on the device's effective reliability
// settings and changes nothing else. The device stores the result as its
// override and re-applies it to every window it builds. Without a window there
// is nothing in effect to merge onto, and a nonpositive multiple keeps the
// connect default.
func applyHostedEvaluationPoolMultiple(device hostedReliabilityDevice, multiple int) {
	if multiple <= 0 {
		return
	}
	effective := device.GetReliabilitySettings()
	if effective == nil || effective.EvaluationPoolMultiple == int32(multiple) {
		return
	}
	merged := *effective
	merged.EvaluationPoolMultiple = int32(multiple)
	device.SetReliabilitySettings(&merged)
}

// Applies the hosted multiple to each window as the device enables it: the
// first window, and any window built after a remote reset of the override.
type hostedEvaluationPoolListener struct {
	device   hostedReliabilityDevice
	multiple int
}

func (self *hostedEvaluationPoolListener) ConnectChanged(connectEnabled bool) {
	if connectEnabled {
		applyHostedEvaluationPoolMultiple(self.device, self.multiple)
	}
}
