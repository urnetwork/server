// Hosted windows evaluate only the candidates they need, and the multiple is
// the only reliability field the proxy changes.
package proxy

import (
	"reflect"
	"testing"

	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
)

// Records the device calls the hosted multiple makes and stores the override
// the way sdk.DeviceLocal does.
type hostedReliabilityDeviceTest struct {
	effective *sdk.ReliabilitySettings
	gets      int
	sets      int
}

func (self *hostedReliabilityDeviceTest) GetReliabilitySettings() *sdk.ReliabilitySettings {
	self.gets++
	if self.effective == nil {
		return nil
	}
	effective := *self.effective
	return &effective
}

func (self *hostedReliabilityDeviceTest) SetReliabilitySettings(reliabilitySettings *sdk.ReliabilitySettings) {
	self.sets++
	effective := *reliabilitySettings
	self.effective = &effective
}

// Distinct nonzero values in every field, so a merge that drops or resets any
// field other than the multiple is visible.
func distinctReliabilitySettingsTest(t *testing.T) *sdk.ReliabilitySettings {
	t.Helper()
	settings := &sdk.ReliabilitySettings{}
	value := reflect.ValueOf(settings).Elem()
	for index := range value.NumField() {
		field := value.Field(index)
		switch field.Kind() {
		case reflect.Bool:
			field.SetBool(true)
		case reflect.Int32, reflect.Int64:
			field.SetInt(int64(index + 3))
		case reflect.Float64:
			field.SetFloat(float64(index) + 0.5)
		default:
			t.Fatalf("reliability field %s has an unhandled kind %s", value.Type().Field(index).Name, field.Kind())
		}
	}
	return settings
}

// The merge sets the multiple and keeps every other effective field, including
// the routing-tier and remote-override fields.
func TestHostedEvaluationPoolMultipleChangesOnlyItsField(t *testing.T) {
	before := distinctReliabilitySettingsTest(t)
	device := &hostedReliabilityDeviceTest{effective: before}
	applyHostedEvaluationPoolMultiple(device, 1)
	if device.sets != 1 || device.effective.EvaluationPoolMultiple != 1 {
		t.Fatalf("sets=%d multiple=%d, want one override with multiple 1", device.sets, device.effective.EvaluationPoolMultiple)
	}
	after := *device.effective
	after.EvaluationPoolMultiple = before.EvaluationPoolMultiple
	if !reflect.DeepEqual(after, *before) {
		t.Fatalf("the merge changed another reliability field:\n before %+v\n after  %+v", *before, after)
	}
}

// A device with no window has nothing in effect to merge onto; an equal
// multiple or a nonpositive setting writes nothing.
func TestHostedEvaluationPoolMultipleWritesOnlyAChange(t *testing.T) {
	waiting := &hostedReliabilityDeviceTest{}
	applyHostedEvaluationPoolMultiple(waiting, 1)
	if waiting.sets != 0 {
		t.Fatal("a device without a window received an override")
	}
	equal := &hostedReliabilityDeviceTest{effective: &sdk.ReliabilitySettings{EvaluationPoolMultiple: 1}}
	applyHostedEvaluationPoolMultiple(equal, 1)
	if equal.sets != 0 {
		t.Fatal("an equal multiple rewrote the override")
	}
	for _, multiple := range []int{0, -1} {
		disabled := &hostedReliabilityDeviceTest{effective: &sdk.ReliabilitySettings{EvaluationPoolMultiple: 2}}
		applyHostedEvaluationPoolMultiple(disabled, multiple)
		if disabled.gets != 0 || disabled.sets != 0 {
			t.Fatalf("multiple %d touched the device", multiple)
		}
	}
}

