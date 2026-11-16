// Boundary regressions preserve conservative fallback and scope/terminal
// refusal. Only valid scoped reads enter a disposable database.
package controller

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Embedding the old Store interface deliberately hides the optional private
// poll method while delegating final sealing to the real guarded store.
type competitionCompletionFallbackStore struct {
	Store
	source    PostgresStore
	sealCalls int
}

// The fallback still reads the current durable automated round each time.
func (self *competitionCompletionFallbackStore) CurrentRound(ctx context.Context, settings *Settings) (*roundRecord, error) {
	return self.source.CurrentAutomationRound(ctx, settings)
}

// Missing optimization never substitutes a caller claim for final authority.
func (self *competitionCompletionFallbackStore) SealAndElect(ctx context.Context, settings *Settings, roundId server.Id) (*model.CompetitionV2RoundResult, error) {
	self.sealCalls++
	return self.source.SealAndElect(ctx, settings, roundId)
}

// An older store implementation remains safe through the genuine final
// transaction for open, held and eventually closed states.
func TestCompetitionCompletionPgOptionalPollFallbackKeepsFinalAuthority(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		store := &competitionCompletionFallbackStore{Store: fixture.store, source: fixture.store}
		if _, present := any(store).(competitionAutomationEpochPollStore); present {
			tb.Fatal("fallback unexpectedly exposed the optional poll capability")
		}
		worker, err := newWorkerWithImageDigest(fixture.settings, store, &fakeEvaluator{}, "synthetic-fallback-worker", testWorkerImageDigest())
		if err != nil {
			tb.Fatal(err)
		}
		if finished, err := worker.finishEpoch(tb.Context()); finished || err != nil || store.sealCalls != 1 {
			tb.Fatal("open fallback did not defer to final guarded readiness", err)
		}
		first, err := fixture.store.SetAutomationHold(tb.Context(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-fallback-held", true, 0))
		if err != nil {
			tb.Fatal(err)
		}
		if finished, err := worker.finishEpoch(tb.Context()); finished || err != ErrAutomationHeld || store.sealCalls != 2 {
			tb.Fatal("optional fallback bypassed held final authority", err)
		}
		if _, err := fixture.store.SetAutomationHold(tb.Context(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-fallback-released", false, first.Revision)); err != nil {
			tb.Fatal(err)
		}
		fixture.now = fixture.round.ClosesAt
		if finished, err := worker.finishEpoch(tb.Context()); !finished || err != nil || store.sealCalls != 3 {
			tb.Fatal("closed fallback failed its genuine final seal", err)
		}
	})
}

// Invalid capability inputs must return before the canceled caller can enter
// database acquisition. This test needs no database or fixture execution.
func TestCompetitionCompletionPollRejectsInvalidCapabilityBeforeDatabase(t *testing.T) {
	enabled := &Settings{}
	enabled.AutomationPolicy.Enabled = true
	disabled := &Settings{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	store := PostgresStore{now: func() time.Time {
		t.Fatal("invalid poll reached its authority clock")
		return time.Time{}
	}}
	for _, item := range []struct {
		name     string
		settings *Settings
		roundId  server.Id
	}{
		{name: "nil", settings: nil, roundId: server.NewId()},
		{name: "disabled", settings: disabled, roundId: server.NewId()},
		{name: "zero-round", settings: enabled, roundId: server.Id{}},
	} {
		if ready, err := store.automationEpochSealReady(ctx, item.settings, item.roundId); ready || err != ErrAutomationDisabled {
			t.Fatalf("%s poll dispatched or changed its exact refusal: ready=%t error=%v", item.name, ready, err)
		}
	}
}

// Every independent scope discriminator and an unknown round stay absent,
// rather than being interpreted as an unheld ready round from another scope.
func TestCompetitionCompletionPgPollRejectsForeignAndMissingRound(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		fixture.now = fixture.round.ClosesAt
		for _, item := range []struct {
			name   string
			change func(*Settings)
		}{
			{name: "competition", change: func(settings *Settings) { settings.CompetitionId = "synthetic-foreign-competition" }},
			{name: "environment", change: func(settings *Settings) { settings.AutomationPolicy.Environment = "synthetic-foreign-environment" }},
			{name: "season", change: func(settings *Settings) { settings.AutomationPolicy.SeasonId = "synthetic-foreign-season" }},
			{name: "staging", change: func(settings *Settings) { settings.AutomationPolicy.Staging = !settings.AutomationPolicy.Staging }},
		} {
			settings := *fixture.settings
			item.change(&settings)
			if ready, err := fixture.store.automationEpochSealReady(tb.Context(), &settings, fixture.round.RoundId); ready || err != ErrNotFound {
				tb.Fatalf("%s scope supplied poll authority: ready=%t error=%v", item.name, ready, err)
			}
		}
		if ready, err := fixture.store.automationEpochSealReady(tb.Context(), fixture.settings, server.NewId()); ready || err != ErrNotFound {
			tb.Fatal("missing round supplied completion authority", ready, err)
		}
	})
}

