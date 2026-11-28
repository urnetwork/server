//go:build sidecar_pending

package computet3

// Web search's rows on the private stores (SRCH-M13-08; DESIGN-COMPUTE §14.4, §21.5, §21.8, compute-10, compute-11):
// sidecar/model's compute_search.go over SRCH's part of MG-C2, and its two reads of BAL's compute_op, through server's
// own adapters on PostgreSQL 18.

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"
)

// A sanctions record of reason at at.
func computeT3SearchFlag(networkId sidecar.Id, sidecarId sidecar.Id, reason model.ComputeSearchFlagReason, payee byte, at time.Time) *model.ComputeSearchFlag {
	flag := &model.ComputeSearchFlag{
		FlagId:    sidecar.NewId(),
		NetworkId: networkId,
		SidecarId: sidecarId,
		Reason:    reason,
		Payee:     bytes.Repeat([]byte{payee}, 20),
		FlagTime:  at,
	}
	if reason == model.ComputeSearchFlagReasonOutOfBand {
		flag.Transaction = bytes.Repeat([]byte{payee}, 32)
	}
	return flag
}

// A refused payee suspends its network's search until reviewed; one record stands open a network, reason, payee and
// transaction, so a repeat writes nothing; an out-of-band record flags without suspending; a review closes them all,
// after which a new refusal suspends again; the retention deletes reviewed records alone.
func TestComputeSearchFlagsOnT3(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		networkId, sidecarId := sidecar.NewId(), sidecar.NewId()
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		insert := func(flag *model.ComputeSearchFlag) bool {
			t3.t.Helper()
			inserted, err := model.ComputeSearchFlagInsert(t3.env, t3.ctx, flag)
			if err != nil {
				t3.t.Fatal(err)
			}
			return inserted
		}
		suspended := func() bool {
			t3.t.Helper()
			suspended, err := model.ComputeSearchSuspended(t3.env, t3.ctx, networkId)
			if err != nil {
				t3.t.Fatal(err)
			}
			return suspended
		}
		if suspended() {
			t3.t.Fatal("a network with no record is suspended")
		}
		outOfBand := computeT3SearchFlag(networkId, sidecarId, model.ComputeSearchFlagReasonOutOfBand, 2, now)
		if !insert(outOfBand) || suspended() {
			t3.t.Fatal("an out-of-band record did not write, or suspended the network")
		}
		if !insert(computeT3SearchFlag(networkId, sidecarId, model.ComputeSearchFlagReasonPayeeRefused, 1, now)) || !suspended() {
			t3.t.Fatal("a refused payee did not suspend the network")
		}
		if insert(computeT3SearchFlag(networkId, sidecar.NewId(), model.ComputeSearchFlagReasonPayeeRefused, 1, now.Add(time.Minute))) {
			t3.t.Fatal("a repeat of an open record wrote a second")
		}
		if insert(computeT3SearchFlag(networkId, sidecarId, model.ComputeSearchFlagReasonOutOfBand, 2, now.Add(time.Minute))) {
			t3.t.Fatal("a repeat of an open out-of-band record wrote a second")
		}
		for _, reference := range []string{"", strings.Repeat("r", 257), "case\n1"} {
			if _, err := model.ComputeSearchFlagReview(t3.env, t3.ctx, networkId, reference, 256, now); !errors.Is(err, model.ErrComputeSearchArgs) {
				t3.t.Fatalf("a review referenced %q answered %v", reference, err)
			}
		}
		reviewed, err := model.ComputeSearchFlagReview(t3.env, t3.ctx, networkId, "case-1", 256, now.Add(time.Hour))
		if err != nil || reviewed != 2 || suspended() {
			t3.t.Fatalf("the review closed %d, %v", reviewed, err)
		}
		if !insert(computeT3SearchFlag(networkId, sidecarId, model.ComputeSearchFlagReasonPayeeRefused, 1, now.Add(2*time.Hour))) || !suspended() {
			t3.t.Fatal("a refusal after the review did not suspend again")
		}
		flags, err := model.ComputeSearchFlagsOf(t3.env, t3.ctx, networkId)
		if err != nil || len(flags) != 3 {
			t3.t.Fatalf("%d records, %v", len(flags), err)
		}
		if flags[0].ReviewReference == nil || *flags[0].ReviewReference != "case-1" || !bytes.Equal(flags[0].Transaction, outOfBand.Transaction) {
			t3.t.Fatalf("the first record read back as %+v", flags[0])
		}
		pruned, err := model.ComputeSearchFlagPrune(t3.env, t3.ctx, now.Add(3*time.Hour), 10)
		if err != nil || pruned != 2 {
			t3.t.Fatalf("pruned %d, %v", pruned, err)
		}
		if !suspended() {
			t3.t.Fatal("the retention deleted an open record")
		}
		if _, err := model.ComputeSearchFlagInsert(t3.env, t3.ctx, &model.ComputeSearchFlag{
			FlagId: sidecar.NewId(), NetworkId: networkId, SidecarId: sidecarId, Reason: model.ComputeSearchFlagReasonPayeeRefused,
			Payee: []byte{1}, FlagTime: now,
		}); !errors.Is(err, model.ErrComputeSearchArgs) {
			t3.t.Fatalf("a payee of one byte answered %v", err)
		}
		if err := t3.exec(`INSERT INTO compute_search_flag (flag_id, network_id, sidecar_id, reason, payee, flag_time) VALUES ($1, $2, $3, 2, $4, $5)`,
			sidecar.NewId(), networkId, sidecarId, bytes.Repeat([]byte{9}, 20), now); !computeT3SqlState(err, "23514") {
			t3.t.Fatalf("an out-of-band record with no transaction answered %v", err)
		}
	})
}

