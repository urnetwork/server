package model

// Real registration uses the existing local database fixture. The expired
// child stops before BEGIN; the same fixture proves custody and healthy work.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

type legacyRegistrationObservationTestLog struct {
	connect.Logger
	lines   []string
	refusal error
}

func (self *legacyRegistrationObservationTestLog) Infof(format string, args ...any) {
	if self.refusal != nil {
		panic(self.refusal)
	}
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

// Only variable numeric observations are erased. The exact producer's keys,
// finite strings, booleans and prefix feed the monitor's shared input fixture.
func normalizeLegacyRegistrationAnnotation(t testing.TB, line string) []byte {
	t.Helper()
	body, ok := strings.CutPrefix(line, "[legacy_registration_db] ")
	if !ok || len(line) > 4096 {
		t.Fatal("registration annotation lost its fixed prefix or bound")
	}
	var value any
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		t.Fatal(err)
	}
	var normalize func(any) any
	normalize = func(value any) any {
		switch value := value.(type) {
		case map[string]any:
			for key, entry := range value {
				if key != "schema" {
					value[key] = normalize(entry)
				}
			}
		case float64:
			return float64(0)
		}
		return value
	}
	encoded, err := json.Marshal(normalize(value))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestLegacyRegistrationDbObservationExpiredChildRetainsCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		fixture, contractId := legacySettlementTestIntent(t, ctx)
		shard := int(contractId[15]) % LegacySettlementShardCount
		cursor := &LegacySettlementOwnerCursor{End: contractId}
		beforeCursor, err := json.Marshal(cursor)
		server.Raise(err)
		beforeReports := legacyFinancialCohortReports(ctx, []server.Id{contractId})
		parent := context.Background()
		turn, cancelTurn := context.WithTimeoutCause(parent, legacySettlementDispatchBudget, errLegacySettlementDispatchBudget)
		defer cancelTurn()
		child, cancelChild := context.WithDeadline(turn, time.Unix(1, 0))
		defer cancelChild()
		observation := newLegacyRegistrationDbObservation(parent, turn, child)
		log := &legacyRegistrationObservationTestLog{Logger: connect.NewNoopLogger()}
		observation.log = log
		retained, registered, unresolved := cursor, 17, 19
		var outcome any
		func() {
			defer func() { outcome = recover() }()
			defer observation.finish()
			retained, registered, unresolved = registerLegacyCloseOwnerPage(child, shard, cursor, observation)
		}()
		observation.finish()
		if outcome != context.DeadlineExceeded || retained != cursor || registered != 17 || unresolved != 19 ||
			observation.phase.Phase() != server.DbOperationAcquire || len(log.lines) != 1 {
			t.Fatal("failed acquisition changed original panic/cursor or duplicated its annotation", outcome, registered, unresolved, len(log.lines))
		}
		afterCursor, err := json.Marshal(cursor)
		server.Raise(err)
		if !bytes.Equal(beforeCursor, afterCursor) || !bytes.Equal(beforeReports, legacyFinancialCohortReports(ctx, []server.Id{contractId})) {
			t.Fatal("observing a failed registration changed cursor or reports")
		}
		requireLegacySettlementTestState(t, ctx, fixture, contractId, true, false, 1000, 100)
		if strings.Contains(log.lines[0], contractId.String()) || strings.Contains(log.lines[0], "context deadline exceeded") || strings.Contains(log.lines[0], "Unexpected error:") {
			t.Fatal("annotation included identity/raw error or became a second panic record")
		}
		golden, err := os.ReadFile("../monitor/testdata/legacy_registration_db_observation.json")
		if err != nil {
			t.Fatal(err)
		}
		var expected any
		if err := json.Unmarshal(golden, &expected); err != nil {
			t.Fatal(err)
		}
		canonical, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(normalizeLegacyRegistrationAnnotation(t, log.lines[0]), canonical) {
			t.Fatal("actual registration annotation diverged from the monitor input contract")
		}
		// The original ErrorJson contract remains exactly two fields.
		var original map[string]any
		if err := json.Unmarshal([]byte(server.ErrorJson(outcome, []byte("synthetic stack"))), &original); err != nil || len(original) != 2 || original["error"] != "context.deadlineExceededError=context deadline exceeded" || original["stack"] == nil {
			t.Fatal("original panic payload or exact error was replaced")
		}
		live, cancelLive := context.WithTimeout(parent, legacySettlementRegistrationBudget)
		defer cancelLive()
		healthy := newLegacyRegistrationDbObservation(parent, live, live)
		healthyLog := &legacyRegistrationObservationTestLog{Logger: connect.NewNoopLogger()}
		healthy.log = healthyLog
		func() { defer healthy.finish(); registerLegacyCloseOwnerPage(live, shard, cursor, healthy) }()
		if len(healthyLog.lines) != 0 || !healthy.timing.Acquire.Admitted {
			t.Fatal("healthy registration emitted an acquisition annotation")
		}
		requireLegacySettlementTestState(t, ctx, fixture, contractId, true, false, 1000, 100)
	})
}

// A diagnostic sink can fail without replacing the already unwinding original.
// A successful Acquire followed by a body refusal emits no acquisition record.
func TestLegacyRegistrationDbObservationPreservesOriginalAndLaterRefusal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		parent := context.Background()
		child, cancel := context.WithDeadline(parent, time.Unix(1, 0))
		defer cancel()
		observation := newLegacyRegistrationDbObservation(parent, parent, child)
		observation.log = &legacyRegistrationObservationTestLog{Logger: connect.NewNoopLogger(), refusal: errors.New("synthetic log sink refusal")}
		var outcome any
		func() {
			defer func() { outcome = recover() }()
			defer observation.finish()
			registerLegacyCloseOwnerPage(child, 0, nil, observation)
		}()
		if outcome != context.DeadlineExceeded {
			t.Fatal("diagnostic sink replaced the original deadline", outcome)
		}
		live := newLegacyRegistrationDbObservation(parent, parent, parent)
		log := &legacyRegistrationObservationTestLog{Logger: connect.NewNoopLogger()}
		live.log = log
		original := errors.New("synthetic callback refusal")
		func() {
			defer func() { outcome = recover() }()
			defer live.finish()
			server.Tx(parent, func(server.PgTx) { panic(original) }, server.OptNoRetry(), &live.timing, &live.phase)
		}()
		if outcome != original || len(log.lines) != 0 || !live.timing.Acquire.Admitted {
			t.Fatal("post-acquisition error became an admission annotation or changed cause", outcome, len(log.lines))
		}
		unavailable := newLegacyRegistrationDbObservation(parent, parent, parent)
		unavailable.log = log
		unavailable.finish()
		if len(log.lines) != 0 {
			t.Fatal("missing observation became a failed acquisition")
		}
		privateParent, stop := context.WithCancelCause(parent)
		stop(errors.New("synthetic-secret 192.0.2.88 private.example"))
		private := newLegacyRegistrationDbObservation(privateParent, privateParent, privateParent)
		private.log = log
		func() {
			defer func() { outcome = recover() }()
			defer private.finish()
			registerLegacyCloseOwnerPage(privateParent, 0, nil, private)
		}()
		if outcome != context.Canceled || len(log.lines) != 1 || !strings.Contains(log.lines[0], `"cause":"other"`) ||
			strings.Contains(log.lines[0], "synthetic-secret") || strings.Contains(log.lines[0], "192.0.2.88") || strings.Contains(log.lines[0], "private.example") {
			t.Fatal("arbitrary parent cause escaped the finite annotation or changed cancellation")
		}
	})
}
