// Package monitor turns the named, numbered checks in SIGNALS.md into
// reusable Go signals. Executable wiring belongs in server/cli/monitor;
// probes, settings, execution, registration, and Markdown rendering remain in
// this package.
package monitor

import (
	"context"
	"fmt"
	"time"
)

// Signal is one named SIGNALS.md probe. Number links to the catalog section;
// Key is its short semantic filename/registry identity.
type Signal interface {
	Number() string
	Key() string
	ID() string
	Name() string
	Cadence() time.Duration
	Run(ctx context.Context, settings SignalSettings) (Alerts, error)
}

// signalAdapter lets the existing, battle-tested check implementations expose
// the reusable Signal API while their command parsers remain private.
type signalAdapter struct {
	number string
	key    string
	name   string
	probe  probe
	accept func(finding) bool
}

func (s *signalAdapter) Number() string         { return s.number }
func (s *signalAdapter) Key() string            { return s.key }
func (s *signalAdapter) ID() string             { return s.probe.id() }
func (s *signalAdapter) Name() string           { return s.name }
func (s *signalAdapter) Cadence() time.Duration { return s.probe.cadence() }

func (s *signalAdapter) Run(ctx context.Context, settings SignalSettings) (Alerts, error) {
	settings = settings.withDefaults()
	if err := settings.validate(); err != nil {
		return nil, err
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		return nil, err
	}
	findings, err := s.probe.check(ctx, env)
	if err != nil {
		return nil, err
	}
	alerts := make(Alerts, 0, len(findings))
	for _, finding := range findings {
		if finding.healthy || (s.accept != nil && !s.accept(finding)) {
			continue
		}
		alerts = append(alerts, alertFromFinding(settings, s.number, s.key, s.name, finding))
	}
	return alerts, nil
}

func alertFromFinding(settings SignalSettings, number, key, name string, f finding) Alert {
	mechanism := f.mechanism
	if mechanism == "" {
		mechanism = "The observed value is outside the SIGNALS.md healthy band; the target and evidence below identify the affected production boundary."
	}
	action := f.action
	if action == "" && f.playbook != "" {
		action = fmt.Sprintf("Follow %s using the evidence above; do not mutate the target until the discriminating measurement is confirmed.", f.playbook)
	}
	verify := f.verify
	if verify == "" {
		verify = "Re-run this signal and confirm the observed value has returned to the expected baseline."
	}
	return Alert{
		SignalNumber: number,
		SignalKey:    key,
		SignalID:     f.probeId,
		SignalName:   name,
		Severity:     Severity(f.tier),
		Class:        f.class,
		Target:       f.target,
		Frame:        f.frame,
		Environment:  settings.Environment,
		ObservedAt:   settings.Now(),
		Sustain:      f.sustain,
		Symptom:      f.symptom,
		Mechanism:    mechanism,
		Baseline:     f.baseline,
		Observed:     f.observed,
		Evidence:     f.evidence,
		Context:      f.context,
		Action:       action,
		Verify:       verify,
		Playbook:     f.playbook,
	}
}

func acceptProbeIDs(ids ...string) func(finding) bool {
	allowed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	return func(f finding) bool {
		_, ok := allowed[f.probeId]
		return ok
	}
}