// An hour's outcome counts add up across reports, read back by class, and leave at their 400 days.
func TestComputeSearchOutcomeHoursOnT3(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		hour := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		for _, counts := range []map[model.ComputeSearchOutcome]int64{
			{model.ComputeSearchOutcomeOk: 3, model.ComputeSearchOutcomeTimeout: 1},
			{model.ComputeSearchOutcomeOk: 2, model.ComputeSearchOutcomeRefused: 0},
		} {
			if err := model.ComputeSearchOutcomeHourAdd(t3.env, t3.ctx, hour, counts); err != nil {
				t3.t.Fatal(err)
			}
		}
		counts, err := model.ComputeSearchOutcomeHourOf(t3.env, t3.ctx, hour)
		if err != nil || counts[model.ComputeSearchOutcomeOk] != 5 || counts[model.ComputeSearchOutcomeTimeout] != 1 || len(counts) != 2 {
			t3.t.Fatalf("the hour reads %v, %v", counts, err)
		}
		if err := model.ComputeSearchOutcomeHourAdd(t3.env, t3.ctx, hour, map[model.ComputeSearchOutcome]int64{model.ComputeSearchOutcomeOk: -1}); !errors.Is(err, model.ErrComputeSearchArgs) {
			t3.t.Fatalf("a negative count answered %v", err)
		}
		pruned, err := model.ComputeSearchOutcomeHourPrune(t3.env, t3.ctx, hour.Add(time.Hour), 1)
		if err != nil || pruned != 1 {
			t3.t.Fatalf("pruned %d, %v", pruned, err)
		}
		pruned, err = model.ComputeSearchOutcomeHourPrune(t3.env, t3.ctx, hour.Add(time.Hour), 10)
		if err != nil || pruned != 1 {
			t3.t.Fatalf("pruned %d the second time, %v", pruned, err)
		}
	})
}

// The open search rows past a time, oldest first and of kind 6 alone, and a network's searches since a time, a void
// one left out: what compute_search_tickets and {cmpr_<network_id>}'s rebuild read of BAL's compute_op.
func TestComputeSearchOpsOnT3(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		settings := model.DefaultComputeLedgerSettings()
		networkId, sidecarId := sidecar.NewId(), sidecar.NewId()
		minute := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		requestId := sidecar.NewId()
		opKeys := []sidecar.Id{}
		for attempt := int16(1); attempt <= 3; attempt += 1 {
			opKey, start, err := sidecarcontroller.ComputeOpKeyForSearch(settings, networkId, requestId, attempt, minute.Add(time.Duration(attempt)*time.Minute))
			if err != nil {
				t3.t.Fatal(err)
			}
			attemptCopy := attempt
			inserted, _, err := model.ComputeOpInsertOpen(t3.env, t3.ctx, settings, &model.ComputeOp{
				OpKey:          opKey,
				NetworkId:      networkId,
				State:          model.ComputeOpStateOpen,
				Kind:           model.ComputeChargeKindWebSearch,
				SubjectRef:     requestId,
				Attempt:        &attemptCopy,
				SidecarId:      sidecarId,
				Provider:       8,
				HoldNanoUsd:    12 * 1000 * 1000,
				ReserveNanoUsd: 12 * 1000 * 1000,
				StartTime:      start,
			})
			if err != nil || !inserted {
				t3.t.Fatalf("attempt %d's row: %v", attempt, err)
			}
			opKeys = append(opKeys, opKey)
		}
		if _, err := model.ComputeOpVoid(t3.env, t3.ctx, networkId, opKeys[2]); err != nil {
			t3.t.Fatal(err)
		}
		open, err := model.ComputeSearchOpListOpen(t3.env, t3.ctx, minute.Add(10*time.Minute), 10)
		if err != nil || len(open) != 2 || open[0].OpKey != opKeys[0] || open[1].OpKey != opKeys[1] {
			t3.t.Fatalf("the open search rows: %d, %v", len(open), err)
		}
		open, err = model.ComputeSearchOpListOpen(t3.env, t3.ctx, minute.Add(2*time.Minute), 10)
		if err != nil || len(open) != 1 {
			t3.t.Fatalf("the rows before the second attempt's minute: %d, %v", len(open), err)
		}
		count, err := model.ComputeSearchOpCountSince(t3.env, t3.ctx, networkId, minute)
		if err != nil || count != 2 {
			t3.t.Fatalf("counted %d, %v", count, err)
		}
		count, err = model.ComputeSearchOpCountSince(t3.env, t3.ctx, sidecar.NewId(), minute)
		if err != nil || count != 0 {
			t3.t.Fatalf("another network counted %d, %v", count, err)
		}
	})
}
