// Enrollment controls exercise the actual monitor YAML projection and public
// signal. Configuration values stay private even when admission fails.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The production monitor loader uses this exact raw-node projection; unknown
// or malformed nested input cannot quietly become an empty healthy inventory.
func TestSubnetArtifactsStrictMonitorYamlProjection(t *testing.T) {
	settings, _, _, _ := subnetArtifactFixture(t)
	raw, err := yaml.Marshal(map[string]any{"subnet_artifacts": settings.SubnetArtifacts})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := decodeMonitorYaml(raw)
	if err != nil {
		t.Fatal(err)
	}
	loaded := subnetArtifactSettingsFromNode(parsed.SubnetArtifacts)
	if loaded.validate(settings.Now()) != "" || len(loaded.Objects) != 1 || loaded.Objects[0] != settings.SubnetArtifacts.Objects[0] {
		t.Fatal("real YAML projection lost approved values")
	}
	for _, raw := range []string{
		"subnet_artifacts: null\n",
		"subnet_artifacts: private-value\n",
		"subnet_artifacts: {schema: wrong, extra_private_key: private-value}\n",
		"subnet_artifacts: &private_anchor {schema: wrong}\n",
		"subnet_artifacts: {schema: urnetwork-monitor-subnet-artifacts-v1, objects: []}\n",
	} {
		parsed, err := decodeMonitorYaml([]byte(raw))
		if err != nil {
			continue
		}
		loaded := subnetArtifactSettingsFromNode(parsed.SubnetArtifacts)
		if loaded == nil || loaded.validate(settings.Now()) == "" {
			t.Fatal("present invalid YAML disappeared or passed")
		}
	}
	parsed, err = decodeMonitorYaml([]byte("address_mode: overlay\n"))
	if err != nil || subnetArtifactSettingsFromNode(parsed.SubnetArtifacts) != nil {
		t.Fatal("missing enrollment distinction changed")
	}
}

// Trusted input is required even when the response would otherwise be healthy.
// Missing review authority and explicit subnet disable do not dispose of history.
func TestSubnetArtifactsMissingInvalidAndExpiredExpectationsNeverContact(t *testing.T) {
	for _, item := range []struct {
		name   string
		change func(*SignalSettings)
	}{
		{name: "missing", change: func(s *SignalSettings) { s.SubnetArtifacts = nil }},
		{name: "disabled missing", change: func(s *SignalSettings) {
			s.STConfigStatus = STConfigurationExplicitDisabled
			s.VerificationEnabled = false
			s.STDeploymentKey = ""
			s.SubnetArtifacts = nil
		}},
		{name: "empty", change: func(s *SignalSettings) { s.SubnetArtifacts.Objects = nil }},
		{name: "too many", change: func(s *SignalSettings) {
			s.SubnetArtifacts.Objects = make([]SubnetArtifactExpectation, maximumSubnetArtifactObjects+1)
		}},
		{name: "duplicate object", change: func(s *SignalSettings) {
			s.SubnetArtifacts.Objects = append(s.SubnetArtifacts.Objects, s.SubnetArtifacts.Objects[0])
		}},
		{name: "duplicate origin", change: func(s *SignalSettings) { s.SubnetArtifacts.Origins[1] = s.SubnetArtifacts.Origins[0] }},
		{name: "one origin", change: func(s *SignalSettings) { s.SubnetArtifacts.Origins = s.SubnetArtifacts.Origins[:1] }},
		{name: "http origin", change: func(s *SignalSettings) { s.SubnetArtifacts.Origins[0] = "http://private.example.test" }},
		{name: "credential origin", change: func(s *SignalSettings) { s.SubnetArtifacts.Origins[0] = "https://private-token@private.example.test" }},
		{name: "path origin", change: func(s *SignalSettings) { s.SubnetArtifacts.Origins[0] += "/unexpected" }},
		{name: "missing wire hash", change: func(s *SignalSettings) { s.SubnetArtifacts.Objects[0].WireSha256 = "" }},
		{name: "foreign hash encoding", change: func(s *SignalSettings) {
			s.SubnetArtifacts.Objects[0].ContentHash = "SHA256:" + strings.Repeat("AB", 32)
		}},
		{name: "missing signer", change: func(s *SignalSettings) { s.SubnetArtifacts.Objects[0].Signer = "" }},
		{name: "bad boundary", change: func(s *SignalSettings) { s.SubnetArtifacts.Objects[0].EndNumber = 0 }},
		{name: "expired", change: func(s *SignalSettings) { s.SubnetArtifacts.ReviewUntil = s.Now().Format(time.RFC3339Nano) }},
		{name: "future", change: func(s *SignalSettings) {
			s.SubnetArtifacts.ReviewFrom = s.Now().Add(time.Minute).Format(time.RFC3339Nano)
		}},
		{name: "missing generation", change: func(s *SignalSettings) { s.SettingsGenerationCheck = nil }},
		{name: "stale generation", change: func(s *SignalSettings) {
			s.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return false, nil }
		}},
		{name: "generation failure", change: func(s *SignalSettings) {
			s.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
				return false, errors.New("private-generation-token")
			}
		}},
	} {
		settings, source, _, _ := subnetArtifactFixture(t)
		item.change(&settings)
		if err := settings.Validate(); err != nil {
			t.Fatalf("%s: authority control has invalid general settings: %v", item.name, err)
		}
		alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || source.calls != 0 {
			t.Fatalf("%s: missing authority contacted source or disappeared", item.name)
		}
		alert := requireAlertClass(t, alerts, "subnet-history-availability")
		if !alert.unobservable || alert.Sustain != 1 || alert.PageSustain != 0 {
			t.Fatalf("%s: invalid enrollment changed the coverage policy", item.name)
		}
		requireAlertOmits(t, alert, "private-token", "private-generation-token")
	}
}