// Each enabled window gets the multiple; a disabled connect does nothing.
func TestHostedEvaluationPoolListenerAppliesToEnabledWindows(t *testing.T) {
	device := &hostedReliabilityDeviceTest{effective: &sdk.ReliabilitySettings{EvaluationPoolMultiple: 2}}
	listener := &hostedEvaluationPoolListener{device: device, multiple: 1}
	listener.ConnectChanged(false)
	if device.gets != 0 || device.sets != 0 {
		t.Fatal("a disabled connect touched the device")
	}
	listener.ConnectChanged(true)
	if device.sets != 1 || device.effective.EvaluationPoolMultiple != 1 {
		t.Fatal("an enabled window kept the connect multiple")
	}
	// a remote reset of the override puts the next window back on the default
	device.effective.EvaluationPoolMultiple = 2
	listener.ConnectChanged(true)
	if device.sets != 2 || device.effective.EvaluationPoolMultiple != 1 {
		t.Fatal("a later window kept the connect multiple")
	}
}

// Every hosted device opened with the defaults evaluates one candidate per need.
func TestHostedEvaluationPoolMultipleDefaults(t *testing.T) {
	if settings := DefaultProxyDeviceManagerSettings(); settings.HostedEvaluationPoolMultiple != 1 {
		t.Fatalf("manager default = %d, want 1", settings.HostedEvaluationPoolMultiple)
	}
	if settings := DefaultProxyDeviceSettings(); settings.HostedEvaluationPoolMultiple != 1 {
		t.Fatalf("device default = %d, want 1", settings.HostedEvaluationPoolMultiple)
	}
}

// A real hosted device's window runs the hosted multiple with every other
// reliability field equal to a device that keeps the connect default. The
// multiple survives a routing-tier change and a rebuilt window.
func TestProxyDeviceHostedWindowsUseTheHostedEvaluationPoolMultiple(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		h := setupProxyTestWithOptions(t, defaultProxyTestOptions())
		defer h.close(t)
		effective := func(multiple int) (*ProxyDevice, sdk.ReliabilitySettings) {
			settings := DefaultProxyDeviceManagerSettings()
			settings.NetworkSpace = h.networkSpace
			settings.HostedEvaluationPoolMultiple = multiple
			manager := NewProxyDeviceManager(h.ctx, settings)
			t.Cleanup(func() { closeProxyDeviceManager(t, manager) })
			pd, err := manager.OpenProxyDevice(h.proxyId)
			if err != nil {
				t.Fatal(err)
			}
			reliabilitySettings := pd.deviceLocal.GetReliabilitySettings()
			if reliabilitySettings == nil {
				t.Fatal("the hosted device built no window from its initial location")
			}
			return pd, *reliabilitySettings
		}
		_, connectDefault := effective(0)
		pd, hosted := effective(1)
		if hosted.EvaluationPoolMultiple != 1 || connectDefault.EvaluationPoolMultiple <= 1 {
			t.Fatalf("hosted multiple = %d, connect default = %d", hosted.EvaluationPoolMultiple, connectDefault.EvaluationPoolMultiple)
		}
		compared := hosted
		compared.EvaluationPoolMultiple = connectDefault.EvaluationPoolMultiple
		if !reflect.DeepEqual(compared, connectDefault) {
			t.Fatalf("the hosted override changed another reliability field:\n default %+v\n hosted  %+v", connectDefault, hosted)
		}

		pd.deviceLocal.SetRoutingTier(sdk.RoutingTierLight)
		tiered := pd.deviceLocal.GetReliabilitySettings()
		if tiered == nil || !tiered.ScoredPlacement || tiered.EvaluationPoolMultiple != 1 {
			t.Fatalf("after a routing tier change: %+v", tiered)
		}

		pd.deviceLocal.Reconnect(pd.deviceLocal.GetConnectLocation())
		rebuilt := pd.deviceLocal.GetReliabilitySettings()
		if rebuilt == nil || rebuilt.EvaluationPoolMultiple != 1 || !rebuilt.ScoredPlacement {
			t.Fatalf("after a rebuilt window: %+v", rebuilt)
		}
	})
}
