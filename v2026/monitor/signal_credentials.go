package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// NewCredentialsSignal implements SIGNALS.md §8.7 (`credentials`). It
// evaluates only secret-free readiness metadata assembled by config.go; raw
// Vault values are never retained by SignalSettings or rendered into Alerts.
func NewCredentialsSignal() Signal { return &credentialsSignal{} }

type credentialsSignal struct{}

func (*credentialsSignal) Number() string         { return "8.7" }
func (*credentialsSignal) Key() string            { return "credentials" }
func (*credentialsSignal) ID() string             { return "deployment/credentials" }
func (*credentialsSignal) Name() string           { return "Required credential completeness" }
func (*credentialsSignal) Cadence() time.Duration { return 5 * time.Minute }

func (s *credentialsSignal) Run(_ context.Context, settings SignalSettings) (Alerts, error) {
	settings = settings.withDefaults()
	if err := settings.validate(); err != nil {
		return nil, err
	}
	requirements := append([]CredentialRequirement(nil), settings.Credentials...)
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Key < requirements[j].Key })
	alerts := make(Alerts, 0)
	for _, requirement := range requirements {
		if requirement.ActivationUnknown {
			alerts = append(alerts, Alert{
				SignalNumber: s.Number(), SignalKey: s.Key(), SignalID: s.ID(), SignalName: s.Name(),
				Severity: SeverityWarn, Class: "credential-activation-unobservable", Target: requirement.Key,
				Frame: "config=" + requirement.Resource, Environment: settings.Environment, ObservedAt: settings.Now(),
				Sustain: 1, unobservable: true,
				Symptom:   fmt.Sprintf("Credential activation for %s is unknown", requirement.Key),
				Mechanism: "The consumer's Config resource could not be resolved, decoded, or validated. Omitting its credential requirement would confuse an observation failure with a deliberately disabled integration.",
				Baseline:  "The consumer configuration is observable and valid before its enabled providers and credential dependencies are evaluated.",
				Observed:  "activation=unknown configuration_status=unavailable-or-invalid",
				Evidence:  "The loader retained only the semantic integration key, Config resource name, and unknown activation status. Resource contents, parser input, lookup paths, and raw errors were discarded; Vault completeness was not inferred.",
				Context:   requirement.Purpose,
				Action:    "Restore readable, valid consumer configuration through its owning Config workflow, then load a fresh monitor settings generation. Preserve any prior credential finding; do not provision secrets or disable an integration merely to clear an unknown activation state.",
				Verify:    "A fresh settings load establishes authoritative optional absence, deliberate disablement, or every enabled provider's credential requirement. Use the controlled watcher handoff and verify the consumer generation and output separately.",
				Playbook:  "SIGNALS.md §8.7",
			})
			continue
		}
		if !requirement.Present && !requirement.Required {
			// Missing optional crash-report credentials deliberately preserve the
			// providers' graceful no-op contract.
			continue
		}
		class, observed, broken := credentialProblem(requirement)
		if !broken {
			continue
		}
		severity := SeverityWarn
		if requirement.Required {
			severity = SeverityPage
		}
		action := "Provision or repair this credential through the supported Vault workflow, deploy the resulting Vault generation, and rerun the monitor. Never copy a credential value into an alert, command argument, source file, or test fixture."
		if !requirement.Required {
			action = "Either complete this optional integration through the supported Vault workflow or remove the partial resource to disable it deliberately. Never copy a credential value into an alert, command argument, source file, or test fixture."
		}
		alerts = append(alerts, Alert{
			SignalNumber: s.Number(),
			SignalKey:    s.Key(),
			SignalID:     s.ID(),
			SignalName:   s.Name(),
			Severity:     severity,
			Class:        class,
			Target:       requirement.Key,
			Frame:        "resource=" + requirement.Resource,
			Environment:  settings.Environment,
			ObservedAt:   settings.Now(),
			Sustain:      1,
			Symptom:      fmt.Sprintf("Credential readiness for %s is incomplete", requirement.Key),
			Mechanism:    "A required integration can stay green at process startup because its Vault resource is resolved only when the dependent route or recurring task runs. Missing, malformed, or unusable credential fields can break that path or leave its intended authentication checks inactive while ordinary liveness remains healthy.",
			Baseline:     "Every required integration has a present, parseable credential with every required field usable by its consumer; an optional integration is either wholly absent or complete.",
			Observed:     observed,
			Evidence:     "The loader retained only the semantic requirement key, resource name, presence/parse status, and missing field names. It discarded every credential value and all parser input before this signal ran. This is the watcher's loaded configuration readiness, not proof of a running consumer's mounted generation or a failed live request.",
			Context:      requirement.Purpose,
			Action:       action,
			Verify:       "The credentials signal is clear, the dependent route or recurring task authenticates successfully, and no skipped-store, authentication, or required-vault event appears for two complete cadences.",
			Playbook:     "SIGNALS.md §8.7",
		})
	}
	return alerts, nil
}

func credentialProblem(requirement CredentialRequirement) (class string, observed string, broken bool) {
	if !requirement.Present {
		return "credential-resource-missing", fmt.Sprintf("required=%t resource_present=false", requirement.Required), true
	}
	if requirement.Malformed {
		return "credential-resource-malformed", fmt.Sprintf("required=%t resource_present=true parseable=false", requirement.Required), true
	}
	if len(requirement.MissingFields) != 0 {
		missing := append([]string(nil), requirement.MissingFields...)
		sort.Strings(missing)
		return "credential-fields-missing", fmt.Sprintf("required=%t resource_present=true missing_fields=%s", requirement.Required, strings.Join(missing, ",")), true
	}
	return "", "", false
}
