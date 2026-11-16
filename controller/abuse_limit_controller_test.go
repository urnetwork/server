package controller

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §21.7's tightening on §21's one path (abuse_limit_controller.go; ABUSE-M5-19), with the real effects and a
// recording mailer, on MG5's and MG1's tables in a build with sidecar_pending.

// pro.yml's numbers the cases read, and a snapshot that never reloads on its own.
func testAbuseLimitControllerSetup(t testing.TB) {
	t.Helper()
	t.Cleanup(model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{
		"evs_closure_bytes_per_day": 64 * 1024 * 1024,
		"evs_closure_bytes_floor":   4 * 1024 * 1024,
		"evs_closure_uploads_per_s": 4,
		"config_statements_per_day": 1000,
	}))
	t.Cleanup(model.Testing_SetNetworkSidecarLimitSettings(&model.NetworkSidecarLimitSettings{SnapshotTtl: time.Hour, ReloadTimeout: 30 * time.Second}))
}

// A reviewed tightening takes the erasers' allowance to the floor and lowers the day's statements, each a row
// citing the case, each action mailed; lifting both clears both rows, and the tier's limits read again.
func TestTightenNetworkLimitWritesRowsALiftClears(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testAbuseLimitControllerSetup(t)
		_, mailer := newTestAbuseDeps(t)
		restoreEffects := SetAbuseEffects(NewAbuseEffects(nil, nil))
		defer restoreEffects()
		networkId, _, caseId := newTestAbuseCase(t, ctx, false)
		reviewerId := server.NewId()

		floor := int64(4 * 1024 * 1024)
		closure, err := TightenNetworkLimit(ctx, reviewerId, caseId, server.NewId(), "evs_closure_bytes_per_day", floor, "review of the closure growth")
		if err != nil || !closure.Applied || !closure.Mailed {
			t.Fatalf("%+v, %v", closure, err)
		}
		statements, err := TightenNetworkLimit(ctx, reviewerId, caseId, server.NewId(), "config_statements_per_day", 2, "review of the closure growth")
		if err != nil || !statements.Applied {
			t.Fatalf("%+v, %v", statements, err)
		}
		limits := model.NetworkSidecarLimits(ctx, networkId)
		if limits.Limit("evs_closure_bytes_per_day") != floor || limits.Limit("config_statements_per_day") != 2 {
			t.Fatalf("limits read %d and %d", limits.Limit("evs_closure_bytes_per_day"), limits.Limit("config_statements_per_day"))
		}
		rows, err := model.GetNetworkSidecarLimitRows(ctx, networkId)
		if err != nil || len(rows) != 2 {
			t.Fatalf("rows %+v, %v", rows, err)
		}
		for _, row := range rows {
			if row.Reason != model.AbuseLimitReason(caseId) {
				t.Fatalf("a row cites %q", row.Reason)
			}
		}
		mailed := mailer.mailed()
		if len(mailed) != 2 {
			t.Fatalf("%d records mailed", len(mailed))
		}
		record, err := ParseAbuseRecord(mailed[0], testAbuseRecordMacKey)
		if err != nil || record.Kind != model.AbuseActionKindLimitTighten || record.Target != model.AbuseLimitTightenTarget("evs_closure_bytes_per_day", floor) {
			t.Fatalf("the record %+v, %v", record, err)
		}

		for _, tightening := range []*AbuseActionResult{closure, statements} {
			lift, err := LiftNetworkLimit(ctx, reviewerId, caseId, tightening.Action.ActionId, server.NewId(), false, "the network fixed it")
			if err != nil || !lift.Applied || !lift.Mailed {
				t.Fatalf("the lift %+v, %v", lift, err)
			}
		}
		if rows, _ := model.GetNetworkSidecarLimitRows(ctx, networkId); len(rows) != 0 {
			t.Fatalf("rows after the lifts %+v", rows)
		}
		limits = model.NetworkSidecarLimits(ctx, networkId)
		if limits.Limit("evs_closure_bytes_per_day") != 64*1024*1024 || limits.Limit("config_statements_per_day") != 1000 {
			t.Fatal("the tier's limits do not read again")
		}
	})
}

// The staff kind refuses each row QUOTA refuses, recording nothing: a floor or pacing row, an allowance under the
// floor, a key §21 does not tighten, a forwarding key; and is the reviewer's alone.
func TestLimitTightenStaffKindRefusesBeforeRecording(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testAbuseLimitControllerSetup(t)
		newTestAbuseDeps(t)
		restoreEffects := SetAbuseEffects(NewAbuseEffects(nil, nil))
		defer restoreEffects()
		networkId, _, caseId := newTestAbuseCase(t, ctx, false)
		reviewer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
		legal := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleLegal}}
		body := func(limitKey string, limitValue int64) []byte {
			return []byte(`{"kind":"limit_tighten","case_id":"` + caseId.String() + `","limit_key":"` + limitKey + `","limit_value":` + strconv.FormatInt(limitValue, 10) + `,"reason":"review"}`)
		}
		if _, err := AbuseStaffAction(ctx, legal, body("config_statements_per_day", 2)); !errors.Is(err, ErrAbuseStaffRoleRefused) {
			t.Fatalf("legal tightens: %v", err)
		}
		for _, refused := range []struct {
			limitKey   string
			limitValue int64
		}{
			{"evs_closure_bytes_floor", 4 * 1024 * 1024},
			{"evs_closure_uploads_per_s", 1},
			{"evs_closure_bytes_per_day", 4*1024*1024 - 1},
			{"storage", 1},
			{"forwarding_bytes_per_day", 0},
		} {
			if _, err := AbuseStaffAction(ctx, reviewer, body(refused.limitKey, refused.limitValue)); !errors.Is(err, model.ErrAbuseLimitRefused) {
				t.Errorf("%s=%d: %v", refused.limitKey, refused.limitValue, err)
			}
		}
		if actions := model.GetAbuseCaseActions(ctx, caseId); len(actions) != 0 {
			t.Fatalf("a refused row recorded %d actions", len(actions))
		}
		if rows, _ := model.GetNetworkSidecarLimitRows(ctx, networkId); len(rows) != 0 {
			t.Fatalf("a refused row wrote %+v", rows)
		}
		// the generic action kind refuses a tightening the model refuses too
		if _, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
			ActionId:   server.NewId(),
			CaseId:     caseId,
			Kind:       model.AbuseActionKindLimitTighten,
			Target:     model.AbuseLimitTightenTarget("evs_closure_bytes_floor", 1),
			ReviewerId: &reviewer.ReviewerId,
		}); !errors.Is(err, model.ErrAbuseLimitRefused) {
			t.Fatalf("a floor row on the action path: %v", err)
		}
		allowed, err := AbuseStaffAction(ctx, reviewer, body("evs_closure_bytes_per_day", 4*1024*1024))
		if err != nil || !allowed.Result.(*AbuseStaffActionResult).Applied {
			t.Fatalf("the floor itself: %+v, %v", allowed, err)
		}
	})
}