// Historical bytes remain in the enrolled read scope when new risk is disabled.
func TestSubnetArtifactsDisabledWorkPreservesEnrolledHistory(t *testing.T) {
	settings, source, _, _ := subnetArtifactFixture(t)
	settings.STConfigStatus = STConfigurationExplicitDisabled
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	if err := settings.Validate(); err != nil {
		t.Fatalf("disabled history control has invalid general settings: %v", err)
	}
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if source.calls != 2 {
		t.Fatal("valid disabled history control did not enter both enrolled origins")
	}
	if err != nil || len(alerts) != 0 || source.calls != 2 {
		t.Fatal("work disable silently retired enrolled historical evidence")
	}
}

// Independent current authority includes every origin, expected field and
// review window in the existing full-settings equality, with no raw values emitted.
func TestSubnetArtifactsSettingsCloningAndGeneration(t *testing.T) {
	settings, _, _, _ := subnetArtifactFixture(t)
	current := settings.withDefaults()
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	if equal, err := settings.SettingsGenerationCheck(context.Background(), settings); err != nil || !equal {
		t.Fatal("equal artifact enrollment changed generation")
	}
	captured := settings.withDefaults()
	current.SubnetArtifacts.Objects[0].Signer = "0x" + strings.Repeat("12", 20)
	current.SubnetArtifacts.Origins[0] = "https://changed.private.example.test"
	if equal, err := settings.SettingsGenerationCheck(context.Background(), settings); err != nil || equal {
		t.Fatal("changed artifact authority retained generation")
	}
	if captured.SubnetArtifacts.Objects[0].Signer != settings.SubnetArtifacts.Objects[0].Signer || captured.SubnetArtifacts.Origins[0] != settings.SubnetArtifacts.Origins[0] {
		t.Fatal("captured settings borrowed mutable enrollment")
	}
	env, err := newProbeEnv(captured)
	if err != nil {
		t.Fatal(err)
	}
	captured.SubnetArtifacts.Origins[0] = "https://mutated.private.example.test"
	if env.cfg.subnetArtifacts.Origins[0] == captured.SubnetArtifacts.Origins[0] {
		t.Fatal("probe environment borrowed an origin slice")
	}
}

// Cancellation from the local authority seam is a run cancellation with its
// original cause, not a configuration warning followed by remote contact.
func TestSubnetArtifactsGenerationCancellationPreservesCause(t *testing.T) {
	settings, source, _, _ := subnetArtifactFixture(t)
	leaf := &subnetArtifactPrivateCause{}
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return false, errors.Join(context.Canceled, leaf) }
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	var actual *subnetArtifactPrivateCause
	if len(alerts) != 0 || source.calls != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, leaf) || !errors.As(err, &actual) || actual != leaf || strings.Contains(err.Error(), "private-") {
		t.Fatal("authority cancellation was flattened, leaked or continued")
	}
}
