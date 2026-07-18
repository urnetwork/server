// Console emitter: the human-readable SIGNALS.md §6b block to stdout and a
// one-line json event to stderr — machine-parseable from day one, so the
// deferred webhook / github pr emitters cost nothing to add behind the same
// ticketEmitter interface.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

type consoleEmitter struct {
	human   io.Writer
	machine io.Writer
}

func newConsoleEmitter(human, machine io.Writer) *consoleEmitter {
	return &consoleEmitter{human: human, machine: machine}
}

type consoleJsonEvent struct {
	Kind     string    `json:"kind"`
	ProbeId  string    `json:"probe_id"`
	Class    string    `json:"class"`
	Target   string    `json:"target"`
	Frame    string    `json:"frame,omitempty"`
	Tier     string    `json:"tier"`
	Env      string    `json:"env"`
	At       time.Time `json:"at"`
	OpenedAt time.Time `json:"opened_at,omitempty"`
	Symptom  string    `json:"symptom,omitempty"`
	Observed string    `json:"observed,omitempty"`
	Playbook string    `json:"playbook,omitempty"`
}

func (self *consoleEmitter) emit(ctx context.Context, ev ticketEvent) error {
	if self.human != nil {
		fmt.Fprintln(self.human, renderTicket(ev))
	}
	if self.machine != nil {
		t := ev.t
		jsonEvent := consoleJsonEvent{
			Kind:     string(ev.kind),
			ProbeId:  t.probeId,
			Class:    t.class,
			Target:   t.target,
			Frame:    t.frame,
			Tier:     t.tier,
			Env:      t.env,
			At:       ev.at.UTC(),
			OpenedAt: t.openedAt.UTC(),
			Symptom:  t.last.symptom,
			Observed: t.last.observed,
			Playbook: t.last.playbook,
		}
		b, err := json.Marshal(jsonEvent)
		if err != nil {
			return err
		}
		fmt.Fprintln(self.machine, string(b))
	}
	return nil
}
