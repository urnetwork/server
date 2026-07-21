// Tickets: the monitor's output and the handoff interface to the separate
// diagnose-and-fix system. A ticket is a detailed textual summary of what was
// observed vs the baseline expected, pinpointed with real names and values
// (SIGNALS.md §6b). It carries no remediation beyond a playbook pointer —
// fixing is the downstream system's job.
//
// The manager dedupes by identity (probe id, class, target, frame): one open
// ticket per identity, with hysteresis (n consecutive failing ticks before it
// opens) and auto-resolve (healthy band held for the resolve window). The
// manager is not safe for concurrent use; the caller serializes ingest.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ticket lifecycle transitions
type ticketEventKind string

const (
	ticketOpen    ticketEventKind = "OPEN"
	ticketUpdate  ticketEventKind = "UPDATE"
	ticketResolve ticketEventKind = "RESOLVE"
)

// ticketIdentity is what makes one ticket distinct.
type ticketIdentity struct {
	probeId string
	class   string
	target  string
	frame   string
}

func (self ticketIdentity) key() string {
	return strings.Join([]string{self.probeId, self.class, self.target, self.frame}, "|")
}

// prefixKey ignores frame — used to resolve frame-bearing tickets when the
// probe reports the (probe, class, target) signal healthy again.
func (self ticketIdentity) prefixKey() string {
	return strings.Join([]string{self.probeId, self.class, self.target}, "|")
}

// ticket is one open issue.
type ticket struct {
	ticketIdentity
	tier     string
	env      string
	openedAt time.Time
	updated  time.Time
	last     finding

	brokenStreak  int
	healthyStreak int
	open          bool
}

// ticketEvent is an emitted lifecycle transition.
type ticketEvent struct {
	kind ticketEventKind
	t    *ticket
	at   time.Time
}

// ticketEmitter delivers ticket events (console now; webhook and github pr are
// deferred future emitters behind this same interface).
type ticketEmitter interface {
	emit(ctx context.Context, ev ticketEvent) error
}

// ticketManager holds ticket state and drives the lifecycle.
type ticketManager struct {
	env     string
	emitter ticketEmitter
	tickets map[string]*ticket
	// healthy ticks required to resolve (5 = 5 min at 60s cadence, §7)
	resolveTicks int
	now          func() time.Time

	// immediate opens a ticket on the first broken tick (sustain treated as
	// 1). Used for --once local dev so a single pass surfaces what would fire.
	immediate bool
}

func newTicketManager(env string, emitter ticketEmitter) *ticketManager {
	return &ticketManager{
		env:          env,
		emitter:      emitter,
		tickets:      map[string]*ticket{},
		resolveTicks: 5,
		now:          time.Now,
	}
}

// ingest folds one tick's findings into ticket state and emits transitions.
func (self *ticketManager) ingest(ctx context.Context, findings []finding) {
	for _, f := range findings {
		if f.healthy {
			self.ingestHealthy(ctx, f)
		} else {
			self.ingestBroken(ctx, f)
		}
	}
}

func (self *ticketManager) ingestBroken(ctx context.Context, f finding) {
	identity := ticketIdentity{probeId: f.probeId, class: f.class, target: f.target, frame: f.frame}
	key := identity.key()
	now := self.now()

	t, ok := self.tickets[key]
	if !ok {
		t = &ticket{ticketIdentity: identity, tier: f.tier, env: self.env}
		self.tickets[key] = t
	}
	t.brokenStreak += 1
	t.healthyStreak = 0
	t.tier = f.tier
	t.last = f
	t.updated = now

	sustain := f.sustain
	if sustain <= 0 || self.immediate {
		sustain = 1
	}

	switch {
	case !t.open && t.brokenStreak >= sustain:
		t.open = true
		t.openedAt = now
		self.emit(ctx, ticketEvent{kind: ticketOpen, t: t, at: now})
	case t.open:
		self.emit(ctx, ticketEvent{kind: ticketUpdate, t: t, at: now})
	}
}

func (self *ticketManager) ingestHealthy(ctx context.Context, f finding) {
	// resolve every open ticket for this (probe, class, target), regardless of
	// frame — a frame-bearing ticket (e.g. specific dead node ports) resolves
	// when the probe reports the signal healthy again
	prefixKey := ticketIdentity{probeId: f.probeId, class: f.class, target: f.target}.prefixKey()
	now := self.now()
	for key, t := range self.tickets {
		if t.prefixKey() != prefixKey {
			continue
		}
		t.healthyStreak += 1
		t.brokenStreak = 0
		if t.open && t.healthyStreak >= self.resolveTicks {
			t.updated = now
			self.emit(ctx, ticketEvent{kind: ticketResolve, t: t, at: now})
			delete(self.tickets, key)
		} else if !t.open {
			// never opened (transient blip below sustain) — forget it
			delete(self.tickets, key)
		}
	}
}

func (self *ticketManager) emit(ctx context.Context, ev ticketEvent) {
	if self.emitter == nil {
		return
	}
	_ = self.emitter.emit(ctx, ev)
}

// openCount is the number of currently-open tickets (for the heartbeat line).
func (self *ticketManager) openCount() int {
	n := 0
	for _, t := range self.tickets {
		if t.open {
			n += 1
		}
	}
	return n
}

// renderTicket produces the SIGNALS.md §6b text block for an event.
func renderTicket(ev ticketEvent) string {
	t := ev.t
	f := t.last
	var b strings.Builder
	fmt.Fprintf(&b, "TICKET %s/%s %s %s tier=%s env=%s\n",
		f.probeId, t.class, ev.kind, ev.at.UTC().Format(time.RFC3339), t.tier, t.env)
	if ev.kind == ticketResolve {
		fmt.Fprintf(&b, "RESOLVED  signal returned to healthy band; opened %s (held %s)\n",
			t.openedAt.UTC().Format(time.RFC3339), ev.at.Sub(t.openedAt).Round(time.Second))
		return b.String()
	}
	writeTicketField(&b, "SYMPTOM", f.symptom)
	writeTicketField(&b, "BASELINE", f.baseline)
	writeTicketField(&b, "OBSERVED", f.observed)
	writeTicketField(&b, "EVIDENCE", f.evidence)
	writeTicketField(&b, "CONTEXT", f.context)
	writeTicketField(&b, "PLAYBOOK", f.playbook)
	return b.String()
}

func writeTicketField(b *strings.Builder, label, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	// indent continuation lines under the label column
	lines := strings.Split(value, "\n")
	fmt.Fprintf(b, "%-9s %s\n", label, lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(b, "%-9s %s\n", "", l)
	}
}
