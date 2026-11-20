package monitor

import (
	"context"
	"testing"
)

type ticketEscalationEvent struct {
	kind     ticketEventKind
	tier     string
	identity string
}

type ticketEscalationEmitter struct {
	events []ticketEscalationEvent
}

func (e *ticketEscalationEmitter) emit(_ context.Context, event ticketEvent) error {
	e.events = append(e.events, ticketEscalationEvent{
		kind:     event.kind,
		tier:     event.t.tier,
		identity: event.t.ticketIdentity.key(),
	})
	return nil
}

func TestTicketManagerPromotesStableIdentityAtPageSustain(t *testing.T) {
	emitter := &ticketEscalationEmitter{}
	manager := newTicketManager("synthetic", emitter)
	broken := finding{
		probeId:     "synthetic/progress",
		tier:        tierWarn,
		class:       "frozen",
		target:      "synthetic-node",
		frame:       "archive",
		sustain:     3,
		pageSustain: 5,
	}
	wantIdentity := ticketIdentity{
		probeId: broken.probeId,
		class:   broken.class,
		target:  broken.target,
		frame:   broken.frame,
	}.key()

	for range 6 {
		manager.ingest(context.Background(), []finding{broken})
	}
	wantKinds := []ticketEventKind{ticketOpen, ticketUpdate, ticketUpdate, ticketUpdate}
	wantTiers := []string{tierWarn, tierWarn, tierPage, tierPage}
	if len(emitter.events) != len(wantKinds) {
		t.Fatalf("event count = %d, want %d: %+v", len(emitter.events), len(wantKinds), emitter.events)
	}
	for i, event := range emitter.events {
		if event.kind != wantKinds[i] || event.tier != wantTiers[i] || event.identity != wantIdentity {
			t.Fatalf(
				"event %d = %s/%s/%q, want %s/%s/%q",
				i, event.kind, event.tier, event.identity,
				wantKinds[i], wantTiers[i], wantIdentity,
			)
		}
	}
}