// A genuine existing seal remains observable across a backward local clock,
// but never waives evaluation hold or rewrites any sealed election history.
func TestCompetitionCompletionPgSealedPollKeepsHoldAndImmutableHistory(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		fixture.now = fixture.round.ClosesAt
		if result, err := fixture.store.SealAndElect(tb.Context(), fixture.settings, fixture.round.RoundId); err != nil || result == nil {
			tb.Fatal("synthetic seal setup failed", err)
		}
		read := func() []byte {
			var content []byte
			server.Db(tb.Context(), func(conn server.PgConn) {
				server.Raise(conn.QueryRow(tb.Context(), `SELECT jsonb_build_object('round',to_jsonb(r),
 'seal',(SELECT to_jsonb(s) FROM competition_evaluation_seal s WHERE s.round_id=r.round_id),
 'decisions',(SELECT count(*) FROM competition_reward_decision d WHERE d.round_id=r.round_id))
 FROM competition_round r WHERE r.round_id=$1`, fixture.round.RoundId).Scan(&content))
			})
			return content
		}
		before := read()
		fixture.now = fixture.round.OpensAt.Add(-time.Second)
		if ready, err := fixture.store.automationEpochSealReady(tb.Context(), fixture.settings, fixture.round.RoundId); !ready || err != nil {
			tb.Fatal("sealed poll lost immutable completion to a stale clock", ready, err)
		}
		if _, err := fixture.store.SetAutomationHold(tb.Context(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-sealed-poll-held", true, 0)); err != nil {
			tb.Fatal(err)
		}
		if ready, err := fixture.store.automationEpochSealReady(tb.Context(), fixture.settings, fixture.round.RoundId); ready || err != ErrAutomationHeld {
			tb.Fatal("existing seal bypassed the held poll boundary", ready, err)
		}
		if !bytes.Equal(before, read()) {
			tb.Fatal("sealed or held poll rewrote immutable round/election state")
		}
	})
}

// Canceled durable state cannot turn ready merely because its close passes.
// An independent hold stays a refusal, not positive completion authority.
func TestCompetitionCompletionPgCanceledPollNeverBecomesReady(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), `UPDATE competition_round SET canceled=true WHERE round_id=$1`, fixture.round.RoundId))
		})
		for _, now := range []time.Time{fixture.now, fixture.round.ClosesAt, fixture.round.ClosesAt.Add(time.Hour)} {
			fixture.now = now
			if ready, err := fixture.store.automationEpochSealReady(tb.Context(), fixture.settings, fixture.round.RoundId); ready || err != nil {
				tb.Fatal("canceled round supplied ready poll authority", ready, err)
			}
		}
		if _, err := fixture.store.SetAutomationHold(tb.Context(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-canceled-poll-held", true, 0)); err != nil {
			tb.Fatal(err)
		}
		if ready, err := fixture.store.automationEpochSealReady(tb.Context(), fixture.settings, fixture.round.RoundId); ready || err != ErrAutomationHeld {
			tb.Fatal("canceled held round supplied positive authority", ready, err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var seals, decisions int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT (SELECT count(*) FROM competition_evaluation_seal WHERE round_id=$1),
 (SELECT count(*) FROM competition_reward_decision WHERE round_id=$1)`, fixture.round.RoundId).Scan(&seals, &decisions))
			if seals != 0 || decisions != 0 {
				tb.Fatal("canceled polling wrote completion/election state")
			}
		})
	})
}
